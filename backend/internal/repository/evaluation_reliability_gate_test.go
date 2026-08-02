package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestLoadRadarGateReliabilityRejectsPolicyFromAnotherTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	runID, policyID := uuid.New(), uuid.New()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_runs WHERE id=\$1`).
		WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(41)))
	mock.ExpectQuery(`SELECT tenant_id FROM evaluation_gate_policies WHERE id=\$1`).
		WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"tenant_id"}).AddRow(int64(42)))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).LoadRadarGateReliability(
		service.WithRadarTenant(context.Background(), 41), runID, policyID,
	)
	require.ErrorIs(t, err, service.ErrRadarForbidden)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNormalizeReliabilityGateRequirementsRejectsDuplicateSliceKeys(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	mock.ExpectBegin()
	runID, policyID := uuid.New(), uuid.New()
	mock.ExpectQuery(`SELECT transaction_timestamp\(\)`).
		WillReturnRows(sqlmock.NewRows([]string{"transaction_timestamp"}).AddRow(time.Now().UTC()))
	policy := json.RawMessage(`{"observation_days":14,"reliability":{"required_slices":[{"profile_id":"profile-a","slice_key":" region:global "},{"profile_id":" profile-a ","slice_key":"region:global"}],"allowed_query_versions":["reliability-query-v1"],"max_p99_latency_ms":1000,"max_error_rate":"1","max_cost_per_success":"10"}}`)
	policyHash, err := service.DigestCanonicalJSON(policy)
	require.NoError(t, err)
	mock.ExpectQuery(`(?s)SELECT version, policy, policy_hash, enforcement_starts_at`).
		WithArgs(policyID).
		WillReturnRows(sqlmock.NewRows([]string{"version", "policy", "policy_hash", "enforcement_starts_at"}).
			AddRow(1, policy, policyHash, time.Now().UTC()))
	mock.ExpectQuery(`(?s)SELECT.*COUNT.*quality_admin.*FROM evaluation_gate_policies p.*JOIN evaluation_gate_policy_approvals`).
		WithArgs(policyID, policyHash, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"eligible"}).AddRow(true))
	mock.ExpectRollback()

	_, err = (&radarGovernanceRepository{db: db}).LoadRadarGateReliability(context.Background(), runID, policyID)
	require.ErrorContains(t, err, "reliability gate required slice is duplicated")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadReliabilityGateSnapshotKeepsHistogramInvalidWhenCostHasNoSuccesses(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	runID := uuid.New()
	snapshotID := uuid.New()
	headEventID := uuid.New()
	createdAt := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)
	windowStart := createdAt.Add(-time.Hour)
	windowEnd := createdAt.Add(-30 * time.Minute)
	freshUntil := createdAt.Add(time.Hour)
	ttftHistogram := mustJSON(reliabilityHistogramEvidence{
		BucketBoundsMS: []int64{100}, Counts: []int64{0, 0}, SampleCount: 0,
	})
	latencyHistogram := mustJSON(reliabilityHistogramEvidence{
		BucketBoundsMS: []int64{100}, Counts: []int64{0, 0}, SampleCount: 0,
	})
	ttftHash := mustCanonicalDigest(ttftHistogram)
	latencyHash := mustCanonicalDigest(latencyHistogram)
	metrics, err := json.Marshal(ReliabilityMetrics{
		RequestCount: 1, SuccessCount: 0, ErrorCount: 1, P99LatencyMS: 0,
		TTFTHistogramHash: ttftHash, LatencyHistogramHash: latencyHash,
		TTFTHistogram: ttftHistogram, LatencyHistogram: latencyHistogram,
	})
	require.NoError(t, err)

	mock.ExpectQuery(`(?s)SELECT s\.id, h\.head_event_id`).
		WithArgs(runID, "profile-a", "region:global").
		WillReturnRows(sqlmock.NewRows([]string{
			"snapshot_id", "head_event_id", "reliability_profile_id", "slice_key",
			"snapshot_hash", "source_hash", "created_at", "load_plan_id", "window_start",
			"window_end", "fresh_until", "query_version", "source_watermark", "metrics",
			"request_count", "success_count", "error_count", "timeout_count",
			"billing_idempotency_failures", "ttft_histogram_hash", "latency_histogram_hash",
			"p99_latency_ms", "error_rate", "cost_amount",
		}).AddRow(
			snapshotID, headEventID, "profile-a", "region:global", strings.Repeat("c", 64),
			strings.Repeat("d", 64), createdAt, nil, windowStart, windowEnd, freshUntil,
			"reliability-query-v1", strings.Repeat("e", 64), metrics, int64(1), int64(0),
			int64(1), int64(0), int64(0), ttftHash, latencyHash, int64(0), "1", "1.25",
		))
	mock.ExpectRollback()

	loaded, err := loadReliabilityGateSnapshot(
		context.Background(), tx, runID,
		reliabilityGateSliceRequirement{ProfileID: "profile-a", SliceKey: "region:global"},
		createdAt, map[string]struct{}{"reliability-query-v1": {}},
	)
	require.NoError(t, err)
	require.False(t, loaded.HistogramIntegrityValid)

	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestLoadReliabilityGateSnapshotRejectsUnreconciledBillingLedger(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	runID := uuid.New()
	snapshotID := uuid.New()
	headEventID := uuid.New()
	loadPlanID := uuid.New()
	createdAt := time.Date(2026, 7, 30, 1, 0, 0, 0, time.UTC)
	windowStart := createdAt.Add(-time.Hour)
	windowEnd := createdAt.Add(-30 * time.Minute)
	freshUntil := createdAt.Add(time.Hour)
	metrics := json.RawMessage(`{"request_count":1}`)
	mock.ExpectQuery(`(?s)SELECT s\.id, h\.head_event_id`).
		WithArgs(runID, "profile-a", "region:global").
		WillReturnRows(sqlmock.NewRows([]string{
			"snapshot_id", "head_event_id", "reliability_profile_id", "slice_key",
			"snapshot_hash", "source_hash", "created_at", "load_plan_id", "window_start",
			"window_end", "fresh_until", "query_version", "source_watermark", "metrics",
			"request_count", "success_count", "error_count", "timeout_count",
			"billing_idempotency_failures", "ttft_histogram_hash", "latency_histogram_hash",
			"p99_latency_ms", "error_rate", "cost_amount",
		}).AddRow(
			snapshotID, headEventID, "profile-a", "region:global", strings.Repeat("c", 64),
			strings.Repeat("d", 64), createdAt, loadPlanID, windowStart, windowEnd, freshUntil,
			"reliability-query-v1", strings.Repeat("e", 64), metrics, int64(1), int64(1),
			int64(0), int64(0), int64(0), strings.Repeat("f", 64), strings.Repeat("a", 64),
			int64(100), "0", "0",
		))
	mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*evaluation_route_evidence`).
		WithArgs(runID, windowStart, windowEnd).
		WillReturnRows(sqlmock.NewRows([]string{
			"request_count", "incomplete_count", "missing_ledger_count",
			"not_applicable_with_cost", "ledger_amount",
		}).AddRow(int64(1), int64(1), int64(0), int64(0), "0"))
	mock.ExpectRollback()

	loaded, err := loadReliabilityGateSnapshot(
		context.Background(), tx, runID,
		reliabilityGateSliceRequirement{ProfileID: "profile-a", SliceKey: "region:global"},
		createdAt, map[string]struct{}{"reliability-query-v1": {}},
	)
	require.NoError(t, err)
	require.False(t, loaded.BillingReconciled)
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestNormalizeReliabilityGateRequirementsPreservesUniqueSlices(t *testing.T) {
	got, err := normalizeReliabilityGateRequirements([]storedReliabilityGateSlice{
		{ProfileID: " profile-a ", SliceKey: "region:global"},
		{ProfileID: "profile-b", SliceKey: " region:edge "},
	})
	require.NoError(t, err)
	require.Equal(t, []reliabilityGateSliceRequirement{
		{ProfileID: "profile-a", SliceKey: "region:global"},
		{ProfileID: "profile-b", SliceKey: "region:edge"},
	}, got)
}

