INSERT INTO quality_policy_versions (id, tenant_id, version, policy, created_by)
SELECT gen_random_uuid(), u.id, 'quality-v1',
       '{"minimum_coverage":0.8,"minimum_confidence":0.7,"minimum_margin":0.15,"minimum_samples_per_dimension":3,"observe_delta_pp":5,"suspected_delta_pp":10,"high_risk_delta_pp":20,"freshness_hours":24}'::jsonb,
       u.id
FROM users u
ON CONFLICT (tenant_id, version) DO NOTHING;

CREATE INDEX IF NOT EXISTS idx_evaluation_cases_quality_dimension
    ON evaluation_cases (quality_dimension) WHERE quality_dimension IS NOT NULL;
