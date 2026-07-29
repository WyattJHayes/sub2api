//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestMigrationsRunner_ConcurrentInstancesSerializeOnSessionLock(t *testing.T) {
	const instances = 2
	errorsByInstance := make([]error, instances)
	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			errorsByInstance[index] = ApplyMigrations(ctx, integrationDB)
		}(i)
	}
	wg.Wait()
	for i, err := range errorsByInstance {
		require.NoErrorf(t, err, "migration instance %d", i)
	}
}

func TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate(t *testing.T) {
	tx := testTx(t)

	// Re-apply migrations to verify idempotency (no errors, no duplicate rows).
	require.NoError(t, ApplyMigrations(context.Background(), integrationDB))

	// schema_migrations should have at least the current migration set.
	var applied int
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM schema_migrations").Scan(&applied))
	require.GreaterOrEqual(t, applied, 7, "expected schema_migrations to contain applied migrations")

	// users: columns required by repository queries
	requireColumn(t, tx, "users", "username", "character varying", 100, false)
	requireColumn(t, tx, "users", "notes", "text", 0, false)

	// accounts: schedulable and rate-limit fields
	requireColumn(t, tx, "accounts", "notes", "text", 0, true)
	requireColumn(t, tx, "accounts", "schedulable", "boolean", 0, false)
	requireColumn(t, tx, "accounts", "rate_limited_at", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "rate_limit_reset_at", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "overload_until", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "accounts", "session_window_status", "character varying", 20, true)
	requireIndex(t, tx, "accounts", "idx_accounts_autopause_expiry_due")

	// groups: OpenAI Live 默认关闭，管理员显式开启后才可访问。
	requireColumn(t, tx, "groups", "allow_live", "boolean", 0, false)

	// api_keys: key length should be 128
	requireColumn(t, tx, "api_keys", "key", "character varying", 128, false)
	requireColumn(t, tx, "api_keys", "is_evaluation", "boolean", 0, false)

	// evaluation_route_evidence: immutable identity plus redacted routing and billing evidence
	requireColumn(t, tx, "evaluation_route_evidence", "route_trace_id", "character varying", 64, false)
	requireColumn(t, tx, "evaluation_route_evidence", "fallback_chain", "jsonb", 0, false)
	requireColumn(t, tx, "evaluation_route_evidence", "billed_amount", "numeric", 0, true)
	requireNumericColumn(t, tx, "evaluation_route_evidence", "billed_amount", 20, 8)

	// Radar control plane: immutable configuration and mutable execution state.
	for _, table := range []string{
		"evaluation_dataset_versions",
		"evaluation_cases",
		"evaluation_plans",
		"evaluation_runs",
		"evaluation_samples",
		"evaluation_assignments",
		"evaluation_workers",
		"evaluation_artifacts",
		"evaluation_run_events",
		"evaluation_budget_ledger",
		"evaluation_key_events",
	} {
		requireTable(t, tx, table)
	}
	requireColumn(t, tx, "evaluation_plans", "gateway_api_key_id", "bigint", 0, true)
	requireForeignKeyOnDelete(t, tx, "evaluation_plans", "gateway_api_key_id", "api_keys", "NO ACTION")
	requireIndex(t, tx, "evaluation_plans", "idx_evaluation_plans_gateway_api_key")

	// redeem_codes: subscription fields
	requireColumn(t, tx, "redeem_codes", "group_id", "bigint", 0, true)
	requireColumn(t, tx, "redeem_codes", "validity_days", "integer", 0, false)

	// usage_logs: billing_type used by filters/stats
	requireColumn(t, tx, "usage_logs", "billing_type", "smallint", 0, false)
	requireColumn(t, tx, "usage_logs", "request_type", "smallint", 0, false)
	requireColumn(t, tx, "usage_logs", "openai_ws_mode", "boolean", 0, false)
	requireColumn(t, tx, "usage_logs", "image_input_size", "character varying", 32, true)
	requireColumn(t, tx, "usage_logs", "image_output_size", "character varying", 32, true)
	requireColumn(t, tx, "usage_logs", "image_size_source", "character varying", 16, true)
	requireColumn(t, tx, "usage_logs", "image_size_breakdown", "jsonb", 0, true)
	requireColumn(t, tx, "usage_logs", "video_count", "integer", 0, false)
	requireColumn(t, tx, "usage_logs", "video_resolution", "character varying", 10, true)
	requireColumn(t, tx, "usage_logs", "video_duration_seconds", "integer", 0, true)
	requireConstraintDefinitionContains(
		t,
		tx,
		"usage_logs",
		"usage_logs_image_size_source_check",
		"image_size_source",
		"'output'",
		"'input'",
		"'default'",
		"'legacy'",
	)
	requireConstraintDefinitionContains(
		t,
		tx,
		"usage_logs",
		"usage_logs_image_billing_size_check",
		"image_count",
		"billing_mode",
		"'video'",
		"video_count",
		"image_size IS NOT NULL",
		"'1K'",
		"'2K'",
		"'4K'",
		"'mixed'",
	)

	// usage_billing_dedup: billing idempotency narrow table
	var usageBillingDedupRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.usage_billing_dedup')").Scan(&usageBillingDedupRegclass))
	require.True(t, usageBillingDedupRegclass.Valid, "expected usage_billing_dedup table to exist")
	requireColumn(t, tx, "usage_billing_dedup", "request_fingerprint", "character varying", 64, false)
	requireIndex(t, tx, "usage_billing_dedup", "idx_usage_billing_dedup_request_api_key")
	requireIndex(t, tx, "usage_billing_dedup", "idx_usage_billing_dedup_created_at_brin")

	var usageBillingDedupArchiveRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.usage_billing_dedup_archive')").Scan(&usageBillingDedupArchiveRegclass))
	require.True(t, usageBillingDedupArchiveRegclass.Valid, "expected usage_billing_dedup_archive table to exist")
	requireColumn(t, tx, "usage_billing_dedup_archive", "request_fingerprint", "character varying", 64, false)
	requireIndex(t, tx, "usage_billing_dedup_archive", "usage_billing_dedup_archive_pkey")

	// settings table should exist
	var settingsRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.settings')").Scan(&settingsRegclass))
	require.True(t, settingsRegclass.Valid, "expected settings table to exist")

	// security_secrets table should exist
	var securitySecretsRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.security_secrets')").Scan(&securitySecretsRegclass))
	require.True(t, securitySecretsRegclass.Valid, "expected security_secrets table to exist")

	// scheduler_outbox pending dedup support
	requireColumn(t, tx, "scheduler_outbox", "dedup_key", "text", 0, true)
	requireIndex(t, tx, "scheduler_outbox", "idx_scheduler_outbox_pending_dedup_key")

	// ops_system_logs: API key id index for operational log triage
	requireColumn(t, tx, "ops_system_logs", "api_key_id", "bigint", 0, true)
	requireIndex(t, tx, "ops_system_logs", "idx_ops_system_logs_api_key_id_created_at")

	// Bounded ingress rejection security aggregates.
	requireColumn(t, tx, "ops_ingress_reject_aggregates", "bucket_start", "timestamp with time zone", 0, false)
	requireColumn(t, tx, "ops_ingress_reject_aggregates", "client_ip", "inet", 0, false)
	requireColumn(t, tx, "ops_ingress_reject_aggregates", "request_count", "bigint", 0, false)
	requireIndex(t, tx, "ops_ingress_reject_aggregates", "idx_ops_ingress_reject_aggregates_bucket")
	requireIndex(t, tx, "ops_ingress_reject_aggregates", "idx_ops_ingress_reject_aggregates_ip_bucket")

	// user_allowed_groups table should exist
	var uagRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.user_allowed_groups')").Scan(&uagRegclass))
	require.True(t, uagRegclass.Valid, "expected user_allowed_groups table to exist")

	// user_subscriptions: deleted_at for soft delete support (migration 012)
	requireColumn(t, tx, "user_subscriptions", "deleted_at", "timestamp with time zone", 0, true)

	// orphan_allowed_groups_audit table should exist (migration 013)
	var orphanAuditRegclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(), "SELECT to_regclass('public.orphan_allowed_groups_audit')").Scan(&orphanAuditRegclass))
	require.True(t, orphanAuditRegclass.Valid, "expected orphan_allowed_groups_audit table to exist")

	// account_groups: created_at should be timestamptz
	requireColumn(t, tx, "account_groups", "created_at", "timestamp with time zone", 0, false)

	// user_allowed_groups: created_at should be timestamptz
	requireColumn(t, tx, "user_allowed_groups", "created_at", "timestamp with time zone", 0, false)
}

