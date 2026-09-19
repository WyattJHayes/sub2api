-- Radar P1 grading and statistics persistence.
-- Score and aggregate rows are append-only and partitioned by their event time.
-- The small idempotency table keeps submission keys globally unique across
-- monthly score partitions.

CREATE TABLE IF NOT EXISTS evaluation_score_idempotency (
    submission_idempotency_key CHAR(64) PRIMARY KEY,
    score_id UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS evaluation_grading_jobs (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    sample_id UUID NOT NULL REFERENCES evaluation_samples(id),
    assignment_id UUID NOT NULL UNIQUE REFERENCES evaluation_assignments(id),
    grader_id VARCHAR(100) NOT NULL,
    grader_version VARCHAR(100) NOT NULL,
    attempt INT NOT NULL DEFAULT 1 CHECK (attempt > 0),
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'leased', 'completed', 'failed')),
    lease_token_hash CHAR(64),
    leased_by UUID REFERENCES evaluation_workers(id),
    lease_expires_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    score_id UUID,
    failure_class VARCHAR(24),
    failure_code VARCHAR(100),
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CHECK ((status = 'leased' AND lease_token_hash IS NOT NULL AND leased_by IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR status <> 'leased')
);

CREATE INDEX IF NOT EXISTS idx_evaluation_grading_jobs_claim
    ON evaluation_grading_jobs (status, created_at)
    WHERE status IN ('pending', 'leased');
CREATE INDEX IF NOT EXISTS idx_evaluation_grading_jobs_lease_expiry
    ON evaluation_grading_jobs (lease_expires_at)
    WHERE status = 'leased';

CREATE TABLE IF NOT EXISTS evaluation_scores (
    id UUID NOT NULL,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    sample_id UUID NOT NULL REFERENCES evaluation_samples(id),
    grader_id VARCHAR(100) NOT NULL,
    grader_version VARCHAR(100) NOT NULL,
    version INT NOT NULL CHECK (version > 0),
    score NUMERIC(12,10) NOT NULL CHECK (score >= 0 AND score <= 1),
    passed BOOLEAN,
    failure_class VARCHAR(24),
    failure_code VARCHAR(100),
    explanation TEXT NOT NULL DEFAULT '',
    evidence_hashes TEXT[] NOT NULL DEFAULT '{}',
    is_current BOOLEAN NOT NULL DEFAULT TRUE,
    manual_review_required BOOLEAN NOT NULL DEFAULT FALSE,
    submission_idempotency_key CHAR(64) NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, created_at),
    CHECK (failure_class IS NULL OR failure_class IN (
        'capability', 'protocol', 'upstream', 'infrastructure', 'judge',
        'invalid_evidence', 'safety', 'performance', 'cost'
    ))
) PARTITION BY RANGE (created_at);

CREATE INDEX IF NOT EXISTS idx_evaluation_scores_id ON evaluation_scores (id);
CREATE INDEX IF NOT EXISTS idx_evaluation_scores_sample_grader_current
    ON evaluation_scores (sample_id, grader_id, is_current);
CREATE INDEX IF NOT EXISTS idx_evaluation_scores_run ON evaluation_scores (run_id, created_at);

CREATE TABLE IF NOT EXISTS evaluation_score_heads (
    sample_id UUID NOT NULL REFERENCES evaluation_samples(id),
    grader_id VARCHAR(100) NOT NULL,
    score_id UUID NOT NULL,
    version INT NOT NULL CHECK (version > 0),
    manual_review_required BOOLEAN NOT NULL DEFAULT FALSE,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (sample_id, grader_id)
);

CREATE TABLE IF NOT EXISTS evaluation_analysis_jobs (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    capability_domain VARCHAR(32) NOT NULL,
    model_route VARCHAR(200) NOT NULL,
    "window" VARCHAR(32) NOT NULL,
    analysis_version VARCHAR(100) NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'leased', 'completed', 'failed')),
    lease_token_hash CHAR(64),
    leased_by UUID REFERENCES evaluation_workers(id),
    lease_expires_at TIMESTAMPTZ,
    heartbeat_at TIMESTAMPTZ,
    snapshot_id UUID,
    submission_idempotency_key CHAR(64),
    failure_code VARCHAR(100),
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (run_id, capability_domain, model_route, "window", analysis_version, window_start),
    CHECK ((status = 'leased' AND lease_token_hash IS NOT NULL AND leased_by IS NOT NULL AND lease_expires_at IS NOT NULL)
        OR status <> 'leased')
);

CREATE INDEX IF NOT EXISTS idx_evaluation_analysis_jobs_claim
    ON evaluation_analysis_jobs (status, created_at)
    WHERE status IN ('pending', 'leased');

CREATE TABLE IF NOT EXISTS evaluation_aggregate_snapshots (
    id UUID NOT NULL,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    capability_domain VARCHAR(32) NOT NULL,
    model_route VARCHAR(200) NOT NULL,
    "window" VARCHAR(32) NOT NULL,
    analysis_version VARCHAR(100) NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    score_ids UUID[] NOT NULL DEFAULT '{}',
    aggregate JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (id, window_start),
    UNIQUE (run_id, capability_domain, model_route, "window", analysis_version, window_start)
) PARTITION BY RANGE (window_start);

CREATE INDEX IF NOT EXISTS idx_evaluation_aggregate_snapshots_run
    ON evaluation_aggregate_snapshots (run_id, window_start);

