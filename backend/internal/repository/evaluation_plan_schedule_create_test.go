package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func testRadarPlanRepo(db *sql.DB) *radarGovernanceRepository {
	return &radarGovernanceRepository{db: db, routeProfileVersion: "main-gateway-v1"}
}

func TestCreatePlanRejectsCronWithoutExpression(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = testRadarPlanRepo(db).CreatePlan(context.Background(), service.CreateRadarPlanInput{
		Name:             "scheduled",
		DatasetVersionID: uuid.New(),
		GatewayAPIKeyID:  221,
		TriggerType:      "cron",
		CronExpression:   "",
		BaselineRef:      json.RawMessage(`{"release":"a"}`),
		CandidateRef:     json.RawMessage(`{"release":"b"}`),
		ModelMatrix:      json.RawMessage(`[{"route":"r","baseline":{"route":"a"},"candidate":{"route":"b"}}]`),
		MaxRunCost:       decimal.RequireFromString("2"),
		DailyCostLimit:   decimal.RequireFromString("2"),
		MaxConcurrency:   1,
		CreatedBy:        41,
	})
	require.Error(t, err)
}

func TestCreatePlanRejectsCronWithoutFrozenReferences(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = testRadarPlanRepo(db).CreatePlan(context.Background(), service.CreateRadarPlanInput{
		Name:             "scheduled",
		DatasetVersionID: uuid.New(),
		GatewayAPIKeyID:  221,
		TriggerType:      "cron",
		CronExpression:   "0 9 * * *",
		BaselineRef:      json.RawMessage(`{}`),
		CandidateRef:     json.RawMessage(`{"release":"b"}`),
		ModelMatrix:      json.RawMessage(`[{"route":"r","baseline":{"route":"a"},"candidate":{"route":"b"}}]`),
		MaxRunCost:       decimal.RequireFromString("2"),
		DailyCostLimit:   decimal.RequireFromString("2"),
		MaxConcurrency:   1,
		CreatedBy:        41,
	})
	require.Error(t, err, "an empty baseline reference must not be stored for a scheduled plan")
}

func TestCreatePlanRejectsUnknownTriggerType(t *testing.T) {
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	_, err = testRadarPlanRepo(db).CreatePlan(context.Background(), service.CreateRadarPlanInput{
		Name:             "release-driven",
		DatasetVersionID: uuid.New(),
		GatewayAPIKeyID:  221,
		TriggerType:      "release",
		ModelMatrix:      json.RawMessage(`[{"route":"r","baseline":{"route":"a"},"candidate":{"route":"b"}}]`),
		MaxRunCost:       decimal.RequireFromString("2"),
		DailyCostLimit:   decimal.RequireFromString("2"),
		MaxConcurrency:   1,
		CreatedBy:        41,
	})
	require.Error(t, err)
}

func TestCreatePlanKeepsManualPlanWithoutReferences(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	planID := uuid.New()
	datasetID := uuid.New()
	createdAt := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)INSERT INTO evaluation_plans.*RETURNING id, name`).
		WithArgs(
			sqlmock.AnyArg(), "manual-plan", datasetID, int64(221), "manual", "",
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			`[{"route":"r","baseline":{"route":"a"},"candidate":{"route":"b"}}]`,
			sqlmock.AnyArg(), sqlmock.AnyArg(), 1, int64(41),
		).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "dataset_version_id", "gateway_api_key_id", "trigger_type",
			"cron_expression", "baseline_ref", "candidate_ref", "next_run_at", "last_run_at",
			"model_matrix", "max_run_cost", "daily_cost_limit", "max_concurrency",
			"enabled", "created_by", "created_at",
		}).AddRow(planID, "manual-plan", datasetID, int64(221), "manual",
			"", nil, nil, nil, nil,
			[]byte(`[{"route":"r","baseline":{"route":"a"},"candidate":{"route":"b"}}]`),
			"2", "2", 1, true, int64(41), createdAt))

	record, err := testRadarPlanRepo(db).CreatePlan(context.Background(), service.CreateRadarPlanInput{
		Name:             "manual-plan",
		DatasetVersionID: datasetID,
		GatewayAPIKeyID:  221,
		TriggerType:      "manual",
		ModelMatrix:      json.RawMessage(`[{"route":"r","baseline":{"route":"a"},"candidate":{"route":"b"}}]`),
		MaxRunCost:       decimal.RequireFromString("2"),
		DailyCostLimit:   decimal.RequireFromString("2"),
		MaxConcurrency:   1,
		CreatedBy:        41,
	})
	require.NoError(t, err, "a manual plan must not require frozen comparison references")
	require.Equal(t, "manual", record.TriggerType)
	require.Empty(t, record.CronExpression)
	require.Nil(t, record.NextRunAt)
	require.NoError(t, mock.ExpectationsWereMet())
}
