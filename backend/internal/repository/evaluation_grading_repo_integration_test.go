//go:build integration

package repository

import (
	"context"
	"database/sql"
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

func TestSubmitEvidenceRequiresSealedRouteEvidence(t *testing.T) {
	ctx := context.Background()
	evidenceRepo, lease, semantics := createOpenRouteEvidenceFixture(t)
	_, err := evidenceRepo.CreateOpen(ctx, createOpenRouteEvidenceInput(lease, semantics))
	require.NoError(t, err)

	repo := NewEvaluationGradingRepository(integrationDB)
	_, err = repo.(*evaluationGradingRepository).SubmitEvidence(ctx, service.EvidenceSubmission{
		AssignmentID: lease.ID,
		SampleID:     lease.SampleID,
		Evidence:     []byte(`{"route_trace_id":"` + lease.RouteTraceID + `"}`),
		LeaseEpoch:   lease.LeaseEpoch,
	}, lease.Token)
	require.EqualError(t, err, "route evidence is not sealed")

	var assignmentStatus, sampleStatus string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT a.status, s.status
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		WHERE a.id = $1`, lease.ID).Scan(&assignmentStatus, &sampleStatus))
	require.Equal(t, "leased", assignmentStatus)
	require.Equal(t, "leased", sampleStatus)
}

func TestEvaluationGrading_ScoreHeadRequiresCompleteScoreRef(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationGradingRepository(integrationDB)
	run, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	var sampleID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT id FROM evaluation_samples WHERE run_id = $1 LIMIT 1`, run.ID).Scan(&sampleID))
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_score_heads (sample_id, grader_id, score_id, score_created_at, version)
		VALUES ($1, 'exact', $2, NOW(), 1)`, sampleID, uuid.New())
	require.Error(t, err)
}

func TestSubmitScoreUsesCompositeScoreRef(t *testing.T) {
	ctx := context.Background()
	fixture, repo, lease := prepareSealedGradingLease(t)
	_ = fixture

	result, err := repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{}, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.NotZero(t, result.CreatedAt)
	var headID uuid.UUID
	var headCreatedAt time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT score_id, score_created_at
		FROM evaluation_score_heads WHERE sample_id = $1 AND grader_id = $2`, result.SampleID, result.GraderID).Scan(&headID, &headCreatedAt))
	require.Equal(t, result.ID, headID)
	require.Equal(t, result.CreatedAt.UTC(), headCreatedAt.UTC())
}

func TestSubmitScorePersistsCompleteIdempotencyScoreRef(t *testing.T) {
	ctx := context.Background()
	_, repo, lease := prepareSealedGradingLease(t)
	submission := service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{}, LeaseEpoch: lease.LeaseEpoch,
	}

	result, err := repo.SubmitScore(ctx, lease.ID, lease.Token, submission)
	require.NoError(t, err)
	var persisted service.ScoreRef
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT score_id, score_created_at
		FROM evaluation_score_idempotency
		WHERE submission_idempotency_key = $1`, scoreSubmissionKey(lease.ID, submission)).Scan(&persisted.ID, &persisted.CreatedAt))
	require.Equal(t, result.Ref, persisted)
}

func TestSubmitScoreRequiresCurrentAssignmentAndSealedEvidenceSet(t *testing.T) {
	ctx := context.Background()
	_, repo, lease := prepareSealedGradingLease(t)
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status)
		VALUES ($1, $2, 2, $3, 'pending')`, uuid.New(), lease.SampleID, strings.Repeat("b", 64))
	require.NoError(t, err)

	_, err = repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{strings.Repeat("g", 64)}, LeaseEpoch: lease.LeaseEpoch,
	})
	require.ErrorIs(t, err, service.ErrLeaseFenced)
}

func TestSubmitScoreRejectsEvidenceFromDifferentLeaseEpoch(t *testing.T) {
	ctx := context.Background()
	_, repo, lease := prepareSealedGradingLease(t)
	_, err := integrationDB.ExecContext(ctx, `
		UPDATE evaluation_assignments SET lease_epoch = lease_epoch + 1 WHERE id = $1`, lease.AssignmentID)
	require.NoError(t, err)

	_, err = repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{}, LeaseEpoch: lease.LeaseEpoch,
	})
	require.ErrorIs(t, err, service.ErrEvidenceMismatch)
}

