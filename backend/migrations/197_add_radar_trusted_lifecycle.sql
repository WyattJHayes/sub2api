-- Radar trusted execution lifecycle expand phase.
-- This migration adds immutable input identities, run control epochs, worker
-- lifecycle metadata, and writer cutover state. Enforcement is enabled by the
-- protocol-aware writers during the staged cutover.

ALTER TABLE evaluation_runs
    ADD COLUMN IF NOT EXISTS budget_mode VARCHAR(20) NOT NULL DEFAULT 'normal',
    ADD COLUMN IF NOT EXISTS paused_from_status VARCHAR(24),
    ADD COLUMN IF NOT EXISTS pause_reason VARCHAR(100),
    ADD COLUMN IF NOT EXISTS control_epoch BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS state_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancelled_by BIGINT REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS route_profile_version VARCHAR(100) NOT NULL DEFAULT 'legacy-unbound';

ALTER TABLE evaluation_runs
    DROP CONSTRAINT IF EXISTS evaluation_runs_budget_mode_check,
    ADD CONSTRAINT evaluation_runs_budget_mode_check
        CHECK (budget_mode IN ('normal', 'exact_p0_drain')),
    DROP CONSTRAINT IF EXISTS evaluation_runs_control_epoch_check,
    ADD CONSTRAINT evaluation_runs_control_epoch_check CHECK (control_epoch >= 0),
    DROP CONSTRAINT IF EXISTS evaluation_runs_state_version_check,
    ADD CONSTRAINT evaluation_runs_state_version_check CHECK (state_version >= 0);

ALTER TABLE evaluation_workers
    ADD COLUMN IF NOT EXISTS region VARCHAR(64) NOT NULL DEFAULT 'legacy-unknown',
    ADD COLUMN IF NOT EXISTS image_digest VARCHAR(200) NOT NULL DEFAULT 'legacy-unknown',
    ADD COLUMN IF NOT EXISTS claim_mode VARCHAR(20) NOT NULL DEFAULT 'open',
    ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;

ALTER TABLE evaluation_workers
    DROP CONSTRAINT IF EXISTS evaluation_workers_claim_mode_check,
    ADD CONSTRAINT evaluation_workers_claim_mode_check
        CHECK (claim_mode IN ('open', 'paused', 'draining'));

ALTER TABLE evaluation_assignments
    ADD COLUMN IF NOT EXISTS lease_epoch BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS worker_image_digest VARCHAR(200) NOT NULL DEFAULT 'legacy-unknown',
    ADD COLUMN IF NOT EXISTS work_origin VARCHAR(20) NOT NULL DEFAULT 'initial';

ALTER TABLE evaluation_assignments
    DROP CONSTRAINT IF EXISTS evaluation_assignments_lease_epoch_check,
    ADD CONSTRAINT evaluation_assignments_lease_epoch_check CHECK (lease_epoch >= 0),
    DROP CONSTRAINT IF EXISTS evaluation_assignments_work_origin_check,
    ADD CONSTRAINT evaluation_assignments_work_origin_check
        CHECK (work_origin IN ('initial', 'regrade'));

ALTER TABLE evaluation_grading_jobs
    ADD COLUMN IF NOT EXISTS lease_epoch BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS worker_image_digest VARCHAR(200) NOT NULL DEFAULT 'legacy-unknown',
    ADD COLUMN IF NOT EXISTS work_origin VARCHAR(20) NOT NULL DEFAULT 'initial';

ALTER TABLE evaluation_grading_jobs
    DROP CONSTRAINT IF EXISTS evaluation_grading_jobs_status_check,
    ADD CONSTRAINT evaluation_grading_jobs_status_check
        CHECK (status IN ('pending', 'leased', 'completed', 'failed', 'cancelled')),
    DROP CONSTRAINT IF EXISTS evaluation_grading_jobs_lease_epoch_check,
    ADD CONSTRAINT evaluation_grading_jobs_lease_epoch_check CHECK (lease_epoch >= 0),
    DROP CONSTRAINT IF EXISTS evaluation_grading_jobs_work_origin_check,
    ADD CONSTRAINT evaluation_grading_jobs_work_origin_check
        CHECK (work_origin IN ('initial', 'regrade'));

ALTER TABLE evaluation_analysis_jobs
    ADD COLUMN IF NOT EXISTS lease_epoch BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS worker_image_digest VARCHAR(200) NOT NULL DEFAULT 'legacy-unknown',
    ADD COLUMN IF NOT EXISTS work_origin VARCHAR(20) NOT NULL DEFAULT 'initial';

