//go:build integration

package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
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

	lease, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{}, time.Minute)
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

func TestEvaluationRepository_EmptyCapabilitiesCannotReclaimExpired(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationRepository(integrationDB)
	_, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID:        fixture.planID,
		TriggerSource: "manual",
		BaselineRef:   map[string]any{"revision": "baseline-1"},
		CandidateRef:  map[string]any{"revision": "candidate-1"},
		CreatedBy:     fixture.userID,
	})
	require.NoError(t, err)

	old, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, old)
	_, err = integrationDB.ExecContext(ctx,
		"UPDATE evaluation_assignments SET lease_expires_at = NOW() - INTERVAL '1 second' WHERE id = $1", old.ID)
	require.NoError(t, err)

	fresh, err := repo.ClaimAssignment(ctx, fixture.workerIDs[1], []string{}, time.Minute)
	require.NoError(t, err)
	require.Nil(t, fresh)

	var attempts int
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_assignments WHERE sample_id = $1`, old.SampleID).Scan(&attempts))
	require.Equal(t, 1, attempts)
}

func TestEvaluationRepository_RegisteredCapabilitiesBoundClaims(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixtureWithCases(t, []evaluationCaseFixtureSpec{
		{capability: "safety", priority: "P0", sampleCount: 1, estimatedCost: decimal.RequireFromString("0.01")},
	}, []map[string]any{{"route": "route-a"}}, decimal.RequireFromString("100"))
	repo := NewEvaluationRepository(integrationDB)
	_, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)

	lease, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{"safety"}, time.Minute)
	require.NoError(t, err)
	require.Nil(t, lease)
}

func TestEvaluationRepository_FreezesModelConfigurationInLease(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixtureWithCases(t, []evaluationCaseFixtureSpec{
		{capability: "coding", priority: "P0", sampleCount: 1, estimatedCost: decimal.RequireFromString("0.01")},
	}, []map[string]any{{
		"route": "route-a", "temperature": 0.2, "max_tokens": 64,
	}}, decimal.RequireFromString("100"))
	repo := NewEvaluationRepository(integrationDB)
	run, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)

	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_plans
		SET model_matrix = '[{"route":"route-a","temperature":0.9,"max_tokens":128}]'::jsonb
		WHERE id = $1`, fixture.planID)
	require.NoError(t, err)

	lease, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.JSONEq(t, `{"max_tokens":64,"route":"route-a","temperature":0.2}`, string(lease.ModelConfig))
	returnedHash := sha256.Sum256(lease.ModelConfig)
	require.Equal(t, fmt.Sprintf("%x", returnedHash), lease.ModelConfigSHA256)

	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_samples SET model_config = '{}'::jsonb WHERE run_id = $1`, run.ID)
	require.Error(t, err)
}

func TestEvaluationRepository_FreezesLosslessNumericModelConfiguration(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixtureWithCases(t, []evaluationCaseFixtureSpec{
		{capability: "coding", priority: "P0", sampleCount: 1, estimatedCost: decimal.RequireFromString("0.01")},
	}, []map[string]any{{
		"route":       "route-a",
		"seed":        json.Number("9007199254740993"),
		"temperature": json.Number("0.12345678901234567890123456789"),
	}}, decimal.RequireFromString("100"))
	repo := NewEvaluationRepository(integrationDB)
	_, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)

	lease, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	const expectedConfig = `{"route":"route-a","seed":9007199254740993,"temperature":0.12345678901234567890123456789}`
	require.Equal(t, expectedConfig, string(lease.ModelConfig))
	expectedHash := sha256.Sum256([]byte(expectedConfig))
	require.Equal(t, fmt.Sprintf("%x", expectedHash), lease.ModelConfigSHA256)
	returnedHash := sha256.Sum256(lease.ModelConfig)
	require.Equal(t, fmt.Sprintf("%x", returnedHash), lease.ModelConfigSHA256)
}

func TestEvaluationRepository_RejectsMismatchedStoredModelConfigDigest(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixtureWithCases(t, []evaluationCaseFixtureSpec{
		{capability: "coding", priority: "P0", sampleCount: 1, estimatedCost: decimal.RequireFromString("0.01")},
	}, []map[string]any{{"route": "route-a", "seed": json.Number("9007199254740993")}}, decimal.RequireFromString("100"))
	repo := NewEvaluationRepository(integrationDB)
	run, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)

	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_samples SET model_config_sha256 = $1 WHERE run_id = $2`,
		strings.Repeat("0", 64), run.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	lease, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.Nil(t, lease)
	require.ErrorContains(t, err, "model configuration digest mismatch")
}

