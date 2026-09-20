CREATE TABLE IF NOT EXISTS async_video_billing_tasks (
    id BIGSERIAL PRIMARY KEY,
    provider VARCHAR(32) NOT NULL,
    upstream_task_id VARCHAR(255) NOT NULL,
    task_key VARCHAR(320) NOT NULL,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    group_id BIGINT,
    account_id BIGINT NOT NULL,
    subscription_id BIGINT,
    model VARCHAR(255) NOT NULL,
    billing_model VARCHAR(255) NOT NULL,
    upstream_model VARCHAR(255),
    original_model VARCHAR(255),
    quota_platform VARCHAR(32),
    subscription_billing BOOLEAN NOT NULL DEFAULT FALSE,
    pricing_at TIMESTAMPTZ NOT NULL,
    request_payload_hash CHAR(64),
    inbound_endpoint VARCHAR(255),
    upstream_endpoint VARCHAR(255),
    status VARCHAR(24) NOT NULL DEFAULT 'pending',
    next_poll_at TIMESTAMPTZ NOT NULL,
    poll_deadline_at TIMESTAMPTZ NOT NULL,
    attempt_count INT NOT NULL DEFAULT 0,
    missing_token_checks INT NOT NULL DEFAULT 0,
    lease_token UUID,
    lease_epoch BIGINT NOT NULL DEFAULT 0,
    lease_expires_at TIMESTAMPTZ,
    last_error_code VARCHAR(80),
    last_error_at TIMESTAMPTZ,
    terminal_at TIMESTAMPTZ,
    settled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT async_video_billing_tasks_provider_check CHECK (provider IN ('seedance')),
    CONSTRAINT async_video_billing_tasks_status_check CHECK (status IN ('pending','settled','failed','cancelled','dead_letter')),
    CONSTRAINT async_video_billing_tasks_attempt_count_check CHECK (attempt_count >= 0),
    CONSTRAINT async_video_billing_tasks_missing_token_checks_check CHECK (missing_token_checks >= 0),
    CONSTRAINT async_video_billing_tasks_deadline_check CHECK (poll_deadline_at >= created_at)
);

CREATE UNIQUE INDEX IF NOT EXISTS async_video_billing_tasks_provider_upstream_owner_key
    ON async_video_billing_tasks(provider, upstream_task_id, user_id, api_key_id);

CREATE INDEX IF NOT EXISTS idx_async_video_billing_tasks_due
    ON async_video_billing_tasks(status, next_poll_at, lease_expires_at, id)
    WHERE status = 'pending';
