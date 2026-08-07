//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestReplacementInvalidatesOldScoreAndAggregateHeads(t *testing.T) {
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

	var sampleID, oldAssignmentID uuid.UUID
	var oldAttempt int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT head.sample_id, event.source_assignment_id, assignment.attempt
		FROM evaluation_score_heads head
		JOIN evaluation_score_head_events event
		  ON event.sample_id=head.sample_id AND event.grader_id=head.grader_id AND event.version=head.version
		JOIN evaluation_assignments assignment ON assignment.id=event.source_assignment_id
		WHERE event.run_id=$1 ORDER BY head.sample_id LIMIT 1`, runID).Scan(&sampleID, &oldAssignmentID, &oldAttempt))
	insertAlternateCurrentScoreHead(t, runID, sampleID, oldAssignmentID)
	affectedJobID, unrelatedJobID := insertReplacementAnalysisJobs(t, runID, oldAssignmentID)
	newAssignmentID := uuid.New()
	tx, err := beginRadarWriterTx(ctx, integrationDB, "scheduler")
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status)
		VALUES ($1,$2,$3,$4,'pending')`, newAssignmentID, sampleID, oldAttempt+1, hashString(uuid.NewString()))
	require.NoError(t, err)
	require.NoError(t, propagateAssignmentReplacement(ctx, tx, runID, sampleID, oldAssignmentID, newAssignmentID, oldAttempt))
	matches, err := currentAnalysisInputMatches(ctx, tx, runID, "cell", "coding", "route-a", "v1", job.InputSetHash)
	require.NoError(t, err)
	require.False(t, matches)
	require.NoError(t, tx.Commit())

	var persistedSnapshot uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT snapshot_id FROM evaluation_aggregate_heads
		WHERE run_id=$1 AND capability_domain='coding' AND canonical_model_route='route-a'`, runID).Scan(&persistedSnapshot))
	require.Equal(t, snapshot.ID, persistedSnapshot, "historical aggregate head remains append-only")
	var replacementEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events
		WHERE run_id=$1 AND source_type='assignment_replacement' AND source_id=$2`, runID, newAssignmentID.String()).Scan(&replacementEvents))
	require.Equal(t, 2, replacementEvents)
	var affectedStatus, unrelatedStatus string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_analysis_jobs WHERE id=$1`, affectedJobID).Scan(&affectedStatus))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_analysis_jobs WHERE id=$1`, unrelatedJobID).Scan(&unrelatedStatus))
	require.Equal(t, "failed", affectedStatus)
	require.Equal(t, "pending", unrelatedStatus, "replacement cannot invalidate an unrelated frozen analysis input")
}

func insertReplacementAnalysisJobs(t *testing.T, runID, affectedAssignmentID uuid.UUID) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	type scoreInput struct {
		id        uuid.UUID
		createdAt time.Time
	}
	var affected, unrelated scoreInput
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id, created_at FROM evaluation_scores
		WHERE run_id=$1 AND source_assignment_id=$2 ORDER BY created_at LIMIT 1`,
		runID, affectedAssignmentID).Scan(&affected.id, &affected.createdAt))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id, created_at FROM evaluation_scores
		WHERE run_id=$1 AND source_assignment_id<>$2 ORDER BY created_at LIMIT 1`,
		runID, affectedAssignmentID).Scan(&unrelated.id, &unrelated.createdAt))
	insertJob := func(input scoreInput, suffix string) uuid.UUID {
		jobID := uuid.New()
		_, err := integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_analysis_jobs (
				id, run_id, capability_domain, model_route, "window", analysis_version,
				window_start, status, scope, work_origin, input_set_hash
			) VALUES ($1,$2,'coding',$3,'revision',$4,NOW(),'pending','cell','initial',$5)`,
			jobID, runID, "replacement-"+suffix, "replacement-"+suffix, hashString(jobID.String()))
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_analysis_job_score_inputs (
				analysis_job_id, input_ordinal, score_id, score_created_at
			) VALUES ($1,0,$2,$3)`, jobID, input.id, input.createdAt)
		require.NoError(t, err)
		return jobID
	}
	return insertJob(affected, "affected"), insertJob(unrelated, "unrelated")
}