func TestEvaluationSampleExecutionIdentityMigrationUpgradesOriginal191(t *testing.T) {
	ctx := context.Background()
	schema := "evaluation_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	conn, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		resetErr := resetEvaluationMigrationTestSearchPath(context.Background(), conn)
		closeErr := conn.Close()
		_, dropErr := integrationDB.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		require.NoError(t, resetErr)
		require.NoError(t, closeErr)
		require.NoError(t, dropErr)
	})
	require.NoError(t, setEvaluationMigrationTestSearchPath(ctx, conn, schema))
	require.NoError(t, executeEvaluationMigrationSQL(ctx, conn, `CREATE TABLE users (id BIGINT PRIMARY KEY)`))

	original191, err := migrations.FS.ReadFile("191_add_radar_control_plane.sql")
	require.NoError(t, err)
	require.NoError(t, executeEvaluationMigrationSQL(ctx, conn, string(original191)))

	var modelConfigColumn bool
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
				AND table_name = 'evaluation_samples'
				AND column_name = 'model_config'
		)`).Scan(&modelConfigColumn))
	require.False(t, modelConfigColumn)

	fixture := insertOriginal191EvaluationSample(t, ctx, conn)
	upgrade192, err := migrations.FS.ReadFile("192_add_evaluation_sample_execution_identity.sql")
	require.NoError(t, err)
	require.NoError(t, executeEvaluationMigrationSQL(ctx, conn, string(upgrade192)))
	require.NoError(t, executeEvaluationMigrationSQL(ctx, conn, string(upgrade192)))

	var modelConfig, modelConfigHash string
	require.NoError(t, conn.QueryRowContext(ctx, `
		SELECT model_config::text, model_config_sha256
		FROM evaluation_samples WHERE id = $1`, fixture.sampleID).Scan(&modelConfig, &modelConfigHash))
	require.Equal(t, "{}", modelConfig)
	emptyConfigHash := sha256.Sum256([]byte("{}"))
	require.Equal(t, fmt.Sprintf("%x", emptyConfigHash), modelConfigHash)

	_, err = conn.ExecContext(ctx, `
		UPDATE evaluation_samples SET status = 'leased' WHERE id = $1`, fixture.sampleID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		UPDATE evaluation_samples SET model_route = 'candidate:changed' WHERE id = $1`, fixture.sampleID)
	require.Error(t, err)
}

func TestEvaluationMigrationTestSearchPathRestoresPublicSchema(t *testing.T) {
	ctx := context.Background()
	schema := "evaluation_search_path_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	conn, err := integrationDB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		resetErr := resetEvaluationMigrationTestSearchPath(context.Background(), conn)
		closeErr := conn.Close()
		_, dropErr := integrationDB.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		require.NoError(t, resetErr)
		require.NoError(t, closeErr)
		require.NoError(t, dropErr)
	})

	require.NoError(t, setEvaluationMigrationTestSearchPath(ctx, conn, schema))
	require.NoError(t, resetEvaluationMigrationTestSearchPath(ctx, conn))

	var currentSchema string
	require.NoError(t, conn.QueryRowContext(ctx, "SELECT current_schema()").Scan(&currentSchema))
	require.Equal(t, "public", currentSchema)
}

type original191SampleFixture struct {
	sampleID uuid.UUID
}

func setEvaluationMigrationTestSearchPath(ctx context.Context, conn *sql.Conn, schema string) error {
	if _, err := conn.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		return err
	}
	_, err := conn.ExecContext(ctx, "SET search_path TO "+schema)
	return err
}

func resetEvaluationMigrationTestSearchPath(ctx context.Context, conn *sql.Conn) error {
	_, err := conn.ExecContext(ctx, "RESET search_path")
	return err
}

func executeEvaluationMigrationSQL(ctx context.Context, conn *sql.Conn, statement string) error {
	_, err := conn.ExecContext(ctx, statement)
	return err
}

func insertOriginal191EvaluationSample(t *testing.T, ctx context.Context, conn *sql.Conn) original191SampleFixture {
	t.Helper()
	datasetID := uuid.New()
	caseID := uuid.New()
	planID := uuid.New()
	runID := uuid.New()
	sampleID := uuid.New()
	userID := int64(1)
	_, err := conn.ExecContext(ctx, `INSERT INTO users (id) VALUES ($1)`, userID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO evaluation_dataset_versions (
			id, dataset_key, version, manifest_sha256, source_type, status, created_by
		) VALUES ($1, 'upgrade', 'v1', $2, 'synthetic', 'draft', $3)`,
		datasetID, fmt.Sprintf("%064d", 1), userID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO evaluation_cases (
			id, dataset_version_id, case_key, capability_domain, priority, weight, sample_count,
			prompt_spec, expected_spec, execution_spec, grader_id, grader_version,
			content_sha256, confidentiality
		) VALUES ($1, $2, 'case', 'coding', 'P0', 1, 1,
			'{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'grader', 'v1', $3, 'synthetic')`,
		caseID, datasetID, fmt.Sprintf("%064d", 2))
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO evaluation_plans (
			id, name, dataset_version_id, trigger_type, model_matrix,
			max_run_cost, daily_cost_limit, max_concurrency, created_by
		) VALUES ($1, 'plan', $2, 'manual', '[{"route":"route-a"}]'::jsonb,
			100, 100, 1, $3)`, planID, datasetID, userID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO evaluation_runs (
			id, plan_id, trigger_source, baseline_ref, candidate_ref, status, budget_limit
		) VALUES ($1, $2, 'manual', '{}'::jsonb, '{}'::jsonb, 'pending', 100)`, runID, planID)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `
		INSERT INTO evaluation_samples (
			id, run_id, case_id, model_route, sample_index, priority, status, estimated_cost
		) VALUES ($1, $2, $3, 'baseline:route-a', 0, 'P0', 'pending', 1)`,
		sampleID, runID, caseID)
	require.NoError(t, err)
	return original191SampleFixture{sampleID: sampleID}
}

