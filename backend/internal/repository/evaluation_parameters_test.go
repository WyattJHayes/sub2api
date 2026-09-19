package repository

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestEvaluationParametersMustMatchEveryFrozenCaseAndSide(t *testing.T) {
	for _, test := range []struct {
		name, baseline, candidate, prompt, secondPrompt, endpoint string
		wantError                                                 bool
	}{
		{name: "route only", baseline: `{}`, candidate: `{}`, prompt: `{"input":"ping"}`},
		{name: "matching responses alias", baseline: `{"max_tokens":64}`, candidate: `{"max_output_tokens":64}`, prompt: `{"max_output_tokens":64}`},
		{name: "matching responses alias with query", baseline: `{"max_tokens":64}`, candidate: `{}`, prompt: `{"max_output_tokens":64}`, endpoint: "/v1/responses?trace=1"},
		{name: "matching responses alias with trailing slash", baseline: `{"max_tokens":64}`, candidate: `{}`, prompt: `{"max_output_tokens":64}`, endpoint: "/v1/responses/"},
		{name: "matching chat token limit", baseline: `{"max_tokens":32}`, candidate: `{}`, prompt: `{"max_tokens":32}`, endpoint: "/v1/chat/completions"},
		{name: "missing frozen parameter", baseline: `{"temperature":0}`, candidate: `{}`, prompt: `{}`, wantError: true},
		{name: "candidate mismatch", baseline: `{"temperature":0}`, candidate: `{"temperature":1}`, prompt: `{"temperature":0}`, wantError: true},
		{name: "second case mismatch", baseline: `{"max_tokens":64}`, candidate: `{}`, prompt: `{"max_output_tokens":64}`, secondPrompt: `{"max_output_tokens":32}`, wantError: true},
		{name: "bool is not number", baseline: `{"seed":true}`, candidate: `{}`, prompt: `{"seed":1}`, wantError: true},
		{name: "numeric lexemes are conservative", baseline: `{"temperature":0.0}`, candidate: `{}`, prompt: `{"temperature":0}`, wantError: true},
		{name: "lossless integer", baseline: `{"seed":9007199254740993}`, candidate: `{}`, prompt: `{"seed":9007199254740993}`},
		{name: "nested object order", baseline: `{"reasoning":{"effort":"low","summary":"auto"}}`, candidate: `{}`, prompt: `{"reasoning":{"summary":"auto","effort":"low"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := evaluationMatrixEntry{baselineConfig: []byte(test.baseline), candidateConfig: []byte(test.candidate)}
			cases := []evaluationCaseForRun{{id: uuid.New(), promptSpec: []byte(test.prompt), executionSpec: []byte(`{"url":"` + test.endpoint + `"}`)}}
			if test.secondPrompt != "" {
				cases = append(cases, evaluationCaseForRun{id: uuid.New(), promptSpec: []byte(test.secondPrompt), executionSpec: []byte(`{}`)})
			}
			err := validateEvaluationMatrixParameters([]evaluationMatrixEntry{entry}, cases)
			if test.wantError {
				require.ErrorContains(t, err, "request parameters")
			} else {
				require.NoError(t, err)
			}
		})
	}
}
