package repository

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type evaluationOutboxRepository struct{ db *sql.DB }

func NewEvaluationOutboxRepository(db *sql.DB) service.EvaluationOutboxRepository {
	return &evaluationOutboxRepository{db: db}
}

func (r *evaluationOutboxRepository) Enqueue(ctx context.Context, input service.EnqueueEvaluationOutboxInput) (*service.EvaluationOutboxEvent, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin evaluation outbox enqueue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	event, err := enqueueEvaluationOutbox(ctx, tx, input)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evaluation outbox enqueue: %w", err)
	}
	return event, nil
}

func enqueueEvaluationOutbox(ctx context.Context, tx *sql.Tx, input service.EnqueueEvaluationOutboxInput) (*service.EvaluationOutboxEvent, error) {
	input.EventType = strings.TrimSpace(input.EventType)
	input.ScopeKey = strings.TrimSpace(input.ScopeKey)
	input.AnalysisVersion = strings.TrimSpace(input.AnalysisVersion)
	input.SourceType = strings.TrimSpace(input.SourceType)
	input.SourceID = strings.TrimSpace(input.SourceID)
	input.WorkOrigin = strings.TrimSpace(input.WorkOrigin)
	if input.WorkOrigin == "" {
		input.WorkOrigin = "initial"
	}
	if input.EventType == "" || input.RunID == uuid.Nil || input.ScopeKey == "" ||
		input.AnalysisVersion == "" || input.SourceType == "" || input.SourceID == "" ||
		len(input.Payload) == 0 || !json.Valid(input.Payload) {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	if input.WorkOrigin != "initial" && input.WorkOrigin != "regrade" {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	if (input.WorkOrigin == "regrade") != (input.RevisionBatchID != uuid.Nil) {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	if _, err := service.OutboxDedupKey(input.EventType, input.RunID, input.ScopeKey, input.AnalysisVersion, input.SourceHash); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_runs WHERE id=$1`, input.RunID).Scan(new(uuid.UUID)); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrEvaluationOutboxInvalid
		}
		return nil, fmt.Errorf("lock evaluation outbox run: %w", err)
	}
	var leaseEpoch int64
	var batchValue any
	if input.RevisionBatchID != uuid.Nil {
		var status service.RevisionBatchStatus
		if err := tx.QueryRowContext(ctx, `
			SELECT status, control_epoch FROM evaluation_revision_batches
			WHERE id=$1 AND run_id=$2 FOR UPDATE`, input.RevisionBatchID, input.RunID).Scan(
			&status, &leaseEpoch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, service.ErrEvaluationOutboxBatchMismatch
			}
			return nil, fmt.Errorf("lock evaluation outbox batch: %w", err)
		}
		if status != service.RevisionBatchRunning {
			return nil, service.ErrEvaluationOutboxFenced
		}
		batchValue = input.RevisionBatchID
	}
	payloadHash, err := service.DigestCanonicalJSON(input.Payload)
	if err != nil {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	dedupKey, err := service.OutboxDedupKey(
		input.EventType, input.RunID, input.ScopeKey, input.AnalysisVersion, input.SourceHash,
	)
	if err != nil {
		return nil, err
	}
	causes, err := normalizeEvaluationOutboxCauses(input.Causes)
	if err != nil {
		return nil, err
	}
	var causeSetHash string
	if len(causes) == 0 {
		causeSetHash = hashString(strings.Join([]string{
			"evaluation-outbox-root", input.RunID.String(), input.SourceType, input.SourceID, input.SourceHash,
		}, "\x00"))
	} else {
		causeIDs := make([]uuid.UUID, 0, len(causes))
		for _, cause := range causes {
			var causeRunID uuid.UUID
			if err := tx.QueryRowContext(ctx, `SELECT run_id FROM evaluation_outbox_events WHERE id=$1`, cause.EventID).Scan(&causeRunID); err != nil {
				return nil, service.ErrEvaluationOutboxInvalid
			}
			if causeRunID != input.RunID {
				return nil, service.ErrEvaluationOutboxInvalid
			}
			causeIDs = append(causeIDs, cause.EventID)
		}
		causeSetHash, err = service.CauseSetHash(causeIDs)
		if err != nil {
			return nil, err
		}
	}
	eventID := uuid.New()
	var insertedID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_outbox_events (
			id, event_type, dedup_key, causation_id, cause_set_hash, work_origin,
			revision_batch_id, run_id, source_type, source_id, source_hash,
			payload_hash, payload, lease_epoch
		) VALUES ($1,$2,$3,$4,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13)
		ON CONFLICT (dedup_key) DO NOTHING RETURNING id`,
		eventID, input.EventType, dedupKey, causeSetHash, input.WorkOrigin, batchValue,
		input.RunID, input.SourceType, input.SourceID, input.SourceHash, payloadHash,
		string(input.Payload), leaseEpoch).Scan(&insertedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("insert evaluation outbox event: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		existing, loadErr := loadEvaluationOutboxEventByDedup(ctx, tx, dedupKey)
		if loadErr != nil {
			return nil, loadErr
		}
		if existing.EventType != input.EventType || existing.RunID != input.RunID ||
			existing.SourceType != input.SourceType || existing.SourceID != input.SourceID ||
			existing.SourceHash != input.SourceHash || existing.PayloadHash != payloadHash ||
			existing.CauseSetHash != causeSetHash || existing.WorkOrigin != input.WorkOrigin ||
			existing.RevisionBatchID != input.RevisionBatchID {
			return nil, service.ErrEvaluationOutboxDedupConflict
		}
		if err := validateEvaluationOutboxCauseIdentity(ctx, tx, existing.ID, causes); err != nil {
			return nil, err
		}
		return existing, nil
	}
	for _, cause := range causes {
		var sourceHeadValue any
		if cause.SourceHeadEventID != uuid.Nil {
			sourceHeadValue = cause.SourceHeadEventID
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_outbox_event_causes (
				event_id, cause_event_id, run_id, revision_batch_id, source_head_event_id
			) VALUES ($1,$2,$3,$4,$5)`, insertedID, cause.EventID, input.RunID,
			batchValue, sourceHeadValue); err != nil {
			return nil, fmt.Errorf("insert evaluation outbox cause: %w", err)
		}
	}
	event, err := loadEvaluationOutboxEvent(ctx, tx, insertedID)
	if err != nil {
		return nil, err
	}
	if err := advanceRevisionRequirementsForOutbox(ctx, tx, event, causes); err != nil {
		return nil, err
	}
	return event, nil
}

func normalizeEvaluationOutboxCauses(input []service.EvaluationOutboxCause) ([]service.EvaluationOutboxCause, error) {
	byID := make(map[uuid.UUID]service.EvaluationOutboxCause, len(input))
	for _, cause := range input {
		if cause.EventID == uuid.Nil {
			return nil, service.ErrEvaluationOutboxInvalid
		}
		if existing, ok := byID[cause.EventID]; ok && existing.SourceHeadEventID != cause.SourceHeadEventID {
			return nil, service.ErrEvaluationOutboxInvalid
		}
		byID[cause.EventID] = cause
	}
	causes := make([]service.EvaluationOutboxCause, 0, len(byID))
	for _, cause := range byID {
		causes = append(causes, cause)
	}
	sort.Slice(causes, func(i, j int) bool {
		return bytes.Compare(causes[i].EventID[:], causes[j].EventID[:]) < 0
	})
	return causes, nil
}

func validateEvaluationOutboxCauseIdentity(ctx context.Context, tx *sql.Tx, eventID uuid.UUID, expected []service.EvaluationOutboxCause) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT cause_event_id, source_head_event_id
		FROM evaluation_outbox_event_causes WHERE event_id=$1`, eventID)
	if err != nil {
		return fmt.Errorf("load evaluation outbox cause identity: %w", err)
	}
	defer rows.Close()
	actual := make(map[uuid.UUID]uuid.UUID, len(expected))
	for rows.Next() {
		var causeID uuid.UUID
		var sourceHeadID uuid.NullUUID
		if err := rows.Scan(&causeID, &sourceHeadID); err != nil {
			return fmt.Errorf("scan evaluation outbox cause identity: %w", err)
		}
		if sourceHeadID.Valid {
			actual[causeID] = sourceHeadID.UUID
		} else {
			actual[causeID] = uuid.Nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate evaluation outbox cause identity: %w", err)
	}
	if len(actual) != len(expected) {
		return service.ErrEvaluationOutboxDedupConflict
	}
	for _, cause := range expected {
		actualSourceHeadID, exists := actual[cause.EventID]
		if !exists || actualSourceHeadID != cause.SourceHeadEventID {
			return service.ErrEvaluationOutboxDedupConflict
		}
	}
	return nil
}

func (r *evaluationOutboxRepository) Claim(ctx context.Context, workerID uuid.UUID, eventTypes []string, limit int, leaseDuration time.Duration) ([]service.EvaluationOutboxEvent, error) {
	if r == nil || r.db == nil || workerID == uuid.Nil || len(eventTypes) == 0 || limit <= 0 || limit > 100 || leaseDuration <= 0 {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin evaluation outbox claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `
		SELECT event.id, event.run_id, event.revision_batch_id
		FROM evaluation_outbox_events event
		WHERE event.event_type=ANY($1::text[]) AND event.available_at <= transaction_timestamp()
		  AND (event.status='pending' OR (event.status='leased' AND event.lease_expires_at <= transaction_timestamp()))
		ORDER BY event.available_at, event.sequence
		LIMIT $2`, pq.Array(eventTypes), limit)
	if err != nil {
		return nil, fmt.Errorf("select evaluation outbox claims: %w", err)
	}
	type candidate struct {
		id      uuid.UUID
		runID   uuid.UUID
		batchID uuid.NullUUID
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.runID, &item.batchID); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan evaluation outbox claim: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close evaluation outbox claims: %w", err)
	}
	type batchLease struct {
		status service.RevisionBatchStatus
		epoch  int64
	}
	batchRuns := make(map[uuid.UUID]uuid.UUID)
	batchIDs := make([]uuid.UUID, 0)
	for _, candidate := range candidates {
		if candidate.batchID.Valid {
			if _, exists := batchRuns[candidate.batchID.UUID]; !exists {
				batchRuns[candidate.batchID.UUID] = candidate.runID
				batchIDs = append(batchIDs, candidate.batchID.UUID)
			}
		}
	}
	sort.Slice(batchIDs, func(i, j int) bool { return bytes.Compare(batchIDs[i][:], batchIDs[j][:]) < 0 })
	batches := make(map[uuid.UUID]batchLease, len(batchIDs))
	for _, batchID := range batchIDs {
		var lease batchLease
		if err := tx.QueryRowContext(ctx, `
			SELECT status, control_epoch FROM evaluation_revision_batches
			WHERE id=$1 AND run_id=$2 FOR UPDATE`, batchID, batchRuns[batchID]).Scan(&lease.status, &lease.epoch); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("lock evaluation outbox batch claim: %w", err)
		}
		batches[batchID] = lease
	}
	claimed := make([]service.EvaluationOutboxEvent, 0, len(candidates))
	for _, candidate := range candidates {
		leaseEpoch := int64(0)
		if candidate.batchID.Valid {
			batch, exists := batches[candidate.batchID.UUID]
			if !exists || batch.status != service.RevisionBatchRunning {
				continue
			}
			leaseEpoch = batch.epoch
		}
		var lockedID uuid.UUID
		lockQuery := `
			SELECT id FROM evaluation_outbox_events
			WHERE id=$1 AND run_id=$2 AND event_type=ANY($3::text[])
			  AND available_at <= transaction_timestamp()
			  AND (status='pending' OR (status='leased' AND lease_expires_at <= transaction_timestamp()))`
		args := []any{candidate.id, candidate.runID, pq.Array(eventTypes)}
		if candidate.batchID.Valid {
			lockQuery += ` AND revision_batch_id=$4`
			args = append(args, candidate.batchID.UUID)
		} else {
			lockQuery += ` AND revision_batch_id IS NULL`
		}
		lockQuery += ` FOR UPDATE SKIP LOCKED`
		if err := tx.QueryRowContext(ctx, lockQuery, args...).Scan(&lockedID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("lock evaluation outbox claim: %w", err)
		}
		token, tokenHash, err := newLeaseToken()
		if err != nil {
			return nil, fmt.Errorf("create evaluation outbox lease token: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_outbox_events
			SET status='leased', attempt=attempt+1, lease_token_hash=$2, lease_owner=$3,
				lease_expires_at=transaction_timestamp()+($4 * INTERVAL '1 millisecond'),
				lease_epoch=$5, updated_at=transaction_timestamp()
			WHERE id=$1`, lockedID, tokenHash, workerID, leaseDuration.Milliseconds(), leaseEpoch); err != nil {
			return nil, fmt.Errorf("claim evaluation outbox event: %w", err)
		}
		event, err := loadEvaluationOutboxEvent(ctx, tx, lockedID)
		if err != nil {
			return nil, err
		}
		event.LeaseToken = token
		claimed = append(claimed, *event)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evaluation outbox claim: %w", err)
	}
	return claimed, nil
}

func (r *evaluationOutboxRepository) Heartbeat(ctx context.Context, eventID uuid.UUID, leaseToken string, leaseEpoch int64, extension time.Duration) error {
	if extension <= 0 {
		return service.ErrEvaluationOutboxInvalid
	}
	return r.updateLeased(ctx, eventID, leaseToken, leaseEpoch, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE evaluation_outbox_events
			SET lease_expires_at=transaction_timestamp()+($2 * INTERVAL '1 millisecond'),
				updated_at=transaction_timestamp() WHERE id=$1`, eventID, extension.Milliseconds())
		return err
	})
}

func (r *evaluationOutboxRepository) Complete(ctx context.Context, eventID uuid.UUID, leaseToken string, leaseEpoch int64) error {
	return r.updateLeased(ctx, eventID, leaseToken, leaseEpoch, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_outbox_events
			SET status='completed', lease_token_hash=NULL, lease_owner=NULL,
				lease_expires_at=NULL, last_error_code=NULL, updated_at=transaction_timestamp()
			WHERE id=$1`, eventID); err != nil {
			return err
		}
		return completeRevisionRequirementForEvent(ctx, tx, eventID)
	})
}

func (r *evaluationOutboxRepository) DeadLetter(ctx context.Context, eventID uuid.UUID, leaseToken string, leaseEpoch int64, errorCode string) error {
	errorCode = strings.TrimSpace(errorCode)
	if errorCode == "" || len(errorCode) > 100 {
		return service.ErrEvaluationOutboxInvalid
	}
	return r.updateLeased(ctx, eventID, leaseToken, leaseEpoch, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_outbox_events
			SET status='dead_letter', lease_token_hash=NULL, lease_owner=NULL,
				lease_expires_at=NULL, last_error_code=$2, updated_at=transaction_timestamp()
			WHERE id=$1`, eventID, errorCode); err != nil {
			return err
		}
		return failRevisionRequirementForEvent(ctx, tx, eventID, errorCode)
	})
}

func (r *evaluationOutboxRepository) updateLeased(ctx context.Context, eventID uuid.UUID, leaseToken string, leaseEpoch int64, update func(context.Context, *sql.Tx) error) error {
	if r == nil || r.db == nil || eventID == uuid.Nil || strings.TrimSpace(leaseToken) == "" || leaseEpoch < 0 {
		return service.ErrEvaluationOutboxInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return fmt.Errorf("begin evaluation outbox lease update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var storedHash string
	var storedEpoch int64
	var batchID uuid.NullUUID
	var leaseCurrent bool
	err = tx.QueryRowContext(ctx, `
		SELECT lease_token_hash, COALESCE(lease_epoch,0), revision_batch_id,
		       status='leased' AND lease_expires_at > transaction_timestamp()
		FROM evaluation_outbox_events WHERE id=$1 FOR UPDATE`, eventID).Scan(
		&storedHash, &storedEpoch, &batchID, &leaseCurrent)
	if err != nil || !leaseCurrent || storedHash != hashToken(leaseToken) || storedEpoch != leaseEpoch {
		return service.ErrEvaluationOutboxFenced
	}
	if batchID.Valid {
		var batchStatus service.RevisionBatchStatus
		var batchEpoch int64
		if err := tx.QueryRowContext(ctx, `
			SELECT status, control_epoch FROM evaluation_revision_batches
			WHERE id=$1 FOR UPDATE`, batchID.UUID).Scan(&batchStatus, &batchEpoch); err != nil ||
			batchStatus != service.RevisionBatchRunning || batchEpoch != leaseEpoch {
			return service.ErrEvaluationOutboxFenced
		}
	}
	if err := update(ctx, tx); err != nil {
		return fmt.Errorf("update evaluation outbox lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit evaluation outbox lease update: %w", err)
	}
	return nil
}

func (r *evaluationOutboxRepository) ReplayDeadLetter(ctx context.Context, eventID uuid.UUID) (*service.EvaluationOutboxEvent, error) {
	if r == nil || r.db == nil || eventID == uuid.Nil {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin evaluation outbox replay: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var status service.EvaluationOutboxStatus
	var batchID uuid.NullUUID
	if err := tx.QueryRowContext(ctx, `
		SELECT status, revision_batch_id
		FROM evaluation_outbox_events WHERE id=$1 FOR UPDATE`, eventID).Scan(&status, &batchID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrEvaluationOutboxNotFound
		}
		return nil, fmt.Errorf("lock evaluation outbox replay: %w", err)
	}
	if status != service.EvaluationOutboxDeadLetter {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	if batchID.Valid {
		return nil, service.ErrRevisionBatchNotRepairable
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_outbox_events
		SET status='pending', available_at=transaction_timestamp(), lease_token_hash=NULL,
			lease_owner=NULL, lease_expires_at=NULL, last_error_code=NULL,
			updated_at=transaction_timestamp() WHERE id=$1`, eventID); err != nil {
		return nil, fmt.Errorf("replay evaluation outbox event: %w", err)
	}
	event, err := loadEvaluationOutboxEvent(ctx, tx, eventID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evaluation outbox replay: %w", err)
	}
	return event, nil
}

func loadEvaluationOutboxEventByDedup(ctx context.Context, tx *sql.Tx, dedupKey string) (*service.EvaluationOutboxEvent, error) {
	var id uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_outbox_events WHERE dedup_key=$1`, dedupKey).Scan(&id); err != nil {
		return nil, fmt.Errorf("load evaluation outbox dedup event: %w", err)
	}
	return loadEvaluationOutboxEvent(ctx, tx, id)
}

func loadEvaluationOutboxEvent(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, eventID uuid.UUID) (*service.EvaluationOutboxEvent, error) {
	var event service.EvaluationOutboxEvent
	var workOrigin, lastError sql.NullString
	var batchID, leaseOwner uuid.NullUUID
	var leaseExpires sql.NullTime
	var leaseEpoch sql.NullInt64
	if err := q.QueryRowContext(ctx, `
		SELECT id, sequence, event_type, dedup_key, causation_id, cause_set_hash,
		       work_origin, revision_batch_id, run_id, source_type, source_id, source_hash,
		       payload_hash, payload, status, attempt, available_at, lease_owner,
		       lease_expires_at, lease_epoch, last_error_code, created_at, updated_at
		FROM evaluation_outbox_events WHERE id=$1`, eventID).Scan(
		&event.ID, &event.Sequence, &event.EventType, &event.DedupKey, &event.CausationID,
		&event.CauseSetHash, &workOrigin, &batchID, &event.RunID, &event.SourceType,
		&event.SourceID, &event.SourceHash, &event.PayloadHash, &event.Payload, &event.Status,
		&event.Attempt, &event.AvailableAt, &leaseOwner, &leaseExpires, &leaseEpoch,
		&lastError, &event.CreatedAt, &event.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrEvaluationOutboxNotFound
		}
		return nil, fmt.Errorf("load evaluation outbox event: %w", err)
	}
	event.WorkOrigin = workOrigin.String
	if batchID.Valid {
		event.RevisionBatchID = batchID.UUID
	}
	if leaseOwner.Valid {
		event.LeaseOwner = leaseOwner.UUID
	}
	if leaseExpires.Valid {
		event.LeaseExpiresAt = leaseExpires.Time
	}
	if leaseEpoch.Valid {
		event.LeaseEpoch = leaseEpoch.Int64
	}
	event.LastErrorCode = lastError.String
	return &event, nil
}
