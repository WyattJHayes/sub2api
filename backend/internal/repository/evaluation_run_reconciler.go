package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

// FailureCause identifies a failure that belongs to the current effective
// attempt. Older attempts are deliberately excluded before this type is built.
type FailureCause struct {
	SampleID     uuid.UUID
	AssignmentID uuid.UUID
	Class        string
	Code         string
}

type RunReconcileFacts struct {
	Status               string
	BudgetMode           string
	Started              bool
	UnrecoverableFailure *FailureCause
	P0Expected           int
	P0Successful         int
	P0Active             int
	P0ScoreHeadsReady    bool
	PendingWork          int
	CurrentCoverageOK    bool
}

type RunTransition struct {
	FromStatus        service.RunStatus
	ToStatus          service.RunStatus
	Reason            string
	Changed           bool
	AppendEvent       bool
	ReadinessRecorded bool
	Fence             bool
}

type RunRecord struct {
	ID           uuid.UUID
	Status       service.RunStatus
	ControlEpoch int64
	StateVersion int64
}

func decideRunTransition(f RunReconcileFacts) RunTransition {
	status := service.RunStatus(f.Status)
	transition := RunTransition{FromStatus: status, ToStatus: status}
	if status == service.RunStatusCompleted || status == service.RunStatusFailed || status == service.RunStatusCancelled {
		return transition
	}
	if status == service.RunStatusPaused {
		transition.ReadinessRecorded = exactP0Ready(f)
		return transition
	}
	if f.UnrecoverableFailure != nil {
		transition.ToStatus = service.RunStatusFailed
		transition.Reason = "unrecoverable_failure"
		transition.Changed = true
		transition.AppendEvent = true
		transition.Fence = true
		return transition
	}
	if status == service.RunStatusBudgetPaused {
		if exactP0Ready(f) {
			transition.ToStatus = service.RunStatusRunning
			transition.Reason = "exact_p0_ready"
			transition.Changed = true
			transition.AppendEvent = true
		}
		return transition
	}
	if f.PendingWork > 0 {
		transition.Reason = "pending_work"
		return transition
	}
	if !f.CurrentCoverageOK {
		transition.Reason = "awaiting_current_aggregate"
		return transition
	}
	transition.ToStatus = service.RunStatusCompleted
	transition.Reason = "current_aggregate_complete"
	transition.Changed = true
	transition.AppendEvent = true
	return transition
}

func exactP0Ready(f RunReconcileFacts) bool {
	return f.P0Expected > 0 && f.P0Successful == f.P0Expected && f.P0Active == 0 && f.P0ScoreHeadsReady
}

