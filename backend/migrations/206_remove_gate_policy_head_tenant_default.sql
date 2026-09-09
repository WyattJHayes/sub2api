-- Keep the seven-argument tenant-aware function distinct from the legacy
-- six-argument compatibility wrapper. Migration 203 is immutable because it
-- has already shipped, so the default is removed through this new migration.
DROP FUNCTION IF EXISTS advance_evaluation_gate_policy_head(
    BIGINT, VARCHAR, VARCHAR, VARCHAR, UUID, UUID, UUID
);

CREATE FUNCTION advance_evaluation_gate_policy_head(
    p_tenant_id BIGINT,
    p_environment VARCHAR,
    p_scope_type VARCHAR,
    p_scope_id VARCHAR,
    p_policy_id UUID,
    p_event_id UUID,
    p_expected_policy_id UUID
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