func TestRunFencePropagatesAssignmentReplacement(t *testing.T) {
	ctx := context.Background()
	fixture, _, runID := prepareAggregateCellFixture(t)
	var sampleID, caseID uuid.UUID
	var modelRoute string
	var sampleIndex, currentAttempt int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT sample.id, sample.case_id, sample.model_route, sample.sample_index, MAX(assignment.attempt)
		FROM evaluation_samples sample
		JOIN evaluation_assignments assignment ON assignment.sample_id=sample.id
		WHERE sample.run_id=$1
		GROUP BY sample.id, sample.case_id, sample.model_route, sample.sample_index
		ORDER BY sample.id LIMIT 1`, runID).Scan(
		&sampleID, &caseID, &modelRoute, &sampleIndex, &currentAttempt))
	leasedAssignmentID := uuid.New()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	require.NoError(t, func() error {
		if _, err := tx.ExecContext(ctx, `SET LOCAL session_replication_role=replica`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_assignments (
				id, sample_id, attempt, idempotency_key, status, lease_token_hash,
				leased_by, lease_expires_at, lease_epoch, work_origin
			) VALUES ($1,$2,$3,$4,'leased',$5,$6,NOW()+INTERVAL '1 minute',1,NULL)`,
			leasedAssignmentID, sampleID, currentAttempt+1,
			assignmentIdempotencyKey(runID, caseID, modelRoute, sampleIndex, currentAttempt+1),
			hashToken("fence-replacement-"+uuid.NewString()), fixture.workerIDs[1]); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status='leased' WHERE id=$1`, sampleID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_runs SET status='running', finished_at=NULL, updated_at=NOW() WHERE id=$1`, runID); err != nil {
			return err
		}
		return tx.Commit()
	}())

	result, err := NewRadarGovernanceRepository(integrationDB).(service.RunControlRepository).FenceRun(
		ctx, runID, "incident", fixture.userID, hashString(uuid.NewString()),
	)
	require.NoError(t, err)
	require.Len(t, result.ReplacementIDs, 1)
	var replacementOrigin string
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT work_origin FROM evaluation_assignments WHERE id=$1`, result.ReplacementIDs[0]).Scan(&replacementOrigin))
	require.Equal(t, "initial", replacementOrigin)
	var replacementEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events
		WHERE run_id=$1 AND source_type='assignment_replacement' AND source_id=$2`,
		runID, result.ReplacementIDs[0].String()).Scan(&replacementEvents))
	require.Positive(t, replacementEvents)
}

func insertAlternateCurrentScoreHead(t *testing.T, runID, sampleID, assignmentID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := beginRadarWriterTx(ctx, integrationDB, "worker")
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	scoreID := uuid.New()
	var scoreCreatedAt time.Time
	evidenceSetHash := hashString("alternate-head-" + sampleID.String())
	require.NoError(t, tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_scores (
			id, run_id, sample_id, grader_id, grader_version, version, score,
			submission_idempotency_key, source_assignment_id, route_evidence_set_hash
		) VALUES ($1,$2,$3,'alternate-grader','v1',1,0.5,$4,$5,$6)
		RETURNING created_at`, scoreID, runID, sampleID, hashString(uuid.NewString()), assignmentID, evidenceSetHash).Scan(&scoreCreatedAt))
	headEventID := uuid.New()
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_score_head_events (
			id, run_id, sample_id, grader_id, version, score_id, score_created_at,
			source_assignment_id, route_evidence_set_hash, reason
		) VALUES ($1,$2,$3,'alternate-grader',1,$4,$5,$6,$7,'initial')`,
		headEventID, runID, sampleID, scoreID, scoreCreatedAt, assignmentID, evidenceSetHash))
	require.NoError(t, execRadarFixtureSQL(ctx, tx, `
		INSERT INTO evaluation_score_heads (
			sample_id, grader_id, score_id, score_created_at, version
		) VALUES ($1,'alternate-grader',$2,$3,1)`, sampleID, scoreID, scoreCreatedAt))
	require.NoError(t, enqueueScoreHeadRecompute(
		ctx, tx, runID, "route-a", "coding", headEventID, evidenceSetHash, "initial", nil, nil,
	))
	require.NoError(t, tx.Commit())
}