func TestMigration197TrustedLifecycleSchema(t *testing.T) {
	tx := testTx(t)

	for _, table := range []string{
		"evaluation_request_manifests",
		"evaluation_pair_specs",
		"evaluation_side_specs",
		"evaluation_pair_bindings",
		"evaluation_schema_cutovers",
		"evaluation_writer_sessions",
		"evaluation_writer_audit_events",
		"evaluation_worker_events",
	} {
		requireTable(t, tx, table)
	}

	requireColumn(t, tx, "evaluation_runs", "budget_mode", "character varying", 20, false)
	requireColumn(t, tx, "evaluation_runs", "control_epoch", "bigint", 0, false)
	requireColumn(t, tx, "evaluation_runs", "state_version", "bigint", 0, false)
	requireColumn(t, tx, "evaluation_runs", "route_profile_version", "character varying", 100, false)
	for _, table := range []string{
		"evaluation_assignments",
		"evaluation_grading_jobs",
		"evaluation_analysis_jobs",
	} {
		requireColumn(t, tx, table, "lease_epoch", "bigint", 0, false)
		requireColumn(t, tx, table, "worker_image_digest", "character varying", 200, false)
		requireColumn(t, tx, table, "work_origin", "character varying", 20, false)
	}
	requireColumn(t, tx, "evaluation_score_heads", "score_created_at", "timestamp with time zone", 0, false)
	requireColumn(t, tx, "evaluation_run_events", "transition_version", "bigint", 0, true)
	requireColumn(t, tx, "evaluation_run_events", "from_status", "character varying", 24, true)
	requireColumn(t, tx, "evaluation_run_events", "to_status", "character varying", 24, true)
	requireColumn(t, tx, "evaluation_run_events", "control_epoch", "bigint", 0, false)
	requireColumn(t, tx, "evaluation_run_events", "idempotency_key", "character", 64, true)
	requireIndex(t, tx, "evaluation_request_manifests", "evaluation_request_manifests_manifest_sha256_key")
	requireIndex(t, tx, "evaluation_pair_specs", "evaluation_pair_specs_run_case_sample_repeat_key")
	requireIndex(t, tx, "evaluation_side_specs", "evaluation_side_specs_pair_side_key")
	requireIndex(t, tx, "evaluation_pair_bindings", "evaluation_pair_bindings_pair_spec_key")
}

