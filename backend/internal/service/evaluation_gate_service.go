package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

type RadarGateDecisionStatus string

const (
	RadarGateRecorded             RadarGateDecisionStatus = "recorded"
	RadarGatePassed               RadarGateDecisionStatus = "passed"
	RadarGateBlocked              RadarGateDecisionStatus = "blocked"
	RadarGateReviewRequired       RadarGateDecisionStatus = "review_required"
	RadarGateInsufficientEvidence RadarGateDecisionStatus = "insufficient_evidence"
	RadarGateWaived               RadarGateDecisionStatus = "waived"
)

type RadarGatePolicy struct {
	Version               int
	ObservationDays       int
	EnforcementStartsAt   time.Time
	CriticalDomainDeltaPP float64
	AggregateDeltaPP      float64
	ConfidenceLevel       float64
	RequireCIExcludeZero  bool
	RequireReliability    bool
	MaxP99LatencyMS       int64
	MaxErrorRate          decimal.Decimal
	MaxCostPerSuccess     decimal.Decimal
}

func DefaultRadarGatePolicy(enabledAt time.Time) RadarGatePolicy {
	return RadarGatePolicy{Version: 1, ObservationDays: 14, EnforcementStartsAt: enabledAt.UTC().Add(14 * 24 * time.Hour), CriticalDomainDeltaPP: -3, AggregateDeltaPP: -2, ConfidenceLevel: 0.95, RequireCIExcludeZero: true}
}

type RadarGateInput struct {
	EvidenceSufficient     bool
	RouteEvidencePresent   bool
	RouteMatch             bool
	ObservedAt             time.Time
	ObservationDays        int
	NewP0Failure           bool
	CriticalDeltaPP        float64
	CriticalCIHighPP       float64
	AggregateDeltaPP       float64
	AggregateCIHighPP      float64
	ReliabilitySLOBreached bool
	Reliability            *RadarGateReliabilityEvidence
	JudgeDisagreement      bool
}

type RadarGateReliabilitySnapshotRef struct {
	SnapshotID   uuid.UUID `json:"snapshot_id"`
	HeadEventID  uuid.UUID `json:"head_event_id"`
	ProfileID    string    `json:"reliability_profile_id"`
	SliceKey     string    `json:"slice_key"`
	SnapshotHash string    `json:"snapshot_hash"`
	SourceHash   string    `json:"source_hash"`
	CreatedAt    time.Time `json:"snapshot_created_at"`
}

type RadarGateReliabilityEvidence struct {
	HeadPresent                bool
	Current                    bool
	Fresh                      bool
	DenominatorComplete        bool
	HistogramIntegrityValid    bool
	SourceWatermarkValid       bool
	QueryVersionAllowed        bool
	BillingReconciled          bool
	BillingIdempotencyFailures int64
	MaxP99LatencyMS            int64
	MaxErrorRate               decimal.Decimal
	MaxCostPerSuccess          decimal.Decimal
	SnapshotRefs               []RadarGateReliabilitySnapshotRef
}

type RadarGateReliabilityContext struct {
	Policy   RadarGatePolicy
	Evidence RadarGateReliabilityEvidence
	// Input is populated by the read-only Gate Evidence Loader from durable
	// score, aggregate, route, and run state. Request payloads must never fill
	// these fields because they influence a release decision.
	Input                RadarGateInput
	InputLoaded          bool
	InputHash            string
	PolicyHash           string
	ObservedAt           time.Time
	ReleaseSubjectHash   string
	SourceWatermark      json.RawMessage
	SupersedesDecisionID *uuid.UUID
}

type RadarGateReliabilityLoader interface {
	LoadRadarGateReliability(context.Context, uuid.UUID, uuid.UUID) (*RadarGateReliabilityContext, error)
}

type RadarGateEvidenceEnvelope struct {
	Version     string                       `json:"version"`
	RunID       uuid.UUID                    `json:"run_id"`
	PolicyID    uuid.UUID                    `json:"policy_id"`
	PolicyHash  string                       `json:"policy_hash"`
	ObservedAt  time.Time                    `json:"observed_at"`
	Input       RadarGateInput               `json:"input"`
	Reliability RadarGateReliabilityEvidence `json:"reliability"`
}

