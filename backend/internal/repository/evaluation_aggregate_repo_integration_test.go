//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestCellJobRequiresCompleteBaselineCandidatePairs(t *testing.T) {
	ctx := context.Background()
	_, gradingRepo, lease := prepareSealedGradingLease(t)
	_, err := gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)

	repo := NewEvaluationAggregateRepository(integrationDB)
	job, err := repo.EnsureCellAnalysisJob(ctx, service.CellAnalysisJobRequest{
		RunID: lease.RunID, CapabilityDomain: "coding", ModelRoute: "route-a", AnalysisVersion: "v1",
	})
	require.ErrorIs(t, err, service.ErrAggregatePairsIncomplete)
	require.Nil(t, job)

	var jobs int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_analysis_jobs
		WHERE run_id=$1 AND scope='cell' AND input_set_hash IS NOT NULL`, lease.RunID).Scan(&jobs))
	require.Zero(t, jobs)
}

func TestCellSnapshotRejectsDifferentScoreRefSet(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, runID := prepareAggregateCellFixture(t)
	aggregateRepo := NewEvaluationAggregateRepository(integrationDB)
	job, err := aggregateRepo.EnsureCellAnalysisJob(ctx, service.CellAnalysisJobRequest{
		RunID: runID, CapabilityDomain: "coding", ModelRoute: "candidate:route-a", AnalysisVersion: "v1",
	})
	require.NoError(t, err)
	require.Len(t, job.ScoreRefs, 2)
	configureAggregateStatisticsWorker(t, fixture.workerIDs[0])
	lease, err := gradingRepo.ClaimAnalysisJob(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)

	wrong := append([]service.ScoreRef(nil), job.ScoreRefs...)
	wrong[0].CreatedAt = wrong[0].CreatedAt.Add(time.Microsecond)
	_, err = gradingRepo.CompleteAnalysisJob(ctx, lease.ID, lease.Token, service.AggregateSubmission{
		RunID: runID, ScoreRefs: wrong, InputSetHash: job.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":0}`), LeaseEpoch: lease.LeaseEpoch,
	})
	require.ErrorIs(t, err, service.ErrAggregateInputMismatch)
}