func TestRadarControlPlaneConstraints(t *testing.T) {
	t.Run("published dataset content cannot be updated", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		_, err := tx.ExecContext(context.Background(), `
			UPDATE evaluation_dataset_versions
			SET manifest_sha256 = $1
			WHERE id = $2`, fmt.Sprintf("%064d", 3), fixture.datasetID)
		require.Error(t, err)
	})

	t.Run("published dataset cannot be deleted", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarDatasetFixture(t, tx, "synthetic")
		require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
			UPDATE evaluation_dataset_versions
			SET status = 'published', published_at = NOW()
			WHERE id = $1`, fixture.datasetID))
		_, err := tx.ExecContext(context.Background(),
			"DELETE FROM evaluation_dataset_versions WHERE id = $1", fixture.datasetID)
		require.Error(t, err)
	})

	t.Run("case cannot be added after publication", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		_, err := tx.ExecContext(context.Background(), `
			INSERT INTO evaluation_cases (
				id, dataset_version_id, case_key, capability_domain, priority, weight, sample_count,
				prompt_spec, expected_spec, execution_spec, grader_id, grader_version,
				content_sha256, confidentiality
			) VALUES ($1, $2, 'late-case', 'protocol', 'P0', 1, 1,
				'{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'exact', 'v1', $3, 'synthetic')`,
			uuid.New(), fixture.datasetID, fmt.Sprintf("%064d", 3))
		require.Error(t, err)
	})

	t.Run("published case payload cannot be updated", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		_, err := tx.ExecContext(context.Background(), `
			UPDATE evaluation_cases
			SET prompt_spec = '{"changed":true}'::jsonb
			WHERE id = $1`, fixture.caseID)
		require.Error(t, err)
	})

	t.Run("published case cannot be deleted", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarPublishedDatasetWithCaseFixture(t, tx)
		_, err := tx.ExecContext(context.Background(),
			"DELETE FROM evaluation_cases WHERE id = $1", fixture.caseID)
		require.Error(t, err)
	})

	t.Run("imported dataset cannot be published", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarDatasetFixture(t, tx, "imported")
		_, err := tx.ExecContext(context.Background(), `
			UPDATE evaluation_dataset_versions
			SET status = 'published', published_at = NOW()
			WHERE id = $1`, fixture.datasetID)
		require.Error(t, err)
	})

	t.Run("dataset with restricted case cannot be published", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarDatasetFixture(t, tx, "synthetic")
		require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
			INSERT INTO evaluation_cases (
				id, dataset_version_id, case_key, capability_domain, priority, weight, sample_count,
				encrypted_spec, execution_spec, grader_id, grader_version, content_sha256, confidentiality
			) VALUES ($1, $2, 'restricted-case', 'safety', 'P0', 1, 1,
				'encrypted', '{}'::jsonb, 'exact', 'v1', $3, 'restricted')`,
			uuid.New(), fixture.datasetID, fmt.Sprintf("%064d", 2)))
		_, err := tx.ExecContext(context.Background(), `
			UPDATE evaluation_dataset_versions
			SET status = 'published', published_at = NOW()
			WHERE id = $1`, fixture.datasetID)
		require.Error(t, err)
	})

	t.Run("terminal run cannot return to running", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		_, err := tx.ExecContext(context.Background(),
			"UPDATE evaluation_runs SET status = 'running' WHERE id = $1", fixture.runID)
		require.Error(t, err)
	})

	t.Run("negative plan budget is rejected", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		_, err := tx.ExecContext(context.Background(), `
			INSERT INTO evaluation_plans (
				id, name, dataset_version_id, trigger_type, model_matrix,
				max_run_cost, daily_cost_limit, max_concurrency, created_by
			) VALUES ($1, 'invalid-budget', $2, 'manual', '[]'::jsonb, -0.01, 1, 1, $3)`,
			uuid.New(), fixture.datasetID, fixture.actorID)
		require.Error(t, err)
	})

	t.Run("sample identity is unique within a run", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		_, err := tx.ExecContext(context.Background(), `
			INSERT INTO evaluation_samples (
				id, run_id, case_id, model_route, sample_index, priority, status, estimated_cost
			) VALUES ($1, $2, $3, 'candidate', 0, 'P0', 'pending', 0.01)`,
			uuid.New(), fixture.runID, fixture.caseID)
		require.Error(t, err)
	})

	t.Run("unknown failure class is rejected", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		_, err := tx.ExecContext(context.Background(),
			"UPDATE evaluation_samples SET failure_class = 'mystery_failure' WHERE id = $1", fixture.sampleID)
		require.Error(t, err)
	})
}

type radarControlPlaneConstraintFixture struct {
	actorID   int64
	datasetID uuid.UUID
	caseID    uuid.UUID
	runID     uuid.UUID
	sampleID  uuid.UUID
}

func insertRadarControlPlaneConstraintFixture(t *testing.T, tx *sql.Tx) radarControlPlaneConstraintFixture {
	t.Helper()
	ctx := context.Background()
	fixture := insertRadarPublishedDatasetWithCaseFixture(t, tx)
	fixture.runID = uuid.New()
	fixture.sampleID = uuid.New()
	planID := uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_plans (
			id, name, dataset_version_id, trigger_type, model_matrix,
			max_run_cost, daily_cost_limit, max_concurrency, created_by
		) VALUES ($1, 'constraint-plan', $2, 'manual', '[]'::jsonb, 1, 2, 1, $3)`,
		planID, fixture.datasetID, fixture.actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_runs (
			id, plan_id, trigger_source, baseline_ref, candidate_ref, status,
			budget_limit, reserved_cost, actual_cost, created_by, finished_at
		) VALUES ($1, $2, 'manual', '{}'::jsonb, '{}'::jsonb, 'completed',
			1, 0.01, 0.01, $3, NOW())`, fixture.runID, planID, fixture.actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_samples (
			id, run_id, case_id, model_route, sample_index, priority, status, estimated_cost
		) VALUES ($1, $2, $3, 'candidate', 0, 'P0', 'completed', 0.01)`,
		fixture.sampleID, fixture.runID, fixture.caseID))
	return fixture
}