func BuildRadarGateEvidenceEnvelope(runID, policyID uuid.UUID, policyHash string, observedAt time.Time, input RadarGateInput, evidence RadarGateReliabilityEvidence) (json.RawMessage, string, error) {
	envelope := RadarGateEvidenceEnvelope{
		Version: "radar-gate-evidence-v1", RunID: runID, PolicyID: policyID,
		PolicyHash: policyHash, ObservedAt: observedAt.UTC(), Input: input, Reliability: evidence,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return nil, "", err
	}
	digest, err := DigestCanonicalJSON(raw)
	if err != nil {
		return nil, "", err
	}
	return raw, digest, nil
}

type RadarGateDecision struct {
	Status RadarGateDecisionStatus
	RuleID string
}

func EvaluateRadarGate(policy RadarGatePolicy, input RadarGateInput) RadarGateDecision {
	if !input.EvidenceSufficient || !input.RouteEvidencePresent {
		return RadarGateDecision{Status: RadarGateInsufficientEvidence, RuleID: "evidence.sufficient"}
	}
	if policy.RequireReliability {
		if input.Reliability == nil || !input.Reliability.HeadPresent {
			return RadarGateDecision{Status: RadarGateInsufficientEvidence, RuleID: "evidence.reliability_head"}
		}
		if !input.Reliability.Current || !input.Reliability.Fresh {
			return RadarGateDecision{Status: RadarGateInsufficientEvidence, RuleID: "evidence.reliability_freshness"}
		}
		if !input.Reliability.DenominatorComplete || !input.Reliability.HistogramIntegrityValid ||
			!input.Reliability.SourceWatermarkValid || !input.Reliability.QueryVersionAllowed {
			return RadarGateDecision{Status: RadarGateInsufficientEvidence, RuleID: "evidence.reliability_integrity"}
		}
		if !input.Reliability.BillingReconciled {
			return RadarGateDecision{Status: RadarGateInsufficientEvidence, RuleID: "evidence.billing_reconciliation"}
		}
	}
	if input.NewP0Failure {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "p0.new_failure"}
	}
	if !input.RouteMatch {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "route.identity"}
	}
	if policy.RequireReliability && input.Reliability.BillingIdempotencyFailures > 0 {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "billing.idempotency"}
	}
	if policy.RequireReliability && policy.MaxP99LatencyMS > 0 && input.Reliability.MaxP99LatencyMS > policy.MaxP99LatencyMS {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "slo.reliability.p99"}
	}
	if policy.RequireReliability && input.Reliability.MaxErrorRate.GreaterThan(policy.MaxErrorRate) {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "slo.reliability.error_rate"}
	}
	if policy.RequireReliability && policy.MaxCostPerSuccess.IsPositive() && input.Reliability.MaxCostPerSuccess.GreaterThan(policy.MaxCostPerSuccess) {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "cost.per_success"}
	}
	if input.ReliabilitySLOBreached {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "slo.reliability"}
	}
	if input.ObservedAt.Before(policy.EnforcementStartsAt) || input.ObservationDays < policy.ObservationDays {
		return RadarGateDecision{Status: RadarGateRecorded, RuleID: "calibration.record_only"}
	}
	if input.CriticalDeltaPP <= policy.CriticalDomainDeltaPP && (!policy.RequireCIExcludeZero || input.CriticalCIHighPP < 0) {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "quality.critical_domain"}
	}
	if input.AggregateDeltaPP <= policy.AggregateDeltaPP && (!policy.RequireCIExcludeZero || input.AggregateCIHighPP < 0) {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "quality.aggregate"}
	}
	if input.JudgeDisagreement {
		return RadarGateDecision{Status: RadarGateReviewRequired, RuleID: "judge.disagreement"}
	}
	return RadarGateDecision{Status: RadarGatePassed, RuleID: "pass"}
}
