//go:build integration

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
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
		"evaluation_worker_events",
		"evaluation_schema_cutovers",
		"evaluation_writer_sessions",
		"evaluation_writer_protocol_audits",
		"evaluation_route_evidence_terminalization_outbox",
	} {
		requireTable(t, tx, table)
	}

	requireColumn(t, tx, "evaluation_runs", "budget_mode", "text", 0, false)
	requireColumn(t, tx, "evaluation_runs", "paused_from_status", "character varying", 24, true)
	requireColumn(t, tx, "evaluation_runs", "pause_reason", "text", 0, true)
	requireColumn(t, tx, "evaluation_runs", "control_epoch", "bigint", 0, false)
	requireColumn(t, tx, "evaluation_runs", "state_version", "bigint", 0, false)
	requireColumn(t, tx, "evaluation_runs", "cancelled_at", "timestamp with time zone", 0, true)
	requireColumn(t, tx, "evaluation_runs", "cancelled_by", "bigint", 0, true)
	requireColumn(t, tx, "evaluation_runs", "route_profile_version", "text", 0, false)
	for _, table := range []string{"evaluation_assignments", "evaluation_grading_jobs", "evaluation_analysis_jobs"} {
		requireColumn(t, tx, table, "lease_epoch", "bigint", 0, true)
		requireColumn(t, tx, table, "worker_image_digest", "text", 0, true)
		requireColumn(t, tx, table, "work_origin", "text", 0, true)
	}
	requireColumn(t, tx, "evaluation_run_events", "transition_version", "bigint", 0, true)
	requireColumn(t, tx, "evaluation_run_events", "from_status", "character varying", 24, true)
	requireColumn(t, tx, "evaluation_run_events", "to_status", "character varying", 24, true)
	requireColumn(t, tx, "evaluation_run_events", "control_epoch", "bigint", 0, false)
	requireColumn(t, tx, "evaluation_run_events", "idempotency_key", "character", 64, false)
	requireColumn(t, tx, "evaluation_score_heads", "score_created_at", "timestamp with time zone", 0, false)
	requireCompositeForeignKey(t, tx, "evaluation_score_heads", []string{"score_id", "score_created_at"}, "evaluation_scores", []string{"id", "created_at"})
	requireColumn(t, tx, "evaluation_route_evidence_terminalization_outbox", "control_epoch", "bigint", 0, false)
	requireColumn(t, tx, "evaluation_route_evidence_terminalization_outbox", "idempotency_key", "character", 64, false)
}

func TestMigration198TrustedGovernanceSchema(t *testing.T) {
	tx := testTx(t)

	for _, table := range []string{
		"evaluation_release_subjects",
		"evaluation_release_subject_events",
		"evaluation_gate_policy_approvals",
		"evaluation_gate_policy_heads",
		"evaluation_gate_policy_events",
		"evaluation_baseline_heads",
		"evaluation_baseline_events",
		"evaluation_gate_decision_heads",
		"evaluation_gate_decision_events",
		"evaluation_gate_reevaluation_outbox",
		"evaluation_gate_storage_modes",
	} {
		requireTable(t, tx, table)
	}
	requireColumn(t, tx, "evaluation_release_subjects", "canonical_subject_bytes", "bytea", 0, false)
	requireColumn(t, tx, "evaluation_release_subject_events", "sequence", "bigint", 0, false)
	requireColumn(t, tx, "evaluation_baseline_approvals", "effective_at", "timestamp with time zone", 0, false)
	requireColumn(t, tx, "evaluation_baseline_approvals", "expires_at", "timestamp with time zone", 0, false)

	requireColumns(t, tx, "evaluation_gate_decisions", []string{
		"release_subject_hash",
		"evidence_hash",
		"source_watermark",
		"supersedes_decision_id",
		"cause_set_hash",
	})
	requireIndex(t, tx, "evaluation_gate_decisions", "uq_evaluation_gate_decisions_natural")
	requireIndex(t, tx, "evaluation_gate_decisions", "uq_evaluation_gate_decisions_supersedes")
	requireColumn(t, tx, "evaluation_gate_decision_heads", "release_subject_hash", "character", 64, false)
	requireColumn(t, tx, "evaluation_gate_decision_heads", "decision_id", "uuid", 0, false)
}

