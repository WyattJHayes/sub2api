package repository

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func validReliabilitySubmission() service.ReliabilitySnapshotSubmission {
	start := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	input := service.ReliabilitySnapshotSubmission{
		WorkerID: uuid.New(), WorkerImageDigest: "sha256:" + strings.Repeat("a", 64),
		RunID: uuid.New(), LoadPlanID: uuid.New(), WindowStart: start, WindowEnd: start.Add(time.Hour),
		ProfileID:    "profile-v1",
		QueryVersion: "reliability-query-v1", SliceKey: "region:test", RequestCount: 10,
		SuccessCount: 8, ErrorCount: 1, TimeoutCount: 1,
		ErrorRate: decimal.RequireFromString("0.2"), CostAmount: decimal.RequireFromString("1.25"),
		FreshUntil: start.Add(2 * time.Hour),
	}
	sealReliabilitySubmission(&input)
	return input
}

func sealReliabilitySubmission(input *service.ReliabilitySnapshotSubmission) {
	histogram := reliabilityHistogramEvidence{
		BucketBoundsMS: []int64{100}, Counts: []int64{input.SuccessCount, 0},
		SampleCount: input.SuccessCount, SumMS: input.SuccessCount * 100, MaxMS: 100,
	}
	input.TTFTHistogram = mustJSON(histogram)
	input.LatencyHistogram = mustJSON(histogram)
	input.TTFTHistogramHash = mustCanonicalDigest(input.TTFTHistogram)
	input.LatencyHistogramHash = mustCanonicalDigest(input.LatencyHistogram)
	input.P99LatencyMS = histogram.percentile(0.99)
	input.SourceManifest = mustJSON(reliabilitySourceManifest{
		Version: "radar-reliability-source-v1", WorkerImageDigest: input.WorkerImageDigest,
		RunID: input.RunID, LoadPlanID: input.LoadPlanID, ProfileID: input.ProfileID, WindowStart: input.WindowStart, WindowEnd: input.WindowEnd,
		QueryVersion: input.QueryVersion, SliceKey: input.SliceKey, RequestCount: input.RequestCount,
		SuccessCount: input.SuccessCount, ErrorCount: input.ErrorCount, TimeoutCount: input.TimeoutCount,
		RetryCount: input.RetryCount, ProtocolErrorCount: input.ProtocolErrorCount,
		BillingIdempotencyFailures: input.BillingIdempotencyFailures,
		TTFTHistogramHash:          input.TTFTHistogramHash, LatencyHistogramHash: input.LatencyHistogramHash,
		P99LatencyMS: input.P99LatencyMS, ErrorRate: input.ErrorRate, CostAmount: input.CostAmount,
		FreshUntil: input.FreshUntil,
	})
	input.SourceWatermark = mustCanonicalDigest(input.SourceManifest)
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func mustCanonicalDigest(raw []byte) string {
	digest, err := service.DigestCanonicalJSON(raw)
	if err != nil {
		panic(err)
	}
	return digest
}

func TestValidateReliabilitySubmissionRequiresCompleteDenominator(t *testing.T) {
	input := validReliabilitySubmission()
	require.NoError(t, validateReliabilitySubmission(input))

	input.RequestCount++
	require.ErrorContains(t, validateReliabilitySubmission(input), "denominator is incomplete")
}

func TestValidateReliabilitySubmissionRecomputesErrorRate(t *testing.T) {
	input := validReliabilitySubmission()
	input.ErrorRate = decimal.RequireFromString("0.1")
	require.ErrorContains(t, validateReliabilitySubmission(input), "error rate")
}

func TestValidateReliabilitySubmissionBoundsBillingIdempotencyFailures(t *testing.T) {
	input := validReliabilitySubmission()
	input.BillingIdempotencyFailures = input.RequestCount + 1
	require.ErrorContains(t, validateReliabilitySubmission(input), "billing idempotency")
}
