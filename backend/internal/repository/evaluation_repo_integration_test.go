//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestEvaluationRepository_ExpandsMatrixIntoSamplesAndAssignments(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 2, []string{"route-a", "route-b"}, 3)
	repo := NewEvaluationRepository(integrationDB)

	run, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID:        fixture.planID,
		TriggerSource: "manual",
		BaselineRef:   map[string]any{"revision": "baseline-1"},
		CandidateRef:  map[string]any{"revision": "candidate-1"},
		CreatedBy:     fixture.userID,
	})
	require.NoError(t, err)
	require.NotNil(t, run)

	var samples, assignments, reservations int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM evaluation_samples WHERE run_id = $1", run.ID).Scan(&samples))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		WHERE s.run_id = $1`, run.ID).Scan(&assignments))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_budget_ledger
		WHERE run_id = $1 AND entry_type = 'reservation'`, run.ID).Scan(&reservations))
	require.Equal(t, 24, samples)
	require.Equal(t, 24, assignments)
	require.Equal(t, 24, reservations)

	var caseID uuid.UUID
	var modelRoute string
	var sampleIndex, attempt int
	var idempotencyKey string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT s.case_id, s.model_route, s.sample_index, a.attempt, a.idempotency_key
		FROM evaluation_samples s
		JOIN evaluation_assignments a ON a.sample_id = s.id
		WHERE s.run_id = $1
		ORDER BY s.id
		LIMIT 1`, run.ID).Scan(&caseID, &modelRoute, &sampleIndex, &attempt, &idempotencyKey))
	expectedKey := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", run.ID, caseID, modelRoute, sampleIndex, attempt)))
	require.Equal(t, fmt.Sprintf("%x", expectedKey), idempotencyKey)
}

func TestEvaluationRepository_ConcurrentLeaseAndLeaseFencing(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a", "route-b"}, 1)
	repo := NewEvaluationRepository(integrationDB)
	run, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID:        fixture.planID,
		TriggerSource: "manual",
		BaselineRef:   map[string]any{"revision": "baseline-1"},
		CandidateRef:  map[string]any{"revision": "candidate-1"},
		CreatedBy:     fixture.userID,
	})
	require.NoError(t, err)

	workers := []uuid.UUID{fixture.workerIDs[0], fixture.workerIDs[1]}
	leases := make([]*service.AssignmentLease, len(workers))
	errs := make([]error, len(workers))
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			leases[index], errs[index] = repo.ClaimAssignment(ctx, workers[index], []string{"coding"}, time.Minute)
		}(i)
	}
	wg.Wait()
	for _, claimErr := range errs {
		require.NoError(t, claimErr)
	}
	require.NotNil(t, leases[0])
	require.NotNil(t, leases[1])
	require.NotEqual(t, leases[0].ID, leases[1].ID)

	old := leases[0]
	_, err = integrationDB.ExecContext(ctx,
		"UPDATE evaluation_assignments SET lease_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1", old.ID)
	require.NoError(t, err)

	fresh, err := repo.ClaimAssignment(ctx, fixture.workerIDs[2], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, fresh)
	require.NotEqual(t, old.ID, fresh.ID)
	require.Equal(t, old.SampleID, fresh.SampleID)
	require.Equal(t, 2, fresh.Attempt)

	_, err = repo.RenewLease(ctx, old.ID, old.Token, time.Minute)
	require.ErrorIs(t, err, service.ErrLeaseFenced)
	require.ErrorIs(t,
		repo.TransitionAssignment(ctx, service.AssignmentTransition{
			AssignmentID: old.ID,
			LeaseToken:   old.Token,
			To:           service.AssignmentCompleted,
		}),
		service.ErrLeaseFenced,
	)

	var attemptOne, attemptTwo int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE attempt = 1), COUNT(*) FILTER (WHERE attempt = 2)
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		WHERE s.run_id = $1 AND s.id = $2`, run.ID, old.SampleID).Scan(&attemptOne, &attemptTwo))
	require.Equal(t, 1, attemptOne)
	require.Equal(t, 1, attemptTwo)
}

func TestEvaluationRepository_EmptyCapabilitiesCannotClaim(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationRepository(integrationDB)
	run, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID:        fixture.planID,
		TriggerSource: "manual",
		BaselineRef:   map[string]any{"revision": "baseline-1"},
		CandidateRef:  map[string]any{"revision": "candidate-1"},
		CreatedBy:     fixture.userID,
	})
	require.NoError(t, err)

	lease, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], nil, time.Minute)
	require.NoError(t, err)
	require.Nil(t, lease)

	var leased int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		WHERE s.run_id = $1 AND a.status = 'leased'`, run.ID).Scan(&leased))
	require.Zero(t, leased)
}

