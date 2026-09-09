package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestEmitFrozenQualityContextJSONForWorkerContract(t *testing.T) {
	timestamp := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	inputs := make([]frozenQualityAnalysisInput, 0, len(qualityDimensions)+1)
	for _, dimension := range qualityDimensions {
		probeSpec := service.QualityProbeSpec{
			SchemaVersion: "quality-v1", QualityDimension: dimension,
			EventClass: service.QualityProbeEventClassResponseShape, MinimumSamples: 1,
		}
		if dimension == service.QualityDimensionModelFingerprint {
			probeSpec.EventClass = service.QualityProbeEventClassFingerprint
			probeSpec.SourceCandidate = &service.SourceCandidate{DisplayName: "Candidate A", Confidence: 0.90}
		}
		inputs = append(inputs, frozenQualityAnalysisInput{
			Dimension: dimension, CaseID: uuid.New(), SampleIndex: 0,
			BaselineScore: decimal.RequireFromString("0.80"), CandidateScore: decimal.RequireFromString("0.70"),
			BaselineScoreID: uuid.New(), CandidateScoreID: uuid.New(),
			BaselineCreated: timestamp, CandidateCreated: timestamp, ContentSHA256: "a" + strings.Repeat("a", 63),
			ProbeSpec: probeSpec,
		})
	}
	inputs = append(inputs, frozenQualityAnalysisInput{
		Dimension: service.QualityDimensionModelFingerprint, CaseID: uuid.New(), SampleIndex: 1,
		BaselineScore: decimal.RequireFromString("0.90"), CandidateScore: decimal.RequireFromString("0.85"),
		BaselineScoreID: uuid.New(), CandidateScoreID: uuid.New(),
		BaselineCreated: timestamp.Add(time.Minute), CandidateCreated: timestamp.Add(time.Minute),
		ContentSHA256: "b" + strings.Repeat("b", 63),
		ProbeSpec: service.QualityProbeSpec{
			SchemaVersion: "quality-v1", QualityDimension: service.QualityDimensionModelFingerprint,
			EventClass: service.QualityProbeEventClassFingerprint, MinimumSamples: 1,
			SourceCandidate: &service.SourceCandidate{DisplayName: "Candidate B", Confidence: 0.70},
		},
	})
	for sampleIndex, candidate := range []service.SourceCandidate{
		{DisplayName: "Candidate A", Confidence: 0.90},
		{DisplayName: "Candidate B", Confidence: 0.70},
		{DisplayName: "Candidate A", Confidence: 0.90},
		{DisplayName: "Candidate B", Confidence: 0.70},
	} {
		inputs = append(inputs, frozenQualityAnalysisInput{
			Dimension: service.QualityDimensionModelFingerprint, CaseID: uuid.New(), SampleIndex: sampleIndex + 2,
			BaselineScore: decimal.RequireFromString("0.80"), CandidateScore: decimal.RequireFromString("0.75"),
			BaselineScoreID: uuid.New(), CandidateScoreID: uuid.New(),
			BaselineCreated:  timestamp.Add(time.Duration(sampleIndex+2) * time.Minute),
			CandidateCreated: timestamp.Add(time.Duration(sampleIndex+2) * time.Minute),
			ContentSHA256:    strings.Repeat(string('c'+rune(sampleIndex)), 64),
			ProbeSpec: service.QualityProbeSpec{
				SchemaVersion: "quality-v1", QualityDimension: service.QualityDimensionModelFingerprint,
				EventClass: service.QualityProbeEventClassFingerprint, MinimumSamples: 1,
				SourceCandidate: &candidate,
			},
		})
	}
	context := buildFrozenQualityAnalysisContext(uuid.New(), "model-a", service.DefaultQualityPolicy(), inputs)
	require.NotNil(t, context)
	encoded, err := json.Marshal(context)
	require.NoError(t, err)
	fmt.Printf("QUALITY_CONTEXT_JSON=%s\n", encoded)
}

