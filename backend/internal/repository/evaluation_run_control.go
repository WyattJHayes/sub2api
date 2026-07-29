package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

type runControlEvent struct {
	ID             uuid.UUID
	RunID          uuid.UUID
	EventType      string
	RequestHash    string
	FromStatus     service.RunStatus
	ToStatus       service.RunStatus
	ControlEpoch   int64
	PreviousEpoch  int64
	Affected       int
	ReplacementIDs []uuid.UUID
}

type runControlMutation struct {
	eventType string
	from      service.RunStatus
	apply     func(context.Context, *sql.Tx, *service.EvaluationRun) (runControlWork, error)
}

type runControlWork struct {
	affected       int
	replacementIDs []uuid.UUID
}

func (r *radarGovernanceRepository) PauseRun(ctx context.Context, input service.RadarRunActionInput) (*service.RadarRunActionResult, error) {
	return r.mutateRun(ctx, input, runControlMutation{
		eventType: "run_paused",
		apply: func(ctx context.Context, tx *sql.Tx, run *service.EvaluationRun) (runControlWork, error) {
			if run.Status != service.RunStatusPending && run.Status != service.RunStatusRunning && run.Status != service.RunStatusBudgetPaused {
				return runControlWork{}, service.ErrRadarRunStateConflict
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE evaluation_runs
				SET status = 'paused', paused_from_status = $2, pause_reason = $3,
					state_version = state_version + 1, updated_at = NOW()
				WHERE id = $1`, run.ID, run.Status, strings.TrimSpace(input.Reason)); err != nil {
				return runControlWork{}, fmt.Errorf("pause evaluation run: %w", err)
			}
			return runControlWork{}, nil
		},
	})
}

func (r *radarGovernanceRepository) ResumeRun(ctx context.Context, input service.RadarRunActionInput) (*service.RadarRunActionResult, error) {
	return r.mutateRun(ctx, input, runControlMutation{
		eventType: "run_resumed",
		apply: func(ctx context.Context, tx *sql.Tx, run *service.EvaluationRun) (runControlWork, error) {
			if run.Status != service.RunStatusPaused {
				return runControlWork{}, service.ErrRadarRunStateConflict
			}
			to, failed, err := recomputeResumeStatus(ctx, tx, run)
			if err != nil {
				return runControlWork{}, err
			}
			if failed {
				if _, err := tx.ExecContext(ctx, `
					UPDATE evaluation_runs SET status = 'failed', control_epoch = control_epoch + 1,
						state_version = state_version + 1, finished_at = NOW(), updated_at = NOW()
					WHERE id = $1`, run.ID); err != nil {
					return runControlWork{}, fmt.Errorf("fail evaluation run while resuming: %w", err)
				}
				if _, err := cancelRunWork(ctx, tx, run.ID); err != nil {
					return runControlWork{}, err
				}
			} else if _, err := tx.ExecContext(ctx, `
				UPDATE evaluation_runs
				SET status = $2, paused_from_status = NULL, pause_reason = NULL,
					state_version = state_version + 1, updated_at = NOW()
				WHERE id = $1`, run.ID, to); err != nil {
				return runControlWork{}, fmt.Errorf("resume evaluation run: %w", err)
			}
			return runControlWork{}, nil
		},
	})
}

func (r *radarGovernanceRepository) CancelRun(ctx context.Context, input service.RadarRunActionInput) (*service.RadarRunActionResult, error) {
	return r.mutateRun(ctx, input, runControlMutation{
		eventType: "run_cancelled",
		apply: func(ctx context.Context, tx *sql.Tx, run *service.EvaluationRun) (runControlWork, error) {
			if run.Status == service.RunStatusCompleted || run.Status == service.RunStatusFailed || run.Status == service.RunStatusCancelled {
				return runControlWork{}, service.ErrRadarRunStateConflict
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE evaluation_runs
				SET status = 'cancelled', control_epoch = control_epoch + 1,
					state_version = state_version + 1, cancelled_at = NOW(), cancelled_by = $2,
					finished_at = COALESCE(finished_at, NOW()), paused_from_status = NULL,
					pause_reason = NULL, updated_at = NOW()
				WHERE id = $1`, run.ID, input.ActorID); err != nil {
				return runControlWork{}, fmt.Errorf("cancel evaluation run: %w", err)
			}
			affected, err := cancelRunWork(ctx, tx, run.ID)
			if err != nil {
				return runControlWork{}, err
			}
			return runControlWork{affected: affected}, nil
		},
	})
}

