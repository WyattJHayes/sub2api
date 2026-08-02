-- Radar reliability, controlled fault experiments, and recovery evidence.
-- This migration extends the migration 199 reliability tables in place.

CREATE TABLE IF NOT EXISTS evaluation_load_plans (
    id UUID PRIMARY KEY,
    schema_version VARCHAR(64) NOT NULL
        CHECK (schema_version = 'radar-load-plan-v1'),
    tenant_id BIGINT NOT NULL,
    canonical_plan_bytes BYTEA NOT NULL
        CHECK (octet_length(canonical_plan_bytes) > 0),
    load_plan_sha256 CHAR(64) NOT NULL
        CHECK (load_plan_sha256 ~ '^[0-9a-f]{64}$'),
    status VARCHAR(16) NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'published', 'retired')),
    created_by BIGINT NOT NULL REFERENCES users(id),
    published_at TIMESTAMPTZ,
    retired_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (load_plan_sha256),
    CHECK ((status = 'published' AND published_at IS NOT NULL) OR status <> 'published'),
    CHECK ((status = 'retired' AND retired_at IS NOT NULL) OR status <> 'retired'),
    CHECK (retired_at IS NULL OR published_at IS NOT NULL)
);

ALTER TABLE evaluation_reliability_snapshots
    ADD COLUMN IF NOT EXISTS load_plan_id UUID,
    ADD COLUMN IF NOT EXISTS source_watermark CHAR(64),
    ADD COLUMN IF NOT EXISTS request_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS success_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS error_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS timeout_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS retry_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS protocol_error_count BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS billing_idempotency_failures BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS ttft_histogram_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS latency_histogram_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS p99_latency_ms BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS error_rate NUMERIC(12,9) NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cost_amount NUMERIC(20,8) NOT NULL DEFAULT 0;

UPDATE evaluation_reliability_snapshots
SET source_watermark = source_hash
WHERE source_watermark IS NULL;

UPDATE evaluation_reliability_snapshots
SET ttft_histogram_hash = repeat('0', 64)
WHERE ttft_histogram_hash IS NULL;

UPDATE evaluation_reliability_snapshots
SET latency_histogram_hash = COALESCE(NULLIF(metrics->>'histogram_or_sketch_hash', ''), repeat('0', 64))
WHERE latency_histogram_hash IS NULL;

ALTER TABLE evaluation_reliability_snapshots
    ALTER COLUMN source_watermark SET NOT NULL,
    ALTER COLUMN ttft_histogram_hash SET NOT NULL,
    ALTER COLUMN latency_histogram_hash SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_reliability_snapshots'::regclass
          AND conname = 'fk_evaluation_reliability_snapshots_load_plan'
    ) THEN
        ALTER TABLE evaluation_reliability_snapshots
            ADD CONSTRAINT fk_evaluation_reliability_snapshots_load_plan
            FOREIGN KEY (load_plan_id) REFERENCES evaluation_load_plans(id)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_reliability_snapshots'::regclass
          AND conname = 'evaluation_reliability_snapshots_source_watermark_check'
    ) THEN
        ALTER TABLE evaluation_reliability_snapshots
            ADD CONSTRAINT evaluation_reliability_snapshots_source_watermark_check
            CHECK (source_watermark ~ '^[0-9a-f]{64}$');
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_reliability_snapshots'::regclass
          AND conname = 'evaluation_reliability_snapshots_denominator_check'
    ) THEN
        ALTER TABLE evaluation_reliability_snapshots
            ADD CONSTRAINT evaluation_reliability_snapshots_denominator_check
            CHECK (
                request_count >= 0
                AND success_count >= 0
                AND error_count >= 0
                AND timeout_count >= 0
                AND retry_count >= 0
                AND protocol_error_count >= 0
                AND billing_idempotency_failures >= 0
                AND p99_latency_ms >= 0
                AND error_rate >= 0 AND error_rate <= 1
                AND cost_amount >= 0
                AND success_count + error_count + timeout_count <= request_count
            );
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_evaluation_reliability_snapshots_load_plan_window
    ON evaluation_reliability_snapshots (run_id, load_plan_id, slice_key, window_end);

