DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'uq_evaluation_runs_id_tenant'
    ) THEN
        ALTER TABLE evaluation_runs
            ADD CONSTRAINT uq_evaluation_runs_id_tenant UNIQUE (id, tenant_id);
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS quality_reports (
    id UUID PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    run_id UUID NOT NULL,
    model_alias VARCHAR(128) NOT NULL,
    overall_conclusion VARCHAR(32) NOT NULL CHECK (overall_conclusion IN (
        'no_significant_anomaly', 'observe', 'suspected', 'high_risk', 'insufficient_coverage'
    )),
    adulteration_risk VARCHAR(32) NOT NULL CHECK (adulteration_risk IN (
        'no_significant_anomaly', 'observe', 'suspected', 'high_risk', 'insufficient_coverage'
    )),
    degradation_risk VARCHAR(32) NOT NULL CHECK (degradation_risk IN (
        'no_significant_anomaly', 'observe', 'suspected', 'high_risk', 'insufficient_coverage'
    )),
    policy_version VARCHAR(100) NOT NULL,
    generated_at TIMESTAMPTZ NOT NULL,
    fresh_until TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (id, tenant_id),
    UNIQUE (tenant_id, run_id, model_alias),
    CHECK (model_alias ~ '^[A-Za-z0-9._:/-]{1,128}$'),
    CHECK (fresh_until >= generated_at),
    CONSTRAINT fk_quality_reports_run_tenant
        FOREIGN KEY (run_id, tenant_id) REFERENCES evaluation_runs(id, tenant_id)
        ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_quality_reports_latest_public
    ON quality_reports (tenant_id, model_alias, generated_at DESC);

CREATE TABLE IF NOT EXISTS quality_dimension_results (
    id UUID PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    report_id UUID NOT NULL,
    dimension_key VARCHAR(32) NOT NULL CHECK (dimension_key IN (
        'knowledge_freshness', 'model_fingerprint', 'reasoning_stability', 'structure_compliance',
        'parameter_fidelity', 'instruction_hierarchy', 'protocol_schema', 'stream_completeness'
    )),
    status VARCHAR(32) NOT NULL CHECK (status IN (
        'no_significant_anomaly', 'observe', 'suspected', 'high_risk', 'insufficient_coverage'
    )),
    score NUMERIC(8,6) NOT NULL CHECK (score >= 0 AND score <= 1),
    sample_count INT NOT NULL CHECK (sample_count >= 0),
    confidence NUMERIC(8,6) NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    stable_baseline_delta_pp NUMERIC(8,4),
    reference_baseline_delta_pp NUMERIC(8,4),
    checked_at TIMESTAMPTZ NOT NULL,
    evidence_code VARCHAR(64) NOT NULL CHECK (evidence_code IN (
        'within_policy_bounds', 'coverage_insufficient', 'fingerprint_matched', 'fingerprint_mismatch',
        'reasoning_variance', 'structure_violation', 'parameter_deviation', 'instruction_violation',
        'protocol_violation', 'stream_incomplete', 'source_confirmed', 'source_inferred', 'source_insufficient_evidence'
    )),
    UNIQUE (id, tenant_id),
    UNIQUE (report_id, dimension_key),
    CONSTRAINT fk_quality_dimension_results_report_tenant
        FOREIGN KEY (report_id, tenant_id) REFERENCES quality_reports(id, tenant_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_quality_dimension_results_report
    ON quality_dimension_results (tenant_id, report_id, dimension_key);

CREATE TABLE IF NOT EXISTS quality_source_attributions (
    id UUID PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    report_id UUID NOT NULL,
    state VARCHAR(32) NOT NULL CHECK (state IN ('confirmed', 'inferred', 'insufficient_evidence')),
    display_name VARCHAR(200),
    confidence NUMERIC(8,6) CHECK (confidence >= 0 AND confidence <= 1),
    coverage NUMERIC(8,6) CHECK (coverage IS NULL OR (coverage >= 0 AND coverage <= 1)),
    alternate_candidates JSONB NOT NULL DEFAULT '[]'::jsonb,
    fingerprint_version VARCHAR(100) NOT NULL,
    evidence_code VARCHAR(64) NOT NULL CHECK (evidence_code IN (
        'within_policy_bounds', 'coverage_insufficient', 'fingerprint_matched', 'fingerprint_mismatch',
        'reasoning_variance', 'structure_violation', 'parameter_deviation', 'instruction_violation',
        'protocol_violation', 'stream_incomplete', 'source_confirmed', 'source_inferred', 'source_insufficient_evidence'
    )),
    UNIQUE (report_id),
    CHECK (display_name IS NULL OR display_name ~ '^[A-Za-z0-9 ._:/-]{1,200}$'),
    CHECK (
        (state = 'confirmed' AND display_name IS NOT NULL AND jsonb_array_length(alternate_candidates) = 0)
        OR (state = 'inferred' AND display_name IS NOT NULL AND confidence IS NOT NULL AND coverage IS NOT NULL AND jsonb_array_length(alternate_candidates) > 0)
        OR (state = 'insufficient_evidence' AND display_name IS NULL AND confidence IS NULL AND coverage IS NULL AND jsonb_array_length(alternate_candidates) = 0)
    ),
    CONSTRAINT fk_quality_source_attributions_report_tenant
        FOREIGN KEY (report_id, tenant_id) REFERENCES quality_reports(id, tenant_id)
        ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS quality_probe_observations (
    id UUID PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    report_id UUID NOT NULL,
    dimension_result_id UUID,
    probe_spec_hash CHAR(64) NOT NULL CHECK (probe_spec_hash ~ '^[0-9a-f]{64}$'),
    observation_hash CHAR(64) NOT NULL CHECK (observation_hash ~ '^[0-9a-f]{64}$'),
    event_class VARCHAR(32) NOT NULL CHECK (event_class IN (
        'request_shape', 'response_shape', 'stream_integrity', 'parameter_echo', 'fingerprint'
    )),
    event_digest CHAR(64) NOT NULL CHECK (event_digest ~ '^[0-9a-f]{64}$'),
    observed_at TIMESTAMPTZ NOT NULL,
    UNIQUE (report_id, observation_hash),
    CONSTRAINT fk_quality_probe_observations_report_tenant
        FOREIGN KEY (report_id, tenant_id) REFERENCES quality_reports(id, tenant_id)
        ON DELETE CASCADE,
    CONSTRAINT fk_quality_probe_observations_dimension_result_tenant
        FOREIGN KEY (dimension_result_id, tenant_id) REFERENCES quality_dimension_results(id, tenant_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_quality_probe_observations_report
    ON quality_probe_observations (tenant_id, report_id, observed_at DESC);

CREATE TABLE IF NOT EXISTS quality_policy_versions (
    id UUID PRIMARY KEY,
    tenant_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    version VARCHAR(100) NOT NULL,
    policy JSONB NOT NULL,
    created_by BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (tenant_id, version)
);

ALTER TABLE quality_reports
    ADD CONSTRAINT fk_quality_reports_policy_tenant
    FOREIGN KEY (tenant_id, policy_version) REFERENCES quality_policy_versions(tenant_id, version);

ALTER TABLE evaluation_cases
    ADD COLUMN IF NOT EXISTS quality_dimension TEXT,
    ADD COLUMN IF NOT EXISTS quality_probe_spec JSONB NOT NULL DEFAULT '{}'::jsonb;
