//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestGlobalEventRecordsEveryCellCause(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	run, err := NewEvaluationRepository(integrationDB).CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)
	firstCause := insertEvaluationOutboxRootFixture(t, run.ID, "cell_head", strings.Repeat("1", 64))
	secondCause := insertEvaluationOutboxRootFixture(t, run.ID, "cell_head", strings.Repeat("2", 64))

	repo := NewEvaluationOutboxRepository(integrationDB)
	event, err := repo.Enqueue(ctx, service.EnqueueEvaluationOutboxInput{
		EventType: "global_recompute", RunID: run.ID, ScopeKey: "global/global",
		AnalysisVersion: "v1", SourceType: "aggregate_head_set", SourceID: "global/global",
		SourceHash: strings.Repeat("3", 64), Payload: json.RawMessage(`{"scope":"global"}`),
		Causes: []service.EvaluationOutboxCause{{EventID: secondCause}, {EventID: firstCause}},
	})
	require.NoError(t, err)
	require.NotNil(t, event)

	rows, err := integrationDB.QueryContext(ctx, `
		SELECT cause_event_id FROM evaluation_outbox_event_causes
		WHERE event_id=$1 ORDER BY cause_event_id`, event.ID)
	require.NoError(t, err)
	var causes []uuid.UUID
	for rows.Next() {
		var cause uuid.UUID
		require.NoError(t, rows.Scan(&cause))
		causes = append(causes, cause)
	}
	require.NoError(t, rows.Close())
	require.ElementsMatch(t, []uuid.UUID{firstCause, secondCause}, causes)
	expectedHash, err := service.CauseSetHash([]uuid.UUID{firstCause, secondCause})
	require.NoError(t, err)
	require.Equal(t, expectedHash, event.CauseSetHash)
}