CREATE TABLE IF NOT EXISTS evaluation_fault_experiments (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    load_plan_id UUID REFERENCES evaluation_load_plans(id),
    fault_kind VARCHAR(40) NOT NULL
        CHECK (fault_kind IN (
            'worker_kill', 'worker_network_isolation', 'upstream_latency',
            'redis_partition', 'artifact_store_outage'
        )),
    target_kind VARCHAR(40) NOT NULL
        CHECK (target_kind IN ('worker', 'upstream', 'redis', 'artifact_store')),
    target_ref VARCHAR(200) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'proposed'
        CHECK (status IN ('proposed', 'approved', 'running', 'aborted', 'completed', 'failed', 'cancelled')),
    requested_by BIGINT NOT NULL REFERENCES users(id),
    approved_by BIGINT REFERENCES users(id),
    approval_reason TEXT,
    abort_deadline TIMESTAMPTZ,
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (id, run_id),
    CHECK ((status IN ('approved', 'running', 'aborted', 'completed', 'failed', 'cancelled') AND approved_by IS NOT NULL) OR status = 'proposed'),
    CHECK ((status IN ('running', 'aborted', 'completed', 'failed', 'cancelled') AND started_at IS NOT NULL) OR status IN ('proposed', 'approved')),
    CHECK ((status IN ('aborted', 'completed', 'failed', 'cancelled') AND finished_at IS NOT NULL) OR status NOT IN ('aborted', 'completed', 'failed', 'cancelled'))
);

ALTER TABLE evaluation_fault_experiments
    ADD COLUMN IF NOT EXISTS environment VARCHAR(40) NOT NULL DEFAULT 'staging';

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_fault_experiments'::regclass
          AND conname = 'evaluation_fault_experiments_environment_check'
    ) THEN
        ALTER TABLE evaluation_fault_experiments
            ADD CONSTRAINT evaluation_fault_experiments_environment_check
            CHECK (environment IN ('staging', 'preproduction', 'production'));
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS evaluation_fault_experiment_events (
    id UUID PRIMARY KEY,
    experiment_id UUID NOT NULL,
    run_id UUID NOT NULL,
    event_type VARCHAR(24) NOT NULL
        CHECK (event_type IN ('proposed', 'approved', 'started', 'aborted', 'completed', 'failed', 'cancelled')),
    actor_id BIGINT REFERENCES users(id),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    event_hash CHAR(64) NOT NULL
        CHECK (event_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (id, run_id),
    CONSTRAINT fk_evaluation_fault_experiment_events_experiment
        FOREIGN KEY (experiment_id, run_id)
        REFERENCES evaluation_fault_experiments(id, run_id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX IF NOT EXISTS idx_evaluation_fault_experiment_events_experiment
    ON evaluation_fault_experiment_events (experiment_id, created_at, id);

CREATE TABLE IF NOT EXISTS evaluation_recovery_evidence (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    experiment_id UUID NOT NULL,
    recovery_generation INT NOT NULL DEFAULT 0 CHECK (recovery_generation >= 0),
    source_watermark CHAR(64) NOT NULL
        CHECK (source_watermark ~ '^[0-9a-f]{64}$'),
    canonical_evidence_bytes BYTEA NOT NULL
        CHECK (octet_length(canonical_evidence_bytes) > 0),
    evidence_hash CHAR(64) NOT NULL
        CHECK (evidence_hash ~ '^[0-9a-f]{64}$'),
    status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'verified', 'rejected')),
    rpo_ms BIGINT CHECK (rpo_ms IS NULL OR rpo_ms >= 0),
    rto_ms BIGINT CHECK (rto_ms IS NULL OR rto_ms >= 0),
    duplicate_score_count INT NOT NULL DEFAULT 0 CHECK (duplicate_score_count >= 0),
    deterministic_run_id UUID REFERENCES evaluation_runs(id),
    verified_by BIGINT REFERENCES users(id),
    verified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (experiment_id, recovery_generation, evidence_hash),
    CONSTRAINT fk_evaluation_recovery_evidence_experiment
        FOREIGN KEY (experiment_id, run_id)
        REFERENCES evaluation_fault_experiments(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CHECK ((status = 'verified' AND verified_at IS NOT NULL AND verified_by IS NOT NULL) OR status <> 'verified'),
    CHECK ((status = 'verified' AND deterministic_run_id IS NOT NULL) OR status <> 'verified')
);

ALTER TABLE evaluation_recovery_evidence
    ADD COLUMN IF NOT EXISTS source_observation_id UUID;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_recovery_evidence'::regclass
          AND conname = 'fk_evaluation_recovery_evidence_source_observation'
    ) THEN
        ALTER TABLE evaluation_recovery_evidence
            ADD CONSTRAINT fk_evaluation_recovery_evidence_source_observation
            FOREIGN KEY (source_observation_id)
            REFERENCES evaluation_recovery_evidence(id);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_evaluation_recovery_evidence_source_observation
    ON evaluation_recovery_evidence (source_observation_id, created_at);

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_load_plans'::regclass
          AND conname = 'uq_evaluation_load_plans_id_run'
    ) THEN
        ALTER TABLE evaluation_load_plans ADD CONSTRAINT uq_evaluation_load_plans_id_run UNIQUE (id, tenant_id);
    END IF;
