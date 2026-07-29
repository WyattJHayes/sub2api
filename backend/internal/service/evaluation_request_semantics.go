package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
)

const RequestSemanticsSchemaV1 = "radar-request-semantics-v1"

var (
	ErrRequestSemanticsVerifierNotRegistered = errors.New("request semantics verifier is not registered")
	ErrRequestSemanticsMismatch              = errors.New("request semantics do not match the frozen request slot")
)

type EvidenceRef struct {
	RouteTraceID   string `json:"route_trace_id"`
	RequestOrdinal int    `json:"request_ordinal"`
	PayloadHash    string `json:"payload_hash"`
}

// RequestSemantics captures request shape and content digests. It has no field
// capable of carrying prompt, tool argument, or completion bodies.
type RequestSemantics struct {
	SchemaVersion       string        `json:"schema_version"`
	InteractionType     string        `json:"interaction_type"`
	SlotID              string        `json:"slot_id"`
	RequestOrdinal      int           `json:"request_ordinal"`
	Phase               string        `json:"phase"`
	MessageRoleSequence []string      `json:"message_role_sequence"`
	ContentPartTypes    [][]string    `json:"content_part_types"`
	PromptHash          string        `json:"prompt_hash"`
	ToolSchemaHash      string        `json:"tool_schema_hash"`
	ProvidedToolSetHash string        `json:"provided_tool_set_hash"`
	ToolChoicePolicy    string        `json:"tool_choice_policy"`
	SamplingPolicyHash  string        `json:"sampling_policy_hash"`
	PreviousEvidence    []EvidenceRef `json:"previous_evidence_refs"`
}

type CanonicalRequestSemantics struct {
	Semantics RequestSemantics
	Bytes     []byte
	SHA256    string
}

func CanonicalizeRequestSemantics(semantics RequestSemantics) (CanonicalRequestSemantics, error) {
	normalized := normalizeRequestSemantics(semantics)
	if err := validateRequestSemantics(normalized); err != nil {
		return CanonicalRequestSemantics{}, err
	}
	canonical, err := canonicalizeContract(normalized)
	if err != nil {
		return CanonicalRequestSemantics{}, fmt.Errorf("canonicalize request semantics: %w", err)
	}
	return CanonicalRequestSemantics{
		Semantics: normalized,
		Bytes:     canonical.Bytes,
		SHA256:    canonical.SHA256,
	}, nil
}

func DeriveSingleRequestSemantics(raw []byte) (RequestSemantics, error) {
	var request map[string]any
	if err := json.Unmarshal(raw, &request); err != nil {
		return RequestSemantics{}, fmt.Errorf("decode request semantics source: %w", err)
	}
	if request == nil {
		return RequestSemantics{}, errors.New("request semantics source must be a JSON object")
	}
	delete(request, "model")
	promptHash, err := digestCanonicalValue(request)
	if err != nil {
		return RequestSemantics{}, err
	}
	toolSchema := make(map[string]any)
	for _, key := range []string{"tools", "functions", "tool_config"} {
		if value, ok := request[key]; ok {
			toolSchema[key] = value
		}
	}
	toolHash, err := digestCanonicalValue(toolSchema)
	if err != nil {
		return RequestSemantics{}, err
	}
	sampling := make(map[string]any)
	for _, key := range []string{"temperature", "top_p", "top_k", "seed", "max_tokens", "max_output_tokens", "reasoning_effort", "response_format"} {
		if value, ok := request[key]; ok {
			sampling[key] = value
		}
	}
	samplingHash, err := digestCanonicalValue(sampling)
	if err != nil {
		return RequestSemantics{}, err
	}
	roles, partTypes := requestRoleAndPartTypes(request)
	toolChoice := "none"
	if value, ok := request["tool_choice"]; ok {
		if choice, stringValue := value.(string); stringValue && strings.TrimSpace(choice) != "" {
			toolChoice = strings.TrimSpace(choice)
		} else {
			choiceHash, hashErr := digestCanonicalValue(value)
			if hashErr != nil {
				return RequestSemantics{}, hashErr
			}
			toolChoice = "sha256:" + choiceHash
		}
	}
	return RequestSemantics{
		SchemaVersion:       RequestSemanticsSchemaV1,
		InteractionType:     "single",
		SlotID:              "request-0",
		RequestOrdinal:      0,
		Phase:               "primary",
		MessageRoleSequence: roles,
		ContentPartTypes:    partTypes,
		PromptHash:          promptHash,
		ToolSchemaHash:      toolHash,
		ProvidedToolSetHash: toolHash,
		ToolChoicePolicy:    toolChoice,
		SamplingPolicyHash:  samplingHash,
		PreviousEvidence:    []EvidenceRef{},
	}, nil
}

