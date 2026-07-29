package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

var _ service.RunControlRepository = (*radarGovernanceRepository)(nil)

var validRunControlReasons = map[string]struct{}{
	"operator": {},
	"budget":   {},
	"incident": {},
	"release":  {},
	"safety":   {},
	"recovery": {},
}

type runControlRow struct {
	status       string
	pausedFrom   sql.NullString
	pauseReason  sql.NullString
	epoch        int64
	stateVersion int64
}

func validateRunControl(runID uuid.UUID, reason, idempotencyKey string) error {
	if runID == uuid.Nil || len(strings.TrimSpace(idempotencyKey)) != 64 {
		return infraerrors.New(http.StatusBadRequest, "RUN_CONTROL_INVALID", "run id and 64 character idempotency key are required")
	}
	if _, ok := validRunControlReasons[strings.TrimSpace(reason)]; !ok {
		return infraerrors.New(http.StatusBadRequest, "RUN_CONTROL_INVALID", "unsupported run control reason")
	}
	return nil
}

func decodeRunControlReplay(payload []byte, eventID, runID uuid.UUID) (*service.RunControlResult, error) {
	var result service.RunControlResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return nil, fmt.Errorf("decode run control replay: %w", err)
	}
	result.EventID = eventID
	result.RunID = runID
	return &result, nil
}

func loadRunControlReplay(ctx context.Context, tx *sql.Tx, runID uuid.UUID, idempotencyKey, reason, action string) (*service.RunControlResult, bool, error) {
	var eventID, eventRunID uuid.UUID
	var eventType string
	var payload []byte
	err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, event_type, payload
		FROM evaluation_run_events
		WHERE idempotency_key = $1
		FOR UPDATE`, idempotencyKey).Scan(&eventID, &eventRunID, &eventType, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load run control event: %w", err)
	}
	if eventRunID != runID || eventType != "run_control_"+action {
		return nil, false, infraerrors.Conflict("RUN_CONTROL_IDEMPOTENCY_CONFLICT", "idempotency key belongs to another run action")
	}
	var body struct {
		Reason string          `json:"reason"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &body); err != nil || body.Reason != reason {
		return nil, false, infraerrors.Conflict("RUN_CONTROL_IDEMPOTENCY_CONFLICT", "idempotency key was reused with different parameters")
	}
	result, err := decodeRunControlReplay(body.Result, eventID, runID)
	if err != nil {
		return nil, false, err
	}
	return result, true, nil
}