func (r *radarGovernanceRepository) FenceRun(ctx context.Context, input service.RadarRunActionInput) (*service.RadarRunActionResult, error) {
	return r.mutateRun(ctx, input, runControlMutation{
		eventType: "run_fenced",
		apply: func(ctx context.Context, tx *sql.Tx, run *service.EvaluationRun) (runControlWork, error) {
			if run.Status == service.RunStatusCompleted || run.Status == service.RunStatusFailed || run.Status == service.RunStatusCancelled {
				return runControlWork{}, service.ErrRadarRunStateConflict
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE evaluation_runs
				SET control_epoch = control_epoch + 1, state_version = state_version + 1, updated_at = NOW()
				WHERE id = $1`, run.ID); err != nil {
				return runControlWork{}, fmt.Errorf("fence evaluation run: %w", err)
			}
			return fenceRunWork(ctx, tx, run.ID)
		},
	})
}

func (r *radarGovernanceRepository) mutateRun(ctx context.Context, input service.RadarRunActionInput, mutation runControlMutation) (*service.RadarRunActionResult, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if input.RunID == uuid.Nil || input.ActorID <= 0 || !validWorkerIdempotencyKey(input.IdempotencyKey) {
		return nil, errors.New("run action requires run, actor and idempotency key")
	}
	if !validRunReason(input.Reason) {
		return nil, errors.New("run action reason is invalid")
	}
	requestHash := workerRequestHash(mutation.eventType, map[string]any{
		"run_id": input.RunID, "reason": strings.TrimSpace(input.Reason),
	})
	result := &service.RadarRunActionResult{}
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"), func(tx *sql.Tx) error {
		existing, err := loadRunControlEvent(ctx, tx, input.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.EventType != mutation.eventType || existing.RequestHash != requestHash || existing.RunID != input.RunID {
				return service.ErrRadarRunIdempotencyConflict
			}
			run, err := loadEvaluationRunForControl(ctx, tx, input.RunID)
			if err != nil {
				return err
			}
			result.Run = run
			result.RunID = run.ID
			result.FromStatus = existing.FromStatus
			result.ToStatus = existing.ToStatus
			result.PreviousEpoch = existing.PreviousEpoch
			result.CurrentEpoch = existing.ControlEpoch
			result.AffectedWorkCount = existing.Affected
			result.ReplacementIDs = existing.ReplacementIDs
			result.EventID = existing.ID
			result.Idempotent = true
			return nil
		}
		run, err := loadEvaluationRunForControl(ctx, tx, input.RunID)
		if err != nil {
			return err
		}
		mutation.from = run.Status
		work, err := mutation.apply(ctx, tx, run)
		if err != nil {
			return err
		}
		updatedBeforeEvent, err := loadEvaluationRunForControl(ctx, tx, input.RunID)
		if err != nil {
			return err
		}
		var eventID uuid.UUID
		var stateVersion int64
		var controlEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT state_version, control_epoch FROM evaluation_runs WHERE id = $1`, input.RunID).Scan(&stateVersion, &controlEpoch); err != nil {
			return fmt.Errorf("read updated evaluation run state: %w", err)
		}
		payload, err := json.Marshal(map[string]any{
			"actor_id": input.ActorID, "reason": strings.TrimSpace(input.Reason),
			"request_hash": requestHash, "affected_work_count": work.affected,
			"replacement_ids": work.replacementIDs, "previous_epoch": run.ControlEpoch,
			"current_epoch": updatedBeforeEvent.ControlEpoch,
		})
		if err != nil {
			return fmt.Errorf("encode run control event: %w", err)
		}
		eventID = uuid.New()
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO evaluation_run_events (
				id, run_id, event_type, payload, actor_type, actor_ref,
				transition_version, from_status, to_status, control_epoch, idempotency_key
			) VALUES ($1, $2, $3, $4::jsonb, 'user', $5, $6, $7, $8, $9, $10)
			RETURNING id`, eventID, input.RunID, mutation.eventType, string(payload), strconv.FormatInt(input.ActorID, 10),
			stateVersion, mutation.from, updatedBeforeEvent.Status, controlEpoch, input.IdempotencyKey).Scan(&eventID); err != nil {
			return fmt.Errorf("append run control event: %w", err)
		}
		updated, err := loadEvaluationRunForControl(ctx, tx, input.RunID)
		if err != nil {
			return err
		}
		result.Run = updated
		result.RunID = updated.ID
		result.FromStatus = mutation.from
		result.ToStatus = updated.Status
		result.PreviousEpoch = run.ControlEpoch
		result.CurrentEpoch = updated.ControlEpoch
		result.AffectedWorkCount = work.affected
		result.ReplacementIDs = work.replacementIDs
		result.EventID = eventID
		return nil
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return result, nil
}

func validRunReason(reason string) bool {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 64 {
		return false
	}
	for _, r := range reason {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

func loadRunControlEvent(ctx context.Context, tx *sql.Tx, key string) (*runControlEvent, error) {
	var event runControlEvent
	var payload []byte
	err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, event_type, payload, from_status, to_status, control_epoch
		FROM evaluation_run_events WHERE idempotency_key = $1 FOR UPDATE`, key).
		Scan(&event.ID, &event.RunID, &event.EventType, &payload, &event.FromStatus, &event.ToStatus, &event.ControlEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load run control event: %w", err)
	}
	var body struct {
		RequestHash string `json:"request_hash"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, fmt.Errorf("decode run control event: %w", err)
	}
	event.RequestHash = body.RequestHash
	var details struct {
		Affected       int         `json:"affected_work_count"`
		ReplacementIDs []uuid.UUID `json:"replacement_ids"`
		PreviousEpoch  int64       `json:"previous_epoch"`
	}
	if err := json.Unmarshal(payload, &details); err == nil {
		event.Affected = details.Affected
		event.ReplacementIDs = details.ReplacementIDs
		event.PreviousEpoch = details.PreviousEpoch
	}
	return &event, nil
}

func recomputeResumeStatus(ctx context.Context, tx *sql.Tx, run *service.EvaluationRun) (service.RunStatus, bool, error) {
	var total, completed, open, p0Total, p0Completed, p0Failed, p0Open int
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE status = 'completed'),
			COUNT(*) FILTER (WHERE status IN ('pending', 'leased', 'running', 'evidence_uploaded', 'grading')),
			COUNT(*) FILTER (WHERE priority = 'P0'),
			COUNT(*) FILTER (WHERE priority = 'P0' AND status = 'completed'),
			COUNT(*) FILTER (WHERE priority = 'P0' AND status IN ('infra_failed', 'upstream_failed', 'invalid_evidence', 'grading_failed', 'cancelled')),
			COUNT(*) FILTER (WHERE priority = 'P0' AND status IN ('pending', 'leased', 'running', 'evidence_uploaded', 'grading'))
		FROM evaluation_samples WHERE run_id = $1`, run.ID).
		Scan(&total, &completed, &open, &p0Total, &p0Completed, &p0Failed, &p0Open); err != nil {
		return service.RunStatusPending, false, fmt.Errorf("recompute evaluation run readiness: %w", err)
	}
	if p0Failed > 0 && p0Open == 0 {
		return service.RunStatusFailed, true, nil
	}
	if p0Total > 0 && p0Completed < p0Total && run.ReservedCost.GreaterThanOrEqual(run.BudgetLimit) {
		return service.RunStatusBudgetPaused, false, nil
	}
	if run.PausedFromStatus == service.RunStatusPending && completed == 0 && open == total {
		return service.RunStatusPending, false, nil
	}
	return service.RunStatusRunning, false, nil
}

func loadEvaluationRunForControl(ctx context.Context, tx *sql.Tx, id uuid.UUID) (*service.EvaluationRun, error) {
	run := &service.EvaluationRun{ID: id}
	var paused sql.NullString
	var cancelledBy sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT plan_id, status, budget_limit, reserved_cost, created_at,
			paused_from_status, pause_reason, control_epoch, state_version,
			cancelled_at, cancelled_by
		FROM evaluation_runs WHERE id = $1 FOR UPDATE`, id).Scan(
		&run.PlanID, &run.Status, &run.BudgetLimit, &run.ReservedCost, &run.CreatedAt,
		&paused, &run.PauseReason, &run.ControlEpoch, &run.StateVersion,
		&run.CancelledAt, &cancelledBy)
	if err != nil {
		return nil, err
	}
	if paused.Valid {
		run.PausedFromStatus = service.RunStatus(paused.String)
	}
	if cancelledBy.Valid {
		run.CancelledBy = &cancelledBy.Int64
	}
	return run, nil
}

func cancelRunWork(ctx context.Context, tx *sql.Tx, runID uuid.UUID) (int, error) {
	affected := 0
	if result, err := tx.ExecContext(ctx, `
		UPDATE evaluation_assignments a
		SET status = 'cancelled', lease_token_hash = NULL, leased_by = NULL,
			lease_expires_at = NULL, heartbeat_at = NULL, finished_at = NOW(), updated_at = NOW()
		FROM evaluation_samples s
		WHERE a.sample_id = s.id AND s.run_id = $1
		  AND a.status IN ('pending', 'leased', 'running', 'evidence_uploaded', 'grading')`, runID); err != nil {
		return 0, fmt.Errorf("cancel evaluation assignments: %w", err)
	} else if n, err := result.RowsAffected(); err == nil {
		affected += int(n)
	}
	if result, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'cancelled', updated_at = NOW() WHERE run_id = $1 AND status IN ('pending', 'leased', 'running', 'evidence_uploaded', 'grading')`, runID); err != nil {
		return 0, fmt.Errorf("cancel evaluation samples: %w", err)
	} else if n, err := result.RowsAffected(); err == nil {
		affected += int(n)
	}
	if result, err := tx.ExecContext(ctx, `UPDATE evaluation_grading_jobs SET status = 'cancelled', lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW() WHERE run_id = $1 AND status IN ('pending', 'leased')`, runID); err != nil {
		return 0, fmt.Errorf("cancel evaluation grading jobs: %w", err)
	} else if n, err := result.RowsAffected(); err == nil {
		affected += int(n)
	}
	if result, err := tx.ExecContext(ctx, `UPDATE evaluation_analysis_jobs SET status = 'cancelled', lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW() WHERE run_id = $1 AND status IN ('pending', 'leased')`, runID); err != nil {
		return 0, fmt.Errorf("cancel evaluation analysis jobs: %w", err)
	} else if n, err := result.RowsAffected(); err == nil {
		affected += int(n)
	}
	return affected, nil
}

func fenceRunWork(ctx context.Context, tx *sql.Tx, runID uuid.UUID) (runControlWork, error) {
	type fencedAssignment struct {
		assignmentID uuid.UUID
		sampleID     uuid.UUID
		caseID       uuid.UUID
		route        string
		sampleIndex  int
		attempt      int
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id, a.sample_id, s.case_id, s.model_route, s.sample_index, a.attempt
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		WHERE s.run_id = $1 AND a.status IN ('leased', 'running')
		FOR UPDATE OF a`, runID)
	if err != nil {
		return runControlWork{}, fmt.Errorf("lock evaluation assignments for fence: %w", err)
	}
	assignments := make([]fencedAssignment, 0)
	for rows.Next() {
		var item fencedAssignment
		if err := rows.Scan(&item.assignmentID, &item.sampleID, &item.caseID, &item.route, &item.sampleIndex, &item.attempt); err != nil {
			_ = rows.Close()
			return runControlWork{}, fmt.Errorf("scan evaluation assignment for fence: %w", err)
		}
		assignments = append(assignments, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return runControlWork{}, fmt.Errorf("iterate evaluation assignments for fence: %w", err)
	}
	if err := rows.Close(); err != nil {
		return runControlWork{}, fmt.Errorf("close evaluation assignments for fence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs SET status = 'cancelled', lease_token_hash = NULL,
			leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE run_id = $1 AND status IN ('pending', 'leased')`, runID); err != nil {
		return runControlWork{}, fmt.Errorf("fence evaluation grading jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_analysis_jobs SET status = 'cancelled', lease_token_hash = NULL,
			leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE run_id = $1 AND status IN ('pending', 'leased')`, runID); err != nil {
		return runControlWork{}, fmt.Errorf("fence evaluation analysis jobs: %w", err)
	}
	replacements := 0
	replacementIDs := make([]uuid.UUID, 0)
	affected := 0
	exhausted := false
	for _, item := range assignments {
		assignmentID, sampleID, caseID := item.assignmentID, item.sampleID, item.caseID
		route, sampleIndex, attempt := item.route, item.sampleIndex, item.attempt
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_assignments SET status = 'cancelled', lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL, heartbeat_at = NULL, finished_at = NOW(), updated_at = NOW() WHERE id = $1`, assignmentID); err != nil {
			return runControlWork{}, fmt.Errorf("fence evaluation assignment: %w", err)
		}
		affected++
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'pending', updated_at = NOW() WHERE id = $1`, sampleID); err != nil {
			return runControlWork{}, fmt.Errorf("reset evaluation sample after fence: %w", err)
		}
		if attempt >= 2 {
			exhausted = true
			if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'infra_failed', failure_class = 'infrastructure', failure_code = 'fence_retry_exhausted', updated_at = NOW() WHERE id = $1`, sampleID); err != nil {
				return runControlWork{}, fmt.Errorf("mark fenced sample failed: %w", err)
			}
			continue
		}
		nextID := uuid.New()
		_, err = tx.ExecContext(ctx, `
			INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status, lease_epoch, work_origin)
			VALUES ($1, $2, $3, $4, 'pending', (SELECT control_epoch FROM evaluation_runs WHERE id = $5), 'initial')`,
			nextID, sampleID, attempt+1, assignmentIdempotencyKey(runID, caseID, route, sampleIndex, attempt+1), runID)
		if err != nil {
			return runControlWork{}, fmt.Errorf("create fenced replacement assignment: %w", err)
		}
		replacements++
		replacementIDs = append(replacementIDs, nextID)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_assignments a SET lease_epoch = r.control_epoch, updated_at = NOW()
		FROM evaluation_samples s JOIN evaluation_runs r ON r.id = s.run_id
		WHERE a.sample_id = s.id AND s.run_id = $1 AND a.status = 'pending'`, runID); err != nil {
		return runControlWork{}, fmt.Errorf("advance pending assignment epoch: %w", err)
	}
	if exhausted {
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_runs
			SET status = 'failed', state_version = state_version + 1,
				finished_at = NOW(), updated_at = NOW()
			WHERE id = $1`, runID); err != nil {
			return runControlWork{}, fmt.Errorf("fail fenced evaluation run: %w", err)
		}
		cancelled, err := cancelRunWork(ctx, tx, runID)
		if err != nil {
			return runControlWork{}, err
		}
		affected += cancelled
	}
	return runControlWork{affected: affected, replacementIDs: replacementIDs}, nil
}
