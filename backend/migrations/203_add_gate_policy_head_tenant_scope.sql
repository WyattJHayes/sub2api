-- Radar gate policy heads are tenant-owned. The original head key omitted the
-- tenant and allowed equal scopes from different tenants to overwrite one
-- another. This migration preserves existing heads and makes every CAS
-- operation tenant-aware.

ALTER TABLE evaluation_gate_policy_heads
    ADD COLUMN IF NOT EXISTS tenant_id BIGINT;

UPDATE evaluation_gate_policy_heads h
SET tenant_id = p.tenant_id
FROM evaluation_gate_policies p
WHERE h.policy_id = p.id
  AND h.tenant_id IS NULL;

UPDATE evaluation_gate_policy_heads
SET tenant_id = 0
WHERE tenant_id IS NULL;

ALTER TABLE evaluation_gate_policy_heads
    ALTER COLUMN tenant_id SET NOT NULL;

DO $$
DECLARE
    primary_key_name TEXT;
BEGIN
    SELECT conname INTO primary_key_name
    FROM pg_constraint
    WHERE conrelid = 'evaluation_gate_policy_heads'::regclass
      AND contype = 'p';
    IF primary_key_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE evaluation_gate_policy_heads DROP CONSTRAINT %I', primary_key_name);
    END IF;
END $$;

ALTER TABLE evaluation_gate_policy_heads
    ADD CONSTRAINT evaluation_gate_policy_heads_pkey
    PRIMARY KEY (tenant_id, environment, scope_type, scope_id);

CREATE INDEX IF NOT EXISTS idx_evaluation_gate_policy_heads_tenant_policy
    ON evaluation_gate_policy_heads (tenant_id, policy_id);

DROP INDEX IF EXISTS idx_evaluation_role_bindings_active;
CREATE UNIQUE INDEX IF NOT EXISTS idx_evaluation_role_bindings_active_tenant
    ON evaluation_role_bindings (tenant_id, actor_id, role, md5(scope::text))
    WHERE enabled;

CREATE OR REPLACE FUNCTION advance_evaluation_gate_policy_head(
    p_tenant_id BIGINT,
    p_environment VARCHAR,
    p_scope_type VARCHAR,
    p_scope_id VARCHAR,
    p_policy_id UUID,
    p_event_id UUID,
    p_expected_policy_id UUID DEFAULT NULL
) RETURNS BOOLEAN AS $$
DECLARE
    current_policy UUID;
    policy_tenant BIGINT;
BEGIN
    SELECT tenant_id INTO policy_tenant
    FROM evaluation_gate_policies
    WHERE id = p_policy_id;
    IF NOT FOUND OR policy_tenant IS DISTINCT FROM p_tenant_id THEN
        RETURN FALSE;
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'policy:' || p_tenant_id::text || ':' || p_environment || ':' || p_scope_type || ':' || p_scope_id,
        0
    ));

    SELECT policy_id INTO current_policy
    FROM evaluation_gate_policy_heads
    WHERE tenant_id = p_tenant_id
      AND environment = p_environment
      AND scope_type = p_scope_type
      AND scope_id = p_scope_id
    FOR UPDATE;
    IF FOUND THEN
        IF current_policy IS DISTINCT FROM p_expected_policy_id THEN
            RETURN FALSE;
        END IF;
        PERFORM set_config('app.evaluation_head_cas', '1', TRUE);
        UPDATE evaluation_gate_policy_heads
        SET policy_id = p_policy_id,
            event_id = p_event_id,
            updated_at = transaction_timestamp()
        WHERE tenant_id = p_tenant_id
          AND environment = p_environment
          AND scope_type = p_scope_type
          AND scope_id = p_scope_id;
        RETURN TRUE;
    END IF;
    IF p_expected_policy_id IS NOT NULL THEN
        RETURN FALSE;
    END IF;

    PERFORM set_config('app.evaluation_head_cas', '1', TRUE);
    INSERT INTO evaluation_gate_policy_heads
        (tenant_id, environment, scope_type, scope_id, policy_id, event_id)
    VALUES
        (p_tenant_id, p_environment, p_scope_type, p_scope_id, p_policy_id, p_event_id);
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;

-- Compatibility for existing SQL callers. The wrapper derives the tenant from
-- the policy and delegates to the tenant-aware function.
CREATE OR REPLACE FUNCTION advance_evaluation_gate_policy_head(
    p_environment VARCHAR,
    p_scope_type VARCHAR,
    p_scope_id VARCHAR,
    p_policy_id UUID,
    p_event_id UUID,
    p_expected_policy_id UUID DEFAULT NULL
) RETURNS BOOLEAN AS $$
DECLARE
    policy_tenant BIGINT;
BEGIN
    SELECT tenant_id INTO policy_tenant
    FROM evaluation_gate_policies
    WHERE id = p_policy_id;
    IF NOT FOUND THEN
        RETURN FALSE;
    END IF;
    RETURN advance_evaluation_gate_policy_head(
        policy_tenant, p_environment, p_scope_type, p_scope_id,
        p_policy_id, p_event_id, p_expected_policy_id
    );
END;
$$ LANGUAGE plpgsql;