func TestLoadRadarGateAuthoritativeInputUsesDurableFacts(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()

	runID := uuid.New()
	startedAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	observedAt := startedAt.Add(15 * 24 * time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT COALESCE\(started_at, created_at\)`).
		WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"started_at"}).AddRow(startedAt))
	mock.ExpectQuery(`(?s)SELECT\s+\(SELECT COUNT\(\*\) FROM evaluation_samples`).
		WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"total", "sealed", "baseline", "matched", "p0"}).AddRow(2, 2, 1, 1, 0))
	mock.ExpectQuery(`(?s)SELECT EXISTS \(`).
		WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"judge_disagreement"}).AddRow(false))
	mock.ExpectQuery(`(?s)SELECT capability_domain,`).
		WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"capability_domain", "delta", "ci_high", "pairs", "sufficiency"}).
			AddRow("coding", "-4.5", "-1.2", "30", "sufficient").
			AddRow("global", "-2.5", "-0.5", "30", "sufficient"))
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	input, inputHash, err := loadRadarGateAuthoritativeInput(
		context.Background(), tx, runID, observedAt, service.RadarGatePolicy{}, service.RadarGateReliabilityEvidence{},
	)
	require.NoError(t, err)
	require.True(t, input.EvidenceSufficient)
	require.True(t, input.RouteEvidencePresent)
	require.True(t, input.RouteMatch)
	require.Equal(t, 15, input.ObservationDays)
	require.Equal(t, -4.5, input.CriticalDeltaPP)
	require.Equal(t, -2.5, input.AggregateDeltaPP)
	require.False(t, input.NewP0Failure)
	require.False(t, input.JudgeDisagreement)
	require.Len(t, inputHash, 64)
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestObservationDaysDoesNotUseCallerTimestampWhenRunHasNoElapsedTime(t *testing.T) {
	startedAt := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	require.Equal(t, 0, observationDays(startedAt, startedAt.Add(time.Hour)))
	require.Equal(t, 0, observationDays(startedAt.Add(-time.Hour), startedAt))
}
