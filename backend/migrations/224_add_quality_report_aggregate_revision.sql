ALTER TABLE quality_reports
    ADD COLUMN IF NOT EXISTS aggregate_revision BIGINT NOT NULL DEFAULT 0
        CHECK (aggregate_revision >= 0);

DO $$
DECLARE
    legacy_constraint RECORD;
BEGIN
    FOR legacy_constraint IN
        SELECT c.conname
        FROM pg_constraint c
        JOIN pg_index i ON i.indexrelid = c.conindid
        WHERE c.conrelid = 'quality_reports'::regclass
          AND c.contype = 'u'
          AND i.indisunique
          AND cardinality(c.conkey) = 3
          AND (
              SELECT array_agg(a.attname::text ORDER BY key_column.position)
              FROM unnest(c.conkey) WITH ORDINALITY AS key_column(attnum, position)
              JOIN pg_attribute a
                ON a.attrelid = c.conrelid
               AND a.attnum = key_column.attnum
          ) @> ARRAY['tenant_id', 'run_id', 'model_alias']
          AND (
              SELECT array_agg(a.attname::text ORDER BY key_column.position)
              FROM unnest(c.conkey) WITH ORDINALITY AS key_column(attnum, position)
              JOIN pg_attribute a
                ON a.attrelid = c.conrelid
               AND a.attnum = key_column.attnum
          ) <@ ARRAY['tenant_id', 'run_id', 'model_alias']
    LOOP
        EXECUTE format('ALTER TABLE quality_reports DROP CONSTRAINT %I', legacy_constraint.conname);
    END LOOP;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'quality_reports'::regclass
          AND conname = 'uq_quality_reports_tenant_run_model_revision'
    ) THEN
        ALTER TABLE quality_reports
            ADD CONSTRAINT uq_quality_reports_tenant_run_model_revision
            UNIQUE (tenant_id, run_id, model_alias, aggregate_revision);
    END IF;
END $$;

DROP INDEX IF EXISTS idx_quality_reports_latest_public;

CREATE INDEX IF NOT EXISTS idx_quality_reports_latest_public
    ON quality_reports (tenant_id, model_alias, aggregate_revision DESC, generated_at DESC);