func TestSubmitScoreRejectsMalformedConfirmedArtifactHash(t *testing.T) {
	ctx := context.Background()
	_, repo, lease := prepareSealedGradingLease(t)
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_artifacts (
			id, run_id, assignment_id, sample_id, object_key, sha256, byte_count, mime_type,
			scan_status, retention_deadline, confirmed_at
		) VALUES ($1, $2, $3, $4, 'evidence/malformed.json', $5, 1, 'application/json', 'clean', NOW() + INTERVAL '1 day', NOW())`,
		uuid.New(), lease.RunID, lease.AssignmentID, lease.SampleID, strings.Repeat("g", 64))
	require.NoError(t, err)

	_, err = repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{strings.Repeat("g", 64)}, LeaseEpoch: lease.LeaseEpoch,
	})
	require.ErrorIs(t, err, service.ErrEvidenceMismatch)
}

func TestSubmitScoreAppendsHeadEventAndOutboxAtomically(t *testing.T) {
	ctx := context.Background()
	_, repo, lease := prepareSealedGradingLease(t)

	result, err := repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{}, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	var eventID uuid.UUID
	var sourceAssignmentID uuid.UUID
	var routeEvidenceSetHash, artifactManifestHash string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id, source_assignment_id, route_evidence_set_hash
		FROM evaluation_score_head_events WHERE score_id = $1 AND score_created_at = $2`, result.ID, result.CreatedAt).Scan(&eventID, &sourceAssignmentID, &routeEvidenceSetHash))
	require.Equal(t, lease.AssignmentID, sourceAssignmentID)
	require.Len(t, routeEvidenceSetHash, 64)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT artifact_manifest_hash FROM evaluation_scores
		WHERE id = $1 AND created_at = $2`, result.ID, result.CreatedAt).Scan(&artifactManifestHash))
	require.Len(t, artifactManifestHash, 64)
	var outboxCount, analysisCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events WHERE source_type = 'score_head_event' AND source_id = $1`, eventID.String()).Scan(&outboxCount))
	require.Equal(t, 1, outboxCount)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE run_id = $1`, result.RunID).Scan(&analysisCount))
	require.Zero(t, analysisCount)
}

func TestSubmitScoreAppendsOutboxForEveryHeadAdvance(t *testing.T) {
	ctx := context.Background()
	fixture, repo, first := prepareSealedGradingLease(t)
	firstScore, err := repo.SubmitScore(ctx, first.ID, first.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{}, LeaseEpoch: first.LeaseEpoch,
	})
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status = 'pending', score_id = NULL, score_created_at = NULL,
			lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL
		WHERE id = $1`, first.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_assignments SET status = 'evidence_uploaded' WHERE id = $1`, first.AssignmentID)
	require.NoError(t, err)
	second, err := repo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	secondScore, err := repo.SubmitScore(ctx, second.ID, second.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.80"), EvidenceHashes: []string{}, LeaseEpoch: second.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, firstScore.Version+1, secondScore.Version)
	var outboxCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events WHERE run_id = $1 AND event_type = 'cell_recompute'`, firstScore.RunID).Scan(&outboxCount))
	require.Equal(t, 2, outboxCount)
}

