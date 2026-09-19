package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestQualityReportPublicationValidateRejectsUnknownDimension(t *testing.T) {
	report := validQualityReportPublication()
	report.Dimensions[0].Key = QualityDimension("unknown_dimension")

	err := report.Validate()

	require.ErrorIs(t, err, ErrInvalidQualityDimension)
}

func TestQualityReportPublicationMarshalsInternalProbeObservations(t *testing.T) {
	report := validQualityReportPublication()
	report.ProbeObservations = []QualityProbeObservation{{
		ProbeSpecHash:   strings.Repeat("a", 64),
		ObservationHash: strings.Repeat("b", 64),
		EventClass:      QualityProbeEventClassResponseShape,
		EventDigest:     strings.Repeat("c", 64),
		ObservedAt:      time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC),
	}}

	encoded, err := json.Marshal(report)

	require.NoError(t, err)
	require.Contains(t, string(encoded), `"probe_observations"`)
	require.NotContains(t, string(encoded), "prompt")
	require.NotContains(t, string(encoded), "completion")
}

func TestQualityReportPublicationValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*QualityReportPublication)
		want   error
	}{
		{
			name: "requires a run ID",
			mutate: func(report *QualityReportPublication) {
				report.RunID = uuid.Nil
			},
			want: ErrInvalidQualityReportRunID,
		},
		{
			name: "rejects a duplicate dimension",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[1].Key = QualityDimensionKnowledgeFreshness
			},
			want: ErrDuplicateQualityDimension,
		},
		{
			name: "requires every dimension",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions = report.Dimensions[:7]
			},
			want: ErrMissingQualityDimension,
		},
		{
			name: "rejects an out of range score",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[0].Score = 1.01
			},
			want: ErrInvalidQualityScore,
		},
		{
			name: "rejects an out of range confidence",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[0].Confidence = -0.01
			},
			want: ErrInvalidQualityConfidence,
		},
		{
			name: "rejects a negative sample count",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[0].SampleCount = -1
			},
			want: ErrInvalidQualitySampleCount,
		},
		{
			name: "limits alternate candidates to inferred attribution",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution.AlternateCandidates = []SourceCandidate{{DisplayName: "Candidate", Confidence: 0.73}}
			},
			want: ErrInvalidQualitySourceAttribution,
		},
		{
			name: "requires an inferred source to meet the coverage threshold",
			mutate: func(report *QualityReportPublication) {
				confidence := 0.85
				report.SourceAttribution = QualitySourceAttribution{
					State:        QualitySourceInferred,
					DisplayName:  "Candidate A",
					Confidence:   &confidence,
					Coverage:     0.79,
					EvidenceCode: QualityEvidenceSourceInferred,
					AlternateCandidates: []SourceCandidate{{
						DisplayName: "Candidate B",
						Confidence:  0.65,
					}},
				}
				report.SourceAttributionPolicy = QualitySourceAttributionPolicy{
					MinimumCoverage:   0.80,
					MinimumConfidence: 0.70,
					MinimumMargin:     0.15,
				}
			},
			want: ErrInvalidQualitySourceAttribution,
		},
		{
			name: "requires an inferred source runner up to establish the margin",
			mutate: func(report *QualityReportPublication) {
				confidence := 0.85
				report.SourceAttribution = QualitySourceAttribution{
					State:        QualitySourceInferred,
					DisplayName:  "Candidate A",
					Confidence:   &confidence,
					Coverage:     0.90,
					EvidenceCode: QualityEvidenceSourceInferred,
				}
				report.SourceAttributionPolicy = QualitySourceAttributionPolicy{
					MinimumCoverage:   0.80,
					MinimumConfidence: 0.70,
					MinimumMargin:     0.15,
				}
			},
			want: ErrInvalidQualitySourceAttribution,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := validQualityReportPublication()
			test.mutate(&report)

			require.ErrorIs(t, report.Validate(), test.want)
		})
	}
}

func TestQualityReportPublicationValidateRejectsFreeFormEvidence(t *testing.T) {
	report := validQualityReportPublication()
	report.Dimensions[0].EvidenceCode = QualityEvidenceCode("the_upstream_answer_says_northstar")

	err := report.Validate()

	require.ErrorIs(t, err, ErrInvalidQualityEvidenceSummary)
}

