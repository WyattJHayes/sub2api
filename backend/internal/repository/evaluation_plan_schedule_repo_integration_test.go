//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// seedScheduleFixture inserts a due cron plan directly so the claim semantics
// can be exercised without the governance API.
func seedScheduleFixture(t *testing.T, cronExpression string, baseline, candidate any) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	suffix := uuid.NewString()
	datasetID := uuid.New()
	planID := uuid.New()

	var tenantID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO users (email, username, password_hash, role, status, created_at, updated_at)
		 VALUES ($1, $2, 'x', 'admin', 'active', NOW(), NOW()) RETURNING id`,
		suffix+"@sched.test", "sched-"+suffix[:8]).Scan(&tenantID))

	// A published dataset version requires published_at, and a plan requires a
	// real dedicated evaluation key so the gateway key foreign key holds.
	_, err := integrationDB.ExecContext(ctx,
		`INSERT INTO evaluation_dataset_versions (id, dataset_key, version, manifest_sha256, source_type, status, published_at, created_by, tenant_id, created_at, updated_at)
		 VALUES ($1, 'sched', $2, $3, 'synthetic', 'published', NOW(), $4, $4, NOW(), NOW())`,
		datasetID, "v-"+suffix[:8], suffix, tenantID)
	require.NoError(t, err)

	var keyID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`INSERT INTO api_keys (user_id, key, name, status, is_evaluation, created_at, updated_at)
		 VALUES ($1, $2, 'sched-eval', 'active', TRUE, NOW(), NOW()) RETURNING id`,
		tenantID, "sk-sched-"+suffix[:24]).Scan(&keyID))

	_, err = integrationDB.ExecContext(ctx,
		`INSERT INTO evaluation_plans (id, name, dataset_version_id, gateway_api_key_id, trigger_type,
		   cron_expression, model_matrix, max_run_cost, daily_cost_limit, max_concurrency, enabled,
		   baseline_ref, candidate_ref, next_run_at, created_by, tenant_id, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'cron', $5, $6::jsonb, 2, 2, 1, TRUE, $7::jsonb, $8::jsonb, NOW() - interval '1 minute', $9, $9, NOW(), NOW())`,
		planID, "sched-"+suffix[:8], datasetID, keyID, cronExpression,
		`[{"route":"r","baseline":{"route":"a"},"candidate":{"route":"b"}}]`,
		nullableScheduleJSON(t, baseline), nullableScheduleJSON(t, candidate), tenantID)
	require.NoError(t, err)
	return planID
}

// nullableScheduleJSON encodes an optional reference. A nil reference must be
// inserted as SQL NULL rather than the JSON literal null, because the runner
// treats a missing reference as "skip this plan".
func nullableScheduleJSON(t *testing.T, value any) any {
	t.Helper()
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func TestEvaluationPlanScheduleMigrationAddsScheduleColumns(t *testing.T) {
	ctx := context.Background()
	for _, column := range []string{"next_run_at", "last_run_at", "schedule_lease_token", "schedule_lease_expires_at", "baseline_ref", "candidate_ref"} {
		var dataType sql.NullString
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			`SELECT data_type FROM information_schema.columns WHERE table_schema='public' AND table_name='evaluation_plans' AND column_name=$1`,
			column).Scan(&dataType))
		require.True(t, dataType.Valid, "evaluation_plans.%s must exist after migration 240", column)
	}
	var indexName sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT indexname FROM pg_indexes WHERE schemaname='public' AND tablename='evaluation_plans' AND indexname='idx_evaluation_plans_schedule_due'`).Scan(&indexName))
	require.True(t, indexName.Valid, "due-plan index must exist")
}