func TestScoreSourceUsesTypedRouteEvidenceRefs(t *testing.T) {
	source := service.ScoreSource{
		RouteEvidenceRefs: []service.RouteEvidenceRef{{
			RouteTraceID: "trace-1", RequestOrdinal: 2, PayloadHash: "abc123",
		}},
	}

	encoded, err := json.Marshal(source)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"assignment_id":"00000000-0000-0000-0000-000000000000",
		"route_evidence_set_hash":"",
		"route_evidence_refs":[{
			"route_trace_id":"trace-1",
			"request_ordinal":2,
			"payload_hash":"abc123"
		}],
		"artifact_manifest_hash":""
	}`, string(encoded))
}

func TestClaimAnalysisQualityContextIncludesCompleteFrozenTenantInputs(t *testing.T) {
	timestamp := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	inputs := make([]frozenQualityAnalysisInput, 0, len(qualityDimensions))
	for ordinal, dimension := range qualityDimensions {
		inputs = append(inputs, frozenQualityAnalysisInput{
			Dimension:        dimension,
			CaseID:           uuid.New(),
			SampleIndex:      0,
			BaselineScore:    decimal.RequireFromString("0.80"),
			CandidateScore:   decimal.RequireFromString("0.70"),
			BaselineScoreID:  uuid.New(),
			CandidateScoreID: uuid.New(),
			BaselineCreated:  timestamp.Add(time.Duration(ordinal) * time.Minute),
			CandidateCreated: timestamp.Add(time.Duration(ordinal+1) * time.Minute),
			ContentSHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ProbeSpec: service.QualityProbeSpec{
				SchemaVersion:    "quality-v1",
				QualityDimension: dimension,
				EventClass:       service.QualityProbeEventClassResponseShape,
				MinimumSamples:   1,
			},
		})
	}

	context := buildFrozenQualityAnalysisContext(uuid.New(), "model-a", service.DefaultQualityPolicy(), inputs)
	require.NotNil(t, context)
	require.Equal(t, "quality-v1", context.PolicyVersion)
	require.Equal(t, service.DefaultQualityPolicy(), context.Policy)
	require.Len(t, context.Dimensions, 8)

	encoded, err := json.Marshal(context)
	require.NoError(t, err)
	for _, forbidden := range []string{"prompt", "completion", "route_trace_id", "artifact", "account", "channel"} {
		require.NotContains(t, string(encoded), forbidden)
	}
}

func TestClaimAnalysisQualityContextWithIncompleteFrozenInputsIsNil(t *testing.T) {
	timestamp := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	inputs := make([]frozenQualityAnalysisInput, 0, 7)
	for _, dimension := range qualityDimensions[:7] {
		inputs = append(inputs, frozenQualityAnalysisInput{
			Dimension: dimension, CaseID: uuid.New(), BaselineScore: decimal.RequireFromString("0.80"),
			CandidateScore: decimal.RequireFromString("0.70"), BaselineScoreID: uuid.New(), CandidateScoreID: uuid.New(),
			BaselineCreated: timestamp, CandidateCreated: timestamp, ContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ProbeSpec: service.QualityProbeSpec{SchemaVersion: "quality-v1", QualityDimension: dimension, EventClass: service.QualityProbeEventClassResponseShape, MinimumSamples: 1},
		})
	}

	require.Nil(t, buildFrozenQualityAnalysisContext(uuid.New(), "model-a", service.DefaultQualityPolicy(), inputs))
}

func TestClaimAnalysisQualityContextWithMixedDimensionProbeSpecsIsNil(t *testing.T) {
	timestamp := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	inputs := make([]frozenQualityAnalysisInput, 0, len(qualityDimensions)+1)
	for _, dimension := range qualityDimensions {
		inputs = append(inputs, frozenQualityAnalysisInput{
			Dimension: dimension, CaseID: uuid.New(), BaselineScore: decimal.RequireFromString("0.80"),
			CandidateScore: decimal.RequireFromString("0.70"), BaselineScoreID: uuid.New(), CandidateScoreID: uuid.New(),
			BaselineCreated: timestamp, CandidateCreated: timestamp, ContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ProbeSpec: service.QualityProbeSpec{SchemaVersion: "quality-v1", QualityDimension: dimension, EventClass: service.QualityProbeEventClassResponseShape, MinimumSamples: 1},
		})
	}
	inputs = append(inputs, frozenQualityAnalysisInput{
		Dimension: service.QualityDimensionKnowledgeFreshness, CaseID: uuid.New(), BaselineScore: decimal.RequireFromString("0.80"),
		CandidateScore: decimal.RequireFromString("0.70"), BaselineScoreID: uuid.New(), CandidateScoreID: uuid.New(),
		BaselineCreated: timestamp, CandidateCreated: timestamp, ContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProbeSpec: service.QualityProbeSpec{SchemaVersion: "quality-v1", QualityDimension: service.QualityDimensionKnowledgeFreshness, EventClass: service.QualityProbeEventClassStreamIntegrity, MinimumSamples: 1},
	})

	require.Nil(t, buildFrozenQualityAnalysisContext(uuid.New(), "model-a", service.DefaultQualityPolicy(), inputs))
}

func TestClaimAnalysisQualityContextAllowsTwoControlledFingerprintSourceCandidates(t *testing.T) {
	timestamp := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	inputs := make([]frozenQualityAnalysisInput, 0, len(qualityDimensions)+1)
	for _, dimension := range qualityDimensions {
		probeSpec := service.QualityProbeSpec{
			SchemaVersion: "quality-v1", QualityDimension: dimension,
			EventClass: service.QualityProbeEventClassResponseShape, MinimumSamples: 1,
		}
		if dimension == service.QualityDimensionModelFingerprint {
			probeSpec.EventClass = service.QualityProbeEventClassFingerprint
			probeSpec.SourceCandidate = &service.SourceCandidate{DisplayName: "Candidate A", Confidence: 0.90}
		}
		inputs = append(inputs, frozenQualityAnalysisInput{
			Dimension: dimension, CaseID: uuid.New(), SampleIndex: 0,
			BaselineScore: decimal.RequireFromString("0.80"), CandidateScore: decimal.RequireFromString("0.70"),
			BaselineScoreID: uuid.New(), CandidateScoreID: uuid.New(),
			BaselineCreated: timestamp, CandidateCreated: timestamp, ContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ProbeSpec: probeSpec,
		})
	}
	inputs = append(inputs, frozenQualityAnalysisInput{
		Dimension: service.QualityDimensionModelFingerprint, CaseID: uuid.New(), SampleIndex: 1,
		BaselineScore: decimal.RequireFromString("0.90"), CandidateScore: decimal.RequireFromString("0.85"),
		BaselineScoreID: uuid.New(), CandidateScoreID: uuid.New(),
		BaselineCreated: timestamp.Add(time.Minute), CandidateCreated: timestamp.Add(time.Minute),
		ContentSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ProbeSpec: service.QualityProbeSpec{
			SchemaVersion: "quality-v1", QualityDimension: service.QualityDimensionModelFingerprint,
			EventClass: service.QualityProbeEventClassFingerprint, MinimumSamples: 1,
			SourceCandidate: &service.SourceCandidate{DisplayName: "Candidate B", Confidence: 0.70},
		},
	})

	context := buildFrozenQualityAnalysisContext(uuid.New(), "model-a", service.DefaultQualityPolicy(), inputs)
	require.NotNil(t, context)
	encoded, err := json.Marshal(context)
	require.NoError(t, err)
	var payload struct {
		SourceCandidates []map[string]any `json:"source_candidates"`
	}
	require.NoError(t, json.Unmarshal(encoded, &payload))
	require.Len(t, payload.SourceCandidates, 2)
	for _, candidate := range payload.SourceCandidates {
		require.Equal(t, float64(1), candidate["sample_count"])
		require.NotEmpty(t, candidate["probe_spec_hash"])
		require.NotEmpty(t, candidate["observation_hash"])
		require.Contains(t, candidate, "baseline_score")
		require.Contains(t, candidate, "candidate_score")
	}
	require.NotContains(t, string(encoded), "prompt")
	require.NotContains(t, string(encoded), "completion")
}

func TestLoadFrozenQualityAnalysisContextProjectsCompleteTenantInputs(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	jobID := uuid.New()
	runID := uuid.New()
	checkedAt := time.Date(2026, time.August, 11, 12, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT policy.policy.*quality_policy_versions policy").
		WithArgs(runID).
		WillReturnRows(sqlmock.NewRows([]string{"policy"}).AddRow([]byte(service.DefaultQualityPolicyJSON())))
	rows := sqlmock.NewRows([]string{
		"quality_dimension", "case_id", "sample_index", "baseline_score", "candidate_score",
		"baseline_score_id", "candidate_score_id", "baseline_created_at", "candidate_created_at",
		"content_sha256", "quality_probe_spec",
	})
	for ordinal, dimension := range qualityDimensions {
		probeSpecInput := service.QualityProbeSpec{
			SchemaVersion:    "quality-v1",
			QualityDimension: dimension,
			EventClass:       service.QualityProbeEventClassResponseShape,
			MinimumSamples:   1,
		}
		if dimension == service.QualityDimensionModelFingerprint {
			probeSpecInput.EventClass = service.QualityProbeEventClassFingerprint
			probeSpecInput.SourceCandidate = &service.SourceCandidate{DisplayName: "Candidate A", Confidence: 0.90}
		}
		probeSpec, marshalErr := json.Marshal(probeSpecInput)
		require.NoError(t, marshalErr)
		rows.AddRow(
			string(dimension), uuid.New(), ordinal,
			"0.80", "0.70", uuid.New(), uuid.New(), checkedAt, checkedAt,
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", probeSpec,
		)
	}
	secondCandidateSpec, marshalErr := json.Marshal(service.QualityProbeSpec{
		SchemaVersion: "quality-v1", QualityDimension: service.QualityDimensionModelFingerprint,
		EventClass: service.QualityProbeEventClassFingerprint, MinimumSamples: 1,
		SourceCandidate: &service.SourceCandidate{DisplayName: "Candidate B", Confidence: 0.70},
	})
	require.NoError(t, marshalErr)
	rows.AddRow(
		string(service.QualityDimensionModelFingerprint), uuid.New(), len(qualityDimensions),
		"0.90", "0.85", uuid.New(), uuid.New(), checkedAt, checkedAt,
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", secondCandidateSpec,
	)
	mock.ExpectQuery("SELECT c.quality_dimension.*evaluation_analysis_job_score_inputs baseline_input").
		WithArgs(jobID, runID, "model-a").
		WillReturnRows(rows)
	mock.ExpectRollback()

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	context := loadFrozenQualityAnalysisContext(context.Background(), tx, jobID, runID, "candidate:model-a")
	require.NotNil(t, context)
	require.Equal(t, "model-a", context.ModelAlias)
	require.Equal(t, "quality-v1", context.PolicyVersion)
	require.Len(t, context.Dimensions, len(qualityDimensions))
	require.Len(t, context.SourceCandidates, 2)
	require.Equal(t, "Candidate A", context.SourceCandidates[0].DisplayName)
	require.Equal(t, "Candidate B", context.SourceCandidates[1].DisplayName)
	require.Equal(t, 1, context.SourceCandidates[0].SampleCount)
	require.Len(t, context.SourceCandidates[0].ProbeSpecHash, 64)
	require.Len(t, context.SourceCandidates[0].ObservationHash, 64)
	require.NoError(t, tx.Rollback())
	require.NoError(t, mock.ExpectationsWereMet())
}
