package repository

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestGetReliabilityFactsBindsAllImmutableReferences(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	runID, policyID := uuid.New(), uuid.New()
	loadPlanID, subjectID := uuid.New(), uuid.New()
	snapshotID, evidenceID, experimentID := uuid.New(), uuid.New(), uuid.New()
	now := time.Date(2026, 7, 31, 1, 2, 3, 0, time.UTC)
	policyHash := strings.Repeat("a", 64)
	snapshotHash := strings.Repeat("b", 64)
	sourceHash := strings.Repeat("c", 64)
	watermark := strings.Repeat("d", 64)
	evidenceHash := strings.Repeat("e", 64)
	metrics := []byte(`{"request_count":2,"success_count":2}`)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_runs WHERE id=\$1`).WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(41)))
	mock.ExpectQuery(`SELECT p\.id, p\.policy_hash`).WithArgs(policyID, int64(41), runID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "policy_hash"}).AddRow(policyID, policyHash))
	mock.ExpectQuery(`SELECT rs\.id, rs\.subject_hash`).WithArgs(runID, int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "subject_hash"}).AddRow(subjectID, strings.Repeat("f", 64)))
	mock.ExpectQuery(`SELECT s\.id, s\.snapshot_hash`).WithArgs(runID, "staging-v1", int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "snapshot_hash", "run_id", "load_plan_id", "reliability_profile_id", "slice_key",
			"window_start", "window_end", "query_version", "source_hash", "source_watermark", "fresh_until", "metrics",
		}).AddRow(snapshotID, snapshotHash, runID, loadPlanID, "staging-v1", "model=deepseek|region=staging",
			now.Add(-time.Hour), now, "reliability-query-v1", sourceHash, watermark, now.Add(time.Hour), metrics))
	mock.ExpectQuery(`SELECT load_plan_sha256`).WithArgs(loadPlanID, int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{"load_plan_sha256"}).AddRow(strings.Repeat("9", 64)))
	mock.ExpectQuery(`SELECT e\.id, e\.evidence_hash`).WithArgs(runID, int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "evidence_hash", "run_id", "experiment_id", "source_watermark", "recovery_generation"}).
			AddRow(evidenceID, evidenceHash, runID, experimentID, strings.Repeat("1", 64), 2))
	mock.ExpectQuery(`SELECT DISTINCT s\.artifact_manifest_hash`).WithArgs(runID, int64(41)).
		WillReturnRows(sqlmock.NewRows([]string{"artifact_manifest_hash"}).AddRow(strings.Repeat("2", 64)))
	mock.ExpectCommit()

	facts, err := (&radarGovernanceRepository{db: db}).GetReliabilityFacts(
		service.WithRadarTenant(context.Background(), 41), runID, policyID, "staging-v1",
	)
	require.NoError(t, err)
	require.Equal(t, runID, facts.RunID)
	require.Equal(t, loadPlanID, facts.LoadPlanID)
	require.Equal(t, strings.Repeat("9", 64), facts.LoadPlanSHA256)
	require.Equal(t, subjectID, facts.ReleaseSubjectID)
	require.Len(t, facts.Snapshots, 1)
	require.NotNil(t, facts.Recovery)
	require.Equal(t, evidenceID, facts.Recovery.EvidenceID)
	require.Equal(t, []string{strings.Repeat("2", 64)}, facts.ArtifactManifestHashes)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetReliabilityFactsRejectsCrossTenantRun(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	runID, policyID := uuid.New(), uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_runs WHERE id=\$1`).WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(42)))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).GetReliabilityFacts(
		service.WithRadarTenant(context.Background(), 41), runID, policyID, "staging-v1",
	)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetReliabilityFactsRequiresIdentifiers(t *testing.T) {
	repo := &radarGovernanceRepository{db: &sql.DB{}}
	_, err := repo.GetReliabilityFacts(context.Background(), uuid.Nil, uuid.New(), "staging-v1")
	require.Error(t, err)
}
