package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// PublishReliabilitySnapshot is exposed through the worker repository so the
// token authentication and snapshot transaction share the same database.
func (r *evaluationGradingRepository) PublishReliabilitySnapshot(ctx context.Context, submission service.ReliabilitySnapshotSubmission) (*service.ReliabilitySnapshotReceipt, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation grading repository")
	}
	if err := validateReliabilitySubmission(submission); err != nil {
		return nil, fmt.Errorf("%w: %v", service.ErrReliabilitySnapshotInvalid, err)
	}
	metrics := ReliabilityMetrics{
		RequestCount:               submission.RequestCount,
		SuccessCount:               submission.SuccessCount,
		SuccessfulLatencyCount:     submission.SuccessCount,
		ErrorCount:                 submission.ErrorCount,
		UpstreamFailureCount:       submission.ErrorCount,
		TimeoutCount:               submission.TimeoutCount,
		RetryCount:                 submission.RetryCount,
		ProtocolErrorCount:         submission.ProtocolErrorCount,
		BillingIdempotencyFailures: submission.BillingIdempotencyFailures,
		P99LatencyMS:               submission.P99LatencyMS,
		HistogramOrSketchHash:      submission.LatencyHistogramHash,
		TTFTHistogramHash:          submission.TTFTHistogramHash,
		LatencyHistogramHash:       submission.LatencyHistogramHash,
		TTFTHistogram:              append([]byte(nil), submission.TTFTHistogram...),
		LatencyHistogram:           append([]byte(nil), submission.LatencyHistogram...),
		SourceManifest:             append([]byte(nil), submission.SourceManifest...),
		ErrorRate:                  submission.ErrorRate.String(),
		CostAmount:                 submission.CostAmount.String(),
	}
	input := ReliabilitySnapshotInput{
		RunID: submission.RunID, LoadPlanID: submission.LoadPlanID, WorkerID: submission.WorkerID,
		WorkerImageDigest: submission.WorkerImageDigest, ProfileID: strings.TrimSpace(submission.ProfileID),
		SliceKey: submission.SliceKey, WindowStart: submission.WindowStart, WindowEnd: submission.WindowEnd,
		QueryVersion: submission.QueryVersion, SourceHash: submission.SourceWatermark, Metrics: metrics,
		FreshUntil: submission.FreshUntil,
	}
	if err := validateReliabilitySnapshotInput(input); err != nil {
		return nil, fmt.Errorf("%w: %v", service.ErrReliabilitySnapshotInvalid, err)
	}
	snapshot, err := NewEvaluationReliabilityRepository(r.db).Publish(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("publish reliability snapshot: %w", err)
	}
	return &service.ReliabilitySnapshotReceipt{SnapshotID: snapshot.ID, SnapshotHash: snapshot.SnapshotHash, HeadAdvanced: snapshot.HeadAdvanced}, nil
}

