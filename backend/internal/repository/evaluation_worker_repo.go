package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

const workerTokenFingerprintLength = 12

type workerEventRecord struct {
	ID          uuid.UUID
	WorkerID    uuid.UUID
	EventType   string
	RequestHash string
	Previous    string
	Next        string
}

type workerMutation struct {
	eventType         string
	previousClaimMode string
	claimMode         string
	status            string
	activeLeaseCount  int
	payload           map[string]any
}

func (r *radarGovernanceRepository) RegisterWorker(ctx context.Context, input service.RadarWorkerRegistrationInput) (*service.RadarWorkerRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if err := validateWorkerRegistration(input); err != nil {
		return nil, err
	}
	tokenHash := hashToken(input.Token)
	requestHash := workerRequestHash("register", map[string]any{
		"name": input.Name, "worker_kind": input.WorkerKind, "region": input.Region,
		"image_digest": input.ImageDigest, "capabilities": normalizeWorkerCapabilities(input.Capabilities),
		"max_concurrency": input.MaxConcurrency, "token_hash": tokenHash,
	})
	var record *service.RadarWorkerRecord
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		if existing, err := loadWorkerEventByKey(ctx, tx, input.IdempotencyKey); err != nil {
			return err
		} else if existing != nil {
			if existing.EventType != "registered" || existing.RequestHash != requestHash {
				return service.ErrRadarWorkerIdempotencyConflict
			}
			record, err = loadWorkerRecordByID(ctx, tx, existing.WorkerID)
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, input.Name); err != nil {
			return fmt.Errorf("lock evaluation worker identity: %w", err)
		}

		var existingID uuid.UUID
		var existingTokenHash, existingKind, existingRegion, existingImage string
		var existingCapabilities pq.StringArray
		var existingMaxConcurrency int
		err := tx.QueryRowContext(ctx, `
			SELECT id, token_hash, worker_kind, region, image_digest, capabilities, max_concurrency
			FROM evaluation_workers WHERE name = $1 FOR UPDATE`, input.Name).
			Scan(&existingID, &existingTokenHash, &existingKind, &existingRegion, &existingImage, &existingCapabilities, &existingMaxConcurrency)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("load evaluation worker identity: %w", err)
		}
		if err == nil {
			if existingTokenHash != tokenHash || existingKind != input.WorkerKind || existingRegion != input.Region || existingImage != input.ImageDigest ||
				!equalWorkerCapabilities([]string(existingCapabilities), input.Capabilities) || existingMaxConcurrency != input.MaxConcurrency {
				return service.ErrRadarWorkerConflict
			}
			record, err = loadWorkerRecordByID(ctx, tx, existingID)
			if err != nil {
				return err
			}
		} else {
			var tokenOwner uuid.UUID
			err = tx.QueryRowContext(ctx, `SELECT id FROM evaluation_workers WHERE token_hash = $1 FOR UPDATE`, tokenHash).Scan(&tokenOwner)
			if err == nil {
				return service.ErrRadarWorkerConflict
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("check evaluation worker token ownership: %w", err)
			}
			workerID := uuid.New()
			capabilities := normalizeWorkerCapabilities(input.Capabilities)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO evaluation_workers (
					id, name, worker_kind, token_hash, status, capabilities, max_concurrency,
					region, image_digest, claim_mode
				) VALUES ($1, $2, $3, $4, 'active', $5, $6, $7, $8, 'open')`,
				workerID, input.Name, input.WorkerKind, tokenHash, pq.Array(capabilities), input.MaxConcurrency,
				input.Region, input.ImageDigest); err != nil {
				return fmt.Errorf("insert evaluation worker: %w", err)
			}
			existingID = workerID
		}

		eventID, err := insertWorkerEvent(ctx, tx, existingID, "registered", input.IdempotencyKey, requestHash, map[string]any{
			"actor_id": input.ActorID, "request_hash": requestHash, "token_fingerprint": workerTokenFingerprint(tokenHash),
		})
		if err != nil {
			return err
		}
		_ = eventID
		record, err = loadWorkerRecordByID(ctx, tx, existingID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (r *radarGovernanceRepository) RotateWorkerToken(ctx context.Context, input service.RadarWorkerTokenRotationInput) (*service.RadarWorkerRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.WorkerID == uuid.Nil || input.ActorID <= 0 || !validWorkerIdempotencyKey(input.IdempotencyKey) || strings.TrimSpace(input.Token) == "" {
		return nil, errors.New("worker token rotation requires worker, token, actor and idempotency key")
	}
	tokenHash := hashToken(input.Token)
	requestHash := workerRequestHash("token_rotated", map[string]any{"worker_id": input.WorkerID, "token_hash": tokenHash})
	var record *service.RadarWorkerRecord
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		if existing, err := loadWorkerEventByKey(ctx, tx, input.IdempotencyKey); err != nil {
			return err
		} else if existing != nil {
			if existing.EventType != "token_rotated" || existing.RequestHash != requestHash {
				return service.ErrRadarWorkerIdempotencyConflict
			}
			record, err = loadWorkerRecordByID(ctx, tx, existing.WorkerID)
			return err
		}
		current, err := loadWorkerRecordByID(ctx, tx, input.WorkerID)
		if err != nil {
			return err
		}
		if current.Status != "active" {
			return service.ErrRadarWorkerStateConflict
		}
		var tokenOwner uuid.UUID
		err = tx.QueryRowContext(ctx, `SELECT id FROM evaluation_workers WHERE token_hash = $1 AND id <> $2 FOR UPDATE`, tokenHash, input.WorkerID).Scan(&tokenOwner)
		if err == nil {
			return service.ErrRadarWorkerConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check rotated worker token ownership: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_workers SET token_hash = $2, updated_at = NOW() WHERE id = $1`, input.WorkerID, tokenHash); err != nil {
			return fmt.Errorf("rotate evaluation worker token: %w", err)
		}
		if _, err := insertWorkerEvent(ctx, tx, input.WorkerID, "token_rotated", input.IdempotencyKey, requestHash, map[string]any{
			"actor_id": input.ActorID, "request_hash": requestHash, "token_fingerprint": workerTokenFingerprint(tokenHash),
		}); err != nil {
			return err
		}
		record, err = loadWorkerRecordByID(ctx, tx, input.WorkerID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (r *radarGovernanceRepository) PauseWorkerClaims(ctx context.Context, input service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error) {
	return r.mutateWorker(ctx, input, "claims_paused", func(current *service.RadarWorkerRecord) (workerMutation, error) {
		if current.Status != "active" || current.ClaimMode != "open" {
			return workerMutation{}, service.ErrRadarWorkerStateConflict
		}
		return workerMutation{claimMode: "paused"}, nil
	})
}

func (r *radarGovernanceRepository) ResumeWorkerClaims(ctx context.Context, input service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error) {
	return r.mutateWorker(ctx, input, "claims_resumed", func(current *service.RadarWorkerRecord) (workerMutation, error) {
		if current.Status != "active" || current.ClaimMode != "paused" {
			return workerMutation{}, service.ErrRadarWorkerStateConflict
		}
		return workerMutation{claimMode: "open"}, nil
	})
}

func (r *radarGovernanceRepository) DrainWorker(ctx context.Context, input service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error) {
	return r.mutateWorker(ctx, input, "draining", func(current *service.RadarWorkerRecord) (workerMutation, error) {
		if current.Status != "active" || (current.ClaimMode != "open" && current.ClaimMode != "paused" && current.ClaimMode != "draining") {
			return workerMutation{}, service.ErrRadarWorkerStateConflict
		}
		return workerMutation{claimMode: "draining", payload: map[string]any{"already_draining": current.ClaimMode == "draining"}}, nil
	})
}

func (r *radarGovernanceRepository) DisableWorker(ctx context.Context, input service.RadarWorkerActionInput) (*service.RadarWorkerActionResult, error) {
	return r.mutateWorker(ctx, input, "disabled", func(current *service.RadarWorkerRecord) (workerMutation, error) {
		if current.Status != "active" {
			return workerMutation{}, service.ErrRadarWorkerStateConflict
		}
		return workerMutation{status: "disabled", claimMode: "draining"}, nil
	})
}

func (r *radarGovernanceRepository) mutateWorker(
	ctx context.Context,
	input service.RadarWorkerActionInput,
	eventType string,
	mutate func(*service.RadarWorkerRecord) (workerMutation, error),
) (*service.RadarWorkerActionResult, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.WorkerID == uuid.Nil || input.ActorID <= 0 || !validWorkerIdempotencyKey(input.IdempotencyKey) {
		return nil, errors.New("worker action requires worker, actor and idempotency key")
	}
	if !validWorkerReason(input.Reason) {
		return nil, errors.New("worker action reason is invalid")
	}
	requestHash := workerRequestHash(eventType, map[string]any{"worker_id": input.WorkerID, "reason": strings.TrimSpace(input.Reason)})
	result := &service.RadarWorkerActionResult{}
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		if existing, err := loadWorkerEventByKey(ctx, tx, input.IdempotencyKey); err != nil {
			return err
		} else if existing != nil {
			if existing.EventType != eventType || existing.RequestHash != requestHash {
				return service.ErrRadarWorkerIdempotencyConflict
			}
			worker, err := loadWorkerRecordByID(ctx, tx, existing.WorkerID)
			if err != nil {
				return err
			}
			result.Worker, result.EventID, result.PreviousClaimMode, result.ClaimMode = worker, existing.ID, existing.Previous, existing.Next
			result.Idempotent = true
			return nil
		}
		current, err := loadWorkerRecordByID(ctx, tx, input.WorkerID)
		if err != nil {
			return err
		}
		mutation, err := mutate(current)
		if err != nil {
			return err
		}
		mutation.eventType = eventType
		mutation.previousClaimMode = current.ClaimMode
		if mutation.claimMode == "" {
			mutation.claimMode = current.ClaimMode
		}
		if mutation.status == "" {
			mutation.status = current.Status
		}
		if mutation.eventType == "draining" {
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM evaluation_assignments
				WHERE leased_by = $1 AND status IN ('leased', 'running') AND lease_expires_at > NOW()`, input.WorkerID).
				Scan(&mutation.activeLeaseCount); err != nil {
				return fmt.Errorf("count active worker leases: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_workers
			SET status = $2, claim_mode = $3, updated_at = NOW()
			WHERE id = $1`, input.WorkerID, mutation.status, mutation.claimMode); err != nil {
			return fmt.Errorf("update evaluation worker state: %w", err)
		}
		payload := map[string]any{
			"actor_id": input.ActorID, "reason": strings.TrimSpace(input.Reason), "request_hash": requestHash,
			"previous_claim_mode": mutation.previousClaimMode, "claim_mode": mutation.claimMode,
			"active_lease_count": mutation.activeLeaseCount,
		}
		for key, value := range mutation.payload {
			payload[key] = value
		}
		eventID, err := insertWorkerEvent(ctx, tx, input.WorkerID, eventType, input.IdempotencyKey, requestHash, payload)
		if err != nil {
			return err
		}
		result.Worker, result.EventID = nil, eventID
		result.PreviousClaimMode, result.ClaimMode = mutation.previousClaimMode, mutation.claimMode
		result.ActiveLeaseCount = mutation.activeLeaseCount
		result.Worker, err = loadWorkerRecordByID(ctx, tx, input.WorkerID)
		if err != nil {
			return err
		}
		if eventType == "draining" && mutation.activeLeaseCount == 0 {
			completedKey := derivedWorkerIdempotencyKey(input.IdempotencyKey, "drain_completed")
			completedID, err := insertWorkerEvent(ctx, tx, input.WorkerID, "drain_completed", completedKey, requestHash, map[string]any{
				"actor_id": input.ActorID, "request_hash": requestHash, "active_lease_count": 0,
			})
			if err != nil {
				return err
			}
			_ = completedID
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func loadWorkerRecordByID(ctx context.Context, tx *sql.Tx, workerID uuid.UUID) (*service.RadarWorkerRecord, error) {
	var record service.RadarWorkerRecord
	var capabilities pq.StringArray
	var tokenHash string
	err := tx.QueryRowContext(ctx, `
		SELECT id, name, worker_kind, region, image_digest, status, claim_mode,
		       capabilities, max_concurrency, last_heartbeat_at, token_hash
		FROM evaluation_workers WHERE id = $1 FOR UPDATE`, workerID).Scan(
		&record.ID, &record.Name, &record.WorkerKind, &record.Region, &record.ImageDigest,
		&record.Status, &record.ClaimMode, &capabilities, &record.MaxConcurrency,
		&record.LastHeartbeatAt, &tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("load evaluation worker: %w", err)
	}
	record.Capabilities = append([]string(nil), capabilities...)
	record.TokenFingerprint = workerTokenFingerprint(tokenHash)
	return &record, nil
}

func loadWorkerEventByKey(ctx context.Context, tx *sql.Tx, key string) (*workerEventRecord, error) {
	var event workerEventRecord
	var payload struct {
		RequestHash string `json:"request_hash"`
		Previous    string `json:"previous_claim_mode"`
		Next        string `json:"claim_mode"`
	}
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT id, worker_id, event_type, payload FROM evaluation_worker_events WHERE idempotency_key = $1 FOR UPDATE`, key).
		Scan(&event.ID, &event.WorkerID, &event.EventType, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load evaluation worker event: %w", err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode evaluation worker event: %w", err)
	}
	event.RequestHash, event.Previous, event.Next = payload.RequestHash, payload.Previous, payload.Next
	return &event, nil
}

func insertWorkerEvent(ctx context.Context, tx *sql.Tx, workerID uuid.UUID, eventType, idempotencyKey, requestHash string, payload map[string]any) (uuid.UUID, error) {
	payload["request_hash"] = requestHash
	body, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal evaluation worker event: %w", err)
	}
	eventID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_worker_events (id, worker_id, event_type, idempotency_key, payload)
		VALUES ($1, $2, $3, $4, $5::jsonb)`, eventID, workerID, eventType, idempotencyKey, string(body)); err != nil {
		return uuid.Nil, fmt.Errorf("record evaluation worker event: %w", err)
	}
	return eventID, nil
}

func validateWorkerRegistration(input service.RadarWorkerRegistrationInput) error {
	if input.ActorID <= 0 || !validWorkerIdempotencyKey(input.IdempotencyKey) {
		return errors.New("worker registration requires actor and idempotency key")
	}
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 120 || strings.TrimSpace(input.Region) == "" || len(input.Region) > 64 || strings.TrimSpace(input.ImageDigest) == "" || len(input.ImageDigest) > 200 {
		return errors.New("worker identity fields are invalid")
	}
	switch input.WorkerKind {
	case "runner", "grader", "statistics":
	default:
		return errors.New("worker kind is invalid")
	}
	if input.MaxConcurrency < 1 || input.MaxConcurrency > 1000 || strings.TrimSpace(input.Token) == "" {
		return errors.New("worker token and concurrency are required")
	}
	return nil
}

func normalizeWorkerCapabilities(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func equalWorkerCapabilities(left, right []string) bool {
	left, right = normalizeWorkerCapabilities(left), normalizeWorkerCapabilities(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validWorkerIdempotencyKey(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validWorkerReason(value string) bool {
	switch strings.TrimSpace(value) {
	case "maintenance", "rotation", "shutdown", "incident", "operator_request", "capacity":
		return true
	default:
		return false
	}
}

func workerRequestHash(eventType string, payload map[string]any) string {
	payload["event_type"] = eventType
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func workerTokenFingerprint(tokenHash string) string {
	if len(tokenHash) <= workerTokenFingerprintLength {
		return tokenHash
	}
	return tokenHash[:workerTokenFingerprintLength]
}

func derivedWorkerIdempotencyKey(parent, suffix string) string {
	digest := sha256.Sum256([]byte(parent + "\x00" + suffix))
	return hex.EncodeToString(digest[:])
}
