package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

// evaluationPlanScheduleRepo owns the cron-plan claim lifecycle. Claiming uses
// SELECT ... FOR UPDATE SKIP LOCKED plus an atomically advanced next_run_at, so
// concurrent instances (and restarted processes) cannot start the same
// scheduled instant twice.
type evaluationPlanScheduleRepo struct {
	db *sql.DB
}

var _ service.EvaluationPlanScheduleStore = (*evaluationPlanScheduleRepo)(nil)

func NewEvaluationPlanScheduleRepository(db *sql.DB) service.EvaluationPlanScheduleStore {
	return &evaluationPlanScheduleRepo{db: db}
}

func (r *evaluationPlanScheduleRepo) ClaimDueScheduledPlans(
	ctx context.Context,
	now time.Time,
	limit int,
	leaseTTL time.Duration,
) ([]service.ScheduledEvaluationPlanClaim, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation plan schedule repository")
	}
	if limit <= 0 {
		return nil, nil
	}
	if leaseTTL <= 0 {
		leaseTTL = 5 * time.Minute
	}

	claims := make([]service.ScheduledEvaluationPlanClaim, 0, limit)
	err := withEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("schedule"), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT id, cron_expression, baseline_ref::text, candidate_ref::text, created_by, tenant_id
FROM evaluation_plans
WHERE trigger_type = 'cron'
  AND enabled
  AND next_run_at IS NOT NULL
  AND next_run_at <= $1
  AND (schedule_lease_expires_at IS NULL OR schedule_lease_expires_at <= $1)
ORDER BY next_run_at, id
LIMIT $2
FOR UPDATE SKIP LOCKED`, now, limit)
		if err != nil {
			return fmt.Errorf("select due evaluation plans: %w", err)
		}

		type pendingClaim struct {
			planID      uuid.UUID
			cron        sql.NullString
			baseline    sql.NullString
			candidate   sql.NullString
			createdBy   sql.NullInt64
			tenantID    sql.NullInt64
			leaseToken  string
			scheduledAt time.Time
		}
		pending := make([]pendingClaim, 0, limit)
		for rows.Next() {
			var item pendingClaim
			if err := rows.Scan(&item.planID, &item.cron, &item.baseline, &item.candidate, &item.createdBy, &item.tenantID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan due evaluation plan: %w", err)
			}
			item.leaseToken = uuid.NewString()
			item.scheduledAt = now
			pending = append(pending, item)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate due evaluation plans: %w", err)
		}
		_ = rows.Close()

		for _, item := range pending {
			if _, err := tx.ExecContext(ctx, `
UPDATE evaluation_plans
SET schedule_lease_token = $2,
    schedule_lease_expires_at = $3,
    last_run_at = $4,
    updated_at = NOW()
WHERE id = $1`, item.planID, item.leaseToken, now.Add(leaseTTL), now); err != nil {
				return fmt.Errorf("lease evaluation plan %s: %w", item.planID, err)
			}
			claims = append(claims, service.ScheduledEvaluationPlanClaim{
				PlanID:         item.planID,
				CronExpression: item.cron.String,
				BaselineRef:    rawJSONOrNil(item.baseline),
				CandidateRef:   rawJSONOrNil(item.candidate),
				CreatedBy:      item.createdBy.Int64,
				TenantID:       item.tenantID.Int64,
				ScheduledFor:   item.scheduledAt,
				LeaseToken:     item.leaseToken,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (r *evaluationPlanScheduleRepo) CompleteScheduledPlanClaim(
	ctx context.Context,
	planID uuid.UUID,
	leaseToken string,
	ranAt time.Time,
	nextRunAt time.Time,
) error {
	if r == nil || r.db == nil {
		return errors.New("nil evaluation plan schedule repository")
	}
	if planID == uuid.Nil || leaseToken == "" {
		return errors.New("evaluation plan schedule completion requires plan id and lease token")
	}
	return withEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("schedule"), func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
UPDATE evaluation_plans
SET next_run_at = $3,
    last_run_at = $4,
    schedule_lease_token = NULL,
    schedule_lease_expires_at = NULL,
    updated_at = NOW()
WHERE id = $1
  AND schedule_lease_token = $2`, planID, leaseToken, nextRunAt, ranAt)
		if err != nil {
			return fmt.Errorf("complete evaluation plan schedule claim: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("read evaluation plan schedule completion result: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("%w: evaluation plan %s schedule lease is no longer current", service.ErrEvaluationPlanScheduleFenced, planID)
		}
		return nil
	})
}

func rawJSONOrNil(value sql.NullString) json.RawMessage {
	if !value.Valid || value.String == "" {
		return nil
	}
	return json.RawMessage(value.String)
}
