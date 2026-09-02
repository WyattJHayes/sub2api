package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

type QualityDimension string

const (
	QualityDimensionKnowledgeFreshness   QualityDimension = "knowledge_freshness"
	QualityDimensionModelFingerprint     QualityDimension = "model_fingerprint"
	QualityDimensionReasoningStability   QualityDimension = "reasoning_stability"
	QualityDimensionStructureCompliance  QualityDimension = "structure_compliance"
	QualityDimensionParameterFidelity    QualityDimension = "parameter_fidelity"
	QualityDimensionInstructionHierarchy QualityDimension = "instruction_hierarchy"
	QualityDimensionProtocolSchema       QualityDimension = "protocol_schema"
	QualityDimensionStreamCompleteness   QualityDimension = "stream_completeness"
)

type QualityConclusion string

const (
	QualityConclusionNoSignificantAnomaly QualityConclusion = "no_significant_anomaly"
	QualityConclusionObserve              QualityConclusion = "observe"
	QualityConclusionSuspected            QualityConclusion = "suspected"
	QualityConclusionHighRisk             QualityConclusion = "high_risk"
	QualityConclusionInsufficientCoverage QualityConclusion = "insufficient_coverage"
)

type QualitySourceState string

const (
	QualitySourceConfirmed            QualitySourceState = "confirmed"
	QualitySourceInferred             QualitySourceState = "inferred"
	QualitySourceInsufficientEvidence QualitySourceState = "insufficient_evidence"
)

type QualityEvidenceCode string

const (
	QualityEvidenceWithinPolicyBounds         QualityEvidenceCode = "within_policy_bounds"
	QualityEvidenceCoverageInsufficient       QualityEvidenceCode = "coverage_insufficient"
	QualityEvidenceFingerprintMatched         QualityEvidenceCode = "fingerprint_matched"
	QualityEvidenceFingerprintMismatch        QualityEvidenceCode = "fingerprint_mismatch"
	QualityEvidenceReasoningVariance          QualityEvidenceCode = "reasoning_variance"
	QualityEvidenceStructureViolation         QualityEvidenceCode = "structure_violation"
	QualityEvidenceParameterDeviation         QualityEvidenceCode = "parameter_deviation"
	QualityEvidenceInstructionViolation       QualityEvidenceCode = "instruction_violation"
	QualityEvidenceProtocolViolation          QualityEvidenceCode = "protocol_violation"
	QualityEvidenceStreamIncomplete           QualityEvidenceCode = "stream_incomplete"
	QualityEvidenceSourceConfirmed            QualityEvidenceCode = "source_confirmed"
	QualityEvidenceSourceInferred             QualityEvidenceCode = "source_inferred"
	QualityEvidenceSourceInsufficientEvidence QualityEvidenceCode = "source_insufficient_evidence"
)

var (
	ErrInvalidQualityReportRunID       = errors.New("quality report run ID is required")
	ErrInvalidQualityReportModelAlias  = errors.New("quality report model alias is required")
	ErrInvalidQualityPolicyVersion     = errors.New("quality report policy version is required")
	ErrInvalidQualityDimension         = errors.New("invalid quality dimension")
	ErrDuplicateQualityDimension       = errors.New("duplicate quality dimension")
	ErrMissingQualityDimension         = errors.New("missing quality dimension")
	ErrInvalidQualityScore             = errors.New("invalid quality score")
	ErrInvalidQualityConfidence        = errors.New("invalid quality confidence")
	ErrInvalidQualitySampleCount       = errors.New("invalid quality sample count")
	ErrInvalidQualityConclusion        = errors.New("invalid quality conclusion")
	ErrInvalidQualitySourceAttribution = errors.New("invalid quality source attribution")
	ErrInvalidQualityEvidenceSummary   = errors.New("invalid quality evidence summary")
	ErrInvalidQualityPolicy            = errors.New("invalid quality policy")
	ErrInvalidQualityProbeSpec         = errors.New("invalid quality probe spec")
	ErrInvalidQualityProbeObservation  = errors.New("invalid quality probe observation")
	ErrQualityReportAlreadyPublished   = errors.New("quality report already published")
	ErrQualityReportNotFound           = errors.New("quality report not found")
)

const defaultQualityPolicyJSON = `{"minimum_coverage":0.8,"minimum_confidence":0.7,"minimum_margin":0.15,"minimum_samples_per_dimension":3,"observe_delta_pp":5,"suspected_delta_pp":10,"high_risk_delta_pp":20,"freshness_hours":24}`

