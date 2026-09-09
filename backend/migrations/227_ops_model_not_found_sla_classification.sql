-- Reclassify local unsupported-model responses that were logged before
-- model_not_found became a recognized Ops error type.
--
-- The upstream guards are deliberate: a provider's real model 404 must remain
-- an SLA-scoped provider error. This update is idempotent and only changes rows
-- that still carry the old local classification.
UPDATE ops_error_logs
SET error_type = 'model_not_found',
    error_phase = 'request',
    error_owner = 'client',
    error_source = 'client_request',
    severity = 'P3',
    is_business_limited = TRUE
WHERE status_code = 404
  AND COALESCE(is_business_limited, FALSE) = FALSE
  AND error_type = 'api_error'
  AND error_owner = 'platform'
  AND error_source = 'gateway'
  AND COALESCE(upstream_status_code, 0) = 0
  AND COALESCE(upstream_errors, '[]'::jsonb) = '[]'::jsonb
  AND error_message ILIKE 'Model % is not supported by any configured account in this group';
