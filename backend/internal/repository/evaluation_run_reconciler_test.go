package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestReconcileFailsBeforeConsideringPendingWork(t *testing.T) {
	transition := decideRunTransition(RunReconcileFacts{
		Status:               string(service.RunStatusRunning),
		UnrecoverableFailure: &FailureCause{Class: "infrastructure", Code: "retry_exhausted"},
		PendingWork:          3,
		CurrentCoverageOK:    true,
	})

	require.Equal(t, service.RunStatusFailed, transition.ToStatus)
	require.Equal(t, "unrecoverable_failure", transition.Reason)
}

func TestReconcileIgnoresSupersededAssignmentFailure(t *testing.T) {
	transition := decideRunTransition(RunReconcileFacts{
		Status:            string(service.RunStatusRunning),
		PendingWork:       0,
		CurrentCoverageOK: true,
	})

	require.Equal(t, service.RunStatusCompleted, transition.ToStatus)
}

func TestExactP0DrainRequiresSampleAssignmentAndScoreHead(t *testing.T) {
	for name, facts := range map[string]RunReconcileFacts{
		"sample incomplete":  {Status: string(service.RunStatusBudgetPaused), P0Expected: 1, P0Successful: 0, P0Active: 1},
		"assignment missing": {Status: string(service.RunStatusBudgetPaused), P0Expected: 1, P0Successful: 0, P0Active: 0},
		"score head missing": {Status: string(service.RunStatusBudgetPaused), P0Expected: 1, P0Successful: 1, P0Active: 0, P0ScoreHeadsReady: false},
	} {
		t.Run(name, func(t *testing.T) {
			transition := decideRunTransition(facts)
			require.NotEqual(t, service.RunStatusRunning, transition.ToStatus)
		})
	}

	ready := decideRunTransition(RunReconcileFacts{
		Status:            string(service.RunStatusBudgetPaused),
		P0Expected:        1,
		P0Successful:      1,
		P0Active:          0,
		P0ScoreHeadsReady: true,
	})
	require.Equal(t, service.RunStatusRunning, ready.ToStatus)
}

func TestPausedRunRecordsReadinessWithoutTransition(t *testing.T) {
	transition := decideRunTransition(RunReconcileFacts{
		Status:            string(service.RunStatusPaused),
		P0Expected:        1,
		P0Successful:      1,
		P0Active:          0,
		P0ScoreHeadsReady: true,
	})

	require.Equal(t, service.RunStatusPaused, transition.ToStatus)
	require.True(t, transition.ReadinessRecorded)
	require.False(t, transition.Changed)
}

func TestReconcileCompletesOnlyWithCurrentAggregateCoverage(t *testing.T) {
	incomplete := decideRunTransition(RunReconcileFacts{
		Status:            string(service.RunStatusRunning),
		CurrentCoverageOK: false,
	})
	require.Equal(t, service.RunStatusRunning, incomplete.ToStatus)
	require.Equal(t, "awaiting_current_aggregate", incomplete.Reason)

	complete := decideRunTransition(RunReconcileFacts{
		Status:            string(service.RunStatusRunning),
		CurrentCoverageOK: true,
	})
	require.Equal(t, service.RunStatusCompleted, complete.ToStatus)
}

func TestReconcileTerminalRetryDoesNotDuplicateEvent(t *testing.T) {
	transition := decideRunTransition(RunReconcileFacts{Status: string(service.RunStatusCompleted)})
	require.Equal(t, service.RunStatusCompleted, transition.ToStatus)
	require.False(t, transition.Changed)
	require.False(t, transition.AppendEvent)
}
