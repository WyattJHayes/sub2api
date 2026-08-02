package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRadarActorContextRoundTrip(t *testing.T) {
	ctx := WithRadarActor(WithRadarTenant(context.Background(), 41), 77)

	actorID, ok := RadarActorID(ctx)
	require.True(t, ok)
	require.Equal(t, int64(77), actorID)
	require.Equal(t, int64(41), mustRadarTenant(t, ctx))
}

func TestRadarActorContextIgnoresInvalidActor(t *testing.T) {
	ctx := WithRadarActor(context.Background(), 0)

	_, ok := RadarActorID(ctx)
	require.False(t, ok)
}

func mustRadarTenant(t *testing.T, ctx context.Context) int64 {
	t.Helper()
	tenantID, err := RequireRadarTenant(ctx)
	require.NoError(t, err)
	return tenantID
}
