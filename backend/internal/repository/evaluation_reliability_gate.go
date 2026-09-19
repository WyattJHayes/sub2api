package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type storedReliabilityGatePolicy struct {
	ObservationDays       int     `json:"observation_days"`
	CriticalDomainDeltaPP float64 `json:"critical_domain_delta_pp"`
	AggregateDeltaPP      float64 `json:"aggregate_delta_pp"`
	ConfidenceLevel       float64 `json:"confidence_level"`
	RequireCIExcludeZero  bool    `json:"require_ci_exclude_zero"`
	Reliability           *struct {
		RequiredSlices       []storedReliabilityGateSlice `json:"required_slices"`
		AllowedQueryVersions []string                     `json:"allowed_query_versions"`
		MaxP99LatencyMS      int64                        `json:"max_p99_latency_ms"`
		MaxErrorRate         decimal.Decimal              `json:"max_error_rate"`
		MaxCostPerSuccess    decimal.Decimal              `json:"max_cost_per_success"`
	} `json:"reliability"`
}

type storedReliabilityGateSlice struct {
	ProfileID string `json:"profile_id"`
	SliceKey  string `json:"slice_key"`
}

type radarGateReliabilityWatermark struct {
	Version       string                                    `json:"version"`
	RunID         uuid.UUID                                 `json:"run_id"`
	PolicyID      uuid.UUID                                 `json:"policy_id"`
	PolicyHash    string                                    `json:"policy_hash"`
	ObservedAt    time.Time                                 `json:"observed_at"`
	GateInputHash string                                    `json:"gate_input_hash"`
	Input         service.RadarGateInput                    `json:"input,omitempty"`
	SnapshotRefs  []service.RadarGateReliabilitySnapshotRef `json:"snapshot_refs"`
}

