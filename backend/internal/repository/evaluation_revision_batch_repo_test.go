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

func TestCreateRevisionBatchRequiresCompletedRun(t *testing.T) {
	ctx := context.Background()
	_, _, lease := prepareSealedGradingLease(t)
	repo := NewRadarGovernanceRepository(integrationDB)

	_, err := repo.(service.RevisionBatchRepository).CreateRevisionBatch(ctx, service.CreateRevisionBatchInput{
		RunID: lease.RunID, Reason: "model regression", RequestedBy: 1,
		IdempotencyKey: strings.Repeat("a", 64),
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchRunNotCompleted)
}

func TestCreateRevisionBatchFreezesGradingRequirements(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, lease := prepareSealedGradingLease(t)
	_, err := gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	completeRevisionTestRun(t, lease.RunID)

	repo := NewRadarGovernanceRepository(integrationDB).(service.RevisionBatchRepository)
	batch, err := repo.CreateRevisionBatch(ctx, service.CreateRevisionBatchInput{
		RunID: lease.RunID, Reason: "model regression", RequestedBy: fixture.userID,
		IdempotencyKey: strings.Repeat("b", 64),
	})
	require.NoError(t, err)
	require.Equal(t, service.RevisionBatchRunning, batch.Status)

	var requirements, jobs int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id = $1 AND requirement_type = 'grading'
		  AND source_assignment_id IS NOT NULL AND previous_score_id IS NOT NULL
		  AND previous_score_created_at IS NOT NULL AND grading_input_hash IS NOT NULL`, batch.ID).Scan(&requirements))
	require.Positive(t, requirements)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_grading_jobs
		WHERE revision_batch_id = $1 AND work_origin = 'regrade' AND recovery_generation = 0`, batch.ID).Scan(&jobs))
	require.Equal(t, requirements, jobs)
}

func completeRevisionTestRun(t *testing.T, runID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, &sql.TxOptions{})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var version, epoch int64
	var status string
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT status, state_version, control_epoch FROM evaluation_runs WHERE id=$1 FOR UPDATE`, runID).Scan(&status, &version, &epoch))
	require.Equal(t, "pending", status)
	firstKey := hashString(runID.String() + ":revision-test-start")
	_, err = tx.ExecContext(ctx, `
		INSERT INTO evaluation_run_events (
			id, run_id, event_type, payload, actor_type, actor_ref,
			transition_version, from_status, to_status, control_epoch, idempotency_key
		) VALUES ($1,$2,'test_start','{}','system','revision-test',$3,'pending','running',$4,$5)`,
		uuid.New(), runID, version+1, epoch, firstKey)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE evaluation_runs SET status='running', state_version=$2, started_at=NOW(), updated_at=NOW() WHERE id=$1`, runID, version+1)
	require.NoError(t, err)
	secondKey := hashString(runID.String() + ":revision-test-complete")
	_, err = tx.ExecContext(ctx, `
		INSERT INTO evaluation_run_events (
			id, run_id, event_type, payload, actor_type, actor_ref,
			transition_version, from_status, to_status, control_epoch, idempotency_key
		) VALUES ($1,$2,'test_complete','{}','system','revision-test',$3,'running','completed',$4,$5)`,
		uuid.New(), runID, version+2, epoch, secondKey)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE evaluation_runs SET status='completed', state_version=$2, finished_at=NOW(), updated_at=NOW() WHERE id=$1`, runID, version+2)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestRunAllowsOnlyOneActiveRevisionBatch(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, batch, _ := prepareRevisionBatchFixture(t)
	_, err := repo.CreateRevisionBatch(ctx, service.CreateRevisionBatchInput{
		RunID: batch.RunID, Reason: "second", RequestedBy: fixture.userID, IdempotencyKey: hashString(uuid.NewString()),
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchConflict)
}

func TestRevisionBatchGradingLeaseUsesFrozenInputAndBatchEpoch(t *testing.T) {
	fixture, gradingRepo, _, batch, _ := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(context.Background(), fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, "regrade", lease.WorkOrigin)
	require.Equal(t, batch.ID, lease.RevisionBatchID)
	require.Equal(t, batch.ControlEpoch, lease.LeaseEpoch)
	require.Len(t, lease.GradingInputHash, 64)
}

func TestRevisionBatchClaimSkipsBlockedBatchWithoutStarvingRunningBatch(t *testing.T) {
	ctx := context.Background()
	blockedFixture, _, _, blocked, _ := prepareRevisionBatchFixture(t)
	tx, err := beginRadarWriterTx(ctx, integrationDB, "api")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE evaluation_revision_batches SET status='blocked', updated_at=NOW() WHERE id=$1`, blocked.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = integrationDB.ExecContext(ctx, `UPDATE evaluation_workers SET token_hash=$2 WHERE id=$1`,
		blockedFixture.workerIDs[0], hashString(uuid.NewString()))
	require.NoError(t, err)

	fixture, gradingRepo, _, running, _ := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	require.Equal(t, running.ID, lease.RevisionBatchID)
}

