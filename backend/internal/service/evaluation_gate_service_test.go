package service

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func reliabilityGateFixture() (RadarGatePolicy, RadarGateInput) {
	now := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	policy := RadarGatePolicy{
		ObservationDays: 14, EnforcementStartsAt: now.Add(-24 * time.Hour), RequireReliability: true,
		MaxP99LatencyMS: 500, MaxErrorRate: decimal.RequireFromString("0.05"),
		MaxCostPerSuccess: decimal.RequireFromString("1.5"),
	}
	input := RadarGateInput{
		EvidenceSufficient: true, RouteEvidencePresent: true, RouteMatch: true,
		ObservedAt: now, ObservationDays: 14,
		Reliability: &RadarGateReliabilityEvidence{
			HeadPresent: true, Current: true, Fresh: true, DenominatorComplete: true,
			HistogramIntegrityValid: true, SourceWatermarkValid: true, QueryVersionAllowed: true,
			BillingReconciled: true, MaxP99LatencyMS: 400,
			MaxErrorRate: decimal.RequireFromString("0.01"), MaxCostPerSuccess: decimal.RequireFromString("1"),
		},
	}
	return policy, input
}

func TestGateRejectsMissingReliabilityHead(t *testing.T) {
	policy, input := reliabilityGateFixture()
	input.Reliability.HeadPresent = false
	require.Equal(t, RadarGateDecision{Status: RadarGateInsufficientEvidence, RuleID: "evidence.reliability_head"}, EvaluateRadarGate(policy, input))
}

func TestGateRejectsExpiredReliabilitySnapshot(t *testing.T) {
	policy, input := reliabilityGateFixture()
	input.Reliability.Fresh = false
	require.Equal(t, "evidence.reliability_freshness", EvaluateRadarGate(policy, input).RuleID)
}

func TestGateBlocksP99BeforeQualityRules(t *testing.T) {
	policy, input := reliabilityGateFixture()
	input.Reliability.MaxP99LatencyMS = 501
	input.CriticalDeltaPP = -100
	require.Equal(t, RadarGateDecision{Status: RadarGateBlocked, RuleID: "slo.reliability.p99"}, EvaluateRadarGate(policy, input))
}

func TestGateBlocksBillingIdempotencyFailure(t *testing.T) {
	policy, input := reliabilityGateFixture()
	input.Reliability.BillingIdempotencyFailures = 1
	require.Equal(t, RadarGateDecision{Status: RadarGateBlocked, RuleID: "billing.idempotency"}, EvaluateRadarGate(policy, input))
}

func TestGateReliabilityHardStopAppliesDuringCalibration(t *testing.T) {
	policy, input := reliabilityGateFixture()
	policy.EnforcementStartsAt = input.ObservedAt.Add(24 * time.Hour)
	input.Reliability.MaxErrorRate = decimal.RequireFromString("0.06")
	require.Equal(t, "slo.reliability.error_rate", EvaluateRadarGate(policy, input).RuleID)
}
