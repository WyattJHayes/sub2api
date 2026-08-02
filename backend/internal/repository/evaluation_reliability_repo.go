package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const (
	minimumReliabilityLatencySamples = 200
	minimumReliabilityQualityPairs   = 30
)

type ReliabilityMetrics struct {
	RequestCount               int64           `json:"request_count"`
	SuccessCount               int64           `json:"success_count,omitempty"`
	ErrorCount                 int64           `json:"error_count,omitempty"`
	TimeoutCount               int64           `json:"timeout_count,omitempty"`
	RetryCount                 int64           `json:"retry_count,omitempty"`
	ProtocolErrorCount         int64           `json:"protocol_error_count,omitempty"`
	BillingIdempotencyFailures int64           `json:"billing_idempotency_failures,omitempty"`
	SuccessfulLatencyCount     int64           `json:"successful_latency_count"`
	ValidPairCount             int64           `json:"valid_pair_count"`
	UpstreamFailureCount       int64           `json:"upstream_failure_count"`
	GatewayFailureCount        int64           `json:"gateway_failure_count"`
	ClientCancellationCount    int64           `json:"client_cancellation_count"`
	ErrorNumerator             int64           `json:"error_numerator"`
	ErrorDenominator           int64           `json:"error_denominator"`
	P99LatencyMS               int64           `json:"p99_latency_ms"`
	HistogramOrSketchHash      string          `json:"histogram_or_sketch_hash"`
	TTFTHistogramHash          string          `json:"ttft_histogram_hash,omitempty"`
	LatencyHistogramHash       string          `json:"latency_histogram_hash,omitempty"`
	TTFTHistogram              json.RawMessage `json:"ttft_histogram,omitempty"`
	LatencyHistogram           json.RawMessage `json:"latency_histogram,omitempty"`
	SourceManifest             json.RawMessage `json:"source_manifest,omitempty"`
	ErrorRate                  string          `json:"error_rate,omitempty"`
	CostAmount                 string          `json:"cost_amount,omitempty"`
	OngoingConfirmedP0Incident bool            `json:"ongoing_confirmed_p0_incident"`
}

type ReliabilitySnapshotInput struct {
	RunID             uuid.UUID
	LoadPlanID        uuid.UUID
	WorkerID          uuid.UUID
	WorkerImageDigest string
	ProfileID         string
	SliceKey          string
	WindowStart       time.Time
	WindowEnd         time.Time
	QueryVersion      string
	SourceHash        string
	Metrics           ReliabilityMetrics
	FreshUntil        time.Time
}

type ReliabilitySnapshot struct {
	ID           uuid.UUID
	RunID        uuid.UUID
	ProfileID    string
	SliceKey     string
	WindowStart  time.Time
	WindowEnd    time.Time
	QueryVersion string
	SourceHash   string
	Metrics      ReliabilityMetrics
	SnapshotHash string
	FreshUntil   time.Time
	CreatedAt    time.Time
	HeadAdvanced bool
}

type ReliabilitySliceKind string

const (
	ReliabilitySliceLatency ReliabilitySliceKind = "latency"
	ReliabilitySliceQuality ReliabilitySliceKind = "quality"
)

type ReliabilitySliceRequirement struct {
	ProfileID string
	SliceKey  string
	Kind      ReliabilitySliceKind
}

type ReliabilitySufficiencyStatus string

const (
	ReliabilitySufficient           ReliabilitySufficiencyStatus = "sufficient"
	ReliabilityInsufficientEvidence ReliabilitySufficiencyStatus = "insufficient_evidence"
	ReliabilityBlocked              ReliabilitySufficiencyStatus = "blocked"
)

type ReliabilitySufficiencyResult struct {
	Status     ReliabilitySufficiencyStatus
	ReasonCode string
}

type evaluationReliabilityRepository struct{ db *sql.DB }

func NewEvaluationReliabilityRepository(db *sql.DB) *evaluationReliabilityRepository {
	return &evaluationReliabilityRepository{db: db}
}