CREATE OR REPLACE FUNCTION enforce_evaluation_score_immutability()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'evaluation score % is immutable', OLD.id;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_scores_immutable ON evaluation_scores;
CREATE TRIGGER trg_evaluation_scores_immutable
    BEFORE UPDATE ON evaluation_scores
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_score_immutability();

CREATE OR REPLACE FUNCTION enforce_evaluation_aggregate_immutability()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'evaluation aggregate snapshot % is immutable', OLD.id;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_aggregate_snapshots_immutable ON evaluation_aggregate_snapshots;
CREATE TRIGGER trg_evaluation_aggregate_snapshots_immutable
    BEFORE UPDATE ON evaluation_aggregate_snapshots
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_aggregate_immutability();

CREATE TABLE IF NOT EXISTS evaluation_manual_reviews (
    id UUID PRIMARY KEY,
    run_id UUID NOT NULL REFERENCES evaluation_runs(id),
    sample_id UUID NOT NULL REFERENCES evaluation_samples(id),
    score_id UUID NOT NULL,
    reason VARCHAR(100) NOT NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'resolved', 'dismissed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    UNIQUE (score_id)
);

-- Keep the current and next two monthly partitions available on every deploy.
DO $$
DECLARE
    month_start DATE := date_trunc('month', CURRENT_DATE)::date;
    next_start DATE;
    name TEXT;
BEGIN
    FOR i IN 0..2 LOOP
        next_start := (month_start + (i + 1) * INTERVAL '1 month')::date;
        name := 'evaluation_scores_' || to_char(month_start + i * INTERVAL '1 month', 'YYYYMM');
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF evaluation_scores FOR VALUES FROM (%L) TO (%L)',
            name, (month_start + i * INTERVAL '1 month')::date, next_start);
        name := 'evaluation_aggregate_snapshots_' || to_char(month_start + i * INTERVAL '1 month', 'YYYYMM');
        EXECUTE format(
            'CREATE TABLE IF NOT EXISTS %I PARTITION OF evaluation_aggregate_snapshots FOR VALUES FROM (%L) TO (%L)',
            name, (month_start + i * INTERVAL '1 month')::date, next_start);
    END LOOP;
END $$;

CREATE TABLE IF NOT EXISTS evaluation_scores_default
    PARTITION OF evaluation_scores DEFAULT;
CREATE TABLE IF NOT EXISTS evaluation_aggregate_snapshots_default
    PARTITION OF evaluation_aggregate_snapshots DEFAULT;

-- Assignment creation is the durable source of grading work. Claims still
-- require the assignment to have uploaded evidence, so a fresh runner lease
-- cannot be graded prematurely.
CREATE OR REPLACE FUNCTION enqueue_evaluation_grading_job()
RETURNS TRIGGER AS $$
DECLARE
    sample_run_id UUID;
    case_grader_id VARCHAR(100);
    case_grader_version VARCHAR(100);
BEGIN
    SELECT s.run_id, c.grader_id, c.grader_version
    INTO sample_run_id, case_grader_id, case_grader_version
    FROM evaluation_samples s
    JOIN evaluation_cases c ON c.id = s.case_id
    WHERE s.id = NEW.sample_id;
    IF sample_run_id IS NULL THEN
        RETURN NEW;
    END IF;
    INSERT INTO evaluation_grading_jobs (
        id, run_id, sample_id, assignment_id, grader_id, grader_version, attempt, status
    ) VALUES (
        md5(NEW.id::text || ':grading')::uuid, sample_run_id, NEW.sample_id, NEW.id,
        case_grader_id, case_grader_version, NEW.attempt, 'pending'
    ) ON CONFLICT (assignment_id) DO NOTHING;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_assignments_grading_job ON evaluation_assignments;
CREATE TRIGGER trg_evaluation_assignments_grading_job
    AFTER INSERT ON evaluation_assignments
    FOR EACH ROW EXECUTE FUNCTION enqueue_evaluation_grading_job();

-- Backfill assignments created by an installation before migration 193.
INSERT INTO evaluation_grading_jobs (
    id, run_id, sample_id, assignment_id, grader_id, grader_version, attempt, status
)
SELECT md5(a.id::text || ':grading')::uuid, s.run_id, a.sample_id, a.id,
       c.grader_id, c.grader_version, a.attempt, 'pending'
FROM evaluation_assignments a
JOIN evaluation_samples s ON s.id = a.sample_id
JOIN evaluation_cases c ON c.id = s.case_id
WHERE a.status IN ('pending', 'evidence_uploaded', 'grading')
ON CONFLICT (assignment_id) DO NOTHING;

-- A daily maintenance call can invoke this function to provision the next
-- month before the current partition closes. It is intentionally idempotent.
CREATE OR REPLACE FUNCTION ensure_evaluation_grading_partitions(target_month DATE)
RETURNS VOID AS $$
DECLARE
    start_month DATE := date_trunc('month', target_month)::date;
    end_month DATE := (date_trunc('month', target_month) + INTERVAL '1 month')::date;
    name TEXT;
BEGIN
    name := 'evaluation_scores_' || to_char(start_month, 'YYYYMM');
    EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF evaluation_scores FOR VALUES FROM (%L) TO (%L)', name, start_month, end_month);
    name := 'evaluation_aggregate_snapshots_' || to_char(start_month, 'YYYYMM');
    EXECUTE format('CREATE TABLE IF NOT EXISTS %I PARTITION OF evaluation_aggregate_snapshots FOR VALUES FROM (%L) TO (%L)', name, start_month, end_month);
END;
$$ LANGUAGE plpgsql;
