-- Forward-only hardening for databases that already applied migration 198.
-- Keep migration 198 byte-for-byte immutable so its published checksum remains valid.

ALTER TABLE evaluation_release_subjects
    ADD COLUMN IF NOT EXISTS canonical_subject_bytes BYTEA;

CREATE OR REPLACE FUNCTION radar_json_text_array_198a(value JSONB)
RETURNS TEXT
LANGUAGE SQL
IMMUTABLE
STRICT
AS $$
    SELECT '[' || COALESCE(
        string_agg(to_jsonb(element)::text, ',' ORDER BY ordinal),
        ''
    ) || ']'
    FROM jsonb_array_elements_text(value) WITH ORDINALITY AS items(element, ordinal)
$$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_trigger
        WHERE tgrelid = 'evaluation_release_subjects'::regclass
          AND tgname = 'trg_evaluation_release_subjects_trusted_immutable'
          AND NOT tgisinternal
    ) THEN
        ALTER TABLE evaluation_release_subjects
            DISABLE TRIGGER trg_evaluation_release_subjects_trusted_immutable;
    END IF;
END $$;

UPDATE evaluation_release_subjects
SET canonical_subject_bytes = convert_to(
    '{' ||
    '"analysis_version":' || to_jsonb(canonical_subject->>'analysis_version')::text || ',' ||
    '"baseline_id":' || to_jsonb(canonical_subject->>'baseline_id')::text || ',' ||
    '"candidate_model_config_sha256":' || to_jsonb(canonical_subject->>'candidate_model_config_sha256')::text || ',' ||
    '"control_plane_image_digest":' || to_jsonb(canonical_subject->>'control_plane_image_digest')::text || ',' ||
    '"dataset_manifest_sha256":' || to_jsonb(canonical_subject->>'dataset_manifest_sha256')::text || ',' ||
    '"deployment_environment":' || to_jsonb(canonical_subject->>'deployment_environment')::text || ',' ||
    '"gateway_image_digest":' || to_jsonb(canonical_subject->>'gateway_image_digest')::text || ',' ||
    '"grader_image_digests":' || radar_json_text_array_198a(canonical_subject->'grader_image_digests') || ',' ||
    '"region_set":' || radar_json_text_array_198a(canonical_subject->'region_set') || ',' ||
    '"route_profile_version":' || to_jsonb(canonical_subject->>'route_profile_version')::text || ',' ||
    '"runner_image_digests":' || radar_json_text_array_198a(canonical_subject->'runner_image_digests') || ',' ||
    '"scope_id":' || to_jsonb(canonical_subject->>'scope_id')::text || ',' ||
    '"scope_type":' || to_jsonb(canonical_subject->>'scope_type')::text || ',' ||
    '"statistics_image_digests":' || radar_json_text_array_198a(canonical_subject->'statistics_image_digests') ||
    '}',
    'UTF8'
)
WHERE canonical_subject_bytes IS NULL;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM evaluation_release_subjects
        WHERE canonical_subject_bytes IS NULL
           OR encode(sha256(canonical_subject_bytes), 'hex') <> subject_hash
    ) THEN
        RAISE EXCEPTION 'release subject canonical byte backfill failed hash validation';
    END IF;
END $$;

ALTER TABLE evaluation_release_subjects
    ALTER COLUMN canonical_subject_bytes SET NOT NULL;

CREATE TABLE IF NOT EXISTS evaluation_release_subject_events (
    id UUID PRIMARY KEY,
    sequence BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
    release_subject_id UUID NOT NULL REFERENCES evaluation_release_subjects(id),
    event_type VARCHAR(32) NOT NULL CHECK (event_type IN ('activated', 'revoked')),
    actor_id BIGINT NOT NULL REFERENCES users(id),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (
        (event_type = 'activated' AND expires_at IS NOT NULL AND expires_at > effective_at)
        OR (event_type = 'revoked' AND expires_at IS NULL)
    )
);
CREATE INDEX IF NOT EXISTS idx_evaluation_release_subject_events_current
    ON evaluation_release_subject_events (release_subject_id, sequence DESC);