type QualityPolicy struct {
	MinimumCoverage            float64 `json:"minimum_coverage"`
	MinimumConfidence          float64 `json:"minimum_confidence"`
	MinimumMargin              float64 `json:"minimum_margin"`
	MinimumSamplesPerDimension int     `json:"minimum_samples_per_dimension"`
	ObserveDeltaPP             float64 `json:"observe_delta_pp"`
	SuspectedDeltaPP           float64 `json:"suspected_delta_pp"`
	HighRiskDeltaPP            float64 `json:"high_risk_delta_pp"`
	FreshnessHours             int     `json:"freshness_hours"`
}

func DefaultQualityPolicy() QualityPolicy {
	return QualityPolicy{
		MinimumCoverage:            0.8,
		MinimumConfidence:          0.7,
		MinimumMargin:              0.15,
		MinimumSamplesPerDimension: 3,
		ObserveDeltaPP:             5,
		SuspectedDeltaPP:           10,
		HighRiskDeltaPP:            20,
		FreshnessHours:             24,
	}
}

func DefaultQualityPolicyJSON() string {
	return defaultQualityPolicyJSON
}

func (policy QualityPolicy) Validate() error {
	if !validQualityFraction(policy.MinimumCoverage) ||
		!validQualityFraction(policy.MinimumConfidence) ||
		!validQualityFraction(policy.MinimumMargin) ||
		policy.MinimumMargin < 0.15 ||
		policy.MinimumSamplesPerDimension < 1 ||
		policy.FreshnessHours < 1 ||
		math.IsNaN(policy.ObserveDeltaPP) || math.IsInf(policy.ObserveDeltaPP, 0) || policy.ObserveDeltaPP < 0 ||
		math.IsNaN(policy.SuspectedDeltaPP) || math.IsInf(policy.SuspectedDeltaPP, 0) ||
		math.IsNaN(policy.HighRiskDeltaPP) || math.IsInf(policy.HighRiskDeltaPP, 0) ||
		policy.ObserveDeltaPP >= policy.SuspectedDeltaPP ||
		policy.SuspectedDeltaPP >= policy.HighRiskDeltaPP {
		return ErrInvalidQualityPolicy
	}
	return nil
}

type QualityProbeEventClass string

const (
	QualityProbeEventClassRequestShape    QualityProbeEventClass = "request_shape"
	QualityProbeEventClassResponseShape   QualityProbeEventClass = "response_shape"
	QualityProbeEventClassStreamIntegrity QualityProbeEventClass = "stream_integrity"
	QualityProbeEventClassParameterEcho   QualityProbeEventClass = "parameter_echo"
	QualityProbeEventClassFingerprint     QualityProbeEventClass = "fingerprint"
)

type QualityProbeSpec struct {
	SchemaVersion    string                 `json:"schema_version"`
	QualityDimension QualityDimension       `json:"quality_dimension"`
	EventClass       QualityProbeEventClass `json:"event_class"`
	MinimumSamples   int                    `json:"minimum_samples"`
	SourceCandidate  *SourceCandidate       `json:"source_candidate,omitempty"`
}

func (spec QualityProbeSpec) Validate() error {
	if spec.SchemaVersion != "quality-v1" || !isQualityDimension(spec.QualityDimension) ||
		!isQualityProbeEventClass(spec.EventClass) || spec.MinimumSamples < 1 {
		return ErrInvalidQualityProbeSpec
	}
	if spec.SourceCandidate != nil {
		if spec.QualityDimension != QualityDimensionModelFingerprint ||
			!validQualitySourceDisplayName(spec.SourceCandidate.DisplayName) ||
			!validQualityFraction(spec.SourceCandidate.Confidence) {
			return ErrInvalidQualityProbeSpec
		}
	}
	return nil
}

type QualityProbeObservation struct {
	ProbeSpecHash   string                 `json:"probe_spec_hash"`
	ObservationHash string                 `json:"observation_hash"`
	EventClass      QualityProbeEventClass `json:"event_class"`
	EventDigest     string                 `json:"event_digest"`
	ObservedAt      time.Time              `json:"observed_at"`
}

