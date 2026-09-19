CREATE TABLE IF NOT EXISTS evaluation_key_events (
    id UUID PRIMARY KEY,
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id),
    action VARCHAR(24) NOT NULL CHECK (action IN ('enabled', 'disabled')),
    actor_id BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_evaluation_key_events_key_created
    ON evaluation_key_events (api_key_id, created_at DESC);
