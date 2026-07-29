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
)

const (
	minimumReliabilityLatencySamples = 200
	minimumReliabilityQualityPairs   = 30
)

type ReliabilityMetrics struct {
	RequestCount               int64  `json:"request_count"`
	SuccessfulLatencyCount     int64  `json:"successful_latency_count"`
	ValidPairCount             int64  `json:"valid_pair_count"`
	UpstreamFailureCount       int64  `json:"upstream_failure_count"`
	GatewayFailureCount        int64  `json:"gateway_failure_count"`
	ClientCancellationCount    int64  `json:"client_cancellation_count"`
	ErrorNumerator             int64  `json:"error_numerator"`
	ErrorDenominator           int64  `json:"error_denominator"`
	P99LatencyMS               int64  `json:"p99_latency_ms"`
	HistogramOrSketchHash      string `json:"histogram_or_sketch_hash"`
	OngoingConfirmedP0Incident bool   `json:"ongoing_confirmed_p0_incident"`
}

type ReliabilitySnapshotInput struct {
	RunID        uuid.UUID
	ProfileID    string
	SliceKey     string
	WindowStart  time.Time
	WindowEnd    time.Time
	QueryVersion string
	SourceHash   string
	Metrics      ReliabilityMetrics
	FreshUntil   time.Time
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
	metrics.ErrorDenominator = metrics.RequestCount - metrics.ClientCancellationCount
	metrics.ErrorNumerator = metrics.UpstreamFailureCount + metrics.GatewayFailureCount
	if metrics.ErrorDenominator < 0 || metrics.ErrorNumerator > metrics.ErrorDenominator {
		return ReliabilityMetrics{}, errors.New("reliability error counts exceed the eligible request denominator")
	}
	if metrics.SuccessfulLatencyCount > metrics.RequestCount {
		return ReliabilityMetrics{}, errors.New("successful latency count exceeds request count")
	}
	classifiedRequests := metrics.SuccessfulLatencyCount + metrics.UpstreamFailureCount +
		metrics.GatewayFailureCount + metrics.ClientCancellationCount
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

	requestedID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_reliability_snapshots (
			id, run_id, reliability_profile_id, slice_key, window_start, window_end,
			query_version, source_hash, metrics, snapshot_hash, fresh_until
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11)
		ON CONFLICT (run_id, reliability_profile_id, slice_key, window_start, window_end, source_hash)
		DO NOTHING`, requestedID, input.RunID, input.ProfileID, input.SliceKey,
		input.WindowStart, input.WindowEnd, input.QueryVersion, input.SourceHash,
		string(metricsJSON), snapshotHash, input.FreshUntil); err != nil {
		return nil, fmt.Errorf("insert reliability snapshot: %w", err)
	}

	snapshot := &ReliabilitySnapshot{}
	var storedMetrics []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, reliability_profile_id, slice_key, window_start, window_end,
		       query_version, source_hash, metrics, snapshot_hash, fresh_until, created_at
		FROM evaluation_reliability_snapshots
		WHERE run_id=$1 AND reliability_profile_id=$2 AND slice_key=$3
		  AND window_start=$4 AND window_end=$5 AND source_hash=$6`,
		input.RunID, input.ProfileID, input.SliceKey, input.WindowStart, input.WindowEnd, input.SourceHash).Scan(
		&snapshot.ID, &snapshot.RunID, &snapshot.ProfileID, &snapshot.SliceKey,
		&snapshot.WindowStart, &snapshot.WindowEnd, &snapshot.QueryVersion,
		&snapshot.SourceHash, &storedMetrics, &snapshot.SnapshotHash,
		&snapshot.FreshUntil, &snapshot.CreatedAt); err != nil {
		return nil, fmt.Errorf("load reliability snapshot: %w", err)
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
