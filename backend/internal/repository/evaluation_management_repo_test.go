package repository

import (
	"encoding/json"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
)

func radarDatasetHashFixture() service.CreateRadarDatasetInput {
	return service.CreateRadarDatasetInput{
		DatasetKey: "reasoning-smoke",
		Version:    "v1",
		SourceType: "synthetic",
		CreatedBy:  1,
		Cases: []service.CreateRadarCaseInput{{
			CaseKey:          "case-1",
			CapabilityDomain: "reasoning",
			Priority:         "P1",
			Weight:           decimal.RequireFromString("1"),
			SampleCount:      1,
			PromptSpec:       json.RawMessage(`{"input":"ping"}`),
			ExpectedSpec:     json.RawMessage(`"pong"`),
			ExecutionSpec:    json.RawMessage(`{"url":"/v1/responses"}`),
			GraderID:         "exact",
			GraderVersion:    "v1",
			Confidentiality:  "synthetic",
			EstimatedCost:    decimal.RequireFromString("0.01"),
		}},
	}
}

func qualityCaseFixture() service.CreateRadarDatasetInput {
	input := radarDatasetHashFixture()
	dimension := service.QualityDimensionModelFingerprint
	input.Cases[0].QualityDimension = &dimension
	input.Cases[0].QualityProbeSpec = &service.QualityProbeSpec{
		SchemaVersion:    "quality-v1",
		QualityDimension: dimension,
		EventClass:       service.QualityProbeEventClassFingerprint,
		MinimumSamples:   3,
		SourceCandidate: &service.SourceCandidate{
			DisplayName: "Radar Synthetic Reference",
			Confidence:  0.9,
		},
	}
	input.Cases[0].SampleCount = 3
	return input
}

func TestRadarCaseLegacyHashesRemainStable(t *testing.T) {
	rows, manifest, err := radarCaseRows(radarDatasetHashFixture())
	if err != nil {
		t.Fatalf("radarCaseRows() error = %v", err)
	}
	if got, want := rows[0].contentSHA256, "910ebc5d64995be8bbcbf139b82b527407aa327984f87a473d9dded1872619fd"; got != want {
		t.Errorf("content hash = %q, want %q", got, want)
	}
	if got, want := manifest, "1d4ee067c08225ee34f43cf95564cb8b68d6f1029eb644ed0e47fb3e4cdef86e"; got != want {
		t.Errorf("manifest hash = %q, want %q", got, want)
	}
}

func TestRadarCaseQualityMetadataChangesHashes(t *testing.T) {
	baseRows, baseManifest, err := radarCaseRows(qualityCaseFixture())
	if err != nil {
		t.Fatalf("radarCaseRows(quality case) error = %v", err)
	}
	if got, want := string(baseRows[0].qualityProbe), `{"schema_version":"quality-v1","quality_dimension":"model_fingerprint","event_class":"fingerprint","minimum_samples":3,"source_candidate":{"display_name":"Radar Synthetic Reference","confidence":0.9}}`; got != want {
		t.Errorf("canonical quality probe = %s, want %s", got, want)
	}

	mutations := map[string]func(*service.CreateRadarCaseInput){
		"dimension": func(item *service.CreateRadarCaseInput) {
			dimension := service.QualityDimensionReasoningStability
			item.QualityDimension = &dimension
			item.QualityProbeSpec.QualityDimension = dimension
			item.QualityProbeSpec.EventClass = service.QualityProbeEventClassResponseShape
			item.QualityProbeSpec.SourceCandidate = nil
		},
		"probe": func(item *service.CreateRadarCaseInput) {
			item.QualityProbeSpec.MinimumSamples = 4
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			input := qualityCaseFixture()
			mutate(&input.Cases[0])
			rows, manifest, err := radarCaseRows(input)
			if err != nil {
				t.Fatalf("radarCaseRows(%s) error = %v", name, err)
			}
			if rows[0].contentSHA256 == baseRows[0].contentSHA256 {
				t.Errorf("content hash did not change after %s mutation", name)
			}
			if manifest == baseManifest {
				t.Errorf("manifest hash did not change after %s mutation", name)
			}
		})
	}
}

func TestRadarCaseQualityMetadataValidation(t *testing.T) {
	invalidCases := map[string]func(*service.CreateRadarCaseInput){
		"probe_without_dimension": func(item *service.CreateRadarCaseInput) {
			item.QualityDimension = nil
		},
		"mismatched_dimension": func(item *service.CreateRadarCaseInput) {
			dimension := service.QualityDimensionReasoningStability
			item.QualityDimension = &dimension
		},
		"source_candidate_outside_model_fingerprint": func(item *service.CreateRadarCaseInput) {
			dimension := service.QualityDimensionReasoningStability
			item.QualityDimension = &dimension
			item.QualityProbeSpec.QualityDimension = dimension
			item.QualityProbeSpec.EventClass = service.QualityProbeEventClassResponseShape
		},
	}
	for name, mutate := range invalidCases {
		t.Run(name, func(t *testing.T) {
			input := qualityCaseFixture()
			mutate(&input.Cases[0])
			if _, _, err := radarCaseRows(input); err == nil {
				t.Fatal("radarCaseRows() error = nil, want invalid quality metadata rejection")
			}
		})
	}
}

func TestRadarCaseQualityMetadataAllowsCoverageBelowMinimum(t *testing.T) {
	input := qualityCaseFixture()
	input.Cases[0].SampleCount = 2
	if _, _, err := radarCaseRows(input); err != nil {
		t.Fatalf("radarCaseRows() error = %v, want coverage-below-minimum fixture to remain valid", err)
	}
}

func TestRadarCaseHashIncludesExecutionGradingCostAndAccessSemantics(t *testing.T) {
	base := radarDatasetHashFixture()
	baseRows, baseManifest, err := radarCaseRows(base)
	if err != nil {
		t.Fatalf("radarCaseRows(base) error = %v", err)
	}

	mutations := map[string]func(*service.CreateRadarCaseInput){
		"weight":          func(item *service.CreateRadarCaseInput) { item.Weight = decimal.RequireFromString("2") },
		"sample_count":    func(item *service.CreateRadarCaseInput) { item.SampleCount = 2 },
		"confidentiality": func(item *service.CreateRadarCaseInput) { item.Confidentiality = "public" },
		"estimated_cost":  func(item *service.CreateRadarCaseInput) { item.EstimatedCost = decimal.RequireFromString("0.02") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			input := radarDatasetHashFixture()
			mutate(&input.Cases[0])
			rows, manifest, err := radarCaseRows(input)
			if err != nil {
				t.Fatalf("radarCaseRows(%s) error = %v", name, err)
			}
			if rows[0].contentSHA256 == baseRows[0].contentSHA256 {
				t.Errorf("content hash did not change after %s mutation", name)
			}
			if manifest == baseManifest {
				t.Errorf("manifest hash did not change after %s mutation", name)
			}
		})
	}
}