func TestRevisionBatchRegradeSubmissionAdvancesHeadAndCompletesRequirement(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, _, batch, previous := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)

	score, err := gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.55"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, 2, score.HeadVersion)

	var reason string
	var eventBatchID uuid.UUID
	var previousRef service.ScoreRef
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT reason, revision_batch_id, previous_score_id, previous_score_created_at
		FROM evaluation_score_head_events
		WHERE score_id=$1 AND score_created_at=$2`, score.Ref.ID, score.Ref.CreatedAt).Scan(
		&reason, &eventBatchID, &previousRef.ID, &previousRef.CreatedAt))
	require.Equal(t, "regrade", reason)
	require.Equal(t, batch.ID, eventBatchID)
	require.Equal(t, previous, previousRef)
	var outboxOrigin string
	var outboxBatchID uuid.UUID
	var outboxEpoch int64
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT work_origin, revision_batch_id, lease_epoch
		FROM evaluation_outbox_events
		WHERE source_type='score_head_event'
		  AND source_id=(
			SELECT id::text FROM evaluation_score_head_events
			WHERE score_id=$1 AND score_created_at=$2
		  )`, score.Ref.ID, score.Ref.CreatedAt).Scan(&outboxOrigin, &outboxBatchID, &outboxEpoch))
	require.Equal(t, "regrade", outboxOrigin)
	require.Equal(t, batch.ID, outboxBatchID)
	require.Equal(t, batch.ControlEpoch, outboxEpoch)

	var requirementStatus string
	var completedAt sql.NullTime
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, completed_at
		FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='grading'
		  AND source_assignment_id=$2 AND grader_id=$3
		  AND recovery_generation=$4`, batch.ID, lease.AssignmentID, lease.GraderID, lease.RecoveryGeneration).Scan(
		&requirementStatus, &completedAt))
	require.Equal(t, "completed", requirementStatus)
	require.True(t, completedAt.Valid)

	var runStatus, assignmentStatus, sampleStatus string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_runs WHERE id=$1`, lease.RunID).Scan(&runStatus))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_assignments WHERE id=$1`, lease.AssignmentID).Scan(&assignmentStatus))
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_samples WHERE id=$1`, lease.SampleID).Scan(&sampleStatus))
	require.Equal(t, "completed", runStatus)
	require.Equal(t, "completed", assignmentStatus)
	require.Equal(t, "completed", sampleStatus)
}

func TestRevisionBatchFenceIncrementsEpochAndRequeuesSafeWork(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, repo, batch, _ := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	key := hashString(uuid.NewString())

	fenced, err := repo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "operator fence", ActorID: fixture.userID, IdempotencyKey: key,
	})
	require.NoError(t, err)
	require.Equal(t, batch.ControlEpoch+1, fenced.ControlEpoch)
	require.Equal(t, service.RevisionBatchRunning, fenced.Status)

	var jobStatus, assignmentStatus string
	var jobEpoch int64
	var tokenCleared, ownerCleared, expiryCleared bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, lease_epoch, lease_token_hash IS NULL, leased_by IS NULL, lease_expires_at IS NULL
		FROM evaluation_grading_jobs WHERE id=$1`, lease.ID).Scan(
		&jobStatus, &jobEpoch, &tokenCleared, &ownerCleared, &expiryCleared))
	require.Equal(t, "pending", jobStatus)
	require.Equal(t, fenced.ControlEpoch, jobEpoch)
	require.True(t, tokenCleared)
	require.True(t, ownerCleared)
	require.True(t, expiryCleared)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_assignments WHERE id=$1`, lease.AssignmentID).Scan(&assignmentStatus))
	require.Equal(t, "completed", assignmentStatus)

	_, err = gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.50"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.ErrorIs(t, err, service.ErrLeaseFenced)

	retried, err := repo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "operator fence", ActorID: fixture.userID, IdempotencyKey: key,
	})
	require.NoError(t, err)
	require.Equal(t, fenced.ControlEpoch, retried.ControlEpoch)
}