CREATE TABLE IF NOT EXISTS evaluation_gate_policy_approvals (
    id UUID PRIMARY KEY,
    policy_id UUID NOT NULL REFERENCES evaluation_gate_policies(id),
    approver_id BIGINT NOT NULL REFERENCES users(id),
    role VARCHAR(32) NOT NULL CHECK (role = 'quality_admin'),
    effective_at TIMESTAMPTZ NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expires_at > effective_at),
    UNIQUE (policy_id, approver_id, role)
);

INSERT INTO evaluation_gate_policy_approvals
    (id, policy_id, approver_id, role, effective_at, expires_at, created_at)
SELECT gen_random_uuid(), p.id, p.created_by, 'quality_admin', p.created_at,
       p.created_at + INTERVAL '30 days', p.created_at
FROM evaluation_gate_policies p
ON CONFLICT (policy_id, approver_id, role) DO NOTHING;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgrelid = 'evaluation_baseline_approvals'::regclass
          AND tgname = 'trg_evaluation_baseline_approvals_trusted_immutable'
          AND NOT tgisinternal
    ) THEN
        ALTER TABLE evaluation_baseline_approvals
            DISABLE TRIGGER trg_evaluation_baseline_approvals_trusted_immutable;
    END IF;
END $$;

ALTER TABLE evaluation_baseline_approvals
    ADD COLUMN IF NOT EXISTS effective_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ;
UPDATE evaluation_baseline_approvals
SET effective_at = COALESCE(effective_at, created_at),
    expires_at = COALESCE(expires_at, created_at + INTERVAL '30 days')
WHERE effective_at IS NULL OR expires_at IS NULL;
ALTER TABLE evaluation_baseline_approvals
    ALTER COLUMN effective_at SET NOT NULL,
    ALTER COLUMN expires_at SET NOT NULL;

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT c.conname
        FROM pg_constraint c
        WHERE c.conrelid = 'evaluation_gate_policy_events'::regclass
          AND c.contype = 'u'
          AND pg_get_constraintdef(c.oid) LIKE 'UNIQUE (policy_id, event_type, environment, scope_type, scope_id)%'
    LOOP
        EXECUTE format('ALTER TABLE evaluation_gate_policy_events DROP CONSTRAINT %I', constraint_name);
    END LOOP;
END $$;

DROP INDEX IF EXISTS uq_evaluation_baseline_events_identity;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_trigger
        WHERE tgrelid = 'evaluation_release_subjects'::regclass
          AND tgname = 'trg_evaluation_release_subjects_trusted_immutable'
          AND NOT tgisinternal
    ) THEN
        ALTER TABLE evaluation_release_subjects
            ENABLE TRIGGER trg_evaluation_release_subjects_trusted_immutable;
    END IF;
END $$;

DO $$
DECLARE
    table_name TEXT;
BEGIN
    FOREACH table_name IN ARRAY ARRAY[
        'evaluation_release_subject_events',
        'evaluation_gate_policy_approvals'
    ] LOOP
        EXECUTE format('DROP TRIGGER IF EXISTS trg_%I_trusted_immutable ON %I', table_name, table_name);
        EXECUTE format('CREATE TRIGGER trg_%I_trusted_immutable BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION reject_immutable_evaluation_record()', table_name, table_name);
    END LOOP;

    IF EXISTS (
        SELECT 1 FROM pg_trigger
        WHERE tgrelid = 'evaluation_baseline_approvals'::regclass
          AND tgname = 'trg_evaluation_baseline_approvals_trusted_immutable'
          AND NOT tgisinternal
    ) THEN
        ALTER TABLE evaluation_baseline_approvals
            ENABLE TRIGGER trg_evaluation_baseline_approvals_trusted_immutable;
    END IF;
END $$;

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
    PERFORM pg_advisory_xact_lock(hashtextextended(
        'policy:' || p_environment || ':' || p_scope_type || ':' || p_scope_id, 0
    ));
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
    PERFORM pg_advisory_xact_lock(hashtextextended(
        'baseline:' || p_environment || ':' || p_scope_type || ':' || p_scope_id || ':' || p_model_route, 0
    ));
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

DROP FUNCTION radar_json_text_array_198a(JSONB);