func TestHeadAdvanceAppendsCellGlobalAndGateRequirements(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, _, batch, _ := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	_, err = gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.61"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	scoreEvent := revisionBatchOutboxEvent(t, batch.ID, "cell_recompute")
	_ = appendRevisionPipelineEvent(t, batch, "projection_refresh", "score_head_event", scoreEvent.SourceID, scoreEvent)
	var cellStatus string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='cell'`, batch.ID).Scan(&cellStatus))
	require.Equal(t, "pending", cellStatus, "unrelated outbox events cannot close a propagation requirement")
	insertAggregateCellHeadFixture(t, batch.RunID, "coding", "route-a", 1)
	var currentSnapshotID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT snapshot_id FROM evaluation_aggregate_heads
		WHERE run_id=$1 AND capability_domain='coding' AND canonical_model_route='route-a'`, batch.RunID).Scan(&currentSnapshotID))
	globalEvent := appendRevisionPipelineEvent(t, batch, "global_recompute", "aggregate_head", currentSnapshotID.String(), scoreEvent)
	_ = appendRevisionPipelineEvent(t, batch, "gate_reevaluation", "aggregate_head", uuid.NewString(), globalEvent)

	rows, err := integrationDB.QueryContext(ctx, `
		SELECT requirement_type, status FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type<>'grading' ORDER BY created_at`, batch.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := make([]string, 0, 3)
	for rows.Next() {
		var requirementType, status string
		require.NoError(t, rows.Scan(&requirementType, &status))
		got = append(got, requirementType+":"+status)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"cell:completed", "global:completed", "gate:pending"}, got)
}

func TestPartialPropagationFailureBlocksBatch(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, _, batch, _ := prepareRevisionBatchFixture(t)
	const firstTenantID int64 = 771001
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	_, err = gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.62"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_runs SET tenant_id=$2 WHERE id=$1`, batch.RunID, firstTenantID)
	require.NoError(t, err)
	event := revisionBatchOutboxEvent(t, batch.ID, "cell_recompute")
	token := leaseRevisionPipelineEvent(t, event.ID, batch.ControlEpoch)
	require.NoError(t, NewEvaluationOutboxRepository(integrationDB).DeadLetter(ctx, event.ID, token, batch.ControlEpoch, "statistics_failed"))

	var status service.RevisionBatchStatus
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_revision_batches WHERE id=$1`, batch.ID).Scan(&status))
	require.Equal(t, service.RevisionBatchBlocked, status)
	var requirementStatus, failureCode string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, failure_code FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='cell'`, batch.ID).Scan(&requirementStatus, &failureCode))
	require.Equal(t, "failed", requirementStatus)
	require.Equal(t, "statistics_failed", failureCode)
	var alertCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_alerts
		WHERE tenant_id=$1 AND cause='insufficient_evidence' AND status='open'`, firstTenantID).Scan(&alertCount))
	require.Positive(t, alertCount)

	failedFixture, failedGradingRepo, _, failedBatch, _ := prepareRevisionBatchFixture(t)
	const secondTenantID int64 = 771002
	failedLease, err := failedGradingRepo.ClaimGradingLease(ctx, failedFixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NoError(t, failedGradingRepo.FailGradingLease(
		ctx, failedLease.ID, failedLease.Token, "permanent", "grader_invalid_output", failedLease.LeaseEpoch,
	))
	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_runs SET tenant_id=$2 WHERE id=$1`, failedBatch.RunID, secondTenantID)
	require.NoError(t, err)
	var failedBatchStatus service.RevisionBatchStatus
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status FROM evaluation_revision_batches WHERE id=$1`, failedBatch.ID).Scan(&failedBatchStatus))
	require.Equal(t, service.RevisionBatchFailed, failedBatchStatus)
	var runStatus, assignmentStatus, sampleStatus string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_runs WHERE id=$1`, failedLease.RunID).Scan(&runStatus))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_assignments WHERE id=$1`, failedLease.AssignmentID).Scan(&assignmentStatus))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_samples WHERE id=$1`, failedLease.SampleID).Scan(&sampleStatus))
	require.Equal(t, "completed", runStatus)
	require.Equal(t, "completed", assignmentStatus)
	require.Equal(t, "completed", sampleStatus)
}

