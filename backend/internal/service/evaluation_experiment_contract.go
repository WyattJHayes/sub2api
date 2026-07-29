package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/google/uuid"
)

const (
	RequestManifestSchemaV1 = "radar-request-manifest-v1"
	PairSpecSchemaV1        = "radar-pair-spec-v1"
	SideSpecSchemaV1        = "radar-side-spec-v1"
)

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// RequestManifest freezes only request-shape and semantic digest metadata.
// Prompt and tool argument bodies deliberately have no representation here.
type RequestManifest struct {
	SchemaVersion   string        `json:"schema_version"`
	InteractionType string        `json:"interaction_type"`
	OrdinalPolicy   string        `json:"ordinal_policy"`
	MinRequests     int           `json:"min_requests"`
	MaxRequests     int           `json:"max_requests"`
	RequestSlots    []RequestSlot `json:"request_slots"`
}

type RequestSlot struct {
	SlotID                         string `json:"slot_id"`
	OrdinalMin                     int    `json:"ordinal_min"`
	OrdinalMax                     int    `json:"ordinal_max"`
	Phase                          string `json:"phase"`
	Required                       bool   `json:"required"`
	SemanticsMode                  string `json:"semantics_mode"`
	ExpectedRequestSemanticsSHA256 string `json:"expected_request_semantics_sha256,omitempty"`
	RequestSemanticsPolicySHA256   string `json:"request_semantics_policy_sha256,omitempty"`
	ToolSchemaSHA256               string `json:"tool_schema_sha256"`
	AllowedToolSetSHA256           string `json:"allowed_tool_set_sha256"`
	MaxOccurrences                 int    `json:"max_occurrences"`
}

type CanonicalRequestManifest struct {
	Bytes  []byte
	SHA256 string
}

// PairSpec contains the shared experimental conditions for a baseline and
// candidate execution. It intentionally accepts content digests only.
type PairSpec struct {
	DatasetVersionID              uuid.UUID `json:"dataset_version_id"`
	CaseID                        uuid.UUID `json:"case_id"`
	SampleIndex                   int       `json:"sample_index"`
	RepeatIndex                   int       `json:"repeat_index"`
	PromptSHA256                  string    `json:"prompt_sha256"`
	ToolSchemaSHA256              string    `json:"tool_schema_sha256"`
	ExpectedRequestManifestID     uuid.UUID `json:"expected_request_manifest_id"`
	ExpectedRequestManifestSHA256 string    `json:"expected_request_manifest_sha256"`
	GraderID                      string    `json:"grader_id"`
	GraderVersion                 string    `json:"grader_version"`
	SamplingPolicy                string    `json:"sampling_policy"`
	RandomSeed                    string    `json:"random_seed"`
	Region                        string    `json:"region"`
	Protocol                      string    `json:"protocol"`
	TimeBlock                     string    `json:"time_block"`
	InterleaveOrder               string    `json:"interleave_order"`
	RetryPolicy                   string    `json:"retry_policy"`
	AllowedTreatmentFields        []string  `json:"allowed_treatment_fields"`
}

type SideSpec struct {
	Side                     string `json:"side"`
	ModelRoute               string `json:"model_route"`
	ModelConfigSHA256        string `json:"model_config_sha256"`
	ExpectedModelAlias       string `json:"expected_model_alias"`
	ExpectedResolvedModel    string `json:"expected_resolved_model"`
	RouteProfileVersion      string `json:"route_profile_version"`
	ProviderParametersSHA256 string `json:"provider_parameters_sha256"`
}

type CanonicalContract struct {
	Bytes  []byte
	SHA256 string
}

type PairBindingRef struct {
	PairSpecID      uuid.UUID
	PairSpecHash    string
	BaselineSideID  uuid.UUID
	CandidateSideID uuid.UUID
	BindingHash     string
}

type PairBinding struct {
	PairSpecHash          string
	BaselineSideSpecHash  string
	CandidateSideSpecHash string
	BindingHash           string
}

func CanonicalizeRequestManifest(manifest RequestManifest) (CanonicalRequestManifest, error) {
	if err := validateRequestManifest(manifest); err != nil {
		return CanonicalRequestManifest{}, err
	}
	contract, err := canonicalizeContract(manifest)
	if err != nil {
		return CanonicalRequestManifest{}, fmt.Errorf("canonicalize request manifest: %w", err)
	}
	return CanonicalRequestManifest{Bytes: contract.Bytes, SHA256: contract.SHA256}, nil
}

// DigestCanonicalJSON computes the RFC 8785 digest of JSON data without
// exposing the canonical bytes through any experiment contract type.
func DigestCanonicalJSON(raw []byte) (string, error) {
	canonical, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return "", err
	}
	return contractSHA256(string(canonical)), nil
}

func CanonicalizePairSpec(spec PairSpec) (CanonicalContract, error) {
	if err := validatePairSpec(spec); err != nil {
		return CanonicalContract{}, err
	}
	canonicalSpec := spec
	canonicalSpec.AllowedTreatmentFields = canonicalTreatmentFields(spec.AllowedTreatmentFields)
	return canonicalizeContract(canonicalSpec)
}

