-- Radar evidence revision pipeline expand schema.
-- Legacy records remain readable. Trusted writers populate the new binding,
-- partition locator, fencing, and causation fields.

CREATE TABLE IF NOT EXISTS evaluation_request_semantics (
    id UUID PRIMARY KEY,
    schema_version TEXT NOT NULL,
    canonical_semantics_bytes BYTEA NOT NULL,
    request_semantics_sha256 CHAR(64) NOT NULL
        CHECK (request_semantics_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (id, request_semantics_sha256),
    UNIQUE (request_semantics_sha256)
);

CREATE TABLE IF NOT EXISTS evaluation_evidence_signing_keys (
    id UUID PRIMARY KEY,
    key_reference TEXT NOT NULL UNIQUE,
    status VARCHAR(16) NOT NULL
        CHECK (status IN ('active', 'verify_only', 'revoked')),
    state_epoch BIGINT NOT NULL DEFAULT 1 CHECK (state_epoch > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    revoked_at TIMESTAMPTZ,
    CHECK ((status = 'revoked' AND revoked_at IS NOT NULL) OR status <> 'revoked')
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_evidence_signing_keys_active
    ON evaluation_evidence_signing_keys ((status)) WHERE status = 'active';

ALTER TABLE evaluation_route_evidence
    ADD COLUMN IF NOT EXISTS schema_version VARCHAR(80) NOT NULL DEFAULT 'legacy-unbound',
    ADD COLUMN IF NOT EXISTS canonicalization_version VARCHAR(80) NOT NULL DEFAULT 'legacy-unbound',
    ADD COLUMN IF NOT EXISTS assignment_id UUID,
    ADD COLUMN IF NOT EXISTS request_ordinal INT,
    ADD COLUMN IF NOT EXISTS lease_epoch BIGINT,
    ADD COLUMN IF NOT EXISTS request_manifest_id UUID,
    ADD COLUMN IF NOT EXISTS request_manifest_sha256 CHAR(64),
    ADD COLUMN IF NOT EXISTS request_slot_id VARCHAR(160),
    ADD COLUMN IF NOT EXISTS request_semantics_id UUID,
    ADD COLUMN IF NOT EXISTS request_semantics_sha256 CHAR(64),
    ADD COLUMN IF NOT EXISTS request_semantics_policy_sha256 CHAR(64),
    ADD COLUMN IF NOT EXISTS request_tool_schema_sha256 CHAR(64),
    ADD COLUMN IF NOT EXISTS request_allowed_tool_set_sha256 CHAR(64),
    ADD COLUMN IF NOT EXISTS evidence_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS terminal_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS sealed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS payload_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS signing_key_id UUID,
    ADD COLUMN IF NOT EXISTS payload_hmac CHAR(64),
    ADD COLUMN IF NOT EXISTS billing_status VARCHAR(20) NOT NULL DEFAULT 'incomplete',
    ADD COLUMN IF NOT EXISTS gateway_image_digest TEXT,
    ADD COLUMN IF NOT EXISTS incomplete_reason VARCHAR(80);

ALTER TABLE evaluation_route_evidence
    DROP CONSTRAINT IF EXISTS evaluation_route_evidence_request_ordinal_check,
    ADD CONSTRAINT evaluation_route_evidence_request_ordinal_check
        CHECK (request_ordinal IS NULL OR request_ordinal >= 0),
    DROP CONSTRAINT IF EXISTS evaluation_route_evidence_evidence_revision_check,
    ADD CONSTRAINT evaluation_route_evidence_evidence_revision_check
        CHECK (evidence_revision >= 0),
    DROP CONSTRAINT IF EXISTS evaluation_route_evidence_billing_status_check,
    ADD CONSTRAINT evaluation_route_evidence_billing_status_check
        CHECK (billing_status IN ('incomplete', 'complete', 'not_applicable')),
    DROP CONSTRAINT IF EXISTS evaluation_route_evidence_trusted_binding_check,
    ADD CONSTRAINT evaluation_route_evidence_trusted_binding_check CHECK (
        (assignment_id IS NULL
            AND request_ordinal IS NULL
            AND lease_epoch IS NULL
            AND request_manifest_id IS NULL
            AND request_manifest_sha256 IS NULL
            AND request_semantics_id IS NULL
            AND request_semantics_sha256 IS NULL)
        OR
        (assignment_id IS NOT NULL
            AND request_ordinal IS NOT NULL
            AND lease_epoch IS NOT NULL
            AND request_manifest_id IS NOT NULL
            AND request_manifest_sha256 IS NOT NULL
            AND request_slot_id IS NOT NULL
            AND request_semantics_id IS NOT NULL
            AND request_semantics_sha256 IS NOT NULL
            AND request_semantics_policy_sha256 IS NOT NULL
            AND request_tool_schema_sha256 IS NOT NULL
            AND request_allowed_tool_set_sha256 IS NOT NULL
            AND gateway_image_digest IS NOT NULL)
    ) NOT VALID,
    DROP CONSTRAINT IF EXISTS evaluation_route_evidence_seal_check,
    ADD CONSTRAINT evaluation_route_evidence_seal_check CHECK (
        (sealed_at IS NULL AND payload_hash IS NULL AND signing_key_id IS NULL AND payload_hmac IS NULL)
        OR
        (sealed_at IS NOT NULL
            AND terminal_at IS NOT NULL
            AND payload_hash ~ '^[0-9a-f]{64}$'
            AND signing_key_id IS NOT NULL
            AND payload_hmac ~ '^[0-9a-f]{64}$'
            AND terminal_at <= sealed_at
            AND (finished_at IS NULL OR finished_at <= terminal_at))
    ) NOT VALID;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_route_evidence_assignment') THEN
        ALTER TABLE evaluation_route_evidence
            ADD CONSTRAINT fk_evaluation_route_evidence_assignment
            FOREIGN KEY (assignment_id) REFERENCES evaluation_assignments(id)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_route_evidence_manifest') THEN
        ALTER TABLE evaluation_route_evidence
            ADD CONSTRAINT fk_evaluation_route_evidence_manifest
            FOREIGN KEY (request_manifest_id, request_manifest_sha256)
            REFERENCES evaluation_request_manifests(id, manifest_sha256)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_route_evidence_semantics') THEN
        ALTER TABLE evaluation_route_evidence
            ADD CONSTRAINT fk_evaluation_route_evidence_semantics
            FOREIGN KEY (request_semantics_id, request_semantics_sha256)
            REFERENCES evaluation_request_semantics(id, request_semantics_sha256)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_route_evidence_signing_key') THEN
        ALTER TABLE evaluation_route_evidence
            ADD CONSTRAINT fk_evaluation_route_evidence_signing_key
            FOREIGN KEY (signing_key_id) REFERENCES evaluation_evidence_signing_keys(id)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_release_subjects_full_identity
    ON evaluation_release_subjects (id, run_id, subject_hash);
CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_gate_decisions_subject_identity
    ON evaluation_gate_decisions (id, release_subject_hash);

CREATE TABLE IF NOT EXISTS evaluation_reliability_snapshots (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    reliability_profile_id VARCHAR(100) NOT NULL,
    slice_key VARCHAR(200) NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    window_end TIMESTAMPTZ NOT NULL,
    query_version VARCHAR(100) NOT NULL,
    source_hash CHAR(64) NOT NULL CHECK (source_hash ~ '^[0-9a-f]{64}$'),
    metrics JSONB NOT NULL,
    snapshot_hash CHAR(64) NOT NULL CHECK (snapshot_hash ~ '^[0-9a-f]{64}$'),
    fresh_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (
        run_id, reliability_profile_id, slice_key,
        window_start, window_end, source_hash
    ),
    UNIQUE (id, run_id),
    CHECK (window_start < window_end),
    CHECK (fresh_until > window_end)
);

CREATE TABLE IF NOT EXISTS evaluation_reliability_head_events (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    reliability_profile_id VARCHAR(100) NOT NULL,
    slice_key VARCHAR(200) NOT NULL,
    previous_snapshot_id UUID,
    snapshot_id UUID NOT NULL,
    snapshot_hash CHAR(64) NOT NULL CHECK (snapshot_hash ~ '^[0-9a-f]{64}$'),
    source_hash CHAR(64) NOT NULL CHECK (source_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (id, run_id),
    UNIQUE (run_id, reliability_profile_id, slice_key, snapshot_id),
    CONSTRAINT fk_evaluation_reliability_head_events_previous_snapshot
        FOREIGN KEY (previous_snapshot_id, run_id)
        REFERENCES evaluation_reliability_snapshots(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_reliability_head_events_snapshot
        FOREIGN KEY (snapshot_id, run_id)
        REFERENCES evaluation_reliability_snapshots(id, run_id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_reliability_heads (
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    reliability_profile_id VARCHAR(100) NOT NULL,
    slice_key VARCHAR(200) NOT NULL,
    snapshot_id UUID NOT NULL,
    head_event_id UUID NOT NULL,
    snapshot_hash CHAR(64) NOT NULL CHECK (snapshot_hash ~ '^[0-9a-f]{64}$'),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (run_id, reliability_profile_id, slice_key),
    CONSTRAINT fk_evaluation_reliability_heads_snapshot
        FOREIGN KEY (snapshot_id, run_id)
        REFERENCES evaluation_reliability_snapshots(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_reliability_heads_event
        FOREIGN KEY (head_event_id, run_id)
        REFERENCES evaluation_reliability_head_events(id, run_id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_gate_evidence_manifests (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    policy_id UUID NOT NULL REFERENCES evaluation_gate_policies(id),
    baseline_id UUID REFERENCES evaluation_baselines(id),
    release_subject_id UUID NOT NULL,
    release_subject_hash CHAR(64) NOT NULL
        CHECK (release_subject_hash ~ '^[0-9a-f]{64}$'),
    canonical_manifest_bytes BYTEA NOT NULL,
    evidence_hash CHAR(64) NOT NULL CHECK (evidence_hash ~ '^[0-9a-f]{64}$'),
    source_watermark CHAR(64) NOT NULL CHECK (source_watermark ~ '^[0-9a-f]{64}$'),
    loader_version VARCHAR(100) NOT NULL,
    cause_set_hash CHAR(64) NOT NULL CHECK (cause_set_hash ~ '^[0-9a-f]{64}$'),
    reliability_snapshot_refs JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (run_id, policy_id, release_subject_hash, evidence_hash),
    UNIQUE (id, run_id),
    CONSTRAINT fk_evaluation_gate_evidence_manifests_release_subject
        FOREIGN KEY (release_subject_id, run_id, release_subject_hash)
        REFERENCES evaluation_release_subjects(id, run_id, subject_hash)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_release_authorizations (
    id UUID PRIMARY KEY,
    decision_id UUID NOT NULL,
    release_subject_hash CHAR(64) NOT NULL
        CHECK (release_subject_hash ~ '^[0-9a-f]{64}$'),
    source_watermark CHAR(64) NOT NULL CHECK (source_watermark ~ '^[0-9a-f]{64}$'),
    waiver_ids UUID[] NOT NULL DEFAULT '{}',
    nonce CHAR(64) NOT NULL UNIQUE CHECK (nonce ~ '^[0-9a-f]{64}$'),
    issued_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK (expires_at > issued_at),
    CHECK (consumed_at IS NULL OR consumed_at >= issued_at),
    CONSTRAINT fk_evaluation_release_authorizations_decision_subject
        FOREIGN KEY (decision_id, release_subject_hash)
        REFERENCES evaluation_gate_decisions(id, release_subject_hash)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_break_glass_requests (
    id UUID PRIMARY KEY,
    decision_id UUID NOT NULL,
    release_subject_hash CHAR(64) NOT NULL
        CHECK (release_subject_hash ~ '^[0-9a-f]{64}$'),
    risk_class VARCHAR(32) NOT NULL
        CHECK (risk_class IN ('statistical_quality', 'quantified_reliability', 'p0_rollback')),
    business_reason TEXT NOT NULL,
    rollback_target TEXT NOT NULL,
    incident_id VARCHAR(100) NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'approved', 'rejected', 'expired', 'consumed')),
    requested_by BIGINT NOT NULL REFERENCES users(id),
    platform_admin_id BIGINT REFERENCES users(id),
    release_manager_id BIGINT REFERENCES users(id),
    security_owner_id BIGINT REFERENCES users(id),
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CHECK (expires_at > created_at),
    CHECK (
        platform_admin_id IS NULL OR release_manager_id IS NULL OR security_owner_id IS NULL
        OR (
            platform_admin_id <> release_manager_id
            AND platform_admin_id <> security_owner_id
            AND release_manager_id <> security_owner_id
        )
    ),
    CONSTRAINT fk_evaluation_break_glass_requests_decision_subject
        FOREIGN KEY (decision_id, release_subject_hash)
        REFERENCES evaluation_gate_decisions(id, release_subject_hash)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_release_projections (
    release_subject_id UUID PRIMARY KEY REFERENCES evaluation_release_subjects(id),
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    release_subject_hash CHAR(64) NOT NULL
        CHECK (release_subject_hash ~ '^[0-9a-f]{64}$'),
    decision_id UUID REFERENCES evaluation_gate_decisions(id),
    authorization_id UUID REFERENCES evaluation_release_authorizations(id),
    status VARCHAR(24) NOT NULL
        CHECK (status IN ('pending', 'authorized', 'deployed', 'degraded', 'blocked')),
    source_watermark CHAR(64) NOT NULL CHECK (source_watermark ~ '^[0-9a-f]{64}$'),
    cause_set_hash CHAR(64) NOT NULL CHECK (cause_set_hash ~ '^[0-9a-f]{64}$'),
    last_outbox_event_id UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT fk_evaluation_release_projections_subject
        FOREIGN KEY (release_subject_id, run_id, release_subject_hash)
        REFERENCES evaluation_release_subjects(id, run_id, subject_hash)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE OR REPLACE FUNCTION enforce_evaluation_release_authorization_update()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evaluation release authorization rows cannot be deleted';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.decision_id IS DISTINCT FROM OLD.decision_id
        OR NEW.release_subject_hash IS DISTINCT FROM OLD.release_subject_hash
        OR NEW.source_watermark IS DISTINCT FROM OLD.source_watermark
        OR NEW.waiver_ids IS DISTINCT FROM OLD.waiver_ids
        OR NEW.nonce IS DISTINCT FROM OLD.nonce
        OR NEW.issued_at IS DISTINCT FROM OLD.issued_at
        OR NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'evaluation release authorization identity is immutable';
    END IF;
    IF OLD.consumed_at IS NOT NULL OR NEW.consumed_at IS NULL THEN
        RAISE EXCEPTION 'evaluation release authorization can be consumed once';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_evaluation_release_authorizations_update ON evaluation_release_authorizations;
CREATE TRIGGER trg_evaluation_release_authorizations_update
    BEFORE UPDATE OR DELETE ON evaluation_release_authorizations
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_release_authorization_update();

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_reliability_snapshots',
        'evaluation_reliability_head_events',
        'evaluation_gate_evidence_manifests'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_gate_immutable ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_gate_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record()', table_name, table_name);
    END LOOP;
END $$;

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_reliability_snapshots', 'evaluation_reliability_head_events',
        'evaluation_reliability_heads', 'evaluation_gate_evidence_manifests',
        'evaluation_release_authorizations', 'evaluation_break_glass_requests',
        'evaluation_release_projections'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_writer_protocol ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_writer_protocol BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION audit_evaluation_writer_protocol()', table_name, table_name);
    END LOOP;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_route_evidence_assignment_ordinal
    ON evaluation_route_evidence (assignment_id, request_ordinal)
    WHERE assignment_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS evaluation_revision_batches (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'running', 'blocked', 'completed', 'failed', 'cancelled')),
    control_epoch BIGINT NOT NULL DEFAULT 1 CHECK (control_epoch > 0),
    reason VARCHAR(80) NOT NULL,
    requested_by BIGINT NOT NULL REFERENCES users(id),
    idempotency_key CHAR(64) NOT NULL UNIQUE
        CHECK (idempotency_key ~ '^[0-9a-f]{64}$'),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (id, run_id),
    CHECK (
        (status IN ('completed', 'failed', 'cancelled') AND finished_at IS NOT NULL)
        OR status NOT IN ('completed', 'failed', 'cancelled')
    ) NOT VALID
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_revision_batches_active_run
    ON evaluation_revision_batches (run_id)
    WHERE status IN ('pending', 'running', 'blocked');

CREATE TABLE IF NOT EXISTS evaluation_revision_batch_requirements (
    id UUID PRIMARY KEY,
    revision_batch_id UUID NOT NULL,
    run_id UUID NOT NULL,
    requirement_type VARCHAR(20) NOT NULL
        CHECK (requirement_type IN ('grading', 'cell', 'global', 'gate', 'alert', 'release')),
    target_key TEXT NOT NULL,
    source_assignment_id UUID REFERENCES evaluation_assignments(id),
    previous_score_id UUID,
    previous_score_created_at TIMESTAMPTZ,
    grader_id VARCHAR(100),
    grader_version VARCHAR(100),
    grading_input_hash CHAR(64),
    source_hash CHAR(64) NOT NULL CHECK (source_hash ~ '^[0-9a-f]{64}$'),
    cause_set_hash CHAR(64) NOT NULL CHECK (cause_set_hash ~ '^[0-9a-f]{64}$'),
    status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'completed', 'failed', 'superseded')),
    recovery_generation INT NOT NULL DEFAULT 0 CHECK (recovery_generation >= 0),
    replaces_requirement_id UUID UNIQUE,
    failure_code VARCHAR(100),
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (revision_batch_id, requirement_type, target_key, recovery_generation),
    CONSTRAINT fk_evaluation_revision_batch_requirements_batch
        FOREIGN KEY (revision_batch_id, run_id)
        REFERENCES evaluation_revision_batches(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_revision_batch_requirements_previous_score
        FOREIGN KEY (previous_score_id, previous_score_created_at)
        REFERENCES evaluation_scores(id, created_at)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_revision_batch_requirements_replacement
        FOREIGN KEY (replaces_requirement_id)
        REFERENCES evaluation_revision_batch_requirements(id)
        DEFERRABLE INITIALLY DEFERRED,
    CHECK (
        (previous_score_id IS NULL AND previous_score_created_at IS NULL)
        OR (previous_score_id IS NOT NULL AND previous_score_created_at IS NOT NULL)
    )
);

ALTER TABLE evaluation_grading_jobs
    ADD COLUMN IF NOT EXISTS revision_batch_id UUID,
    ADD COLUMN IF NOT EXISTS grading_input_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS evidence_manifest_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS recovery_generation INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS score_created_at TIMESTAMPTZ;
UPDATE evaluation_grading_jobs SET work_origin = 'initial' WHERE work_origin IS NULL;
UPDATE evaluation_grading_jobs j
SET score_created_at = (
    SELECT MIN(s.created_at) FROM evaluation_scores s WHERE s.id = j.score_id
)
WHERE j.score_id IS NOT NULL
  AND j.score_created_at IS NULL
  AND (SELECT COUNT(*) FROM evaluation_scores s WHERE s.id = j.score_id) = 1;
ALTER TABLE evaluation_grading_jobs
    ALTER COLUMN work_origin SET DEFAULT 'initial',
    ALTER COLUMN work_origin SET NOT NULL,
    DROP CONSTRAINT IF EXISTS evaluation_grading_jobs_origin_batch_check,
    ADD CONSTRAINT evaluation_grading_jobs_origin_batch_check CHECK (
        (work_origin IN ('initial', 'reclaimed') AND revision_batch_id IS NULL)
        OR
        (work_origin = 'regrade' AND revision_batch_id IS NOT NULL
            AND grading_input_hash IS NOT NULL AND evidence_manifest_hash IS NOT NULL)
    ) NOT VALID,
    DROP CONSTRAINT IF EXISTS evaluation_grading_jobs_recovery_generation_check,
    ADD CONSTRAINT evaluation_grading_jobs_recovery_generation_check
        CHECK (recovery_generation >= 0),
    DROP CONSTRAINT IF EXISTS evaluation_grading_jobs_score_ref_check,
    ADD CONSTRAINT evaluation_grading_jobs_score_ref_check CHECK (
        (score_id IS NULL AND score_created_at IS NULL)
        OR (score_id IS NOT NULL AND score_created_at IS NOT NULL)
    ) NOT VALID;

ALTER TABLE evaluation_grading_jobs
    DROP CONSTRAINT IF EXISTS evaluation_grading_jobs_assignment_id_key;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_grading_jobs_revision_batch') THEN
        ALTER TABLE evaluation_grading_jobs
            ADD CONSTRAINT fk_evaluation_grading_jobs_revision_batch
            FOREIGN KEY (revision_batch_id, run_id)
            REFERENCES evaluation_revision_batches(id, run_id)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_grading_jobs_score_ref') THEN
        ALTER TABLE evaluation_grading_jobs
            ADD CONSTRAINT fk_evaluation_grading_jobs_score_ref
            FOREIGN KEY (score_id, score_created_at)
            REFERENCES evaluation_scores(id, created_at)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_grading_jobs_initial_input
    ON evaluation_grading_jobs (assignment_id, grader_id, grading_input_hash) NULLS NOT DISTINCT
    WHERE work_origin = 'initial';
CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_grading_jobs_regrade_input
    ON evaluation_grading_jobs (
        revision_batch_id, assignment_id, grader_id, grading_input_hash, recovery_generation
    ) NULLS NOT DISTINCT
    WHERE work_origin = 'regrade';

-- Assignment expansion remains the durable source for legacy initial grading
-- jobs after the assignment-only uniqueness constraint is replaced.
CREATE OR REPLACE FUNCTION enqueue_evaluation_grading_job()
RETURNS TRIGGER AS $$
DECLARE
    sample_run_id UUID;
    case_grader_id VARCHAR(100);
    case_grader_version VARCHAR(100);
BEGIN
    SELECT s.run_id, c.grader_id, c.grader_version
    INTO sample_run_id, case_grader_id, case_grader_version
    FROM evaluation_samples s
    JOIN evaluation_cases c ON c.id = s.case_id
    WHERE s.id = NEW.sample_id;
    IF sample_run_id IS NULL THEN
        RETURN NEW;
    END IF;
    INSERT INTO evaluation_grading_jobs (
        id, run_id, sample_id, assignment_id, grader_id, grader_version,
        attempt, status, work_origin
    ) VALUES (
        md5(NEW.id::text || ':grading')::uuid, sample_run_id, NEW.sample_id,
        NEW.id, case_grader_id, case_grader_version, NEW.attempt, 'pending', 'initial'
    ) ON CONFLICT DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE evaluation_manual_reviews
    ADD COLUMN IF NOT EXISTS score_created_at TIMESTAMPTZ;
UPDATE evaluation_manual_reviews mr
SET score_created_at = (
    SELECT MIN(s.created_at) FROM evaluation_scores s WHERE s.id = mr.score_id
)
WHERE mr.score_created_at IS NULL
  AND (SELECT COUNT(*) FROM evaluation_scores s WHERE s.id = mr.score_id) = 1;
ALTER TABLE evaluation_manual_reviews
    DROP CONSTRAINT IF EXISTS evaluation_manual_reviews_complete_score_ref_check,
    ADD CONSTRAINT evaluation_manual_reviews_complete_score_ref_check
        CHECK (score_created_at IS NOT NULL) NOT VALID;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_manual_reviews_score_ref') THEN
        ALTER TABLE evaluation_manual_reviews
            ADD CONSTRAINT fk_evaluation_manual_reviews_score_ref
            FOREIGN KEY (score_id, score_created_at)
            REFERENCES evaluation_scores(id, created_at)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;

ALTER TABLE evaluation_analysis_jobs
    ADD COLUMN IF NOT EXISTS scope VARCHAR(12) NOT NULL DEFAULT 'cell',
    ADD COLUMN IF NOT EXISTS revision_batch_id UUID,
    ADD COLUMN IF NOT EXISTS input_set_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS input_score_refs JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS input_snapshot_refs JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS aggregate_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cause_set_hash CHAR(64);
UPDATE evaluation_analysis_jobs SET work_origin = 'initial' WHERE work_origin IS NULL;
ALTER TABLE evaluation_analysis_jobs
    ALTER COLUMN work_origin SET DEFAULT 'initial',
    ALTER COLUMN work_origin SET NOT NULL,
    DROP CONSTRAINT IF EXISTS evaluation_analysis_jobs_scope_check,
    ADD CONSTRAINT evaluation_analysis_jobs_scope_check CHECK (scope IN ('cell', 'global')),
    DROP CONSTRAINT IF EXISTS evaluation_analysis_jobs_origin_batch_check,
    ADD CONSTRAINT evaluation_analysis_jobs_origin_batch_check CHECK (
        (work_origin IN ('initial', 'reclaimed') AND revision_batch_id IS NULL)
        OR (work_origin = 'regrade' AND revision_batch_id IS NOT NULL)
    ) NOT VALID,
    DROP CONSTRAINT IF EXISTS evaluation_analysis_jobs_aggregate_revision_check,
    ADD CONSTRAINT evaluation_analysis_jobs_aggregate_revision_check
        CHECK (aggregate_revision >= 0);

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT c.conname
        FROM pg_constraint c
        WHERE c.conrelid = 'evaluation_analysis_jobs'::regclass
          AND c.contype = 'u'
          AND pg_get_constraintdef(c.oid) LIKE '%run_id, capability_domain, model_route, "window", analysis_version, window_start%'
    LOOP
        EXECUTE format('ALTER TABLE evaluation_analysis_jobs DROP CONSTRAINT %I', constraint_name);
    END LOOP;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_analysis_jobs_revision_batch') THEN
        ALTER TABLE evaluation_analysis_jobs
            ADD CONSTRAINT fk_evaluation_analysis_jobs_revision_batch
            FOREIGN KEY (revision_batch_id, run_id)
            REFERENCES evaluation_revision_batches(id, run_id)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_analysis_jobs_cell_input
    ON evaluation_analysis_jobs (
        run_id, capability_domain, model_route, analysis_version, input_set_hash
    ) NULLS NOT DISTINCT
    WHERE scope = 'cell';
CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_analysis_jobs_global_input
    ON evaluation_analysis_jobs (run_id, analysis_version, input_set_hash) NULLS NOT DISTINCT
    WHERE scope = 'global';

ALTER TABLE evaluation_scores
    ADD COLUMN IF NOT EXISTS grading_job_id UUID REFERENCES evaluation_grading_jobs(id),
    ADD COLUMN IF NOT EXISTS source_assignment_id UUID REFERENCES evaluation_assignments(id),
    ADD COLUMN IF NOT EXISTS route_evidence_set_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS route_evidence_refs JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS artifact_manifest_hash CHAR(64);
CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_scores_grading_job
    ON evaluation_scores (grading_job_id, created_at) WHERE grading_job_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS evaluation_score_head_events (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    sample_id UUID NOT NULL REFERENCES evaluation_samples(id),
    grader_id VARCHAR(100) NOT NULL,
    version INT NOT NULL CHECK (version > 0),
    previous_score_id UUID,
    previous_score_created_at TIMESTAMPTZ,
    score_id UUID NOT NULL,
    score_created_at TIMESTAMPTZ NOT NULL,
    source_assignment_id UUID NOT NULL REFERENCES evaluation_assignments(id),
    route_evidence_set_hash CHAR(64) NOT NULL
        CHECK (route_evidence_set_hash ~ '^[0-9a-f]{64}$'),
    reason VARCHAR(80) NOT NULL,
    actor_id BIGINT REFERENCES users(id),
    grading_job_id UUID REFERENCES evaluation_grading_jobs(id),
    revision_batch_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (sample_id, grader_id, version),
    UNIQUE (id, run_id),
    CONSTRAINT fk_evaluation_score_head_events_previous_score
        FOREIGN KEY (previous_score_id, previous_score_created_at)
        REFERENCES evaluation_scores(id, created_at)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_score_head_events_score
        FOREIGN KEY (score_id, score_created_at)
        REFERENCES evaluation_scores(id, created_at)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_score_head_events_revision_batch
        FOREIGN KEY (revision_batch_id, run_id)
        REFERENCES evaluation_revision_batches(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CHECK (
        (previous_score_id IS NULL AND previous_score_created_at IS NULL)
        OR (previous_score_id IS NOT NULL AND previous_score_created_at IS NOT NULL)
    )
);

CREATE TABLE IF NOT EXISTS evaluation_analysis_job_score_inputs (
    analysis_job_id UUID NOT NULL REFERENCES evaluation_analysis_jobs(id),
    input_ordinal INT NOT NULL CHECK (input_ordinal >= 0),
    score_id UUID NOT NULL,
    score_created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (analysis_job_id, input_ordinal),
    UNIQUE (analysis_job_id, score_id, score_created_at),
    CONSTRAINT fk_evaluation_analysis_job_score_inputs_score
        FOREIGN KEY (score_id, score_created_at)
        REFERENCES evaluation_scores(id, created_at)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_analysis_job_snapshot_inputs (
    analysis_job_id UUID NOT NULL REFERENCES evaluation_analysis_jobs(id),
    input_ordinal INT NOT NULL CHECK (input_ordinal >= 0),
    snapshot_id UUID NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (analysis_job_id, input_ordinal),
    UNIQUE (analysis_job_id, snapshot_id, window_start)
);

ALTER TABLE evaluation_aggregate_snapshots
    ADD COLUMN IF NOT EXISTS analysis_job_id UUID REFERENCES evaluation_analysis_jobs(id),
    ADD COLUMN IF NOT EXISTS revision_batch_id UUID,
    ADD COLUMN IF NOT EXISTS input_set_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS aggregate_revision BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS aggregate_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS score_refs JSONB NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS source_head_event_ids UUID[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS origin_revision_batch_ids UUID[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS cause_set_hash CHAR(64);
ALTER TABLE evaluation_aggregate_snapshots
    DROP CONSTRAINT IF EXISTS evaluation_aggregate_snapshots_aggregate_revision_check,
    ADD CONSTRAINT evaluation_aggregate_snapshots_aggregate_revision_check
        CHECK (aggregate_revision >= 0);
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_aggregate_snapshots_revision_batch') THEN
        ALTER TABLE evaluation_aggregate_snapshots
            ADD CONSTRAINT fk_evaluation_aggregate_snapshots_revision_batch
            FOREIGN KEY (revision_batch_id, run_id)
            REFERENCES evaluation_revision_batches(id, run_id)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;
CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_aggregate_snapshots_analysis_job
    ON evaluation_aggregate_snapshots (analysis_job_id, window_start)
    WHERE analysis_job_id IS NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_analysis_job_snapshot_inputs_snapshot') THEN
        ALTER TABLE evaluation_analysis_job_snapshot_inputs
            ADD CONSTRAINT fk_evaluation_analysis_job_snapshot_inputs_snapshot
            FOREIGN KEY (snapshot_id, window_start)
            REFERENCES evaluation_aggregate_snapshots(id, window_start)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS evaluation_aggregate_snapshot_score_inputs (
    snapshot_id UUID NOT NULL,
    snapshot_window_start TIMESTAMPTZ NOT NULL,
    input_ordinal INT NOT NULL CHECK (input_ordinal >= 0),
    score_id UUID NOT NULL,
    score_created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (snapshot_id, snapshot_window_start, input_ordinal),
    UNIQUE (snapshot_id, snapshot_window_start, score_id, score_created_at),
    CONSTRAINT fk_evaluation_aggregate_snapshot_score_inputs_snapshot
        FOREIGN KEY (snapshot_id, snapshot_window_start)
        REFERENCES evaluation_aggregate_snapshots(id, window_start)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_aggregate_snapshot_score_inputs_score
        FOREIGN KEY (score_id, score_created_at)
        REFERENCES evaluation_scores(id, created_at)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_aggregate_snapshot_sources (
    snapshot_id UUID NOT NULL,
    snapshot_window_start TIMESTAMPTZ NOT NULL,
    source_ordinal INT NOT NULL CHECK (source_ordinal >= 0),
    source_snapshot_id UUID NOT NULL,
    source_window_start TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (snapshot_id, snapshot_window_start, source_ordinal),
    UNIQUE (snapshot_id, snapshot_window_start, source_snapshot_id, source_window_start),
    CONSTRAINT fk_evaluation_aggregate_snapshot_sources_snapshot
        FOREIGN KEY (snapshot_id, snapshot_window_start)
        REFERENCES evaluation_aggregate_snapshots(id, window_start)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_aggregate_snapshot_sources_source
        FOREIGN KEY (source_snapshot_id, source_window_start)
        REFERENCES evaluation_aggregate_snapshots(id, window_start)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_aggregate_heads (
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    capability_domain VARCHAR(32) NOT NULL,
    canonical_model_route VARCHAR(200) NOT NULL,
    analysis_version VARCHAR(100) NOT NULL,
    snapshot_id UUID NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    aggregate_revision BIGINT NOT NULL CHECK (aggregate_revision > 0),
    input_set_hash CHAR(64) NOT NULL CHECK (input_set_hash ~ '^[0-9a-f]{64}$'),
    aggregate_hash CHAR(64) NOT NULL CHECK (aggregate_hash ~ '^[0-9a-f]{64}$'),
    revision_batch_id UUID,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (run_id, capability_domain, canonical_model_route, analysis_version),
    CONSTRAINT fk_evaluation_aggregate_heads_snapshot
        FOREIGN KEY (snapshot_id, window_start)
        REFERENCES evaluation_aggregate_snapshots(id, window_start)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_aggregate_heads_revision_batch
        FOREIGN KEY (revision_batch_id, run_id)
        REFERENCES evaluation_revision_batches(id, run_id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_outbox_events (
    id UUID PRIMARY KEY,
    sequence BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    event_type VARCHAR(80) NOT NULL,
    dedup_key CHAR(64) NOT NULL UNIQUE CHECK (dedup_key ~ '^[0-9a-f]{64}$'),
    causation_id CHAR(64) NOT NULL CHECK (causation_id ~ '^[0-9a-f]{64}$'),
    cause_set_hash CHAR(64) NOT NULL CHECK (cause_set_hash ~ '^[0-9a-f]{64}$'),
    work_origin VARCHAR(12) CHECK (work_origin IN ('initial', 'regrade')),
    revision_batch_id UUID,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    source_type VARCHAR(80) NOT NULL,
    source_id TEXT NOT NULL,
    source_hash CHAR(64) NOT NULL CHECK (source_hash ~ '^[0-9a-f]{64}$'),
    payload_hash CHAR(64) NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    status VARCHAR(16) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'leased', 'completed', 'dead_letter')),
    attempt INT NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    available_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    lease_token_hash CHAR(64),
    lease_owner UUID REFERENCES evaluation_workers(id),
    lease_expires_at TIMESTAMPTZ,
    lease_epoch BIGINT,
    last_error_code VARCHAR(100),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (id, run_id),
    CONSTRAINT fk_evaluation_outbox_events_revision_batch
        FOREIGN KEY (revision_batch_id, run_id)
        REFERENCES evaluation_revision_batches(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT evaluation_outbox_events_origin_batch_check CHECK (
        (work_origin = 'regrade' AND revision_batch_id IS NOT NULL)
        OR (work_origin IS DISTINCT FROM 'regrade' AND revision_batch_id IS NULL)
    ),
    CHECK (
        (status = 'leased' AND lease_token_hash IS NOT NULL
            AND lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR status <> 'leased'
    )
);
CREATE INDEX IF NOT EXISTS idx_evaluation_outbox_events_claim
    ON evaluation_outbox_events (available_at, sequence)
    WHERE status IN ('pending', 'leased');

CREATE TABLE IF NOT EXISTS evaluation_outbox_event_causes (
    event_id UUID NOT NULL,
    cause_event_id UUID NOT NULL,
    run_id UUID NOT NULL,
    revision_batch_id UUID,
    source_head_event_id UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    PRIMARY KEY (event_id, cause_event_id),
    CONSTRAINT fk_evaluation_outbox_event_causes_event
        FOREIGN KEY (event_id, run_id)
        REFERENCES evaluation_outbox_events(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_outbox_event_causes_cause
        FOREIGN KEY (cause_event_id, run_id)
        REFERENCES evaluation_outbox_events(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_outbox_event_causes_revision_batch
        FOREIGN KEY (revision_batch_id, run_id)
        REFERENCES evaluation_revision_batches(id, run_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_evaluation_outbox_event_causes_head_event
        FOREIGN KEY (source_head_event_id)
        REFERENCES evaluation_score_head_events(id)
        DEFERRABLE INITIALLY DEFERRED,
    CHECK (event_id <> cause_event_id)
);

CREATE OR REPLACE FUNCTION enforce_evaluation_route_evidence_revision_immutability()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.sealed_at IS NOT NULL THEN
            RAISE EXCEPTION 'sealed evaluation route evidence % is immutable', OLD.route_trace_id;
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.sealed_at IS NOT NULL THEN
        RAISE EXCEPTION 'sealed evaluation route evidence % is immutable', OLD.route_trace_id;
    END IF;
    IF OLD.assignment_id IS NOT NULL AND (
        NEW.route_trace_id IS DISTINCT FROM OLD.route_trace_id
        OR NEW.evaluation_run_id IS DISTINCT FROM OLD.evaluation_run_id
        OR NEW.sample_id IS DISTINCT FROM OLD.sample_id
        OR NEW.api_key_id IS DISTINCT FROM OLD.api_key_id
        OR NEW.schema_version IS DISTINCT FROM OLD.schema_version
        OR NEW.canonicalization_version IS DISTINCT FROM OLD.canonicalization_version
        OR NEW.assignment_id IS DISTINCT FROM OLD.assignment_id
        OR NEW.request_ordinal IS DISTINCT FROM OLD.request_ordinal
        OR NEW.lease_epoch IS DISTINCT FROM OLD.lease_epoch
        OR NEW.request_manifest_id IS DISTINCT FROM OLD.request_manifest_id
        OR NEW.request_manifest_sha256 IS DISTINCT FROM OLD.request_manifest_sha256
        OR NEW.request_slot_id IS DISTINCT FROM OLD.request_slot_id
        OR NEW.request_semantics_id IS DISTINCT FROM OLD.request_semantics_id
        OR NEW.request_semantics_sha256 IS DISTINCT FROM OLD.request_semantics_sha256
        OR NEW.request_semantics_policy_sha256 IS DISTINCT FROM OLD.request_semantics_policy_sha256
        OR NEW.request_tool_schema_sha256 IS DISTINCT FROM OLD.request_tool_schema_sha256
        OR NEW.request_allowed_tool_set_sha256 IS DISTINCT FROM OLD.request_allowed_tool_set_sha256
        OR NEW.request_id IS DISTINCT FROM OLD.request_id
        OR NEW.requested_model IS DISTINCT FROM OLD.requested_model
        OR NEW.route_profile_version IS DISTINCT FROM OLD.route_profile_version
        OR NEW.gateway_image_digest IS DISTINCT FROM OLD.gateway_image_digest
        OR NEW.region IS DISTINCT FROM OLD.region
        OR NEW.started_at IS DISTINCT FROM OLD.started_at
    ) THEN
        RAISE EXCEPTION 'evaluation route evidence identity % is immutable', OLD.route_trace_id;
    END IF;
    IF NEW.evidence_revision < OLD.evidence_revision THEN
        RAISE EXCEPTION 'evaluation route evidence revision cannot move backwards';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_evaluation_route_evidence_revision_immutable ON evaluation_route_evidence;
CREATE TRIGGER trg_evaluation_route_evidence_revision_immutable
    BEFORE UPDATE OR DELETE ON evaluation_route_evidence
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_route_evidence_revision_immutability();

CREATE OR REPLACE FUNCTION enforce_evaluation_signing_key_state()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evaluation evidence signing key rows are immutable';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.key_reference IS DISTINCT FROM OLD.key_reference
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'evaluation evidence signing key identity is immutable';
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NEW.state_epoch <> OLD.state_epoch + 1 THEN
            RAISE EXCEPTION 'signing key state transition must increment state_epoch once';
        END IF;
    ELSIF NEW.state_epoch IS DISTINCT FROM OLD.state_epoch THEN
        RAISE EXCEPTION 'signing key state_epoch requires a state transition';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_evaluation_evidence_signing_keys_state ON evaluation_evidence_signing_keys;
CREATE TRIGGER trg_evaluation_evidence_signing_keys_state
    BEFORE UPDATE OR DELETE ON evaluation_evidence_signing_keys
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_signing_key_state();

CREATE OR REPLACE FUNCTION enforce_evaluation_revision_batch_identity()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evaluation revision batch rows are immutable';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.reason IS DISTINCT FROM OLD.reason
        OR NEW.requested_by IS DISTINCT FROM OLD.requested_by
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'evaluation revision batch identity is immutable';
    END IF;
    IF NEW.control_epoch < OLD.control_epoch THEN
        RAISE EXCEPTION 'evaluation revision batch epoch cannot move backwards';
    END IF;
    IF OLD.status IN ('completed', 'failed', 'cancelled')
        AND NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'terminal evaluation revision batch cannot transition';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_evaluation_revision_batches_identity ON evaluation_revision_batches;
CREATE TRIGGER trg_evaluation_revision_batches_identity
    BEFORE UPDATE OR DELETE ON evaluation_revision_batches
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_revision_batch_identity();

CREATE OR REPLACE FUNCTION enforce_evaluation_revision_requirement_update()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evaluation revision batch requirement rows are immutable';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.revision_batch_id IS DISTINCT FROM OLD.revision_batch_id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.requirement_type IS DISTINCT FROM OLD.requirement_type
        OR NEW.target_key IS DISTINCT FROM OLD.target_key
        OR NEW.source_assignment_id IS DISTINCT FROM OLD.source_assignment_id
        OR NEW.previous_score_id IS DISTINCT FROM OLD.previous_score_id
        OR NEW.previous_score_created_at IS DISTINCT FROM OLD.previous_score_created_at
        OR NEW.grader_id IS DISTINCT FROM OLD.grader_id
        OR NEW.grader_version IS DISTINCT FROM OLD.grader_version
        OR NEW.grading_input_hash IS DISTINCT FROM OLD.grading_input_hash
        OR NEW.source_hash IS DISTINCT FROM OLD.source_hash
        OR NEW.cause_set_hash IS DISTINCT FROM OLD.cause_set_hash
        OR NEW.recovery_generation IS DISTINCT FROM OLD.recovery_generation
        OR NEW.replaces_requirement_id IS DISTINCT FROM OLD.replaces_requirement_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'evaluation revision batch requirement identity is immutable';
    END IF;
    IF OLD.status = 'pending' AND NEW.status IN ('completed', 'failed', 'superseded') THEN
        RETURN NEW;
    END IF;
    IF OLD.status = 'failed' AND NEW.status = 'superseded' THEN
        IF NOT EXISTS (
            SELECT 1 FROM evaluation_revision_batch_requirements replacement
            WHERE replacement.replaces_requirement_id = OLD.id
              AND replacement.revision_batch_id = OLD.revision_batch_id
              AND replacement.run_id = OLD.run_id
        ) THEN
            RAISE EXCEPTION 'failed requirement requires an inserted replacement before supersession';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        RAISE EXCEPTION 'invalid evaluation revision requirement transition % to %', OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_evaluation_revision_batch_requirements_update ON evaluation_revision_batch_requirements;
CREATE TRIGGER trg_evaluation_revision_batch_requirements_update
    BEFORE UPDATE OR DELETE ON evaluation_revision_batch_requirements
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_revision_requirement_update();

CREATE OR REPLACE FUNCTION enforce_evaluation_outbox_insert_only()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'evaluation outbox events are insert-only';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.sequence IS DISTINCT FROM OLD.sequence
        OR NEW.event_type IS DISTINCT FROM OLD.event_type
        OR NEW.dedup_key IS DISTINCT FROM OLD.dedup_key
        OR NEW.causation_id IS DISTINCT FROM OLD.causation_id
        OR NEW.cause_set_hash IS DISTINCT FROM OLD.cause_set_hash
        OR NEW.work_origin IS DISTINCT FROM OLD.work_origin
        OR NEW.revision_batch_id IS DISTINCT FROM OLD.revision_batch_id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.source_type IS DISTINCT FROM OLD.source_type
        OR NEW.source_id IS DISTINCT FROM OLD.source_id
        OR NEW.source_hash IS DISTINCT FROM OLD.source_hash
        OR NEW.payload_hash IS DISTINCT FROM OLD.payload_hash
        OR NEW.payload IS DISTINCT FROM OLD.payload
        OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'evaluation outbox immutable fields cannot be updated';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_evaluation_outbox_events_insert_only ON evaluation_outbox_events;
CREATE TRIGGER trg_evaluation_outbox_events_insert_only
    BEFORE UPDATE OR DELETE ON evaluation_outbox_events
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_outbox_insert_only();

CREATE OR REPLACE FUNCTION enforce_evaluation_outbox_cause()
RETURNS TRIGGER AS $$
DECLARE
    child evaluation_outbox_events%ROWTYPE;
    cause evaluation_outbox_events%ROWTYPE;
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        RAISE EXCEPTION 'evaluation outbox cause relations are immutable';
    END IF;
    SELECT * INTO child FROM evaluation_outbox_events WHERE id = NEW.event_id;
    SELECT * INTO cause FROM evaluation_outbox_events WHERE id = NEW.cause_event_id;
    IF child.id IS NULL OR cause.id IS NULL THEN
        RAISE EXCEPTION 'outbox cause relation references a missing event';
    END IF;
    IF child.run_id IS DISTINCT FROM cause.run_id OR NEW.run_id IS DISTINCT FROM child.run_id THEN
        RAISE EXCEPTION 'outbox cause events must belong to the same run';
    END IF;
    IF cause.sequence >= child.sequence THEN
        RAISE EXCEPTION 'outbox cause event must precede its child event';
    END IF;
    IF NEW.revision_batch_id IS DISTINCT FROM child.revision_batch_id THEN
        RAISE EXCEPTION 'outbox cause batch must match child event batch';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_evaluation_outbox_event_causes_validate ON evaluation_outbox_event_causes;
CREATE TRIGGER trg_evaluation_outbox_event_causes_validate
    BEFORE INSERT OR UPDATE OR DELETE ON evaluation_outbox_event_causes
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_outbox_cause();

DROP TRIGGER IF EXISTS trg_evaluation_scores_immutable ON evaluation_scores;
CREATE TRIGGER trg_evaluation_scores_immutable
    BEFORE UPDATE OR DELETE ON evaluation_scores
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record();
DROP TRIGGER IF EXISTS trg_evaluation_aggregate_snapshots_immutable ON evaluation_aggregate_snapshots;
CREATE TRIGGER trg_evaluation_aggregate_snapshots_immutable
    BEFORE UPDATE OR DELETE ON evaluation_aggregate_snapshots
    FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record();

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_request_semantics',
        'evaluation_score_head_events',
        'evaluation_analysis_job_score_inputs',
        'evaluation_analysis_job_snapshot_inputs',
        'evaluation_aggregate_snapshot_score_inputs',
        'evaluation_aggregate_snapshot_sources'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_revision_immutable ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_revision_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record()', table_name, table_name);
    END LOOP;
END $$;

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_route_evidence', 'evaluation_evidence_signing_keys',
        'evaluation_revision_batches', 'evaluation_revision_batch_requirements',
        'evaluation_grading_jobs', 'evaluation_analysis_jobs',
        'evaluation_score_head_events', 'evaluation_aggregate_heads',
        'evaluation_outbox_events', 'evaluation_outbox_event_causes'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_writer_protocol ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_writer_protocol BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION audit_evaluation_writer_protocol()', table_name, table_name);
    END LOOP;
END $$;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_release_projections_outbox_event') THEN
        ALTER TABLE evaluation_release_projections
            ADD CONSTRAINT fk_evaluation_release_projections_outbox_event
            FOREIGN KEY (last_outbox_event_id)
            REFERENCES evaluation_outbox_events(id)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;