func normalizeReliabilityMetrics(metrics ReliabilityMetrics) (ReliabilityMetrics, error) {
	counts := []int64{
		metrics.RequestCount,
		metrics.SuccessfulLatencyCount,
		metrics.ValidPairCount,
		metrics.UpstreamFailureCount,
		metrics.GatewayFailureCount,
		metrics.ClientCancellationCount,
		metrics.P99LatencyMS,
	}
	for _, count := range counts {
		if count < 0 {
			return ReliabilityMetrics{}, errors.New("reliability metrics cannot be negative")
		}
	}
	if metrics.SuccessCount == 0 && metrics.SuccessfulLatencyCount > 0 {
		metrics.SuccessCount = metrics.SuccessfulLatencyCount
	}
	if metrics.SuccessfulLatencyCount == 0 && metrics.SuccessCount > 0 {
		metrics.SuccessfulLatencyCount = metrics.SuccessCount
	}
	if metrics.ErrorCount == 0 {
		metrics.ErrorCount = metrics.UpstreamFailureCount + metrics.GatewayFailureCount
	}
	if metrics.ErrorCount < metrics.UpstreamFailureCount+metrics.GatewayFailureCount {
		return ReliabilityMetrics{}, errors.New("reliability error count is smaller than classified failures")
	}
	for _, count := range []int64{metrics.SuccessCount, metrics.ErrorCount, metrics.TimeoutCount, metrics.RetryCount, metrics.ProtocolErrorCount, metrics.BillingIdempotencyFailures} {
		if count < 0 {
			return ReliabilityMetrics{}, errors.New("reliability metrics cannot be negative")
		}
	}
	metrics.ErrorDenominator = metrics.RequestCount - metrics.ClientCancellationCount
	metrics.ErrorNumerator = metrics.ErrorCount + metrics.TimeoutCount
	if metrics.ErrorDenominator < 0 || metrics.ErrorNumerator > metrics.ErrorDenominator {
		return ReliabilityMetrics{}, errors.New("reliability error counts exceed the eligible request denominator")
	}
	if metrics.SuccessfulLatencyCount > metrics.RequestCount {
		return ReliabilityMetrics{}, errors.New("successful latency count exceeds request count")
	}
	if metrics.SuccessCount+metrics.ErrorCount+metrics.TimeoutCount+metrics.ClientCancellationCount > metrics.RequestCount {
		return ReliabilityMetrics{}, errors.New("reliability terminal outcomes exceed request count")
	}
	if metrics.RetryCount > metrics.RequestCount || metrics.ProtocolErrorCount > metrics.RequestCount || metrics.BillingIdempotencyFailures > metrics.RequestCount {
		return ReliabilityMetrics{}, errors.New("reliability event counts exceed request count")
	}
	classifiedRequests := metrics.SuccessCount + metrics.ErrorCount + metrics.TimeoutCount + metrics.ClientCancellationCount
	if classifiedRequests > metrics.RequestCount {
		return ReliabilityMetrics{}, errors.New("classified reliability outcomes exceed request count")
	}
	return metrics, nil
}

func validateReliabilitySnapshotInput(input ReliabilitySnapshotInput) error {
	if input.RunID == uuid.Nil {
		return errors.New("reliability run is required")
	}
	if strings.TrimSpace(input.ProfileID) == "" || strings.TrimSpace(input.SliceKey) == "" || strings.TrimSpace(input.QueryVersion) == "" {
		return errors.New("reliability profile, slice, and query version are required")
	}
	for _, instant := range []time.Time{input.WindowStart, input.WindowEnd, input.FreshUntil} {
		_, offset := instant.Zone()
		if instant.IsZero() || offset != 0 {
			return errors.New("reliability windows must use UTC")
		}
	}
	if !input.WindowStart.Before(input.WindowEnd) {
		return errors.New("reliability window must be half-open with a positive duration")
	}
	if !input.FreshUntil.After(input.WindowEnd) {
		return errors.New("reliability freshness must extend beyond the window")
	}
	if !validLowerHexSHA256(input.SourceHash) {
		return errors.New("reliability source hash must be lowercase SHA256")
	}
	if !validLowerHexSHA256(input.Metrics.HistogramOrSketchHash) {
		return errors.New("reliability histogram or sketch hash must be lowercase SHA256")
	}
	for _, histogramHash := range []string{input.Metrics.TTFTHistogramHash, input.Metrics.LatencyHistogramHash} {
		if histogramHash != "" && !validLowerHexSHA256(histogramHash) {
			return errors.New("reliability histogram hash must be lowercase SHA256")
		}
	}
	_, err := normalizeReliabilityMetrics(input.Metrics)
	return err
}

func validLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func EvaluateReliabilitySufficiency(requirements []ReliabilitySliceRequirement, snapshots []ReliabilitySnapshot, now time.Time) ReliabilitySufficiencyResult {
	if len(requirements) == 0 {
		return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "required_slice_empty"}
	}
	byKey := make(map[string]ReliabilitySnapshot, len(snapshots))
	duplicates := make(map[string]bool)
	for _, snapshot := range snapshots {
		key := snapshot.ProfileID + "\x00" + snapshot.SliceKey
		if _, exists := byKey[key]; exists {
			duplicates[key] = true
		}
		byKey[key] = snapshot
	}
	for _, requirement := range requirements {
		key := requirement.ProfileID + "\x00" + requirement.SliceKey
		snapshot, exists := byKey[key]
		if !exists {
			return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "required_slice_missing"}
		}
		if duplicates[key] {
			return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "required_slice_ambiguous"}
		}
		if !snapshot.FreshUntil.After(now) {
			return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "reliability_snapshot_stale"}
		}
		if snapshot.Metrics.RequestCount > 0 {
			terminalCount := snapshot.Metrics.SuccessCount + snapshot.Metrics.ErrorCount + snapshot.Metrics.TimeoutCount + snapshot.Metrics.ClientCancellationCount
			if terminalCount != snapshot.Metrics.RequestCount {
				return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "reliability_denominator_incomplete"}
			}
			if snapshot.Metrics.BillingIdempotencyFailures > 0 {
				return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "billing_not_reconciled"}
			}
		}
		if snapshot.Metrics.OngoingConfirmedP0Incident {
			return ReliabilitySufficiencyResult{Status: ReliabilityBlocked, ReasonCode: "confirmed_p0_incident"}
		}
		switch requirement.Kind {
		case ReliabilitySliceLatency:
			if snapshot.Metrics.SuccessfulLatencyCount < minimumReliabilityLatencySamples {
				return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "latency_sample_too_small"}
			}
		case ReliabilitySliceQuality:
			if snapshot.Metrics.ValidPairCount < minimumReliabilityQualityPairs {
				return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "quality_sample_too_small"}
			}
		default:
			return ReliabilitySufficiencyResult{Status: ReliabilityInsufficientEvidence, ReasonCode: "required_slice_kind_invalid"}
		}
	}
	return ReliabilitySufficiencyResult{Status: ReliabilitySufficient}
}