func TestRevisionBatchControlIdempotencyRejectsDifferentActorOrPayload(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, batch, _ := prepareRevisionBatchFixture(t)
	key := hashString(uuid.NewString())

	_, err := repo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "operator fence", ActorID: fixture.userID, IdempotencyKey: key,
	})
	require.NoError(t, err)

	secondActor := mustCreateUser(t, integrationEntClient, &service.User{
		Email: "revision-control-actor-" + uuid.NewString() + "@example.com",
	})
	_, err = repo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "operator fence", ActorID: secondActor.ID, IdempotencyKey: key,
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchConflict)

	_, err = repo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "different reason", ActorID: fixture.userID, IdempotencyKey: key,
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchConflict)
}

func TestRevisionBatchControlIdempotencyRejectsDifferentBatch(t *testing.T) {
	ctx := context.Background()
	firstFixture, _, firstRepo, firstBatch, _ := prepareRevisionBatchFixture(t)
	_, err := integrationDB.ExecContext(ctx, `UPDATE evaluation_workers SET token_hash=$2 WHERE id=$1`,
		firstFixture.workerIDs[0], hashString(uuid.NewString()))
	require.NoError(t, err)
	tx, err := beginRadarWriterTx(ctx, integrationDB, "api")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status='failed', failure_code='test_isolation', finished_at=NOW(), updated_at=NOW()
		WHERE revision_batch_id=$1 AND status='pending'`, firstBatch.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, _, secondRepo, secondBatch, _ := prepareRevisionBatchFixture(t)
	key := hashString(uuid.NewString())

	_, err = firstRepo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: firstBatch.ID, Reason: "operator fence", ActorID: firstFixture.userID, IdempotencyKey: key,
	})
	require.NoError(t, err)

	_, err = secondRepo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: secondBatch.ID, Reason: "operator fence", ActorID: firstFixture.userID, IdempotencyKey: key,
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchConflict)
}

func TestBlockedBatchFenceRejectsWithoutFailedGradingRequirement(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, batch, _ := prepareRevisionBatchFixture(t)
	tx, err := beginRadarWriterTx(ctx, integrationDB, "api")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE evaluation_revision_batches SET status='blocked', updated_at=NOW() WHERE id=$1`, batch.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	_, err = repo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "recover blocked batch", ActorID: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchNotRepairable)

	var status service.RevisionBatchStatus
	var fenceEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_revision_batches WHERE id=$1`, batch.ID).Scan(&status))
	require.Equal(t, service.RevisionBatchBlocked, status)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_revision_batch_events
		WHERE revision_batch_id=$1 AND event_type='fenced'`, batch.ID).Scan(&fenceEvents))
	require.Zero(t, fenceEvents)
}

