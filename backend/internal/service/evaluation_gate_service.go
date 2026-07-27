package service

import "time"

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
	JudgeDisagreement      bool
}

type RadarGateDecision struct {
	Status RadarGateDecisionStatus
	RuleID string
}

func EvaluateRadarGate(policy RadarGatePolicy, input RadarGateInput) RadarGateDecision {
	if !input.EvidenceSufficient || !input.RouteEvidencePresent {
		return RadarGateDecision{Status: RadarGateInsufficientEvidence, RuleID: "evidence.sufficient"}
	}
	if input.ObservedAt.Before(policy.EnforcementStartsAt) || input.ObservationDays < policy.ObservationDays {
		return RadarGateDecision{Status: RadarGateRecorded, RuleID: "calibration.record_only"}
	}
	if input.NewP0Failure {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "p0.new_failure"}
	}
	if !input.RouteMatch {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "route.identity"}
	}
	if input.ReliabilitySLOBreached {
		return RadarGateDecision{Status: RadarGateBlocked, RuleID: "slo.reliability"}
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