func TestCellAggregateHeadEmitsEveryScoreCause(t *testing.T) {
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
	snapshot, err := gradingRepo.CompleteAnalysisJob(ctx, lease.ID, lease.Token, service.AggregateSubmission{
		RunID: runID, ScoreRefs: job.ScoreRefs, InputSetHash: job.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-1}`), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.True(t, snapshot.HeadAdvanced)

	var eventID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id FROM evaluation_outbox_events
		WHERE event_type='gate_reevaluation' AND source_type='aggregate_head'
		  AND source_id=$1`, snapshot.ID.String()).Scan(&eventID))
	var causes, sourceHeads int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*), COUNT(source_head_event_id)
		FROM evaluation_outbox_event_causes WHERE event_id=$1`, eventID).Scan(&causes, &sourceHeads))
	require.Equal(t, 2, causes)
	require.Equal(t, 2, sourceHeads)
}

func TestGlobalAggregateHeadEmitsEveryCellCause(t *testing.T) {
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
			SourceHash: aggregateHash, Payload: json.RawMessage(`{}`),
		})
		require.NoError(t, err)
	}
	require.NoError(t, rows.Close())

	aggregateRepo := NewEvaluationAggregateRepository(integrationDB)
	job, err := aggregateRepo.EnsureGlobalAnalysisJob(ctx, service.GlobalAnalysisJobRequest{
		RunID: run.ID, AnalysisVersion: "v1",
	})
	require.NoError(t, err)
	configureAggregateStatisticsWorker(t, fixture.workerIDs[0])
	lease, err := gradingRepo.ClaimAnalysisJob(ctx, fixture.workerIDs[0], []string{"global"}, time.Minute)
	require.NoError(t, err)
	snapshot, err := gradingRepo.CompleteAnalysisJob(ctx, lease.ID, lease.Token, service.AggregateSubmission{
		RunID: run.ID, SnapshotRefs: job.SnapshotRefs, InputSetHash: job.InputSetHash,
		Aggregate: json.RawMessage(`{"delta_pp":-1}`), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.True(t, snapshot.HeadAdvanced)

	var eventID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id FROM evaluation_outbox_events
		WHERE event_type='gate_reevaluation' AND source_type='aggregate_head'
		  AND source_id=$1`, snapshot.ID.String()).Scan(&eventID))
	var causes int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_event_causes WHERE event_id=$1`, eventID).Scan(&causes))
	require.Equal(t, 2, causes)
}

func TestRegradeOutboxRequiresSameRunBatch(t *testing.T) {
	ctx := context.Background()
	_, _, _, batch, _ := prepareRevisionBatchFixture(t)
	otherFixture := createEvaluationRepositoryFixture(t, 1, []string{"route-b"}, 1)
	otherRun, err := NewEvaluationRepository(integrationDB).CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: otherFixture.planID, TriggerSource: "manual", CreatedBy: otherFixture.userID,
	})
	require.NoError(t, err)

	_, err = NewEvaluationOutboxRepository(integrationDB).Enqueue(ctx, service.EnqueueEvaluationOutboxInput{
		EventType: "cell_recompute", RunID: otherRun.ID, ScopeKey: "coding/route-b",
		AnalysisVersion: "v1", SourceType: "score_head_event", SourceID: uuid.NewString(),
		SourceHash: strings.Repeat("4", 64), Payload: json.RawMessage(`{}`),
		WorkOrigin: "regrade", RevisionBatchID: batch.ID,
	})
	require.ErrorIs(t, err, service.ErrEvaluationOutboxBatchMismatch)
}

func TestBatchFenceRejectsOldOutboxHandlerCommit(t *testing.T) {
	ctx := context.Background()
	fixture, _, governance, batch, _ := prepareRevisionBatchFixture(t)
	repo := NewEvaluationOutboxRepository(integrationDB)
	event, err := repo.Enqueue(ctx, service.EnqueueEvaluationOutboxInput{
		EventType: "batch_fence_test", RunID: batch.RunID, ScopeKey: "coding/route-a",
		AnalysisVersion: "v1", SourceType: "score_head_event", SourceID: uuid.NewString(),
		SourceHash: strings.Repeat("5", 64), Payload: json.RawMessage(`{}`),
		WorkOrigin: "regrade", RevisionBatchID: batch.ID,
	})
	require.NoError(t, err)

	claimed, err := repo.Claim(ctx, fixture.workerIDs[0], []string{"batch_fence_test"}, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, event.ID, claimed[0].ID)
	require.Equal(t, batch.ControlEpoch, claimed[0].LeaseEpoch)

	fenced, err := governance.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "test fence", ActorID: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.NoError(t, err)
	require.Greater(t, fenced.ControlEpoch, claimed[0].LeaseEpoch)
	require.ErrorIs(t, repo.Complete(ctx, event.ID, claimed[0].LeaseToken, claimed[0].LeaseEpoch), service.ErrEvaluationOutboxFenced)

	reclaimed, err := repo.Claim(ctx, fixture.workerIDs[0], []string{"batch_fence_test"}, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	require.Equal(t, event.ID, reclaimed[0].ID)
	require.Equal(t, fenced.ControlEpoch, reclaimed[0].LeaseEpoch)
}

func TestClaimLocksBatchBeforeReadingEpoch(t *testing.T) {
	ctx := context.Background()
	fixture, _, _, batch, _ := prepareRevisionBatchFixture(t)
	repo := NewEvaluationOutboxRepository(integrationDB)
	event, err := repo.Enqueue(ctx, service.EnqueueEvaluationOutboxInput{
		EventType: "batch_claim_lock_test", RunID: batch.RunID, ScopeKey: "coding/route-a",
		AnalysisVersion: "v1", SourceType: "score_head_event", SourceID: uuid.NewString(),
		SourceHash: strings.Repeat("8", 64), Payload: json.RawMessage(`{}`),
		WorkOrigin: "regrade", RevisionBatchID: batch.ID,
	})
	require.NoError(t, err)

	_, err = integrationDB.ExecContext(ctx, `
		CREATE FUNCTION pause_outbox_claim_for_batch_lock_test() RETURNS trigger AS $$
		BEGIN
			PERFORM pg_sleep(0.5);
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql`)
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `
		CREATE TRIGGER pause_outbox_claim_for_batch_lock_test
		BEFORE UPDATE ON evaluation_outbox_events
		FOR EACH ROW WHEN (NEW.id = '`+event.ID.String()+`'::uuid)
		EXECUTE FUNCTION pause_outbox_claim_for_batch_lock_test()`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = integrationDB.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS pause_outbox_claim_for_batch_lock_test ON evaluation_outbox_events`)
		_, _ = integrationDB.ExecContext(context.Background(), `DROP FUNCTION IF EXISTS pause_outbox_claim_for_batch_lock_test()`)
	})

	type claimResult struct {
		events []service.EvaluationOutboxEvent
		err    error
	}
	claimed := make(chan claimResult, 1)
	go func() {
		events, claimErr := repo.Claim(context.Background(), fixture.workerIDs[0], []string{"batch_claim_lock_test"}, 1, time.Minute)
		claimed <- claimResult{events: events, err: claimErr}
	}()
	time.Sleep(100 * time.Millisecond)

	fenceDone := make(chan error, 1)
	go func() {
		fenceTx, fenceErr := beginRadarWriterTx(context.Background(), integrationDB, "api")
		if fenceErr == nil {
			defer func() { _ = fenceTx.Rollback() }()
			_, fenceErr = fenceTx.ExecContext(context.Background(), `
				UPDATE evaluation_revision_batches
				SET control_epoch=control_epoch+1, updated_at=NOW() WHERE id=$1`, batch.ID)
			if fenceErr == nil {
				fenceErr = fenceTx.Commit()
			}
		}
		fenceDone <- fenceErr
	}()

	select {
	case err := <-fenceDone:
		require.NoError(t, err)
		t.Fatal("fence advanced while the claim held its event update")
	case <-time.After(150 * time.Millisecond):
	}

	result := <-claimed
	require.NoError(t, result.err)
	require.Len(t, result.events, 1)
	require.Equal(t, event.ID, result.events[0].ID)
	require.Equal(t, batch.ControlEpoch, result.events[0].LeaseEpoch)
	require.NoError(t, <-fenceDone)
}

