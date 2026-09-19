package service

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var ErrReliabilitySnapshotInvalid = errors.New("invalid reliability snapshot submission")

// ReliabilitySnapshotSubmission is the worker-to-control-plane contract for a
// completed reliability window. Worker identity is assigned by the handler
// after token authentication and never trusted from the request body.
type ReliabilitySnapshotSubmission struct {
	WorkerID                   uuid.UUID       `json:"-"`
	WorkerImageDigest          string          `json:"worker_image_digest"`
	RunID                      uuid.UUID       `json:"run_id"`
	LoadPlanID                 uuid.UUID       `json:"load_plan_id"`
	ProfileID                  string          `json:"profile_id"`
	WindowStart                time.Time       `json:"window_start"`
	WindowEnd                  time.Time       `json:"window_end"`
	SourceWatermark            string          `json:"source_watermark"`
	SourceManifest             json.RawMessage `json:"source_manifest"`
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
	TTFTHistogram              json.RawMessage `json:"ttft_histogram"`
	LatencyHistogram           json.RawMessage `json:"latency_histogram"`
	P99LatencyMS               int64           `json:"p99_latency_ms"`
	ErrorRate                  decimal.Decimal `json:"error_rate"`
	CostAmount                 decimal.Decimal `json:"cost_amount"`
	FreshUntil                 time.Time       `json:"fresh_until"`
}

type ReliabilitySnapshotReceipt struct {
	SnapshotID   uuid.UUID `json:"snapshot_id"`
	SnapshotHash string    `json:"snapshot_hash"`
	HeadAdvanced bool      `json:"head_advanced"`
}

type ReliabilitySnapshotPublisher interface {
	PublishReliabilitySnapshot(context.Context, ReliabilitySnapshotSubmission) (*ReliabilitySnapshotReceipt, error)
}
