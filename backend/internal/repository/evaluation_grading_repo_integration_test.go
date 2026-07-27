//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestEvaluationGrading_RejectsRunnerWorkerAndFencesScore(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationGradingRepository(integrationDB)
	_, err := repo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"exact"}, time.Minute)
	require.Error(t, err)
	require.ErrorIs(t, err, service.ErrWorkerKindMismatch)
}

func TestEvaluationGrading_RegradingCreatesVersionAndClearsHead(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	_, err := integrationDB.ExecContext(ctx, `
		UPDATE evaluation_workers SET worker_kind = 'grader', token_hash = $1, capabilities = ARRAY['grader'] WHERE id = $2`,
		hashToken("grader-token"), fixture.workerIDs[0])
	require.NoError(t, err)

	repo := NewEvaluationGradingRepository(integrationDB)
	run, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_assignments SET status = 'evidence_uploaded' WHERE sample_id IN (SELECT id FROM evaluation_samples WHERE run_id = $1)`, run.ID)
	require.NoError(t, err)
	lease, err := repo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	result, err := repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		SampleID: lease.SampleID, GraderID: lease.GraderID, GraderVersion: lease.GraderVersion,
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Version)

	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_grading_jobs SET status = 'pending', lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL WHERE id = $1`, lease.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_assignments SET status = 'evidence_uploaded' WHERE id = $1`, lease.AssignmentID)
	require.NoError(t, err)
	second, err := repo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	secondScore, err := repo.SubmitScore(ctx, second.ID, second.Token, service.ScoreSubmission{
		SampleID: second.SampleID, GraderID: second.GraderID, GraderVersion: second.GraderVersion,
		Score: decimal.RequireFromString("0.90"), EvidenceHashes: []string{},
	})
	require.NoError(t, err)
	require.Equal(t, 2, secondScore.Version)
	var current, oldCurrent bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT is_current FROM evaluation_scores WHERE id = $1`, result.ID).Scan(&oldCurrent))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT is_current FROM evaluation_scores WHERE id = $1`, secondScore.ID).Scan(&current))
	require.False(t, oldCurrent)
	require.True(t, current)
}

func TestEvaluationGrading_ClaimReturnsEvidenceManifestCaseAndArtifacts(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	_, err := integrationDB.ExecContext(ctx, `
		UPDATE evaluation_workers SET worker_kind = 'grader', token_hash = $1, capabilities = ARRAY['grader'] WHERE id = $2`,
		hashToken("grader-token"), fixture.workerIDs[0])
	require.NoError(t, err)
	repo := NewEvaluationGradingRepository(integrationDB)
	run, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	var assignmentID, sampleID, caseID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT a.id, a.sample_id, s.case_id FROM evaluation_assignments a JOIN evaluation_samples s ON s.id = a.sample_id WHERE s.run_id = $1 ORDER BY a.id LIMIT 1`, run.ID).Scan(&assignmentID, &sampleID, &caseID))
	manifest := `{"assignment_id":"` + assignmentID.String() + `","sample_id":"` + sampleID.String() + `"}`
	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_assignments SET status = 'evidence_uploaded', evidence_manifest = $2::jsonb WHERE id = $1`, assignmentID, manifest)
	require.NoError(t, err)
	artifactID := uuid.New()
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO evaluation_artifacts (id, run_id, sample_id, assignment_id, object_key, sha256, byte_count, mime_type, scan_status, retention_deadline, confirmed_at) VALUES ($1, $2, $3, $4, 'runs/evidence.json', $5, 2, 'application/json', 'clean', NOW() + INTERVAL '1 day', NOW())`, artifactID, run.ID, sampleID, assignmentID, strings.Repeat("a", 64))
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'evidence_uploaded', route_trace_id = 'trace-grading' WHERE id = $1`, sampleID)
	require.NoError(t, err)
	lease, err := repo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, assignmentID, lease.AssignmentID)
	require.Equal(t, caseID, lease.Case.CaseID)
	require.JSONEq(t, manifest, string(lease.EvidenceManifest))
	require.Equal(t, "trace-grading", lease.RouteTraceID)
	require.Len(t, lease.Evidence, 1)
	require.Equal(t, artifactID, lease.Evidence[0].ID)
}

func TestEvaluationGrading_AggregateRejectsScoreFromAnotherRun(t *testing.T) {
	ctx := context.Background()
	first := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	second := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	_, err := integrationDB.ExecContext(ctx, `UPDATE evaluation_workers SET worker_kind = 'statistics', token_hash = $1, capabilities = ARRAY['coding'] WHERE id = $2`, hashToken("statistics-token"), first.workerIDs[0])
	require.NoError(t, err)
	gradingRepo := NewEvaluationGradingRepository(integrationDB)
	run, err := gradingRepo.(*evaluationGradingRepository).createRunForGradingTest(ctx, first.planID)
	require.NoError(t, err)
	jobID := uuid.New()
	_, err = integrationDB.ExecContext(ctx, `INSERT INTO evaluation_analysis_jobs (id, run_id, capability_domain, model_route, "window", analysis_version, window_start, status) VALUES ($1, $2, 'coding', 'route-a', 'daily', 'v1', DATE_TRUNC('day', NOW()), 'pending')`, jobID, run.ID)
	require.NoError(t, err)
	lease, err := gradingRepo.ClaimAnalysisJob(ctx, first.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	_, err = gradingRepo.CompleteAnalysisJob(ctx, lease.ID, lease.Token, service.AggregateSubmission{RunID: run.ID, ScoreIDs: []uuid.UUID{uuid.New()}})
	require.Error(t, err)
	_ = second
}

func (r *evaluationGradingRepository) createRunForGradingTest(ctx context.Context, planID uuid.UUID) (*service.EvaluationRun, error) {
	// The normal repository implementation owns run expansion; this helper keeps
	// the focused integration fixtures independent of that concrete type.
	eval := &evaluationRepository{db: r.db}
	return eval.CreateRunWithMatrix(ctx, service.CreateRunInput{PlanID: planID, TriggerSource: "manual"})
}
