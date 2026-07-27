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