func TestQualityReportPublicationValidateRejectsSensitiveSourceDisplayNames(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*QualityReportPublication)
	}{
		{
			name: "confirmed source display name contains a prompt",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution.DisplayName = "prompt: summarize this customer request"
			},
		},
		{
			name: "inferred candidate display name contains a completion",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution = validInferredQualitySourceAttribution()
				report.SourceAttributionPolicy = validQualitySourceAttributionPolicy()
				report.SourceAttribution.AlternateCandidates[0].DisplayName = "completion: internal output"
			},
		},
		{
			name: "inferred candidate display name contains an API key",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution = validInferredQualitySourceAttribution()
				report.SourceAttributionPolicy = validQualitySourceAttributionPolicy()
				report.SourceAttribution.AlternateCandidates[0].DisplayName = "sk-secret-token"
			},
		},
		{
			name: "inferred candidate display name contains a route reference",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution = validInferredQualitySourceAttribution()
				report.SourceAttributionPolicy = validQualitySourceAttributionPolicy()
				report.SourceAttribution.AlternateCandidates[0].DisplayName = "route: production/model-a"
			},
		},
		{
			name: "inferred candidate display name contains an account reference",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution = validInferredQualitySourceAttribution()
				report.SourceAttributionPolicy = validQualitySourceAttributionPolicy()
				report.SourceAttribution.AlternateCandidates[0].DisplayName = "account: 12345"
			},
		},
		{
			name: "inferred candidate display name contains a channel reference",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution = validInferredQualitySourceAttribution()
				report.SourceAttributionPolicy = validQualitySourceAttributionPolicy()
				report.SourceAttribution.AlternateCandidates[0].DisplayName = "channel: vendor-a"
			},
		},
		{
			name: "inferred candidate display name contains an artifact reference",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution = validInferredQualitySourceAttribution()
				report.SourceAttributionPolicy = validQualitySourceAttributionPolicy()
				report.SourceAttribution.AlternateCandidates[0].DisplayName = "artifact: sha256/abcdef"
			},
		},
		{
			name: "confirmed source display name exceeds the SQL length limit",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution.DisplayName = string(make([]byte, 201))
			},
		},
		{
			name: "inferred candidate display name has characters outside the SQL allowlist",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttribution = validInferredQualitySourceAttribution()
				report.SourceAttributionPolicy = validQualitySourceAttributionPolicy()
				report.SourceAttribution.AlternateCandidates[0].DisplayName = "Candidate B!"
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := validQualityReportPublication()
			test.mutate(&report)

			require.ErrorIs(t, report.Validate(), ErrInvalidQualitySourceAttribution)
		})
	}
}

func TestQualityReportPublicationValidateRejectsSourceFieldsForInsufficientEvidence(t *testing.T) {
	confidence := 0.8
	tests := []struct {
		name   string
		mutate func(*QualitySourceAttribution)
	}{
		{
			name: "display name",
			mutate: func(attribution *QualitySourceAttribution) {
				attribution.DisplayName = "Verified provider"
			},
		},
		{
			name: "confidence",
			mutate: func(attribution *QualitySourceAttribution) {
				attribution.Confidence = &confidence
			},
		},
		{
			name: "coverage",
			mutate: func(attribution *QualitySourceAttribution) {
				attribution.Coverage = 0.8
			},
		},
		{
			name: "alternate candidates",
			mutate: func(attribution *QualitySourceAttribution) {
				attribution.AlternateCandidates = []SourceCandidate{{DisplayName: "Candidate B", Confidence: 0.65}}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := validQualityReportPublication()
			report.SourceAttribution = QualitySourceAttribution{
				State:        QualitySourceInsufficientEvidence,
				EvidenceCode: QualityEvidenceSourceInsufficientEvidence,
			}
			test.mutate(&report.SourceAttribution)

			require.ErrorIs(t, report.Validate(), ErrInvalidQualitySourceAttribution)
		})
	}
}

func TestPublicQualityReportDoesNotContainSensitiveFields(t *testing.T) {
	report := PublicQualityReport{
		ModelAlias:        "gpt-5",
		OverallConclusion: QualityConclusionObserve,
		AdulterationRisk:  QualityConclusionNoSignificantAnomaly,
		DegradationRisk:   QualityConclusionObserve,
		GeneratedAt:       time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC),
		FreshUntil:        time.Date(2026, time.August, 12, 0, 0, 0, 0, time.UTC),
		Dimensions: []QualityDimensionResult{{
			Key:          QualityDimensionReasoningStability,
			Score:        0.82,
			Status:       QualityConclusionObserve,
			SampleCount:  24,
			Confidence:   0.94,
			CheckedAt:    time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC),
			EvidenceCode: QualityEvidenceWithinPolicyBounds,
		}},
		SourceAttribution: QualitySourceAttribution{
			State:        QualitySourceInsufficientEvidence,
			EvidenceCode: QualityEvidenceSourceInsufficientEvidence,
		},
		Evidence: []PublicQualityEvidence{{
			DimensionKey: QualityDimensionReasoningStability,
			Code:         QualityEvidenceReasoningVariance,
		}},
	}

	payload, err := json.Marshal(report)

	require.NoError(t, err)
	for _, sensitiveField := range []string{"route_trace_id", "prompt", "completion"} {
		require.NotContains(t, string(payload), sensitiveField)
	}
}

