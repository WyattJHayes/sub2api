//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestReliabilityPublisherAdvancesHeadAndOutboxAtomically(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	gradingRepo := NewEvaluationGradingRepository(integrationDB)
	run, err := gradingRepo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	repo := NewEvaluationReliabilityRepository(integrationDB)
	windowStart := time.Now().UTC().Truncate(time.Hour)
	input := ReliabilitySnapshotInput{
		RunID: run.ID, ProfileID: "production-v1", SliceKey: "region:global",
		WindowStart: windowStart, WindowEnd: windowStart.Add(time.Hour),
		QueryVersion: "query-v1", SourceHash: strings.Repeat("a", 64),
		FreshUntil: windowStart.Add(2 * time.Hour),
		Metrics: ReliabilityMetrics{
			RequestCount: 250, SuccessfulLatencyCount: 220, ValidPairCount: 35,
			UpstreamFailureCount: 10, GatewayFailureCount: 5, ClientCancellationCount: 5,
			P99LatencyMS: 900, HistogramOrSketchHash: strings.Repeat("b", 64),
		},
	}

	first, err := repo.Publish(ctx, input)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, first.ID)
	require.Equal(t, int64(15), first.Metrics.ErrorNumerator)
	require.Equal(t, int64(245), first.Metrics.ErrorDenominator)

	var snapshotID, eventID uuid.UUID
	var snapshotHash string
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT snapshot_id, head_event_id, snapshot_hash
		FROM evaluation_reliability_heads
		WHERE run_id=$1 AND reliability_profile_id=$2 AND slice_key=$3`,
		run.ID, input.ProfileID, input.SliceKey).Scan(&snapshotID, &eventID, &snapshotHash))
	require.Equal(t, first.ID, snapshotID)
	require.Equal(t, first.SnapshotHash, snapshotHash)
	var outboxCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_outbox_events
		WHERE run_id=$1 AND source_type='reliability_head_event' AND source_id=$2`,
		run.ID, eventID.String()).Scan(&outboxCount))
	require.Equal(t, 1, outboxCount)

	retry, err := repo.Publish(ctx, input)
	require.NoError(t, err)
	require.Equal(t, first.ID, retry.ID)
	conflictInput := input
	conflictInput.Metrics.P99LatencyMS++
	_, err = repo.Publish(ctx, conflictInput)
	require.ErrorContains(t, err, "source identity conflicts")
	var eventCount int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_reliability_head_events
		WHERE run_id=$1 AND reliability_profile_id=$2 AND slice_key=$3`,
		run.ID, input.ProfileID, input.SliceKey).Scan(&eventCount))
	require.Equal(t, 1, eventCount)

	secondInput := input
	secondInput.WindowStart = input.WindowEnd
	secondInput.WindowEnd = secondInput.WindowStart.Add(time.Hour)
	secondInput.FreshUntil = secondInput.WindowEnd.Add(time.Hour)
	secondInput.SourceHash = strings.Repeat("c", 64)
	second, err := repo.Publish(ctx, secondInput)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	var previousSnapshotID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT previous_snapshot_id
		FROM evaluation_reliability_head_events
		WHERE snapshot_id=$1`, second.ID).Scan(&previousSnapshotID))
	require.Equal(t, first.ID, previousSnapshotID)

	tx := testTx(t)
	requireSQLRejectedWithinSavepoint(t, tx, "reliability_snapshot_update", `
		UPDATE evaluation_reliability_snapshots SET source_hash=$2 WHERE id=$1`, first.ID, strings.Repeat("d", 64))
	requireSQLRejectedWithinSavepoint(t, tx, "reliability_snapshot_delete", `
		DELETE FROM evaluation_reliability_snapshots WHERE id=$1`, first.ID)
	requireSQLRejectedWithinSavepoint(t, tx, "reliability_head_event_update", `
		UPDATE evaluation_reliability_head_events SET source_hash=$2 WHERE id=$1`, eventID, strings.Repeat("e", 64))
}

func TestReliabilityHeadOutboxIncludesHeadIdentityAcrossAdvances(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	gradingRepo := NewEvaluationGradingRepository(integrationDB)
	run, err := gradingRepo.(*evaluationGradingRepository).createRunForGradingTest(ctx, fixture.planID)
	require.NoError(t, err)
	repo := NewEvaluationReliabilityRepository(integrationDB)
	windowStart := time.Now().UTC().Truncate(time.Hour)
	input := ReliabilitySnapshotInput{
		RunID: run.ID, ProfileID: "repeat-v1", SliceKey: "region:global",
		WindowStart: windowStart, WindowEnd: windowStart.Add(time.Hour),
		QueryVersion: "query-v1", SourceHash: strings.Repeat("a", 64), FreshUntil: windowStart.Add(2 * time.Hour),
		Metrics: ReliabilityMetrics{RequestCount: 10, SuccessfulLatencyCount: 10, ValidPairCount: 1, HistogramOrSketchHash: strings.Repeat("b", 64)},
	}
	first, err := repo.Publish(ctx, input)
	require.NoError(t, err)
	intermediate := input
	intermediate.SourceHash = strings.Repeat("c", 64)
	second, err := repo.Publish(ctx, intermediate)
	require.NoError(t, err)
	for _, snapshot := range []*ReliabilitySnapshot{first, second} {
		var headEventID uuid.UUID
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
			SELECT id FROM evaluation_reliability_head_events WHERE snapshot_id=$1`, snapshot.ID).Scan(&headEventID))
		var outboxHash string
		require.NoError(t, integrationDB.QueryRowContext(ctx, `
			SELECT source_hash FROM evaluation_outbox_events
			WHERE source_type='reliability_head_event' AND source_id=$1`, headEventID.String()).Scan(&outboxHash))
		require.Equal(t, hashString("reliability-head-event\x00"+headEventID.String()+"\x00"+snapshot.SnapshotHash), outboxHash)
	}
}

func TestReliabilityPublisherRollsBackWhenRunDoesNotExist(t *testing.T) {
	ctx := context.Background()
	repo := NewEvaluationReliabilityRepository(integrationDB)
	windowStart := time.Now().UTC().Truncate(time.Hour)
	sourceHash := strings.Repeat("f", 64)
	_, err := repo.Publish(ctx, ReliabilitySnapshotInput{
		RunID: uuid.New(), ProfileID: "production-v1", SliceKey: "region:global",
		WindowStart: windowStart, WindowEnd: windowStart.Add(time.Hour),
		QueryVersion: "query-v1", SourceHash: sourceHash,
		FreshUntil: windowStart.Add(2 * time.Hour),
		Metrics: ReliabilityMetrics{
			RequestCount: 250, SuccessfulLatencyCount: 220, ValidPairCount: 35,
			HistogramOrSketchHash: strings.Repeat("1", 64),
		},
	})
	require.Error(t, err)
	var count int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM evaluation_reliability_snapshots WHERE source_hash=$1`, sourceHash).Scan(&count))
	require.Equal(t, 0, count)
}
