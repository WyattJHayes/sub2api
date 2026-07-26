package repository

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestEvaluationMatrixEntriesPreservesNumericLexemes(t *testing.T) {
	entries, err := evaluationMatrixEntries([]byte(`[{"route":"route-a","seed":9007199254740993,"temperature":0.12345678901234567890123456789}]`))
	if err != nil {
		t.Fatalf("evaluationMatrixEntries() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}

	const wantConfig = `{"route":"route-a","seed":9007199254740993,"temperature":0.12345678901234567890123456789}`
	if got := string(entries[0].config); got != wantConfig {
		t.Errorf("config = %s, want %s", got, wantConfig)
	}
	wantHash := sha256.Sum256([]byte(wantConfig))
	if got := entries[0].configSHA256; got != fmt.Sprintf("%x", wantHash) {
		t.Errorf("configSHA256 = %s, want %x", got, wantHash)
	}
}
