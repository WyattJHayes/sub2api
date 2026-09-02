package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPublishRecoveryEvidenceRejectsMalformedSourceWatermarkBeforeTransaction(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	canonical := []byte(`{"status":"rejected"}`)
	digest := sha256.Sum256(canonical)
	_, err = (&evaluationGradingRepository{db: db}).PublishRecoveryEvidence(
		context.Background(), uuid.New(), service.RadarRecoveryEvidenceSubmission{
			RunID: uuid.New(), ExperimentID: uuid.New(), SourceWatermark: strings.Repeat("G", 64),
			Status: "rejected", CanonicalEvidence: canonical, EvidenceHash: hex.EncodeToString(digest[:]),
		},
	)
	require.ErrorIs(t, err, service.ErrRecoveryEvidenceInvalid)
}

func TestPublishRecoveryEvidenceRequiresVerifiedTimestamp(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	canonical := []byte(`{"status":"verified"}`)
	digest := sha256.Sum256(canonical)
	_, err = (&evaluationGradingRepository{db: db}).PublishRecoveryEvidence(
		context.Background(), uuid.New(), service.RadarRecoveryEvidenceSubmission{
			RunID: uuid.New(), ExperimentID: uuid.New(), RecoveryGeneration: 1,
			SourceWatermark: strings.Repeat("a", 64), Status: "verified",
			RPOms: ptrInt64(1), RTOms: ptrInt64(2), VerifiedBy: ptrInt64(7),
			DeterministicRunID: ptrUUID(uuid.New()), CanonicalEvidence: canonical,
			EvidenceHash: hex.EncodeToString(digest[:]), VerifiedAt: time.Time{},
		},
	)
	require.ErrorIs(t, err, service.ErrRecoveryEvidenceInvalid)
}

func TestRadarFaultEventHashMatchesWorkerCanonicalVector(t *testing.T) {
	event := service.RadarFaultEventSubmission{
		ExperimentID:    uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		RunID:           uuid.MustParse("00000000-0000-0000-0000-000000000002"),
		EventType:       "started",
		ActorID:         ptrInt64(7),
		ServiceIdentity: "chaos-worker-1",
		CauseEvent:      "operator-approved",
		CreatedAt:       time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC),
		Payload:         json.RawMessage(`{"a":1,"cause_event":"operator-approved","service_identity":"chaos-worker-1","z":2}`),
	}

	createdAt, ok := canonicalRadarEventTimestamp(event.CreatedAt)
	require.True(t, ok)
	hash, err := radarFaultEventHash(event, createdAt)
	require.NoError(t, err)
	require.Equal(t, "5f2b70ce81769c6300e45b74da60641148653db6704cf94db725c6036b648f35", hash)
}

func TestGetApprovedFaultExperimentRejectsCrossTenantWorker(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	experimentID := uuid.New()
	runID := uuid.New()
	workerID := uuid.New()
	mock.ExpectQuery("SELECT id, run_id, load_plan_id, environment, fault_kind, target_kind").
		WithArgs(experimentID).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "run_id", "load_plan_id", "environment", "fault_kind", "target_kind",
			"target_ref", "status", "approved_by", "abort_deadline",
		}).AddRow(experimentID.String(), runID.String(), nil, "staging", "worker_kill", "worker", "worker-1", "approved", int64(7), time.Now().UTC().Add(time.Hour)))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(workerID, runID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	ctx := service.WithRadarWorkerID(context.Background(), workerID)
	_, err = (&evaluationGradingRepository{db: db}).GetApprovedFaultExperiment(ctx, experimentID)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRecoveryObservationRejectsCrossTenantWorker(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	observationID := uuid.New()
	runID := uuid.New()
	workerID := uuid.New()
	mock.ExpectQuery("SELECT e.run_id, e.tenant_id, r.tenant_id, e.canonical_evidence_bytes, e.status").
		WithArgs(observationID).
		WillReturnRows(sqlmock.NewRows([]string{"run_id", "tenant_id", "run_tenant_id", "canonical_evidence_bytes", "status"}).
			AddRow(runID.String(), int64(7), int64(7), []byte(`{"observation":{"ok":true}}`), "pending"))
	mock.ExpectQuery("SELECT EXISTS").WithArgs(workerID, runID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	ctx := service.WithRadarWorkerID(context.Background(), workerID)
	_, err = (&evaluationGradingRepository{db: db}).GetRecoveryObservation(ctx, observationID)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func ptrInt64(value int64) *int64 { return &value }

func ptrUUID(value uuid.UUID) *uuid.UUID { return &value }
