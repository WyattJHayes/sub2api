package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestPauseRunPreservesInflightLeaseAndEpoch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	runID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT id, run_id, event_type, payload").WithArgs(strings.Repeat("a", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT status, paused_from_status, pause_reason, control_epoch, state_version").WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "paused_from_status", "pause_reason", "control_epoch", "state_version"}).AddRow("running", nil, nil, int64(4), int64(7)))
	mock.ExpectExec("UPDATE evaluation_runs SET status='paused'").WithArgs(runID, "running", "operator", int64(8)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO evaluation_run_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &radarGovernanceRepository{db: db}
	result, err := repo.PauseRun(context.Background(), runID, "operator", 9, strings.Repeat("a", 64))
	require.NoError(t, err)
	require.Equal(t, int64(4), result.PreviousEpoch)
	require.Equal(t, int64(4), result.CurrentEpoch)
	require.Equal(t, "running", result.FromStatus)
	require.Equal(t, "paused", result.ToStatus)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFenceRunPreservesStatusAndIncrementsEpoch(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	runID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT id, run_id, event_type, payload").WithArgs(strings.Repeat("b", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT status, paused_from_status, pause_reason, control_epoch, state_version").WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "paused_from_status", "pause_reason", "control_epoch", "state_version"}).AddRow("running", nil, nil, int64(4), int64(7)))
	mock.ExpectQuery("SELECT a.id, a.sample_id").WithArgs(runID).WillReturnRows(sqlmock.NewRows([]string{"id", "sample_id", "case_id", "model_route", "sample_index", "attempt"}))
	mock.ExpectExec("UPDATE evaluation_runs SET control_epoch").WithArgs(runID, int64(5), int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO evaluation_run_events").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &radarGovernanceRepository{db: db}
	result, err := repo.FenceRun(context.Background(), runID, "incident", 9, strings.Repeat("b", 64))
	require.NoError(t, err)
	require.Equal(t, service.RunStatusRunning, service.RunStatus(result.ToStatus))
	require.Equal(t, int64(5), result.CurrentEpoch)
	require.Equal(t, int64(0), int64(result.AffectedWorkCount))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRunControlIdempotencyReturnsOriginalResult(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	runID, eventID := uuid.New(), uuid.New()
	original := service.RunControlResult{RunID: runID, FromStatus: "running", ToStatus: "paused", PreviousEpoch: 4, CurrentEpoch: 4, EventID: eventID}
	payload, err := json.Marshal(map[string]any{"reason": "operator", "result": original})
	require.NoError(t, err)
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT id, run_id, event_type, payload").WithArgs(strings.Repeat("c", 64)).WillReturnRows(
		sqlmock.NewRows([]string{"id", "run_id", "event_type", "payload"}).AddRow(eventID, runID, "run_control_pause", payload))
	mock.ExpectCommit()

	repo := &radarGovernanceRepository{db: db}
	result, err := repo.PauseRun(context.Background(), runID, "operator", 9, strings.Repeat("c", 64))
	require.NoError(t, err)
	require.Equal(t, eventID, result.EventID)
	require.Equal(t, "paused", result.ToStatus)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestRunControlRejectsInvalidReasonBeforeOpeningTransaction(t *testing.T) {
	repo := &radarGovernanceRepository{db: nil}
	_, err := repo.PauseRun(context.Background(), uuid.New(), "unknown", 9, strings.Repeat("d", 64))
	require.Error(t, err)
}

func TestRunControlRejectsTerminalStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	runID := uuid.New()
	expectRadarWorkerWriter(t, mock)
	mock.ExpectQuery("SELECT id, run_id, event_type, payload").WithArgs(strings.Repeat("e", 64)).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT status, paused_from_status, pause_reason, control_epoch, state_version").WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "paused_from_status", "pause_reason", "control_epoch", "state_version"}).AddRow("completed", nil, nil, int64(4), int64(7)))
	repo := &radarGovernanceRepository{db: db}
	_, err = repo.CancelRun(context.Background(), runID, "operator", 9, strings.Repeat("e", 64))
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
