-- Cron scheduling for Radar evaluation plans.
--
-- trigger_type/cron_expression already exist on evaluation_plans; this adds the
-- scheduling state and the frozen comparison references a scheduled run needs.
-- A scheduled run must never infer its own baseline: without a stored
-- reference the runner skips the plan instead of comparing unknown revisions.

ALTER TABLE evaluation_plans
    ADD COLUMN IF NOT EXISTS next_run_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_run_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS schedule_lease_token UUID,
    ADD COLUMN IF NOT EXISTS schedule_lease_expires_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS baseline_ref JSONB,
    ADD COLUMN IF NOT EXISTS candidate_ref JSONB;

-- A cron plan is claimable only while it is enabled, due, and not leased.
CREATE INDEX IF NOT EXISTS idx_evaluation_plans_schedule_due
    ON evaluation_plans(next_run_at, id)
    WHERE trigger_type = 'cron' AND enabled;

-- Backfill the next firing instant for pre-existing cron plans. Plans created
-- through the governance API are manual, so this is normally a no-op, but it
-- keeps a hand-written cron row usable instead of leaving next_run_at NULL.
UPDATE evaluation_plans
SET next_run_at = NOW()
WHERE trigger_type = 'cron' AND enabled AND next_run_at IS NULL;