func insertRadarPublishedDatasetWithCaseFixture(t *testing.T, tx *sql.Tx) radarControlPlaneConstraintFixture {
	t.Helper()
	ctx := context.Background()
	fixture := insertRadarDatasetFixture(t, tx, "synthetic")
	fixture.caseID = uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_cases (
			id, dataset_version_id, case_key, capability_domain, priority, weight, sample_count,
			prompt_spec, expected_spec, execution_spec, grader_id, grader_version,
			content_sha256, confidentiality
		) VALUES ($1, $2, 'case-1', 'protocol', 'P0', 1, 1,
			'{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'exact', 'v1', $3, 'synthetic')`,
		fixture.caseID, fixture.datasetID, fmt.Sprintf("%064d", 2)))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		UPDATE evaluation_dataset_versions
		SET status = 'published', published_at = NOW()
		WHERE id = $1`, fixture.datasetID))
	return fixture
}

func insertRadarDatasetFixture(t *testing.T, tx *sql.Tx, sourceType string) radarControlPlaneConstraintFixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()
	fixture := radarControlPlaneConstraintFixture{
		datasetID: uuid.New(),
		caseID:    uuid.New(),
		runID:     uuid.New(),
		sampleID:  uuid.New(),
	}
	require.NoError(t, tx.QueryRowContext(ctx, `
		INSERT INTO users (email, password_hash, role, balance, concurrency, status)
		VALUES ($1, 'radar-control-plane-test', 'admin', 0, 1, 'active')
		RETURNING id`, "radar-control-plane-"+suffix+"@example.com").Scan(&fixture.actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_dataset_versions (
			id, dataset_key, version, manifest_sha256, source_type, status, created_by
		) VALUES ($1, $2, 'v1', $3, $4, 'draft', $5)`,
		fixture.datasetID, "radar-constraints-"+suffix, fmt.Sprintf("%064d", 1), sourceType, fixture.actorID))
	return fixture
}