func TestRepairSupersedesFailedRequirementAndCompletesOriginalCauseChain(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, repo, batch, _ := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	_, err = gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.63"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	event := revisionBatchOutboxEvent(t, batch.ID, "cell_recompute")
	token := leaseRevisionPipelineEvent(t, event.ID, batch.ControlEpoch)
	require.NoError(t, NewEvaluationOutboxRepository(integrationDB).DeadLetter(ctx, event.ID, token, batch.ControlEpoch, "statistics_failed"))
	_, err = NewEvaluationOutboxRepository(integrationDB).ReplayDeadLetter(ctx, event.ID)
	require.ErrorIs(t, err, service.ErrRevisionBatchNotRepairable)
	var deadLetterStatus string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_outbox_events WHERE id=$1`, event.ID).Scan(&deadLetterStatus))
	require.Equal(t, "dead_letter", deadLetterStatus)

	repaired, err := repo.RepairRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "repair propagation", ActorID: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.NoError(t, err)
	require.Equal(t, service.RevisionBatchRunning, repaired.Status)
	var oldStatus, replacementStatus string
	var oldID, replacesID uuid.UUID
	var generation int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT old.id, old.status, replacement.status, replacement.recovery_generation, replacement.replaces_requirement_id
		FROM evaluation_revision_batch_requirements old
		JOIN evaluation_revision_batch_requirements replacement ON replacement.replaces_requirement_id=old.id
		WHERE old.revision_batch_id=$1 AND old.requirement_type='cell'`, batch.ID).Scan(
		&oldID, &oldStatus, &replacementStatus, &generation, &replacesID))
	require.Equal(t, "superseded", oldStatus)
	require.Equal(t, "pending", replacementStatus)
	require.Equal(t, 1, generation)
	require.Equal(t, oldID, replacesID)
	var outboxStatus string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_outbox_events WHERE id=$1`, event.ID).Scan(&outboxStatus))
	require.Equal(t, "pending", outboxStatus)
}

func TestBatchCompletesOnlyAfterAllCurrentHeadsAndDecisionCoverCauseSet(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, _, batch, _ := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	_, err = gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.64"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	scoreEvent := revisionBatchOutboxEvent(t, batch.ID, "cell_recompute")
	insertAggregateCellHeadFixture(t, batch.RunID, "coding", "route-a", 1)
	var currentSnapshotID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT snapshot_id FROM evaluation_aggregate_heads
		WHERE run_id=$1 AND capability_domain='coding' AND canonical_model_route='route-a'`, batch.RunID).Scan(&currentSnapshotID))
	globalEvent := appendRevisionPipelineEvent(t, batch, "global_recompute", "aggregate_head", currentSnapshotID.String(), scoreEvent)
	gateEvent := appendRevisionPipelineEvent(t, batch, "gate_reevaluation", "aggregate_head", uuid.NewString(), globalEvent)
	var causeSetHash string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT cause_set_hash FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='gate'`, batch.ID).Scan(&causeSetHash))
	policyID := insertRevisionPipelinePolicy(t, fixture.userID)
	releaseSubjectHash := hashString("release-" + batch.ID.String())
	historical, err := NewRadarGovernanceRepository(integrationDB).RecordGateDecision(ctx, service.RadarGateDecisionInput{
		RunID: batch.RunID, PolicyID: policyID, Status: service.RadarGatePassed,
		Evidence: json.RawMessage(`{}`), EvidenceHash: hashString(uuid.NewString()),
		ReleaseSubjectHash: releaseSubjectHash, SourceWatermark: json.RawMessage(`{}`),
		CauseSetHash: causeSetHash,
	})
	require.NoError(t, err)
	currentWrongCause, err := NewRadarGovernanceRepository(integrationDB).RecordGateDecision(ctx, service.RadarGateDecisionInput{
		RunID: batch.RunID, PolicyID: policyID, Status: service.RadarGatePassed,
		Evidence: json.RawMessage(`{}`), EvidenceHash: hashString(uuid.NewString()),
		ReleaseSubjectHash: releaseSubjectHash, SourceWatermark: json.RawMessage(`{}`),
		SupersedesDecisionID: &historical.ID, CauseSetHash: hashString("incomplete-cause-set"),
	})
	require.NoError(t, err)
	token := leaseRevisionPipelineEvent(t, gateEvent.ID, batch.ControlEpoch)
	require.NoError(t, NewEvaluationOutboxRepository(integrationDB).Complete(ctx, gateEvent.ID, token, batch.ControlEpoch))

	var status service.RevisionBatchStatus
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_revision_batches WHERE id=$1`, batch.ID).Scan(&status))
	require.Equal(t, service.RevisionBatchRunning, status, "a historical matching Decision cannot cover the current Gate Head")
	decision, err := NewRadarGovernanceRepository(integrationDB).RecordGateDecision(ctx, service.RadarGateDecisionInput{
		RunID: batch.RunID, PolicyID: policyID, Status: service.RadarGatePassed,
		Evidence: json.RawMessage(`{}`), EvidenceHash: hashString(uuid.NewString()),
		ReleaseSubjectHash: releaseSubjectHash, SourceWatermark: json.RawMessage(`{}`),
		SupersedesDecisionID: &currentWrongCause.ID, CauseSetHash: causeSetHash,
	})
	require.NoError(t, err)
	var currentDecisionID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT decision_id FROM evaluation_gate_decision_heads
		WHERE run_id=$1 AND policy_id=$2 AND release_subject_hash=$3`,
		batch.RunID, policyID, decision.ReleaseSubjectHash).Scan(&currentDecisionID))
	require.Equal(t, decision.ID, currentDecisionID)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_revision_batches WHERE id=$1`, batch.ID).Scan(&status))
	require.Equal(t, service.RevisionBatchCompleted, status)
}

