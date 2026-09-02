package service

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func capturePricingSourceLog(t *testing.T, now time.Time, snapshot PricingSourceSnapshot) map[string]any {
	t.Helper()
	var output bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logPricingSourceSnapshot(log, now, snapshot)

	var record map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &record))
	return record
}

func TestPricingSourceObservabilityLogsFallbackAsWarn(t *testing.T) {
	now := time.Date(2026, 8, 2, 17, 0, 0, 0, time.UTC)
	record := capturePricingSourceLog(t, now, PricingSourceSnapshot{
		Source:           "local",
		ModelCount:       12,
		LocalHash:        "abc123",
		LastUpdated:      now.Add(-2 * time.Minute),
		LastRefreshAt:    now.Add(-30 * time.Second),
		LastRefreshOK:    false,
		LastRefreshError: "remote hash unavailable",
		FallbackTotal:    3,
	})

	require.Equal(t, "WARN", record["level"])
	require.Equal(t, "radar_pricing_source_health", record["msg"])
	require.Equal(t, "service.pricing", record["component"])
	require.Equal(t, "local", record["pricing_source"])
	require.EqualValues(t, 120, record["source_age_seconds"])
	require.EqualValues(t, 3, record["fallback_total"])
	require.Equal(t, false, record["last_refresh_ok"])
	require.Equal(t, "remote hash unavailable", record["last_refresh_error"])
}

func TestPricingSourceObservabilityLogsEmptySourceAsError(t *testing.T) {
	now := time.Date(2026, 8, 2, 17, 0, 0, 0, time.UTC)
	record := capturePricingSourceLog(t, now, PricingSourceSnapshot{
		Source:        "",
		ModelCount:    0,
		LastRefreshOK: false,
		FallbackTotal: 4,
	})

	require.Equal(t, "ERROR", record["level"])
	require.Equal(t, "empty", record["pricing_source"])
	require.EqualValues(t, 0, record["source_age_seconds"])
	require.EqualValues(t, 4, record["fallback_total"])
}

func TestPricingSourceMetricsPublishesBoundedSeries(t *testing.T) {
	now := time.Date(2026, 8, 2, 17, 0, 0, 0, time.UTC)
	metrics := PricingSourceMetrics(now, PricingSourceSnapshot{
		Source:        "local",
		ModelCount:    9,
		LastUpdated:   now.Add(-90 * time.Second),
		LastRefreshOK: false,
		FallbackTotal: 7,
	})

	require.Equal(t, []PricingSourceMetric{
		{Name: "radar_pricing_source_current", Labels: map[string]string{"source": "local"}, Value: 1},
		{Name: "radar_pricing_source_age_seconds", Labels: map[string]string{}, Value: 90},
		{Name: "radar_pricing_last_refresh_ok", Labels: map[string]string{}, Value: 0},
		{Name: "radar_pricing_fallback_total", Labels: map[string]string{}, Value: 7},
	}, metrics)
}