func execRadarFixtureSQL(ctx context.Context, tx *sql.Tx, query string, args ...any) error {
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func requireTable(t *testing.T, tx *sql.Tx, table string) {
	t.Helper()
	var regclass sql.NullString
	require.NoError(t, tx.QueryRowContext(context.Background(),
		"SELECT to_regclass('public.' || $1)", table).Scan(&regclass))
	require.True(t, regclass.Valid, "expected table %s to exist", table)
}

func TestMigrationsRunner_AuthIdentityAndPaymentSchemaStayAligned(t *testing.T) {
	tx := testTx(t)

	requireColumn(t, tx, "auth_identity_migration_reports", "report_type", "character varying", 80, false)
	requireColumn(t, tx, "users", "signup_source", "character varying", 20, false)
	requireColumnDefaultContains(t, tx, "users", "signup_source", "email")
	requireConstraintDefinitionContains(
		t,
		tx,
		"users",
		"users_signup_source_check",
		"signup_source",
		"'email'",
		"'linuxdo'",
		"'wechat'",
		"'oidc'",
	)

	requireForeignKeyOnDelete(t, tx, "auth_identities", "user_id", "users", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "auth_identity_channels", "identity_id", "auth_identities", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "pending_auth_sessions", "target_user_id", "users", "SET NULL")
	requireForeignKeyOnDelete(t, tx, "identity_adoption_decisions", "pending_auth_session_id", "pending_auth_sessions", "CASCADE")
	requireForeignKeyOnDelete(t, tx, "identity_adoption_decisions", "identity_id", "auth_identities", "SET NULL")

	requireIndex(t, tx, "payment_orders", "paymentorder_out_trade_no")
	requirePartialUniqueIndexDefinition(t, tx, "payment_orders", "paymentorder_out_trade_no", "out_trade_no", "WHERE")
	requireIndexAbsent(t, tx, "payment_orders", "paymentorder_out_trade_no_unique")
}

func requireIndex(t *testing.T, tx *sql.Tx, table, index string) {
	t.Helper()

	var exists bool
	err := tx.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1
	FROM pg_indexes
	WHERE schemaname = 'public'
	  AND tablename = $1
	  AND indexname = $2
)
`, table, index).Scan(&exists)
	require.NoError(t, err, "query pg_indexes for %s.%s", table, index)
	require.True(t, exists, "expected index %s on %s", index, table)
}

func requireIndexAbsent(t *testing.T, tx *sql.Tx, table, index string) {
	t.Helper()

	var exists bool
	err := tx.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1
	FROM pg_indexes
	WHERE schemaname = 'public'
	  AND tablename = $1
	  AND indexname = $2
)
`, table, index).Scan(&exists)
	require.NoError(t, err, "query pg_indexes for %s.%s", table, index)
	require.False(t, exists, "expected index %s on %s to be absent", index, table)
}

