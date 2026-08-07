-- Radar trusted lifecycle expand schema. Existing rows receive compatible
-- defaults; trusted writers populate the immutable and fencing fields.

ALTER TABLE evaluation_runs
    ADD COLUMN IF NOT EXISTS budget_mode TEXT NOT NULL DEFAULT 'normal',
    ADD COLUMN IF NOT EXISTS paused_from_status VARCHAR(24),
    ADD COLUMN IF NOT EXISTS pause_reason TEXT,
    ADD COLUMN IF NOT EXISTS control_epoch BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS state_version BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS cancelled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cancelled_by BIGINT REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS route_profile_version TEXT NOT NULL DEFAULT 'legacy-unbound';

ALTER TABLE evaluation_runs
    DROP CONSTRAINT IF EXISTS evaluation_runs_budget_mode_check,
    ADD CONSTRAINT evaluation_runs_budget_mode_check
        CHECK (budget_mode IN ('normal', 'exact_p0_drain')),
    DROP CONSTRAINT IF EXISTS evaluation_runs_pause_fields_check,
    ADD CONSTRAINT evaluation_runs_pause_fields_check CHECK (
        (status = 'paused' AND paused_from_status IN ('pending', 'budget_paused', 'running') AND pause_reason IS NOT NULL)
        OR (status <> 'paused' AND paused_from_status IS NULL AND pause_reason IS NULL)
    ) NOT VALID,
    DROP CONSTRAINT IF EXISTS evaluation_runs_terminal_fields_check,
    ADD CONSTRAINT evaluation_runs_terminal_fields_check CHECK (
        (status IN ('completed', 'failed', 'cancelled') AND finished_at IS NOT NULL)
        OR status NOT IN ('completed', 'failed', 'cancelled')
    ) NOT VALID,
    DROP CONSTRAINT IF EXISTS evaluation_runs_cancel_fields_check,
    ADD CONSTRAINT evaluation_runs_cancel_fields_check CHECK (
        (status = 'cancelled' AND cancelled_at IS NOT NULL AND cancelled_by IS NOT NULL)
        OR status <> 'cancelled'
    ) NOT VALID;

ALTER TABLE evaluation_run_events
    ADD COLUMN IF NOT EXISTS transition_version BIGINT,
    ADD COLUMN IF NOT EXISTS from_status VARCHAR(24),
    ADD COLUMN IF NOT EXISTS to_status VARCHAR(24),
    ADD COLUMN IF NOT EXISTS control_epoch BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS idempotency_key CHAR(64);

UPDATE evaluation_run_events
SET idempotency_key = md5(id::text || ':event-a') || md5(id::text || ':event-b')
WHERE idempotency_key IS NULL;

ALTER TABLE evaluation_run_events
    ALTER COLUMN idempotency_key SET NOT NULL,
    DROP CONSTRAINT IF EXISTS evaluation_run_events_idempotency_key_check,
    ADD CONSTRAINT evaluation_run_events_idempotency_key_check
        CHECK (idempotency_key ~ '^[0-9a-f]{64}$'),
    DROP CONSTRAINT IF EXISTS evaluation_run_events_transition_fields_check,
    ADD CONSTRAINT evaluation_run_events_transition_fields_check CHECK (
        (transition_version IS NULL AND from_status IS NULL AND to_status IS NULL)
        OR (transition_version IS NOT NULL AND from_status IS NOT NULL AND to_status IS NOT NULL)
    );

CREATE UNIQUE INDEX IF NOT EXISTS idx_evaluation_run_events_transition
    ON evaluation_run_events (run_id, transition_version)
    WHERE transition_version IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_evaluation_run_events_idempotency
    ON evaluation_run_events (idempotency_key);

ALTER TABLE evaluation_workers
    ADD COLUMN IF NOT EXISTS region TEXT,
    ADD COLUMN IF NOT EXISTS image_digest TEXT,
    ADD COLUMN IF NOT EXISTS claim_mode VARCHAR(16) NOT NULL DEFAULT 'open',
    ADD COLUMN IF NOT EXISTS drain_requested_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS token_epoch BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS token_fingerprint CHAR(12);

ALTER TABLE evaluation_workers
    DROP CONSTRAINT IF EXISTS evaluation_workers_claim_mode_check,
    ADD CONSTRAINT evaluation_workers_claim_mode_check
        CHECK (claim_mode IN ('open', 'paused', 'draining'));

ALTER TABLE evaluation_assignments
    ADD COLUMN IF NOT EXISTS lease_epoch BIGINT,
    ADD COLUMN IF NOT EXISTS worker_image_digest TEXT,
    ADD COLUMN IF NOT EXISTS work_origin TEXT;