func TestAssignmentReplacementImmediatelyRemovesOldHeadEligibility(t *testing.T) {
	ctx := context.Background()
	_, repo, lease := prepareSealedGradingLease(t)
	result, err := repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{}, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	var before int
	require.NoError(t, integrationDB.QueryRowContext(ctx, eligibleScoreHeadsSQL, result.RunID).Scan(&before))
	require.Equal(t, 1, before)
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status)
		VALUES ($1, $2, 2, $3, 'pending')`, uuid.New(), lease.SampleID, strings.Repeat("c", 64))
	require.NoError(t, err)
	var after int
	require.NoError(t, integrationDB.QueryRowContext(ctx, eligibleScoreHeadsSQL, result.RunID).Scan(&after))
	require.Zero(t, after)
}

func TestScoreZeroRemainsSuccessfulEligibleResult(t *testing.T) {
	ctx := context.Background()
	_, repo, lease := prepareSealedGradingLease(t)
	result, err := repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.Zero, EvidenceHashes: []string{}, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.True(t, result.Score.IsZero())
	var eligible int
	require.NoError(t, integrationDB.QueryRowContext(ctx, eligibleScoreHeadsSQL, result.RunID).Scan(&eligible))
	require.Equal(t, 1, eligible)
}

const eligibleScoreHeadsSQL = `
	SELECT COUNT(*)
	FROM evaluation_score_heads h
	JOIN evaluation_scores score ON score.id = h.score_id AND score.created_at = h.score_created_at
	JOIN evaluation_assignments assignment ON assignment.id = score.source_assignment_id
	JOIN evaluation_samples sample ON sample.id = score.sample_id
	WHERE score.run_id = $1
	  AND assignment.attempt = (
		SELECT MAX(current_assignment.attempt)
		FROM evaluation_assignments current_assignment
		WHERE current_assignment.sample_id = score.sample_id
	  )`

func TestProductionCodeNeverUpdatesScoreIsCurrent(t *testing.T) {
	ctx := context.Background()
	fixture, repo, lease := prepareSealedGradingLease(t)
	result, err := repo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), EvidenceHashes: []string{}, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Version)

	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_grading_jobs SET status = 'pending', score_id = NULL, score_created_at = NULL, lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL WHERE id = $1`, lease.ID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_assignments SET status = 'evidence_uploaded' WHERE id = $1`, lease.AssignmentID)
	require.NoError(t, err)
	second, err := repo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, second)
	secondScore, err := repo.SubmitScore(ctx, second.ID, second.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.90"), EvidenceHashes: []string{}, LeaseEpoch: second.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, 2, secondScore.Version)
	var headID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT score_id FROM evaluation_score_heads WHERE sample_id = $1 AND grader_id = $2`, secondScore.SampleID, secondScore.GraderID).Scan(&headID))
	require.Equal(t, secondScore.ID, headID)
	var immutableVersions int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_scores
		WHERE sample_id = $1 AND grader_id = $2`, secondScore.SampleID, secondScore.GraderID).Scan(&immutableVersions))
	require.Equal(t, 2, immutableVersions)
}

func prepareSealedGradingLease(t *testing.T) (evaluationRepositoryFixture, service.EvaluationGradingRepository, *service.GradingLease) {
	t.Helper()
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	_, err := integrationDB.ExecContext(ctx, `
		UPDATE evaluation_workers SET worker_kind = 'grader', token_hash = $1, capabilities = ARRAY['grader'] WHERE id = $2`,
		hashToken("grader-token-"+fixture.workerIDs[0].String()), fixture.workerIDs[0])
	require.NoError(t, err)
	repo := NewEvaluationGradingRepository(integrationDB)
	run, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	var assignmentID, sampleID, manifestID uuid.UUID
	var manifestHash string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT assignment.id, assignment.sample_id, pair_spec.request_manifest_id, pair_spec.request_manifest_sha256
		FROM evaluation_assignments assignment
		JOIN evaluation_samples sample ON sample.id = assignment.sample_id
		JOIN evaluation_side_specs side_spec ON side_spec.sample_id = sample.id
		JOIN evaluation_pair_specs pair_spec ON pair_spec.id = side_spec.pair_spec_id
		WHERE sample.run_id = $1
		ORDER BY assignment.id
		LIMIT 1`, run.ID).Scan(&assignmentID, &sampleID, &manifestID, &manifestHash))
	semanticsID := uuid.New()
	signingKeyID := uuid.New()
	semanticsHash := hashString(uuid.NewString())
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_request_semantics (id, schema_version, canonical_semantics_bytes, request_semantics_sha256)
		VALUES ($1, 'radar-request-semantics-v1', convert_to('{}', 'UTF8'), $2)`, semanticsID, semanticsHash)
	require.NoError(t, err)
	err = integrationDB.QueryRowContext(ctx, `
		SELECT id FROM evaluation_evidence_signing_keys WHERE status = 'active'`).Scan(&signingKeyID)
	if err == sql.ErrNoRows {
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_evidence_signing_keys (id, key_reference, status, state_epoch)
			VALUES ($1, $2, 'active', 1)`, signingKeyID, "score-test-key-"+uuid.NewString())
	}
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_route_evidence (
			route_trace_id, evaluation_run_id, sample_id, api_key_id, request_id,
			requested_model, resolved_model, route_profile_version, provider, region,
			attempts, fallback_chain, finish_reason, input_tokens, output_tokens,
			latency_ms, billed_amount, transport_status, started_at, finished_at,
			schema_version, canonicalization_version, assignment_id, request_ordinal,
			lease_epoch, request_manifest_id, request_manifest_sha256, request_slot_id,
			request_semantics_id, request_semantics_sha256,
			request_semantics_policy_sha256, request_tool_schema_sha256,
			request_allowed_tool_set_sha256, evidence_revision, terminal_at, sealed_at,
			payload_hash, signing_key_id, payload_hmac, billing_status, gateway_image_digest
		) VALUES (
			$1, $2, $3, $4, 'request-1',
			'route-a', 'model-a', 'route-v1', 'provider-a', 'region-a',
			1, '[]'::jsonb, 'stop', 1, 1,
			1, 0.00000001, 'succeeded', NOW(), NOW(),
			'radar-route-evidence-v1', 'rfc8785-v1', $5, 0,
			0, $6, $7, 'request-0',
			$8, $9,
			$10, $11, $12, 1, NOW(), NOW(),
			$13, $14, $15, 'complete', 'sha256:gateway'
		)`, "score-sealed-"+uuid.NewString(), run.ID, sampleID, fixture.apiKeyID, assignmentID,
		manifestID, manifestHash, semanticsID, semanticsHash, strings.Repeat("2", 64),
		strings.Repeat("3", 64), strings.Repeat("4", 64), strings.Repeat("5", 64), signingKeyID, strings.Repeat("6", 64))
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_assignments
		SET status = 'evidence_uploaded', lease_epoch = 0, evidence_manifest = '{}'::jsonb
		WHERE id = $1`, assignmentID)
	require.NoError(t, err)
	lease, err := repo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	return fixture, repo, lease
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