func TestEvaluationRepository_RecordsBudgetWarningAtEightyPercent(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixtureWithCases(t, []evaluationCaseFixtureSpec{
		{capability: "coding", priority: "P1", sampleCount: 1, estimatedCost: decimal.RequireFromString("40")},
	}, []map[string]any{{"route": "route-a"}}, decimal.RequireFromString("100"))
	repo := NewEvaluationRepository(integrationDB)
	run, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)
	require.Equal(t, service.RunStatusPending, run.Status)

	var warnings int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_run_events
		WHERE run_id = $1 AND event_type = 'budget_warning'`, run.ID).Scan(&warnings))
	require.Equal(t, 1, warnings)

	lease, err := repo.ClaimAssignment(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
}

func TestEvaluationRepository_AtBudgetLimitLeasesOnlyP0(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixtureWithCases(t, []evaluationCaseFixtureSpec{
		{capability: "coding", priority: "P0", sampleCount: 1, estimatedCost: decimal.RequireFromString("10")},
		{capability: "coding", priority: "P1", sampleCount: 1, estimatedCost: decimal.RequireFromString("40")},
	}, []map[string]any{{"route": "route-a"}}, decimal.RequireFromString("100"))
	repo := NewEvaluationRepository(integrationDB)
	run, err := repo.CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)
	require.Equal(t, service.RunStatusBudgetPaused, run.Status)

	for i := 0; i < 2; i++ {
		lease, claimErr := repo.ClaimAssignment(ctx, fixture.workerIDs[i], []string{"coding"}, time.Minute)
		require.NoError(t, claimErr)
		require.NotNil(t, lease)
		var priority string
		require.NoError(t, integrationDB.QueryRowContext(ctx,
			`SELECT priority FROM evaluation_samples WHERE id = $1`, lease.SampleID).Scan(&priority))
		require.Equal(t, "P0", priority)
	}

	lease, err := repo.ClaimAssignment(ctx, fixture.workerIDs[2], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.Nil(t, lease)
}

type evaluationRepositoryFixture struct {
	userID    int64
	datasetID uuid.UUID
	planID    uuid.UUID
	workerIDs []uuid.UUID
}

type evaluationCaseFixtureSpec struct {
	capability    string
	priority      string
	sampleCount   int
	estimatedCost decimal.Decimal
}

func createEvaluationRepositoryFixture(t *testing.T, caseCount int, routes []string, sampleCount int) evaluationRepositoryFixture {
	t.Helper()
	cases := make([]evaluationCaseFixtureSpec, caseCount)
	for i := range cases {
		cases[i] = evaluationCaseFixtureSpec{
			capability: "coding", priority: "P0", sampleCount: sampleCount,
			estimatedCost: decimal.RequireFromString("0.01"),
		}
	}
	matrix := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		matrix = append(matrix, map[string]any{"route": route})
	}
	return createEvaluationRepositoryFixtureWithCases(t, cases, matrix, decimal.RequireFromString("100"))
}

func createEvaluationRepositoryFixtureWithCases(
	t *testing.T,
	cases []evaluationCaseFixtureSpec,
	matrix []map[string]any,
	budgetLimit decimal.Decimal,
) evaluationRepositoryFixture {
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
	for i, evaluationCase := range cases {
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_cases (
				id, dataset_version_id, case_key, capability_domain, priority, weight, sample_count,
				prompt_spec, expected_spec, execution_spec, grader_id, grader_version,
				content_sha256, confidentiality, estimated_cost
			) VALUES (
					$1, $2, $3, $4, $5, 1, $6,
					'{}'::jsonb, '{}'::jsonb, '{}'::jsonb, 'grader', 'v1',
					$7, 'synthetic', $8
				)`, uuid.New(), datasetID, fmt.Sprintf("case-%d", i), evaluationCase.capability,
			evaluationCase.priority, evaluationCase.sampleCount, fmt.Sprintf("%064d", i+10), evaluationCase.estimatedCost)
		require.NoError(t, err)
	}
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_dataset_versions SET status = 'published', published_at = NOW() WHERE id = $1`, datasetID)
	require.NoError(t, err)
	matrixJSON, err := json.Marshal(matrix)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_plans (
			id, name, dataset_version_id, trigger_type, model_matrix,
			max_run_cost, daily_cost_limit, max_concurrency, created_by
		) VALUES ($1, $2, $3, 'manual', $4::jsonb, $5, $6, 10, $7)`,
		planID, "evaluation-plan-"+uuid.NewString(), datasetID, matrixJSON,
		budgetLimit, budgetLimit, user.ID)
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