func digestCanonicalValue(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal request semantics projection: %w", err)
	}
	digest, err := DigestCanonicalJSON(raw)
	if err != nil {
		return "", fmt.Errorf("digest request semantics projection: %w", err)
	}
	return digest, nil
}

func requestRoleAndPartTypes(request map[string]any) ([]string, [][]string) {
	if messages, ok := request["messages"].([]any); ok && len(messages) > 0 {
		return roleAndPartTypesFromItems(messages, "user")
	}
	if input, ok := request["input"].([]any); ok && len(input) > 0 {
		return roleAndPartTypesFromItems(input, "user")
	}
	if contents, ok := request["contents"].([]any); ok && len(contents) > 0 {
		return roleAndPartTypesFromItems(contents, "user")
	}
	return []string{"user"}, [][]string{{"text"}}
}

func roleAndPartTypesFromItems(items []any, defaultRole string) ([]string, [][]string) {
	roles := make([]string, 0, len(items))
	parts := make([][]string, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			roles = append(roles, defaultRole)
			parts = append(parts, []string{"text"})
			continue
		}
		role := defaultRole
		if value, ok := object["role"].(string); ok && strings.TrimSpace(value) != "" {
			role = strings.TrimSpace(value)
		}
		roles = append(roles, role)
		content := object["content"]
		if content == nil {
			content = object["parts"]
		}
		parts = append(parts, contentPartTypes(content))
	}
	return roles, parts
}

func contentPartTypes(content any) []string {
	items, ok := content.([]any)
	if !ok || len(items) == 0 {
		return []string{"text"}
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		object, objectValue := item.(map[string]any)
		if !objectValue {
			result = append(result, "text")
			continue
		}
		if value, ok := object["type"].(string); ok && strings.TrimSpace(value) != "" {
			result = append(result, strings.TrimSpace(value))
			continue
		}
		partType := "unknown"
		for _, key := range []string{"text", "inline_data", "file_data", "function_call", "function_response"} {
			if _, exists := object[key]; exists {
				partType = key
				break
			}
		}
		result = append(result, partType)
	}
	return result
}

type RequestSemanticsVerifier interface {
	VerifyRequestSemantics(ctx context.Context, semantics CanonicalRequestSemantics) error
}

type RequestSemanticsVerifierFunc func(context.Context, CanonicalRequestSemantics) error

func (f RequestSemanticsVerifierFunc) VerifyRequestSemantics(ctx context.Context, semantics CanonicalRequestSemantics) error {
	return f(ctx, semantics)
}

type RequestSemanticsVerifierRegistry struct {
	mu        sync.RWMutex
	verifiers map[string]RequestSemanticsVerifier
}

func NewRequestSemanticsVerifierRegistry() *RequestSemanticsVerifierRegistry {
	return &RequestSemanticsVerifierRegistry{verifiers: make(map[string]RequestSemanticsVerifier)}
}