END $$;

CREATE OR REPLACE FUNCTION enforce_evaluation_load_plan_update()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evaluation load plan rows cannot be deleted';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.schema_version IS DISTINCT FROM OLD.schema_version
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.canonical_plan_bytes IS DISTINCT FROM OLD.canonical_plan_bytes
        OR NEW.load_plan_sha256 IS DISTINCT FROM OLD.load_plan_sha256
        OR NEW.created_by IS DISTINCT FROM OLD.created_by
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'evaluation load plan identity is immutable';
    END IF;
    IF OLD.status = 'retired' AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'retired evaluation load plans cannot change status';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_load_plans_immutable ON evaluation_load_plans;
CREATE TRIGGER trg_evaluation_load_plans_immutable
    BEFORE UPDATE OR DELETE ON evaluation_load_plans
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_load_plan_update();

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_load_plans',
        'evaluation_fault_experiment_events',
        'evaluation_recovery_evidence'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_writer_protocol ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_writer_protocol BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION audit_evaluation_writer_protocol()', table_name, table_name);
    END LOOP;
END $$;

DROP TRIGGER IF EXISTS trg_evaluation_fault_experiment_events_immutable ON evaluation_fault_experiment_events;
CREATE TRIGGER trg_evaluation_fault_experiment_events_immutable
    BEFORE UPDATE OR DELETE ON evaluation_fault_experiment_events
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record();

DROP TRIGGER IF EXISTS trg_evaluation_recovery_evidence_immutable ON evaluation_recovery_evidence;
CREATE TRIGGER trg_evaluation_recovery_evidence_immutable
    BEFORE UPDATE OR DELETE ON evaluation_recovery_evidence
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record();

CREATE INDEX IF NOT EXISTS idx_evaluation_fault_experiments_run_status
    ON evaluation_fault_experiments (run_id, status, created_at);

CREATE INDEX IF NOT EXISTS idx_evaluation_recovery_evidence_run_generation
    ON evaluation_recovery_evidence (run_id, recovery_generation, created_at);

ALTER TABLE evaluation_artifacts
    ADD COLUMN IF NOT EXISTS scan_reason TEXT,
    ADD COLUMN IF NOT EXISTS scan_provider VARCHAR(80),
    ADD COLUMN IF NOT EXISTS scanned_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ;

ALTER TABLE evaluation_artifacts
    DROP CONSTRAINT IF EXISTS evaluation_artifacts_scan_status_check,
    DROP CONSTRAINT IF EXISTS evaluation_artifacts_scan_status_check_v2;

ALTER TABLE evaluation_artifacts
    ADD CONSTRAINT evaluation_artifacts_scan_status_check_v2
    CHECK (scan_status IN ('pending', 'clean', 'rejected', 'failed'));

-- Rows created by the pre-object-store protocol have no scanner identity. They
-- must be re-uploaded and scanned before a future grading lease can consume
-- them. New rows remain pending until ConfirmArtifact completes both checks.
UPDATE evaluation_artifacts
SET scan_status = 'pending', confirmed_at = NULL
WHERE scan_status = 'clean' AND scan_provider IS NULL;

CREATE INDEX IF NOT EXISTS idx_evaluation_artifacts_scan_pending
    ON evaluation_artifacts (scan_status, scanned_at, retention_deadline)
    WHERE scan_status IN ('pending', 'failed');