type evaluationRepositoryFixture struct {
	userID    int64
	datasetID uuid.UUID
	planID    uuid.UUID
	workerIDs []uuid.UUID
}

func createEvaluationRepositoryFixture(t *testing.T, caseCount int, routes []string, sampleCount int) evaluationRepositoryFixture {
	t.Helper()
	ctx := context.Background()
	user := mustCreateUser(t, integrationEntClient, &service.User{Email: "evaluation-repository-" + uuid.NewString() + "@example.com"})
	datasetID := uuid.New()
	planID := uuid.New()

	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_dataset_versions (
			id, dataset_key, version, manifest_sha256, source_type, status, created_by
		) VALUES ($1, $2, $3, $4, 'synthetic', 'draft', $5)`,
		datasetID, "evaluation-repository-"+uuid.NewString(), "v1", fmt.Sprintf("%064d", 1), user.ID)
	require.NoError(t, err)
	for i := 0; i < caseCount; i++ {
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_cases (
				id, dataset_version_id, case_key, capability_domain, priority, weight, sample_count,
				prompt_spec, expected_spec, execution_spec, grader_id, grader_version,
				content_sha256, confidentiality, estimated_cost
			) VALUES (
				$1, $2, $3, 'coding', 'P0', 1, $4,
				'{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'grader', 'v1',
				$5, 'synthetic', 0.01
			)`, uuid.New(), datasetID, fmt.Sprintf("case-%d", i), sampleCount, fmt.Sprintf("%064d", i+10))
		require.NoError(t, err)
	}
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_dataset_versions SET status = 'published', published_at = NOW() WHERE id = $1`, datasetID)
	require.NoError(t, err)
	matrix := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		matrix = append(matrix, map[string]any{"route": route})
	}
	matrixJSON, err := json.Marshal(matrix)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_plans (
			id, name, dataset_version_id, trigger_type, model_matrix,
			max_run_cost, daily_cost_limit, max_concurrency, created_by
		) VALUES ($1, $2, $3, 'manual', $4::jsonb, $5, $6, 10, $7)`,
		planID, "evaluation-plan-"+uuid.NewString(), datasetID, matrixJSON,
		decimal.RequireFromString("100.00000000"), decimal.RequireFromString("100.00000000"), user.ID)
	require.NoError(t, err)

	workers := make([]uuid.UUID, 3)
	for i := range workers {
		workers[i] = uuid.New()
		tokenHash := sha256.Sum256([]byte(uuid.NewString()))
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_workers (id, name, worker_kind, token_hash, capabilities)
			VALUES ($1, $2, 'runner', $3, ARRAY['coding'])`,
			workers[i], "worker-"+uuid.NewString(), fmt.Sprintf("%x", tokenHash))
		require.NoError(t, err)
	}
	fixture := evaluationRepositoryFixture{
		userID: user.ID, datasetID: datasetID, planID: planID, workerIDs: workers,
	}
	t.Cleanup(func() {
		cleanupEvaluationRepositoryFixture(t, fixture)
	})
	return fixture
}

func cleanupEvaluationRepositoryFixture(t *testing.T, fixture evaluationRepositoryFixture) {
	t.Helper()
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)

	for _, statement := range []string{
		`DELETE FROM evaluation_budget_ledger WHERE run_id IN (SELECT id FROM evaluation_runs WHERE plan_id = $1)`,
		`DELETE FROM evaluation_artifacts WHERE run_id IN (SELECT id FROM evaluation_runs WHERE plan_id = $1)`,
		`DELETE FROM evaluation_assignments WHERE sample_id IN (
			SELECT s.id FROM evaluation_samples s
			JOIN evaluation_runs r ON r.id = s.run_id
			WHERE r.plan_id = $1
		)`,
		`DELETE FROM evaluation_samples WHERE run_id IN (SELECT id FROM evaluation_runs WHERE plan_id = $1)`,
		`DELETE FROM evaluation_run_events WHERE run_id IN (SELECT id FROM evaluation_runs WHERE plan_id = $1)`,
		`DELETE FROM evaluation_runs WHERE plan_id = $1`,
		`DELETE FROM evaluation_plans WHERE id = $1`,
	} {
		_, err = tx.ExecContext(ctx, statement, fixture.planID)
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM evaluation_cases WHERE dataset_version_id = $1`, fixture.datasetID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM evaluation_dataset_versions WHERE id = $1`, fixture.datasetID)
	require.NoError(t, err)
	for _, workerID := range fixture.workerIDs {
		_, err = tx.ExecContext(ctx, `DELETE FROM evaluation_workers WHERE id = $1`, workerID)
		require.NoError(t, err)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, fixture.userID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}