func (r *RequestSemanticsVerifierRegistry) Register(policySHA256 string, verifier RequestSemanticsVerifier) error {
	policySHA256 = strings.TrimSpace(policySHA256)
	if !validSHA256(policySHA256) || verifier == nil {
		return errors.New("request semantics verifier requires a policy SHA-256 and implementation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.verifiers[policySHA256]; exists {
		return fmt.Errorf("request semantics verifier policy %s is already registered", policySHA256)
	}
	r.verifiers[policySHA256] = verifier
	return nil
}

func (r *RequestSemanticsVerifierRegistry) Verify(ctx context.Context, policySHA256 string, semantics CanonicalRequestSemantics) error {
	if r == nil {
		return ErrRequestSemanticsVerifierNotRegistered
	}
	r.mu.RLock()
	verifier := r.verifiers[strings.TrimSpace(policySHA256)]
	r.mu.RUnlock()
	if verifier == nil {
		return fmt.Errorf("%w: %s", ErrRequestSemanticsVerifierNotRegistered, strings.TrimSpace(policySHA256))
	}
	return verifier.VerifyRequestSemantics(ctx, semantics)
}

func normalizeRequestSemantics(semantics RequestSemantics) RequestSemantics {
	semantics.SchemaVersion = strings.TrimSpace(semantics.SchemaVersion)
	semantics.InteractionType = strings.TrimSpace(semantics.InteractionType)
	semantics.SlotID = strings.TrimSpace(semantics.SlotID)
	semantics.Phase = strings.TrimSpace(semantics.Phase)
	semantics.PromptHash = strings.TrimSpace(semantics.PromptHash)
	semantics.ToolSchemaHash = strings.TrimSpace(semantics.ToolSchemaHash)
	semantics.ProvidedToolSetHash = strings.TrimSpace(semantics.ProvidedToolSetHash)
	semantics.ToolChoicePolicy = strings.TrimSpace(semantics.ToolChoicePolicy)
	semantics.SamplingPolicyHash = strings.TrimSpace(semantics.SamplingPolicyHash)
	if semantics.MessageRoleSequence == nil {
		semantics.MessageRoleSequence = []string{}
	}
	if semantics.ContentPartTypes == nil {
		semantics.ContentPartTypes = [][]string{}
	}
	if semantics.PreviousEvidence == nil {
		semantics.PreviousEvidence = []EvidenceRef{}
	}
	return semantics
}

func validateRequestSemantics(semantics RequestSemantics) error {
	if semantics.SchemaVersion != RequestSemanticsSchemaV1 {
		return fmt.Errorf("unsupported request semantics schema version %q", semantics.SchemaVersion)
	}
	if semantics.InteractionType != "single" && semantics.InteractionType != "multi_turn" && semantics.InteractionType != "agent" {
		return fmt.Errorf("invalid request semantics interaction type %q", semantics.InteractionType)
	}
	if semantics.SlotID == "" || semantics.Phase == "" || semantics.RequestOrdinal < 0 {
		return errors.New("request semantics slot identity is invalid")
	}
	if len(semantics.MessageRoleSequence) == 0 || len(semantics.MessageRoleSequence) != len(semantics.ContentPartTypes) {
		return errors.New("request semantics roles and content parts must align")
	}
	for index, role := range semantics.MessageRoleSequence {
		if strings.TrimSpace(role) == "" || len(semantics.ContentPartTypes[index]) == 0 {
			return errors.New("request semantics role and content part types are required")
		}
		for _, partType := range semantics.ContentPartTypes[index] {
			if strings.TrimSpace(partType) == "" {
				return errors.New("request semantics content part type is required")
			}
		}
	}
	for name, value := range map[string]string{
		"prompt":            semantics.PromptHash,
		"tool schema":       semantics.ToolSchemaHash,
		"provided tool set": semantics.ProvidedToolSetHash,
		"sampling policy":   semantics.SamplingPolicyHash,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("request semantics %s hash is invalid", name)
		}
	}
	if semantics.ToolChoicePolicy == "" {
		return errors.New("request semantics tool choice policy is required")
	}
	previousOrdinal := -1
	for _, ref := range semantics.PreviousEvidence {
		if _, err := uuid.Parse(strings.TrimSpace(ref.RouteTraceID)); err != nil || ref.RequestOrdinal < 0 || ref.RequestOrdinal >= semantics.RequestOrdinal || !validSHA256(ref.PayloadHash) {
			return errors.New("request semantics previous evidence reference is invalid")
		}
		if ref.RequestOrdinal <= previousOrdinal {
			return errors.New("request semantics previous evidence references must be ordered and unique")
		}
		previousOrdinal = ref.RequestOrdinal
	}
	return nil
}