func TestQualityPolicyValidateRejectsMarginBelowMinimum(t *testing.T) {
	policy := DefaultQualityPolicy()
	policy.MinimumMargin = 0.14

	require.ErrorIs(t, policy.Validate(), ErrInvalidQualityPolicy)
}

func TestQualityProbeSpecValidateRejectsCandidateOutsideModelFingerprint(t *testing.T) {
	spec := QualityProbeSpec{
		SchemaVersion:    "quality-v1",
		QualityDimension: QualityDimensionReasoningStability,
		EventClass:       QualityProbeEventClassFingerprint,
		MinimumSamples:   1,
		SourceCandidate:  &SourceCandidate{DisplayName: "Candidate A", Confidence: 0.9},
	}

	require.ErrorIs(t, spec.Validate(), ErrInvalidQualityProbeSpec)
}

func TestQualityProbeObservationValidateRejectsInvalidDigest(t *testing.T) {
	observation := QualityProbeObservation{
		ProbeSpecHash:   strings.Repeat("a", 64),
		ObservationHash: strings.Repeat("b", 64),
		EventClass:      QualityProbeEventClassFingerprint,
		EventDigest:     "not-a-sha256-digest",
		ObservedAt:      time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC),
	}

	require.ErrorIs(t, observation.Validate(), ErrInvalidQualityProbeObservation)
}

func TestValidateQualityReportAgainstPolicyAcceptsWorkerDerivedReport(t *testing.T) {
	report := validQualityReportForPolicyValidation()

	require.NoError(t, ValidateQualityReportAgainstPolicy(report, DefaultQualityPolicy()))
}

func TestValidateQualityReportAgainstPolicyRejectsForgedDimensionStatus(t *testing.T) {
	report := validQualityReportForPolicyValidation()
	report.Dimensions[0].ReferenceBaselineDeltaPP = qualityFloatPointer(20)
	report.Dimensions[0].Status = QualityConclusionNoSignificantAnomaly

	require.ErrorIs(t, ValidateQualityReportAgainstPolicy(report, DefaultQualityPolicy()), ErrInvalidQualityPolicy)
}

func TestValidateQualityReportAgainstPolicyRejectsWrongFreshness(t *testing.T) {
	report := validQualityReportForPolicyValidation()
	report.FreshUntil = report.GeneratedAt.Add(23 * time.Hour)

	require.ErrorIs(t, ValidateQualityReportAgainstPolicy(report, DefaultQualityPolicy()), ErrInvalidQualityPolicy)
}

func TestValidateQualityReportAgainstPolicyAcceptsDimensionThresholdBoundaries(t *testing.T) {
	tests := []struct {
		name            string
		delta           float64
		sampleCount     int
		status          QualityConclusion
		confidence      float64
		score           float64
		overall         QualityConclusion
		degradationRisk QualityConclusion
		evidenceCode    QualityEvidenceCode
	}{
		{
			name: "observe threshold", delta: 5, sampleCount: 3,
			status: QualityConclusionObserve, confidence: 1, score: 0.8,
			overall: QualityConclusionObserve, degradationRisk: QualityConclusionObserve,
			evidenceCode: QualityEvidenceWithinPolicyBounds,
		},
		{
			name: "suspected threshold", delta: 10, sampleCount: 3,
			status: QualityConclusionSuspected, confidence: 1, score: 0.8,
			overall: QualityConclusionNoSignificantAnomaly, degradationRisk: QualityConclusionSuspected,
			evidenceCode: QualityEvidenceWithinPolicyBounds,
		},
		{
			name: "high risk threshold", delta: 20, sampleCount: 3,
			status: QualityConclusionHighRisk, confidence: 1, score: 0.8,
			overall: QualityConclusionSuspected, degradationRisk: QualityConclusionHighRisk,
			evidenceCode: QualityEvidenceWithinPolicyBounds,
		},
		{
			name: "insufficient coverage", delta: 20, sampleCount: 2,
			status: QualityConclusionInsufficientCoverage, confidence: 0, score: 0,
			overall: QualityConclusionInsufficientCoverage, degradationRisk: QualityConclusionInsufficientCoverage,
			evidenceCode: QualityEvidenceCoverageInsufficient,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := validQualityReportForPolicyValidation()
			result := &report.Dimensions[0]
			result.ReferenceBaselineDeltaPP = qualityFloatPointer(test.delta)
			result.SampleCount = test.sampleCount
			result.Status = test.status
			result.Confidence = test.confidence
			result.Score = test.score
			result.EvidenceCode = test.evidenceCode
			report.OverallConclusion = test.overall
			report.DegradationRisk = test.degradationRisk

			require.NoError(t, ValidateQualityReportAgainstPolicy(report, DefaultQualityPolicy()))
		})
	}
}