func CanonicalizeSideSpec(spec SideSpec) (CanonicalContract, error) {
	if err := validateSideSpec(spec); err != nil {
		return CanonicalContract{}, err
	}
	return canonicalizeContract(spec)
}

func BindEvaluationPair(pair PairSpec, baseline, candidate SideSpec) (PairBinding, error) {
	pairContract, err := CanonicalizePairSpec(pair)
	if err != nil {
		return PairBinding{}, err
	}
	baselineContract, err := CanonicalizeSideSpec(baseline)
	if err != nil {
		return PairBinding{}, err
	}
	candidateContract, err := CanonicalizeSideSpec(candidate)
	if err != nil {
		return PairBinding{}, err
	}
	if err := validatePairTreatment(pair, baseline, candidate); err != nil {
		return PairBinding{}, err
	}
	bindingHash := contractSHA256(pairContract.SHA256 + "\x00" + baselineContract.SHA256 + "\x00" + candidateContract.SHA256)
	return PairBinding{
		PairSpecHash:          pairContract.SHA256,
		BaselineSideSpecHash:  baselineContract.SHA256,
		CandidateSideSpecHash: candidateContract.SHA256,
		BindingHash:           bindingHash,
	}, nil
}

func validateRequestManifest(manifest RequestManifest) error {
	if manifest.SchemaVersion != RequestManifestSchemaV1 {
		return fmt.Errorf("unsupported request manifest schema version %q", manifest.SchemaVersion)
	}
	if manifest.InteractionType != "single" && manifest.InteractionType != "multi_turn" && manifest.InteractionType != "agent" {
		return fmt.Errorf("invalid request manifest interaction type %q", manifest.InteractionType)
	}
	if manifest.OrdinalPolicy != "exact" && manifest.OrdinalPolicy != "contiguous_bounded" {
		return fmt.Errorf("invalid request manifest ordinal policy %q", manifest.OrdinalPolicy)
	}
	if manifest.MinRequests < 1 || manifest.MaxRequests < manifest.MinRequests {
		return errors.New("invalid request manifest request bounds")
	}
	if len(manifest.RequestSlots) == 0 || len(manifest.RequestSlots) > manifest.MaxRequests {
		return errors.New("invalid request manifest slots")
	}
	if manifest.InteractionType == "single" && (manifest.OrdinalPolicy != "exact" || manifest.MinRequests != 1 || manifest.MaxRequests != 1 || len(manifest.RequestSlots) != 1) {
		return errors.New("single request manifest must declare one exact request")
	}

	seenIDs := make(map[string]struct{}, len(manifest.RequestSlots))
	previousMax := -1
	for index, slot := range manifest.RequestSlots {
		if err := validateRequestSlot(slot); err != nil {
			return fmt.Errorf("invalid request slot %d: %w", index, err)
		}
		if _, exists := seenIDs[slot.SlotID]; exists {
			return fmt.Errorf("duplicate request slot id %q", slot.SlotID)
		}
		seenIDs[slot.SlotID] = struct{}{}
		if slot.OrdinalMin <= previousMax {
			return errors.New("request slot ordinal ranges overlap or are unordered")
		}
		if manifest.OrdinalPolicy == "exact" && slot.OrdinalMin != slot.OrdinalMax {
			return errors.New("exact ordinal policy requires exact request slots")
		}
		if manifest.InteractionType == "single" && (slot.OrdinalMin != 0 || slot.OrdinalMax != 0 || !slot.Required || slot.MaxOccurrences != 1) {
			return errors.New("single request manifest must declare ordinal zero exactly once")
		}
		if manifest.InteractionType == "agent" && index > 0 && slot.OrdinalMin != previousMax+1 {
			return errors.New("agent request manifest slots must be contiguous")
		}
		previousMax = slot.OrdinalMax
	}
	if manifest.InteractionType == "agent" && manifest.RequestSlots[0].OrdinalMin != 0 {
		return errors.New("agent request manifest slots must begin at ordinal zero")
	}
	return nil
}

func validateRequestSlot(slot RequestSlot) error {
	if strings.TrimSpace(slot.SlotID) == "" || strings.TrimSpace(slot.Phase) == "" || slot.OrdinalMin < 0 || slot.OrdinalMax < slot.OrdinalMin || slot.MaxOccurrences < 1 {
		return errors.New("invalid slot identity or bounds")
	}
	if slot.SemanticsMode == "exact" {
		if !validSHA256(slot.ExpectedRequestSemanticsSHA256) || slot.RequestSemanticsPolicySHA256 != "" {
			return errors.New("exact request slot requires only expected semantics hash")
		}
	} else if slot.SemanticsMode == "adapter_policy" {
		if !validSHA256(slot.RequestSemanticsPolicySHA256) || slot.ExpectedRequestSemanticsSHA256 != "" {
			return errors.New("adapter policy request slot requires only policy semantics hash")
		}
	} else {
		return fmt.Errorf("invalid semantics mode %q", slot.SemanticsMode)
	}
	if !validSHA256(slot.ToolSchemaSHA256) || !validSHA256(slot.AllowedToolSetSHA256) {
		return errors.New("request slot tool hashes must be SHA-256 digests")
	}
	return nil
}

