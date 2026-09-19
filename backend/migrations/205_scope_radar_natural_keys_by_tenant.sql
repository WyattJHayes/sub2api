DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_dataset_versions'::regclass
          AND conname = 'uq_evaluation_dataset_versions_tenant_key_version'
    ) THEN
        ALTER TABLE evaluation_dataset_versions
            ADD CONSTRAINT uq_evaluation_dataset_versions_tenant_key_version
            UNIQUE (tenant_id, dataset_key, version);
    END IF;
END $$;

ALTER TABLE evaluation_dataset_versions
    DROP CONSTRAINT IF EXISTS evaluation_dataset_versions_dataset_key_version_key;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_gate_policies'::regclass
          AND conname = 'uq_evaluation_gate_policies_tenant_version'
    ) THEN
        ALTER TABLE evaluation_gate_policies
            ADD CONSTRAINT uq_evaluation_gate_policies_tenant_version
            UNIQUE (tenant_id, version);
    END IF;
END $$;

ALTER TABLE evaluation_gate_policies
    DROP CONSTRAINT IF EXISTS evaluation_gate_policies_version_key;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_workers'::regclass
          AND conname = 'uq_evaluation_workers_tenant_name'
    ) THEN
        ALTER TABLE evaluation_workers
            ADD CONSTRAINT uq_evaluation_workers_tenant_name
            UNIQUE (tenant_id, name);
    END IF;
END $$;

ALTER TABLE evaluation_workers
    DROP CONSTRAINT IF EXISTS evaluation_workers_name_key;