func validateReliabilitySubmission(submission service.ReliabilitySnapshotSubmission) error {
	if submission.WorkerID == uuid.Nil {
		return errors.New("statistics worker identity is required")
	}
	if submission.RunID == uuid.Nil || submission.LoadPlanID == uuid.Nil {
		return errors.New("reliability run and load plan are required")
	}
	if strings.TrimSpace(submission.ProfileID) == "" || strings.TrimSpace(submission.ProfileID) == submission.LoadPlanID.String() {
		return errors.New("reliability profile id must be explicit and independent from the load plan id")
	}
	if strings.TrimSpace(submission.WorkerImageDigest) == "" {
		return errors.New("statistics worker image digest is required")
	}
	if submission.RequestCount != submission.SuccessCount+submission.ErrorCount+submission.TimeoutCount {
		return errors.New("reliability request denominator is incomplete")
	}
	if submission.RequestCount < 1 {
		return errors.New("reliability request denominator is empty")
	}
	if submission.BillingIdempotencyFailures < 0 || submission.BillingIdempotencyFailures > submission.RequestCount {
		return errors.New("billing idempotency count exceeds request denominator")
	}
	if submission.ErrorCount < 0 || submission.TimeoutCount < 0 || submission.SuccessCount < 0 {
		return errors.New("reliability outcome counts cannot be negative")
	}
	expectedErrorRate := decimal.NewFromInt(submission.ErrorCount + submission.TimeoutCount).Div(decimal.NewFromInt(submission.RequestCount))
	if !submission.ErrorRate.Equal(expectedErrorRate) {
		return errors.New("reliability error rate does not match the request denominator")
	}
	if submission.CostAmount.IsNegative() {
		return errors.New("reliability cost cannot be negative")
	}
	for label, evidence := range map[string]struct {
		raw  []byte
		hash string
	}{
		"source manifest":   {submission.SourceManifest, submission.SourceWatermark},
		"TTFT histogram":    {submission.TTFTHistogram, submission.TTFTHistogramHash},
		"latency histogram": {submission.LatencyHistogram, submission.LatencyHistogramHash},
	} {
		if len(evidence.raw) == 0 {
			return fmt.Errorf("%s canonical evidence is required", label)
		}
		computed, err := service.DigestCanonicalJSON(evidence.raw)
		if err != nil || computed != evidence.hash {
			return fmt.Errorf("%s hash does not match canonical evidence", label)
		}
	}
	if err := validateReliabilitySourceManifest(submission); err != nil {
		return err
	}
	ttftHistogram, err := decodeReliabilityHistogram(submission.TTFTHistogram)
	if err != nil {
		return fmt.Errorf("invalid TTFT histogram: %w", err)
	}
	latencyHistogram, err := decodeReliabilityHistogram(submission.LatencyHistogram)
	if err != nil {
		return fmt.Errorf("invalid latency histogram: %w", err)
	}
	if ttftHistogram.SampleCount > submission.SuccessCount || latencyHistogram.SampleCount != submission.SuccessCount {
		return errors.New("histogram sample counts do not match successful requests")
	}
	if latencyHistogram.percentile(0.99) != submission.P99LatencyMS {
		return errors.New("P99 latency does not match canonical histogram")
	}
	return nil
}

type reliabilitySourceManifest struct {
	Version                    string          `json:"version"`
	WorkerImageDigest          string          `json:"worker_image_digest"`
	RunID                      uuid.UUID       `json:"run_id"`
	LoadPlanID                 uuid.UUID       `json:"load_plan_id"`
	ProfileID                  string          `json:"profile_id"`
	WindowStart                time.Time       `json:"window_start"`
	WindowEnd                  time.Time       `json:"window_end"`
	QueryVersion               string          `json:"query_version"`
	SliceKey                   string          `json:"slice_key"`
	RequestCount               int64           `json:"request_count"`
	SuccessCount               int64           `json:"success_count"`
	ErrorCount                 int64           `json:"error_count"`
	TimeoutCount               int64           `json:"timeout_count"`
	RetryCount                 int64           `json:"retry_count"`
	ProtocolErrorCount         int64           `json:"protocol_error_count"`
	BillingIdempotencyFailures int64           `json:"billing_idempotency_failures"`
	TTFTHistogramHash          string          `json:"ttft_histogram_hash"`
	LatencyHistogramHash       string          `json:"latency_histogram_hash"`
	P99LatencyMS               int64           `json:"p99_latency_ms"`
	ErrorRate                  decimal.Decimal `json:"error_rate"`
	CostAmount                 decimal.Decimal `json:"cost_amount"`
	FreshUntil                 time.Time       `json:"fresh_until"`
}