func TestMigration198aBackfillsCanonicalReleaseSubjectBytes(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	schema := "migration_198a_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `CREATE SCHEMA `+schema))
	_, err := tx.ExecContext(ctx, `SELECT set_config('search_path', $1, true)`, schema+",public")
	require.NoError(t, err)
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
			CREATE TABLE users (id BIGINT PRIMARY KEY);
			CREATE TABLE evaluation_gate_policies (
				id UUID PRIMARY KEY,
				created_by BIGINT NOT NULL REFERENCES users(id),
				created_at TIMESTAMPTZ NOT NULL
			);
			CREATE TABLE evaluation_baseline_approvals (
				id UUID PRIMARY KEY,
				created_at TIMESTAMPTZ NOT NULL
			);
			CREATE TABLE evaluation_release_subjects (
				id UUID PRIMARY KEY,
			run_id UUID NOT NULL,
			subject_hash CHAR(64) NOT NULL,
			canonical_subject JSONB NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				UNIQUE (run_id, subject_hash)
			);
			CREATE TABLE evaluation_gate_policy_events (
				id UUID PRIMARY KEY,
				policy_id UUID NOT NULL REFERENCES evaluation_gate_policies(id),
				event_type VARCHAR(32) NOT NULL,
				policy_hash CHAR(64) NOT NULL,
				environment VARCHAR(64) NOT NULL,
				scope_type VARCHAR(32) NOT NULL,
				scope_id VARCHAR(200) NOT NULL,
				actor_id BIGINT REFERENCES users(id),
				created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				UNIQUE (policy_id, event_type, environment, scope_type, scope_id)
			);
			CREATE TABLE evaluation_baseline_events (
				id UUID PRIMARY KEY,
				baseline_id UUID NOT NULL,
				event_type VARCHAR(32) NOT NULL,
				evidence_hash CHAR(64) NOT NULL,
				environment VARCHAR(64) NOT NULL,
				scope_type VARCHAR(32) NOT NULL,
				scope_id VARCHAR(200) NOT NULL,
				actor_id BIGINT REFERENCES users(id),
				created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
			);
			CREATE UNIQUE INDEX uq_evaluation_baseline_events_identity
				ON evaluation_baseline_events
				(baseline_id, event_type, environment, scope_type, scope_id, COALESCE(actor_id, 0));
			CREATE TABLE evaluation_gate_policy_heads (
				environment VARCHAR(64) NOT NULL,
				scope_type VARCHAR(32) NOT NULL,
				scope_id VARCHAR(200) NOT NULL,
				policy_id UUID NOT NULL,
				event_id UUID NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY (environment, scope_type, scope_id)
			);
			CREATE TABLE evaluation_baseline_heads (
				environment VARCHAR(64) NOT NULL,
				scope_type VARCHAR(32) NOT NULL,
				scope_id VARCHAR(200) NOT NULL,
				model_route VARCHAR(200) NOT NULL,
				baseline_id UUID NOT NULL,
				event_id UUID NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY (environment, scope_type, scope_id, model_route)
			);
			CREATE TABLE evaluation_gate_storage_modes (
			id SMALLINT PRIMARY KEY,
			mode VARCHAR(24) NOT NULL
		);
			INSERT INTO evaluation_gate_storage_modes (id, mode) VALUES (1, 'compatibility');
		`))

	actorID := time.Now().UnixNano()
	policyID := uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `INSERT INTO users (id) VALUES ($1)`, actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_gate_policies (id, created_by, created_at)
		VALUES ($1, $2, transaction_timestamp() - INTERVAL '1 day')`, policyID, actorID))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_baseline_approvals (id, created_at)
		VALUES ($1, transaction_timestamp() - INTERVAL '1 day')`, uuid.New()))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		CREATE TRIGGER trg_evaluation_baseline_approvals_trusted_immutable
		BEFORE UPDATE OR DELETE ON evaluation_baseline_approvals
		FOR EACH ROW EXECUTE FUNCTION public.reject_immutable_evaluation_record()
	`))

	subject := releaseSubjectFixture()
	canonical, err := service.CanonicalizeReleaseSubject(subject)
	require.NoError(t, err)
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_release_subjects
			(id, run_id, subject_hash, canonical_subject)
		VALUES ($1, $2, $3, $4::jsonb)`,
		uuid.New(), uuid.New(), canonical.SHA256, string(canonical.Bytes)))

	migrationPath := filepath.Join("..", "..", "migrations", "198a_backfill_release_subject_canonical_bytes.sql")
	migrationSQL, err := os.ReadFile(migrationPath)
	require.NoError(t, err)
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `SAVEPOINT invalid_canonical_backfill`))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `UPDATE evaluation_release_subjects SET subject_hash=$1`, hashString("invalid-canonical-bytes")))
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.ErrorContains(t, err, "canonical byte backfill failed hash validation")
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `ROLLBACK TO SAVEPOINT invalid_canonical_backfill`))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		CREATE TRIGGER trg_evaluation_release_subjects_trusted_immutable
		BEFORE UPDATE OR DELETE ON evaluation_release_subjects
		FOR EACH ROW EXECUTE FUNCTION public.reject_immutable_evaluation_record()
	`))

	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err, "forward migration must remain idempotent")

	var stored []byte
	var storedHash, mode string
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT canonical_subject_bytes,
		       encode(sha256(canonical_subject_bytes), 'hex')
		FROM evaluation_release_subjects`).Scan(&stored, &storedHash))
	require.Equal(t, []byte(canonical.Bytes), stored)
	require.Equal(t, canonical.SHA256, storedHash)
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT mode FROM evaluation_gate_storage_modes WHERE id=1`).Scan(&mode))
	require.Equal(t, "compatibility", mode)
	var policyApprovals int
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_gate_policy_approvals
		WHERE policy_id=$1 AND approver_id=$2 AND role='quality_admin'
		  AND expires_at > effective_at`, policyID, actorID).Scan(&policyApprovals))
	require.Equal(t, 1, policyApprovals)
	var baselineValidityRows int
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_baseline_approvals
		WHERE effective_at IS NOT NULL AND expires_at > effective_at`).Scan(&baselineValidityRows))
	require.Equal(t, 1, baselineValidityRows)
	for _, table := range []string{"evaluation_release_subject_events", "evaluation_gate_policy_approvals"} {
		var exists bool
		require.NoError(t, tx.QueryRowContext(ctx, `
			SELECT to_regclass($1) IS NOT NULL`, schema+"."+table).Scan(&exists))
		require.True(t, exists, "forward migration must create %s", table)
	}
	var oldPolicyUnique, oldBaselineUnique bool
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_constraint
			WHERE conrelid='evaluation_gate_policy_events'::regclass AND contype='u'
		)`).Scan(&oldPolicyUnique))
	require.False(t, oldPolicyUnique)
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT to_regclass($1) IS NOT NULL`, schema+".uq_evaluation_baseline_events_identity").Scan(&oldBaselineUnique))
	require.False(t, oldBaselineUnique)
	for _, function := range []string{"advance_evaluation_gate_policy_head", "advance_evaluation_baseline_head"} {
		var definition string
		require.NoError(t, tx.QueryRowContext(ctx, `
			SELECT pg_get_functiondef(p.oid)
			FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
			WHERE n.nspname=$1 AND p.proname=$2`, schema, function).Scan(&definition))
		require.Contains(t, definition, "pg_advisory_xact_lock")
	}
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `SAVEPOINT immutable_after_backfill`))
	_, err = tx.ExecContext(ctx, `UPDATE evaluation_release_subjects SET canonical_subject_bytes=canonical_subject_bytes`)
	require.Error(t, err, "forward migration must restore the immutable trigger")
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `ROLLBACK TO SAVEPOINT immutable_after_backfill`))

	var nullable string
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT is_nullable
		FROM information_schema.columns
		WHERE table_schema=$1 AND table_name='evaluation_release_subjects'
		  AND column_name='canonical_subject_bytes'`, schema).Scan(&nullable))
	require.Equal(t, "NO", nullable)
}

func TestMigration199EvidenceRevisionSchema(t *testing.T) {
	tx := testTx(t)

	for _, table := range []string{
		"evaluation_request_semantics",
		"evaluation_evidence_signing_keys",
		"evaluation_revision_batches",
		"evaluation_revision_batch_requirements",
		"evaluation_score_head_events",
		"evaluation_analysis_job_score_inputs",
		"evaluation_analysis_job_snapshot_inputs",
		"evaluation_aggregate_snapshot_score_inputs",
		"evaluation_aggregate_snapshot_sources",
		"evaluation_aggregate_heads",
		"evaluation_outbox_events",
		"evaluation_outbox_event_causes",
	} {
		requireTable(t, tx, table)
	}
	requireColumns(t, tx, "evaluation_route_evidence", []string{
		"assignment_id",
		"request_ordinal",
		"lease_epoch",
		"request_manifest_id",
		"request_manifest_sha256",
		"request_semantics_id",
		"request_semantics_sha256",
		"evidence_revision",
		"terminal_at",
		"sealed_at",
		"payload_hash",
		"signing_key_id",
		"payload_hmac",
	})
	requireColumns(t, tx, "evaluation_grading_jobs", []string{
		"work_origin", "revision_batch_id", "grading_input_hash",
		"evidence_manifest_hash", "recovery_generation", "score_created_at",
	})
	requireColumns(t, tx, "evaluation_analysis_jobs", []string{
		"scope", "work_origin", "revision_batch_id", "input_set_hash",
		"input_score_refs", "input_snapshot_refs", "aggregate_revision", "cause_set_hash",
	})
	requireColumns(t, tx, "evaluation_aggregate_snapshots", []string{
		"analysis_job_id", "revision_batch_id", "input_set_hash", "aggregate_revision",
		"aggregate_hash", "score_refs", "source_head_event_ids",
		"origin_revision_batch_ids", "cause_set_hash",
	})

	for _, table := range []string{
		"evaluation_revision_batch_requirements",
		"evaluation_grading_jobs",
		"evaluation_analysis_jobs",
		"evaluation_score_head_events",
		"evaluation_aggregate_snapshots",
		"evaluation_outbox_events",
		"evaluation_outbox_event_causes",
	} {
		requireCompositeForeignKey(t, tx, table, []string{"revision_batch_id", "run_id"}, "evaluation_revision_batches", []string{"id", "run_id"})
	}

	requireCompositeForeignKey(t, tx, "evaluation_grading_jobs", []string{"score_id", "score_created_at"}, "evaluation_scores", []string{"id", "created_at"})
	requireCompositeForeignKey(t, tx, "evaluation_manual_reviews", []string{"score_id", "score_created_at"}, "evaluation_scores", []string{"id", "created_at"})
	requireCompositeForeignKey(t, tx, "evaluation_score_head_events", []string{"previous_score_id", "previous_score_created_at"}, "evaluation_scores", []string{"id", "created_at"})
	requireCompositeForeignKey(t, tx, "evaluation_score_head_events", []string{"score_id", "score_created_at"}, "evaluation_scores", []string{"id", "created_at"})
	requireCompositeForeignKey(t, tx, "evaluation_analysis_job_score_inputs", []string{"score_id", "score_created_at"}, "evaluation_scores", []string{"id", "created_at"})
	requireCompositeForeignKey(t, tx, "evaluation_aggregate_snapshot_score_inputs", []string{"score_id", "score_created_at"}, "evaluation_scores", []string{"id", "created_at"})
	requireCompositeForeignKey(t, tx, "evaluation_aggregate_heads", []string{"snapshot_id", "window_start"}, "evaluation_aggregate_snapshots", []string{"id", "window_start"})
	requireCompositeForeignKey(t, tx, "evaluation_analysis_job_snapshot_inputs", []string{"snapshot_id", "window_start"}, "evaluation_aggregate_snapshots", []string{"id", "window_start"})
	requireCompositeForeignKey(t, tx, "evaluation_aggregate_snapshot_sources", []string{"source_snapshot_id", "source_window_start"}, "evaluation_aggregate_snapshots", []string{"id", "window_start"})

	requireIndex(t, tx, "evaluation_route_evidence", "uq_evaluation_route_evidence_assignment_ordinal")
	requireIndex(t, tx, "evaluation_revision_batches", "uq_evaluation_revision_batches_active_run")
	requireConstraintDefinitionContains(t, tx, "evaluation_grading_jobs", "evaluation_grading_jobs_origin_batch_check", "work_origin", "revision_batch_id")
	requireConstraintDefinitionContains(t, tx, "evaluation_analysis_jobs", "evaluation_analysis_jobs_origin_batch_check", "work_origin", "revision_batch_id")
}

func TestMigration199EvidenceRevisionSQLIsIdempotent(t *testing.T) {
	tx := testTx(t)
	migrationPath := filepath.Join("..", "..", "migrations", "199_add_radar_evidence_revision_pipeline.sql")
	migrationSQL, err := os.ReadFile(migrationPath)
	require.NoError(t, err)
	require.NoError(t, execRadarFixtureSQL(context.Background(), tx, string(migrationSQL)))
}

func TestMigration201RevisionBatchEventsSchema(t *testing.T) {
	tx := testTx(t)
	requireTable(t, tx, "evaluation_revision_batch_events")
	requireColumns(t, tx, "evaluation_revision_batch_events", []string{
		"id", "revision_batch_id", "run_id", "event_type", "actor_id",
		"control_epoch", "idempotency_key", "payload", "created_at",
	})
	requireCompositeForeignKey(t, tx, "evaluation_revision_batch_events",
		[]string{"revision_batch_id", "run_id"}, "evaluation_revision_batches", []string{"id", "run_id"})
	requireIndex(t, tx, "evaluation_revision_batch_events", "idx_evaluation_revision_batch_events_approvals")
	var immutableTrigger, writerTrigger bool
	require.NoError(t, tx.QueryRowContext(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_trigger
			WHERE tgrelid='evaluation_revision_batch_events'::regclass
			  AND tgname='trg_evaluation_revision_batch_events_immutable'
			  AND NOT tgisinternal
		)`).Scan(&immutableTrigger))
	require.True(t, immutableTrigger)
	require.NoError(t, tx.QueryRowContext(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_trigger
			WHERE tgrelid='evaluation_revision_batch_events'::regclass
			  AND tgname='trg_evaluation_revision_batch_events_writer_protocol'
			  AND NOT tgisinternal
		)`).Scan(&writerTrigger))
	require.True(t, writerTrigger)

	migrationPath := filepath.Join("..", "..", "migrations", "201_add_revision_batch_events.sql")
	migrationSQL, err := os.ReadFile(migrationPath)
	require.NoError(t, err)
	require.NoError(t, execRadarFixtureSQL(context.Background(), tx, string(migrationSQL)))
}

func TestMigration199GateEvidenceSchema(t *testing.T) {
	tx := testTx(t)
	for _, table := range []string{
		"evaluation_reliability_snapshots",
		"evaluation_reliability_head_events",
		"evaluation_reliability_heads",
		"evaluation_gate_evidence_manifests",
		"evaluation_release_authorizations",
		"evaluation_break_glass_requests",
		"evaluation_release_projections",
	} {
		requireTable(t, tx, table)
	}
	requireColumns(t, tx, "evaluation_reliability_snapshots", []string{
		"run_id", "reliability_profile_id", "slice_key", "window_start", "window_end",
		"query_version", "source_hash", "metrics", "snapshot_hash", "fresh_until",
	})
	requireColumns(t, tx, "evaluation_gate_evidence_manifests", []string{
		"canonical_manifest_bytes", "evidence_hash", "source_watermark",
		"loader_version", "release_subject_hash", "cause_set_hash",
	})
	requireColumns(t, tx, "evaluation_release_authorizations", []string{
		"decision_id", "release_subject_hash", "source_watermark", "waiver_ids",
		"nonce", "expires_at", "consumed_at",
	})
	requireCompositeForeignKey(t, tx, "evaluation_reliability_head_events", []string{"snapshot_id", "run_id"}, "evaluation_reliability_snapshots", []string{"id", "run_id"})
	requireCompositeForeignKey(t, tx, "evaluation_reliability_heads", []string{"snapshot_id", "run_id"}, "evaluation_reliability_snapshots", []string{"id", "run_id"})
	requireCompositeForeignKey(t, tx, "evaluation_reliability_heads", []string{"head_event_id", "run_id"}, "evaluation_reliability_head_events", []string{"id", "run_id"})
	requireCompositeForeignKey(t, tx, "evaluation_gate_evidence_manifests", []string{"release_subject_id", "run_id", "release_subject_hash"}, "evaluation_release_subjects", []string{"id", "run_id", "subject_hash"})
	requireCompositeForeignKey(t, tx, "evaluation_release_authorizations", []string{"decision_id", "release_subject_hash"}, "evaluation_gate_decisions", []string{"id", "release_subject_hash"})
}

func requireColumns(t *testing.T, tx *sql.Tx, table string, columns []string) {
	t.Helper()
	for _, column := range columns {
		var exists bool
		err := tx.QueryRowContext(context.Background(), `
SELECT EXISTS (
	SELECT 1 FROM information_schema.columns
	WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
)`, table, column).Scan(&exists)
		require.NoError(t, err)
		require.True(t, exists, "expected column %s.%s to exist", table, column)
	}
}

func requireCompositeForeignKey(t *testing.T, tx *sql.Tx, table string, columns []string, refTable string, refColumns []string) {
	t.Helper()
	var definitions []string
	rows, err := tx.QueryContext(context.Background(), `
SELECT pg_get_constraintdef(c.oid)
FROM pg_constraint c
JOIN pg_class tbl ON tbl.oid = c.conrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'public' AND tbl.relname = $1 AND c.contype = 'f'`, table)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var definition string
		require.NoError(t, rows.Scan(&definition))
		definitions = append(definitions, definition)
	}
	require.NoError(t, rows.Err())
	joined := strings.Join(definitions, "\n")
	require.Contains(t, joined, fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s(%s)", strings.Join(columns, ", "), refTable, strings.Join(refColumns, ", ")))
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

func TestMigration197TrustedLifecycleConstraints(t *testing.T) {
	t.Run("request manifest rejects delete", func(t *testing.T) {
		tx := testTx(t)
		manifestID := uuid.New()
		manifestHash := strings.Repeat("a", 64)
		require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
			INSERT INTO evaluation_request_manifests (id, schema_version, interaction_type, canonical_manifest_bytes, manifest_sha256)
			VALUES ($1, 'radar-request-manifest-v1', 'single', decode('7b7d', 'hex'), $2)`, manifestID, manifestHash))
		_, err := tx.ExecContext(context.Background(), `DELETE FROM evaluation_request_manifests WHERE id = $1`, manifestID)
		require.Error(t, err)
	})

	t.Run("immutable pair spec rejects update", func(t *testing.T) {
		tx := testTx(t)
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		manifestID := uuid.New()
		pairID := uuid.New()
		manifestHash := strings.Repeat("a", 64)
		require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
			INSERT INTO evaluation_request_manifests (id, schema_version, interaction_type, canonical_manifest_bytes, manifest_sha256)
			VALUES ($1, 'radar-request-manifest-v1', 'single', decode('7b7d', 'hex'), $2)`, manifestID, manifestHash))
		require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
			INSERT INTO evaluation_pair_specs (
				id, run_id, case_id, sample_index, repeat_index, request_manifest_id,
				request_manifest_sha256, canonical_spec, pair_spec_hash
			) VALUES ($1, $2, $3, 0, 0, $4, $5, '{}'::jsonb, $6)`,
			pairID, fixture.runID, fixture.caseID, manifestID, manifestHash, strings.Repeat("b", 64)))
		_, err := tx.ExecContext(context.Background(), `UPDATE evaluation_pair_specs SET canonical_spec = '{"changed":true}'::jsonb WHERE id = $1`, pairID)
		require.Error(t, err)
	})

	t.Run("run transition without event is rejected at commit", func(t *testing.T) {
		tx, err := integrationDB.BeginTx(context.Background(), nil)
		require.NoError(t, err)
		defer tx.Rollback()
		fixture := insertRadarControlPlaneConstraintFixture(t, tx)
		runID := uuid.New()
		var planID uuid.UUID
		require.NoError(t, tx.QueryRowContext(context.Background(), `SELECT plan_id FROM evaluation_runs WHERE id = $1`, fixture.runID).Scan(&planID))
		require.NoError(t, execRadarFixtureSQL(context.Background(), tx, `
			INSERT INTO evaluation_runs (id, plan_id, trigger_source, baseline_ref, candidate_ref, status, budget_limit, created_by)
			VALUES ($1, $2, 'manual', '{}'::jsonb, '{}'::jsonb, 'pending', 1, $3)`, runID, planID, fixture.actorID))
		_, err = tx.ExecContext(context.Background(), `UPDATE evaluation_runs SET status = 'running' WHERE id = $1`, runID)
		require.NoError(t, err)
		require.Error(t, tx.Commit())
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