func (r *radarGovernanceRepository) controlRun(ctx context.Context, runID uuid.UUID, reason string, actorID int64, idempotencyKey, action string) (*service.RunControlResult, error) {
	reason = strings.TrimSpace(reason)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if err := validateRunControl(runID, reason, idempotencyKey); err != nil {
		return nil, err
	}
	if actorID <= 0 {
		return nil, infraerrors.New(http.StatusBadRequest, "RUN_CONTROL_INVALID", "actor is required")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if replay, found, err := loadRunControlReplay(ctx, tx, runID, idempotencyKey, reason, action); err != nil {
		return nil, err
	} else if found {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return replay, nil
	}
	var row runControlRow
	if err := tx.QueryRowContext(ctx, `
		SELECT status, paused_from_status, pause_reason, control_epoch, state_version
		FROM evaluation_runs WHERE id = $1 FOR UPDATE`, runID).Scan(
		&row.status, &row.pausedFrom, &row.pauseReason, &row.epoch, &row.stateVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, infraerrors.New(http.StatusNotFound, "RUN_NOT_FOUND", "evaluation run not found")
		}
		return nil, fmt.Errorf("lock evaluation run: %w", err)
	}
	if row.status == "completed" || row.status == "failed" || row.status == "cancelled" {
		return nil, infraerrors.Conflict("RUN_TERMINAL", "terminal evaluation runs cannot be controlled")
	}
	result := &service.RunControlResult{RunID: runID, FromStatus: row.status, PreviousEpoch: row.epoch, CurrentEpoch: row.epoch}
	toStatus := row.status
	newEpoch := row.epoch
	newStateVersion := row.stateVersion + 1
	var affected int
	var replacementIDs []uuid.UUID
	var eventPayload map[string]any
	switch action {
	case "pause":
		if row.status != "pending" && row.status != "running" && row.status != "budget_paused" {
			return nil, infraerrors.Conflict("RUN_PAUSE_INVALID", "run is not pauseable")
		}
		toStatus = "paused"
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_runs SET status='paused', paused_from_status=$2, pause_reason=$3, state_version=$4, updated_at=NOW() WHERE id=$1`, runID, row.status, reason, newStateVersion); err != nil {
			return nil, fmt.Errorf("pause evaluation run: %w", err)
		}
	case "resume":
		if row.status != "paused" {
			return nil, infraerrors.Conflict("RUN_RESUME_INVALID", "run is not paused")
		}
		toStatus = "pending"
		if row.pausedFrom.Valid && row.pausedFrom.String != "" {
			toStatus = row.pausedFrom.String
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_runs SET status=$2, paused_from_status=NULL, pause_reason=NULL, state_version=$3, updated_at=NOW() WHERE id=$1`, runID, toStatus, newStateVersion); err != nil {
			return nil, fmt.Errorf("resume evaluation run: %w", err)
		}
		var p0Pending int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluation_samples WHERE run_id=$1 AND priority='P0' AND status NOT IN ('completed','cancelled','infra_failed','upstream_failed','invalid_evidence','grading_failed')`, runID).Scan(&p0Pending); err != nil {
			return nil, fmt.Errorf("recompute P0 readiness: %w", err)
		}
		affected = p0Pending
	case "cancel":
		toStatus = "cancelled"
		newEpoch++
		if resultCount, err := tx.ExecContext(ctx, `UPDATE evaluation_assignments SET status='cancelled', lease_token_hash=NULL, leased_by=NULL, lease_expires_at=NULL, heartbeat_at=NULL, finished_at=NOW(), updated_at=NOW() WHERE sample_id IN (SELECT id FROM evaluation_samples WHERE run_id=$1) AND status NOT IN ('completed','cancelled','infra_failed','upstream_failed','invalid_evidence','grading_failed')`, runID); err != nil {
			return nil, fmt.Errorf("cancel evaluation assignments: %w", err)
		} else if n, err := resultCount.RowsAffected(); err == nil {
			affected += int(n)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status='cancelled', updated_at=NOW() WHERE run_id=$1 AND status NOT IN ('completed','cancelled','infra_failed','upstream_failed','invalid_evidence','grading_failed')`, runID); err != nil {
			return nil, fmt.Errorf("cancel evaluation samples: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_grading_jobs SET status='cancelled', lease_token_hash=NULL, leased_by=NULL, lease_expires_at=NULL, updated_at=NOW() WHERE run_id=$1 AND status NOT IN ('completed','cancelled','failed')`, runID); err != nil {
			return nil, fmt.Errorf("cancel grading jobs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_analysis_jobs SET status='cancelled', lease_token_hash=NULL, leased_by=NULL, lease_expires_at=NULL, finished_at=NOW(), updated_at=NOW() WHERE run_id=$1 AND status NOT IN ('completed','cancelled','failed')`, runID); err != nil {
			return nil, fmt.Errorf("cancel analysis jobs: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_route_evidence_terminalization_outbox
				(id, run_id, terminal_status, control_epoch, idempotency_key, payload)
			VALUES ($1, $2, 'cancelled', $3, $4, jsonb_build_object('run_id', $2::uuid, 'terminal_status', 'cancelled', 'control_epoch', $3::bigint))
			ON CONFLICT (idempotency_key) DO NOTHING`, uuid.New(), runID, newEpoch,
			hashReconcileKey(fmt.Sprintf("evidence-terminalization:%s:cancelled:%d", runID, newEpoch))); err != nil {
			return nil, fmt.Errorf("enqueue route evidence terminalization: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_runs SET status='cancelled', cancelled_at=NOW(), cancelled_by=$2, finished_at=NOW(), control_epoch=$3, state_version=$4, updated_at=NOW() WHERE id=$1`, runID, actorID, newEpoch, newStateVersion); err != nil {
			return nil, fmt.Errorf("cancel evaluation run: %w", err)
		}
	case "fence":
		newEpoch++
		newStateVersion = row.stateVersion
		rows, err := tx.QueryContext(ctx, `SELECT a.id, a.sample_id, s.case_id, s.model_route, s.sample_index, a.attempt, a.work_origin FROM evaluation_assignments a JOIN evaluation_samples s ON s.id=a.sample_id WHERE s.run_id=$1 AND a.status IN ('leased','running') FOR UPDATE`, runID)
		if err != nil {
			return nil, fmt.Errorf("load retryable runner work: %w", err)
		}
		type fencedAssignment struct {
			assignmentID, sampleID, caseID uuid.UUID
			modelRoute                     string
			sampleIndex, attempt           int
			workOrigin                     string
		}
		var fenced []fencedAssignment
		for rows.Next() {
			var item fencedAssignment
			if err := rows.Scan(&item.assignmentID, &item.sampleID, &item.caseID, &item.modelRoute, &item.sampleIndex, &item.attempt, &item.workOrigin); err != nil {
				rows.Close()
				return nil, err
			}
			fenced = append(fenced, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate retryable runner work: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close retryable runner work: %w", err)
		}
		for _, item := range fenced {
			if _, err := tx.ExecContext(ctx, `UPDATE evaluation_assignments SET status='infra_failed', lease_token_hash=NULL, leased_by=NULL, lease_expires_at=NULL, heartbeat_at=NULL, failure_class='infrastructure', failure_code='fenced', finished_at=NOW(), updated_at=NOW() WHERE id=$1`, item.assignmentID); err != nil {
				return nil, err
			}
			replacementID := uuid.New()
			if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status, lease_epoch, work_origin) VALUES ($1,$2,$3,$4,'pending',$5,$6)`, replacementID, item.sampleID, item.attempt+1, assignmentIdempotencyKey(runID, item.caseID, item.modelRoute, item.sampleIndex, item.attempt+1), newEpoch, item.workOrigin); err != nil {
				return nil, err
			}
			if err := propagateAssignmentReplacement(ctx, tx, runID, item.sampleID, item.assignmentID, replacementID, item.attempt); err != nil {
				return nil, err
			}
			replacementIDs = append(replacementIDs, replacementID)
		}
		affected = len(replacementIDs)
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_runs SET control_epoch=$2, state_version=$3, updated_at=NOW() WHERE id=$1`, runID, newEpoch, newStateVersion); err != nil {
			return nil, err
		}
	default:
		return nil, infraerrors.New(http.StatusBadRequest, "RUN_CONTROL_INVALID", "unsupported run action")
	}
	result.ToStatus = toStatus
	result.CurrentEpoch = newEpoch
	result.AffectedWorkCount = affected
	result.ReplacementIDs = replacementIDs
	eventID := uuid.New()
	result.EventID = eventID
	if eventPayload == nil {
		eventPayload = map[string]any{}
	}
	eventPayload["reason"] = reason
	eventPayload["result"] = result
	payload, _ := json.Marshal(eventPayload)
	var transitionVersion any
	var fromStatus, toStatusValue any
	if toStatus != row.status {
		transitionVersion = newStateVersion
		fromStatus, toStatusValue = row.status, toStatus
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_run_events (id, run_id, event_type, payload, actor_type, actor_ref, transition_version, from_status, to_status, control_epoch, idempotency_key) VALUES ($1,$2,$3,$4::jsonb,'user',$5,$6,$7,$8,$9,$10)`, eventID, runID, "run_control_"+action, string(payload), fmt.Sprintf("%d", actorID), transitionVersion, fromStatus, toStatusValue, newEpoch, idempotencyKey); err != nil {
		return nil, fmt.Errorf("record run control event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *radarGovernanceRepository) PauseRun(ctx context.Context, runID uuid.UUID, reason string, actorID int64, key string) (*service.RunControlResult, error) {
	return r.controlRun(ctx, runID, reason, actorID, key, "pause")
}
func (r *radarGovernanceRepository) ResumeRun(ctx context.Context, runID uuid.UUID, reason string, actorID int64, key string) (*service.RunControlResult, error) {
	return r.controlRun(ctx, runID, reason, actorID, key, "resume")
}
func (r *radarGovernanceRepository) CancelRun(ctx context.Context, runID uuid.UUID, reason string, actorID int64, key string) (*service.RunControlResult, error) {
	return r.controlRun(ctx, runID, reason, actorID, key, "cancel")
}
func (r *radarGovernanceRepository) FenceRun(ctx context.Context, runID uuid.UUID, reason string, actorID int64, key string) (*service.RunControlResult, error) {
	return r.controlRun(ctx, runID, reason, actorID, key, "fence")
}
