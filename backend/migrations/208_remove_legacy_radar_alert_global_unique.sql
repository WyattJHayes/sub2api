-- Migration 200 attempted to remove this global key by name. PostgreSQL
-- truncates generated constraint names, so deployments can retain it. Remove
-- only the legacy definition and preserve the tenant-scoped unique key.

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT c.conname
        FROM pg_constraint c
        WHERE c.conrelid = 'evaluation_alerts'::regclass
          AND c.contype = 'u'
          AND pg_get_constraintdef(c.oid) LIKE
              'UNIQUE (model_route, capability_domain, cause, policy_version)%'
    LOOP
        EXECUTE format('ALTER TABLE evaluation_alerts DROP CONSTRAINT %I', constraint_name);
    END LOOP;
END $$;