func (r *radarGovernanceRepository) LoadRadarGateReliability(ctx context.Context, runID, policyID uuid.UUID) (*service.RadarGateReliabilityContext, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if runID == uuid.Nil || policyID == uuid.Nil {
		return nil, errors.New("gate reliability run and policy are required")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin reliability gate evidence load: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	contextValue, err := loadRadarGateReliabilityContext(ctx, tx, runID, policyID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit reliability gate evidence load: %w", err)
	}
	return contextValue, nil
}

func loadRadarGateReliabilityContext(ctx context.Context, tx *sql.Tx, runID, policyID uuid.UUID) (*service.RadarGateReliabilityContext, error) {
	if tx == nil || runID == uuid.Nil || policyID == uuid.Nil {
		return nil, errors.New("gate reliability transaction, run, and policy are required")
	}
	if err := ensureRadarRunTenant(ctx, tx, runID); err != nil {
		return nil, err
	}
	if tenantID, scoped := radarTenant(ctx); scoped {
		var ownerID int64
		if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_gate_policies WHERE id=$1`, policyID).Scan(&ownerID); err != nil {
			return nil, err
		}
		if ownerID != tenantID {
			return nil, service.ErrRadarForbidden
		}
	}
	var observedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&observedAt); err != nil {
		return nil, fmt.Errorf("load gate observation timestamp: %w", err)
	}

	var rawPolicy []byte
	var policyHash string
	var policyVersion int
	var enforcementStartsAt time.Time
	if err := tx.QueryRowContext(ctx, `
		SELECT version, policy, policy_hash, enforcement_starts_at
		FROM evaluation_gate_policies WHERE id=$1 AND retired_at IS NULL`, policyID).
		Scan(&policyVersion, &rawPolicy, &policyHash, &enforcementStartsAt); err != nil {
		return nil, fmt.Errorf("load reliability gate policy: %w", err)
	}
	computedPolicyHash, err := service.DigestCanonicalJSON(rawPolicy)
	if err != nil || computedPolicyHash != policyHash {
		return nil, errors.New("gate policy hash does not match canonical policy")
	}
	if err := ensureGatePolicyApprovalsValid(ctx, tx, policyID, policyHash, observedAt, enforcementStartsAt); err != nil {
		return nil, err
	}
	var document storedReliabilityGatePolicy
	if err := json.Unmarshal(rawPolicy, &document); err != nil {
		return nil, fmt.Errorf("decode reliability gate policy: %w", err)
	}
	policy := service.RadarGatePolicy{
		Version: policyVersion, ObservationDays: document.ObservationDays,
		EnforcementStartsAt: enforcementStartsAt, CriticalDomainDeltaPP: document.CriticalDomainDeltaPP,
		AggregateDeltaPP: document.AggregateDeltaPP, ConfidenceLevel: document.ConfidenceLevel,
		RequireCIExcludeZero: document.RequireCIExcludeZero,
	}
	evidence := service.RadarGateReliabilityEvidence{
		HeadPresent: true, Current: true, Fresh: true, DenominatorComplete: true,
		HistogramIntegrityValid: true, SourceWatermarkValid: true, QueryVersionAllowed: true,
		BillingReconciled: true,
	}
	var requirements []reliabilityGateSliceRequirement
	allowedQueryVersions := map[string]struct{}{}
	if document.Reliability != nil {
		policy.RequireReliability = true
		policy.MaxP99LatencyMS = document.Reliability.MaxP99LatencyMS
		policy.MaxErrorRate = document.Reliability.MaxErrorRate
		policy.MaxCostPerSuccess = document.Reliability.MaxCostPerSuccess
		if policy.MaxP99LatencyMS <= 0 || policy.MaxErrorRate.IsNegative() || policy.MaxErrorRate.GreaterThan(decimal.NewFromInt(1)) || policy.MaxCostPerSuccess.IsNegative() {
			return nil, errors.New("reliability gate thresholds are invalid")
		}
		for _, queryVersion := range document.Reliability.AllowedQueryVersions {
			queryVersion = strings.TrimSpace(queryVersion)
			if queryVersion != "" {
				allowedQueryVersions[queryVersion] = struct{}{}
			}
		}
		requirements, err = normalizeReliabilityGateRequirements(document.Reliability.RequiredSlices)
		if err != nil {
			return nil, err
		}
		if len(requirements) == 0 || len(allowedQueryVersions) == 0 {
			evidence.HeadPresent = false
			evidence.QueryVersionAllowed = false
		}
	}

	for _, requirement := range requirements {
		loaded, loadErr := loadReliabilityGateSnapshot(ctx, tx, runID, requirement, observedAt, allowedQueryVersions)
		if errors.Is(loadErr, sql.ErrNoRows) {
			evidence.HeadPresent = false
			continue
		}
		if loadErr != nil {
			return nil, loadErr
		}
		evidence.SnapshotRefs = append(evidence.SnapshotRefs, loaded.Ref)
		evidence.Fresh = evidence.Fresh && loaded.Fresh
		evidence.DenominatorComplete = evidence.DenominatorComplete && loaded.DenominatorComplete
		evidence.HistogramIntegrityValid = evidence.HistogramIntegrityValid && loaded.HistogramIntegrityValid
		evidence.SourceWatermarkValid = evidence.SourceWatermarkValid && loaded.SourceWatermarkValid
		evidence.QueryVersionAllowed = evidence.QueryVersionAllowed && loaded.QueryVersionAllowed
		evidence.BillingReconciled = evidence.BillingReconciled && loaded.BillingReconciled
		evidence.BillingIdempotencyFailures += loaded.BillingIdempotencyFailures
		if loaded.P99LatencyMS > evidence.MaxP99LatencyMS {
			evidence.MaxP99LatencyMS = loaded.P99LatencyMS
		}
		if loaded.ErrorRate.GreaterThan(evidence.MaxErrorRate) {
			evidence.MaxErrorRate = loaded.ErrorRate
		}
		if loaded.CostPerSuccess.GreaterThan(evidence.MaxCostPerSuccess) {
			evidence.MaxCostPerSuccess = loaded.CostPerSuccess
		}
	}
	authoritativeInput, gateInputHash, err := loadRadarGateAuthoritativeInput(
		ctx, tx, runID, observedAt, policy, evidence,
	)
	if err != nil {
		return nil, err
	}
	sort.Slice(evidence.SnapshotRefs, func(i, j int) bool {
		left, right := evidence.SnapshotRefs[i], evidence.SnapshotRefs[j]
		return left.ProfileID+"\x00"+left.SliceKey < right.ProfileID+"\x00"+right.SliceKey
	})
	watermark := radarGateReliabilityWatermark{
		Version: "radar-gate-reliability-watermark-v1", RunID: runID, PolicyID: policyID,
		PolicyHash: policyHash, ObservedAt: observedAt.UTC(), GateInputHash: gateInputHash,
		Input:        authoritativeInput,
		SnapshotRefs: evidence.SnapshotRefs,
	}
	watermarkBytes, err := json.Marshal(watermark)
	if err != nil {
		return nil, fmt.Errorf("marshal reliability gate watermark: %w", err)
	}
	contextValue := &service.RadarGateReliabilityContext{
		Policy: policy, Evidence: evidence, Input: authoritativeInput, InputLoaded: true,
		InputHash: gateInputHash, PolicyHash: policyHash, ObservedAt: observedAt.UTC(), SourceWatermark: watermarkBytes,
	}
	contextValue.ReleaseSubjectHash, err = loadActiveReleaseSubjectHash(ctx, tx, runID, policyID)
	if err != nil {
		return nil, err
	}
	contextValue.SupersedesDecisionID, err = loadCurrentGateDecisionID(ctx, tx, runID, policyID, contextValue.ReleaseSubjectHash)
	if err != nil {
		return nil, err
	}
	return contextValue, nil
}

func ensureGatePolicyApprovalsValid(ctx context.Context, tx *sql.Tx, policyID uuid.UUID, policyHash string, observedAt, enforcementStartsAt time.Time) error {
	var eligible bool
	if err := tx.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (
				WHERE a.role='quality_admin' AND a.approver_id <> p.created_by
				  AND a.effective_at <= $3 AND a.expires_at > $3
				  AND a.effective_at <= p.enforcement_starts_at AND a.expires_at > p.enforcement_starts_at
			) >= 1
			AND COUNT(*) FILTER (
				WHERE a.role='release_manager' AND a.approver_id <> p.created_by
				  AND a.effective_at <= $3 AND a.expires_at > $3
				  AND a.effective_at <= p.enforcement_starts_at AND a.expires_at > p.enforcement_starts_at
			) >= 1
			AND COUNT(DISTINCT a.approver_id) FILTER (
				WHERE a.approver_id <> p.created_by
				  AND a.effective_at <= $3 AND a.expires_at > $3
				  AND a.effective_at <= p.enforcement_starts_at AND a.expires_at > p.enforcement_starts_at
			) >= 2
		FROM evaluation_gate_policies p
		JOIN evaluation_gate_policy_approvals a ON a.policy_id=p.id AND a.policy_hash=$2
		JOIN evaluation_role_bindings rb
		  ON rb.actor_id=a.approver_id AND rb.role=a.role AND rb.enabled=TRUE
		 AND rb.scope='{}'::jsonb AND rb.tenant_id=p.tenant_id
		WHERE p.id=$1`, policyID, policyHash, observedAt).Scan(&eligible); err != nil {
		return fmt.Errorf("check gate policy approvals: %w", err)
	}
	if !eligible {
		return service.ErrGovernanceHeadConflict
	}
	return nil
}

type radarGateAggregateFact struct {
	Domain         string
	DeltaPP        float64
	CIHighPP       float64
	EffectivePairs int
	Sufficient     bool
	HasDelta       bool
	HasCIHigh      bool
}

// loadRadarGateAuthoritativeInput derives every non-reliability Gate input
// from the same repeatable-read transaction as the reliability heads. The
// request handler deliberately has no way to override these facts.
func loadRadarGateAuthoritativeInput(
	ctx context.Context,
	tx *sql.Tx,
	runID uuid.UUID,
	observedAt time.Time,
	policy service.RadarGatePolicy,
	reliability service.RadarGateReliabilityEvidence,
) (service.RadarGateInput, string, error) {
	var runStartedAt time.Time
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(started_at, created_at)
		FROM evaluation_runs WHERE id=$1`, runID).Scan(&runStartedAt); err != nil {
		return service.RadarGateInput{}, "", fmt.Errorf("load gate run start: %w", err)
	}

	var totalSamples, sealedSamples, baselineSamples, matchedBaselineSamples, p0Failures int64
	if err := tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM evaluation_samples WHERE run_id=$1),
			(SELECT COUNT(*) FROM evaluation_samples s
			 WHERE s.run_id=$1
			   AND EXISTS (
				   SELECT 1 FROM evaluation_route_evidence e
				   WHERE e.evaluation_run_id=s.run_id AND e.sample_id=s.id AND e.sealed_at IS NOT NULL
			   )),
			(SELECT COUNT(*) FROM evaluation_samples
			 WHERE run_id=$1 AND model_route LIKE 'baseline:%'),
			(SELECT COUNT(*) FROM evaluation_samples baseline
			 WHERE baseline.run_id=$1 AND baseline.model_route LIKE 'baseline:%'
			   AND EXISTS (
				   SELECT 1 FROM evaluation_samples candidate
				   WHERE candidate.run_id=baseline.run_id
				     AND candidate.case_id=baseline.case_id
				     AND candidate.sample_index=baseline.sample_index
				     AND candidate.model_route='candidate:' || substring(baseline.model_route from 10)
			   )),
			(SELECT COUNT(*) FROM evaluation_samples
			 WHERE run_id=$1 AND priority='P0'
			   AND (failure_class IS NOT NULL OR status IN ('infra_failed','upstream_failed','invalid_evidence','grading_failed')))
	`, runID).Scan(
		&totalSamples, &sealedSamples, &baselineSamples, &matchedBaselineSamples, &p0Failures,
	); err != nil {
		return service.RadarGateInput{}, "", fmt.Errorf("load gate route and p0 facts: %w", err)
	}

	var judgeDisagreement bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM evaluation_score_heads h
			JOIN evaluation_samples s ON s.id=h.sample_id
			WHERE s.run_id=$1 AND h.manual_review_required
		) OR EXISTS (
			SELECT 1 FROM evaluation_manual_reviews
			WHERE run_id=$1 AND status='pending'
		)`, runID).Scan(&judgeDisagreement); err != nil {
		return service.RadarGateInput{}, "", fmt.Errorf("load gate judge disagreement: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT capability_domain,
		       aggregate->>'delta_pp', aggregate->>'ci_high_pp',
		       aggregate->>'effective_pair_count', aggregate->>'evidence_sufficiency'
		FROM (
			SELECT DISTINCT ON (capability_domain)
			       capability_domain, aggregate, window_start, created_at
			FROM evaluation_aggregate_snapshots
			WHERE run_id=$1
			ORDER BY capability_domain, window_start DESC, created_at DESC
		) latest
		ORDER BY capability_domain`, runID)
	if err != nil {
		return service.RadarGateInput{}, "", fmt.Errorf("load gate aggregate heads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var facts []radarGateAggregateFact
	for rows.Next() {
		var fact radarGateAggregateFact
		var deltaRaw, highRaw, countRaw, sufficiencyRaw sql.NullString
		if err := rows.Scan(&fact.Domain, &deltaRaw, &highRaw, &countRaw, &sufficiencyRaw); err != nil {
			return service.RadarGateInput{}, "", fmt.Errorf("scan gate aggregate head: %w", err)
		}
		if deltaRaw.Valid && strings.TrimSpace(deltaRaw.String) != "" {
			fact.DeltaPP, err = strconv.ParseFloat(deltaRaw.String, 64)
			if err != nil {
				return service.RadarGateInput{}, "", fmt.Errorf("parse gate aggregate delta: %w", err)
			}
			fact.HasDelta = true
		}
		if highRaw.Valid && strings.TrimSpace(highRaw.String) != "" {
			fact.CIHighPP, err = strconv.ParseFloat(highRaw.String, 64)
			if err != nil {
				return service.RadarGateInput{}, "", fmt.Errorf("parse gate aggregate confidence bound: %w", err)
			}
			fact.HasCIHigh = true
		}
		if countRaw.Valid && strings.TrimSpace(countRaw.String) != "" {
			fact.EffectivePairs, err = strconv.Atoi(countRaw.String)
			if err != nil {
				return service.RadarGateInput{}, "", fmt.Errorf("parse gate aggregate pair count: %w", err)
			}
		}
		fact.Sufficient = sufficiencyRaw.Valid && strings.EqualFold(strings.TrimSpace(sufficiencyRaw.String), "sufficient") &&
			fact.EffectivePairs > 0 && fact.HasDelta && fact.HasCIHigh
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return service.RadarGateInput{}, "", fmt.Errorf("iterate gate aggregate heads: %w", err)
	}

	var global *radarGateAggregateFact
	var critical *radarGateAggregateFact
	allQualitySufficient := len(facts) > 0
	for i := range facts {
		fact := &facts[i]
		allQualitySufficient = allQualitySufficient && fact.Sufficient
		if fact.Domain == "global" {
			global = fact
		}
		if fact.Domain != "global" && fact.HasDelta && fact.HasCIHigh && (critical == nil || fact.DeltaPP < critical.DeltaPP) {
			critical = fact
		}
	}
	if critical == nil {
		critical = global
	}

	input := service.RadarGateInput{
		EvidenceSufficient:   global != nil && global.Sufficient && allQualitySufficient,
		RouteEvidencePresent: totalSamples > 0 && sealedSamples == totalSamples,
		RouteMatch:           baselineSamples > 0 && matchedBaselineSamples == baselineSamples,
		ObservedAt:           observedAt.UTC(),
		ObservationDays:      observationDays(observedAt, runStartedAt),
		NewP0Failure:         p0Failures > 0,
		JudgeDisagreement:    judgeDisagreement,
		Reliability:          &reliability,
	}
	if critical != nil {
		input.CriticalDeltaPP = critical.DeltaPP
		input.CriticalCIHighPP = critical.CIHighPP
	}
	if global != nil {
		input.AggregateDeltaPP = global.DeltaPP
		input.AggregateCIHighPP = global.CIHighPP
	}
	input.ReliabilitySLOBreached = reliability.BillingIdempotencyFailures > 0 ||
		(policy.MaxP99LatencyMS > 0 && reliability.MaxP99LatencyMS > policy.MaxP99LatencyMS) ||
		(!policy.MaxErrorRate.IsZero() && reliability.MaxErrorRate.GreaterThan(policy.MaxErrorRate)) ||
		(policy.MaxCostPerSuccess.IsPositive() && reliability.MaxCostPerSuccess.GreaterThan(policy.MaxCostPerSuccess))

	canonical, err := json.Marshal(input)
	if err != nil {
		return service.RadarGateInput{}, "", fmt.Errorf("marshal gate input hash: %w", err)
	}
	inputHash, err := service.DigestCanonicalJSON(canonical)
	if err != nil {
		return service.RadarGateInput{}, "", fmt.Errorf("hash gate input: %w", err)
	}
	return input, inputHash, nil
}

func observationDays(observedAt, startedAt time.Time) int {
	if startedAt.IsZero() || !observedAt.After(startedAt) {
		return 0
	}
	return int(observedAt.Sub(startedAt) / (24 * time.Hour))
}

type reliabilityGateSliceRequirement struct {
	ProfileID string
	SliceKey  string
}

func normalizeReliabilityGateRequirements(raw []storedReliabilityGateSlice) ([]reliabilityGateSliceRequirement, error) {
	requirements := make([]reliabilityGateSliceRequirement, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, required := range raw {
		requirement := reliabilityGateSliceRequirement{
			ProfileID: strings.TrimSpace(required.ProfileID),
			SliceKey:  strings.TrimSpace(required.SliceKey),
		}
		if requirement.ProfileID == "" || requirement.SliceKey == "" {
			return nil, errors.New("reliability gate required slice is incomplete")
		}
		key := requirement.ProfileID + "\x00" + requirement.SliceKey
		if _, exists := seen[key]; exists {
			return nil, errors.New("reliability gate required slice is duplicated")
		}
		seen[key] = struct{}{}
		requirements = append(requirements, requirement)
	}
	return requirements, nil
}

type loadedReliabilityGateSnapshot struct {
	Ref                        service.RadarGateReliabilitySnapshotRef
	Fresh                      bool
	DenominatorComplete        bool
	HistogramIntegrityValid    bool
	SourceWatermarkValid       bool
	QueryVersionAllowed        bool
	BillingReconciled          bool
	BillingIdempotencyFailures int64
	P99LatencyMS               int64
	ErrorRate                  decimal.Decimal
	CostPerSuccess             decimal.Decimal
}

type reliabilityBillingReconciliation struct {
	RequestCount          int64
	IncompleteCount       int64
	MissingLedgerCount    int64
	NotApplicableWithCost int64
	LedgerAmount          decimal.Decimal
}

func reconcileReliabilityBilling(
	ctx context.Context,
	tx *sql.Tx,
	runID uuid.UUID,
	windowStart, windowEnd time.Time,
	requestCount int64,
	costAmount decimal.Decimal,
) (bool, error) {
	var result reliabilityBillingReconciliation
	var ledgerAmount string
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)::bigint,
		       COUNT(*) FILTER (
		           WHERE e.terminal_at IS NULL OR e.billing_status = 'incomplete'
		       )::bigint,
		       COUNT(*) FILTER (
		           WHERE e.billing_status = 'complete' AND COALESCE(b.id, 0) = 0
		       )::bigint,
		       COUNT(*) FILTER (
		           WHERE e.billing_status = 'not_applicable'
		             AND COALESCE(e.billed_amount, 0) <> 0
		       )::bigint,
		       COALESCE(SUM(
		           CASE
		               WHEN e.billing_status = 'complete' AND b.id IS NOT NULL
		                   THEN COALESCE(b.delta_usd, u.actual_cost, 0)
		               WHEN e.billing_status = 'complete'
		                   THEN COALESCE(u.actual_cost, 0)
		               ELSE 0
		           END
		       ), 0)::text
		FROM evaluation_route_evidence e
		LEFT JOIN usage_logs u
		  ON u.request_id = e.request_id AND u.api_key_id = e.api_key_id
		LEFT JOIN billing_usage_entries b ON b.usage_log_id = u.id
		WHERE e.evaluation_run_id = $1
		  AND e.terminal_at >= $2
		  AND e.terminal_at < $3`,
		runID, windowStart, windowEnd).Scan(
		&result.RequestCount,
		&result.IncompleteCount,
		&result.MissingLedgerCount,
		&result.NotApplicableWithCost,
		&ledgerAmount,
	); err != nil {
		return false, fmt.Errorf("reconcile reliability billing: %w", err)
	}
	parsedAmount, err := decimal.NewFromString(ledgerAmount)
	if err != nil {
		return false, fmt.Errorf("decode reconciled billing amount: %w", err)
	}
	result.LedgerAmount = parsedAmount
	return result.RequestCount == requestCount &&
		result.IncompleteCount == 0 &&
		result.MissingLedgerCount == 0 &&
		result.NotApplicableWithCost == 0 &&
		result.LedgerAmount.Equal(costAmount), nil
}

func loadReliabilityGateSnapshot(ctx context.Context, tx *sql.Tx, runID uuid.UUID, requirement reliabilityGateSliceRequirement, observedAt time.Time, allowedQueries map[string]struct{}) (loadedReliabilityGateSnapshot, error) {
	var loaded loadedReliabilityGateSnapshot
	var loadPlanID uuid.NullUUID
	var windowStart, windowEnd, freshUntil time.Time
	var queryVersion, sourceHash, sourceWatermark, snapshotHash string
	var metricsJSON []byte
	var requestCount, successCount, errorCount, timeoutCount, billingFailures, p99LatencyMS int64
	var ttftHash, latencyHash string
	var storedErrorRate, costAmount decimal.Decimal
	if err := tx.QueryRowContext(ctx, `
		SELECT s.id, h.head_event_id, s.reliability_profile_id, s.slice_key,
		       s.snapshot_hash, s.source_hash, s.created_at, s.load_plan_id,
		       s.window_start, s.window_end, s.fresh_until, s.query_version,
		       s.source_watermark, s.metrics, s.request_count, s.success_count,
		       s.error_count, s.timeout_count, s.billing_idempotency_failures,
		       s.ttft_histogram_hash, s.latency_histogram_hash, s.p99_latency_ms,
		       s.error_rate, s.cost_amount
		FROM evaluation_reliability_heads h
		JOIN evaluation_reliability_snapshots s ON s.id=h.snapshot_id AND s.run_id=h.run_id
		WHERE h.run_id=$1 AND h.reliability_profile_id=$2 AND h.slice_key=$3`,
		runID, requirement.ProfileID, requirement.SliceKey).Scan(
		&loaded.Ref.SnapshotID, &loaded.Ref.HeadEventID, &loaded.Ref.ProfileID, &loaded.Ref.SliceKey,
		&loaded.Ref.SnapshotHash, &loaded.Ref.SourceHash, &loaded.Ref.CreatedAt, &loadPlanID,
		&windowStart, &windowEnd, &freshUntil, &queryVersion, &sourceWatermark, &metricsJSON,
		&requestCount, &successCount, &errorCount, &timeoutCount, &billingFailures,
		&ttftHash, &latencyHash, &p99LatencyMS, &storedErrorRate, &costAmount); err != nil {
		return loadedReliabilityGateSnapshot{}, err
	}
	loaded.Fresh = !windowEnd.After(observedAt) && freshUntil.After(observedAt)
	loaded.DenominatorComplete = requestCount > 0 && successCount+errorCount+timeoutCount == requestCount
	_, loaded.QueryVersionAllowed = allowedQueries[queryVersion]
	loaded.BillingReconciled = true
	loaded.BillingIdempotencyFailures = billingFailures
	loaded.P99LatencyMS = p99LatencyMS
	if requestCount > 0 {
		loaded.ErrorRate = decimal.NewFromInt(errorCount + timeoutCount).Div(decimal.NewFromInt(requestCount))
	}
	if !loaded.ErrorRate.Equal(storedErrorRate) {
		loaded.DenominatorComplete = false
	}
	invalidCostWithoutSuccess := successCount == 0 && costAmount.IsPositive()
	if successCount > 0 {
		loaded.CostPerSuccess = costAmount.Div(decimal.NewFromInt(successCount))
	}
	if loadPlanID.Valid && len(metricsJSON) > 0 {
		billingReconciled, billingErr := reconcileReliabilityBilling(
			ctx, tx, runID, windowStart, windowEnd, requestCount, costAmount,
		)
		if billingErr != nil {
			return loadedReliabilityGateSnapshot{}, billingErr
		}
		loaded.BillingReconciled = billingReconciled
	}

	var metrics ReliabilityMetrics
	if err := json.Unmarshal(metricsJSON, &metrics); err != nil {
		return loadedReliabilityGateSnapshot{}, fmt.Errorf("decode reliability gate metrics: %w", err)
	}
	loaded.HistogramIntegrityValid = !invalidCostWithoutSuccess &&
		metrics.TTFTHistogramHash == ttftHash && metrics.LatencyHistogramHash == latencyHash &&
		metrics.P99LatencyMS == p99LatencyMS && metrics.RequestCount == requestCount && metrics.SuccessCount == successCount &&
		metrics.ErrorCount == errorCount && metrics.TimeoutCount == timeoutCount && metrics.BillingIdempotencyFailures == billingFailures
	if loaded.HistogramIntegrityValid {
		ttft, ttftErr := decodeReliabilityHistogram(metrics.TTFTHistogram)
		latency, latencyErr := decodeReliabilityHistogram(metrics.LatencyHistogram)
		computedTTFT, ttftHashErr := service.DigestCanonicalJSON(metrics.TTFTHistogram)
		computedLatency, latencyHashErr := service.DigestCanonicalJSON(metrics.LatencyHistogram)
		loaded.HistogramIntegrityValid = ttftErr == nil && latencyErr == nil && ttftHashErr == nil && latencyHashErr == nil &&
			computedTTFT == ttftHash && computedLatency == latencyHash && ttft.SampleCount <= successCount &&
			latency.SampleCount == successCount && latency.percentile(0.99) == p99LatencyMS
	}
	loaded.SourceWatermarkValid = sourceHash == sourceWatermark && loadPlanID.Valid && len(metrics.SourceManifest) > 0
	if loaded.SourceWatermarkValid {
		computedSource, sourceErr := service.DigestCanonicalJSON(metrics.SourceManifest)
		var manifest reliabilitySourceManifest
		manifestErr := json.Unmarshal(metrics.SourceManifest, &manifest)
		submission := service.ReliabilitySnapshotSubmission{
			WorkerImageDigest: manifest.WorkerImageDigest, RunID: runID, LoadPlanID: loadPlanID.UUID, ProfileID: manifest.ProfileID,
			WindowStart: windowStart, WindowEnd: windowEnd, SourceWatermark: sourceWatermark,
			SourceManifest: metrics.SourceManifest, QueryVersion: queryVersion, SliceKey: requirement.SliceKey,
			RequestCount: requestCount, SuccessCount: successCount, ErrorCount: errorCount, TimeoutCount: timeoutCount,
			RetryCount: metrics.RetryCount, ProtocolErrorCount: metrics.ProtocolErrorCount,
			BillingIdempotencyFailures: billingFailures, TTFTHistogramHash: ttftHash,
			LatencyHistogramHash: latencyHash, P99LatencyMS: p99LatencyMS,
			ErrorRate: storedErrorRate, CostAmount: costAmount, FreshUntil: freshUntil,
		}
		loaded.SourceWatermarkValid = sourceErr == nil && manifestErr == nil && computedSource == sourceWatermark &&
			validateReliabilitySourceManifest(submission) == nil
	}
	if loaded.HistogramIntegrityValid && loaded.SourceWatermarkValid {
		metricsCanonical, marshalErr := json.Marshal(metrics)
		computedSnapshotHash, hashErr := reliabilitySnapshotHash(ReliabilitySnapshotInput{
			RunID: runID, LoadPlanID: loadPlanID.UUID, ProfileID: requirement.ProfileID, SliceKey: requirement.SliceKey,
			WindowStart: windowStart, WindowEnd: windowEnd, QueryVersion: queryVersion,
			SourceHash: sourceHash, Metrics: metrics, FreshUntil: freshUntil,
		}, metricsCanonical)
		loaded.HistogramIntegrityValid = marshalErr == nil && hashErr == nil && computedSnapshotHash == snapshotHash
	}
	return loaded, nil
}

func loadActiveReleaseSubjectHash(ctx context.Context, tx *sql.Tx, runID, policyID uuid.UUID) (string, error) {
	var subjectHash string
	var subjectJSON []byte
	var subjectTenantID int64
	err := tx.QueryRowContext(ctx, `
		SELECT rs.subject_hash, rs.canonical_subject, rs.tenant_id
		FROM evaluation_release_subjects rs
		JOIN evaluation_runs r ON r.id=rs.run_id AND r.tenant_id=rs.tenant_id
		JOIN LATERAL (
			SELECT event_type, effective_at, expires_at
			FROM evaluation_release_subject_events e
			WHERE e.release_subject_id=rs.id
			ORDER BY sequence DESC LIMIT 1
		) event ON event.event_type='activated' AND event.effective_at <= transaction_timestamp()
		          AND event.expires_at > transaction_timestamp()
		WHERE rs.run_id=$1 ORDER BY rs.created_at DESC LIMIT 1`, runID).Scan(&subjectHash, &subjectJSON, &subjectTenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", service.ErrGovernanceHeadConflict
	}
	if err != nil {
		return "", fmt.Errorf("load active release subject: %w", err)
	}
	var subject service.ReleaseSubject
	if err := json.Unmarshal(subjectJSON, &subject); err != nil {
		return "", fmt.Errorf("decode active release subject: %w", err)
	}
	var currentPolicyID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		SELECT policy_id FROM evaluation_gate_policy_heads
		WHERE tenant_id=$1 AND environment=$2 AND scope_type=$3 AND scope_id=$4`, subjectTenantID, subject.DeploymentEnvironment, subject.ScopeType, subject.ScopeID).
		Scan(&currentPolicyID); err != nil {
		return "", fmt.Errorf("load active gate policy head: %w", err)
	}
	if currentPolicyID != policyID {
		return "", service.ErrGovernanceHeadConflict
	}
	return subjectHash, nil
}

