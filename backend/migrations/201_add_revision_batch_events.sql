CREATE TABLE IF NOT EXISTS evaluation_revision_batch_events (
    id UUID PRIMARY KEY,
    revision_batch_id UUID NOT NULL,
    run_id UUID NOT NULL,
    event_type VARCHAR(40) NOT NULL CHECK (event_type IN (
        'created', 'fenced', 'resumed', 'cancelled', 'repaired',
        'compensating_head_approved', 'compensating_head_applied'
    )),
    actor_id BIGINT NOT NULL REFERENCES users(id),
    control_epoch BIGINT NOT NULL CHECK (control_epoch > 0),
    idempotency_key CHAR(64) NOT NULL UNIQUE CHECK (idempotency_key ~ '^[0-9a-f]{64}$'),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT transaction_timestamp(),
    UNIQUE (revision_batch_id, event_type, actor_id, idempotency_key),
    CONSTRAINT fk_evaluation_revision_batch_events_batch
        FOREIGN KEY (revision_batch_id, run_id)
        REFERENCES evaluation_revision_batches(id, run_id)
        DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX IF NOT EXISTS idx_evaluation_revision_batch_events_approvals
    ON evaluation_revision_batch_events (revision_batch_id, event_type, actor_id);

CREATE OR REPLACE FUNCTION enforce_evaluation_revision_batch_event_immutability()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'evaluation revision batch events are append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_evaluation_revision_batch_events_immutable
    ON evaluation_revision_batch_events;
CREATE TRIGGER trg_evaluation_revision_batch_events_immutable
    BEFORE UPDATE OR DELETE ON evaluation_revision_batch_events
    FOR EACH ROW EXECUTE FUNCTION enforce_evaluation_revision_batch_event_immutability();

DROP TRIGGER IF EXISTS trg_evaluation_revision_batch_events_writer_protocol
    ON evaluation_revision_batch_events;
CREATE TRIGGER trg_evaluation_revision_batch_events_writer_protocol
    BEFORE INSERT OR UPDATE OR DELETE ON evaluation_revision_batch_events
    FOR EACH ROW EXECUTE FUNCTION audit_evaluation_writer_protocol();
