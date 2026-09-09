-- Radar baseline heads are tenant-owned. The original key omitted tenant_id,
-- causing equal scopes and model routes from different tenants to share one
-- lineage. Preserve existing heads and make every CAS operation tenant-aware.

ALTER TABLE evaluation_baseline_heads
    ADD COLUMN IF NOT EXISTS tenant_id BIGINT;

-- The governance head trigger permits only CAS helper writes. This migration
-- derives ownership from the immutable baseline lineage in one transaction.
SELECT set_config('app.evaluation_head_cas', '1', TRUE);

UPDATE evaluation_baseline_heads h
SET tenant_id = b.tenant_id
FROM evaluation_baselines b
WHERE h.baseline_id = b.id
  AND h.tenant_id IS NULL;

UPDATE evaluation_baseline_heads
SET tenant_id = 0
WHERE tenant_id IS NULL;

ALTER TABLE evaluation_baseline_heads
    ALTER COLUMN tenant_id SET NOT NULL;

DO $$
DECLARE
    primary_key_name TEXT;
BEGIN
    SELECT conname INTO primary_key_name
    FROM pg_constraint
    WHERE conrelid = 'evaluation_baseline_heads'::regclass
      AND contype = 'p';
    IF primary_key_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE evaluation_baseline_heads DROP CONSTRAINT %I', primary_key_name);
    END IF;
END $$;

ALTER TABLE evaluation_baseline_heads
    ADD CONSTRAINT evaluation_baseline_heads_pkey
    PRIMARY KEY (tenant_id, environment, scope_type, scope_id, model_route);

CREATE INDEX IF NOT EXISTS idx_evaluation_baseline_heads_tenant_baseline
    ON evaluation_baseline_heads (tenant_id, baseline_id);

CREATE OR REPLACE FUNCTION advance_evaluation_baseline_head(
    p_tenant_id BIGINT,
    p_environment VARCHAR,
    p_scope_type VARCHAR,
    p_scope_id VARCHAR,
    p_model_route VARCHAR,
    p_baseline_id UUID,
    p_event_id UUID,
    p_expected_baseline_id UUID
) RETURNS BOOLEAN AS $$
DECLARE
    current_baseline UUID;
    baseline_tenant BIGINT;
BEGIN
    SELECT tenant_id INTO baseline_tenant
    FROM evaluation_baselines
    WHERE id = p_baseline_id;
    IF NOT FOUND OR baseline_tenant IS DISTINCT FROM p_tenant_id THEN
        RETURN FALSE;
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(
        'baseline:' || p_tenant_id::text || ':' || p_environment || ':' ||
        p_scope_type || ':' || p_scope_id || ':' || p_model_route,
        0
    ));

    SELECT baseline_id INTO current_baseline
    FROM evaluation_baseline_heads
    WHERE tenant_id = p_tenant_id
      AND environment = p_environment
      AND scope_type = p_scope_type
      AND scope_id = p_scope_id
      AND model_route = p_model_route
    FOR UPDATE;
    IF FOUND THEN
        IF current_baseline IS DISTINCT FROM p_expected_baseline_id THEN
            RETURN FALSE;
        END IF;
        PERFORM set_config('app.evaluation_head_cas', '1', TRUE);
        UPDATE evaluation_baseline_heads
        SET baseline_id = p_baseline_id,
            event_id = p_event_id,
            updated_at = transaction_timestamp()
        WHERE tenant_id = p_tenant_id
          AND environment = p_environment
          AND scope_type = p_scope_type
          AND scope_id = p_scope_id
          AND model_route = p_model_route;
        RETURN TRUE;
    END IF;
    IF p_expected_baseline_id IS NOT NULL THEN
        RETURN FALSE;
    END IF;

    PERFORM set_config('app.evaluation_head_cas', '1', TRUE);
    INSERT INTO evaluation_baseline_heads
        (tenant_id, environment, scope_type, scope_id, model_route, baseline_id, event_id)
    VALUES
        (p_tenant_id, p_environment, p_scope_type, p_scope_id, p_model_route, p_baseline_id, p_event_id);
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;

-- Keep existing SQL callers working while they migrate. Ownership is derived
-- from the baseline and delegated to the tenant-aware implementation.
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
    baseline_tenant BIGINT;
BEGIN
    SELECT tenant_id INTO baseline_tenant
    FROM evaluation_baselines
    WHERE id = p_baseline_id;
    IF NOT FOUND THEN
        RETURN FALSE;
    END IF;
    RETURN advance_evaluation_baseline_head(
        baseline_tenant, p_environment, p_scope_type, p_scope_id,
        p_model_route, p_baseline_id, p_event_id, p_expected_baseline_id
    );
END;
$$ LANGUAGE plpgsql;