func TestClaimDueScheduledPlansIsExclusiveAcrossConcurrentClaimants(t *testing.T) {
	planID := seedScheduleFixture(t, "0 9 * * *", map[string]any{"release": "v0.2.7-sol"}, map[string]any{"release": "v0.2.7-4models"})
	ctx := context.Background()
	repo := NewEvaluationPlanScheduleRepository(integrationDB)

	const claimers = 4
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		total int
	)
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claims, err := repo.ClaimDueScheduledPlans(ctx, time.Now(), 16, 5*time.Minute)
			require.NoError(t, err)
			for _, claim := range claims {
				if claim.PlanID == planID {
					mu.Lock()
					total++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, total, "exactly one claimant may lease a due plan")

	var leased int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT count(*) FROM evaluation_plans WHERE id=$1 AND schedule_lease_token IS NOT NULL AND schedule_lease_expires_at > NOW()`, planID).Scan(&leased))
	require.Equal(t, 1, leased)
}

func TestExpiredScheduleLeaseIsReclaimable(t *testing.T) {
	planID := seedScheduleFixture(t, "0 9 * * *", map[string]any{"release": "a"}, map[string]any{"release": "b"})
	ctx := context.Background()
	repo := NewEvaluationPlanScheduleRepository(integrationDB)

	first, err := repo.ClaimDueScheduledPlans(ctx, time.Now(), 16, 5*time.Minute)
	require.NoError(t, err)
	var firstToken string
	for _, claim := range first {
		if claim.PlanID == planID {
			firstToken = claim.LeaseToken
		}
	}
	require.NotEmpty(t, firstToken)

	// A live lease must not be reclaimed.
	second, err := repo.ClaimDueScheduledPlans(ctx, time.Now(), 16, 5*time.Minute)
	require.NoError(t, err)
	for _, claim := range second {
		require.NotEqual(t, planID, claim.PlanID, "a live lease must not be reclaimed")
	}

	// Once expired the plan is claimable again with a different token.
	_, err = integrationDB.ExecContext(ctx,
		`UPDATE evaluation_plans SET schedule_lease_expires_at = NOW() - interval '1 second' WHERE id=$1`, planID)
	require.NoError(t, err)
	third, err := repo.ClaimDueScheduledPlans(ctx, time.Now(), 16, 5*time.Minute)
	require.NoError(t, err)
	var thirdToken string
	for _, claim := range third {
		if claim.PlanID == planID {
			thirdToken = claim.LeaseToken
		}
	}
	require.NotEmpty(t, thirdToken, "an expired lease must be reclaimable")
	require.NotEqual(t, firstToken, thirdToken)
}

func TestCompleteScheduledPlanClaimRejectsStaleTokenAndAdvancesSchedule(t *testing.T) {
	planID := seedScheduleFixture(t, "0 9 * * *", map[string]any{"release": "a"}, map[string]any{"release": "b"})
	ctx := context.Background()
	repo := NewEvaluationPlanScheduleRepository(integrationDB)

	claims, err := repo.ClaimDueScheduledPlans(ctx, time.Now(), 16, 5*time.Minute)
	require.NoError(t, err)
	var token string
	for _, claim := range claims {
		if claim.PlanID == planID {
			token = claim.LeaseToken
		}
	}
	require.NotEmpty(t, token)

	err = repo.CompleteScheduledPlanClaim(ctx, planID, uuid.NewString(), time.Now(), time.Now().Add(24*time.Hour))
	require.ErrorIs(t, err, service.ErrEvaluationPlanScheduleFenced, "a stale token must not advance the schedule")

	ranAt := time.Now()
	nextRunAt := ranAt.Add(24 * time.Hour)
	require.NoError(t, repo.CompleteScheduledPlanClaim(ctx, planID, token, ranAt, nextRunAt))

	var (
		leaseToken sql.NullString
		storedNext sql.NullTime
	)
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT schedule_lease_token, next_run_at FROM evaluation_plans WHERE id=$1`, planID).Scan(&leaseToken, &storedNext))
	require.False(t, leaseToken.Valid, "completion must release the lease")
	require.True(t, storedNext.Valid)
	require.WithinDuration(t, nextRunAt, storedNext.Time, time.Second)
}

func TestScheduledPlanWithMissingReferenceRemainsClaimableButReferenceLess(t *testing.T) {
	planID := seedScheduleFixture(t, "0 9 * * *", nil, map[string]any{"release": "b"})
	ctx := context.Background()
	repo := NewEvaluationPlanScheduleRepository(integrationDB)

	claims, err := repo.ClaimDueScheduledPlans(ctx, time.Now(), 16, 5*time.Minute)
	require.NoError(t, err)
	var found *service.ScheduledEvaluationPlanClaim
	for i := range claims {
		if claims[i].PlanID == planID {
			found = &claims[i]
		}
	}
	require.NotNil(t, found, "a plan with a missing reference is still claimable so the runner can skip and advance it")
	require.Empty(t, found.BaselineRef)
	require.NotEmpty(t, found.CandidateRef)
}

func TestManualPlanIsNeverClaimedBySchedule(t *testing.T) {
	ctx := context.Background()
	planID := seedScheduleFixture(t, "0 9 * * *", map[string]any{"release": "a"}, map[string]any{"release": "b"})

	// Flip this plan to manual so it must no longer be claimable.
	_, err := integrationDB.ExecContext(ctx,
		`UPDATE evaluation_plans SET trigger_type='manual', cron_expression=NULL, next_run_at=NULL WHERE id=$1`, planID)
	require.NoError(t, err)

	repo := NewEvaluationPlanScheduleRepository(integrationDB)
	claims, err := repo.ClaimDueScheduledPlans(ctx, time.Now(), 64, time.Minute)
	require.NoError(t, err)
	for _, claim := range claims {
		require.NotEqual(t, planID, claim.PlanID, "a manual plan must never be claimed by the scheduler")
		require.NotEmpty(t, claim.CronExpression, "a cron claim must always carry its expression")
	}
}