func TestOutboxDedupConflictRejectsAlteredCauseHeadMetadata(t *testing.T) {
	ctx := context.Background()
	_, _, runID := prepareAggregateCellFixture(t)
	rows, err := integrationDB.QueryContext(ctx, `
		SELECT id FROM evaluation_score_head_events WHERE run_id=$1 ORDER BY id`, runID)
	require.NoError(t, err)
	var headEvents []uuid.UUID
	for rows.Next() {
		var headEventID uuid.UUID
		require.NoError(t, rows.Scan(&headEventID))
		headEvents = append(headEvents, headEventID)
	}
	require.NoError(t, rows.Close())
	require.Len(t, headEvents, 2)

	causeID := insertEvaluationOutboxRootFixture(t, runID, "cause_fixture", strings.Repeat("9", 64))
	input := service.EnqueueEvaluationOutboxInput{
		EventType: "cause_metadata_test", RunID: runID, ScopeKey: "coding/route-a",
		AnalysisVersion: "v1", SourceType: "aggregate_head", SourceID: uuid.NewString(),
		SourceHash: strings.Repeat("a", 64), Payload: json.RawMessage(`{}`),
		Causes: []service.EvaluationOutboxCause{{EventID: causeID, SourceHeadEventID: headEvents[0]}},
	}
	_, err = NewEvaluationOutboxRepository(integrationDB).Enqueue(ctx, input)
	require.NoError(t, err)
	input.Causes[0].SourceHeadEventID = headEvents[1]
	_, err = NewEvaluationOutboxRepository(integrationDB).Enqueue(ctx, input)
	require.ErrorIs(t, err, service.ErrEvaluationOutboxDedupConflict)
}

