package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

func (r *evaluationGradingRepository) GetApprovedFaultExperiment(ctx context.Context, id uuid.UUID) (*service.RadarFaultExperiment, error) {
	if r == nil || r.db == nil || id == uuid.Nil {
		return nil, service.ErrFaultExperimentInvalid
	}
	record, err := scanRadarFaultExperiment(r.db.QueryRowContext(ctx, `
		SELECT id, run_id, load_plan_id, environment, fault_kind, target_kind,
			target_ref, status, approved_by, abort_deadline
		FROM evaluation_fault_experiments
		WHERE id=$1 AND status='approved'`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("load approved fault experiment: %w", err)
	}
	if err := ensureRadarExecutionScope(ctx, r.db, record.RunID); err != nil {
		return nil, err
	}
	return record, nil
}

func (r *evaluationGradingRepository) ApplyFaultAction(ctx context.Context, id uuid.UUID, request service.RadarFaultActionRequest) (*service.RadarFaultActionReceipt, error) {
	if r == nil || r.db == nil || id == uuid.Nil {
		return nil, service.ErrFaultActionInvalid
	}
	action := strings.ToLower(strings.TrimSpace(request.Action))
	if action != "inject" && action != "rollback" {
		return nil, service.ErrFaultActionInvalid
	}
	if strings.TrimSpace(request.FaultKind) == "" || strings.TrimSpace(request.TargetKind) == "" || strings.TrimSpace(request.TargetRef) == "" {
		return nil, service.ErrFaultActionInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin fault action: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	experiment, err := scanRadarFaultExperiment(tx.QueryRowContext(ctx, `
		SELECT id, run_id, load_plan_id, environment, fault_kind, target_kind,
			target_ref, status, approved_by, abort_deadline
		FROM evaluation_fault_experiments WHERE id=$1 FOR UPDATE`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("lock fault experiment: %w", err)
	}
	if err := ensureRadarExecutionScope(ctx, tx, experiment.RunID); err != nil {
		return nil, err
	}
	if experiment.FaultKind != request.FaultKind || experiment.TargetKind != request.TargetKind || experiment.TargetRef != request.TargetRef {
		return nil, service.ErrFaultActionInvalid
	}
	if action == "inject" {
		if experiment.Status != "approved" || experiment.ApprovedBy == nil || experiment.AbortDeadline == nil || !experiment.AbortDeadline.After(time.Now().UTC()) {
			return nil, service.ErrFaultActionInvalid
		}
	} else if experiment.Status != "running" {
		return nil, service.ErrFaultActionInvalid
	}
	receipt := &service.RadarFaultActionReceipt{
		ExperimentID: id,
		Action:       action,
		OperationID:  uuid.New(),
		Status:       experiment.Status,
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit fault action: %w", err)
	}
	return receipt, nil
}

func (r *evaluationGradingRepository) AppendFaultEvent(ctx context.Context, event service.RadarFaultEventSubmission) (*service.RadarFaultEventReceipt, error) {
	if r == nil || r.db == nil || event.ExperimentID == uuid.Nil || event.RunID == uuid.Nil {
		return nil, service.ErrFaultEventInvalid
	}
	event.EventType = strings.ToLower(strings.TrimSpace(event.EventType))
	event.ServiceIdentity = strings.TrimSpace(event.ServiceIdentity)
	event.CauseEvent = strings.TrimSpace(event.CauseEvent)
	event.EventHash = strings.TrimSpace(event.EventHash)
	if event.EventType == "" || event.ServiceIdentity == "" || event.CauseEvent == "" || len(event.Payload) == 0 || !isLowerHexDigest(event.EventHash) {
		return nil, service.ErrFaultEventInvalid
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, service.ErrFaultEventInvalid
	}
	if payload["cause_event"] != event.CauseEvent || payload["service_identity"] != event.ServiceIdentity {
		return nil, service.ErrFaultEventInvalid
	}
	canonicalCreatedAt, ok := canonicalRadarEventTimestamp(event.CreatedAt)
	if !ok {
		return nil, service.ErrFaultEventInvalid
	}
	expectedEventHash, err := radarFaultEventHash(event, canonicalCreatedAt)
	if err != nil || event.EventHash != expectedEventHash {
		return nil, service.ErrFaultEventInvalid
	}
	status, ok := faultEventTransition(event.EventType)
	if !ok {
		return nil, service.ErrFaultEventInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin fault event: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureRadarExecutionScope(ctx, tx, event.RunID); err != nil {
		return nil, err
	}
	var existingID uuid.UUID
	var existingEventType string
	err = tx.QueryRowContext(ctx, `
		SELECT id, event_type FROM evaluation_fault_experiment_events
		WHERE experiment_id=$1 AND event_hash=$2 LIMIT 1`, event.ExperimentID, event.EventHash).
		Scan(&existingID, &existingEventType)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit idempotent fault event: %w", err)
		}
		existingStatus, _ := faultEventTransition(existingEventType)
		return &service.RadarFaultEventReceipt{Accepted: true, EventID: existingID, EventHash: event.EventHash, Status: existingStatus}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check fault event idempotency: %w", err)
	}
	var currentStatus string
	if err := tx.QueryRowContext(ctx, `
		SELECT status FROM evaluation_fault_experiments
		WHERE id=$1 AND run_id=$2 FOR UPDATE`, event.ExperimentID, event.RunID).Scan(&currentStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrFaultEventInvalid
		}
		return nil, fmt.Errorf("lock fault event experiment: %w", err)
	}
	if !faultStatusTransitionAllowed(currentStatus, event.EventType) {
		return nil, service.ErrFaultEventInvalid
	}
	eventID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_fault_experiment_events (
			id, experiment_id, run_id, event_type, actor_id, payload, event_hash, created_at
		) VALUES ($1,$2,$3,$4,$5,$6::jsonb,$7,$8)`, eventID, event.ExperimentID, event.RunID,
		event.EventType, event.ActorID, string(event.Payload), event.EventHash, event.CreatedAt); err != nil {
		return nil, fmt.Errorf("insert fault event: %w", err)
	}
	var update string
	switch event.EventType {
	case "started":
		update = `UPDATE evaluation_fault_experiments SET status='running', started_at=$2, updated_at=transaction_timestamp() WHERE id=$1`
	case "aborted", "completed", "failed", "cancelled":
		update = `UPDATE evaluation_fault_experiments SET status=$2, finished_at=$3, updated_at=transaction_timestamp() WHERE id=$1`
	}
	if event.EventType == "started" {
		if _, err := tx.ExecContext(ctx, update, event.ExperimentID, event.CreatedAt); err != nil {
			return nil, fmt.Errorf("advance fault experiment status: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, update, event.ExperimentID, status, event.CreatedAt); err != nil {
			return nil, fmt.Errorf("finish fault experiment status: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit fault event: %w", err)
	}
	return &service.RadarFaultEventReceipt{Accepted: true, EventID: eventID, EventHash: event.EventHash, Status: status}, nil
}

func (r *evaluationGradingRepository) GetRecoveryObservation(ctx context.Context, id uuid.UUID) (*service.RadarRecoveryObservation, error) {
	if r == nil || r.db == nil || id == uuid.Nil {
		return nil, service.ErrRecoveryObservationNotFound
	}
	var runID uuid.UUID
	var evidenceTenantID, runTenantID int64
	var raw []byte
	var status string
	if err := r.db.QueryRowContext(ctx, `
		SELECT e.run_id, e.tenant_id, r.tenant_id, e.canonical_evidence_bytes, e.status
		FROM evaluation_recovery_evidence e
		JOIN evaluation_runs r ON r.id=e.run_id
		WHERE e.id=$1`, id).Scan(&runID, &evidenceTenantID, &runTenantID, &raw, &status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrRecoveryObservationNotFound
		}
		return nil, fmt.Errorf("load recovery observation: %w", err)
	}
	if err := ensureRadarExecutionScope(ctx, r.db, runID); err != nil {
		return nil, err
	}
	if evidenceTenantID != runTenantID {
		return nil, service.ErrRadarForbidden
	}
	if status != "pending" || !json.Valid(raw) {
		return nil, service.ErrRecoveryObservationNotFound
	}
	var envelope struct {
		Observation json.RawMessage `json:"observation"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Observation) > 0 {
		raw = envelope.Observation
	}
	if !json.Valid(raw) {
		return nil, service.ErrRecoveryObservationNotFound
	}
	return &service.RadarRecoveryObservation{Observation: append(json.RawMessage(nil), raw...)}, nil
}

