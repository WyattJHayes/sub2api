//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestCreateManualPlanWithoutReferencesStoresSQLNull(t *testing.T) {
	ctx := context.Background()
	seedID := seedScheduleFixture(t, "0 9 * * *", map[string]any{"release": "a"}, map[string]any{"release": "b"})
	var datasetID uuid.UUID
	var keyID, tenantID int64
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT dataset_version_id, gateway_api_key_id, created_by FROM evaluation_plans WHERE id=$1`,
		seedID).Scan(&datasetID, &keyID, &tenantID))
	input := service.CreateRadarPlanInput{
		Name: "manual-without-references", DatasetVersionID: datasetID, GatewayAPIKeyID: keyID,
		TriggerType: "manual", CreatedBy: tenantID,
		ModelMatrix: json.RawMessage(`[{"route":"r","baseline":{"route":"a"},"candidate":{"route":"b"}}]`),
		MaxRunCost:  decimal.RequireFromString("2"), DailyCostLimit: decimal.RequireFromString("2"), MaxConcurrency: 1,
	}
	plan, err := testRadarPlanRepo(integrationDB).CreatePlan(ctx, input)
	require.NoError(t, err, "manual plans must send SQL NULL, not an empty JSON byte slice")
	require.Nil(t, plan.BaselineRef)
	require.Nil(t, plan.CandidateRef)
	require.Nil(t, plan.NextRunAt)
	var noBaseline, noCandidate, noNextRun bool
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		`SELECT baseline_ref IS NULL, candidate_ref IS NULL, next_run_at IS NULL FROM evaluation_plans WHERE id=$1`,
		plan.ID).Scan(&noBaseline, &noCandidate, &noNextRun))
	require.True(t, noBaseline)
	require.True(t, noCandidate)
	require.True(t, noNextRun)

	input.Name = "cron-with-references"
	input.TriggerType = "cron"
	input.CronExpression = "0 9 * * *"
	input.BaselineRef = json.RawMessage(`{"release":"a"}`)
	input.CandidateRef = json.RawMessage(`{"release":"b"}`)
	cronPlan, err := testRadarPlanRepo(integrationDB).CreatePlan(ctx, input)
	require.NoError(t, err, "cron reference serialization must remain valid JSON")
	require.JSONEq(t, string(input.BaselineRef), string(cronPlan.BaselineRef))
	require.JSONEq(t, string(input.CandidateRef), string(cronPlan.CandidateRef))
	require.NotNil(t, cronPlan.NextRunAt)
}
