package repository

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// Case prompt_spec is the frozen request source. A route configuration may
// repeat a parameter only when it agrees with every case; it cannot override
// the shared exact request manifest.
var evaluationRequestParameterKeys = []string{
	"temperature", "top_p", "top_k", "seed", "max_tokens", "max_output_tokens", "max_completion_tokens",
	"reasoning_effort", "reasoning", "response_format", "text", "stop", "tools", "tool_choice",
	"functions", "function_call", "tool_config", "parallel_tool_calls",
}

func validateEvaluationMatrixParameters(matrix []evaluationMatrixEntry, cases []evaluationCaseForRun) error {
	for _, evaluationCase := range cases {
		var prompt map[string]json.RawMessage
		if !json.Valid(evaluationCase.promptSpec) {
			return fmt.Errorf("decode evaluation case request parameters: invalid JSON")
		}
		if bytes.HasPrefix(bytes.TrimSpace(evaluationCase.promptSpec), []byte("{")) {
			if err := json.Unmarshal(evaluationCase.promptSpec, &prompt); err != nil {
				return fmt.Errorf("decode evaluation case request parameters: %w", err)
			}
		}
		var execution struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(evaluationCase.executionSpec, &execution); err != nil {
			return fmt.Errorf("decode evaluation case execution parameters: %w", err)
		}
		for _, entry := range matrix {
			for _, side := range []string{"baseline", "candidate"} {
				raw, _ := entry.configForSide(side)
				var config map[string]json.RawMessage
				if err := json.Unmarshal(raw, &config); err != nil {
					return fmt.Errorf("decode evaluation route parameters: %w", err)
				}
				for _, key := range evaluationRequestParameterKeys {
					value, exists := config[key]
					if !exists {
						continue
					}
					requestKey := key
					if key == "max_tokens" && evaluationUsesResponsesAPI(execution.URL) {
						requestKey = "max_output_tokens"
					}
					frozen, present := prompt[requestKey]
					actual, err := canonicalizeModelConfig(value)
					if err != nil {
						return err
					}
					var expected []byte
					if present {
						expected, err = canonicalizeModelConfig(frozen)
						if err != nil {
							return err
						}
					}
					if !present || string(actual) != string(expected) {
						return infraerrors.New(http.StatusBadRequest, "UNFROZEN_REQUEST_PARAMETERS",
							fmt.Sprintf("%s request parameters %q do not match case %s; configure parameters in published case prompt_spec", side, key, evaluationCase.id))
					}
				}
			}
		}
	}
	return nil
}

func evaluationUsesResponsesAPI(rawURL string) bool {
	path := strings.TrimSpace(rawURL)
	if path == "" {
		return true
	}
	path, _, _ = strings.Cut(path, "?")
	return strings.HasSuffix(strings.TrimRight(path, "/"), "/responses")
}
