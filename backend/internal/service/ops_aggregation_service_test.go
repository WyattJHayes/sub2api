package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpsAggregationStartUsesInitialWindowWhenTableIsEmpty(t *testing.T) {
	end := time.Date(2026, 8, 27, 12, 34, 56, 0, time.UTC)

	got := opsAggregationStart(end, time.Time{}, false, true, opsAggInitialBackfillWindow, opsAggHourlyOverlap, utcFloorToHour)

	require.Equal(t, end.Add(-opsAggInitialBackfillWindow).Truncate(time.Hour), got)
}

func TestOpsAggregationStartRecomputesLatestOverlap(t *testing.T) {
	end := time.Date(2026, 8, 27, 12, 34, 56, 0, time.UTC)
	latest := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	got := opsAggregationStart(end, latest, true, true, opsAggInitialBackfillWindow, opsAggHourlyOverlap, utcFloorToHour)

	require.Equal(t, latest.Add(-opsAggHourlyOverlap).Truncate(time.Hour), got)
}

func TestOpsAggregationStartKeepsShortWindowWhenLatestQueryFails(t *testing.T) {
	end := time.Date(2026, 8, 27, 12, 34, 56, 0, time.UTC)

	got := opsAggregationStart(end, time.Time{}, false, false, opsAggInitialBackfillWindow, opsAggHourlyOverlap, utcFloorToHour)

	require.Equal(t, end.Add(-opsAggBackfillWindow).Truncate(time.Hour), got)
}