func loadCurrentGateDecisionID(ctx context.Context, tx *sql.Tx, runID, policyID uuid.UUID, releaseSubjectHash string) (*uuid.UUID, error) {
	var decisionID uuid.UUID
	err := tx.QueryRowContext(ctx, `
		SELECT decision_id FROM evaluation_gate_decision_heads
		WHERE run_id=$1 AND policy_id=$2 AND release_subject_hash=$3`, runID, policyID, releaseSubjectHash).Scan(&decisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load current gate decision head: %w", err)
	}
	return &decisionID, nil
}

func validateGateReliabilityWatermark(ctx context.Context, tx *sql.Tx, runID, policyID uuid.UUID, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "{}" {
		return service.ErrGovernanceHeadConflict
	}
	var watermark radarGateReliabilityWatermark
	if err := json.Unmarshal(raw, &watermark); err != nil || watermark.Version != "radar-gate-reliability-watermark-v1" ||
		watermark.RunID != runID || watermark.PolicyID != policyID {
		return service.ErrGovernanceHeadConflict
	}
	if watermark.GateInputHash != "" {
		canonicalInput, err := json.Marshal(watermark.Input)
		if err != nil {
			return service.ErrGovernanceHeadConflict
		}
		computedInputHash, err := service.DigestCanonicalJSON(canonicalInput)
		if err != nil || computedInputHash != watermark.GateInputHash {
			return service.ErrGovernanceHeadConflict
		}
	}
	var currentPolicyHash string
	var rawPolicy []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT policy_hash, policy FROM evaluation_gate_policies
		WHERE id=$1 AND retired_at IS NULL
		FOR SHARE`, policyID).Scan(&currentPolicyHash, &rawPolicy); err != nil || currentPolicyHash != watermark.PolicyHash {
		return service.ErrGovernanceHeadConflict
	}
	var policy storedReliabilityGatePolicy
	if err := json.Unmarshal(rawPolicy, &policy); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	var requirements []reliabilityGateSliceRequirement
	if policy.Reliability != nil {
		var err error
		requirements, err = normalizeReliabilityGateRequirements(policy.Reliability.RequiredSlices)
		if err != nil {
			return service.ErrGovernanceHeadConflict
		}
	}
	if len(watermark.SnapshotRefs) != len(requirements) {
		return service.ErrGovernanceHeadConflict
	}
	expectedRefs := make(map[string]struct{}, len(requirements))
	for _, requirement := range requirements {
		expectedRefs[requirement.ProfileID+"\x00"+requirement.SliceKey] = struct{}{}
	}
	var transactionNow time.Time
	if err := tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&transactionNow); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	for _, ref := range watermark.SnapshotRefs {
		key := ref.ProfileID + "\x00" + ref.SliceKey
		if _, expected := expectedRefs[key]; !expected {
			return service.ErrGovernanceHeadConflict
		}
		delete(expectedRefs, key)
		var snapshotID, headEventID uuid.UUID
		var snapshotHash, sourceHash string
		var createdAt, freshUntil time.Time
		if err := tx.QueryRowContext(ctx, `
			SELECT h.snapshot_id, h.head_event_id, h.snapshot_hash, s.source_hash, s.created_at, s.fresh_until
			FROM evaluation_reliability_heads h
			JOIN evaluation_reliability_snapshots s ON s.id=h.snapshot_id AND s.run_id=h.run_id
			WHERE h.run_id=$1 AND h.reliability_profile_id=$2 AND h.slice_key=$3`,
			runID, ref.ProfileID, ref.SliceKey).Scan(&snapshotID, &headEventID, &snapshotHash, &sourceHash, &createdAt, &freshUntil); err != nil {
			return service.ErrGovernanceHeadConflict
		}
		if snapshotID != ref.SnapshotID || headEventID != ref.HeadEventID || snapshotHash != ref.SnapshotHash ||
			sourceHash != ref.SourceHash || !createdAt.Equal(ref.CreatedAt) || !freshUntil.After(transactionNow) {
			return service.ErrGovernanceHeadConflict
		}
	}
	return nil
}

func isRadarGateReliabilityWatermark(raw json.RawMessage) bool {
	var watermark radarGateReliabilityWatermark
	return json.Unmarshal(raw, &watermark) == nil &&
		watermark.Version == "radar-gate-reliability-watermark-v1"
}

// validateCurrentGateAuthority closes the gap between the read-only evidence
// load and the decision write. The same advisory keys used by policy and
// release-subject activation serialize head changes while this transaction
// rechecks the active subject and approval window.
func validateCurrentGateAuthority(ctx context.Context, tx *sql.Tx, runID, policyID uuid.UUID, releaseSubjectHash string) error {
	if !validLowerHexSHA256(releaseSubjectHash) {
		return service.ErrGovernanceHeadConflict
	}

	var subjectID uuid.UUID
	var subjectHash string
	var subjectJSON []byte
	var subjectTenantID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT rs.id,rs.subject_hash,rs.canonical_subject,rs.tenant_id
		FROM evaluation_release_subjects rs
		JOIN evaluation_runs r ON r.id=rs.run_id AND r.tenant_id=rs.tenant_id
		JOIN LATERAL (
			SELECT event_type,effective_at,expires_at
			FROM evaluation_release_subject_events e
			WHERE e.release_subject_id=rs.id
			ORDER BY sequence DESC LIMIT 1
		) event ON event.event_type='activated'
			          AND event.effective_at <= transaction_timestamp()
			          AND event.expires_at > transaction_timestamp()
		WHERE rs.run_id=$1
		ORDER BY rs.created_at DESC LIMIT 1`, runID).
		Scan(&subjectID, &subjectHash, &subjectJSON, &subjectTenantID); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	if subjectHash != releaseSubjectHash {
		return service.ErrGovernanceHeadConflict
	}
	var subject service.ReleaseSubject
	if err := json.Unmarshal(subjectJSON, &subject); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "release-subject:"+subjectID.String()); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT rs.subject_hash,rs.canonical_subject
		FROM evaluation_release_subjects rs
		JOIN LATERAL (
			SELECT event_type,effective_at,expires_at
			FROM evaluation_release_subject_events e
			WHERE e.release_subject_id=rs.id
			ORDER BY sequence DESC LIMIT 1
		) event ON event.event_type='activated'
			          AND event.effective_at <= transaction_timestamp()
			          AND event.expires_at > transaction_timestamp()
		WHERE rs.id=$1`, subjectID).
		Scan(&subjectHash, &subjectJSON); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	if subjectHash != releaseSubjectHash {
		return service.ErrGovernanceHeadConflict
	}
	if err := json.Unmarshal(subjectJSON, &subject); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		fmt.Sprintf("policy:%d:%s:%s:%s", subjectTenantID, subject.DeploymentEnvironment, subject.ScopeType, subject.ScopeID)); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	var currentPolicyID uuid.UUID
	if err := tx.QueryRowContext(ctx, `
		SELECT policy_id FROM evaluation_gate_policy_heads
		WHERE tenant_id=$1 AND environment=$2 AND scope_type=$3 AND scope_id=$4
		FOR UPDATE`, subjectTenantID, subject.DeploymentEnvironment, subject.ScopeType, subject.ScopeID).
		Scan(&currentPolicyID); err != nil || currentPolicyID != policyID {
		return service.ErrGovernanceHeadConflict
	}
	var policyHash string
	var enforcementStartsAt time.Time
	var retiredAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `
		SELECT policy_hash,enforcement_starts_at,retired_at
		FROM evaluation_gate_policies WHERE id=$1 AND tenant_id=$2 FOR SHARE`, policyID, subjectTenantID).
		Scan(&policyHash, &enforcementStartsAt, &retiredAt); err != nil || retiredAt.Valid {
		return service.ErrGovernanceHeadConflict
	}
	var observedAt time.Time
	if err := tx.QueryRowContext(ctx, `SELECT transaction_timestamp()`).Scan(&observedAt); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	if err := ensureGatePolicyApprovalsValid(ctx, tx, policyID, policyHash, observedAt, enforcementStartsAt); err != nil {
		return service.ErrGovernanceHeadConflict
	}
	return nil
}