func TestCurrentCellJobAdvancesAggregateHead(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, runID := prepareAggregateCellFixture(t)
	aggregateRepo := NewEvaluationAggregateRepository(integrationDB)
	job, err := aggregateRepo.EnsureCellAnalysisJob(ctx, service.CellAnalysisJobRequest{
		RunID: runID, CapabilityDomain: "coding", ModelRoute: "route-a", AnalysisVersion: "v1",
	})
	require.NoError(t, err)
	configureAggregateStatisticsWorker(t, fixture.workerIDs[0])
	lease, err := gradingRepo.ClaimAnalysisJob(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)

	snapshot, err := gradingRepo.CompleteAnalysisJob(ctx, lease.ID, lease.Token, service.AggregateSubmission{
		RunID: runID, ScoreRefs: job.ScoreRefs, InputSetHash: job.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-1.25}`), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, job.InputSetHash, snapshot.InputSetHash)
	require.Equal(t, job.AggregateRevision, snapshot.AggregateRevision)

	var head service.SnapshotRef
	var headHash string
	var revision int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT snapshot_id, window_start, input_set_hash, aggregate_revision
		FROM evaluation_aggregate_heads
		WHERE run_id=$1 AND capability_domain='coding'
		  AND canonical_model_route='route-a' AND analysis_version='v1'`, runID).Scan(
		&head.ID, &head.WindowStart, &headHash, &revision))
	require.Equal(t, snapshot.Ref, head)
	require.Equal(t, job.InputSetHash, headHash)
	require.Equal(t, job.AggregateRevision, revision)
	retried, err := gradingRepo.CompleteAnalysisJob(ctx, lease.ID, lease.Token, service.AggregateSubmission{
		RunID: runID, ScoreRefs: job.ScoreRefs, InputSetHash: job.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-1.25}`), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, snapshot.Ref, retried.Ref)
	require.True(t, retried.HeadAdvanced)
}

func TestAggregateHeadOutboxPreservesRepeatedContentAdvance(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, runID := prepareAggregateCellFixture(t)
	aggregateRepo := NewEvaluationAggregateRepository(integrationDB)
	firstJob, err := aggregateRepo.EnsureCellAnalysisJob(ctx, service.CellAnalysisJobRequest{
		RunID: runID, CapabilityDomain: "coding", ModelRoute: "route-a", AnalysisVersion: "v1",
	})
	require.NoError(t, err)
	configureAggregateStatisticsWorker(t, fixture.workerIDs[0])
	firstLease, err := gradingRepo.ClaimAnalysisJob(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	first, err := gradingRepo.CompleteAnalysisJob(ctx, firstLease.ID, firstLease.Token, service.AggregateSubmission{
		RunID: runID, ScoreRefs: firstJob.ScoreRefs, InputSetHash: firstJob.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-1}`), LeaseEpoch: firstLease.LeaseEpoch,
	})
	require.NoError(t, err)

	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_workers SET worker_kind='grader', capabilities=ARRAY['grader'] WHERE id=$1`, fixture.workerIDs[0])
	require.NoError(t, err)
	advanceAggregateFixtureScoreHead(t, gradingRepo, fixture.workerIDs[0], runID)
	secondJob, err := aggregateRepo.EnsureCellAnalysisJob(ctx, service.CellAnalysisJobRequest{
		RunID: runID, CapabilityDomain: "coding", ModelRoute: "route-a", AnalysisVersion: "v1",
	})
	require.NoError(t, err)
	configureAggregateStatisticsWorker(t, fixture.workerIDs[0])
	secondLease, err := gradingRepo.ClaimAnalysisJob(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	second, err := gradingRepo.CompleteAnalysisJob(ctx, secondLease.ID, secondLease.Token, service.AggregateSubmission{
		RunID: runID, ScoreRefs: secondJob.ScoreRefs, InputSetHash: secondJob.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-1}`), LeaseEpoch: secondLease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	var firstAggregateHash, secondAggregateHash string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT aggregate_hash FROM evaluation_aggregate_snapshots WHERE id=$1 AND window_start=$2`, first.ID, first.WindowStart).Scan(&firstAggregateHash))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT aggregate_hash FROM evaluation_aggregate_snapshots WHERE id=$1 AND window_start=$2`, second.ID, second.WindowStart).Scan(&secondAggregateHash))
	require.Equal(t, firstAggregateHash, secondAggregateHash)
	var eventCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events
		WHERE run_id=$1 AND event_type='global_recompute' AND source_type='aggregate_head'`, runID).Scan(&eventCount))
	require.Equal(t, 2, eventCount)
}

func TestStaleCellJobCannotAdvanceAggregateHead(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, runID := prepareAggregateCellFixture(t)
	aggregateRepo := NewEvaluationAggregateRepository(integrationDB)
	staleJob, err := aggregateRepo.EnsureCellAnalysisJob(ctx, service.CellAnalysisJobRequest{
		RunID: runID, CapabilityDomain: "coding", ModelRoute: "route-a", AnalysisVersion: "v1",
	})
	require.NoError(t, err)
	advanceAggregateFixtureScoreHead(t, gradingRepo, fixture.workerIDs[0], runID)
	currentJob, err := aggregateRepo.EnsureCellAnalysisJob(ctx, service.CellAnalysisJobRequest{
		RunID: runID, CapabilityDomain: "coding", ModelRoute: "route-a", AnalysisVersion: "v1",
	})
	require.NoError(t, err)
	require.NotEqual(t, staleJob.InputSetHash, currentJob.InputSetHash)
	require.Greater(t, currentJob.AggregateRevision, staleJob.AggregateRevision)

	configureAggregateStatisticsWorker(t, fixture.workerIDs[0])
	staleLease, err := gradingRepo.ClaimAnalysisJob(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, staleJob.ID, staleLease.ID)
	staleSnapshot, err := gradingRepo.CompleteAnalysisJob(ctx, staleLease.ID, staleLease.Token, service.AggregateSubmission{
		RunID: runID, ScoreRefs: staleJob.ScoreRefs, InputSetHash: staleJob.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-1}`), LeaseEpoch: staleLease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.False(t, staleSnapshot.HeadAdvanced)
	var heads int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_aggregate_heads
		WHERE run_id=$1 AND capability_domain='coding' AND canonical_model_route='route-a'`, runID).Scan(&heads))
	require.Zero(t, heads)

	currentLease, err := gradingRepo.ClaimAnalysisJob(ctx, fixture.workerIDs[0], []string{"coding"}, time.Minute)
	require.NoError(t, err)
	require.Equal(t, currentJob.ID, currentLease.ID)
	currentSnapshot, err := gradingRepo.CompleteAnalysisJob(ctx, currentLease.ID, currentLease.Token, service.AggregateSubmission{
		RunID: runID, ScoreRefs: currentJob.ScoreRefs, InputSetHash: currentJob.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-2}`), LeaseEpoch: currentLease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.True(t, currentSnapshot.HeadAdvanced)
}