func (observation QualityProbeObservation) Validate() error {
	if !validQualityDigest(observation.ProbeSpecHash) ||
		!validQualityDigest(observation.ObservationHash) ||
		!isQualityProbeEventClass(observation.EventClass) ||
		!validQualityDigest(observation.EventDigest) ||
		observation.ObservedAt.IsZero() {
		return ErrInvalidQualityProbeObservation
	}
	return nil
}

type SourceCandidate struct {
	DisplayName string  `json:"display_name"`
	Confidence  float64 `json:"confidence"`
}

type QualityReportReader interface {
	ListPublicQualitySummaries(ctx context.Context) ([]PublicQualitySummary, error)
	GetPublicQualityReport(ctx context.Context, modelAlias string) (*PublicQualityReport, error)
}

type QualityDimensionResult struct {
	Key                      QualityDimension    `json:"key"`
	Score                    float64             `json:"score"`
	Status                   QualityConclusion   `json:"status"`
	SampleCount              int                 `json:"sample_count"`
	Confidence               float64             `json:"confidence"`
	StableBaselineDeltaPP    *float64            `json:"stable_baseline_delta_pp,omitempty"`
	ReferenceBaselineDeltaPP *float64            `json:"reference_baseline_delta_pp,omitempty"`
	CheckedAt                time.Time           `json:"checked_at"`
	EvidenceCode             QualityEvidenceCode `json:"evidence_code"`
}

type QualitySourceAttribution struct {
	State               QualitySourceState  `json:"state"`
	DisplayName         string              `json:"display_name,omitempty"`
	Confidence          *float64            `json:"confidence,omitempty"`
	Coverage            float64             `json:"coverage,omitempty"`
	AlternateCandidates []SourceCandidate   `json:"alternate_candidates,omitempty"`
	EvidenceCode        QualityEvidenceCode `json:"evidence_code"`
}

type QualitySourceAttributionPolicy struct {
	MinimumCoverage   float64 `json:"minimum_coverage"`
	MinimumConfidence float64 `json:"minimum_confidence"`
	MinimumMargin     float64 `json:"minimum_margin"`
}

type QualityReportPublication struct {
	RunID                   uuid.UUID                      `json:"run_id"`
	ModelAlias              string                         `json:"model_alias"`
	PolicyVersion           string                         `json:"policy_version"`
	OverallConclusion       QualityConclusion              `json:"overall_conclusion"`
	AdulterationRisk        QualityConclusion              `json:"adulteration_risk"`
	DegradationRisk         QualityConclusion              `json:"degradation_risk"`
	GeneratedAt             time.Time                      `json:"generated_at"`
	FreshUntil              time.Time                      `json:"fresh_until"`
	Dimensions              []QualityDimensionResult       `json:"dimension_results"`
	SourceAttribution       QualitySourceAttribution       `json:"source_attribution"`
	SourceAttributionPolicy QualitySourceAttributionPolicy `json:"source_attribution_policy"`
	ProbeObservations       []QualityProbeObservation      `json:"probe_observations,omitempty"`
}

type PublicQualitySummary struct {
	ModelAlias        string            `json:"model_alias"`
	OverallConclusion QualityConclusion `json:"overall_conclusion"`
	AdulterationRisk  QualityConclusion `json:"adulteration_risk"`
	DegradationRisk   QualityConclusion `json:"degradation_risk"`
	CheckedAt         time.Time         `json:"checked_at"`
	FreshUntil        time.Time         `json:"fresh_until"`
}

type PublicQualityEvidence struct {
	DimensionKey QualityDimension    `json:"dimension_key,omitempty"`
	Code         QualityEvidenceCode `json:"code"`
}

type PublicQualityReport struct {
	ModelAlias        string                   `json:"model_alias"`
	OverallConclusion QualityConclusion        `json:"overall_conclusion"`
	AdulterationRisk  QualityConclusion        `json:"adulteration_risk"`
	DegradationRisk   QualityConclusion        `json:"degradation_risk"`
	GeneratedAt       time.Time                `json:"generated_at"`
	FreshUntil        time.Time                `json:"fresh_until"`
	Dimensions        []QualityDimensionResult `json:"dimension_results"`
	SourceAttribution QualitySourceAttribution `json:"source_attribution"`
	Evidence          []PublicQualityEvidence  `json:"evidence"`
}