ALTER TABLE evaluation_analysis_jobs
    DROP CONSTRAINT IF EXISTS evaluation_analysis_jobs_status_check,
    ADD CONSTRAINT evaluation_analysis_jobs_status_check
        CHECK (status IN ('pending', 'leased', 'completed', 'failed', 'cancelled')),
    DROP CONSTRAINT IF EXISTS evaluation_analysis_jobs_lease_epoch_check,
    ADD CONSTRAINT evaluation_analysis_jobs_lease_epoch_check CHECK (lease_epoch >= 0),
    DROP CONSTRAINT IF EXISTS evaluation_analysis_jobs_work_origin_check,
    ADD CONSTRAINT evaluation_analysis_jobs_work_origin_check
        CHECK (work_origin IN ('initial', 'regrade'));

ALTER TABLE evaluation_score_heads
    ADD COLUMN IF NOT EXISTS score_created_at TIMESTAMPTZ;

DO $$
DECLARE
    missing_count BIGINT;
    duplicate_count BIGINT;
BEGIN
    SELECT COUNT(*) INTO missing_count
    FROM evaluation_score_heads h
    LEFT JOIN evaluation_scores s ON s.id = h.score_id
    WHERE h.score_created_at IS NULL AND s.id IS NULL;
    IF missing_count > 0 THEN
        RAISE EXCEPTION 'migration 197 cannot locate % score head rows', missing_count;
    END IF;

    SELECT COUNT(*) INTO duplicate_count
    FROM (
        SELECT h.sample_id, h.grader_id, h.score_id
        FROM evaluation_score_heads h
        JOIN evaluation_scores s ON s.id = h.score_id
        GROUP BY h.sample_id, h.grader_id, h.score_id
        HAVING COUNT(*) FILTER (WHERE s.created_at IS NOT NULL) > 1
    ) duplicates;
    IF duplicate_count > 0 THEN
        RAISE EXCEPTION 'migration 197 found % ambiguous score head locators', duplicate_count;
    END IF;

    UPDATE evaluation_score_heads h
    SET score_created_at = s.created_at
    FROM evaluation_scores s
    WHERE h.score_id = s.id AND h.score_created_at IS NULL;
END $$;

ALTER TABLE evaluation_score_heads
    ALTER COLUMN score_created_at SET NOT NULL;

ALTER TABLE evaluation_run_events
    ADD COLUMN IF NOT EXISTS transition_version BIGINT,
    ADD COLUMN IF NOT EXISTS from_status VARCHAR(24),
    ADD COLUMN IF NOT EXISTS to_status VARCHAR(24),
    ADD COLUMN IF NOT EXISTS control_epoch BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS idempotency_key CHAR(64);

ALTER TABLE evaluation_run_events
    DROP CONSTRAINT IF EXISTS evaluation_run_events_transition_version_check,
    ADD CONSTRAINT evaluation_run_events_transition_version_check
        CHECK (transition_version IS NULL OR transition_version > 0),
    DROP CONSTRAINT IF EXISTS evaluation_run_events_control_epoch_check,
    ADD CONSTRAINT evaluation_run_events_control_epoch_check CHECK (control_epoch >= 0),
    DROP CONSTRAINT IF EXISTS evaluation_run_events_idempotency_key_check,
    ADD CONSTRAINT evaluation_run_events_idempotency_key_check
        CHECK (idempotency_key IS NULL OR idempotency_key ~ '^[0-9a-f]{64}$');