func TestMultiCellRunRequiresExplicitGlobalSnapshot(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixtureWithCases(t, []evaluationCaseFixtureSpec{
		{capability: "coding", priority: "P0", sampleCount: 1, estimatedCost: decimal.RequireFromString("0.01")},
		{capability: "reasoning", priority: "P0", sampleCount: 1, estimatedCost: decimal.RequireFromString("0.01")},
	}, []map[string]any{{"route": "route-a"}}, decimal.RequireFromString("100"))
	gradingRepo := NewEvaluationGradingRepository(integrationDB)
	run, err := gradingRepo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	insertAggregateCellHeadFixture(t, run.ID, "coding", "route-a", 1)
	insertAggregateCellHeadFixture(t, run.ID, "reasoning", "route-a", 2)
	rows, err := integrationDB.QueryContext(ctx, `
		SELECT capability_domain, snapshot_id, aggregate_hash
		FROM evaluation_aggregate_heads WHERE run_id=$1 ORDER BY capability_domain`, run.ID)
	require.NoError(t, err)
	for rows.Next() {
		var domain, aggregateHash string
		var snapshotID uuid.UUID
		require.NoError(t, rows.Scan(&domain, &snapshotID, &aggregateHash))
		_, err = NewEvaluationOutboxRepository(integrationDB).Enqueue(ctx, service.EnqueueEvaluationOutboxInput{
			EventType: "global_recompute", RunID: run.ID, ScopeKey: domain + "/route-a",
			AnalysisVersion: "v1", SourceType: "aggregate_head", SourceID: snapshotID.String(),
			SourceHash: hashString("aggregate-head\x00" + snapshotID.String() + "\x00" + aggregateHash),
			Payload:    json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}
	require.NoError(t, rows.Close())

	aggregateRepo := NewEvaluationAggregateRepository(integrationDB)
	job, err := aggregateRepo.EnsureGlobalAnalysisJob(ctx, service.GlobalAnalysisJobRequest{
		RunID: run.ID, AnalysisVersion: "v1",
	})
	require.NoError(t, err)
	require.NotNil(t, job)
	require.Equal(t, "global", job.Scope)
	require.Equal(t, "global", job.CapabilityDomain)
	require.Equal(t, "global", job.CanonicalModelRoute)
	require.Len(t, job.SnapshotRefs, 2)

	configureAggregateStatisticsWorker(t, fixture.workerIDs[0])
	lease, err := gradingRepo.ClaimAnalysisJob(ctx, fixture.workerIDs[0], []string{"global"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, job.ID, lease.ID)
	global, err := gradingRepo.CompleteAnalysisJob(ctx, lease.ID, lease.Token, service.AggregateSubmission{
		RunID: run.ID, SnapshotRefs: job.SnapshotRefs, InputSetHash: job.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-1.5}`), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.True(t, global.HeadAdvanced)
	require.Len(t, global.SourceSnapshots, 2)

	var sourceCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_aggregate_snapshot_sources
		WHERE snapshot_id=$1 AND snapshot_window_start=$2`, global.Ref.ID, global.Ref.WindowStart).Scan(&sourceCount))
	require.Equal(t, 2, sourceCount)
	var head service.SnapshotRef
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT snapshot_id, window_start FROM evaluation_aggregate_heads
		WHERE run_id=$1 AND capability_domain='global'
		  AND canonical_model_route='global' AND analysis_version='v1'`, run.ID).Scan(
		&head.ID, &head.WindowStart))
	require.Equal(t, global.Ref, head)
}

func prepareAggregateCellFixture(t *testing.T) (evaluationRepositoryFixture, service.EvaluationGradingRepository, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	_, err := integrationDB.ExecContext(ctx, `
		UPDATE evaluation_workers SET worker_kind='grader', token_hash=$1, capabilities=ARRAY['grader']
		WHERE id=$2`, hashString(uuid.NewString()), fixture.workerIDs[0])
	require.NoError(t, err)
	gradingRepo := NewEvaluationGradingRepository(integrationDB)
	run, err := gradingRepo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	semanticsID := uuid.New()
	semanticsHash := hashString(uuid.NewString())
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_request_semantics (
			id, schema_version, canonical_semantics_bytes, request_semantics_sha256
		) VALUES ($1, 'radar-request-semantics-v1', convert_to('{}', 'UTF8'), $2)`, semanticsID, semanticsHash)
	require.NoError(t, err)
	var signingKeyID uuid.UUID
	err = integrationDB.QueryRowContext(ctx, `
		SELECT id FROM evaluation_evidence_signing_keys WHERE status='active' LIMIT 1`).Scan(&signingKeyID)
	if err == sql.ErrNoRows {
		signingKeyID = uuid.New()
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_evidence_signing_keys (id, key_reference, status, state_epoch)
			VALUES ($1,$2,'active',1)`, signingKeyID, "aggregate-test-key-"+uuid.NewString())
	}
	require.NoError(t, err)
	rows, err := integrationDB.QueryContext(ctx, `
		SELECT assignment.id, assignment.sample_id, sample.model_route,
		       pair_spec.request_manifest_id, pair_spec.request_manifest_sha256
		FROM evaluation_assignments assignment
		JOIN evaluation_samples sample ON sample.id=assignment.sample_id
		JOIN evaluation_side_specs side_spec ON side_spec.sample_id=sample.id
		JOIN evaluation_pair_specs pair_spec ON pair_spec.id=side_spec.pair_spec_id
		WHERE sample.run_id=$1 ORDER BY sample.model_route`, run.ID)
	require.NoError(t, err)
	type assignmentInput struct {
		assignmentID, sampleID, manifestID uuid.UUID
		modelRoute, manifestHash           string
	}
	assignments := make([]assignmentInput, 0, 2)
	for rows.Next() {
		var input assignmentInput
		require.NoError(t, rows.Scan(&input.assignmentID, &input.sampleID, &input.modelRoute, &input.manifestID, &input.manifestHash))
		assignments = append(assignments, input)
	}
	require.NoError(t, rows.Close())
	require.Len(t, assignments, 2)
	for ordinal, input := range assignments {
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
				$1,$2,$3,$4,$5,$6,'model-a','route-v1','provider-a','region-a',
				1,'[]'::jsonb,'stop',1,1,1,0.00000001,'succeeded',NOW(),NOW(),
				'radar-route-evidence-v1','rfc8785-v1',$7,0,0,$8,$9,'request-0',
				$10,$11,$12,$13,$14,1,NOW(),NOW(),$15,$16,$17,'complete','sha256:gateway'
			)`, "aggregate-sealed-"+uuid.NewString(), run.ID, input.sampleID, fixture.apiKeyID,
			"request-"+uuid.NewString(), input.modelRoute, input.assignmentID, input.manifestID,
			input.manifestHash, semanticsID, semanticsHash, strings.Repeat("2", 64),
			strings.Repeat("3", 64), strings.Repeat("4", 64), hashString(uuid.NewString()),
			signingKeyID, strings.Repeat("6", 64))
		require.NoError(t, err, "insert sealed evidence %d", ordinal)
		_, err = integrationDB.ExecContext(ctx, `
			UPDATE evaluation_assignments
			SET status='evidence_uploaded', lease_epoch=0, evidence_manifest='{}'::jsonb
			WHERE id=$1`, input.assignmentID)
		require.NoError(t, err)
	}
	for index := range assignments {
		lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
		require.NoError(t, err)
		require.NotNil(t, lease)
		_, err = gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
			Score: decimal.RequireFromString([]string{"0.80", "0.70"}[index]), LeaseEpoch: lease.LeaseEpoch,
		})
		require.NoError(t, err)
	}
	return fixture, gradingRepo, run.ID
}