func validateReliabilitySourceManifest(submission service.ReliabilitySnapshotSubmission) error {
	var manifest reliabilitySourceManifest
	if err := json.Unmarshal(submission.SourceManifest, &manifest); err != nil {
		return errors.New("source manifest is not valid JSON")
	}
	if manifest.Version != "radar-reliability-source-v1" ||
		manifest.WorkerImageDigest != submission.WorkerImageDigest ||
		manifest.RunID != submission.RunID || manifest.LoadPlanID != submission.LoadPlanID ||
		manifest.ProfileID != submission.ProfileID ||
		!manifest.WindowStart.Equal(submission.WindowStart) || !manifest.WindowEnd.Equal(submission.WindowEnd) ||
		manifest.QueryVersion != submission.QueryVersion || manifest.SliceKey != submission.SliceKey ||
		manifest.RequestCount != submission.RequestCount || manifest.SuccessCount != submission.SuccessCount ||
		manifest.ErrorCount != submission.ErrorCount || manifest.TimeoutCount != submission.TimeoutCount ||
		manifest.RetryCount != submission.RetryCount || manifest.ProtocolErrorCount != submission.ProtocolErrorCount ||
		manifest.BillingIdempotencyFailures != submission.BillingIdempotencyFailures ||
		manifest.TTFTHistogramHash != submission.TTFTHistogramHash || manifest.LatencyHistogramHash != submission.LatencyHistogramHash ||
		manifest.P99LatencyMS != submission.P99LatencyMS || !manifest.ErrorRate.Equal(submission.ErrorRate) ||
		!manifest.CostAmount.Equal(submission.CostAmount) || !manifest.FreshUntil.Equal(submission.FreshUntil) {
		return errors.New("source manifest identity does not match snapshot submission")
	}
	return nil
}

type reliabilityHistogramEvidence struct {
	BucketBoundsMS []int64 `json:"bucket_bounds_ms"`
	Counts         []int64 `json:"counts"`
	SampleCount    int64   `json:"sample_count"`
	SumMS          int64   `json:"sum_ms"`
	MaxMS          int64   `json:"max_ms"`
}

func decodeReliabilityHistogram(raw []byte) (reliabilityHistogramEvidence, error) {
	var histogram reliabilityHistogramEvidence
	if err := json.Unmarshal(raw, &histogram); err != nil {
		return reliabilityHistogramEvidence{}, err
	}
	if len(histogram.BucketBoundsMS) == 0 || len(histogram.Counts) != len(histogram.BucketBoundsMS)+1 ||
		histogram.SampleCount < 0 || histogram.SumMS < 0 || histogram.MaxMS < 0 {
		return reliabilityHistogramEvidence{}, errors.New("histogram shape is invalid")
	}
	var samples int64
	for index, bound := range histogram.BucketBoundsMS {
		if bound <= 0 || (index > 0 && bound <= histogram.BucketBoundsMS[index-1]) {
			return reliabilityHistogramEvidence{}, errors.New("histogram bounds are invalid")
		}
	}
	for _, count := range histogram.Counts {
		if count < 0 {
			return reliabilityHistogramEvidence{}, errors.New("histogram counts are invalid")
		}
		samples += count
	}
	if samples != histogram.SampleCount || (histogram.SampleCount == 0 && (histogram.SumMS != 0 || histogram.MaxMS != 0)) ||
		(histogram.SampleCount > 0 && histogram.MaxMS > histogram.SumMS) {
		return reliabilityHistogramEvidence{}, errors.New("histogram aggregates are inconsistent")
	}
	return histogram, nil
}

func (h reliabilityHistogramEvidence) percentile(quantile float64) int64 {
	if h.SampleCount == 0 {
		return 0
	}
	target := int64(float64(h.SampleCount)*quantile + 0.999999)
	if target < 1 {
		target = 1
	}
	var seen int64
	for index, count := range h.Counts {
		seen += count
		if seen >= target {
			if index < len(h.BucketBoundsMS) {
				return h.BucketBoundsMS[index]
			}
			return h.BucketBoundsMS[len(h.BucketBoundsMS)-1]
		}
	}
	return h.BucketBoundsMS[len(h.BucketBoundsMS)-1]
}
