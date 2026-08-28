//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseMemAvailableBytes(t *testing.T) {
	bytes, ok := parseMemAvailableBytes("MemFree: 10 kB\nMemAvailable: 262144 kB\n")
	require.True(t, ok)
	require.EqualValues(t, 262144*1024, bytes)

	_, ok = parseMemAvailableBytes("MemAvailable: nope kB")
	require.False(t, ok)
}

func TestParseOOMKillCount(t *testing.T) {
	count, ok := parseOOMKillCount("pgscan_kswapd 3\noom_kill 7\n")
	require.True(t, ok)
	require.EqualValues(t, 7, count)

	_, ok = parseOOMKillCount("oom_kill -1")
	require.False(t, ok)
}

func TestResourceWarningSummary(t *testing.T) {
	available := int64(200)
	swap := int64(4)
	disk := 81.0
	warning := resourceWarningSummary(&available, &swap, &disk, true)
	require.Equal(t, "warning:mem_available,warning:swap_used,warning:disk,critical:oom_kill", warning)
}

func TestComputeResourceHealthTreatsMissingMetricsAsNeutral(t *testing.T) {
	score, available := computeResourceHealth(&OpsSystemMetricsSnapshot{})
	require.False(t, available)
	require.Equal(t, 100.0, score)
}

func TestComputeResourceHealthUsesCriticalThresholds(t *testing.T) {
	available := int64(64)
	disk := 95.0
	score, availableMetrics := computeResourceHealth(&OpsSystemMetricsSnapshot{
		MemAvailableMB:  &available,
		DiskUsedPercent: &disk,
		ResourceWarning: "critical:mem_available",
	})
	require.True(t, availableMetrics)
	require.Equal(t, 0.0, score)
}
