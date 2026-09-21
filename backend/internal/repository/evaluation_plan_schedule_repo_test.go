package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func expectEvaluationScheduleWriter(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	identity := defaultEvaluationWriterIdentity("schedule")
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO evaluation_writer_sessions").WithArgs(identity.InstanceID, "schedule", currentEvaluationWriterProtocolVersion).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_instance_id'").WithArgs(identity.InstanceID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_protocol'").WithArgs("2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_kind'").WithArgs("schedule").WillReturnResult(sqlmock.NewResult(0, 1))
}

func TestClaimDueScheduledPlansLeasesEachPlanOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	planID := uuid.New()
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	expectEvaluationScheduleWriter(t, mock)
	mock.ExpectQuery(`(?s)SELECT id, cron_expression, baseline_ref::text, candidate_ref::text, created_by, tenant_id.*FROM evaluation_plans.*trigger_type = 'cron'.*enabled.*next_run_at <= \$1.*FOR UPDATE SKIP LOCKED`).
		WithArgs(now, 16).
		WillReturnRows(sqlmock.NewRows([]string{"id", "cron_expression", "baseline_ref", "candidate_ref", "created_by", "tenant_id"}).
			AddRow(planID, "0 9 * * *", `{"release":"v0.2.7-sol"}`, `{"release":"v0.2.7-4models"}`, int64(41), int64(41)))
	mock.ExpectExec(`(?s)UPDATE evaluation_plans.*SET schedule_lease_token = \$2`).
		WithArgs(planID, sqlmock.AnyArg(), sqlmock.AnyArg(), now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claims, err := NewEvaluationPlanScheduleRepository(db).ClaimDueScheduledPlans(context.Background(), now, 16, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.Equal(t, planID, claims[0].PlanID)
	require.Equal(t, "0 9 * * *", claims[0].CronExpression)
	require.Equal(t, `{"release":"v0.2.7-sol"}`, string(claims[0].BaselineRef))
	require.Equal(t, int64(41), claims[0].CreatedBy)
	require.NotEmpty(t, claims[0].LeaseToken)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClaimDueScheduledPlansTreatsJSONNullReferenceAsAbsent(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	planID := uuid.New()
	now := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	expectEvaluationScheduleWriter(t, mock)
	mock.ExpectQuery(`(?s)SELECT id, cron_expression, baseline_ref::text, candidate_ref::text`).
		WithArgs(now, 16).
		WillReturnRows(sqlmock.NewRows([]string{"id", "cron_expression", "baseline_ref", "candidate_ref", "created_by", "tenant_id"}).
			AddRow(planID, "0 9 * * *", "null", `{"release":"b"}`, int64(41), int64(41)))
	mock.ExpectExec(`(?s)UPDATE evaluation_plans.*SET schedule_lease_token = \$2`).
		WithArgs(planID, sqlmock.AnyArg(), sqlmock.AnyArg(), now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claims, err := NewEvaluationPlanScheduleRepository(db).ClaimDueScheduledPlans(context.Background(), now, 16, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.Empty(t, claims[0].BaselineRef, "a JSON null reference must read as absent so the runner skips the plan")
	require.NotEmpty(t, claims[0].CandidateRef)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClaimDueScheduledPlansKeepsUnconfiguredReferencesEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	planID := uuid.New()
	now := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	expectEvaluationScheduleWriter(t, mock)
	mock.ExpectQuery(`(?s)SELECT id, cron_expression, baseline_ref::text, candidate_ref::text`).
		WithArgs(now, 16).
		WillReturnRows(sqlmock.NewRows([]string{"id", "cron_expression", "baseline_ref", "candidate_ref", "created_by", "tenant_id"}).
			AddRow(planID, "0 9 * * *", nil, nil, int64(41), int64(41)))
	mock.ExpectExec(`(?s)UPDATE evaluation_plans.*SET schedule_lease_token = \$2`).
		WithArgs(planID, sqlmock.AnyArg(), sqlmock.AnyArg(), now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claims, err := NewEvaluationPlanScheduleRepository(db).ClaimDueScheduledPlans(context.Background(), now, 16, 5*time.Minute)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.Empty(t, claims[0].BaselineRef)
	require.Empty(t, claims[0].CandidateRef)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCompleteScheduledPlanClaimFencesStaleLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	planID := uuid.New()
	ranAt := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	nextRunAt := ranAt.Add(24 * time.Hour)
	expectEvaluationScheduleWriter(t, mock)
	mock.ExpectExec(`(?s)UPDATE evaluation_plans.*schedule_lease_token = NULL.*WHERE id = \$1.*AND schedule_lease_token = \$2`).
		WithArgs(planID, "stale-token", nextRunAt, ranAt).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	err = NewEvaluationPlanScheduleRepository(db).CompleteScheduledPlanClaim(context.Background(), planID, "stale-token", ranAt, nextRunAt)
	require.Error(t, err)
	require.True(t, errors.Is(err, service.ErrEvaluationPlanScheduleFenced), "stale lease must surface a fence error, got %v", err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestCompleteScheduledPlanClaimAdvancesSchedule(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	planID := uuid.New()
	ranAt := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	nextRunAt := ranAt.Add(24 * time.Hour)
	expectEvaluationScheduleWriter(t, mock)
	mock.ExpectExec(`(?s)UPDATE evaluation_plans.*SET next_run_at = \$3`).
		WithArgs(planID, "token-1", nextRunAt, ranAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	require.NoError(t, NewEvaluationPlanScheduleRepository(db).CompleteScheduledPlanClaim(context.Background(), planID, "token-1", ranAt, nextRunAt))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestClaimDueScheduledPlansWithoutLimitIsNoop(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	claims, err := NewEvaluationPlanScheduleRepository(db).ClaimDueScheduledPlans(context.Background(), time.Now(), 0, time.Minute)
	require.NoError(t, err)
	require.Empty(t, claims)
}

func TestClaimDueScheduledPlansRejectsNilDatabase(t *testing.T) {
	var repo evaluationPlanScheduleRepo
	_, err := repo.ClaimDueScheduledPlans(context.Background(), time.Now(), 4, time.Minute)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil evaluation plan schedule repository")
	_ = sql.ErrNoRows
}
