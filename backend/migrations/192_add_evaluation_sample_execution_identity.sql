ALTER TABLE evaluation_samples
    ADD COLUMN IF NOT EXISTS model_config JSONB,
    ADD COLUMN IF NOT EXISTS model_config_sha256 CHAR(64);

UPDATE evaluation_samples
SET model_config = '{}'::jsonb,
    model_config_sha256 = '44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a'
WHERE model_config IS NULL
   OR model_config_sha256 IS NULL;

ALTER TABLE evaluation_samples
    ALTER COLUMN model_config SET DEFAULT '{}'::jsonb,
    ALTER COLUMN model_config SET NOT NULL,
    ALTER COLUMN model_config_sha256 SET DEFAULT '44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a',
    ALTER COLUMN model_config_sha256 SET NOT NULL;

ALTER TABLE evaluation_samples
    DROP CONSTRAINT IF EXISTS evaluation_samples_model_config_sha256_check,
    ADD CONSTRAINT evaluation_samples_model_config_sha256_check
        CHECK (model_config_sha256 ~ '^[0-9a-f]{64}$');

CREATE OR REPLACE FUNCTION enforce_evaluation_sample_execution_identity()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.run_id IS DISTINCT FROM OLD.run_id
        OR NEW.case_id IS DISTINCT FROM OLD.case_id
        OR NEW.model_route IS DISTINCT FROM OLD.model_route
        OR NEW.model_config IS DISTINCT FROM OLD.model_config
        OR NEW.model_config_sha256 IS DISTINCT FROM OLD.model_config_sha256
        OR NEW.sample_index IS DISTINCT FROM OLD.sample_index
        OR NEW.priority IS DISTINCT FROM OLD.priority
        OR NEW.estimated_cost IS DISTINCT FROM OLD.estimated_cost THEN
        RAISE EXCEPTION 'evaluation sample execution identity % is immutable', OLD.id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_samples_execution_identity ON evaluation_samples;
CREATE TRIGGER trg_evaluation_samples_execution_identity
    BEFORE UPDATE ON evaluation_samples
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_sample_execution_identity();