func validatePairSpec(spec PairSpec) error {
	if spec.DatasetVersionID == uuid.Nil || spec.CaseID == uuid.Nil || spec.ExpectedRequestManifestID == uuid.Nil || spec.SampleIndex < 0 || spec.SampleIndex > 9 || spec.RepeatIndex < 0 {
		return errors.New("invalid pair spec identity")
	}
	for name, value := range map[string]string{
		"prompt": spec.PromptSHA256, "tool schema": spec.ToolSchemaSHA256, "request manifest": spec.ExpectedRequestManifestSHA256,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("invalid pair spec %s hash", name)
		}
	}
	for name, value := range map[string]string{
		"grader id": spec.GraderID, "grader version": spec.GraderVersion, "sampling policy": spec.SamplingPolicy,
		"random seed": spec.RandomSeed, "region": spec.Region, "protocol": spec.Protocol,
		"time block": spec.TimeBlock, "interleave order": spec.InterleaveOrder, "retry policy": spec.RetryPolicy,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("pair spec %s is required", name)
		}
	}
	allowed := make(map[string]struct{}, len(spec.AllowedTreatmentFields))
	for _, field := range spec.AllowedTreatmentFields {
		if _, permitted := permittedTreatmentFields[field]; !permitted {
			return fmt.Errorf("unapproved treatment field %q", field)
		}
		if _, duplicate := allowed[field]; duplicate {
			return fmt.Errorf("duplicate treatment field %q", field)
		}
		allowed[field] = struct{}{}
	}
	return nil
}

func validateSideSpec(spec SideSpec) error {
	if spec.Side != "baseline" && spec.Side != "candidate" {
		return fmt.Errorf("invalid side %q", spec.Side)
	}
	if _, ok := comparisonRouteKey(spec.Side, spec.ModelRoute); !ok {
		return fmt.Errorf("invalid %s model route %q", spec.Side, spec.ModelRoute)
	}
	if !validSHA256(spec.ModelConfigSHA256) || !validSHA256(spec.ProviderParametersSHA256) {
		return errors.New("side spec hashes must be SHA-256 digests")
	}
	for name, value := range map[string]string{
		"expected model alias":    spec.ExpectedModelAlias,
		"expected resolved model": spec.ExpectedResolvedModel,
		"route profile version":   spec.RouteProfileVersion,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("side spec %s is required", name)
		}
	}
	return nil
}

var permittedTreatmentFields = map[string]struct{}{
	"model_config_sha256":        {},
	"expected_model_alias":       {},
	"expected_resolved_model":    {},
	"provider_parameters_sha256": {},
}

func validatePairTreatment(pair PairSpec, baseline, candidate SideSpec) error {
	if baseline.Side != "baseline" || candidate.Side != "candidate" {
		return errors.New("pair binding requires baseline and candidate side specs")
	}
	baselineRoute, _ := comparisonRouteKey("baseline", baseline.ModelRoute)
	candidateRoute, _ := comparisonRouteKey("candidate", candidate.ModelRoute)
	if baselineRoute != candidateRoute {
		return errors.New("pair sides must share the same comparison route")
	}
	allowed := make(map[string]struct{}, len(pair.AllowedTreatmentFields))
	for _, field := range pair.AllowedTreatmentFields {
		allowed[field] = struct{}{}
	}
	for field, values := range map[string][2]string{
		"model_config_sha256":        {baseline.ModelConfigSHA256, candidate.ModelConfigSHA256},
		"expected_model_alias":       {baseline.ExpectedModelAlias, candidate.ExpectedModelAlias},
		"expected_resolved_model":    {baseline.ExpectedResolvedModel, candidate.ExpectedResolvedModel},
		"route_profile_version":      {baseline.RouteProfileVersion, candidate.RouteProfileVersion},
		"provider_parameters_sha256": {baseline.ProviderParametersSHA256, candidate.ProviderParametersSHA256},
	} {
		if values[0] != values[1] {
			if _, permitted := allowed[field]; !permitted {
				return fmt.Errorf("unapproved treatment difference %q", field)
			}
		}
	}
	return nil
}

func canonicalizeContract(value any) (CanonicalContract, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return CanonicalContract{}, err
	}
	canonical, err := jsoncanonicalizer.Transform(encoded)
	if err != nil {
		return CanonicalContract{}, err
	}
	return CanonicalContract{Bytes: canonical, SHA256: contractSHA256(string(canonical))}, nil
}

func contractSHA256(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validSHA256(value string) bool {
	return sha256Pattern.MatchString(value)
}

func comparisonRouteKey(side, route string) (string, bool) {
	prefix := side + ":"
	key, found := strings.CutPrefix(strings.TrimSpace(route), prefix)
	return key, found && strings.TrimSpace(key) != ""
}

func canonicalTreatmentFields(fields []string) []string {
	canonical := append([]string(nil), fields...)
	sort.Strings(canonical)
	return canonical
}
