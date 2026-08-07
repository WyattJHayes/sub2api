package integration

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRadarTrustedGateE2E(t *testing.T) {
	database := radarRevisionDatabase(t)
	ctx := context.Background()
	assertTrustedGateStorageMode(t, database.db, "compatibility")

	require.NoError(t, runRadar198Cutover(t, database, "audit"))
	require.NoError(t, runRadar198Cutover(t, database, "drain"))
	writerID := uuid.New()
	_, err := database.db.ExecContext(ctx, `
		INSERT INTO evaluation_writer_sessions (
			instance_id, writer_kind, protocol_version, active_lease_count,
			heartbeat_expires_at, last_transaction_at
		) VALUES ($1, 'gate', 2, 1, NOW() + INTERVAL '5 minutes', NOW())`, writerID)
	require.NoError(t, err)

	err = runRadar198Cutover(t, database, "close")
	require.Error(t, err)
	require.Contains(t, err.Error(), "active evaluation writer or lease count is 1")
	requireCutoverState(t, database.db, "draining:audit:1")

	_, err = database.db.ExecContext(ctx, `
		UPDATE evaluation_writer_sessions
		SET active_lease_count = 0, heartbeat_expires_at = NOW() - INTERVAL '1 second'
		WHERE instance_id = $1`, writerID)
	require.NoError(t, err)
	require.NoError(t, runRadar198Cutover(t, database, "close"))
	requireCutoverState(t, database.db, "closed:audit:1")

	require.NoError(t, runRadar198Cutover(t, database, "migrate"))
	require.NoError(t, runRadar198Cutover(t, database, "reopen"))
	requireCutoverState(t, database.db, "open:audit:1")
	assertTrustedGateStorageMode(t, database.db, "compatibility")

	require.NoError(t, runRadar199Cutover(t, database, "audit"))
	require.NoError(t, runRadar199Cutover(t, database, "drain"))
	require.NoError(t, runRadar199Cutover(t, database, "close"))
	require.NoError(t, runRadar199Cutover(t, database, "migrate"))
	require.NoError(t, runRadar199Cutover(t, database, "enforce"))
	require.NoError(t, runRadar199Cutover(t, database, "register"))
	require.NoError(t, runRadar199Cutover(t, database, "reopen"))
	requireCutoverState(t, database.db, "open:enforce:2")
	assertTrustedGateStorageMode(t, database.db, "trusted")
	_ = ctx
}

func runRadar198Cutover(t *testing.T, database *radarRevisionTestDatabase, phase string) error {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "radar_migration_198_cutover.sh"))
	require.NoError(t, err)
	cmd := exec.Command(script, phase)
	cmd.Env = append(os.Environ(), "RADAR_DATABASE_URL="+database.dsn,
		"RADAR_PSQL_BIN="+database.psqlBin,
		"RADAR_MIGRATIONS_DIR="+database.migrationsDir,
		"RADAR_WRITER_INSTANCE_ID="+database.writerInstanceID,
		"RADAR_WRITER_KIND=api",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return &cutoverCommandError{phase: phase, output: string(output), err: err}
	}
	return nil
}

func assertTrustedGateStorageMode(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	var mode string
	require.NoError(t, db.QueryRow(`SELECT mode FROM evaluation_gate_storage_modes WHERE id=1`).Scan(&mode))
	require.Equal(t, want, strings.TrimSpace(mode))
}

func TestRadarTrustedGateCutoverRejectsExpiredWriter(t *testing.T) {
	database := radarRevisionDatabase(t)
	require.NoError(t, runRadar198Cutover(t, database, "audit"))
	require.NoError(t, runRadar198Cutover(t, database, "drain"))
	_, err := database.db.Exec(`
		INSERT INTO evaluation_writer_sessions (
			instance_id, writer_kind, protocol_version, active_lease_count,
			heartbeat_expires_at, last_transaction_at
		) VALUES ($1, 'gate', 2, 0, NOW() - INTERVAL '1 second', NOW())`, uuid.New())
	require.NoError(t, err)
	require.NoError(t, runRadar198Cutover(t, database, "close"))
	requireCutoverState(t, database.db, "closed:audit:1")
}

func TestRadarLegacyAnalysisCompletionLocksOnlyJobOnPostgres18(t *testing.T) {
	database := radarRevisionDatabase(t)
	fixture := createRadarRevisionFixture(t, database.db)
	_, err := database.db.ExecContext(context.Background(), `
		UPDATE evaluation_workers SET worker_kind='statistics', capabilities=ARRAY['coding'] WHERE id=$1`, fixture.workerID)
	require.NoError(t, err)
	jobID := uuid.New()
	leaseToken := "legacy-analysis-token"
	_, err = database.db.ExecContext(context.Background(), `
		INSERT INTO evaluation_analysis_jobs (
			id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, status, lease_token_hash, leased_by, lease_epoch, lease_expires_at
		) VALUES ($1, $2, 'coding', 'route-a', 'daily', 'v1', NOW(), 'leased', $3, $4, 0, $5)`,
		jobID, fixture.runID, radarRevisionHash(leaseToken), fixture.workerID, time.Now().UTC().Add(time.Minute))
	require.NoError(t, err)

	_, err = repository.NewEvaluationGradingRepository(database.db).CompleteAnalysisJob(
		context.Background(), jobID, leaseToken,
		service.AggregateSubmission{RunID: fixture.runID, ScoreIDs: []uuid.UUID{uuid.New()}},
	)
	require.ErrorIs(t, err, service.ErrAggregateRunMismatch)
	require.NotContains(t, err.Error(), "nullable side")
}
