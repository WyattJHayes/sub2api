-- Forward-only hardening for Gate Policy approval independence.
-- Policy creation and approval are separate actions. Existing rows are kept for
-- audit and can no longer satisfy activation when they belong to the creator.

ALTER TABLE evaluation_gate_policy_approvals
    ADD COLUMN IF NOT EXISTS policy_hash CHAR(64),
    ADD COLUMN IF NOT EXISTS evidence_hash CHAR(64);

UPDATE evaluation_gate_policy_approvals a
SET policy_hash = p.policy_hash
FROM evaluation_gate_policies p
WHERE a.policy_id = p.id AND a.policy_hash IS NULL;

UPDATE evaluation_gate_policy_approvals
SET evidence_hash = repeat('0', 64)
WHERE evidence_hash IS NULL;

ALTER TABLE evaluation_gate_policy_approvals
    ALTER COLUMN policy_hash SET NOT NULL,
    ALTER COLUMN evidence_hash SET NOT NULL;

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT c.conname
        FROM pg_constraint c
        WHERE c.conrelid = 'evaluation_gate_policy_approvals'::regclass
          AND c.contype = 'c'
          AND pg_get_constraintdef(c.oid) LIKE '%role%'
    LOOP
        EXECUTE format('ALTER TABLE evaluation_gate_policy_approvals DROP CONSTRAINT %I', constraint_name);
    END LOOP;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_gate_policy_approvals'::regclass
          AND conname = 'evaluation_gate_policy_approvals_role_check_v2'
    ) THEN
        ALTER TABLE evaluation_gate_policy_approvals
            ADD CONSTRAINT evaluation_gate_policy_approvals_role_check_v2
            CHECK (role IN ('quality_admin', 'release_manager'));
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_gate_policy_approvals'::regclass
          AND conname = 'evaluation_gate_policy_approvals_policy_hash_check'
    ) THEN
        ALTER TABLE evaluation_gate_policy_approvals
            ADD CONSTRAINT evaluation_gate_policy_approvals_policy_hash_check
            CHECK (policy_hash ~ '^[0-9a-f]{64}$');
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_gate_policy_approvals'::regclass
          AND conname = 'evaluation_gate_policy_approvals_evidence_hash_check'
    ) THEN
        ALTER TABLE evaluation_gate_policy_approvals
            ADD CONSTRAINT evaluation_gate_policy_approvals_evidence_hash_check
            CHECK (evidence_hash ~ '^[0-9a-f]{64}$');
    END IF;
END $$;

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    FOR constraint_name IN
        SELECT c.conname
        FROM pg_constraint c
        WHERE c.conrelid = 'evaluation_gate_policy_events'::regclass
          AND c.contype = 'c'
          AND pg_get_constraintdef(c.oid) LIKE '%event_type%'
    LOOP
        EXECUTE format('ALTER TABLE evaluation_gate_policy_events DROP CONSTRAINT %I', constraint_name);
    END LOOP;
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'evaluation_gate_policy_events'::regclass
          AND conname = 'evaluation_gate_policy_events_event_type_check_v2'
    ) THEN
        ALTER TABLE evaluation_gate_policy_events
            ADD CONSTRAINT evaluation_gate_policy_events_event_type_check_v2
            CHECK (event_type IN ('created', 'approved', 'activated', 'retired'));
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_evaluation_gate_policy_approvals_current
    ON evaluation_gate_policy_approvals (policy_id, role, effective_at, expires_at, approver_id);
