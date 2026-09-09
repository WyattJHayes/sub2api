package repository

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func expectArtifactCleanupWriter(mock sqlmock.Sqlmock) {
	identity := defaultEvaluationWriterIdentity("api")
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO evaluation_writer_sessions").WithArgs(identity.InstanceID, "api", currentEvaluationWriterProtocolVersion).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_instance_id'").WithArgs(identity.InstanceID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_protocol'").WithArgs("2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_kind'").WithArgs("api").WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestEvaluationArtifactCleanupRepositoryListsEveryExpiredScanStatus(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cutoff := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "object_key", "scan_status", "retention_deadline"})
	statuses := []service.ArtifactScanStatus{
		service.ArtifactScanClean,
		"pending",
		service.ArtifactScanRejected,
		service.ArtifactScanFailed,
	}
	for index, status := range statuses {
		rows.AddRow(uuid.New(), "artifacts/"+string(status), status, cutoff.Add(-time.Duration(index+1)*time.Hour))
	}
	mock.ExpectQuery(`SELECT id, object_key, scan_status, retention_deadline\s+FROM evaluation_artifacts\s+WHERE deleted_at IS NULL AND retention_deadline <= \$1\s+ORDER BY retention_deadline, id\s+LIMIT \$2`).
		WithArgs(cutoff, 100).
		WillReturnRows(rows)

	repo := NewEvaluationArtifactCleanupRepository(db)
	candidates, err := repo.ListExpiredArtifacts(context.Background(), cutoff, 100)

	require.NoError(t, err)
	require.Len(t, candidates, len(statuses))
	for index, candidate := range candidates {
		require.Equal(t, statuses[index], candidate.ScanStatus)
	}
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationArtifactCleanupRepositoryMarksOnlyUnchangedExpiredRow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(true)

	deadline := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	deletedAt := deadline.Add(24 * time.Hour)
	candidate := service.ArtifactCleanupCandidate{
		ID:                uuid.New(),
		ObjectKey:         "artifacts/expired",
		ScanStatus:        service.ArtifactScanClean,
		RetentionDeadline: deadline,
	}
	expectArtifactCleanupWriter(mock)
	mock.ExpectExec(`UPDATE evaluation_artifacts\s+SET deleted_at = \$4\s+WHERE id = \$1 AND object_key = \$2 AND retention_deadline = \$3\s+AND deleted_at IS NULL AND retention_deadline <= \$4`).
		WithArgs(candidate.ID, candidate.ObjectKey, candidate.RetentionDeadline, deletedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := NewEvaluationArtifactCleanupRepository(db)
	marked, err := repo.MarkArtifactDeleted(context.Background(), candidate, deletedAt)

	require.NoError(t, err)
	require.True(t, marked)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationArtifactCleanupRepositorySkipsChangedRetentionRow(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(true)

	deadline := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	deletedAt := deadline.Add(24 * time.Hour)
	candidate := service.ArtifactCleanupCandidate{
		ID:                uuid.New(),
		ObjectKey:         "artifacts/extended",
		RetentionDeadline: deadline,
	}
	expectArtifactCleanupWriter(mock)
	mock.ExpectExec(`UPDATE evaluation_artifacts`).
		WithArgs(candidate.ID, candidate.ObjectKey, candidate.RetentionDeadline, deletedAt).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	repo := NewEvaluationArtifactCleanupRepository(db)
	marked, err := repo.MarkArtifactDeleted(context.Background(), candidate, deletedAt)

	require.NoError(t, err)
	require.False(t, marked)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestEvaluationArtifactCleanupRepositoryScopesTenantQueries(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	cutoff := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "object_key", "scan_status", "retention_deadline"})
	artifactID := uuid.New()
	rows.AddRow(artifactID, "artifacts/tenant", service.ArtifactScanClean, cutoff.Add(-time.Hour))
	mock.ExpectQuery(`SELECT id, object_key, scan_status, retention_deadline\s+FROM evaluation_artifacts\s+WHERE deleted_at IS NULL AND retention_deadline <= \$1\s+AND tenant_id = \$3\s+ORDER BY retention_deadline, id\s+LIMIT \$2`).
		WithArgs(cutoff, 100, int64(42)).
		WillReturnRows(rows)

	candidate := service.ArtifactCleanupCandidate{
		ID:                artifactID,
		ObjectKey:         "artifacts/tenant",
		ScanStatus:        service.ArtifactScanClean,
		RetentionDeadline: cutoff.Add(-time.Hour),
	}
	deletedAt := cutoff.Add(time.Hour)
	expectArtifactCleanupWriter(mock)
	mock.ExpectExec(`UPDATE evaluation_artifacts\s+SET deleted_at = \$4\s+WHERE id = \$1 AND object_key = \$2 AND retention_deadline = \$3\s+AND deleted_at IS NULL AND retention_deadline <= \$4 AND tenant_id = \$5`).
		WithArgs(candidate.ID, candidate.ObjectKey, candidate.RetentionDeadline, deletedAt, int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := NewEvaluationArtifactCleanupRepository(db)
	ctx := service.WithRadarTenant(context.Background(), 42)
	candidates, err := repo.ListExpiredArtifacts(ctx, cutoff, 100)
	require.NoError(t, err)
	require.Len(t, candidates, 1)
	marked, err := repo.MarkArtifactDeleted(ctx, candidate, deletedAt)
	require.NoError(t, err)
	require.True(t, marked)
	require.NoError(t, mock.ExpectationsWereMet())
}