func requirePartialUniqueIndexDefinition(t *testing.T, tx *sql.Tx, table, index string, fragments ...string) {
	t.Helper()

	var (
		unique bool
		def    string
	)

	err := tx.QueryRowContext(context.Background(), `
SELECT
	i.indisunique,
	pg_get_indexdef(i.indexrelid)
FROM pg_class idx
JOIN pg_index i ON i.indexrelid = idx.oid
JOIN pg_class tbl ON tbl.oid = i.indrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND idx.relname = $2
`, table, index).Scan(&unique, &def)
	require.NoError(t, err, "query index definition for %s.%s", table, index)
	require.True(t, unique, "expected index %s on %s to be unique", index, table)

	for _, fragment := range fragments {
		require.Contains(t, def, fragment, "expected index definition for %s.%s to contain %q", table, index, fragment)
	}
}

func requireForeignKeyOnDelete(t *testing.T, tx *sql.Tx, table, column, refTable, expected string) {
	t.Helper()

	var actual string
	err := tx.QueryRowContext(context.Background(), `
SELECT CASE c.confdeltype
	WHEN 'a' THEN 'NO ACTION'
	WHEN 'r' THEN 'RESTRICT'
	WHEN 'c' THEN 'CASCADE'
	WHEN 'n' THEN 'SET NULL'
	WHEN 'd' THEN 'SET DEFAULT'
END
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
JOIN pg_class ref_tbl ON ref_tbl.oid = c.confrelid
JOIN pg_attribute attr ON attr.attrelid = tbl.oid AND attr.attnum = ANY(c.conkey)
WHERE ns.nspname = 'public'
  AND c.contype = 'f'
  AND tbl.relname = $1
  AND attr.attname = $2
  AND ref_tbl.relname = $3
LIMIT 1
`, table, column, refTable).Scan(&actual)
	require.NoError(t, err, "query foreign key action for %s.%s -> %s", table, column, refTable)
	require.Equal(t, expected, actual, "unexpected ON DELETE action for %s.%s -> %s", table, column, refTable)
}

