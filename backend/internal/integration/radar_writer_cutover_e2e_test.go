//go:build integration

package integration

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

func TestRadarWriterCutover197(t *testing.T) {
	dsn := os.Getenv("RADAR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RADAR_TEST_DATABASE_URL is required for cutover acceptance")
	}
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	require.NoError(t, db.PingContext(context.Background()))

	ctx := context.Background()
	_, err = db.ExecContext(ctx, `
		INSERT INTO evaluation_schema_cutovers (id, write_mode, guard_mode, minimum_protocol_version)
		VALUES (1, 'open', 'audit', 1)
		ON CONFLICT (id) DO UPDATE SET write_mode = 'open', guard_mode = 'audit', minimum_protocol_version = 2`)
	require.NoError(t, err)
	oldWriter := "00000000-0000-0000-0000-000000000197"
	_, err = db.ExecContext(ctx, `
		INSERT INTO evaluation_writer_sessions (instance_id, writer_kind, protocol_version, heartbeat_expires_at)
		VALUES ($1::uuid, 'worker', 1, NOW() + INTERVAL '5 minutes')
		ON CONFLICT (instance_id) DO UPDATE SET protocol_version = 1, active_lease_count = 1, heartbeat_expires_at = NOW() + INTERVAL '5 minutes'`, oldWriter)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE evaluation_writer_sessions SET active_lease_count = 1 WHERE instance_id = $1::uuid`, oldWriter)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `UPDATE evaluation_schema_cutovers SET write_mode = 'draining' WHERE id = 1`)
	require.NoError(t, err)
	var oldSessions int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluation_writer_sessions WHERE instance_id = $1::uuid AND heartbeat_expires_at > NOW() AND protocol_version < 2`, oldWriter).Scan(&oldSessions))
	require.Equal(t, 1, oldSessions, "drain must retain an active old writer session")

	_, err = db.ExecContext(ctx, `UPDATE evaluation_writer_sessions SET active_lease_count = 0, heartbeat_expires_at = NOW() - INTERVAL '1 second' WHERE instance_id = $1::uuid`, oldWriter)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE evaluation_schema_cutovers SET write_mode = 'closed', guard_mode = 'enforce' WHERE id = 1`)
	require.NoError(t, err)

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT set_config('app.evaluation_writer_instance_id', $1, true), set_config('app.evaluation_writer_protocol', '1', true), set_config('app.evaluation_writer_kind', 'worker', true)`, oldWriter)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT assert_evaluation_writer_protocol('evaluation_scores', 'I')`)
	require.Error(t, err, "closed/enforce must reject the old writer")
	require.NoError(t, tx.Rollback())

	newWriter := "00000000-0000-0000-0000-000000000198"
	_, err = db.ExecContext(ctx, `
		INSERT INTO evaluation_writer_sessions (instance_id, writer_kind, protocol_version, heartbeat_expires_at)
		VALUES ($1::uuid, 'worker', 2, NOW() + INTERVAL '5 minutes')
		ON CONFLICT (instance_id) DO UPDATE SET protocol_version = 2, heartbeat_expires_at = NOW() + INTERVAL '5 minutes'`, newWriter)
	require.NoError(t, err)
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT set_config('app.evaluation_writer_instance_id', $1, true), set_config('app.evaluation_writer_protocol', '2', true), set_config('app.evaluation_writer_kind', 'worker', true)`, newWriter)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SELECT assert_evaluation_writer_protocol('evaluation_scores', 'I')`)
	require.NoError(t, err, "current protocol writer must pass enforce")
	require.NoError(t, tx.Rollback())

	_, err = db.ExecContext(ctx, `UPDATE evaluation_schema_cutovers SET write_mode = 'open', guard_mode = 'audit' WHERE id = 1`)
	require.NoError(t, err)
}