ALTER TABLE evaluation_grading_jobs
    ADD COLUMN IF NOT EXISTS lease_epoch BIGINT,
    ADD COLUMN IF NOT EXISTS worker_image_digest TEXT,
    ADD COLUMN IF NOT EXISTS work_origin TEXT;

ALTER TABLE evaluation_analysis_jobs
    ADD COLUMN IF NOT EXISTS lease_epoch BIGINT,
    ADD COLUMN IF NOT EXISTS worker_image_digest TEXT,
    ADD COLUMN IF NOT EXISTS work_origin TEXT;

ALTER TABLE evaluation_grading_jobs
    DROP CONSTRAINT IF EXISTS evaluation_grading_jobs_status_check,
    ADD CONSTRAINT evaluation_grading_jobs_status_check
        CHECK (status IN ('pending', 'leased', 'completed', 'failed', 'cancelled'));
ALTER TABLE evaluation_analysis_jobs
    DROP CONSTRAINT IF EXISTS evaluation_analysis_jobs_status_check,
    ADD CONSTRAINT evaluation_analysis_jobs_status_check
        CHECK (status IN ('pending', 'leased', 'completed', 'failed', 'cancelled'));

CREATE TABLE IF NOT EXISTS evaluation_request_manifests (
    id UUID PRIMARY KEY,
    schema_version VARCHAR(80) NOT NULL CHECK (schema_version = 'radar-request-manifest-v1'),
    interaction_type VARCHAR(20) NOT NULL CHECK (interaction_type IN ('single', 'multi_turn', 'agent')),
    canonical_manifest_bytes BYTEA NOT NULL,
    manifest_sha256 CHAR(64) NOT NULL CHECK (manifest_sha256 ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (manifest_sha256),
    UNIQUE (id, manifest_sha256)
);

CREATE TABLE IF NOT EXISTS evaluation_pair_specs (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    case_id UUID NOT NULL REFERENCES evaluation_cases(id),
    sample_index INT NOT NULL CHECK (sample_index BETWEEN 0 AND 9),
    repeat_index INT NOT NULL CHECK (repeat_index >= 0),
    request_manifest_id UUID NOT NULL,
    request_manifest_sha256 CHAR(64) NOT NULL,
    schema_version VARCHAR(80) NOT NULL DEFAULT 'radar-pair-spec-v1',
    canonical_spec JSONB NOT NULL,
    pair_spec_hash CHAR(64) NOT NULL CHECK (pair_spec_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, case_id, sample_index, repeat_index),
    UNIQUE (id, run_id),
    CONSTRAINT fk_evaluation_pair_specs_manifest
        FOREIGN KEY (request_manifest_id, request_manifest_sha256)
        REFERENCES evaluation_request_manifests(id, manifest_sha256)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_side_specs (
    id UUID PRIMARY KEY,
    pair_spec_id UUID NOT NULL REFERENCES evaluation_pair_specs(id),
    sample_id UUID NOT NULL,
    side VARCHAR(12) NOT NULL CHECK (side IN ('baseline', 'candidate')),
    schema_version VARCHAR(80) NOT NULL DEFAULT 'radar-side-spec-v1',
    canonical_spec JSONB NOT NULL,
    side_spec_hash CHAR(64) NOT NULL CHECK (side_spec_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pair_spec_id, side),
    UNIQUE (sample_id),
    UNIQUE (id, pair_spec_id),
    CONSTRAINT fk_evaluation_side_specs_sample
        FOREIGN KEY (sample_id) REFERENCES evaluation_samples(id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE TABLE IF NOT EXISTS evaluation_pair_bindings (
    id UUID PRIMARY KEY,
    pair_spec_id UUID NOT NULL REFERENCES evaluation_pair_specs(id),
    baseline_side_spec_id UUID NOT NULL,
    candidate_side_spec_id UUID NOT NULL,
    pair_binding_hash CHAR(64) NOT NULL CHECK (pair_binding_hash ~ '^[0-9a-f]{64}$'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (pair_spec_id),
    CONSTRAINT fk_pair_binding_baseline_side
        FOREIGN KEY (baseline_side_spec_id, pair_spec_id)
        REFERENCES evaluation_side_specs(id, pair_spec_id)
        DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT fk_pair_binding_candidate_side
        FOREIGN KEY (candidate_side_spec_id, pair_spec_id)
        REFERENCES evaluation_side_specs(id, pair_spec_id)
        DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE evaluation_score_heads
    ADD COLUMN IF NOT EXISTS score_created_at TIMESTAMPTZ;

DO $$
DECLARE
    head RECORD;
    match_count INTEGER;
    matched_created_at TIMESTAMPTZ;
BEGIN
    FOR head IN SELECT sample_id, grader_id, score_id FROM evaluation_score_heads LOOP
        SELECT COUNT(*), MIN(created_at)
        INTO match_count, matched_created_at
        FROM evaluation_scores
        WHERE id = head.score_id;
        IF match_count <> 1 THEN
            RAISE EXCEPTION 'cannot backfill ScoreRef for score_id %, expected one score row, found %', head.score_id, match_count;
        END IF;
        UPDATE evaluation_score_heads
        SET score_created_at = matched_created_at
        WHERE sample_id = head.sample_id AND grader_id = head.grader_id;
    END LOOP;
END $$;

ALTER TABLE evaluation_score_heads
    ALTER COLUMN score_created_at SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_score_heads_score_ref'
    ) THEN
        ALTER TABLE evaluation_score_heads
            ADD CONSTRAINT fk_evaluation_score_heads_score_ref
            FOREIGN KEY (score_id, score_created_at)
            REFERENCES evaluation_scores(id, created_at)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS evaluation_worker_events (
    id UUID PRIMARY KEY,
    worker_id UUID NOT NULL REFERENCES evaluation_workers(id),
    event_type VARCHAR(40) NOT NULL CHECK (event_type IN ('registered', 'claims_paused', 'claims_resumed', 'draining', 'drain_completed', 'disabled', 'token_rotated', 'enabled')),
    idempotency_key CHAR(64) NOT NULL UNIQUE,
    actor_id BIGINT REFERENCES users(id),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_route_evidence_terminalization_outbox (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    terminal_status VARCHAR(24) NOT NULL CHECK (terminal_status IN ('failed', 'cancelled')),
    control_epoch BIGINT NOT NULL,
    idempotency_key CHAR(64) NOT NULL UNIQUE,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_evaluation_route_evidence_terminalization_outbox_pending
    ON evaluation_route_evidence_terminalization_outbox (created_at)
    WHERE processed_at IS NULL;

CREATE TABLE IF NOT EXISTS evaluation_schema_cutovers (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    write_mode VARCHAR(16) NOT NULL CHECK (write_mode IN ('open', 'draining', 'closed')),
    guard_mode VARCHAR(16) NOT NULL CHECK (guard_mode IN ('audit', 'enforce')),
    minimum_protocol_version INT NOT NULL CHECK (minimum_protocol_version > 0),
    updated_by BIGINT REFERENCES users(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO evaluation_schema_cutovers (id, write_mode, guard_mode, minimum_protocol_version)
VALUES (1, 'open', 'audit', 1)
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS evaluation_writer_sessions (
    instance_id UUID PRIMARY KEY,
    writer_kind VARCHAR(32) NOT NULL,
    protocol_version INT NOT NULL CHECK (protocol_version > 0),
    active_lease_count INT NOT NULL DEFAULT 0 CHECK (active_lease_count >= 0),
    last_transaction_at TIMESTAMPTZ,
    heartbeat_expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_writer_protocol_audits (
    id UUID PRIMARY KEY,
    table_name TEXT NOT NULL,
    operation CHAR(1) NOT NULL CHECK (operation IN ('I', 'U', 'D')),
    instance_id UUID,
    protocol_version INT,
    guard_mode VARCHAR(16) NOT NULL,
    accepted BOOLEAN NOT NULL,
    reason_code VARCHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE OR REPLACE FUNCTION assert_evaluation_writer_protocol(p_table_name TEXT DEFAULT NULL, p_operation TEXT DEFAULT NULL)
RETURNS VOID AS $$
DECLARE
    cutover evaluation_schema_cutovers%ROWTYPE;
    protocol_text TEXT;
    instance_text TEXT;
    writer_kind TEXT;
    protocol INT;
    instance UUID;
    accepted BOOLEAN := TRUE;
    reason TEXT := 'ok';
BEGIN
    SELECT * INTO cutover FROM evaluation_schema_cutovers WHERE id = 1;
    protocol_text := current_setting('app.evaluation_writer_protocol', TRUE);
    instance_text := current_setting('app.evaluation_writer_instance_id', TRUE);
    writer_kind := current_setting('app.evaluation_writer_kind', TRUE);
    IF COALESCE(protocol_text, '') ~ '^[0-9]+$' THEN
        BEGIN
            protocol := protocol_text::INT;
        EXCEPTION WHEN invalid_text_representation OR numeric_value_out_of_range THEN
            protocol := NULL;
        END;
    END IF;
    IF COALESCE(instance_text, '') ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$' THEN
        instance := instance_text::UUID;
    END IF;

    IF cutover.write_mode = 'draining' AND COALESCE(writer_kind, '') <> 'migration' THEN
        accepted := FALSE;
        reason := 'radar_cutover_active';
    ELSIF cutover.write_mode = 'closed' AND COALESCE(writer_kind, '') <> 'migration' THEN
        accepted := FALSE;
        reason := 'radar_cutover_active';
    ELSIF protocol IS NULL OR instance IS NULL THEN
        accepted := FALSE;
        reason := 'missing_writer_identity';
    ELSIF NOT EXISTS (
        SELECT 1 FROM evaluation_writer_sessions s
        WHERE s.instance_id = instance
          AND s.protocol_version >= cutover.minimum_protocol_version
          AND s.heartbeat_expires_at > NOW()
    ) THEN
        accepted := FALSE;
        reason := 'unknown_writer_session';
    END IF;

    IF cutover.guard_mode = 'enforce' AND NOT accepted THEN
        RAISE EXCEPTION '%', reason;
    END IF;

    IF NOT accepted OR cutover.guard_mode = 'audit' THEN
        INSERT INTO evaluation_writer_protocol_audits (
            id, table_name, operation, instance_id, protocol_version,
            guard_mode, accepted, reason_code
        ) VALUES (
            gen_random_uuid(), COALESCE(p_table_name, 'unknown'), SUBSTRING(COALESCE(p_operation, 'X'), 1, 1), instance,
            protocol, cutover.guard_mode, accepted, reason
        );
    END IF;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION audit_evaluation_writer_protocol()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM assert_evaluation_writer_protocol(TG_TABLE_NAME, TG_OP);
    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION reject_immutable_evaluation_record()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% rows are immutable', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_request_manifests', 'evaluation_pair_specs',
        'evaluation_side_specs', 'evaluation_pair_bindings',
        'evaluation_worker_events', 'evaluation_run_events',
        'evaluation_writer_protocol_audits'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_immutable ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record()', table_name, table_name);
    END LOOP;
END $$;

CREATE OR REPLACE FUNCTION enforce_evaluation_run_status_graph()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF OLD.status IN ('completed', 'failed', 'cancelled') THEN
            RAISE EXCEPTION 'terminal evaluation run status % cannot transition to %', OLD.status, NEW.status;
        END IF;
        IF NOT (
            (OLD.status = 'pending' AND NEW.status IN ('running', 'paused', 'cancelled', 'failed')) OR
            (OLD.status = 'budget_paused' AND NEW.status IN ('running', 'paused', 'cancelled', 'failed')) OR
            (OLD.status = 'running' AND NEW.status IN ('paused', 'completed', 'cancelled', 'failed')) OR
            (OLD.status = 'paused' AND NEW.status IN ('pending', 'budget_paused', 'running', 'failed', 'cancelled'))
        ) THEN
            RAISE EXCEPTION 'invalid evaluation run transition % -> %', OLD.status, NEW.status;
        END IF;
        NEW.state_version := OLD.state_version + 1;
    ELSIF NEW.state_version IS DISTINCT FROM OLD.state_version THEN
        RAISE EXCEPTION 'evaluation run state_version can only change with a status transition';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_runs_status_graph ON evaluation_runs;
CREATE TRIGGER trg_evaluation_runs_status_graph
    BEFORE UPDATE OF status ON evaluation_runs
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_run_status_graph();

CREATE OR REPLACE FUNCTION verify_evaluation_run_transition_event()
RETURNS TRIGGER AS $$
DECLARE
    event_count INTEGER;
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        SELECT COUNT(*) INTO event_count
        FROM evaluation_run_events
        WHERE run_id = NEW.id
          AND transition_version = NEW.state_version
          AND from_status = OLD.status
          AND to_status = NEW.status
          AND control_epoch = NEW.control_epoch;
        IF event_count <> 1 THEN
            RAISE EXCEPTION 'evaluation run % transition % requires exactly one matching event, found %', NEW.id, NEW.state_version, event_count;
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_runs_transition_event ON evaluation_runs;
CREATE CONSTRAINT TRIGGER trg_evaluation_runs_transition_event
    AFTER UPDATE OF status, state_version ON evaluation_runs
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION verify_evaluation_run_transition_event();

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_runs', 'evaluation_run_events', 'evaluation_workers',
        'evaluation_worker_events', 'evaluation_assignments',
        'evaluation_grading_jobs', 'evaluation_analysis_jobs',
        'evaluation_score_heads', 'evaluation_request_manifests',
        'evaluation_pair_specs', 'evaluation_side_specs', 'evaluation_pair_bindings'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_writer_protocol ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_writer_protocol BEFORE INSERT OR UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION audit_evaluation_writer_protocol()', table_name, table_name);
    END LOOP;
END $$;