CREATE INDEX IF NOT EXISTS idx_evaluation_artifacts_deleted
    ON evaluation_artifacts (deleted_at, retention_deadline)
    WHERE deleted_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_evaluation_artifacts_cleanup_due
    ON evaluation_artifacts (retention_deadline, id)
    WHERE deleted_at IS NULL;

-- Tenant ownership is carried by every Radar top-level resource. Legacy rows
-- that cannot be attributed safely use tenant 0 and remain invisible to scoped
-- API queries until an operator explicitly rehomes them.
DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_role_bindings',
        'evaluation_dataset_versions',
        'evaluation_plans',
        'evaluation_runs',
        'evaluation_workers',
        'evaluation_reliability_snapshots',
        'evaluation_fault_experiments',
        'evaluation_recovery_evidence',
        'evaluation_gate_policies',
        'evaluation_gate_decisions',
        'evaluation_release_subjects',
        'evaluation_baselines',
        'evaluation_artifacts',
        'evaluation_alerts'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ADD COLUMN IF NOT EXISTS tenant_id BIGINT NOT NULL DEFAULT 0', table_name);
    END LOOP;
END $$;

-- The tenant column is deployment metadata, so backfilling it must not be
-- interpreted as changing an already immutable Radar record. Lock each table
-- through this transaction, suspend user triggers for the backfill, and restore
-- them before the migration commits.
DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_role_bindings',
        'evaluation_dataset_versions',
        'evaluation_plans',
        'evaluation_runs',
        'evaluation_workers',
        'evaluation_reliability_snapshots',
        'evaluation_fault_experiments',
        'evaluation_recovery_evidence',
        'evaluation_gate_policies',
        'evaluation_gate_decisions',
        'evaluation_release_subjects',
        'evaluation_baselines',
        'evaluation_artifacts',
        'evaluation_alerts'
    ] LOOP
        EXECUTE format('ALTER TABLE %I DISABLE TRIGGER USER', table_name);
    END LOOP;
END $$;

UPDATE evaluation_role_bindings
SET tenant_id = actor_id
WHERE tenant_id = 0 AND actor_id > 0;

UPDATE evaluation_dataset_versions
SET tenant_id = created_by
WHERE tenant_id = 0 AND created_by > 0;

UPDATE evaluation_plans
SET tenant_id = created_by
WHERE tenant_id = 0 AND created_by > 0;

UPDATE evaluation_runs r
SET tenant_id = p.tenant_id
FROM evaluation_plans p
WHERE r.tenant_id = 0 AND p.id = r.plan_id AND p.tenant_id > 0;

-- Worker event actor_id identifies the administrative user, so it cannot be
-- used as a tenant. Recover a legacy worker only when every historical lease
-- relation points to exactly one non-zero run tenant. Ambiguous or idle workers
-- remain at tenant 0 and require explicit operator rehoming.
WITH worker_run_tenants AS (
    SELECT a.leased_by AS worker_id, r.tenant_id
    FROM evaluation_assignments a
    JOIN evaluation_samples s ON s.id = a.sample_id
    JOIN evaluation_runs r ON r.id = s.run_id
    WHERE a.leased_by IS NOT NULL AND r.tenant_id > 0
    UNION
    SELECT g.leased_by AS worker_id, r.tenant_id
    FROM evaluation_grading_jobs g
    JOIN evaluation_runs r ON r.id = g.run_id
    WHERE g.leased_by IS NOT NULL AND r.tenant_id > 0
    UNION
    SELECT j.leased_by AS worker_id, r.tenant_id
    FROM evaluation_analysis_jobs j
    JOIN evaluation_runs r ON r.id = j.run_id
    WHERE j.leased_by IS NOT NULL AND r.tenant_id > 0
), worker_tenants AS (
    SELECT worker_id, MIN(tenant_id) AS tenant_id, COUNT(*) AS tenant_count
    FROM worker_run_tenants
    GROUP BY worker_id
)
UPDATE evaluation_workers w
SET tenant_id = wt.tenant_id
FROM worker_tenants wt
WHERE w.tenant_id = 0 AND w.id = wt.worker_id AND wt.tenant_count = 1;

UPDATE evaluation_reliability_snapshots s
SET tenant_id = r.tenant_id
FROM evaluation_runs r
WHERE s.tenant_id = 0 AND r.id = s.run_id AND r.tenant_id > 0;

