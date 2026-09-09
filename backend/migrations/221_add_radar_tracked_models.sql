CREATE TABLE IF NOT EXISTS evaluation_tracked_models (
    tenant_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    model_alias TEXT NOT NULL,
    created_by BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT evaluation_tracked_models_pkey PRIMARY KEY (tenant_id, model_alias),
    CONSTRAINT evaluation_tracked_models_alias_check
        CHECK (model_alias ~ '^[A-Za-z0-9._:/-]{1,128}$')
);

CREATE INDEX IF NOT EXISTS idx_evaluation_tracked_models_tenant_created_at
    ON evaluation_tracked_models (tenant_id, created_at DESC);
