-- Radar P2 governance: scoped roles, immutable baselines and policies,
-- release decisions, controlled waivers, and append-only alert history.

CREATE TABLE IF NOT EXISTS evaluation_role_bindings (
    id UUID PRIMARY KEY,
    actor_id BIGINT NOT NULL REFERENCES users(id),
    role VARCHAR(32) NOT NULL CHECK (role IN ('viewer', 'test_operator', 'quality_admin', 'release_manager', 'platform_admin')),
    scope JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_by BIGINT REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disabled_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_evaluation_role_bindings_active
    ON evaluation_role_bindings (actor_id, role, md5(scope::text)) WHERE enabled;

CREATE TABLE IF NOT EXISTS evaluation_baselines (
    id UUID PRIMARY KEY,
    model_route VARCHAR(200) NOT NULL,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    dataset_manifest_sha256 CHAR(64) NOT NULL,
    evidence_hash CHAR(64) NOT NULL,
    route_profile_version VARCHAR(100) NOT NULL,
    policy_version INT NOT NULL CHECK (policy_version > 0),
    status VARCHAR(24) NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'active', 'retired', 'rejected')),
    proposed_by BIGINT NOT NULL REFERENCES users(id),
    proposed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    activated_at TIMESTAMPTZ,
    retired_at TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_evaluation_baselines_active_route
    ON evaluation_baselines (model_route) WHERE status = 'active';

CREATE TABLE IF NOT EXISTS evaluation_baseline_approvals (
    id UUID PRIMARY KEY,
    baseline_id UUID NOT NULL REFERENCES evaluation_baselines(id),
    approver_id BIGINT NOT NULL REFERENCES users(id),
    role VARCHAR(32) NOT NULL CHECK (role IN ('quality_admin', 'release_manager')),
    evidence_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (baseline_id, approver_id, role)
);

CREATE TABLE IF NOT EXISTS evaluation_gate_policies (
    id UUID PRIMARY KEY,
    version INT NOT NULL UNIQUE CHECK (version > 0),
    policy JSONB NOT NULL,
    policy_hash CHAR(64) NOT NULL,
    enforcement_starts_at TIMESTAMPTZ NOT NULL,
    created_by BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    retired_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS evaluation_gate_decisions (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    baseline_id UUID REFERENCES evaluation_baselines(id),
    policy_id UUID NOT NULL REFERENCES evaluation_gate_policies(id),
    status VARCHAR(32) NOT NULL CHECK (status IN ('recorded', 'passed', 'blocked', 'review_required', 'insufficient_evidence', 'waived')),
    rule_ids TEXT[] NOT NULL DEFAULT '{}',
    evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
    evidence_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, policy_id)
);

CREATE TABLE IF NOT EXISTS evaluation_gate_waivers (
    id UUID PRIMARY KEY,
    decision_id UUID NOT NULL REFERENCES evaluation_gate_decisions(id),
    business_reason TEXT NOT NULL,
    risk_owner_user_id BIGINT NOT NULL REFERENCES users(id),
    mitigation TEXT NOT NULL,
    retest_plan TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    approved_by BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK (expires_at > created_at)
);

CREATE TABLE IF NOT EXISTS evaluation_alerts (
    id UUID PRIMARY KEY,
    model_route VARCHAR(200) NOT NULL,
    capability_domain VARCHAR(32) NOT NULL,
    cause VARCHAR(32) NOT NULL CHECK (cause IN ('upstream_model', 'channel_or_pool', 'gateway_protocol', 'service_quality', 'insufficient_evidence')),
    policy_version INT NOT NULL CHECK (policy_version > 0),
    status VARCHAR(24) NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'acknowledged', 'resolved')),
    severity VARCHAR(8) NOT NULL DEFAULT 'P1' CHECK (severity IN ('P0', 'P1', 'P2')),
    attribution_confidence NUMERIC(5,4),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    acknowledged_at TIMESTAMPTZ,
    resolved_at TIMESTAMPTZ,
    recovery_test_id UUID,
    UNIQUE (model_route, capability_domain, cause, policy_version) DEFERRABLE INITIALLY IMMEDIATE
);

CREATE TABLE IF NOT EXISTS evaluation_alert_events (
    id UUID PRIMARY KEY,
    alert_id UUID NOT NULL REFERENCES evaluation_alerts(id),
    kind VARCHAR(32) NOT NULL CHECK (kind IN ('observed', 'acknowledged', 'diagnostic_requested', 'recommendation', 'recovery_test', 'resolved')),
    actor_id BIGINT REFERENCES users(id),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_attributions (
    id UUID PRIMARY KEY,
    alert_id UUID NOT NULL REFERENCES evaluation_alerts(id),
    cause VARCHAR(32) NOT NULL,
    confidence NUMERIC(5,4),
    route_slices JSONB NOT NULL DEFAULT '[]'::jsonb,
    evidence_hash CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_evaluation_alerts_status_seen ON evaluation_alerts (status, first_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_evaluation_alert_events_alert_created ON evaluation_alert_events (alert_id, created_at);
CREATE INDEX IF NOT EXISTS idx_evaluation_gate_decisions_run_created ON evaluation_gate_decisions (run_id, created_at DESC);