UPDATE evaluation_fault_experiments e
SET tenant_id = r.tenant_id
FROM evaluation_runs r
WHERE e.tenant_id = 0 AND r.id = e.run_id AND r.tenant_id > 0;

UPDATE evaluation_recovery_evidence e
SET tenant_id = r.tenant_id
FROM evaluation_runs r
WHERE e.tenant_id = 0 AND r.id = e.run_id AND r.tenant_id > 0;

UPDATE evaluation_gate_policies
SET tenant_id = created_by
WHERE tenant_id = 0 AND created_by > 0;

UPDATE evaluation_gate_decisions d
SET tenant_id = r.tenant_id
FROM evaluation_runs r
WHERE d.tenant_id = 0 AND r.id = d.run_id AND r.tenant_id > 0;

UPDATE evaluation_release_subjects s
SET tenant_id = r.tenant_id
FROM evaluation_runs r
WHERE s.tenant_id = 0 AND r.id = s.run_id AND r.tenant_id > 0;

UPDATE evaluation_baselines
SET tenant_id = proposed_by
WHERE tenant_id = 0 AND proposed_by > 0;

UPDATE evaluation_artifacts a
SET tenant_id = r.tenant_id
FROM evaluation_runs r
WHERE a.tenant_id = 0 AND r.id = a.run_id AND r.tenant_id > 0;

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_role_bindings',
        'evaluation_dataset_versions',
        'evaluation_plans',
        'evaluation_runs',
        'evaluation_workers',
        'evaluation_reliability_snapshots',
        'evaluation_fault_experiments',
        'evaluation_recovery_evidence',
        'evaluation_gate_policies',
        'evaluation_gate_decisions',
        'evaluation_release_subjects',
        'evaluation_baselines',
        'evaluation_artifacts',
        'evaluation_alerts'
    ] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE TRIGGER USER', table_name);
    END LOOP;
END $$;

-- Alert history carries administrative actor ids and no resource tenant key.
-- Keep legacy alerts at tenant 0 until an operator provides an explicit
-- mapping; an actor id must never be interpreted as a tenant id.

-- The governance migration created a global alert uniqueness constraint. Radar
-- alerts are tenant-owned after this migration, so replace that constraint
-- before repository upserts start inferring conflicts by tenant.
ALTER TABLE evaluation_alerts
    DROP CONSTRAINT IF EXISTS evaluation_alerts_model_route_capability_domain_cause_policy_version_key;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_alerts'::regclass
          AND conname = 'uq_evaluation_alerts_tenant_scope'
    ) THEN
        ALTER TABLE evaluation_alerts
            ADD CONSTRAINT uq_evaluation_alerts_tenant_scope
            UNIQUE (tenant_id, model_route, capability_domain, cause, policy_version);
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_evaluation_role_bindings_tenant_actor
    ON evaluation_role_bindings (tenant_id, actor_id, enabled);
CREATE INDEX IF NOT EXISTS idx_evaluation_dataset_versions_tenant_created
    ON evaluation_dataset_versions (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_evaluation_plans_tenant_created
    ON evaluation_plans (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_evaluation_runs_tenant_created
    ON evaluation_runs (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_evaluation_workers_tenant_status
    ON evaluation_workers (tenant_id, status);
CREATE INDEX IF NOT EXISTS idx_evaluation_reliability_snapshots_tenant_run
    ON evaluation_reliability_snapshots (tenant_id, run_id, window_end DESC);
CREATE INDEX IF NOT EXISTS idx_evaluation_gate_decisions_tenant_run
    ON evaluation_gate_decisions (tenant_id, run_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_evaluation_release_subjects_tenant_run
    ON evaluation_release_subjects (tenant_id, run_id);
CREATE INDEX IF NOT EXISTS idx_evaluation_baselines_tenant_run
    ON evaluation_baselines (tenant_id, run_id);
CREATE INDEX IF NOT EXISTS idx_evaluation_artifacts_tenant_run
    ON evaluation_artifacts (tenant_id, run_id, id);
CREATE INDEX IF NOT EXISTS idx_evaluation_alerts_tenant_seen
    ON evaluation_alerts (tenant_id, first_seen_at DESC);
