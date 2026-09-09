-- Rebuild derived Ops pre-aggregates after traffic classification is introduced.
--
-- Before migration 231, hourly/daily rows could include metadata and synthetic
-- traffic because the source tables had no traffic_class column. Those derived
-- rows cannot be corrected in place, so invalidate only the pre-aggregate tables
-- and let the background job rebuild them from the classified raw logs.
-- Raw usage_logs and ops_error_logs are intentionally untouched.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '1min';

DO $$
BEGIN
    IF to_regclass('public.ops_metrics_daily') IS NOT NULL THEN
        TRUNCATE TABLE public.ops_metrics_daily;
    END IF;
    IF to_regclass('public.ops_metrics_hourly') IS NOT NULL THEN
        TRUNCATE TABLE public.ops_metrics_hourly;
    END IF;
END $$;