func (r *evaluationGradingRepository) PublishRecoveryEvidence(ctx context.Context, observationID uuid.UUID, submission service.RadarRecoveryEvidenceSubmission) (*service.RadarRecoveryEvidenceReceipt, error) {
	if r == nil || r.db == nil || observationID == uuid.Nil {
		return nil, service.ErrRecoveryEvidenceInvalid
	}
	canonical, err := decodeCanonicalEvidence(submission.CanonicalEvidence)
	if err != nil || len(canonical) == 0 || !json.Valid(canonical) {
		return nil, service.ErrRecoveryEvidenceInvalid
	}
	digest := sha256.Sum256(canonical)
	submission.SourceWatermark = strings.TrimSpace(submission.SourceWatermark)
	if submission.EvidenceHash != hex.EncodeToString(digest[:]) ||
		!isLowerHexDigest(submission.SourceWatermark) ||
		submission.RunID == uuid.Nil || submission.ExperimentID == uuid.Nil ||
		submission.RecoveryGeneration < 0 || submission.DuplicateScoreCount < 0 {
		return nil, service.ErrRecoveryEvidenceInvalid
	}
	if submission.Status != "verified" && submission.Status != "rejected" && submission.Status != "pending" {
		return nil, service.ErrRecoveryEvidenceInvalid
	}
	if submission.Status == "verified" && (submission.RPOms == nil || submission.RTOms == nil || submission.VerifiedBy == nil || submission.DeterministicRunID == nil || submission.VerifiedAt.IsZero()) {
		return nil, service.ErrRecoveryEvidenceInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin recovery evidence publish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sourceRunID, sourceExperimentID uuid.UUID
	var sourceTenantID, runTenantID int64
	var sourceGeneration int
	var sourceStatus string
	if err := tx.QueryRowContext(ctx, `
		SELECT e.run_id, e.experiment_id, e.recovery_generation, e.status,
		       e.tenant_id, r.tenant_id
		FROM evaluation_recovery_evidence e
		JOIN evaluation_runs r ON r.id=e.run_id
		WHERE e.id=$1 FOR SHARE`, observationID).
		Scan(&sourceRunID, &sourceExperimentID, &sourceGeneration, &sourceStatus, &sourceTenantID, &runTenantID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrRecoveryObservationNotFound
		}
		return nil, fmt.Errorf("load recovery observation identity: %w", err)
	}
	if err := ensureRadarExecutionScope(ctx, tx, sourceRunID); err != nil {
		return nil, err
	}
	if sourceTenantID != runTenantID {
		return nil, service.ErrRadarForbidden
	}
	if sourceStatus != "pending" {
		return nil, service.ErrRecoveryEvidenceInvalid
	}
	if sourceRunID != submission.RunID || sourceExperimentID != submission.ExperimentID || sourceGeneration != submission.RecoveryGeneration {
		return nil, service.ErrRecoveryEvidenceInvalid
	}
	var existingID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		SELECT id FROM evaluation_recovery_evidence
		WHERE experiment_id=$1 AND run_id=$2 AND recovery_generation=$3 AND evidence_hash=$4`,
		submission.ExperimentID, submission.RunID, submission.RecoveryGeneration, submission.EvidenceHash).Scan(&existingID); err == nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit idempotent recovery evidence: %w", err)
		}
		return &service.RadarRecoveryEvidenceReceipt{EvidenceID: existingID, EvidenceHash: submission.EvidenceHash, Status: submission.Status}, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check recovery evidence idempotency: %w", err)
	}
	evidenceID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_recovery_evidence (
			id, run_id, experiment_id, recovery_generation, source_watermark,
				canonical_evidence_bytes, evidence_hash, status, rpo_ms, rto_ms,
				duplicate_score_count, deterministic_run_id, verified_by, verified_at,
				source_observation_id, tenant_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, evidenceID,
		submission.RunID, submission.ExperimentID, submission.RecoveryGeneration, submission.SourceWatermark,
		canonical, submission.EvidenceHash, submission.Status, submission.RPOms, submission.RTOms,
		submission.DuplicateScoreCount, submission.DeterministicRunID, submission.VerifiedBy,
		submission.VerifiedAt, observationID, sourceTenantID); err != nil {
		return nil, fmt.Errorf("insert recovery evidence: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit recovery evidence publish: %w", err)
	}
	return &service.RadarRecoveryEvidenceReceipt{EvidenceID: evidenceID, EvidenceHash: submission.EvidenceHash, Status: submission.Status}, nil
}

func scanRadarFaultExperiment(scanner interface{ Scan(...any) error }) (*service.RadarFaultExperiment, error) {
	record := &service.RadarFaultExperiment{}
	var loadPlanID uuid.NullUUID
	var approvedBy sql.NullInt64
	var abortDeadline sql.NullTime
	if err := scanner.Scan(&record.ID, &record.RunID, &loadPlanID, &record.Environment, &record.FaultKind,
		&record.TargetKind, &record.TargetRef, &record.Status, &approvedBy, &abortDeadline); err != nil {
		return nil, err
	}
	if loadPlanID.Valid {
		record.LoadPlanID = &loadPlanID.UUID
	}
	if approvedBy.Valid {
		record.ApprovedBy = &approvedBy.Int64
	}
	if abortDeadline.Valid {
		value := abortDeadline.Time
		record.AbortDeadline = &value
	}
	return record, nil
}

func faultEventTransition(eventType string) (string, bool) {
	switch eventType {
	case "started":
		return "running", true
	case "aborted", "completed", "failed", "cancelled":
		return eventType, true
	default:
		return "", false
	}
}

func faultStatusTransitionAllowed(currentStatus, eventType string) bool {
	if eventType == "started" {
		return currentStatus == "approved"
	}
	return currentStatus == "running"
}

func isLowerHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func canonicalRadarEventTimestamp(value time.Time) (string, bool) {
	if value.IsZero() {
		return "", false
	}
	_, offset := value.Zone()
	if offset != 0 || value.Nanosecond()%1000 != 0 {
		return "", false
	}
	utc := value.UTC()
	result := utc.Format("2006-01-02T15:04:05")
	if micros := utc.Nanosecond() / 1000; micros > 0 {
		result += "." + strings.TrimRight(fmt.Sprintf("%06d", micros), "0")
	}
	return result + "Z", true
}

func radarFaultEventHash(event service.RadarFaultEventSubmission, canonicalCreatedAt string) (string, error) {
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		return "", err
	}
	canonicalEvent, err := json.Marshal(struct {
		ActorID      *int64          `json:"actor_id"`
		CreatedAt    string          `json:"created_at"`
		EventType    string          `json:"event_type"`
		ExperimentID string          `json:"experiment_id"`
		Payload      json.RawMessage `json:"payload"`
		RunID        string          `json:"run_id"`
	}{
		ActorID: event.ActorID, CreatedAt: canonicalCreatedAt, EventType: event.EventType,
		ExperimentID: event.ExperimentID.String(), Payload: payload, RunID: event.RunID.String(),
	})
	if err != nil {
		return "", err
	}
	return service.DigestCanonicalJSON(canonicalEvent)
}

func decodeCanonicalEvidence(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, errors.New("canonical evidence is empty")
	}
	if raw[0] == '"' {
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, err
		}
		return []byte(encoded), nil
	}
	return append([]byte(nil), raw...), nil
}

var _ service.RadarReliabilityExecutionRepository = (*evaluationGradingRepository)(nil)
