//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestEvaluationRecordedBudgetPausesEnforcedRunAndPreservesInflightLease(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixtureWithCases(t, []evaluationCaseFixtureSpec{{
		capability: "coding", priority: "P0", sampleCount: 1, estimatedCost: decimal.RequireFromString("0.0001"),
	}}, []map[string]any{{"route": "route-a"}}, decimal.NewFromInt(2))
	repo := NewEvaluationRepository(integrationDB)
	run, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID})
	require.NoError(t, err)
	first, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, first)
	var beforeLeaseStatus, beforeLeaseTokenHash string
	var beforeLeasedBy uuid.UUID
	var beforeLeaseExpiresAt time.Time
	var beforeLeaseEpoch int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status, lease_token_hash, leased_by,
		lease_expires_at, lease_epoch FROM evaluation_assignments WHERE id=$1`, first.ID).Scan(
		&beforeLeaseStatus, &beforeLeaseTokenHash, &beforeLeasedBy, &beforeLeaseExpiresAt, &beforeLeaseEpoch,
	))
	require.Equal(t, "leased", beforeLeaseStatus)
	require.NotEmpty(t, beforeLeaseTokenHash)
	require.Equal(t, fixture.workerIDs[0], beforeLeasedBy)
	require.Equal(t, first.LeaseEpoch, beforeLeaseEpoch)

	insertEvaluationBudgetCharge(t, run.ID, first.SampleID, fixture.apiKeyID, "2", time.Now().UTC())
	var originalGuardMode string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT guard_mode FROM evaluation_schema_cutovers WHERE id=1`).Scan(&originalGuardMode))
	t.Cleanup(func() {
		result, restoreErr := integrationDB.ExecContext(context.Background(),
			`UPDATE evaluation_schema_cutovers SET guard_mode=$1, updated_at=NOW() WHERE id=1`, originalGuardMode)
		require.NoError(t, restoreErr)
		restored, rowsErr := result.RowsAffected()
		require.NoError(t, rowsErr)
		require.Equal(t, int64(1), restored)
	})
	result, err := integrationDB.ExecContext(ctx,
		`UPDATE evaluation_schema_cutovers SET guard_mode='enforce', updated_at=NOW() WHERE id=1`)
	require.NoError(t, err)
	updated, err := result.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), updated)

	second, err := repo.ClaimAssignment(ctx, fixture.workerIDs[1], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.Nil(t, second)
	var status, reason, fromStatus string
	var epoch, version int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status, pause_reason, paused_from_status, control_epoch, state_version FROM evaluation_runs WHERE id=$1`, run.ID).
		Scan(&status, &reason, &fromStatus, &epoch, &version))
	require.Equal(t, "paused", status)
	require.Equal(t, "budget", reason)
	require.Equal(t, "running", fromStatus)
	require.Equal(t, first.LeaseEpoch, epoch)
	var matchingEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluation_run_events WHERE run_id=$1 AND event_type='budget_actual_paused'
		AND from_status='running' AND to_status='paused' AND control_epoch=$2 AND transition_version=$3`, run.ID, epoch, version).Scan(&matchingEvents))
	require.Equal(t, 1, matchingEvents)
	var afterLeaseStatus, afterLeaseTokenHash string
	var afterLeasedBy uuid.UUID
	var afterLeaseExpiresAt time.Time
	var afterLeaseEpoch int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status, lease_token_hash, leased_by,
		lease_expires_at, lease_epoch FROM evaluation_assignments WHERE id=$1`, first.ID).Scan(
		&afterLeaseStatus, &afterLeaseTokenHash, &afterLeasedBy, &afterLeaseExpiresAt, &afterLeaseEpoch,
	))
	require.Equal(t, beforeLeaseStatus, afterLeaseStatus)
	require.Equal(t, beforeLeaseTokenHash, afterLeaseTokenHash)
	require.Equal(t, beforeLeasedBy, afterLeasedBy)
	require.True(t, beforeLeaseExpiresAt.Equal(afterLeaseExpiresAt))
	require.Equal(t, beforeLeaseEpoch, afterLeaseEpoch)
	require.Equal(t, epoch, afterLeaseEpoch)
	projections, err := (&radarGovernanceRepository{db: integrationDB}).ListRuns(service.WithRadarTenant(ctx, fixture.userID))
	require.NoError(t, err)
	require.Len(t, projections, 1)
	require.NotNil(t, projections[0].ActualCost)
	require.Equal(t, "2", projections[0].ActualCost.String())
	require.Equal(t, 1, projections[0].EvidenceCount)
	require.Equal(t, 1, projections[0].BilledEvidenceCount)
}

func TestEvaluationDailyCommittedCostIncludesOldRunRequestsWithoutDoubleReservation(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationRepository(integrationDB)
	oldRun, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID})
	require.NoError(t, err)
	todayRun, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID})
	require.NoError(t, err)
	tx := testTx(t)
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `UPDATE evaluation_runs SET created_at=date_trunc('day', NOW())-INTERVAL '1 day' WHERE id=$1`, oldRun.ID))
	require.NoError(t, tx.Commit())
	var oldSample, todaySample uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT id FROM evaluation_samples WHERE run_id=$1 LIMIT 1`, oldRun.ID).Scan(&oldSample))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT id FROM evaluation_samples WHERE run_id=$1 LIMIT 1`, todayRun.ID).Scan(&todaySample))
	insertEvaluationBudgetCharge(t, oldRun.ID, oldSample, fixture.apiKeyID, "0.5", time.Now().UTC())
	insertEvaluationBudgetCharge(t, oldRun.ID, oldSample, fixture.apiKeyID, "9", time.Now().UTC().Add(-48*time.Hour))
	insertEvaluationBudgetCharge(t, todayRun.ID, todaySample, fixture.apiKeyID, "0.1", time.Now().UTC())
	var committed decimal.Decimal
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT `+evaluationDailyCommittedCostSQL+` FROM evaluation_plans p WHERE p.id=$1`, fixture.planID).Scan(&committed))
	require.Equal(t, "0.6", committed.String(), "old reservation is excluded and today's 0.02 reservation is not added to its 0.1 charge")
	readTx := testTx(t)
	usage, err := loadEvaluationBudgetUsage(ctx, readTx, todayRun.ID, fixture.planID)
	require.NoError(t, err)
	require.Equal(t, "0.1", usage.runCost.String())
	require.Equal(t, "0.6", usage.dailyCost.String())
}

func insertEvaluationBudgetCharge(t *testing.T, runID, sampleID uuid.UUID, keyID int64, amount string, startedAt time.Time) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), `INSERT INTO evaluation_route_evidence
		(route_trace_id, evaluation_run_id, sample_id, api_key_id, request_id, requested_model, route_profile_version, region, transport_status, started_at, billed_amount)
		VALUES ($1,$2,$3,$4,$5,'route-a','route-v1','default','succeeded',$6,$7)`, uuid.NewString(), runID, sampleID, keyID, uuid.NewString(), startedAt, amount)
	require.NoError(t, err)
}