func (r *evaluationReliabilityRepository) Publish(ctx context.Context, input ReliabilitySnapshotInput) (*ReliabilitySnapshot, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation reliability repository")
	}
	if err := validateReliabilitySnapshotInput(input); err != nil {
		return nil, err
	}
	if boundWorker, bound := service.RadarWorkerID(ctx); bound && boundWorker != input.WorkerID {
		return nil, service.ErrRadarForbidden
	}
	input.ProfileID = strings.TrimSpace(input.ProfileID)
	input.SliceKey = strings.TrimSpace(input.SliceKey)
	input.QueryVersion = strings.TrimSpace(input.QueryVersion)
	metrics, err := normalizeReliabilityMetrics(input.Metrics)
	if err != nil {
		return nil, err
	}
	input.Metrics = metrics
	metricsJSON, err := json.Marshal(metrics)
	if err != nil {
		return nil, fmt.Errorf("marshal reliability metrics: %w", err)
	}
	snapshotHash, err := reliabilitySnapshotHash(input, metricsJSON)
	if err != nil {
		return nil, err
	}

	tx, err := beginRadarWriterTx(ctx, r.db, "statistics")
	if err != nil {
		return nil, fmt.Errorf("begin reliability publish: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	tenantID := int64(0)
	var runTenantID int64
	if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_runs WHERE id=$1 FOR SHARE`, input.RunID).Scan(&runTenantID); err != nil {
		return nil, fmt.Errorf("load reliability run tenant: %w", err)
	}
	if scopedTenant, scoped := radarTenant(ctx); scoped {
		tenantID = scopedTenant
		if err := ensureRadarRunTenant(ctx, tx, input.RunID); err != nil {
			return nil, err
		}
	}
	if runTenantID > 0 {
		if tenantID > 0 && tenantID != runTenantID {
			return nil, service.ErrRadarForbidden
		}
		tenantID = runTenantID
	}
	workerTenantID := int64(0)
	if input.WorkerID != uuid.Nil {
		var workerKind, workerStatus, workerImageDigest string
		if err := tx.QueryRowContext(ctx, `
			SELECT worker_kind, status, COALESCE(image_digest, ''), tenant_id
			FROM evaluation_workers WHERE id=$1 FOR UPDATE`, input.WorkerID).
			Scan(&workerKind, &workerStatus, &workerImageDigest, &workerTenantID); err != nil {
			return nil, fmt.Errorf("load reliability worker identity: %w", err)
		}
		if workerKind != "statistics" || workerStatus != "active" || strings.TrimSpace(input.WorkerImageDigest) == "" || workerImageDigest != input.WorkerImageDigest {
			return nil, service.ErrWorkerIdentityMismatch
		}
		if _, bound := service.RadarWorkerID(ctx); bound && workerTenantID <= 0 {
			return nil, service.ErrRadarForbidden
		}
		if workerTenantID > 0 {
			if err := ensureRadarWorkerRunTenant(ctx, tx, input.WorkerID, input.RunID); err != nil {
				return nil, err
			}
			if tenantID > 0 && tenantID != workerTenantID {
				return nil, service.ErrRadarForbidden
			}
			tenantID = workerTenantID
		}
	}
	if input.LoadPlanID != uuid.Nil {
		var status string
		var canonicalPlan []byte
		var sameTenant bool
		var planTenant int64
		if err := tx.QueryRowContext(ctx, `
			SELECT lp.status, lp.canonical_plan_bytes, lp.tenant_id,
		       EXISTS (
			   SELECT 1 FROM evaluation_runs r
			   JOIN evaluation_plans p ON p.id=r.plan_id
			   WHERE r.id=$2 AND r.tenant_id=lp.tenant_id AND p.tenant_id=lp.tenant_id
		       )
				FROM evaluation_load_plans lp WHERE lp.id=$1 FOR SHARE`, input.LoadPlanID, input.RunID).
			Scan(&status, &canonicalPlan, &planTenant, &sameTenant); err != nil {
			return nil, fmt.Errorf("load reliability plan: %w", err)
		}
		if tenantID == 0 {
			tenantID = planTenant
		} else if planTenant != tenantID {
			return nil, service.ErrRadarForbidden
		}
		if workerTenantID > 0 && planTenant != workerTenantID {
			return nil, service.ErrRadarForbidden
		}
		if status != "published" {
			return nil, errors.New("reliability load plan is not published")
		}
		if !sameTenant {
			return nil, errors.New("reliability run and load plan tenant do not match")
		}
		var planIdentity struct {
			ClientImageDigest string `json:"client_image_digest"`
		}
		if err := json.Unmarshal(canonicalPlan, &planIdentity); err != nil {
			return nil, fmt.Errorf("decode reliability load plan identity: %w", err)
		}
		if input.WorkerID != uuid.Nil && planIdentity.ClientImageDigest != input.WorkerImageDigest {
			return nil, service.ErrWorkerIdentityMismatch
		}
	}

	requestedID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_reliability_snapshots (
			id, run_id, reliability_profile_id, slice_key, window_start, window_end,
			query_version, source_hash, metrics, snapshot_hash, fresh_until,
			load_plan_id, source_watermark, request_count, success_count, error_count,
			timeout_count, retry_count, protocol_error_count, billing_idempotency_failures,
			ttft_histogram_hash, latency_histogram_hash, p99_latency_ms, error_rate, cost_amount, tenant_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$8,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
		ON CONFLICT (run_id, reliability_profile_id, slice_key, window_start, window_end, source_hash)
		DO NOTHING`, requestedID, input.RunID, input.ProfileID, input.SliceKey,
		input.WindowStart, input.WindowEnd, input.QueryVersion, input.SourceHash,
		string(metricsJSON), snapshotHash, input.FreshUntil, nullableUUID(input.LoadPlanID),
		input.Metrics.RequestCount, input.Metrics.SuccessCount, input.Metrics.ErrorCount,
		input.Metrics.TimeoutCount, input.Metrics.RetryCount, input.Metrics.ProtocolErrorCount,
		input.Metrics.BillingIdempotencyFailures, reliabilityHistogramHash(input.Metrics.TTFTHistogramHash, input.Metrics.HistogramOrSketchHash),
		reliabilityHistogramHash(input.Metrics.LatencyHistogramHash, input.Metrics.HistogramOrSketchHash), input.Metrics.P99LatencyMS,
		normalizeErrorRate(input.Metrics), normalizeCost(input.Metrics), tenantID); err != nil {
		return nil, fmt.Errorf("insert reliability snapshot: %w", err)
	}

	snapshot := &ReliabilitySnapshot{}
	var storedMetrics []byte
	var storedTenantID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, reliability_profile_id, slice_key, window_start, window_end,
		       query_version, source_hash, metrics, snapshot_hash, fresh_until, created_at, tenant_id
		FROM evaluation_reliability_snapshots
		WHERE run_id=$1 AND reliability_profile_id=$2 AND slice_key=$3
		  AND window_start=$4 AND window_end=$5 AND source_hash=$6`,
		input.RunID, input.ProfileID, input.SliceKey, input.WindowStart, input.WindowEnd, input.SourceHash).Scan(
		&snapshot.ID, &snapshot.RunID, &snapshot.ProfileID, &snapshot.SliceKey,
		&snapshot.WindowStart, &snapshot.WindowEnd, &snapshot.QueryVersion,
		&snapshot.SourceHash, &storedMetrics, &snapshot.SnapshotHash,
		&snapshot.FreshUntil, &snapshot.CreatedAt, &storedTenantID); err != nil {
		return nil, fmt.Errorf("load reliability snapshot: %w", err)
	}
	if tenantID > 0 && storedTenantID != tenantID {
		return nil, service.ErrRadarForbidden
	}
	if snapshot.SnapshotHash != snapshotHash || snapshot.QueryVersion != input.QueryVersion {
		return nil, errors.New("reliability source identity conflicts with an existing snapshot")
	}
	if err := json.Unmarshal(storedMetrics, &snapshot.Metrics); err != nil {
		return nil, fmt.Errorf("decode stored reliability metrics: %w", err)
	}

	lockKey := "reliability:" + input.RunID.String() + ":" + input.ProfileID + ":" + input.SliceKey
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return nil, fmt.Errorf("lock reliability head: %w", err)
	}
	var previousSnapshotID uuid.NullUUID
	err = tx.QueryRowContext(ctx, `
		SELECT snapshot_id FROM evaluation_reliability_heads
		WHERE run_id=$1 AND reliability_profile_id=$2 AND slice_key=$3
		FOR UPDATE`, input.RunID, input.ProfileID, input.SliceKey).Scan(&previousSnapshotID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load reliability head: %w", err)
	}
	if previousSnapshotID.Valid && previousSnapshotID.UUID == snapshot.ID {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit idempotent reliability publish: %w", err)
		}
		snapshot.HeadAdvanced = false
		return snapshot, nil
	}

	headEventID := uuid.New()
	var previous any
	if previousSnapshotID.Valid {
		previous = previousSnapshotID.UUID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_reliability_head_events (
			id, run_id, reliability_profile_id, slice_key, previous_snapshot_id,
			snapshot_id, snapshot_hash, source_hash
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, headEventID, input.RunID,
		input.ProfileID, input.SliceKey, previous, snapshot.ID, snapshot.SnapshotHash,
		snapshot.SourceHash); err != nil {
		return nil, fmt.Errorf("insert reliability head event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_reliability_heads (
			run_id, reliability_profile_id, slice_key, snapshot_id,
			head_event_id, snapshot_hash, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,transaction_timestamp())
		ON CONFLICT (run_id, reliability_profile_id, slice_key) DO UPDATE
		SET snapshot_id=EXCLUDED.snapshot_id, head_event_id=EXCLUDED.head_event_id,
			snapshot_hash=EXCLUDED.snapshot_hash, updated_at=EXCLUDED.updated_at`,
		input.RunID, input.ProfileID, input.SliceKey, snapshot.ID, headEventID,
		snapshot.SnapshotHash); err != nil {
		return nil, fmt.Errorf("advance reliability head: %w", err)
	}

	payload, err := json.Marshal(struct {
		ProfileID    string    `json:"reliability_profile_id"`
		SliceKey     string    `json:"slice_key"`
		SnapshotID   uuid.UUID `json:"snapshot_id"`
		SnapshotHash string    `json:"snapshot_hash"`
	}{input.ProfileID, input.SliceKey, snapshot.ID, snapshot.SnapshotHash})
	if err != nil {
		return nil, fmt.Errorf("marshal reliability outbox payload: %w", err)
	}
	if _, err := enqueueEvaluationOutbox(ctx, tx, service.EnqueueEvaluationOutboxInput{
		EventType: "gate_reevaluation", RunID: input.RunID,
		ScopeKey: input.ProfileID + "/" + input.SliceKey, AnalysisVersion: input.QueryVersion,
		SourceType: "reliability_head_event", SourceID: headEventID.String(),
		SourceHash: hashString("reliability-head-event\x00" + headEventID.String() + "\x00" + snapshot.SnapshotHash),
		Payload:    payload,
	}); err != nil {
		return nil, fmt.Errorf("enqueue reliability reevaluation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reliability publish: %w", err)
	}
	snapshot.HeadAdvanced = true
	return snapshot, nil
}

