package service

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestCanonicalLoadPlanNormalizesSetsAndProducesStableHash(t *testing.T) {
	input := RadarLoadPlanInput{
		TenantID:             7,
		Environment:          "staging",
		RouteProfileVersion:  "route-v42",
		ModelAliases:         []string{"qwen-plus", "deepseek-chat", "deepseek-chat"},
		Regions:              []string{"cn-east", "cn-east"},
		TrafficMode:          "closed_loop",
		ConcurrencyLevels:    []int{50, 1, 10},
		InputTokenBuckets:    []int{2048, 128},
		OutputTokenBuckets:   []int{512, 64},
		WarmupSeconds:        120,
		MeasurementSeconds:   600,
		MinimumValidRequests: 100,
		MaxRunCost:           decimal.RequireFromString("10"),
		MaxConcurrency:       50,
		ClientImageDigest:    "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GeneratorVersion:     "loadgen-v1",
	}

	got, err := CanonicalLoadPlan(input)
	require.NoError(t, err)
	require.Equal(t, []string{"deepseek-chat", "qwen-plus"}, got.ModelAliases)
	require.Equal(t, []string{"cn-east"}, got.Regions)
	require.Equal(t, []int{1, 10, 50}, got.ConcurrencyLevels)
	require.Equal(t, []int{128, 2048}, got.InputTokenBuckets)
	require.Equal(t, []int{64, 512}, got.OutputTokenBuckets)
	require.NotEmpty(t, got.CanonicalBytes)
	require.Len(t, got.SHA256, 64)

	permuted := input
	permuted.ModelAliases = []string{"deepseek-chat", "qwen-plus"}
	permuted.Regions = []string{"cn-east"}
	permuted.ConcurrencyLevels = []int{10, 50, 1}
	permuted.InputTokenBuckets = []int{128, 2048}
	permuted.OutputTokenBuckets = []int{64, 512}
	permutedPlan, err := CanonicalLoadPlan(permuted)
	require.NoError(t, err)
	require.Equal(t, got.SHA256, permutedPlan.SHA256)
}

func TestCanonicalLoadPlanRejectsUnsafeBounds(t *testing.T) {
	base := RadarLoadPlanInput{
		TenantID:             7,
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
		ClientImageDigest:    "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		GeneratorVersion:     "loadgen-v1",
	}

	for name, mutate := range map[string]func(*RadarLoadPlanInput){
		"empty model":             func(input *RadarLoadPlanInput) { input.ModelAliases = nil },
		"zero concurrency":        func(input *RadarLoadPlanInput) { input.ConcurrencyLevels = []int{0} },
		"concurrency exceeds cap": func(input *RadarLoadPlanInput) { input.ConcurrencyLevels = []int{2} },
		"zero budget":             func(input *RadarLoadPlanInput) { input.MaxRunCost = decimal.Zero },
		"invalid image digest":    func(input *RadarLoadPlanInput) { input.ClientImageDigest = "sha256:INVALID" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			_, err := CanonicalLoadPlan(input)
			require.Error(t, err)
		})
	}
}

func TestQualityAdminCanManageLoadPlansAndViewerCanOnlyRead(t *testing.T) {
	authorizer := NewStaticRadarAuthorizer(map[int64][]RadarRole{
		7: {RoleQualityAdmin},
		8: {RoleViewer},
	})
	require.NoError(t, authorizer.Require(t.Context(), 7, PermissionLoadPlanManage))
	require.ErrorIs(t, authorizer.Require(t.Context(), 8, PermissionLoadPlanManage), ErrRadarForbidden)
	require.NoError(t, authorizer.Require(t.Context(), 8, PermissionView))
}
