//go:build integration

package repository

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestEvaluationRunCreationIdempotency_ReplayAfterRepositoryRestart(t *testing.T) {
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	input := service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
		IdempotencyKey: "same-request", BaselineRef: map[string]any{"revision": "base"}}
	ctx := service.WithRadarTenant(context.Background(), fixture.userID)
	first, err := NewEvaluationRepository(integrationDB).CreateRunWithMatrix(ctx, input)
	require.NoError(t, err)
	// A fresh repository models losing all in-process state. Even a disabled
	// plan must replay the already accepted request rather than reserve again.
	_, err = integrationDB.Exec(`UPDATE evaluation_plans SET enabled=FALSE WHERE id=$1`, fixture.planID)
	require.NoError(t, err)
	second, err := NewEvaluationRepository(integrationDB).CreateRunWithMatrix(ctx, input)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.PairBindings, second.PairBindings)
	require.False(t, first.Replayed)
	require.True(t, second.Replayed)
	var runs, assignments, reservations int
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM evaluation_runs WHERE plan_id=$1`, fixture.planID).Scan(&runs))
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM evaluation_assignments a JOIN evaluation_samples s ON s.id=a.sample_id WHERE s.run_id=$1`, first.ID).Scan(&assignments))
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM evaluation_budget_ledger WHERE run_id=$1 AND entry_type='reservation'`, first.ID).Scan(&reservations))
	require.Equal(t, 1, runs)
	require.Equal(t, 2, assignments)
	require.Equal(t, 2, reservations)
}

func TestEvaluationRunCreationIdempotency_ResultWriteFailureRollsBackRunAndBudget(t *testing.T) {
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	input := service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID, IdempotencyKey: "persist-failure"}
	// Inject a real PostgreSQL failure after run/contracts/assignments/budget
	// have been inserted, at the final durable result write.
	name := "test_run_idem_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	scope := fmt.Sprintf("radar.run.create:tenant:%d:actor:%d", fixture.userID, fixture.userID)
	_, err := integrationDB.Exec(`CREATE FUNCTION ` + name + `() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected result write failure'; END $$`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := integrationDB.Exec(`DROP TRIGGER IF EXISTS ` + name + ` ON idempotency_records; DROP FUNCTION IF EXISTS ` + name + `()`)
		require.NoError(t, err)
	})
	_, err = integrationDB.Exec(`CREATE TRIGGER ` + name + ` BEFORE UPDATE ON idempotency_records FOR EACH ROW WHEN (NEW.scope = '` + scope + `') EXECUTE FUNCTION ` + name + `()`)
	require.NoError(t, err)
	repo := NewEvaluationRepository(integrationDB)
	_, err = repo.CreateRunWithMatrix(context.Background(), input)
	require.ErrorIs(t, err, service.ErrIdempotencyStoreUnavail)
	var runs, reservations, records int
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM evaluation_runs WHERE plan_id=$1`, fixture.planID).Scan(&runs))
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM evaluation_budget_ledger l JOIN evaluation_runs r ON r.id=l.run_id WHERE r.plan_id=$1`, fixture.planID).Scan(&reservations))
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM idempotency_records WHERE scope=$1`, scope).Scan(&records))
	require.Zero(t, runs)
	require.Zero(t, reservations)
	require.Zero(t, records)
	_, err = integrationDB.Exec(`DROP TRIGGER ` + name + ` ON idempotency_records`)
	require.NoError(t, err)
	first, err := repo.CreateRunWithMatrix(context.Background(), input)
	require.NoError(t, err)
	second, err := repo.CreateRunWithMatrix(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
}

func TestEvaluationRunCreationIdempotency_NoKeyPreservesIndependentCreations(t *testing.T) {
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	input := service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID}
	repo := NewEvaluationRepository(integrationDB)
	first, err := repo.CreateRunWithMatrix(context.Background(), input)
	require.NoError(t, err)
	second, err := repo.CreateRunWithMatrix(context.Background(), input)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
}

func TestEvaluationRunCreationIdempotency_RejectsChangedPayload(t *testing.T) {
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	input := service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID, IdempotencyKey: "payload-conflict"}
	repo := NewEvaluationRepository(integrationDB)
	_, err := repo.CreateRunWithMatrix(context.Background(), input)
	require.NoError(t, err)
	input.CandidateRef = map[string]any{"revision": "changed"}
	_, err = repo.CreateRunWithMatrix(context.Background(), input)
	require.ErrorIs(t, err, service.ErrIdempotencyKeyConflict)
	var runs int
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM evaluation_runs WHERE plan_id=$1`, fixture.planID).Scan(&runs))
	require.Equal(t, 1, runs)
}

func TestEvaluationRunCreationIdempotency_ConcurrentRequestsCreateOnce(t *testing.T) {
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	input := service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID, IdempotencyKey: "concurrent-request"}
	const count = 6
	runs := make([]*service.EvaluationRun, count)
	errs := make([]error, count)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range runs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			runs[i], errs[i] = NewEvaluationRepository(integrationDB).CreateRunWithMatrix(context.Background(), input)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range runs {
		require.NoError(t, errs[i])
		require.Equal(t, runs[0].ID, runs[i].ID)
	}
	var total int
	require.NoError(t, integrationDB.QueryRow(`SELECT COUNT(*) FROM evaluation_runs WHERE plan_id=$1`, fixture.planID).Scan(&total))
	require.Equal(t, 1, total)
}

func TestEvaluationRunCreationIdempotency_RollbackAllowsRetry(t *testing.T) {
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	input := service.CreateRunInput{PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID, IdempotencyKey: "rolled-back-request"}
	_, err := integrationDB.Exec(`UPDATE evaluation_plans SET enabled=FALSE WHERE id=$1`, fixture.planID)
	require.NoError(t, err)
	repo := NewEvaluationRepository(integrationDB)
	_, err = repo.CreateRunWithMatrix(context.Background(), input)
	require.Error(t, err)
	_, err = integrationDB.Exec(`UPDATE evaluation_plans SET enabled=TRUE WHERE id=$1`, fixture.planID)
	require.NoError(t, err)
	first, err := repo.CreateRunWithMatrix(context.Background(), input)
	require.NoError(t, err)
	second, err := repo.CreateRunWithMatrix(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
}

func TestEvaluationRunCreationIdempotency_TenantIsolation(t *testing.T) {
	a := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	b := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationRepository(integrationDB)
	input := service.CreateRunInput{PlanID: a.planID, TriggerSource: "manual", CreatedBy: a.userID, IdempotencyKey: "tenant-shared-key"}
	first, err := repo.CreateRunWithMatrix(service.WithRadarTenant(context.Background(), a.userID), input)
	require.NoError(t, err)
	_, err = repo.CreateRunWithMatrix(service.WithRadarTenant(context.Background(), b.userID), input)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	input.PlanID, input.CreatedBy = b.planID, b.userID
	second, err := repo.CreateRunWithMatrix(service.WithRadarTenant(context.Background(), b.userID), input)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
}