CREATE UNIQUE INDEX IF NOT EXISTS idx_evaluation_run_events_transition
    ON evaluation_run_events (run_id, transition_version)
    WHERE transition_version IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_evaluation_run_events_idempotency
    ON evaluation_run_events (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS evaluation_request_manifests (
    id UUID PRIMARY KEY,
    schema_version VARCHAR(100) NOT NULL,
    interaction_type VARCHAR(20) NOT NULL CHECK (interaction_type IN ('single', 'multi_turn', 'agent')),
    canonical_manifest_bytes BYTEA NOT NULL,
    manifest_sha256 CHAR(64) NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT evaluation_request_manifests_manifest_sha256_key UNIQUE (manifest_sha256),
    CONSTRAINT evaluation_request_manifests_id_hash_key UNIQUE (id, manifest_sha256)
);

CREATE TABLE IF NOT EXISTS evaluation_pair_specs (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    case_id UUID NOT NULL REFERENCES evaluation_cases(id),
    sample_index INT NOT NULL CHECK (sample_index >= 0),
    repeat_index INT NOT NULL DEFAULT 0 CHECK (repeat_index >= 0),
    request_manifest_id UUID NOT NULL,
    request_manifest_sha256 CHAR(64) NOT NULL CHECK (request_manifest_sha256 ~ '^[0-9a-f]{64}$'),
    schema_version VARCHAR(100) NOT NULL,
    canonical_spec JSONB NOT NULL,
    pair_spec_hash CHAR(64) NOT NULL CHECK (pair_spec_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT evaluation_pair_specs_run_case_sample_repeat_key
        UNIQUE (run_id, case_id, sample_index, repeat_index),
    CONSTRAINT evaluation_pair_specs_manifest_fk
        FOREIGN KEY (request_manifest_id, request_manifest_sha256)
        REFERENCES evaluation_request_manifests (id, manifest_sha256)
);

CREATE TABLE IF NOT EXISTS evaluation_side_specs (
    id UUID PRIMARY KEY,
    pair_spec_id UUID NOT NULL REFERENCES evaluation_pair_specs(id),
    sample_id UUID NOT NULL REFERENCES evaluation_samples(id),
    side VARCHAR(20) NOT NULL CHECK (side IN ('baseline', 'candidate')),
    schema_version VARCHAR(100) NOT NULL,
    canonical_spec JSONB NOT NULL,
    side_spec_hash CHAR(64) NOT NULL CHECK (side_spec_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT evaluation_side_specs_pair_side_key UNIQUE (pair_spec_id, side),
    CONSTRAINT evaluation_side_specs_sample_key UNIQUE (sample_id)
);

CREATE TABLE IF NOT EXISTS evaluation_pair_bindings (
    id UUID PRIMARY KEY,
    pair_spec_id UUID NOT NULL REFERENCES evaluation_pair_specs(id),
    baseline_side_spec_id UUID NOT NULL REFERENCES evaluation_side_specs(id),
    candidate_side_spec_id UUID NOT NULL REFERENCES evaluation_side_specs(id),
    pair_binding_hash CHAR(64) NOT NULL CHECK (pair_binding_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT evaluation_pair_bindings_pair_spec_key UNIQUE (pair_spec_id),
    CONSTRAINT evaluation_pair_bindings_sides_distinct_check
        CHECK (baseline_side_spec_id <> candidate_side_spec_id)
);

CREATE OR REPLACE FUNCTION prevent_evaluation_immutable_record_mutation()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% % is immutable', TG_TABLE_NAME, OLD.id;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_request_manifests_immutable ON evaluation_request_manifests;
CREATE TRIGGER trg_evaluation_request_manifests_immutable
    BEFORE UPDATE OR DELETE ON evaluation_request_manifests
    FOR EACH ROW EXECUTE FUNCTION prevent_evaluation_immutable_record_mutation();
DROP TRIGGER IF EXISTS trg_evaluation_pair_specs_immutable ON evaluation_pair_specs;
CREATE TRIGGER trg_evaluation_pair_specs_immutable
    BEFORE UPDATE OR DELETE ON evaluation_pair_specs
    FOR EACH ROW EXECUTE FUNCTION prevent_evaluation_immutable_record_mutation();
DROP TRIGGER IF EXISTS trg_evaluation_side_specs_immutable ON evaluation_side_specs;
CREATE TRIGGER trg_evaluation_side_specs_immutable
    BEFORE UPDATE OR DELETE ON evaluation_side_specs
    FOR EACH ROW EXECUTE FUNCTION prevent_evaluation_immutable_record_mutation();
DROP TRIGGER IF EXISTS trg_evaluation_pair_bindings_immutable ON evaluation_pair_bindings;
CREATE TRIGGER trg_evaluation_pair_bindings_immutable
    BEFORE UPDATE OR DELETE ON evaluation_pair_bindings
    FOR EACH ROW EXECUTE FUNCTION prevent_evaluation_immutable_record_mutation();

CREATE TABLE IF NOT EXISTS evaluation_schema_cutovers (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    write_mode VARCHAR(20) NOT NULL DEFAULT 'open'
        CHECK (write_mode IN ('open', 'draining', 'closed')),
    guard_mode VARCHAR(20) NOT NULL DEFAULT 'audit'
        CHECK (guard_mode IN ('audit', 'enforce')),
    minimum_protocol_version BIGINT NOT NULL DEFAULT 0 CHECK (minimum_protocol_version >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
INSERT INTO evaluation_schema_cutovers (id) VALUES (1) ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS evaluation_writer_sessions (
    id UUID PRIMARY KEY,
    instance_id VARCHAR(200) NOT NULL UNIQUE,
    writer_kind VARCHAR(32) NOT NULL,
    protocol_version BIGINT NOT NULL CHECK (protocol_version >= 0),
    active_lease_count INT NOT NULL DEFAULT 0 CHECK (active_lease_count >= 0),
    last_transaction_at TIMESTAMPTZ,
    heartbeat_expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_worker_events (
    id UUID PRIMARY KEY,
    worker_id UUID NOT NULL REFERENCES evaluation_workers(id),
    event_type VARCHAR(80) NOT NULL,
    idempotency_key CHAR(64),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT evaluation_worker_events_idempotency_key_key UNIQUE (idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_evaluation_pair_specs_run ON evaluation_pair_specs (run_id, created_at);
CREATE INDEX IF NOT EXISTS idx_evaluation_side_specs_pair ON evaluation_side_specs (pair_spec_id, side);
CREATE INDEX IF NOT EXISTS idx_evaluation_worker_sessions_expiry ON evaluation_writer_sessions (heartbeat_expires_at);
CREATE INDEX IF NOT EXISTS idx_evaluation_worker_events_worker_created
    ON evaluation_worker_events (worker_id, created_at DESC);
