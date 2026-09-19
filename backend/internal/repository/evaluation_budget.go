package repository

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Reserve today's admitted runs and account for recorded charges, without
// adding a run's reservation to its charges twice. Old runs' requests issued
// today also count. This remains a soft guard: billing may arrive after a lease.
const evaluationDailyCommittedCostSQL = `COALESCE((
	SELECT SUM(GREATEST(
		CASE WHEN existing.created_at >= date_trunc('day', NOW()) THEN existing.reserved_cost ELSE 0 END,
		COALESCE((SELECT SUM(GREATEST(e.billed_amount, 0)) FROM evaluation_route_evidence e
			WHERE e.evaluation_run_id=existing.id AND e.started_at >= date_trunc('day', NOW())), 0)
	)) FROM evaluation_runs existing WHERE existing.plan_id=p.id
), 0)`

type evaluationBudgetUsage struct {
	runCost, dailyCost, dailyLimit decimal.Decimal
}

func loadEvaluationBudgetUsage(ctx context.Context, tx *sql.Tx, runID, planID uuid.UUID) (evaluationBudgetUsage, error) {
	var usage evaluationBudgetUsage
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT SUM(GREATEST(e.billed_amount, 0)) FROM evaluation_route_evidence e
			WHERE e.evaluation_run_id=$1), 0),
			COALESCE((SELECT SUM(GREATEST(e.billed_amount, 0)) FROM evaluation_route_evidence e
			JOIN evaluation_runs existing ON existing.id=e.evaluation_run_id
			WHERE existing.plan_id=p.id AND e.started_at >= date_trunc('day', NOW())), 0),
			p.daily_cost_limit
		FROM evaluation_plans p WHERE p.id=$2`, runID, planID).Scan(&usage.runCost, &usage.dailyCost, &usage.dailyLimit)
	if err != nil {
		return usage, fmt.Errorf("read evaluation recorded budget usage: %w", err)
	}
	return usage, nil
}

func pauseEvaluationRunForBudget(ctx context.Context, tx *sql.Tx, runID uuid.UUID, status service.RunStatus, usage evaluationBudgetUsage) error {
	var epoch, stateVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT control_epoch, state_version FROM evaluation_runs WHERE id=$1`, runID).Scan(&epoch, &stateVersion); err != nil {
		return fmt.Errorf("read evaluation budget pause version: %w", err)
	}
	// The caller already holds the run/plan locks. Preserve in-flight work and
	// epoch, and use the existing audited status transition protocol.
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_runs SET status='paused', paused_from_status=$2,
		pause_reason='budget', state_version=state_version+1, updated_at=NOW() WHERE id=$1`, runID, status); err != nil {
		return fmt.Errorf("pause evaluation run for recorded budget: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO evaluation_run_events
		(id, run_id, event_type, payload, actor_type, transition_version, from_status, to_status, control_epoch, idempotency_key)
		VALUES ($1, $2, 'budget_actual_paused', jsonb_build_object(
			'reason', 'budget', 'recorded_cost', $3::text, 'daily_recorded_cost', $4::text,
			'daily_limit', $5::text), 'system', $6, $7, 'paused', $8, $9)`,
		uuid.New(), runID, usage.runCost, usage.dailyCost, usage.dailyLimit, stateVersion+1, status, epoch,
		runTransitionIdempotencyKey(runID, stateVersion+1, status, service.RunStatusPaused, epoch)); err != nil {
		return fmt.Errorf("record evaluation budget pause event: %w", err)
	}
	return nil
}
