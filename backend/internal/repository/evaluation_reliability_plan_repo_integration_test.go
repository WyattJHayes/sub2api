//go:build integration

package repository

import (
	"context"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestRadarReliabilityRepositoryPersistsAndPublishesImmutableLoadPlan(t *testing.T) {
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	ctx := service.WithRadarTenant(context.Background(), fixture.userID)
	repo := NewRadarReliabilityRepository(integrationDB)
	input := service.RadarLoadPlanInput{
		TenantID:             fixture.userID,
		Environment:          "staging",
		RouteProfileVersion:  "route-v42",
		ModelAliases:         []string{"deepseek-chat"},
		Regions:              []string{"cn-east"},
		TrafficMode:          "closed_loop",
		ConcurrencyLevels:    []int{1},
		InputTokenBuckets:    []int{128},
		OutputTokenBuckets:   []int{64},
		WarmupSeconds:        0,
		MeasurementSeconds:   60,
		MinimumValidRequests: 1,
		MaxRunCost:           decimal.RequireFromString("1"),
		MaxConcurrency:       1,
		ClientImageDigest:    "sha256:" + strings.Repeat("a", 64),
		GeneratorVersion:     "loadgen-v1",
	}

	draft, err := repo.CreateLoadPlan(ctx, input, fixture.userID)
	require.NoError(t, err)
	require.Equal(t, "draft", draft.Status)
	require.Len(t, draft.LoadPlanSHA256, 64)
	require.NotEmpty(t, draft.CanonicalPlan)

	published, err := repo.PublishLoadPlan(ctx, draft.ID, fixture.userID)
	require.NoError(t, err)
	require.Equal(t, "published", published.Status)
	require.NotNil(t, published.PublishedAt)

	retry, err := repo.PublishLoadPlan(ctx, draft.ID, fixture.userID)
	require.NoError(t, err)
	require.Equal(t, published.ID, retry.ID)
	require.Equal(t, published.LoadPlanSHA256, retry.LoadPlanSHA256)

	loaded, err := repo.GetLoadPlan(ctx, draft.ID)
	require.NoError(t, err)
	require.Equal(t, published.CanonicalPlan, loaded.CanonicalPlan)

	_, err = integrationDB.ExecContext(ctx, `
		UPDATE evaluation_load_plans
		SET canonical_plan_bytes='changed'::bytea
		WHERE id=$1`, draft.ID)
	require.Error(t, err)
}
