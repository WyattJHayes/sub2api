package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOutboxProcessingLoadsDispatchMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	eventID := uuid.New()
	runID := uuid.New()
	now := time.Now().UTC()
	payload := []byte(`{"sample_id":"` + uuid.NewString() + `"}`)
	mock.ExpectQuery("SELECT event.id, event.sequence, event.event_type").WithArgs(eventID).WillReturnRows(sqlmock.NewRows([]string{
		"id", "sequence", "event_type", "dedup_key", "causation_id", "cause_set_hash",
		"work_origin", "revision_batch_id", "run_id", "scope_key", "analysis_version",
		"source_type", "source_id", "source_hash", "payload_hash", "payload", "status",
		"attempt", "available_at", "lease_owner", "lease_expires_at", "lease_epoch",
		"last_error_code", "created_at", "updated_at",
	}).AddRow(
		eventID, int64(8), "cell_recompute", strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64),
		"initial", nil, runID, "reasoning/route-b", "v1", "assignment_replacement", uuid.NewString(),
		strings.Repeat("4", 64), strings.Repeat("5", 64), payload, "pending", 0, now, nil, nil, nil, nil, now, now,
	))

	event, err := loadEvaluationOutboxEvent(context.Background(), db, eventID)

	require.NoError(t, err)
	require.Equal(t, "reasoning/route-b", event.ScopeKey)
	require.Equal(t, "v1", event.AnalysisVersion)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOutboxProcessingValidatesSealedRouteEvidence(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runID := uuid.New()
	traceID := uuid.NewString()
	sourceHash := strings.Repeat("a", 64)
	sealedAt := time.Now().UTC()
	payload, err := json.Marshal(map[string]any{
		"route_trace_id": traceID, "schema_version": "radar-route-evidence-v1", "evidence_revision": 2,
	})
	require.NoError(t, err)
	event := service.EvaluationOutboxEvent{
		RunID: runID, SourceID: traceID, SourceHash: sourceHash, Payload: payload,
	}
	mock.ExpectQuery("SELECT evaluation_run_id, schema_version, evidence_revision, payload_hash, sealed_at").
		WithArgs(traceID).
		WillReturnRows(sqlmock.NewRows([]string{
			"evaluation_run_id", "schema_version", "evidence_revision", "payload_hash", "sealed_at",
		}).AddRow(runID, "radar-route-evidence-v1", int64(2), sourceHash, sealedAt))

	err = NewEvaluationOutboxDomainRepository(db).ValidateSealedRouteEvidence(context.Background(), event)

	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOutboxProcessingRejectsConflictingSealedRouteEvidence(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runID := uuid.New()
	traceID := uuid.NewString()
	payload := json.RawMessage(`{"route_trace_id":"` + traceID + `","schema_version":"radar-route-evidence-v1","evidence_revision":2}`)
	mock.ExpectQuery("SELECT evaluation_run_id, schema_version, evidence_revision, payload_hash, sealed_at").
		WithArgs(traceID).
		WillReturnRows(sqlmock.NewRows([]string{
			"evaluation_run_id", "schema_version", "evidence_revision", "payload_hash", "sealed_at",
		}).AddRow(runID, "radar-route-evidence-v1", int64(1), strings.Repeat("b", 64), time.Now().UTC()))

	err = NewEvaluationOutboxDomainRepository(db).ValidateSealedRouteEvidence(context.Background(), service.EvaluationOutboxEvent{
		RunID: runID, SourceID: traceID, SourceHash: strings.Repeat("a", 64), Payload: payload,
	})

	require.ErrorIs(t, err, service.ErrRouteEvidenceSealedConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOutboxProcessingReturnsNoGateTargetWithoutActiveSubject(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("FROM evaluation_runs run").WithArgs(runID).
		WillReturnRows(gateTargetRows())
	mock.ExpectCommit()

	target, err := NewEvaluationOutboxDomainRepository(db).ResolveRadarGateTarget(context.Background(), runID)

	require.NoError(t, err)
	require.Nil(t, target)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOutboxProcessingResolvesSingleGateTarget(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runID := uuid.New()
	subjectID := uuid.New()
	policyID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("FROM evaluation_runs run").WithArgs(runID).
		WillReturnRows(gateTargetRows().AddRow(subjectID, policyID, int64(42), int64(42), int64(42)))
	mock.ExpectCommit()

	target, err := NewEvaluationOutboxDomainRepository(db).ResolveRadarGateTarget(context.Background(), runID)

	require.NoError(t, err)
	require.Equal(t, &service.RadarGateTarget{
		ReleaseSubjectID: subjectID, PolicyID: policyID, TenantID: 42,
	}, target)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOutboxProcessingRejectsConflictingGateTargets(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runID := uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery("FROM evaluation_runs run").WithArgs(runID).
		WillReturnRows(gateTargetRows().
			AddRow(uuid.New(), uuid.New(), int64(42), int64(42), int64(42)).
			AddRow(uuid.New(), uuid.New(), int64(42), int64(42), int64(42)))
	mock.ExpectRollback()

	target, err := NewEvaluationOutboxDomainRepository(db).ResolveRadarGateTarget(context.Background(), runID)

	require.Nil(t, target)
	require.ErrorIs(t, err, service.ErrGovernanceHeadConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func gateTargetRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"release_subject_id", "policy_id", "run_tenant_id", "subject_tenant_id", "policy_tenant_id",
	})
}