// Validate keeps the worker publication payload to aggregate and digest-safe data.
func (report QualityReportPublication) Validate() error {
	if report.RunID == uuid.Nil {
		return ErrInvalidQualityReportRunID
	}
	if strings.TrimSpace(report.ModelAlias) == "" {
		return ErrInvalidQualityReportModelAlias
	}
	if strings.TrimSpace(report.PolicyVersion) == "" {
		return ErrInvalidQualityPolicyVersion
	}
	for _, conclusion := range []QualityConclusion{
		report.OverallConclusion,
		report.AdulterationRisk,
		report.DegradationRisk,
	} {
		if !isQualityConclusion(conclusion) {
			return ErrInvalidQualityConclusion
		}
	}

	seenDimensions := make(map[QualityDimension]struct{}, len(report.Dimensions))
	for _, result := range report.Dimensions {
		if !isQualityDimension(result.Key) {
			return fmt.Errorf("%w: %q", ErrInvalidQualityDimension, result.Key)
		}
		if _, exists := seenDimensions[result.Key]; exists {
			return fmt.Errorf("%w: %q", ErrDuplicateQualityDimension, result.Key)
		}
		seenDimensions[result.Key] = struct{}{}
		if !validQualityFraction(result.Score) {
			return fmt.Errorf("%w: %q", ErrInvalidQualityScore, result.Key)
		}
		if !validQualityFraction(result.Confidence) {
			return fmt.Errorf("%w: %q", ErrInvalidQualityConfidence, result.Key)
		}
		if result.SampleCount < 0 {
			return fmt.Errorf("%w: %q", ErrInvalidQualitySampleCount, result.Key)
		}
		if !isQualityConclusion(result.Status) {
			return fmt.Errorf("%w: dimension %q", ErrInvalidQualityConclusion, result.Key)
		}
		if !isQualityEvidenceCode(result.EvidenceCode) {
			return fmt.Errorf("%w: dimension %q", ErrInvalidQualityEvidenceSummary, result.Key)
		}
	}
	for _, dimension := range requiredQualityDimensions {
		if _, exists := seenDimensions[dimension]; !exists {
			return fmt.Errorf("%w: %q", ErrMissingQualityDimension, dimension)
		}
	}
	if err := validateQualitySourceAttribution(report.SourceAttribution, report.SourceAttributionPolicy); err != nil {
		return err
	}
	for _, observation := range report.ProbeObservations {
		if err := observation.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ValidateQualityReportAgainstPolicy verifies that worker-derived quality
// conclusions agree with the tenant policy persisted for the publication.
func ValidateQualityReportAgainstPolicy(report QualityReportPublication, policy QualityPolicy) error {
	if err := report.Validate(); err != nil {
		return err
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	if report.SourceAttributionPolicy.MinimumCoverage != policy.MinimumCoverage ||
		report.SourceAttributionPolicy.MinimumConfidence != policy.MinimumConfidence ||
		report.SourceAttributionPolicy.MinimumMargin != policy.MinimumMargin {
		return ErrInvalidQualityPolicy
	}

	statuses := make(map[QualityDimension]QualityConclusion, len(report.Dimensions))
	for _, result := range report.Dimensions {
		if result.ReferenceBaselineDeltaPP == nil ||
			math.IsNaN(*result.ReferenceBaselineDeltaPP) || math.IsInf(*result.ReferenceBaselineDeltaPP, 0) {
			return ErrInvalidQualityPolicy
		}
		expectedStatus := qualityConclusionForPolicy(math.Abs(*result.ReferenceBaselineDeltaPP), result.SampleCount, policy)
		if result.Status != expectedStatus || result.EvidenceCode != qualityEvidenceForDimension(result.Key, expectedStatus) {
			return ErrInvalidQualityPolicy
		}
		if expectedStatus == QualityConclusionInsufficientCoverage {
			if result.Score != 0 || result.Confidence != 0 {
				return ErrInvalidQualityPolicy
			}
		} else if result.Confidence != 1 {
			return ErrInvalidQualityPolicy
		}
		statuses[result.Key] = expectedStatus
	}

	if !report.FreshUntil.Equal(report.GeneratedAt.Add(time.Duration(policy.FreshnessHours)*time.Hour)) ||
		report.AdulterationRisk != worstQualityConclusion(
			statuses[QualityDimensionModelFingerprint],
			statuses[QualityDimensionReasoningStability],
			statuses[QualityDimensionStructureCompliance],
		) ||
		report.DegradationRisk != worstQualityConclusion(
			statuses[QualityDimensionKnowledgeFreshness],
			statuses[QualityDimensionReasoningStability],
			statuses[QualityDimensionInstructionHierarchy],
		) ||
		report.OverallConclusion != overallQualityConclusion(report.SourceAttribution, statuses) {
		return ErrInvalidQualityPolicy
	}
	return nil
}

func qualityConclusionForPolicy(delta float64, sampleCount int, policy QualityPolicy) QualityConclusion {
	if sampleCount < policy.MinimumSamplesPerDimension {
		return QualityConclusionInsufficientCoverage
	}
	if delta < policy.ObserveDeltaPP {
		return QualityConclusionNoSignificantAnomaly
	}
	if delta < policy.SuspectedDeltaPP {
		return QualityConclusionObserve
	}
	if delta < policy.HighRiskDeltaPP {
		return QualityConclusionSuspected
	}
	return QualityConclusionHighRisk
}

func qualityEvidenceForDimension(dimension QualityDimension, conclusion QualityConclusion) QualityEvidenceCode {
	if conclusion == QualityConclusionInsufficientCoverage {
		return QualityEvidenceCoverageInsufficient
	}
	if conclusion == QualityConclusionNoSignificantAnomaly {
		if dimension == QualityDimensionModelFingerprint {
			return QualityEvidenceFingerprintMatched
		}
		return QualityEvidenceWithinPolicyBounds
	}
	switch dimension {
	case QualityDimensionModelFingerprint:
		return QualityEvidenceFingerprintMismatch
	case QualityDimensionReasoningStability:
		return QualityEvidenceReasoningVariance
	case QualityDimensionStructureCompliance:
		return QualityEvidenceStructureViolation
	case QualityDimensionParameterFidelity:
		return QualityEvidenceParameterDeviation
	case QualityDimensionInstructionHierarchy:
		return QualityEvidenceInstructionViolation
	case QualityDimensionProtocolSchema:
		return QualityEvidenceProtocolViolation
	case QualityDimensionStreamCompleteness:
		return QualityEvidenceStreamIncomplete
	default:
		return QualityEvidenceWithinPolicyBounds
	}
}

func worstQualityConclusion(conclusions ...QualityConclusion) QualityConclusion {
	for _, expected := range []QualityConclusion{
		QualityConclusionInsufficientCoverage,
		QualityConclusionHighRisk,
		QualityConclusionSuspected,
		QualityConclusionObserve,
	} {
		for _, conclusion := range conclusions {
			if conclusion == expected {
				return conclusion
			}
		}
	}
	return QualityConclusionNoSignificantAnomaly
}

func overallQualityConclusion(source QualitySourceAttribution, statuses map[QualityDimension]QualityConclusion) QualityConclusion {
	conclusions := make([]QualityConclusion, 0, len(statuses))
	highRiskCount := 0
	suspectedCount := 0
	hasObserve := false
	for _, conclusion := range statuses {
		conclusions = append(conclusions, conclusion)
		switch conclusion {
		case QualityConclusionHighRisk:
			highRiskCount++
		case QualityConclusionSuspected:
			suspectedCount++
		case QualityConclusionObserve:
			hasObserve = true
		}
	}
	if worstQualityConclusion(conclusions...) == QualityConclusionInsufficientCoverage {
		return QualityConclusionInsufficientCoverage
	}
	if (source.State == QualitySourceConfirmed && source.EvidenceCode == QualityEvidenceFingerprintMismatch) || highRiskCount >= 2 {
		return QualityConclusionHighRisk
	}
	if suspectedCount >= 2 || highRiskCount == 1 {
		return QualityConclusionSuspected
	}
	if hasObserve {
		return QualityConclusionObserve
	}
	return QualityConclusionNoSignificantAnomaly
}

var requiredQualityDimensions = []QualityDimension{
	QualityDimensionKnowledgeFreshness,
	QualityDimensionModelFingerprint,
	QualityDimensionReasoningStability,
	QualityDimensionStructureCompliance,
	QualityDimensionParameterFidelity,
	QualityDimensionInstructionHierarchy,
	QualityDimensionProtocolSchema,
	QualityDimensionStreamCompleteness,
}

func isQualityDimension(dimension QualityDimension) bool {
	for _, requiredDimension := range requiredQualityDimensions {
		if dimension == requiredDimension {
			return true
		}
	}
	return false
}

func isQualityConclusion(conclusion QualityConclusion) bool {
	switch conclusion {
	case QualityConclusionNoSignificantAnomaly,
		QualityConclusionObserve,
		QualityConclusionSuspected,
		QualityConclusionHighRisk,
		QualityConclusionInsufficientCoverage:
		return true
	default:
		return false
	}
}

func validQualityFraction(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

var qualitySourceDisplayNamePattern = regexp.MustCompile(`^[A-Za-z0-9 ._:/-]{1,200}$`)
var qualityDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func isQualityProbeEventClass(eventClass QualityProbeEventClass) bool {
	switch eventClass {
	case QualityProbeEventClassRequestShape,
		QualityProbeEventClassResponseShape,
		QualityProbeEventClassStreamIntegrity,
		QualityProbeEventClassParameterEcho,
		QualityProbeEventClassFingerprint:
		return true
	default:
		return false
	}
}

func validQualityDigest(value string) bool {
	return qualityDigestPattern.MatchString(value)
}

func validateQualitySourceAttribution(attribution QualitySourceAttribution, policy QualitySourceAttributionPolicy) error {
	if attribution.State != QualitySourceConfirmed && attribution.State != QualitySourceInferred && attribution.State != QualitySourceInsufficientEvidence {
		return ErrInvalidQualitySourceAttribution
	}
	if !isQualityEvidenceCode(attribution.EvidenceCode) {
		return ErrInvalidQualityEvidenceSummary
	}
	if attribution.Confidence != nil && !validQualityFraction(*attribution.Confidence) {
		return ErrInvalidQualityConfidence
	}
	if !validQualityFraction(attribution.Coverage) {
		return ErrInvalidQualitySourceAttribution
	}
	if attribution.State != QualitySourceInferred && len(attribution.AlternateCandidates) > 0 {
		return ErrInvalidQualitySourceAttribution
	}
	if attribution.State == QualitySourceConfirmed {
		if !validQualitySourceDisplayName(attribution.DisplayName) {
			return ErrInvalidQualitySourceAttribution
		}
		return nil
	}
	if attribution.State == QualitySourceInsufficientEvidence {
		if attribution.DisplayName != "" || attribution.Confidence != nil || attribution.Coverage != 0 {
			return ErrInvalidQualitySourceAttribution
		}
		return nil
	}
	if !validQualitySourceDisplayName(attribution.DisplayName) || attribution.Confidence == nil || len(attribution.AlternateCandidates) == 0 {
		return ErrInvalidQualitySourceAttribution
	}
	if !validQualityFraction(attribution.Coverage) || !validQualityFraction(policy.MinimumCoverage) || !validQualityFraction(policy.MinimumConfidence) || !validQualityFraction(policy.MinimumMargin) || policy.MinimumMargin < 0.15 {
		return ErrInvalidQualitySourceAttribution
	}
	if attribution.Coverage < policy.MinimumCoverage || *attribution.Confidence < policy.MinimumConfidence {
		return ErrInvalidQualitySourceAttribution
	}
	for _, candidate := range attribution.AlternateCandidates {
		if !validQualitySourceDisplayName(candidate.DisplayName) || !validQualityFraction(candidate.Confidence) {
			return ErrInvalidQualitySourceAttribution
		}
		if *attribution.Confidence-candidate.Confidence < policy.MinimumMargin {
			return ErrInvalidQualitySourceAttribution
		}
	}
	return nil
}

func validQualitySourceDisplayName(displayName string) bool {
	if !qualitySourceDisplayNamePattern.MatchString(displayName) {
		return false
	}
	for _, sensitiveToken := range []string{
		"prompt", "completion", "api key", "api_key", "sk-", "route", "account", "channel", "artifact",
	} {
		if strings.Contains(strings.ToLower(displayName), sensitiveToken) {
			return false
		}
	}
	return true
}

func isQualityEvidenceCode(code QualityEvidenceCode) bool {
	switch code {
	case QualityEvidenceWithinPolicyBounds,
		QualityEvidenceCoverageInsufficient,
		QualityEvidenceFingerprintMatched,
		QualityEvidenceFingerprintMismatch,
		QualityEvidenceReasoningVariance,
		QualityEvidenceStructureViolation,
		QualityEvidenceParameterDeviation,
		QualityEvidenceInstructionViolation,
		QualityEvidenceProtocolViolation,
		QualityEvidenceStreamIncomplete,
		QualityEvidenceSourceConfirmed,
		QualityEvidenceSourceInferred,
		QualityEvidenceSourceInsufficientEvidence:
		return true
	default:
		return false
	}
}