func TestBatchGateDecisionRequiresEveryFrozenGradingCause(t *testing.T) {
	ctx := context.Background()
	fixture, _, _, batch, _ := prepareRevisionBatchFixture(t)
	tx, err := beginRadarWriterTx(ctx, integrationDB, "worker")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_revision_batch_requirements
		SET status='completed', completed_at=NOW(), updated_at=NOW()
		WHERE revision_batch_id=$1 AND requirement_type='grading' AND status='pending'`, batch.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var initialEventID, initialHeadID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id, source_id::uuid
		FROM evaluation_outbox_events
		WHERE run_id=$1 AND source_type='score_head_event' AND revision_batch_id IS NULL
		ORDER BY sequence LIMIT 1`, batch.RunID).Scan(&initialEventID, &initialHeadID))
	initialEvent, err := loadEvaluationOutboxEvent(ctx, integrationDB, initialEventID)
	require.NoError(t, err)
	cellEvent := appendRevisionPipelineEvent(t, batch, "cell_recompute", "score_head_event", initialHeadID.String(), *initialEvent)
	insertAggregateCellHeadFixture(t, batch.RunID, "coding", "route-a", 1)
	var snapshotID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT snapshot_id FROM evaluation_aggregate_heads
		WHERE run_id=$1 AND capability_domain='coding' AND canonical_model_route='route-a'`, batch.RunID).Scan(&snapshotID))
	globalEvent := appendRevisionPipelineEvent(t, batch, "global_recompute", "aggregate_head", snapshotID.String(), cellEvent)
	gateEvent := appendRevisionPipelineEvent(t, batch, "gate_reevaluation", "aggregate_head", uuid.NewString(), globalEvent)
	token := leaseRevisionPipelineEvent(t, gateEvent.ID, batch.ControlEpoch)
	require.NoError(t, NewEvaluationOutboxRepository(integrationDB).Complete(ctx, gateEvent.ID, token, batch.ControlEpoch))

	var causeSetHash string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT cause_set_hash FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='gate'`, batch.ID).Scan(&causeSetHash))
	policyID := insertRevisionPipelinePolicy(t, fixture.userID)
	_, err = NewRadarGovernanceRepository(integrationDB).RecordGateDecision(ctx, service.RadarGateDecisionInput{
		RunID: batch.RunID, PolicyID: policyID, Status: service.RadarGatePassed,
		Evidence: json.RawMessage(`{}`), EvidenceHash: hashString(uuid.NewString()),
		ReleaseSubjectHash: hashString("release-" + batch.ID.String()), SourceWatermark: json.RawMessage(`{}`),
		CauseSetHash: causeSetHash,
	})
	require.NoError(t, err)
	var status service.RevisionBatchStatus
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status FROM evaluation_revision_batches WHERE id=$1`, batch.ID).Scan(&status))
	require.Equal(t, service.RevisionBatchRunning, status, "an initial Score Head cannot cover a frozen regrade cause")
}

func TestNewBatchIsNotSuppressedByHistoricalGeneration(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, first, _ := prepareRevisionBatchFixture(t)
	failedID := blockRevisionBatchGrading(t, first.ID)
	_, err := repo.RepairRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: first.ID, Reason: "repair generation", ActorID: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.NoError(t, err)
	tx, err := beginRadarWriterTx(ctx, integrationDB, "api")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_revision_batch_requirements SET status='completed', completed_at=NOW(), updated_at=NOW()
		WHERE revision_batch_id=$1 AND status='pending'`, first.ID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_revision_batches SET status='completed', finished_at=NOW(), updated_at=NOW() WHERE id=$1`, first.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	second, err := repo.CreateRevisionBatch(ctx, service.CreateRevisionBatchInput{
		RunID: first.RunID, Reason: "second independent regression", RequestedBy: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	var generation, count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT MIN(recovery_generation), COUNT(*) FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='grading'`, second.ID).Scan(&generation, &count))
	require.Zero(t, generation)
	require.Positive(t, count)
	var historicalGeneration int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT recovery_generation FROM evaluation_revision_batch_requirements
		WHERE replaces_requirement_id=$1`, failedID).Scan(&historicalGeneration))
	require.Equal(t, 1, historicalGeneration)
}

func revisionBatchOutboxEvent(t *testing.T, batchID uuid.UUID, eventType string) service.EvaluationOutboxEvent {
	t.Helper()
	var eventID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT id FROM evaluation_outbox_events
		WHERE revision_batch_id=$1 AND event_type=$2 ORDER BY sequence DESC LIMIT 1`, batchID, eventType).Scan(&eventID))
	event, err := loadEvaluationOutboxEvent(context.Background(), integrationDB, eventID)
	require.NoError(t, err)
	return *event
}