func TestEvaluationRevisionSchema_RejectsInvalidOriginsAndCrossRunBatch(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationGradingRepository(integrationDB)
	firstRun, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	secondRun, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	tx := testTx(t)

	firstBatchID := uuid.New()
	secondBatchID := uuid.New()
	for _, batch := range []struct {
		id    uuid.UUID
		runID uuid.UUID
	}{{firstBatchID, firstRun.ID}, {secondBatchID, secondRun.ID}} {
		require.NoError(t, execRadarFixtureSQL(ctx, tx, `
			INSERT INTO evaluation_revision_batches (
				id, run_id, status, control_epoch, reason, requested_by, idempotency_key
			) VALUES ($1, $2, 'running', 1, 'model_regression', $3, $4)`,
			batch.id, batch.runID, fixture.userID, strings.Repeat(batch.id.String()[:1], 64)))
	}
	requireSQLRejectedWithinSavepoint(t, tx, "batch_identity_update", `
		UPDATE evaluation_revision_batches SET reason = 'mutated' WHERE id = $1`, firstBatchID)
	requireSQLRejectedWithinSavepoint(t, tx, "batch_delete", `
		DELETE FROM evaluation_revision_batches WHERE id = $1`, firstBatchID)

	var gradingJobID, assignmentID uuid.UUID
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT id, assignment_id FROM evaluation_grading_jobs WHERE run_id = $1 ORDER BY id LIMIT 1`, firstRun.ID).Scan(&gradingJobID, &assignmentID))
	requireSQLRejectedWithinSavepoint(t, tx, "regrade_without_batch", `
		UPDATE evaluation_grading_jobs
		SET work_origin = 'regrade', revision_batch_id = NULL, grading_input_hash = $2
		WHERE id = $1`, gradingJobID, strings.Repeat("a", 64))
	requireSQLRejectedWithinSavepoint(t, tx, "initial_with_batch", `
		UPDATE evaluation_grading_jobs
		SET work_origin = 'initial', revision_batch_id = $2
		WHERE id = $1`, gradingJobID, firstBatchID)
	requireSQLRejectedWithinSavepoint(t, tx, "cross_run_batch", `
		UPDATE evaluation_grading_jobs
		SET work_origin = 'regrade', revision_batch_id = $2, grading_input_hash = $3
		WHERE id = $1`, gradingJobID, secondBatchID, strings.Repeat("b", 64))

	analysisJobID := uuid.New()
	requireSQLRejectedWithinSavepoint(t, tx, "analysis_regrade_without_batch", `
		INSERT INTO evaluation_analysis_jobs (
			id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, status, scope, work_origin, revision_batch_id,
			input_set_hash, aggregate_revision
		) VALUES (
			$1, $2, 'coding', 'route-a', 'daily', 'v1',
			DATE_TRUNC('day', NOW()), 'pending', 'cell', 'regrade', NULL,
			$3, 1
		)`, analysisJobID, firstRun.ID, strings.Repeat("c", 64))

	requirementID := uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_revision_batch_requirements (
			id, revision_batch_id, run_id, requirement_type, target_key,
			source_assignment_id, source_hash, cause_set_hash
		) VALUES ($1, $2, $3, 'grading', 'sample:grader', $4, $5, $6)`,
		requirementID, firstBatchID, firstRun.ID, assignmentID,
		strings.Repeat("4", 64), strings.Repeat("5", 64)))
	requireSQLRejectedWithinSavepoint(t, tx, "requirement_identity_update", `
		UPDATE evaluation_revision_batch_requirements SET target_key = 'mutated' WHERE id = $1`, requirementID)
	requireSQLRejectedWithinSavepoint(t, tx, "requirement_delete", `
		DELETE FROM evaluation_revision_batch_requirements WHERE id = $1`, requirementID)
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		UPDATE evaluation_revision_batch_requirements
		SET status = 'completed', completed_at = NOW(), updated_at = NOW()
		WHERE id = $1`, requirementID))
}

func TestEvaluationRevisionSchema_ImmutablePartitionedRecordsAndCompositeRefs(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationGradingRepository(integrationDB)
	run, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	tx := testTx(t)

	var sampleID, assignmentID uuid.UUID
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT s.id, a.id
		FROM evaluation_samples s
		JOIN evaluation_assignments a ON a.sample_id = s.id
		WHERE s.run_id = $1
		ORDER BY s.id, a.id
		LIMIT 1`, run.ID).Scan(&sampleID, &assignmentID))
	scoreID := uuid.New()
	var scoreCreatedAt time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_scores (
			id, run_id, sample_id, grader_id, grader_version, version, score,
			submission_idempotency_key
		) VALUES ($1, $2, $3, 'grader', 'v1', 1, 0.5, $4)
		RETURNING created_at`, scoreID, run.ID, sampleID, strings.Repeat("d", 64)).Scan(&scoreCreatedAt))

	reviewID := uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_manual_reviews (
			id, run_id, sample_id, score_id, score_created_at, reason
		) VALUES ($1, $2, $3, $4, $5, 'schema-test')`,
		reviewID, run.ID, sampleID, scoreID, scoreCreatedAt))
	requireSQLRejectedWithinSavepoint(t, tx, "wrong_score_partition", `
		INSERT INTO evaluation_manual_reviews (
			id, run_id, sample_id, score_id, score_created_at, reason
		) VALUES ($1, $2, $3, $4, $5, 'wrong-partition')`,
		uuid.New(), run.ID, sampleID, scoreID, scoreCreatedAt.Add(time.Second))
	requireSQLRejectedWithinSavepoint(t, tx, "score_update", `
		UPDATE evaluation_scores SET explanation = 'mutated' WHERE id = $1 AND created_at = $2`, scoreID, scoreCreatedAt)
	requireSQLRejectedWithinSavepoint(t, tx, "score_delete", `
		DELETE FROM evaluation_scores WHERE id = $1 AND created_at = $2`, scoreID, scoreCreatedAt)

	headEventID := uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_score_head_events (
			id, run_id, sample_id, grader_id, version, score_id, score_created_at,
			source_assignment_id, route_evidence_set_hash, reason
		) VALUES ($1, $2, $3, 'grader', 1, $4, $5, $6, $7, 'initial')`,
		headEventID, run.ID, sampleID, scoreID, scoreCreatedAt, assignmentID, strings.Repeat("3", 64)))
	requireSQLRejectedWithinSavepoint(t, tx, "head_event_update", `
		UPDATE evaluation_score_head_events SET reason = 'mutated' WHERE id = $1`, headEventID)
	requireSQLRejectedWithinSavepoint(t, tx, "head_event_delete", `
		DELETE FROM evaluation_score_head_events WHERE id = $1`, headEventID)

	snapshotID := uuid.New()
	windowStart := time.Now().UTC().Truncate(24 * time.Hour)
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_aggregate_snapshots (
			id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, aggregate
		) VALUES ($1, $2, 'coding', 'route-a', 'daily', 'v1', $3, '{}'::jsonb)`,
		snapshotID, run.ID, windowStart))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_aggregate_heads (
			run_id, capability_domain, canonical_model_route, analysis_version,
			snapshot_id, window_start, aggregate_revision, input_set_hash, aggregate_hash
		) VALUES ($1, 'coding', 'route-a', 'v1', $2, $3, 1, $4, $5)`,
		run.ID, snapshotID, windowStart, strings.Repeat("e", 64), strings.Repeat("f", 64)))
	requireSQLRejectedWithinSavepoint(t, tx, "wrong_snapshot_partition", `
		INSERT INTO evaluation_aggregate_heads (
			run_id, capability_domain, canonical_model_route, analysis_version,
			snapshot_id, window_start, aggregate_revision, input_set_hash, aggregate_hash
		) VALUES ($1, 'reasoning', 'route-a', 'v1', $2, $3, 1, $4, $5)`,
		run.ID, snapshotID, windowStart.Add(time.Second), strings.Repeat("1", 64), strings.Repeat("2", 64))
	requireSQLRejectedWithinSavepoint(t, tx, "snapshot_update", `
		UPDATE evaluation_aggregate_snapshots SET aggregate = '{"mutated":true}'::jsonb
		WHERE id = $1 AND window_start = $2`, snapshotID, windowStart)
	requireSQLRejectedWithinSavepoint(t, tx, "snapshot_delete", `
		DELETE FROM evaluation_aggregate_snapshots WHERE id = $1 AND window_start = $2`, snapshotID, windowStart)
}

func TestEvaluationOutbox_RejectsFutureCauseAndInsertOnlyMutation(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	repo := NewEvaluationGradingRepository(integrationDB)
	run, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	otherRun, err := repo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	tx := testTx(t)

	insertEvent := func(id, runID uuid.UUID, suffix string) {
		t.Helper()
		require.NoError(t, execRadarFixtureSQL(ctx, tx, `
			INSERT INTO evaluation_outbox_events (
				id, event_type, dedup_key, causation_id, cause_set_hash, run_id,
				source_type, source_id, source_hash, payload_hash, payload
			) VALUES ($1, 'cell_recompute', $2, $3, $4, $5,
				'score_head_event', $6, $7, $8, '{}'::jsonb)`,
			id, strings.Repeat(suffix, 64), strings.Repeat(suffix, 64),
			strings.Repeat(suffix, 64), runID, id.String(), strings.Repeat(suffix, 64), strings.Repeat(suffix, 64)))
	}
	crossRunCauseID := uuid.New()
	parentID := uuid.New()
	childID := uuid.New()
	futureCauseID := uuid.New()
	insertEvent(crossRunCauseID, otherRun.ID, "5")
	insertEvent(parentID, run.ID, "6")
	insertEvent(childID, run.ID, "7")
	insertEvent(futureCauseID, run.ID, "8")
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_outbox_event_causes (event_id, cause_event_id, run_id)
		VALUES ($1, $2, $3)`, childID, parentID, run.ID))
	requireSQLRejectedWithinSavepoint(t, tx, "cause_relation_update", `
		UPDATE evaluation_outbox_event_causes SET source_head_event_id = NULL
		WHERE event_id = $1 AND cause_event_id = $2`, childID, parentID)
	requireSQLRejectedWithinSavepoint(t, tx, "cause_relation_delete", `
		DELETE FROM evaluation_outbox_event_causes
		WHERE event_id = $1 AND cause_event_id = $2`, childID, parentID)
	requireSQLRejectedWithinSavepoint(t, tx, "cross_run_cause", `
		INSERT INTO evaluation_outbox_event_causes (event_id, cause_event_id, run_id)
		VALUES ($1, $2, $3)`, childID, crossRunCauseID, run.ID)
	requireSQLRejectedWithinSavepoint(t, tx, "future_cause", `
		INSERT INTO evaluation_outbox_event_causes (event_id, cause_event_id, run_id)
		VALUES ($1, $2, $3)`, childID, futureCauseID, run.ID)
	requireSQLRejectedWithinSavepoint(t, tx, "outbox_payload_mutation", `
		UPDATE evaluation_outbox_events SET payload = '{"mutated":true}'::jsonb WHERE id = $1`, childID)
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		UPDATE evaluation_outbox_events
		SET status = 'leased', attempt = attempt + 1, lease_token_hash = $2,
			lease_owner = $3, lease_expires_at = NOW() + INTERVAL '1 minute'
		WHERE id = $1`, childID, strings.Repeat("9", 64), fixture.workerIDs[0]))
}

func (r *evaluationGradingRepository) createRunForGradingTest(ctx context.Context, planID uuid.UUID) (*service.EvaluationRun, error) {
	// The normal repository implementation owns run expansion; this helper keeps
	// the focused integration fixtures independent of that concrete type.
	eval := &evaluationRepository{db: r.db}
	var tenantID int64
	if err := r.db.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_plans WHERE id=$1`, planID).Scan(&tenantID); err != nil {
		return nil, err
	}
	return eval.CreateRunWithMatrix(ctx, service.CreateRunInput{PlanID: planID, TriggerSource: "manual", CreatedBy: tenantID})
}