func TestBlockedBatchRepairCreatesNextRecoveryGeneration(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, batch, _ := prepareRevisionBatchFixture(t)
	failedRequirementID := blockRevisionBatchGrading(t, batch.ID)

	repaired, err := repo.RepairRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "retry failed grader", ActorID: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.NoError(t, err)
	require.Equal(t, service.RevisionBatchRunning, repaired.Status)
	require.Equal(t, batch.ControlEpoch+1, repaired.ControlEpoch)

	var replacementID, replacesID uuid.UUID
	var replacementStatus, replacementInputHash string
	var generation int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id, replaces_requirement_id, status, recovery_generation, grading_input_hash
		FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND recovery_generation=1`, batch.ID).Scan(
		&replacementID, &replacesID, &replacementStatus, &generation, &replacementInputHash))
	require.NotEqual(t, failedRequirementID, replacementID)
	require.Equal(t, failedRequirementID, replacesID)
	require.Equal(t, "pending", replacementStatus)
	require.Equal(t, 1, generation)
	var failedStatus, failedInputHash string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status, grading_input_hash FROM evaluation_revision_batch_requirements WHERE id=$1`, failedRequirementID).Scan(&failedStatus, &failedInputHash))
	require.Equal(t, "superseded", failedStatus)
	require.Equal(t, failedInputHash, replacementInputHash)

	var jobStatus, workOrigin string
	var jobGeneration int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, work_origin, recovery_generation
		FROM evaluation_grading_jobs
		WHERE revision_batch_id=$1 AND recovery_generation=1`, batch.ID).Scan(
		&jobStatus, &workOrigin, &jobGeneration))
	require.Equal(t, "pending", jobStatus)
	require.Equal(t, "regrade", workOrigin)
	require.Equal(t, 1, jobGeneration)
}

func TestBlockedBatchFenceRepairsFailedGradingAndReturnsToRunning(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, batch, _ := prepareRevisionBatchFixture(t)
	failedRequirementID := blockRevisionBatchGrading(t, batch.ID)

	fenced, err := repo.FenceRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "recover blocked batch", ActorID: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.NoError(t, err)
	require.Equal(t, service.RevisionBatchRunning, fenced.Status)
	require.Equal(t, batch.ControlEpoch+1, fenced.ControlEpoch)
	var oldStatus, replacementStatus string
	var replacesID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status FROM evaluation_revision_batch_requirements WHERE id=$1`, failedRequirementID).Scan(&oldStatus))
	require.Equal(t, "superseded", oldStatus)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, replaces_requirement_id
		FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='grading' AND recovery_generation=1`, batch.ID).Scan(
		&replacementStatus, &replacesID))
	require.Equal(t, "pending", replacementStatus)
	require.Equal(t, failedRequirementID, replacesID)
}

func TestBatchCancelBeforeHeadAdvanceFencesPendingWork(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, repo, batch, _ := prepareRevisionBatchFixture(t)
	cancelled, err := repo.CancelRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "operator cancel", ActorID: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.NoError(t, err)
	require.Equal(t, service.RevisionBatchCancelled, cancelled.Status)
	require.Equal(t, batch.ControlEpoch+1, cancelled.ControlEpoch)

	var jobStatus string
	var failureCode sql.NullString
	var tokenCleared, ownerCleared, expiryCleared bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, failure_code, lease_token_hash IS NULL, leased_by IS NULL, lease_expires_at IS NULL
		FROM evaluation_grading_jobs WHERE revision_batch_id=$1`, batch.ID).Scan(
		&jobStatus, &failureCode, &tokenCleared, &ownerCleared, &expiryCleared))
	require.Equal(t, "failed", jobStatus)
	require.Equal(t, "revision_batch_cancelled", failureCode.String)
	require.True(t, tokenCleared)
	require.True(t, ownerCleared)
	require.True(t, expiryCleared)
	var requirementStatus string
	var requirementFailure sql.NullString
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, failure_code
		FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='grading'`, batch.ID).Scan(
		&requirementStatus, &requirementFailure))
	require.Equal(t, "failed", requirementStatus)
	require.Equal(t, "revision_batch_cancelled", requirementFailure.String)
	var persistedStatus service.RevisionBatchStatus
	var persistedEpoch int64
	var finishedAt sql.NullTime
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT status, control_epoch, finished_at
		FROM evaluation_revision_batches WHERE id=$1`, batch.ID).Scan(
		&persistedStatus, &persistedEpoch, &finishedAt))
	require.Equal(t, service.RevisionBatchCancelled, persistedStatus)
	require.Equal(t, cancelled.ControlEpoch, persistedEpoch)
	require.True(t, finishedAt.Valid)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.Nil(t, lease)
}