func reliabilitySnapshotHash(input ReliabilitySnapshotInput, metricsJSON []byte) (string, error) {
	canonical := struct {
		RunID        string          `json:"run_id"`
		ProfileID    string          `json:"reliability_profile_id"`
		SliceKey     string          `json:"slice_key"`
		WindowStart  string          `json:"window_start"`
		WindowEnd    string          `json:"window_end"`
		QueryVersion string          `json:"query_version"`
		SourceHash   string          `json:"source_hash"`
		Metrics      json.RawMessage `json:"metrics"`
		FreshUntil   string          `json:"fresh_until"`
	}{
		RunID: input.RunID.String(), ProfileID: input.ProfileID, SliceKey: input.SliceKey,
		WindowStart:  input.WindowStart.UTC().Format(time.RFC3339Nano),
		WindowEnd:    input.WindowEnd.UTC().Format(time.RFC3339Nano),
		QueryVersion: input.QueryVersion, SourceHash: input.SourceHash,
		Metrics: metricsJSON, FreshUntil: input.FreshUntil.UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("marshal reliability snapshot identity: %w", err)
	}
	return digestReliabilityBytes(encoded), nil
}

func digestReliabilityText(value string) string { return digestReliabilityBytes([]byte(value)) }

func digestReliabilityBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func nullableUUID(value uuid.UUID) any {
	if value == uuid.Nil {
		return nil
	}
	return value
}

func reliabilityHistogramHash(primary, fallback string) string {
	if strings.TrimSpace(primary) != "" {
		return primary
	}
	return fallback
}

func normalizeErrorRate(metrics ReliabilityMetrics) string {
	if strings.TrimSpace(metrics.ErrorRate) != "" {
		return metrics.ErrorRate
	}
	if metrics.RequestCount == 0 {
		return "0"
	}
	return decimal.New(metrics.ErrorNumerator, 0).Div(decimal.New(metrics.RequestCount, 0)).String()
}

func normalizeCost(metrics ReliabilityMetrics) string {
	if strings.TrimSpace(metrics.CostAmount) == "" {
		return "0"
	}
	return metrics.CostAmount
}