func TestDeadLetterReplayKeepsEventIdentityAndCauses(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	run, err := NewEvaluationRepository(integrationDB).CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)
	causeID := insertEvaluationOutboxRootFixture(t, run.ID, "cell_head", strings.Repeat("6", 64))
	repo := NewEvaluationOutboxRepository(integrationDB)
	event, err := repo.Enqueue(ctx, service.EnqueueEvaluationOutboxInput{
		EventType: "dead_letter_replay_test", RunID: run.ID, ScopeKey: "global/global",
		AnalysisVersion: "v1", SourceType: "aggregate_head", SourceID: uuid.NewString(),
		SourceHash: strings.Repeat("7", 64), Payload: json.RawMessage(`{}`),
		Causes: []service.EvaluationOutboxCause{{EventID: causeID}},
	})
	require.NoError(t, err)
	rootClaim, err := repo.Claim(ctx, fixture.workerIDs[0], []string{"root_fixture"}, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, rootClaim, 1)
	require.Equal(t, causeID, rootClaim[0].ID)
	require.NoError(t, repo.Complete(ctx, causeID, rootClaim[0].LeaseToken, rootClaim[0].LeaseEpoch))
	claimed, err := repo.Claim(ctx, fixture.workerIDs[0], []string{"dead_letter_replay_test"}, 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.Equal(t, event.ID, claimed[0].ID)
	require.NoError(t, repo.DeadLetter(ctx, event.ID, claimed[0].LeaseToken, claimed[0].LeaseEpoch, "handler_failed"))

	replayed, err := repo.ReplayDeadLetter(ctx, event.ID)
	require.NoError(t, err)
	require.Equal(t, event.ID, replayed.ID)
	require.Equal(t, event.DedupKey, replayed.DedupKey)
	require.Equal(t, service.EvaluationOutboxPending, replayed.Status)
	var causeCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_event_causes WHERE event_id=$1`, event.ID).Scan(&causeCount))
	require.Equal(t, 1, causeCount)
}

func TestEvaluationRepositoryFixtureCleanupRemovesOutboxEvents(t *testing.T) {
	ctx := context.Background()
	var causeID, eventID uuid.UUID

	t.Run("create fixture outbox event and cause", func(t *testing.T) {
		fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
		run, err := NewEvaluationRepository(integrationDB).CreateRunWithMatrix(ctx, service.CreateRunInput{
			PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
		})
		require.NoError(t, err)
		causeID = insertEvaluationOutboxRootFixture(t, run.ID, "fixture_cleanup", strings.Repeat("8", 64))
		event, err := NewEvaluationOutboxRepository(integrationDB).Enqueue(ctx, service.EnqueueEvaluationOutboxInput{
			EventType: "fixture_cleanup_child", RunID: run.ID, ScopeKey: "global/global",
			AnalysisVersion: "v1", SourceType: "fixture_cleanup", SourceID: uuid.NewString(),
			SourceHash: strings.Repeat("9", 64), Payload: json.RawMessage(`{}`),
			Causes: []service.EvaluationOutboxCause{{EventID: causeID}},
		})
		require.NoError(t, err)
		eventID = event.ID
		var causes int
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM evaluation_outbox_event_causes
			WHERE event_id=$1 AND cause_event_id=$2`, eventID, causeID).Scan(&causes))
		require.Equal(t, 1, causes)
	})

	var remainingEvents, remainingCauses int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events WHERE id IN ($1,$2)`, causeID, eventID).Scan(&remainingEvents))
	require.Zero(t, remainingEvents, "fixture cleanup must remove events before it removes the run")
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_event_causes
		WHERE event_id=$1 AND cause_event_id=$2`, eventID, causeID).Scan(&remainingCauses))
	require.Zero(t, remainingCauses, "fixture cleanup must remove outbox cause relationships before it removes the run")
}

func insertEvaluationOutboxRootFixture(t *testing.T, runID uuid.UUID, sourceType, sourceHash string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	rootHash := hashString("outbox-root:" + id.String())
	_, err := integrationDB.ExecContext(context.Background(), `
		INSERT INTO evaluation_outbox_events (
			id, event_type, dedup_key, causation_id, cause_set_hash, run_id,
			source_type, source_id, source_hash, payload_hash, payload
		) VALUES ($1,'root_fixture',$2,$3,$3,$4,$5,$6,$7,$8,'{}'::jsonb)`,
		id, hashString("outbox-dedup:"+id.String()), rootHash, runID, sourceType,
		id.String(), sourceHash, hashString("{}"))
	require.NoError(t, err)
	return id
}
