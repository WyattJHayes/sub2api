-- Radar trusted governance storage compatibility.
-- This migration adds immutable versions and lineage heads. Evaluation of a
-- release remains insufficient_evidence until migration 199 supplies trusted
-- Evidence metadata.

CREATE TABLE IF NOT EXISTS evaluation_release_subjects (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    subject_hash CHAR(64) NOT NULL CHECK (subject_hash ~ '^[0-9a-f]{64}$'),
    canonical_subject JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, subject_hash)
);

CREATE TABLE IF NOT EXISTS evaluation_gate_policy_events (
    id UUID PRIMARY KEY,
    policy_id UUID NOT NULL REFERENCES evaluation_gate_policies(id),
    event_type VARCHAR(32) NOT NULL CHECK (event_type IN ('created', 'activated', 'retired')),
    policy_hash CHAR(64) NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    environment VARCHAR(64) NOT NULL DEFAULT 'global',
    scope_type VARCHAR(32) NOT NULL DEFAULT 'global',
    scope_id VARCHAR(200) NOT NULL DEFAULT 'global',
    actor_id BIGINT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (policy_id, event_type, environment, scope_type, scope_id)
);

CREATE TABLE IF NOT EXISTS evaluation_gate_policy_heads (
    environment VARCHAR(64) NOT NULL,
    scope_type VARCHAR(32) NOT NULL,
    scope_id VARCHAR(200) NOT NULL,
    policy_id UUID NOT NULL REFERENCES evaluation_gate_policies(id),
    event_id UUID NOT NULL REFERENCES evaluation_gate_policy_events(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (environment, scope_type, scope_id)
);

CREATE TABLE IF NOT EXISTS evaluation_baseline_events (
    id UUID PRIMARY KEY,
    baseline_id UUID NOT NULL REFERENCES evaluation_baselines(id),
    event_type VARCHAR(32) NOT NULL CHECK (event_type IN ('proposed', 'approved', 'activated', 'retired')),
    evidence_hash CHAR(64) NOT NULL CHECK (evidence_hash ~ '^[0-9a-f]{64}$'),
    environment VARCHAR(64) NOT NULL DEFAULT 'global',
    scope_type VARCHAR(32) NOT NULL DEFAULT 'global',
    scope_id VARCHAR(200) NOT NULL DEFAULT 'global',
    actor_id BIGINT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_baseline_events_identity
    ON evaluation_baseline_events (baseline_id, event_type, environment, scope_type, scope_id, COALESCE(actor_id, 0));

CREATE TABLE IF NOT EXISTS evaluation_baseline_heads (
    environment VARCHAR(64) NOT NULL,
    scope_type VARCHAR(32) NOT NULL,
    scope_id VARCHAR(200) NOT NULL,
    model_route VARCHAR(200) NOT NULL,
    baseline_id UUID NOT NULL REFERENCES evaluation_baselines(id),
    event_id UUID NOT NULL REFERENCES evaluation_baseline_events(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (environment, scope_type, scope_id, model_route)
);

ALTER TABLE evaluation_gate_decisions
    ADD COLUMN IF NOT EXISTS release_subject_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS source_watermark JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN IF NOT EXISTS supersedes_decision_id UUID REFERENCES evaluation_gate_decisions(id),
    ADD COLUMN IF NOT EXISTS cause_set_hash CHAR(64);

UPDATE evaluation_gate_decisions
SET status = 'blocked'
WHERE status = 'waived';

UPDATE evaluation_gate_decisions
SET release_subject_hash = COALESCE(release_subject_hash, repeat('0', 64)),
    cause_set_hash = COALESCE(cause_set_hash, repeat('0', 64))
WHERE release_subject_hash IS NULL OR cause_set_hash IS NULL;

ALTER TABLE evaluation_gate_decisions
    ALTER COLUMN release_subject_hash SET NOT NULL,
    ALTER COLUMN cause_set_hash SET NOT NULL,
    DROP CONSTRAINT IF EXISTS evaluation_gate_decisions_run_id_policy_id_key,
    DROP CONSTRAINT IF EXISTS evaluation_gate_decisions_status_check,
    ADD CONSTRAINT evaluation_gate_decisions_status_check CHECK (
        status IN ('recorded', 'passed', 'blocked', 'review_required', 'insufficient_evidence')
    );

CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_gate_decisions_natural
    ON evaluation_gate_decisions (run_id, policy_id, evidence_hash);
CREATE UNIQUE INDEX IF NOT EXISTS uq_evaluation_gate_decisions_supersedes
    ON evaluation_gate_decisions (supersedes_decision_id)
    WHERE supersedes_decision_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS evaluation_gate_decision_heads (
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    policy_id UUID NOT NULL REFERENCES evaluation_gate_policies(id),
    release_subject_hash CHAR(64) NOT NULL CHECK (release_subject_hash ~ '^[0-9a-f]{64}$'),
    decision_id UUID NOT NULL REFERENCES evaluation_gate_decisions(id),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (run_id, policy_id, release_subject_hash)
);

CREATE TABLE IF NOT EXISTS evaluation_gate_decision_events (
    id UUID PRIMARY KEY,
    decision_id UUID NOT NULL REFERENCES evaluation_gate_decisions(id),
    event_type VARCHAR(32) NOT NULL CHECK (event_type IN ('recorded', 'superseded')),
    supersedes_decision_id UUID REFERENCES evaluation_gate_decisions(id),
    source_watermark JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_gate_reevaluation_outbox (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    cause_type VARCHAR(32) NOT NULL CHECK (cause_type IN ('policy_head', 'baseline_head', 'evidence', 'score_head', 'aggregate_head')),
    cause_id UUID,
    idempotency_key CHAR(64) NOT NULL UNIQUE,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS evaluation_gate_storage_modes (
    id SMALLINT PRIMARY KEY CHECK (id = 1),
    mode VARCHAR(24) NOT NULL CHECK (mode IN ('compatibility', 'trusted')),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO evaluation_gate_storage_modes (id, mode)
VALUES (1, 'compatibility')
ON CONFLICT (id) DO NOTHING;

CREATE OR REPLACE FUNCTION enforce_evaluation_gate_storage_mode()
RETURNS TRIGGER AS $$
DECLARE
    storage_mode VARCHAR(24);
BEGIN
    SELECT mode INTO storage_mode FROM evaluation_gate_storage_modes WHERE id = 1;
    IF storage_mode = 'compatibility' THEN
        NEW.status := 'insufficient_evidence';
        NEW.rule_ids := ARRAY['migration_199_required'];
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_gate_decisions_storage_mode ON evaluation_gate_decisions;
CREATE TRIGGER trg_evaluation_gate_decisions_storage_mode
    BEFORE INSERT ON evaluation_gate_decisions
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_gate_storage_mode();

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_release_subjects', 'evaluation_gate_policy_events',
        'evaluation_baseline_events', 'evaluation_gate_decisions',
        'evaluation_gate_decision_events', 'evaluation_gate_waivers',
        'evaluation_gate_policies', 'evaluation_baselines',
        'evaluation_baseline_approvals'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_trusted_immutable ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_trusted_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record()', table_name, table_name);
    END LOOP;
END $$;

CREATE OR REPLACE FUNCTION guard_evaluation_governance_head_mutation()
RETURNS TRIGGER AS $$
BEGIN
    IF current_setting('app.evaluation_head_cas', TRUE) IS DISTINCT FROM '1' THEN
        RAISE EXCEPTION 'governance head mutations require compare-and-set function';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_gate_policy_heads_cas ON evaluation_gate_policy_heads;
CREATE TRIGGER trg_evaluation_gate_policy_heads_cas
    BEFORE INSERT OR UPDATE OR DELETE ON evaluation_gate_policy_heads
    FOR EACH ROW EXECUTE FUNCTION guard_evaluation_governance_head_mutation();

DROP TRIGGER IF EXISTS trg_evaluation_baseline_heads_cas ON evaluation_baseline_heads;
CREATE TRIGGER trg_evaluation_baseline_heads_cas
    BEFORE INSERT OR UPDATE OR DELETE ON evaluation_baseline_heads
    FOR EACH ROW EXECUTE FUNCTION guard_evaluation_governance_head_mutation();

DROP TRIGGER IF EXISTS trg_evaluation_gate_decision_heads_cas ON evaluation_gate_decision_heads;
CREATE TRIGGER trg_evaluation_gate_decision_heads_cas
    BEFORE INSERT OR UPDATE OR DELETE ON evaluation_gate_decision_heads
    FOR EACH ROW EXECUTE FUNCTION guard_evaluation_governance_head_mutation();

CREATE OR REPLACE FUNCTION advance_evaluation_gate_policy_head(
    p_environment VARCHAR,
    p_scope_type VARCHAR,
    p_scope_id VARCHAR,
    p_policy_id UUID,
    p_event_id UUID,
    p_expected_policy_id UUID DEFAULT NULL
) RETURNS BOOLEAN AS $$
DECLARE
    current_policy UUID;
BEGIN
    SELECT policy_id INTO current_policy
    FROM evaluation_gate_policy_heads
    WHERE environment = p_environment AND scope_type = p_scope_type AND scope_id = p_scope_id
    FOR UPDATE;
    IF FOUND THEN
        IF current_policy IS DISTINCT FROM p_expected_policy_id THEN
            RETURN FALSE;
        END IF;
        PERFORM set_config('app.evaluation_head_cas', '1', TRUE);
        UPDATE evaluation_gate_policy_heads
        SET policy_id = p_policy_id, event_id = p_event_id, updated_at = transaction_timestamp()
        WHERE environment = p_environment AND scope_type = p_scope_type AND scope_id = p_scope_id;
        RETURN TRUE;
    END IF;
    IF p_expected_policy_id IS NOT NULL THEN
        RETURN FALSE;
    END IF;
    PERFORM set_config('app.evaluation_head_cas', '1', TRUE);
    INSERT INTO evaluation_gate_policy_heads (environment, scope_type, scope_id, policy_id, event_id)
    VALUES (p_environment, p_scope_type, p_scope_id, p_policy_id, p_event_id);
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION advance_evaluation_baseline_head(
    p_environment VARCHAR,
    p_scope_type VARCHAR,
    p_scope_id VARCHAR,
    p_model_route VARCHAR,
    p_baseline_id UUID,
    p_event_id UUID,
    p_expected_baseline_id UUID DEFAULT NULL
) RETURNS BOOLEAN AS $$
DECLARE
    current_baseline UUID;
BEGIN
    SELECT baseline_id INTO current_baseline
    FROM evaluation_baseline_heads
    WHERE environment = p_environment AND scope_type = p_scope_type
      AND scope_id = p_scope_id AND model_route = p_model_route
    FOR UPDATE;
    IF FOUND THEN
        IF current_baseline IS DISTINCT FROM p_expected_baseline_id THEN
            RETURN FALSE;
        END IF;
        PERFORM set_config('app.evaluation_head_cas', '1', TRUE);
        UPDATE evaluation_baseline_heads
        SET baseline_id = p_baseline_id, event_id = p_event_id, updated_at = transaction_timestamp()
        WHERE environment = p_environment AND scope_type = p_scope_type
          AND scope_id = p_scope_id AND model_route = p_model_route;
        RETURN TRUE;
    END IF;
    IF p_expected_baseline_id IS NOT NULL THEN
        RETURN FALSE;
    END IF;
    PERFORM set_config('app.evaluation_head_cas', '1', TRUE);
    INSERT INTO evaluation_baseline_heads
        (environment, scope_type, scope_id, model_route, baseline_id, event_id)
    VALUES (p_environment, p_scope_type, p_scope_id, p_model_route, p_baseline_id, p_event_id);
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;

CREATE INDEX IF NOT EXISTS idx_evaluation_gate_reevaluation_outbox_pending
    ON evaluation_gate_reevaluation_outbox (created_at)
    WHERE processed_at IS NULL;