func TestValidateQualityReportAgainstPolicyRejectsDerivedFieldMismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*QualityReportPublication)
	}{
		{
			name: "missing reference baseline delta",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[0].ReferenceBaselineDeltaPP = nil
			},
		},
		{
			name: "forged healthy fingerprint evidence",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[1].EvidenceCode = QualityEvidenceWithinPolicyBounds
			},
		},
		{
			name: "non-worker confidence",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[0].Confidence = 0.9
			},
		},
		{
			name: "non-zero insufficient coverage score",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[0].SampleCount = 2
				report.Dimensions[0].Status = QualityConclusionInsufficientCoverage
				report.Dimensions[0].Confidence = 0
				report.Dimensions[0].EvidenceCode = QualityEvidenceCoverageInsufficient
			},
		},
		{
			name: "non-zero insufficient coverage confidence",
			mutate: func(report *QualityReportPublication) {
				report.Dimensions[0].SampleCount = 2
				report.Dimensions[0].Status = QualityConclusionInsufficientCoverage
				report.Dimensions[0].Score = 0
				report.Dimensions[0].Confidence = 1
				report.Dimensions[0].EvidenceCode = QualityEvidenceCoverageInsufficient
			},
		},
		{
			name: "source attribution policy threshold",
			mutate: func(report *QualityReportPublication) {
				report.SourceAttributionPolicy.MinimumMargin = 0.2
			},
		},
		{
			name: "forged aggregate conclusion",
			mutate: func(report *QualityReportPublication) {
				report.OverallConclusion = QualityConclusionObserve
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := validQualityReportForPolicyValidation()
			test.mutate(&report)

			require.ErrorIs(t, ValidateQualityReportAgainstPolicy(report, DefaultQualityPolicy()), ErrInvalidQualityPolicy)
		})
	}
}

func validQualityReportPublication() QualityReportPublication {
	checkedAt := time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)
	dimensions := []QualityDimension{
		QualityDimensionKnowledgeFreshness,
		QualityDimensionModelFingerprint,
		QualityDimensionReasoningStability,
		QualityDimensionStructureCompliance,
		QualityDimensionParameterFidelity,
		QualityDimensionInstructionHierarchy,
		QualityDimensionProtocolSchema,
		QualityDimensionStreamCompleteness,
	}
	results := make([]QualityDimensionResult, 0, len(dimensions))
	for _, dimension := range dimensions {
		results = append(results, QualityDimensionResult{
			Key:          dimension,
			Score:        0.8,
			Status:       QualityConclusionNoSignificantAnomaly,
			SampleCount:  12,
			Confidence:   0.9,
			CheckedAt:    checkedAt,
			EvidenceCode: QualityEvidenceWithinPolicyBounds,
		})
	}
	return QualityReportPublication{
		RunID:             uuid.New(),
		ModelAlias:        "gpt-5",
		PolicyVersion:     "quality-v1",
		OverallConclusion: QualityConclusionNoSignificantAnomaly,
		AdulterationRisk:  QualityConclusionNoSignificantAnomaly,
		DegradationRisk:   QualityConclusionNoSignificantAnomaly,
		GeneratedAt:       checkedAt,
		FreshUntil:        checkedAt.Add(24 * time.Hour),
		Dimensions:        results,
		SourceAttribution: QualitySourceAttribution{
			State:        QualitySourceConfirmed,
			DisplayName:  "Verified provider",
			EvidenceCode: QualityEvidenceSourceConfirmed,
		},
	}
}

func validQualityReportForPolicyValidation() QualityReportPublication {
	report := validQualityReportPublication()
	report.SourceAttributionPolicy = validQualitySourceAttributionPolicy()
	for index := range report.Dimensions {
		result := &report.Dimensions[index]
		result.Confidence = 1
		result.ReferenceBaselineDeltaPP = qualityFloatPointer(0)
		if result.Key == QualityDimensionModelFingerprint {
			result.EvidenceCode = QualityEvidenceFingerprintMatched
		}
	}
	return report
}

func qualityFloatPointer(value float64) *float64 {
	return &value
}

func validInferredQualitySourceAttribution() QualitySourceAttribution {
	confidence := 0.85
	return QualitySourceAttribution{
		State:        QualitySourceInferred,
		DisplayName:  "Candidate A",
		Confidence:   &confidence,
		Coverage:     0.9,
		EvidenceCode: QualityEvidenceSourceInferred,
		AlternateCandidates: []SourceCandidate{{
			DisplayName: "Candidate B",
			Confidence:  0.65,
		}},
	}
}

func validQualitySourceAttributionPolicy() QualitySourceAttributionPolicy {
	return QualitySourceAttributionPolicy{
		MinimumCoverage:   0.8,
		MinimumConfidence: 0.7,
		MinimumMargin:     0.15,
	}
}