func configureAggregateStatisticsWorker(t *testing.T, workerID uuid.UUID) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), `
		UPDATE evaluation_workers
		SET worker_kind='statistics', token_hash=$1, capabilities=ARRAY['coding','global']
		WHERE id=$2`, hashString(uuid.NewString()), workerID)
	require.NoError(t, err)
}

func advanceAggregateFixtureScoreHead(t *testing.T, gradingRepo service.EvaluationGradingRepository, workerID, runID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var jobID, assignmentID, sampleID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT job.id, job.assignment_id, job.sample_id
		FROM evaluation_grading_jobs job
		WHERE job.run_id=$1 AND job.status='completed'
		ORDER BY job.id LIMIT 1`, runID).Scan(&jobID, &assignmentID, &sampleID))
	_, err := integrationDB.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status='pending', score_id=NULL, score_created_at=NULL,
			lease_token_hash=NULL, leased_by=NULL, lease_expires_at=NULL,
			finished_at=NULL, updated_at=NOW()
		WHERE id=$1`, jobID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_assignments
		SET status='evidence_uploaded', finished_at=NULL, updated_at=NOW()
		WHERE id=$1`, assignmentID)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_samples SET status='evidence_uploaded', updated_at=NOW() WHERE id=$1`, sampleID)
	require.NoError(t, err)
	lease, err := gradingRepo.ClaimGradingLease(ctx, workerID, []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	_, err = gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.65"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
}

func insertAggregateCellHeadFixture(t *testing.T, runID uuid.UUID, domain, route string, revision int64) {
	t.Helper()
	ctx := context.Background()
	snapshotID := uuid.New()
	windowStart := time.Now().UTC().Add(time.Duration(revision) * time.Microsecond)
	inputHash := hashString(domain + route + "input")
	aggregateHash := hashString(domain + route + "aggregate")
	_, err := integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_aggregate_snapshots (
			id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, aggregate, input_set_hash, aggregate_revision, aggregate_hash
		) VALUES ($1,$2,$3,$4,'revision','v1',$5,'{}'::jsonb,$6,$7,$8)`,
		snapshotID, runID, domain, route, windowStart, inputHash, revision, aggregateHash)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_aggregate_heads (
			run_id, capability_domain, canonical_model_route, analysis_version,
			snapshot_id, window_start, aggregate_revision, input_set_hash, aggregate_hash
		) VALUES ($1,$2,$3,'v1',$4,$5,$6,$7,$8)`,
		runID, domain, route, snapshotID, windowStart, revision, inputHash, aggregateHash)
	require.NoError(t, err)
}
