-- Separate billable production traffic from metadata and synthetic probes.
-- This migration is forward-only and idempotent. Existing non-empty valid
-- classifications are preserved on re-run.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '10min';

ALTER TABLE usage_logs
    ADD COLUMN IF NOT EXISTS traffic_class VARCHAR(16);

ALTER TABLE ops_error_logs
    ADD COLUMN IF NOT EXISTS traffic_class VARCHAR(16);

ALTER TABLE ops_system_metrics
    ADD COLUMN IF NOT EXISTS mem_available_mb BIGINT,
    ADD COLUMN IF NOT EXISTS swap_used_mb BIGINT,
    ADD COLUMN IF NOT EXISTS swap_total_mb BIGINT,
    ADD COLUMN IF NOT EXISTS disk_used_percent DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS oom_kill_count BIGINT,
    ADD COLUMN IF NOT EXISTS resource_warning VARCHAR(128);

-- Only rows without a classification are backfilled. This preserves any
-- valid classification written by an earlier partial migration or operator.
UPDATE usage_logs
SET traffic_class = 'metadata'
WHERE traffic_class IS NULL
  AND (
      lower(trim(trailing '/' FROM split_part(split_part(COALESCE(inbound_endpoint, ''), '?', 1), '#', 1)))
          IN ('/models', '/v1/models', '/v1beta/models')
      OR lower(trim(trailing '/' FROM split_part(split_part(COALESCE(upstream_endpoint, ''), '?', 1), '#', 1)))
          IN ('/models', '/v1/models', '/v1beta/models')
      OR lower(trim(trailing '/' FROM split_part(split_part(COALESCE(inbound_endpoint, ''), '?', 1), '#', 1))) LIKE '%/count_tokens'
      OR lower(trim(trailing '/' FROM split_part(split_part(COALESCE(upstream_endpoint, ''), '?', 1), '#', 1))) LIKE '%/count_tokens'
  );

UPDATE usage_logs
SET traffic_class = 'synthetic'
WHERE traffic_class IS NULL
  AND (
      lower(COALESCE(user_agent, '')) LIKE '%sub2api-acceptance-probe%'
      OR lower(COALESCE(user_agent, '')) LIKE '%sub2api-synthetic%'
      OR lower(COALESCE(user_agent, '')) LIKE '%sub2api-healthcheck%'
      OR lower(COALESCE(user_agent, '')) LIKE '%sub2api-smoke-test%'
      OR lower(COALESCE(user_agent, '')) LIKE '%sub2api-probe%'
  );

UPDATE usage_logs
SET traffic_class = 'production'
WHERE traffic_class IS NULL;

-- Error rows without reliable origin evidence remain visible as unknown.
UPDATE ops_error_logs
SET traffic_class = CASE
    WHEN is_count_tokens THEN 'metadata'
    WHEN lower(COALESCE(request_path, '')) IN ('/models', '/v1/models', '/v1beta/models')
      OR lower(COALESCE(inbound_endpoint, '')) IN ('/models', '/v1/models', '/v1beta/models')
      OR lower(COALESCE(upstream_endpoint, '')) IN ('/models', '/v1/models', '/v1beta/models')
      OR lower(COALESCE(request_path, '')) LIKE '%/count_tokens'
      OR lower(COALESCE(inbound_endpoint, '')) LIKE '%/count_tokens'
      OR lower(COALESCE(upstream_endpoint, '')) LIKE '%/count_tokens'
      THEN 'metadata'
    WHEN lower(COALESCE(user_agent, '')) LIKE '%sub2api-acceptance-probe%'
      OR lower(COALESCE(user_agent, '')) LIKE '%sub2api-synthetic%'
      OR lower(COALESCE(user_agent, '')) LIKE '%sub2api-healthcheck%'
      OR lower(COALESCE(user_agent, '')) LIKE '%sub2api-smoke-test%'
      OR lower(COALESCE(user_agent, '')) LIKE '%sub2api-probe%'
      THEN 'synthetic'
    ELSE 'unknown'
END
WHERE traffic_class IS NULL;

-- Normalize values that may have been written by an earlier partial rollout.
-- This keeps the check constraint safe on re-run and prevents malformed or
-- empty values from being treated as production traffic.
UPDATE usage_logs
SET traffic_class = CASE lower(trim(COALESCE(traffic_class, '')))
    WHEN 'production' THEN 'production'
    WHEN 'metadata' THEN 'metadata'
    WHEN 'synthetic' THEN 'synthetic'
    WHEN 'unknown' THEN 'unknown'
    ELSE 'unknown'
END;

UPDATE ops_error_logs
SET traffic_class = CASE lower(trim(COALESCE(traffic_class, '')))
    WHEN 'production' THEN 'production'
    WHEN 'metadata' THEN 'metadata'
    WHEN 'synthetic' THEN 'synthetic'
    WHEN 'unknown' THEN 'unknown'
    ELSE 'unknown'
END;

ALTER TABLE usage_logs
    ALTER COLUMN traffic_class SET DEFAULT 'unknown',
    ALTER COLUMN traffic_class SET NOT NULL;

ALTER TABLE ops_error_logs
    ALTER COLUMN traffic_class SET DEFAULT 'unknown',
    ALTER COLUMN traffic_class SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'usage_logs_traffic_class_check'
          AND conrelid = 'usage_logs'::regclass
    ) THEN
        ALTER TABLE usage_logs
            ADD CONSTRAINT usage_logs_traffic_class_check
            CHECK (traffic_class IN ('production', 'metadata', 'synthetic', 'unknown'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'ops_error_logs_traffic_class_check'
          AND conrelid = 'ops_error_logs'::regclass
    ) THEN
        ALTER TABLE ops_error_logs
            ADD CONSTRAINT ops_error_logs_traffic_class_check
            CHECK (traffic_class IN ('production', 'metadata', 'synthetic', 'unknown'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_usage_logs_traffic_class_created_at
    ON usage_logs (traffic_class, created_at);

CREATE INDEX IF NOT EXISTS idx_ops_error_logs_traffic_class_created_at
    ON ops_error_logs (traffic_class, created_at);

COMMENT ON COLUMN usage_logs.traffic_class IS 'production/metadata/synthetic/unknown';
COMMENT ON COLUMN ops_error_logs.traffic_class IS 'production/metadata/synthetic/unknown';