func TestBatchCancelRejectsAfterEligibleHeadAdvance(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, repo, batch, _ := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	_, err = gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.65"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)

	_, err = repo.CancelRevisionBatch(ctx, service.RevisionBatchControlInput{
		BatchID: batch.ID, Reason: "operator cancel", ActorID: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchPropagationRequired)
	var status service.RevisionBatchStatus
	var cancelEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `SELECT status FROM evaluation_revision_batches WHERE id=$1`, batch.ID).Scan(&status))
	require.Equal(t, service.RevisionBatchRunning, status)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_revision_batch_events
		WHERE revision_batch_id=$1 AND event_type='cancelled'`, batch.ID).Scan(&cancelEvents))
	require.Zero(t, cancelEvents)
}

func TestCompensatingHeadRequiresTwoDistinctApprovers(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, repo, batch, previous := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	regraded, err := gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.40"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	secondActor := mustCreateUser(t, integrationEntClient, &service.User{
		Email: "revision-approver-" + uuid.NewString() + "@example.com",
	})

	approve := func(actorID int64, key string) (*service.CompensatingScoreHeadResult, error) {
		return repo.ApproveCompensatingScoreHead(ctx, service.CompensatingScoreHeadInput{
			BatchID: batch.ID, SampleID: lease.SampleID, GraderID: lease.GraderID,
			ScoreRef: previous, ActorID: actorID, IdempotencyKey: key,
		})
	}
	first, err := approve(fixture.userID, hashString(uuid.NewString()))
	require.NoError(t, err)
	require.Equal(t, 1, first.ApprovalCount)
	require.False(t, first.Applied)
	sameActorRetry, err := approve(fixture.userID, hashString(uuid.NewString()))
	require.NoError(t, err)
	require.Equal(t, 1, sameActorRetry.ApprovalCount)
	require.False(t, sameActorRetry.Applied)

	second, err := approve(secondActor.ID, hashString(uuid.NewString()))
	require.NoError(t, err)
	require.Equal(t, 2, second.ApprovalCount)
	require.True(t, second.Applied)
	require.Equal(t, regraded.HeadVersion+1, second.HeadVersion)

	var current service.ScoreRef
	var headVersion int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT score_id, score_created_at, version
		FROM evaluation_score_heads WHERE sample_id=$1 AND grader_id=$2`, lease.SampleID, lease.GraderID).Scan(
		&current.ID, &current.CreatedAt, &headVersion))
	require.Equal(t, previous, current)
	require.Equal(t, second.HeadVersion, headVersion)
	var eventReason string
	var eventActor int64
	var eventBatchID uuid.UUID
	var outboxOrigin string
	var outboxBatchID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT head.reason, head.actor_id, head.revision_batch_id,
		       outbox.work_origin, outbox.revision_batch_id
		FROM evaluation_score_head_events head
		JOIN evaluation_outbox_events outbox
		  ON outbox.source_type='score_head_event' AND outbox.source_id=head.id::text
		WHERE head.score_id=$1 AND head.score_created_at=$2 AND head.reason='compensating'`, previous.ID, previous.CreatedAt).Scan(
		&eventReason, &eventActor, &eventBatchID, &outboxOrigin, &outboxBatchID))
	require.Equal(t, "compensating", eventReason)
	require.Equal(t, secondActor.ID, eventActor)
	require.Equal(t, batch.ID, eventBatchID)
	require.Equal(t, "regrade", outboxOrigin)
	require.Equal(t, batch.ID, outboxBatchID)
}

func TestCompensatingHeadApprovalIdempotencyRejectsDifferentActor(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, batch, previous := prepareRevisionBatchFixture(t)
	secondActor := mustCreateUser(t, integrationEntClient, &service.User{
		Email: "revision-idempotency-actor-" + uuid.NewString() + "@example.com",
	})
	key := hashString(uuid.NewString())
	input := service.CompensatingScoreHeadInput{
		BatchID: batch.ID, SampleID: revisionBatchSampleID(t, batch.ID), GraderID: "grader",
		ScoreRef: previous, ActorID: fixture.userID, IdempotencyKey: key,
	}

	first, err := repo.ApproveCompensatingScoreHead(ctx, input)
	require.NoError(t, err)
	require.Equal(t, 1, first.ApprovalCount)

	input.ActorID = secondActor.ID
	_, err = repo.ApproveCompensatingScoreHead(ctx, input)
	require.ErrorIs(t, err, service.ErrRevisionBatchConflict)
}

func TestCompensatingHeadAlreadyAtFrozenTargetRecordsAppliedEvent(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, batch, previous := prepareRevisionBatchFixture(t)
	sampleID := revisionBatchSampleID(t, batch.ID)
	secondActor := mustCreateUser(t, integrationEntClient, &service.User{
		Email: "revision-current-target-" + uuid.NewString() + "@example.com",
	})
	approve := func(actorID int64) (*service.CompensatingScoreHeadResult, error) {
		return repo.ApproveCompensatingScoreHead(ctx, service.CompensatingScoreHeadInput{
			BatchID: batch.ID, SampleID: sampleID, GraderID: "grader", ScoreRef: previous,
			ActorID: actorID, IdempotencyKey: hashString(uuid.NewString()),
		})
	}

	first, err := approve(fixture.userID)
	require.NoError(t, err)
	require.False(t, first.Applied)
	second, err := approve(secondActor.ID)
	require.NoError(t, err)
	require.True(t, second.Applied)
	require.Equal(t, 2, second.ApprovalCount)
	require.Equal(t, 1, second.HeadVersion)

	var compensatingHeadEvents, appliedEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_score_head_events
		WHERE revision_batch_id=$1 AND reason='compensating'`, batch.ID).Scan(&compensatingHeadEvents))
	require.Zero(t, compensatingHeadEvents)
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_revision_batch_events
		WHERE revision_batch_id=$1 AND event_type='compensating_head_applied'`, batch.ID).Scan(&appliedEvents))
	require.Equal(t, 1, appliedEvents)
}

func TestCompensatingHeadRejectsInvalidTargetBeforeApproval(t *testing.T) {
	ctx := context.Background()
	fixture, _, repo, batch, _ := prepareRevisionBatchFixture(t)
	invalid := service.ScoreRef{ID: uuid.New(), CreatedAt: time.Now().UTC()}

	_, err := repo.ApproveCompensatingScoreHead(ctx, service.CompensatingScoreHeadInput{
		BatchID: batch.ID, SampleID: uuid.New(), GraderID: "grader", ScoreRef: invalid,
		ActorID: fixture.userID, IdempotencyKey: hashString(uuid.NewString()),
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchInvalid)
	var approvalEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_revision_batch_events
		WHERE revision_batch_id=$1 AND event_type='compensating_head_approved'`, batch.ID).Scan(&approvalEvents))
	require.Zero(t, approvalEvents)
}

