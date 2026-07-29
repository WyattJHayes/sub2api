-- Keep idempotency records addressable through the immutable score reference.
ALTER TABLE evaluation_score_idempotency
    ADD COLUMN IF NOT EXISTS score_created_at TIMESTAMPTZ;

UPDATE evaluation_score_idempotency record
SET score_created_at = score.created_at
FROM evaluation_scores score
WHERE score.id = record.score_id
  AND record.score_created_at IS NULL;

ALTER TABLE evaluation_score_idempotency
    ALTER COLUMN score_created_at SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_evaluation_score_idempotency_score_ref') THEN
        ALTER TABLE evaluation_score_idempotency
            ADD CONSTRAINT fk_evaluation_score_idempotency_score_ref
            FOREIGN KEY (score_id, score_created_at)
            REFERENCES evaluation_scores(id, created_at)
            DEFERRABLE INITIALLY DEFERRED;
    END IF;
END $$;