func requireConstraintDefinitionContains(t *testing.T, tx *sql.Tx, table, constraint string, fragments ...string) {
	t.Helper()

	var def string
	err := tx.QueryRowContext(context.Background(), `
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public'
  AND tbl.relname = $1
  AND c.conname = $2
`, table, constraint).Scan(&def)
	require.NoError(t, err, "query constraint definition for %s.%s", table, constraint)

	for _, fragment := range fragments {
		require.Contains(t, def, fragment, "expected constraint definition for %s.%s to contain %q", table, constraint, fragment)
	}
}

func requireColumnDefaultContains(t *testing.T, tx *sql.Tx, table, column string, fragments ...string) {
	t.Helper()

	var columnDefault sql.NullString
	err := tx.QueryRowContext(context.Background(), `
SELECT column_default
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = $1
  AND column_name = $2
`, table, column).Scan(&columnDefault)
	require.NoError(t, err, "query column_default for %s.%s", table, column)
	require.True(t, columnDefault.Valid, "expected column_default for %s.%s", table, column)

	for _, fragment := range fragments {
		require.Contains(t, columnDefault.String, fragment, "expected default for %s.%s to contain %q", table, column, fragment)
	}
}

func requireColumn(t *testing.T, tx *sql.Tx, table, column, dataType string, maxLen int, nullable bool) {
	t.Helper()

	var row struct {
		DataType string
		MaxLen   sql.NullInt64
		Nullable string
	}

	err := tx.QueryRowContext(context.Background(), `
SELECT
  data_type,
  character_maximum_length,
  is_nullable
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = $1
  AND column_name = $2
`, table, column).Scan(&row.DataType, &row.MaxLen, &row.Nullable)
	require.NoError(t, err, "query information_schema.columns for %s.%s", table, column)
	require.Equal(t, dataType, row.DataType, "data_type mismatch for %s.%s", table, column)

	if maxLen > 0 {
		require.True(t, row.MaxLen.Valid, "expected maxLen for %s.%s", table, column)
		require.Equal(t, int64(maxLen), row.MaxLen.Int64, "maxLen mismatch for %s.%s", table, column)
	}

	if nullable {
		require.Equal(t, "YES", row.Nullable, "nullable mismatch for %s.%s", table, column)
	} else {
		require.Equal(t, "NO", row.Nullable, "nullable mismatch for %s.%s", table, column)
	}
}

func requireNumericColumn(t *testing.T, tx *sql.Tx, table, column string, precision, scale int) {
	t.Helper()

	var actualPrecision, actualScale sql.NullInt64
	err := tx.QueryRowContext(context.Background(), `
SELECT numeric_precision, numeric_scale
FROM information_schema.columns
WHERE table_schema = 'public'
  AND table_name = $1
  AND column_name = $2
`, table, column).Scan(&actualPrecision, &actualScale)
	require.NoError(t, err, "query numeric metadata for %s.%s", table, column)
	require.Equal(t, int64(precision), actualPrecision.Int64, "numeric precision mismatch for %s.%s", table, column)
	require.Equal(t, int64(scale), actualScale.Int64, "numeric scale mismatch for %s.%s", table, column)
}
