package repository

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/shopspring/decimal"
)

func TestEvaluationMatrixEntriesPreservesNumericLexemes(t *testing.T) {
	entries, err := evaluationMatrixEntries([]byte(`[{
		"route":"route-a",
		"baseline":{"route":"model-v1","seed":9007199254740993,"temperature":0.12345678901234567890123456789},
		"candidate":{"route":"model-v2","seed":9007199254740993,"temperature":0.12345678901234567890123456789}
	}]`))
	if err != nil {
		t.Fatalf("evaluationMatrixEntries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}

	if got := entries[0].route; got != "route-a" {
		t.Errorf("route = %s, want route-a", got)
	}
	const wantBaseline = `{"route":"model-v1","seed":9007199254740993,"temperature":0.12345678901234567890123456789}`
	if got := string(entries[0].baselineConfig); got != wantBaseline {
		t.Errorf("baseline config = %s, want %s", got, wantBaseline)
	}
	const wantCandidate = `{"route":"model-v2","seed":9007199254740993,"temperature":0.12345678901234567890123456789}`
	if got := string(entries[0].candidateConfig); got != wantCandidate {
		t.Errorf("candidate config = %s, want %s", got, wantCandidate)
	}
	baselineHash := sha256.Sum256([]byte(wantBaseline))
	if got := entries[0].baselineConfigSHA256; got != fmt.Sprintf("%x", baselineHash) {
		t.Errorf("baselineConfigSHA256 = %s, want %x", got, baselineHash)
	}
	candidateHash := sha256.Sum256([]byte(wantCandidate))
	if got := entries[0].candidateConfigSHA256; got != fmt.Sprintf("%x", candidateHash) {
		t.Errorf("candidateConfigSHA256 = %s, want %x", got, candidateHash)
	}
}

func TestEvaluationMatrixEntriesRejectsUnpairedConfiguration(t *testing.T) {
	_, err := evaluationMatrixEntries([]byte(`[{"route":"route-a","temperature":0}]`))
	if err == nil {
		t.Fatal("evaluationMatrixEntries() error = nil, want unpaired matrix rejection")
	}
}

func TestCanonicalizeModelConfigPreservesNumbersAndDigestBytes(t *testing.T) {
	config, err := canonicalizeModelConfig([]byte(`{"temperature": 0.12345678901234567890123456789, "route": "route-a", "seed": 9007199254740993}`))
	if err != nil {
		t.Fatalf("canonicalizeModelConfig() error = %v", err)
	}

	const wantConfig = `{"route":"route-a","seed":9007199254740993,"temperature":0.12345678901234567890123456789}`
	if got := string(config); got != wantConfig {
		t.Errorf("config = %s, want %s", got, wantConfig)
	}
	wantHash := sha256.Sum256([]byte(wantConfig))
	if got := hashString(string(config)); got != fmt.Sprintf("%x", wantHash) {
		t.Errorf("config SHA256 = %s, want %x", got, wantHash)
	}
}

func TestRunCreationEligibilityEnforcesPlanKeyAndDailyBudget(t *testing.T) {
	reservation := decimal.RequireFromString("2")
	tests := []struct {
		name  string
		state evaluationPlanControlState
		ok    bool
	}{
		{name: "eligible", state: evaluationPlanControlState{enabled: true, keyUsable: true, dailyCostLimit: decimal.RequireFromString("10"), dailyReservedCost: decimal.RequireFromString("7")}, ok: true},
		{name: "disabled", state: evaluationPlanControlState{enabled: false, keyUsable: true, dailyCostLimit: decimal.RequireFromString("10")}, ok: false},
		{name: "unusable key", state: evaluationPlanControlState{enabled: true, keyUsable: false, dailyCostLimit: decimal.RequireFromString("10")}, ok: false},
		{name: "daily budget exceeded", state: evaluationPlanControlState{enabled: true, keyUsable: true, dailyCostLimit: decimal.RequireFromString("10"), dailyReservedCost: decimal.RequireFromString("9")}, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := runCreationEligible(test.state, reservation); got != test.ok {
				t.Errorf("runCreationEligible() = %t, want %t", got, test.ok)
			}
		})
	}
}

func TestAssignmentLeaseEligibilityEnforcesPlanConcurrency(t *testing.T) {
	tests := []struct {
		name  string
		state evaluationPlanControlState
		ok    bool
	}{
		{name: "slot available", state: evaluationPlanControlState{enabled: true, keyUsable: true, maxConcurrency: 4, activeLeases: 3}, ok: true},
		{name: "at limit", state: evaluationPlanControlState{enabled: true, keyUsable: true, maxConcurrency: 4, activeLeases: 4}, ok: false},
		{name: "disabled", state: evaluationPlanControlState{enabled: false, keyUsable: true, maxConcurrency: 4}, ok: false},
		{name: "unusable key", state: evaluationPlanControlState{enabled: true, keyUsable: false, maxConcurrency: 4}, ok: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := assignmentLeaseEligible(test.state); got != test.ok {
				t.Errorf("assignmentLeaseEligible() = %t, want %t", got, test.ok)
			}
		})
	}
}
