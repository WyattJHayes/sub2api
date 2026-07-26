CREATE TABLE IF NOT EXISTS evaluation_dataset_versions (
    id UUID PRIMARY KEY,
    dataset_key VARCHAR(100) NOT NULL,
    version VARCHAR(100) NOT NULL,
    manifest_sha256 CHAR(64) NOT NULL,
    source_type VARCHAR(20) NOT NULL CHECK (source_type IN ('public', 'synthetic', 'imported')),
    status VARCHAR(20) NOT NULL CHECK (status IN ('draft', 'published', 'retired')),
    created_by BIGINT NOT NULL REFERENCES users(id),
    published_at TIMESTAMPTZ,
    retired_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (dataset_key, version),
    CHECK ((status = 'published' AND published_at IS NOT NULL) OR status <> 'published')
);

CREATE TABLE IF NOT EXISTS evaluation_cases (
    id UUID PRIMARY KEY,
    dataset_version_id UUID NOT NULL REFERENCES evaluation_dataset_versions(id),
    case_key VARCHAR(160) NOT NULL,
    capability_domain VARCHAR(32) NOT NULL CHECK (capability_domain IN (
        'coding', 'reasoning', 'instruction', 'long_context', 'tool_call',
        'protocol', 'safety', 'performance', 'cost'
    )),
    priority VARCHAR(4) NOT NULL CHECK (priority IN ('P0', 'P1', 'P2')),
    weight NUMERIC(10,4) NOT NULL CHECK (weight > 0),
    sample_count INT NOT NULL CHECK (sample_count BETWEEN 1 AND 10),
    prompt_spec JSONB,
    expected_spec JSONB,
    encrypted_spec TEXT,
    execution_spec JSONB NOT NULL,
    grader_id VARCHAR(100) NOT NULL,
    grader_version VARCHAR(100) NOT NULL,
    content_sha256 CHAR(64) NOT NULL,
    confidentiality VARCHAR(20) NOT NULL CHECK (confidentiality IN ('public', 'synthetic', 'restricted', 'safety')),
    estimated_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (estimated_cost >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (dataset_version_id, case_key),
    CHECK (
        (confidentiality IN ('public', 'synthetic')
            AND prompt_spec IS NOT NULL AND expected_spec IS NOT NULL AND encrypted_spec IS NULL)
        OR (confidentiality IN ('restricted', 'safety')
            AND prompt_spec IS NULL AND expected_spec IS NULL AND encrypted_spec IS NOT NULL)
    )
);

CREATE TABLE IF NOT EXISTS evaluation_plans (
    id UUID PRIMARY KEY,
    name VARCHAR(120) NOT NULL,
    dataset_version_id UUID NOT NULL REFERENCES evaluation_dataset_versions(id),
    trigger_type VARCHAR(20) NOT NULL CHECK (trigger_type IN ('manual', 'cron', 'release', 'event')),
    cron_expression VARCHAR(100),
    model_matrix JSONB NOT NULL,
    max_run_cost NUMERIC(20,8) NOT NULL CHECK (max_run_cost > 0),
    daily_cost_limit NUMERIC(20,8) NOT NULL CHECK (daily_cost_limit > 0),
    max_concurrency INT NOT NULL CHECK (max_concurrency BETWEEN 1 AND 1000),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    created_by BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((trigger_type = 'cron' AND cron_expression IS NOT NULL) OR trigger_type <> 'cron')
);

CREATE TABLE IF NOT EXISTS evaluation_runs (
    id UUID PRIMARY KEY,
    plan_id UUID NOT NULL REFERENCES evaluation_plans(id),
    trigger_source VARCHAR(20) NOT NULL CHECK (trigger_source IN ('manual', 'cron', 'release', 'event', 'recovery')),
    baseline_ref JSONB NOT NULL,
    candidate_ref JSONB NOT NULL,
    status VARCHAR(24) NOT NULL CHECK (status IN (
        'pending', 'running', 'paused', 'budget_paused', 'completed', 'failed', 'cancelled'
    )),
    budget_limit NUMERIC(20,8) NOT NULL CHECK (budget_limit > 0),
    reserved_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (reserved_cost >= 0 AND reserved_cost <= budget_limit),
    actual_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (actual_cost >= 0),
    calibration_mode BOOLEAN NOT NULL DEFAULT TRUE,
    created_by BIGINT REFERENCES users(id),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_workers (
    id UUID PRIMARY KEY,
    name VARCHAR(120) NOT NULL UNIQUE,
    worker_kind VARCHAR(20) NOT NULL CHECK (worker_kind IN ('runner', 'grader', 'statistics')),
    token_hash CHAR(64) NOT NULL UNIQUE,
    status VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    capabilities TEXT[] NOT NULL DEFAULT '{}',
    max_concurrency INT NOT NULL DEFAULT 1 CHECK (max_concurrency BETWEEN 1 AND 1000),
    last_heartbeat_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_samples (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    case_id UUID NOT NULL REFERENCES evaluation_cases(id),
    model_route VARCHAR(200) NOT NULL,
    model_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    model_config_sha256 CHAR(64) NOT NULL DEFAULT '44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a',
    sample_index INT NOT NULL CHECK (sample_index BETWEEN 0 AND 9),
    priority VARCHAR(4) NOT NULL CHECK (priority IN ('P0', 'P1', 'P2')),
    status VARCHAR(24) NOT NULL CHECK (status IN (
        'pending', 'leased', 'running', 'evidence_uploaded', 'grading', 'completed',
        'infra_failed', 'upstream_failed', 'invalid_evidence', 'grading_failed', 'cancelled'
    )),
    estimated_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (estimated_cost >= 0),
    actual_cost NUMERIC(20,8) NOT NULL DEFAULT 0 CHECK (actual_cost >= 0),
    failure_class VARCHAR(24) CHECK (failure_class IN (
        'capability', 'protocol', 'upstream', 'infrastructure', 'judge',
        'invalid_evidence', 'safety', 'performance', 'cost'
    )),
    failure_code VARCHAR(100),
    route_trace_id VARCHAR(64),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, case_id, model_route, sample_index)
);

CREATE TABLE IF NOT EXISTS evaluation_assignments (
    id UUID PRIMARY KEY,
    sample_id UUID NOT NULL REFERENCES evaluation_samples(id),
    attempt INT NOT NULL CHECK (attempt > 0),
    idempotency_key CHAR(64) NOT NULL UNIQUE,
    status VARCHAR(24) NOT NULL CHECK (status IN (
        'pending', 'leased', 'running', 'evidence_uploaded', 'grading', 'completed',
        'infra_failed', 'upstream_failed', 'invalid_evidence', 'grading_failed', 'cancelled'
    )),
    lease_token_hash CHAR(64),
    leased_by UUID REFERENCES evaluation_workers(id),
    lease_expires_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    evidence_manifest JSONB,
    evidence_receipt_id UUID,
    failure_class VARCHAR(24) CHECK (failure_class IN (
        'capability', 'protocol', 'upstream', 'infrastructure', 'judge',
        'invalid_evidence', 'safety', 'performance', 'cost'
    )),
    failure_code VARCHAR(100),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (sample_id, attempt),
    CHECK (
        (status IN ('leased', 'running') AND lease_token_hash IS NOT NULL
            AND leased_by IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR status NOT IN ('leased', 'running')
    )
);

CREATE TABLE IF NOT EXISTS evaluation_artifacts (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    sample_id UUID NOT NULL REFERENCES evaluation_samples(id),
    assignment_id UUID NOT NULL REFERENCES evaluation_assignments(id),
    object_key VARCHAR(500) NOT NULL UNIQUE,
    sha256 CHAR(64) NOT NULL,
    byte_count BIGINT NOT NULL CHECK (byte_count >= 0),
    mime_type VARCHAR(100) NOT NULL,
    scan_status VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (scan_status IN ('pending', 'clean', 'rejected', 'failed')),
    retention_deadline TIMESTAMPTZ NOT NULL,
    confirmed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_run_events (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    event_type VARCHAR(80) NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    actor_type VARCHAR(20) NOT NULL DEFAULT 'system' CHECK (actor_type IN ('system', 'user', 'worker')),
    actor_ref VARCHAR(100),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_budget_ledger (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    sample_id UUID REFERENCES evaluation_samples(id),
    assignment_id UUID REFERENCES evaluation_assignments(id),
    entry_type VARCHAR(20) NOT NULL CHECK (entry_type IN ('reservation', 'release', 'actual', 'warning')),
    amount NUMERIC(20,8) NOT NULL CHECK (amount >= 0),
    idempotency_key CHAR(64) NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_evaluation_cases_dataset_domain
    ON evaluation_cases (dataset_version_id, capability_domain, priority);
CREATE INDEX IF NOT EXISTS idx_evaluation_plans_enabled_trigger
    ON evaluation_plans (enabled, trigger_type);
CREATE INDEX IF NOT EXISTS idx_evaluation_runs_plan_created
    ON evaluation_runs (plan_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_evaluation_runs_status_created
    ON evaluation_runs (status, created_at);
CREATE INDEX IF NOT EXISTS idx_evaluation_samples_run_status_priority
    ON evaluation_samples (run_id, status, priority);
CREATE INDEX IF NOT EXISTS idx_evaluation_assignments_claim
    ON evaluation_assignments (status, created_at) WHERE status IN ('pending', 'leased', 'running');
CREATE INDEX IF NOT EXISTS idx_evaluation_assignments_lease_expiry
    ON evaluation_assignments (lease_expires_at) WHERE status IN ('leased', 'running');
CREATE INDEX IF NOT EXISTS idx_evaluation_workers_status_kind
    ON evaluation_workers (status, worker_kind);
CREATE INDEX IF NOT EXISTS idx_evaluation_artifacts_retention
    ON evaluation_artifacts (retention_deadline, scan_status);
CREATE INDEX IF NOT EXISTS idx_evaluation_run_events_run_created
    ON evaluation_run_events (run_id, created_at);
CREATE INDEX IF NOT EXISTS idx_evaluation_budget_ledger_run_created
    ON evaluation_budget_ledger (run_id, created_at);

CREATE OR REPLACE FUNCTION enforce_evaluation_dataset_version_lifecycle()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.status IN ('published', 'retired') THEN
            RAISE EXCEPTION 'published evaluation dataset version % cannot be deleted', OLD.id;
        END IF;
        RETURN OLD;
    END IF;

    IF TG_OP = 'UPDATE' AND OLD.status IN ('published', 'retired') THEN
        IF OLD.status = 'published'
            AND NEW.status = 'retired'
            AND NEW.retired_at IS NOT NULL
            AND NEW.id IS NOT DISTINCT FROM OLD.id
            AND NEW.dataset_key IS NOT DISTINCT FROM OLD.dataset_key
            AND NEW.version IS NOT DISTINCT FROM OLD.version
            AND NEW.manifest_sha256 IS NOT DISTINCT FROM OLD.manifest_sha256
            AND NEW.source_type IS NOT DISTINCT FROM OLD.source_type
            AND NEW.created_by IS NOT DISTINCT FROM OLD.created_by
            AND NEW.published_at IS NOT DISTINCT FROM OLD.published_at
            AND NEW.created_at IS NOT DISTINCT FROM OLD.created_at THEN
            RETURN NEW;
        END IF;

        RAISE EXCEPTION 'published evaluation dataset version % is immutable', OLD.id;
    END IF;

    IF NEW.status = 'published' THEN
        IF NEW.source_type NOT IN ('public', 'synthetic') THEN
            RAISE EXCEPTION 'evaluation dataset version % with source type % cannot be published', NEW.id, NEW.source_type;
        END IF;

        IF EXISTS (
            SELECT 1
            FROM evaluation_cases
            WHERE dataset_version_id = NEW.id
              AND confidentiality NOT IN ('public', 'synthetic')
        ) THEN
            RAISE EXCEPTION 'evaluation dataset version % contains cases outside P1 provenance', NEW.id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_dataset_versions_lifecycle ON evaluation_dataset_versions;
CREATE TRIGGER trg_evaluation_dataset_versions_lifecycle
    BEFORE INSERT OR UPDATE OR DELETE ON evaluation_dataset_versions
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_dataset_version_lifecycle();

CREATE OR REPLACE FUNCTION enforce_evaluation_case_dataset_lifecycle()
RETURNS TRIGGER AS $$
DECLARE
    parent_status VARCHAR(20);
BEGIN
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        SELECT status INTO parent_status
        FROM evaluation_dataset_versions
        WHERE id = OLD.dataset_version_id
        FOR SHARE;

        IF parent_status IS DISTINCT FROM 'draft' THEN
            RAISE EXCEPTION 'cases in evaluation dataset version % cannot be changed after publication', OLD.dataset_version_id;
        END IF;
    END IF;

    IF TG_OP IN ('INSERT', 'UPDATE') THEN
        SELECT status INTO parent_status
        FROM evaluation_dataset_versions
        WHERE id = NEW.dataset_version_id
        FOR SHARE;

        IF parent_status IS DISTINCT FROM 'draft' THEN
            RAISE EXCEPTION 'cases cannot be added to evaluation dataset version % after publication', NEW.dataset_version_id;
        END IF;
    END IF;

    IF TG_OP = 'DELETE' THEN
        RETURN OLD;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_cases_dataset_lifecycle ON evaluation_cases;
CREATE TRIGGER trg_evaluation_cases_dataset_lifecycle
    BEFORE INSERT OR UPDATE OR DELETE ON evaluation_cases
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_case_dataset_lifecycle();

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

CREATE OR REPLACE FUNCTION prevent_terminal_evaluation_run_reopen()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IN ('completed', 'failed', 'cancelled') AND NEW.status <> OLD.status THEN
        RAISE EXCEPTION 'terminal evaluation run status % cannot transition to %', OLD.status, NEW.status;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_runs_terminal_status ON evaluation_runs;
CREATE TRIGGER trg_evaluation_runs_terminal_status
    BEFORE UPDATE OF status ON evaluation_runs
    FOR EACH ROW EXECUTE FUNCTION prevent_terminal_evaluation_run_reopen();