func TestCompensatingHeadRejectsScoreNotFrozenByBatch(t *testing.T) {
	ctx := context.Background()
	fixture, gradingRepo, repo, batch, _ := prepareRevisionBatchFixture(t)
	lease, err := gradingRepo.ClaimGradingLease(ctx, fixture.workerIDs[0], []string{"grader"}, time.Minute)
	require.NoError(t, err)
	require.NotNil(t, lease)
	regraded, err := gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.45"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)

	_, err = repo.ApproveCompensatingScoreHead(ctx, service.CompensatingScoreHeadInput{
		BatchID: batch.ID, SampleID: lease.SampleID, GraderID: lease.GraderID,
		ScoreRef: regraded.Ref, ActorID: fixture.userID, IdempotencyKey: hashString(uuid.NewString()),
	})
	require.ErrorIs(t, err, service.ErrRevisionBatchInvalid)
	var approvalEvents int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_revision_batch_events
		WHERE revision_batch_id=$1 AND event_type='compensating_head_approved'`, batch.ID).Scan(&approvalEvents))
	require.Zero(t, approvalEvents)
}

func blockRevisionBatchGrading(t *testing.T, batchID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var requirementID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT id FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND requirement_type='grading' AND recovery_generation=0`, batchID).Scan(&requirementID))
	tx, err := beginRadarWriterTx(ctx, integrationDB, "api")
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_revision_batch_requirements
		SET status='failed', failure_code='grader_timeout', updated_at=NOW()
		WHERE id=$1`, requirementID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status='failed', failure_code='grader_timeout', finished_at=NOW(), updated_at=NOW()
		WHERE revision_batch_id=$1 AND recovery_generation=0`, batchID)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_revision_batches SET status='blocked', updated_at=NOW() WHERE id=$1`, batchID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	return requirementID
}

func revisionBatchSampleID(t *testing.T, batchID uuid.UUID) uuid.UUID {
	t.Helper()
	var sampleID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(context.Background(), `
		SELECT assignment.sample_id
		FROM evaluation_revision_batch_requirements requirement
		JOIN evaluation_assignments assignment ON assignment.id=requirement.source_assignment_id
		WHERE requirement.revision_batch_id=$1 AND requirement.requirement_type='grading'
		ORDER BY requirement.recovery_generation
		LIMIT 1`, batchID).Scan(&sampleID))
	return sampleID
}

func prepareRevisionBatchFixture(t *testing.T) (evaluationRepositoryFixture, service.EvaluationGradingRepository, service.RevisionBatchRepository, *service.RevisionBatch, service.ScoreRef) {
	t.Helper()
	ctx := context.Background()
	fixture, gradingRepo, lease := prepareSealedGradingLease(t)
	score, err := gradingRepo.SubmitScore(ctx, lease.ID, lease.Token, service.ScoreSubmission{
		Score: decimal.RequireFromString("0.75"), LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	completeRevisionTestRun(t, lease.RunID)
	repo := NewRadarGovernanceRepository(integrationDB).(service.RevisionBatchRepository)
	batch, err := repo.CreateRevisionBatch(ctx, service.CreateRevisionBatchInput{
		RunID: lease.RunID, Reason: "model regression", RequestedBy: fixture.userID,
		IdempotencyKey: hashString(uuid.NewString()),
	})
	require.NoError(t, err)
	return fixture, gradingRepo, repo, batch, score.Ref
}
