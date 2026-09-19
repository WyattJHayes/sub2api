package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAssignmentRecordedBudgetStopsP0AndP1AndPreservesInflightLeases(t *testing.T) {
	for _, test := range []struct {
		name, priority, runCost, dailyCost string
	}{
		{"run at limit P0", "P0", "2", "2"},
		{"run over limit P1", "P1", "2.1", "2.1"},
		{"plan daily at limit", "P0", "0.25", "4"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			runID, planID := uuid.New(), uuid.New()
			mock.ExpectBegin()
			tx, err := db.Begin()
			require.NoError(t, err)
			t.Cleanup(func() { _ = tx.Rollback() })
			expectBudgetEligibilityLock(mock, runID, planID)
			mock.ExpectQuery(`(?s)SELECT.*FROM evaluation_route_evidence`).WithArgs(runID, planID).
				WillReturnRows(sqlmock.NewRows([]string{"run_cost", "daily_cost", "daily_limit"}).AddRow(test.runCost, test.dailyCost, "4"))
			mock.ExpectQuery(`SELECT control_epoch, state_version`).WithArgs(runID).
				WillReturnRows(sqlmock.NewRows([]string{"control_epoch", "state_version"}).AddRow(4, 7))
			mock.ExpectExec(`UPDATE evaluation_runs SET status='paused'`).
				WithArgs(runID, service.RunStatusRunning).
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO evaluation_run_events`).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			ok, err := lockRunLeaseEligibility(context.Background(), tx, runID, test.priority)
			require.NoError(t, err)
			require.False(t, ok)
			require.NoError(t, tx.Commit())
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

func expectBudgetEligibilityLock(mock sqlmock.Sqlmock, runID, planID uuid.UUID) {
	mock.ExpectQuery(`(?s)SELECT r.status, r.reserved_cost.*FOR UPDATE OF r, p`).WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "reserved_cost", "budget_limit", "plan_id", "enabled", "max_concurrency", "key_usable"}).
			AddRow("running", "0.0048", "2", planID, true, 2, true))
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*FROM evaluation_assignments`).WithArgs(planID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
}

func TestAssignmentRecordedBudgetBelowLimitAllowsLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runID, planID := uuid.New(), uuid.New()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	expectBudgetEligibilityLock(mock, runID, planID)
	mock.ExpectQuery(`(?s)SELECT.*FROM evaluation_route_evidence`).WithArgs(runID, planID).
		WillReturnRows(sqlmock.NewRows([]string{"run_cost", "daily_cost", "daily_limit"}).AddRow("0.25401", "0.25401", "4"))
	mock.ExpectRollback()
	ok, err := lockRunLeaseEligibility(context.Background(), tx, runID, "P1")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAssignmentRecordedBudgetQueryFailureDeniesLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	runID, planID := uuid.New(), uuid.New()
	mock.ExpectBegin()
	tx, err := db.Begin()
	require.NoError(t, err)
	expectBudgetEligibilityLock(mock, runID, planID)
	wantErr := errors.New("billing query unavailable")
	mock.ExpectQuery(`(?s)SELECT.*FROM evaluation_route_evidence`).WithArgs(runID, planID).WillReturnError(wantErr)
	mock.ExpectRollback()
	ok, err := lockRunLeaseEligibility(context.Background(), tx, runID, "P0")
	require.ErrorIs(t, err, wantErr)
	require.False(t, ok)
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}