func appendRevisionPipelineEvent(t *testing.T, batch *service.RevisionBatch, eventType, sourceType, sourceID string, cause service.EvaluationOutboxEvent) service.EvaluationOutboxEvent {
	t.Helper()
	tx, err := beginRadarWriterTx(context.Background(), integrationDB, "worker")
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	sourceHeadID := uuid.Nil
	if cause.SourceType == "score_head_event" {
		sourceHeadID, err = uuid.Parse(cause.SourceID)
		require.NoError(t, err)
	}
	event, err := enqueueEvaluationOutbox(context.Background(), tx, service.EnqueueEvaluationOutboxInput{
		EventType: eventType, RunID: batch.RunID, ScopeKey: "coding/route-a", AnalysisVersion: "v1",
		SourceType: sourceType, SourceID: sourceID, SourceHash: hashString(eventType + sourceID),
		Payload: json.RawMessage(`{}`), WorkOrigin: "regrade", RevisionBatchID: batch.ID,
		Causes: []service.EvaluationOutboxCause{{EventID: cause.ID, SourceHeadEventID: sourceHeadID}},
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return *event
}

func leaseRevisionPipelineEvent(t *testing.T, eventID uuid.UUID, epoch int64) string {
	t.Helper()
	token := "revision-pipeline-test-token-" + uuid.NewString()
	var workerID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT id FROM evaluation_workers WHERE status='active' ORDER BY created_at DESC LIMIT 1`).Scan(&workerID))
	tx, err := beginRadarWriterTx(context.Background(), integrationDB, "api")
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), `
		UPDATE evaluation_outbox_events
		SET status='leased', lease_token_hash=$2, lease_owner=$3, lease_expires_at=NOW()+INTERVAL '1 minute', lease_epoch=$4
		WHERE id=$1`, eventID, hashToken(token), workerID, epoch)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return token
}

func insertRevisionPipelinePolicy(t *testing.T, actorID int64) uuid.UUID {
	t.Helper()
	policyID := uuid.New()
	version := int(time.Now().UnixNano()%1_000_000_000) + 1
	tx, err := beginRadarWriterTx(context.Background(), integrationDB, "api")
	require.NoError(t, err)
	_, err = tx.ExecContext(context.Background(), `
		INSERT INTO evaluation_gate_policies (id, version, policy, policy_hash, enforcement_starts_at, created_by)
		VALUES ($1,$2,'{}'::jsonb,$3,NOW(),$4)`, policyID, version, hashString(fmt.Sprint(policyID)), actorID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return policyID
}
