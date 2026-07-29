package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestNormalizeReliabilityMetricsSeparatesCancellationAndCountsUpstreamFailure(t *testing.T) {
	metrics, err := normalizeReliabilityMetrics(ReliabilityMetrics{
		RequestCount:               100,
		SuccessfulLatencyCount:     90,
		ValidPairCount:             40,
		UpstreamFailureCount:       5,
		GatewayFailureCount:        3,
		ClientCancellationCount:    2,
		P99LatencyMS:               1200,
		HistogramOrSketchHash:      strings.Repeat("a", 64),
		OngoingConfirmedP0Incident: false,
	})
	require.NoError(t, err)
	require.Equal(t, int64(8), metrics.ErrorNumerator)
	require.Equal(t, int64(98), metrics.ErrorDenominator)
	require.Equal(t, int64(2), metrics.ClientCancellationCount)

	_, err = normalizeReliabilityMetrics(ReliabilityMetrics{
		RequestCount: 100, SuccessfulLatencyCount: 90,
		UpstreamFailureCount: 10, GatewayFailureCount: 1,
	})
	require.Error(t, err)
}

func TestValidateReliabilitySnapshotInputRequiresUTCHalfOpenWindow(t *testing.T) {
	base := ReliabilitySnapshotInput{
		RunID:        uuid.New(),
		ProfileID:    "production-v1",
		SliceKey:     "region:global",
		WindowStart:  time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
		WindowEnd:    time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC),
		QueryVersion: "reliability-query-v1",
		SourceHash:   strings.Repeat("b", 64),
		FreshUntil:   time.Date(2026, 7, 28, 2, 0, 0, 0, time.UTC),
		Metrics: ReliabilityMetrics{
			RequestCount:           250,
			SuccessfulLatencyCount: 220,
			ValidPairCount:         40,
			HistogramOrSketchHash:  strings.Repeat("c", 64),
		},
	}
	require.NoError(t, validateReliabilitySnapshotInput(base))

	nonUTC := base
	nonUTC.WindowStart = nonUTC.WindowStart.In(time.FixedZone("UTC+8", 8*60*60))
	require.Error(t, validateReliabilitySnapshotInput(nonUTC))

	closed := base
	closed.WindowEnd = closed.WindowStart
	require.Error(t, validateReliabilitySnapshotInput(closed))

	staleAtPublish := base
	staleAtPublish.FreshUntil = staleAtPublish.WindowEnd
	require.Error(t, validateReliabilitySnapshotInput(staleAtPublish))
}

func TestEvaluateReliabilitySufficiency(t *testing.T) {
	now := time.Date(2026, 7, 28, 3, 0, 0, 0, time.UTC)
	latencyRequirement := ReliabilitySliceRequirement{ProfileID: "p1", SliceKey: "latency", Kind: ReliabilitySliceLatency}
	qualityRequirement := ReliabilitySliceRequirement{ProfileID: "p1", SliceKey: "quality", Kind: ReliabilitySliceQuality}
	base := []ReliabilitySnapshot{
		{
			ProfileID: "p1", SliceKey: "latency", FreshUntil: now.Add(time.Hour),
			Metrics: ReliabilityMetrics{SuccessfulLatencyCount: 200},
		},
		{
			ProfileID: "p1", SliceKey: "quality", FreshUntil: now.Add(time.Hour),
			Metrics: ReliabilityMetrics{ValidPairCount: 30},
		},
	}

	tests := []struct {
		name         string
		requirements []ReliabilitySliceRequirement
		snapshots    []ReliabilitySnapshot
		wantStatus   ReliabilitySufficiencyStatus
		wantReason   string
	}{
		{name: "sufficient", requirements: []ReliabilitySliceRequirement{latencyRequirement, qualityRequirement}, snapshots: base, wantStatus: ReliabilitySufficient},
		{name: "required slice set empty", requirements: nil, snapshots: base, wantStatus: ReliabilityInsufficientEvidence, wantReason: "required_slice_empty"},
		{name: "required slice missing", requirements: []ReliabilitySliceRequirement{latencyRequirement, qualityRequirement}, snapshots: base[:1], wantStatus: ReliabilityInsufficientEvidence, wantReason: "required_slice_missing"},
		{name: "latency sample too small", requirements: []ReliabilitySliceRequirement{latencyRequirement}, snapshots: []ReliabilitySnapshot{{ProfileID: "p1", SliceKey: "latency", FreshUntil: now.Add(time.Hour), Metrics: ReliabilityMetrics{SuccessfulLatencyCount: 199}}}, wantStatus: ReliabilityInsufficientEvidence, wantReason: "latency_sample_too_small"},
		{name: "quality sample too small", requirements: []ReliabilitySliceRequirement{qualityRequirement}, snapshots: []ReliabilitySnapshot{{ProfileID: "p1", SliceKey: "quality", FreshUntil: now.Add(time.Hour), Metrics: ReliabilityMetrics{ValidPairCount: 29}}}, wantStatus: ReliabilityInsufficientEvidence, wantReason: "quality_sample_too_small"},
		{name: "snapshot stale", requirements: []ReliabilitySliceRequirement{latencyRequirement}, snapshots: []ReliabilitySnapshot{{ProfileID: "p1", SliceKey: "latency", FreshUntil: now, Metrics: ReliabilityMetrics{SuccessfulLatencyCount: 200}}}, wantStatus: ReliabilityInsufficientEvidence, wantReason: "reliability_snapshot_stale"},
		{name: "confirmed P0 incident", requirements: []ReliabilitySliceRequirement{latencyRequirement}, snapshots: []ReliabilitySnapshot{{ProfileID: "p1", SliceKey: "latency", FreshUntil: now.Add(time.Hour), Metrics: ReliabilityMetrics{SuccessfulLatencyCount: 200, OngoingConfirmedP0Incident: true}}}, wantStatus: ReliabilityBlocked, wantReason: "confirmed_p0_incident"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EvaluateReliabilitySufficiency(test.requirements, test.snapshots, now)
			require.Equal(t, test.wantStatus, got.Status)
			require.Equal(t, test.wantReason, got.ReasonCode)
		})
	}
}
