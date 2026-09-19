ALTER TABLE evaluation_plans
    ADD COLUMN IF NOT EXISTS gateway_api_key_id BIGINT REFERENCES api_keys(id);

CREATE INDEX IF NOT EXISTS idx_evaluation_plans_gateway_api_key
    ON evaluation_plans (gateway_api_key_id)
    WHERE gateway_api_key_id IS NOT NULL;

COMMENT ON COLUMN evaluation_plans.gateway_api_key_id IS
    'Dedicated is_evaluation API key used by controlled Radar workers';