func (r *evaluationRepository) ReconcileEvaluationRun(ctx context.Context, runID uuid.UUID) (RunRecord, error) {
	if r == nil || r.db == nil {
		return RunRecord{}, errors.New("nil evaluation repository")
	}
	if runID == uuid.Nil {
		return RunRecord{}, errors.New("evaluation run id is required")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "system")
	if err != nil {
		return RunRecord{}, fmt.Errorf("begin evaluation run reconcile: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var record RunRecord
	var started sql.NullTime
	var status string
	var budgetMode string
	if err := tx.QueryRowContext(ctx, `
		SELECT id, status, control_epoch, state_version, started_at, budget_mode
		FROM evaluation_runs WHERE id = $1 FOR UPDATE`, runID).Scan(
		&record.ID, &status, &record.ControlEpoch, &record.StateVersion, &started, &budgetMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunRecord{}, fmt.Errorf("evaluation run %s not found", runID)
		}
		return RunRecord{}, fmt.Errorf("lock evaluation run: %w", err)
	}
	record.Status = service.RunStatus(status)
	if record.Status == service.RunStatusCompleted || record.Status == service.RunStatusFailed || record.Status == service.RunStatusCancelled {
		if err := tx.Commit(); err != nil {
			return RunRecord{}, fmt.Errorf("commit terminal evaluation run reconcile: %w", err)
		}
		return record, nil
	}

	facts, err := loadRunReconcileFacts(ctx, tx, runID, RunReconcileFacts{Status: status, Started: started.Valid, BudgetMode: budgetMode})
	if err != nil {
		return RunRecord{}, err
	}
	transition := decideRunTransition(facts)
	if transition.ReadinessRecorded {
		if err := recordRunReadiness(ctx, tx, runID, record.ControlEpoch, facts); err != nil {
			return RunRecord{}, err
		}
	}
	if !transition.Changed {
		if err := tx.Commit(); err != nil {
			return RunRecord{}, fmt.Errorf("commit evaluation run reconcile no-op: %w", err)
		}
		return record, nil
	}

	newEpoch := record.ControlEpoch
	if transition.Fence {
		newEpoch++
		if err := fenceRunWork(ctx, tx, runID, transition.ToStatus, newEpoch); err != nil {
			return RunRecord{}, err
		}
	}
	newVersion := record.StateVersion + 1
	idempotencyKey := runTransitionIdempotencyKey(runID, newVersion, record.Status, transition.ToStatus, newEpoch)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_run_events
			(id, run_id, event_type, payload, actor_type, transition_version, from_status, to_status, control_epoch, idempotency_key)
		VALUES ($1, $2, $3, jsonb_build_object('reason', $4::text), 'system', $5, $6, $7, $8, $9)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		uuid.New(), runID, "run_reconciled", transition.Reason, newVersion,
		record.Status, transition.ToStatus, newEpoch, idempotencyKey); err != nil {
		return RunRecord{}, fmt.Errorf("record evaluation run transition: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_runs
		SET status = $2::varchar, control_epoch = $3, state_version = $4,
			finished_at = CASE WHEN $2::varchar IN ('completed', 'failed') THEN NOW() ELSE finished_at END,
			updated_at = NOW()
		WHERE id = $1`, runID, transition.ToStatus, newEpoch, newVersion); err != nil {
		return RunRecord{}, fmt.Errorf("apply evaluation run transition: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RunRecord{}, fmt.Errorf("commit evaluation run transition: %w", err)
	}
	record.Status = transition.ToStatus
	record.ControlEpoch = newEpoch
	record.StateVersion = newVersion
	return record, nil
}

func loadRunReconcileFacts(ctx context.Context, tx *sql.Tx, runID uuid.UUID, facts RunReconcileFacts) (RunReconcileFacts, error) {
	var p0Expected, p0Successful, p0Active, pending int
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE priority = 'P0'),
			COUNT(*) FILTER (WHERE priority = 'P0' AND status = 'completed'),
			COUNT(*) FILTER (WHERE priority = 'P0' AND status NOT IN ('completed','cancelled','infra_failed','upstream_failed','invalid_evidence','grading_failed')),
			COUNT(*) FILTER (WHERE status IN ('pending','leased','running','evidence_uploaded','grading'))
		FROM evaluation_samples WHERE run_id = $1`, runID).Scan(&p0Expected, &p0Successful, &p0Active, &pending); err != nil {
		return facts, fmt.Errorf("load evaluation run work counts: %w", err)
	}
	var pendingPipeline int
	if err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM evaluation_grading_jobs
			 WHERE run_id=$1 AND status NOT IN ('completed','cancelled','failed')) +
			(SELECT COUNT(*) FROM evaluation_analysis_jobs
			 WHERE run_id=$1 AND status NOT IN ('completed','cancelled','failed')) +
			(SELECT COUNT(*) FROM evaluation_outbox_events
			 WHERE run_id=$1 AND COALESCE(work_origin,'initial')='initial'
			   AND status IN ('pending','leased'))`, runID).Scan(&pendingPipeline); err != nil {
		return facts, fmt.Errorf("load evaluation run pipeline work count: %w", err)
	}
	pending += pendingPipeline
	facts.P0Expected, facts.P0Successful, facts.P0Active, facts.PendingWork = p0Expected, p0Successful, p0Active, pending
	if p0Expected == 0 {
		facts.P0ScoreHeadsReady = true
	} else if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) = $2
		FROM evaluation_samples s
		JOIN evaluation_assignments a ON a.sample_id = s.id
		JOIN evaluation_score_heads h ON h.sample_id = s.id
		WHERE s.run_id = $1 AND s.priority = 'P0' AND s.status = 'completed'
		  AND a.attempt = (SELECT MAX(a2.attempt) FROM evaluation_assignments a2 WHERE a2.sample_id = s.id)
		  AND a.status = 'completed'`, runID, p0Expected).Scan(&facts.P0ScoreHeadsReady); err != nil {
		return facts, fmt.Errorf("load P0 score head readiness: %w", err)
	}

	var currentAggregateCount, expectedAggregateCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT c.capability_domain || ':' || regexp_replace(s.model_route, '^(baseline|candidate):', '')),
			COUNT(DISTINCT c.capability_domain || ':' || regexp_replace(s.model_route, '^(baseline|candidate):', '')) FILTER (WHERE EXISTS (
				SELECT 1
				FROM evaluation_aggregate_heads current_aggregate
				WHERE current_aggregate.run_id = s.run_id
				  AND current_aggregate.capability_domain = c.capability_domain
				  AND current_aggregate.canonical_model_route = regexp_replace(s.model_route, '^(baseline|candidate):', '')
				  AND current_aggregate.analysis_version = 'v1'
			))
		FROM evaluation_samples s
		JOIN evaluation_cases c ON c.id = s.case_id
		WHERE s.run_id = $1 AND s.status = 'completed'`, runID).Scan(&expectedAggregateCount, &currentAggregateCount); err != nil {
		return facts, fmt.Errorf("load evaluation aggregate coverage: %w", err)
	}
	facts.CurrentCoverageOK = expectedAggregateCount > 0 && currentAggregateCount >= expectedAggregateCount

	var cause FailureCause
	var failureFound bool
	err := tx.QueryRowContext(ctx, `
		SELECT s.id, a.id, COALESCE(s.failure_class, a.failure_class, ''), COALESCE(s.failure_code, a.failure_code, '')
		FROM evaluation_samples s
		JOIN evaluation_assignments a ON a.sample_id = s.id
		WHERE s.run_id = $1
		  AND a.attempt = (SELECT MAX(a2.attempt) FROM evaluation_assignments a2 WHERE a2.sample_id = s.id)
		  AND a.status IN ('infra_failed','upstream_failed','invalid_evidence','grading_failed')
		  AND NOT EXISTS (
			SELECT 1 FROM evaluation_assignments replacement
			WHERE replacement.sample_id = s.id AND replacement.attempt > a.attempt
			  AND replacement.status NOT IN ('cancelled','infra_failed','upstream_failed','invalid_evidence','grading_failed')
		  )
		ORDER BY s.priority, s.id LIMIT 1`, runID).Scan(&cause.SampleID, &cause.AssignmentID, &cause.Class, &cause.Code)
	if err == nil {
		failureFound = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return facts, fmt.Errorf("load evaluation run failure: %w", err)
	}
	if failureFound {
		facts.UnrecoverableFailure = &cause
	}
	return facts, nil
}

func fenceRunWork(ctx context.Context, tx *sql.Tx, runID uuid.UUID, terminal service.RunStatus, epoch int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_assignments a SET status = CASE WHEN $2 = 'failed' THEN 'infra_failed' ELSE 'cancelled' END,
			lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL, heartbeat_at = NULL,
			finished_at = NOW(), updated_at = NOW()
		FROM evaluation_samples s
		WHERE a.sample_id = s.id AND s.run_id = $1
		  AND a.status NOT IN ('completed','cancelled','infra_failed','upstream_failed','invalid_evidence','grading_failed')`, runID, terminal); err != nil {
		return fmt.Errorf("fence evaluation assignments: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_samples SET status = CASE WHEN $2 = 'failed' THEN 'infra_failed' ELSE 'cancelled' END,
			updated_at = NOW()
		WHERE run_id = $1 AND status NOT IN ('completed','cancelled','infra_failed','upstream_failed','invalid_evidence','grading_failed')`, runID, terminal); err != nil {
		return fmt.Errorf("fence evaluation samples: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs SET status = CASE WHEN $2 = 'failed' THEN 'failed' ELSE 'cancelled' END,
			lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE run_id = $1 AND status NOT IN ('completed','cancelled','failed')`, runID, terminal); err != nil {
		return fmt.Errorf("fence evaluation grading jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_analysis_jobs SET status = CASE WHEN $2 = 'failed' THEN 'failed' ELSE 'cancelled' END,
			lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE run_id = $1 AND status NOT IN ('completed','cancelled','failed')`, runID, terminal); err != nil {
		return fmt.Errorf("fence evaluation analysis jobs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_route_evidence_terminalization_outbox
			(id, run_id, terminal_status, control_epoch, idempotency_key, payload)
		VALUES ($1, $2, $3, $4, $5, jsonb_build_object('run_id', $2::uuid, 'terminal_status', $3::varchar, 'control_epoch', $4::bigint))
		ON CONFLICT (idempotency_key) DO NOTHING`,
		uuid.New(), runID, terminal, epoch,
		hashReconcileKey(fmt.Sprintf("evidence-terminalization:%s:%s:%d", runID, terminal, epoch))); err != nil {
		return fmt.Errorf("enqueue route evidence terminalization: %w", err)
	}
	return nil
}

func recordRunReadiness(ctx context.Context, tx *sql.Tx, runID uuid.UUID, epoch int64, facts RunReconcileFacts) error {
	key := runReadinessIdempotencyKey(runID, facts)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_run_events
			(id, run_id, event_type, payload, actor_type, control_epoch, idempotency_key)
		VALUES ($1, $2, 'run_readiness', jsonb_build_object('p0_expected', $3, 'p0_successful', $4, 'p0_active', $5), 'system', $6, $7)
		ON CONFLICT (idempotency_key) DO NOTHING`, uuid.New(), runID, facts.P0Expected, facts.P0Successful, facts.P0Active, epoch, key)
	if err != nil {
		return fmt.Errorf("record evaluation run readiness: %w", err)
	}
	return nil
}

func runTransitionIdempotencyKey(runID uuid.UUID, version int64, from, to service.RunStatus, epoch int64) string {
	return hashReconcileKey(fmt.Sprintf("transition:%s:%d:%s:%s:%d", runID, version, from, to, epoch))
}

func runReadinessIdempotencyKey(runID uuid.UUID, facts RunReconcileFacts) string {
	return hashReconcileKey(fmt.Sprintf("readiness:%s:%d:%d:%d:%t", runID, facts.P0Expected, facts.P0Successful, facts.P0Active, facts.P0ScoreHeadsReady))
}

func hashReconcileKey(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])
}
