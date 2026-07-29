package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/google/uuid"
)

const RequestManifestSchemaVersion = "radar-request-manifest-v1"

type InteractionType string

const (
	InteractionSingle    InteractionType = "single"
	InteractionMultiTurn InteractionType = "multi_turn"
	InteractionAgent     InteractionType = "agent"
)

type OrdinalPolicy string

const (
	OrdinalPolicyExact             OrdinalPolicy = "exact"
	OrdinalPolicyContiguousBounded OrdinalPolicy = "contiguous_bounded"
)

type SemanticsMode string

const (
	SemanticsModeExact         SemanticsMode = "exact"
	SemanticsModeAdapterPolicy SemanticsMode = "adapter_policy"
)

type RequestManifest struct {
	ID              uuid.UUID       `json:"-"`
	SchemaVersion   string          `json:"schema_version"`
	InteractionType InteractionType `json:"interaction_type"`
	OrdinalPolicy   OrdinalPolicy   `json:"ordinal_policy"`
	MinRequests     int             `json:"min_requests"`
	MaxRequests     int             `json:"max_requests"`
	RequestSlots    []RequestSlot   `json:"request_slots"`
}

type RequestManifestRecord struct {
	ID              uuid.UUID
	SchemaVersion   string
	InteractionType InteractionType
	ManifestSHA256  string
}

type RequestSlot struct {
	SlotID                         string        `json:"slot_id"`
	OrdinalMin                     int           `json:"ordinal_min"`
	OrdinalMax                     int           `json:"ordinal_max"`
	Phase                          string        `json:"phase"`
	Required                       bool          `json:"required"`
	SemanticsMode                  SemanticsMode `json:"semantics_mode"`
	ExpectedRequestSemanticsSHA256 string        `json:"expected_request_semantics_sha256,omitempty"`
	RequestSemanticsPolicySHA256   string        `json:"request_semantics_policy_sha256,omitempty"`
	ToolSchemaSHA256               string        `json:"tool_schema_sha256,omitempty"`
	AllowedToolSetSHA256           string        `json:"allowed_tool_set_sha256,omitempty"`
	MaxOccurrences                 int           `json:"max_occurrences"`
}

type PairSpec struct {
	ID                            uuid.UUID
	DatasetVersionID              uuid.UUID
	CaseID                        uuid.UUID
	SampleIndex                   int
	RepeatIndex                   int
	PromptSHA256                  string
	ToolSchemaSHA256              string
	ExpectedRequestManifestID     uuid.UUID
	ExpectedRequestManifestSHA256 string
	GraderID                      string
	GraderVersion                 string
	SamplingPolicy                string
	RandomSeed                    int64
	Region                        string
	Protocol                      string
	TimeBlock                     string
	InterleaveOrder               string
	RetryPolicy                   string
	AllowedTreatmentFields        []string
}

type SideSpec struct {
	ID                       uuid.UUID
	PairSpecID               uuid.UUID
	SampleID                 uuid.UUID
	Side                     string
	ModelRoute               string
	ModelConfigSHA256        string
	ExpectedModelAlias       string
	ExpectedResolvedModel    string
	RouteProfileVersion      string
	ProviderParametersSHA256 string
}

type PairBinding struct {
	ID                uuid.UUID
	PairSpecID        uuid.UUID
	PairSpecHash      string
	BaselineSideID    uuid.UUID
	BaselineSideHash  string
	CandidateSideID   uuid.UUID
	CandidateSideHash string
	BindingHash       string
}

func CanonicalRequestManifest(manifest RequestManifest) ([]byte, string, error) {
	if err := validateRequestManifest(manifest); err != nil {
		return nil, "", err
	}
	sorted := manifest
	sorted.RequestSlots = append([]RequestSlot(nil), manifest.RequestSlots...)
	sort.Slice(sorted.RequestSlots, func(i, j int) bool {
		left, right := sorted.RequestSlots[i], sorted.RequestSlots[j]
		if left.OrdinalMin != right.OrdinalMin {
			return left.OrdinalMin < right.OrdinalMin
		}
		if left.OrdinalMax != right.OrdinalMax {
			return left.OrdinalMax < right.OrdinalMax
		}
		return left.SlotID < right.SlotID
	})
	encoded, err := json.Marshal(sorted)
	if err != nil {
		return nil, "", fmt.Errorf("marshal request manifest: %w", err)
	}
	canonical, err := jsoncanonicalizer.Transform(encoded)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize request manifest: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(digest[:]), nil
}

func validateRequestManifest(manifest RequestManifest) error {
	if manifest.SchemaVersion != RequestManifestSchemaVersion {
		return fmt.Errorf("unsupported request manifest schema %q", manifest.SchemaVersion)
	}
	if manifest.InteractionType != InteractionSingle && manifest.InteractionType != InteractionMultiTurn && manifest.InteractionType != InteractionAgent {
		return fmt.Errorf("invalid interaction type %q", manifest.InteractionType)
	}
	if manifest.OrdinalPolicy != OrdinalPolicyExact && manifest.OrdinalPolicy != OrdinalPolicyContiguousBounded {
		return fmt.Errorf("invalid ordinal policy %q", manifest.OrdinalPolicy)
	}
	if manifest.MinRequests < 1 || manifest.MaxRequests < manifest.MinRequests {
		return errors.New("request count bounds are invalid")
	}
	if len(manifest.RequestSlots) == 0 {
		return errors.New("request manifest requires request slots")
	}
	if manifest.InteractionType == InteractionSingle && (manifest.OrdinalPolicy != OrdinalPolicyExact || manifest.MinRequests != 1 || manifest.MaxRequests != 1) {
		return errors.New("single interaction requires one exact request")
	}

	slots := append([]RequestSlot(nil), manifest.RequestSlots...)
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].OrdinalMin != slots[j].OrdinalMin {
			return slots[i].OrdinalMin < slots[j].OrdinalMin
		}
		return slots[i].OrdinalMax < slots[j].OrdinalMax
	})
	seen := make(map[string]struct{}, len(slots))
	previousMax := -1
	for index, slot := range slots {
		if strings.TrimSpace(slot.SlotID) == "" {
			return errors.New("request slot id is required")
		}
		if _, exists := seen[slot.SlotID]; exists {
			return fmt.Errorf("duplicate request slot %q", slot.SlotID)
		}
		seen[slot.SlotID] = struct{}{}
		if slot.OrdinalMin < 0 || slot.OrdinalMax < slot.OrdinalMin {
			return fmt.Errorf("request slot %q has invalid ordinal range", slot.SlotID)
		}
		if index > 0 && slot.OrdinalMin <= previousMax {
			return fmt.Errorf("request slot %q ordinal range overlap", slot.SlotID)
		}
		if index > 0 && manifest.OrdinalPolicy == OrdinalPolicyContiguousBounded && slot.OrdinalMin != previousMax+1 {
			return fmt.Errorf("request slot %q ordinal range is not contiguous", slot.SlotID)
		}
		if manifest.OrdinalPolicy == OrdinalPolicyExact && slot.OrdinalMin != slot.OrdinalMax {
			return fmt.Errorf("exact request slot %q must have one ordinal", slot.SlotID)
		}
		if slot.MaxOccurrences < 1 {
			return fmt.Errorf("request slot %q max occurrences must be positive", slot.SlotID)
		}
		switch slot.SemanticsMode {
		case SemanticsModeExact:
			if !validSHA256(slot.ExpectedRequestSemanticsSHA256) || slot.RequestSemanticsPolicySHA256 != "" {
				return fmt.Errorf("exact semantics slot %q requires expected semantics hash only", slot.SlotID)
			}
		case SemanticsModeAdapterPolicy:
			if slot.ExpectedRequestSemanticsSHA256 != "" || !validSHA256(slot.RequestSemanticsPolicySHA256) {
				return fmt.Errorf("adapter policy slot %q requires policy hash only", slot.SlotID)
			}
		default:
			return fmt.Errorf("request slot %q has invalid semantics mode %q", slot.SlotID, slot.SemanticsMode)
		}
		for name, value := range map[string]string{
			"tool_schema_sha256":      slot.ToolSchemaSHA256,
			"allowed_tool_set_sha256": slot.AllowedToolSetSHA256,
		} {
			if value != "" && !validSHA256(value) {
				return fmt.Errorf("request slot %q has invalid %s", slot.SlotID, name)
			}
		}
		previousMax = slot.OrdinalMax
	}
	if manifest.OrdinalPolicy == OrdinalPolicyContiguousBounded && slots[0].OrdinalMin != 0 {
		return errors.New("contiguous request slots must start at ordinal zero")
	}
	return nil
}

func CanonicalPairSpec(pair PairSpec) ([]byte, string, error) {
	if err := validatePairSpec(pair); err != nil {
		return nil, "", err
	}
	allowedTreatmentFields := append([]string(nil), pair.AllowedTreatmentFields...)
	sort.Strings(allowedTreatmentFields)
	payload := struct {
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
		RandomSeed                    int64     `json:"random_seed"`
		Region                        string    `json:"region"`
		Protocol                      string    `json:"protocol"`
		TimeBlock                     string    `json:"time_block"`
		InterleaveOrder               string    `json:"interleave_order"`
		RetryPolicy                   string    `json:"retry_policy"`
		AllowedTreatmentFields        []string  `json:"allowed_treatment_fields"`
	}{pair.DatasetVersionID, pair.CaseID, pair.SampleIndex, pair.RepeatIndex, pair.PromptSHA256, pair.ToolSchemaSHA256,
		pair.ExpectedRequestManifestID, pair.ExpectedRequestManifestSHA256, pair.GraderID, pair.GraderVersion,
		pair.SamplingPolicy, pair.RandomSeed, pair.Region, pair.Protocol, pair.TimeBlock, pair.InterleaveOrder,
		pair.RetryPolicy, allowedTreatmentFields}
	return canonicalHash(payload, "pair spec")
}

func CanonicalSideSpec(side SideSpec) ([]byte, string, error) {
	if err := validateSideSpec(side); err != nil {
		return nil, "", err
	}
	payload := struct {
		Side                     string `json:"side"`
		ModelRoute               string `json:"model_route"`
		ModelConfigSHA256        string `json:"model_config_sha256"`
		ExpectedModelAlias       string `json:"expected_model_alias"`
		ExpectedResolvedModel    string `json:"expected_resolved_model"`
		RouteProfileVersion      string `json:"route_profile_version"`
		ProviderParametersSHA256 string `json:"provider_parameters_sha256"`
	}{side.Side, side.ModelRoute, side.ModelConfigSHA256, side.ExpectedModelAlias, side.ExpectedResolvedModel, side.RouteProfileVersion, side.ProviderParametersSHA256}
	return canonicalHash(payload, "side spec")
}

func BuildPairBinding(pair PairSpec, baseline, candidate SideSpec) (PairBinding, error) {
	if err := validatePairSpec(pair); err != nil {
		return PairBinding{}, err
	}
	if err := validateSideSpec(baseline); err != nil {
		return PairBinding{}, fmt.Errorf("baseline side: %w", err)
	}
	if err := validateSideSpec(candidate); err != nil {
		return PairBinding{}, fmt.Errorf("candidate side: %w", err)
	}
	if baseline.Side != "baseline" || candidate.Side != "candidate" {
		return PairBinding{}, errors.New("pair binding requires baseline and candidate sides")
	}
	if baseline.ID == candidate.ID {
		return PairBinding{}, errors.New("pair binding sides must have distinct identities")
	}
	if baseline.SampleID == uuid.Nil || candidate.SampleID == uuid.Nil {
		return PairBinding{}, errors.New("pair binding sides require sample identities")
	}
	if baseline.SampleID == candidate.SampleID {
		return PairBinding{}, errors.New("pair binding sides must reference distinct samples")
	}
	for sideName, side := range map[string]SideSpec{"baseline": baseline, "candidate": candidate} {
		if side.PairSpecID != uuid.Nil && side.PairSpecID != pair.ID {
			return PairBinding{}, fmt.Errorf("%s side belongs to a different pair spec", sideName)
		}
	}
	baseKey, ok := comparisonRouteKey(baseline.ModelRoute)
	if !ok {
		return PairBinding{}, errors.New("baseline model route must use baseline prefix")
	}
	candidateKey, ok := comparisonRouteKey(candidate.ModelRoute)
	if !ok || baseKey != candidateKey {
		return PairBinding{}, errors.New("baseline and candidate comparison routes do not match")
	}
	allowed := make(map[string]struct{}, len(pair.AllowedTreatmentFields))
	for _, field := range pair.AllowedTreatmentFields {
		allowed[field] = struct{}{}
	}
	baseValues := sideTreatmentValues(baseline)
	candidateValues := sideTreatmentValues(candidate)
	for field, baseValue := range baseValues {
		if baseValue != candidateValues[field] {
			if _, exists := allowed[field]; !exists {
				return PairBinding{}, fmt.Errorf("treatment field %q differs without approval", field)
			}
		}
	}
	_, pairHash, err := CanonicalPairSpec(pair)
	if err != nil {
		return PairBinding{}, err
	}
	_, baselineHash, err := CanonicalSideSpec(baseline)
	if err != nil {
		return PairBinding{}, err
	}
	_, candidateHash, err := CanonicalSideSpec(candidate)
	if err != nil {
		return PairBinding{}, err
	}
	bindingPayload := struct {
		PairSpecHash      string `json:"pair_spec_hash"`
		BaselineSideHash  string `json:"baseline_side_hash"`
		CandidateSideHash string `json:"candidate_side_hash"`
	}{pairHash, baselineHash, candidateHash}
	_, bindingHash, err := canonicalHash(bindingPayload, "pair binding")
	if err != nil {
		return PairBinding{}, err
	}
	return PairBinding{
		ID:                uuid.New(),
		PairSpecID:        pair.ID,
		PairSpecHash:      pairHash,
		BaselineSideID:    baseline.ID,
		BaselineSideHash:  baselineHash,
		CandidateSideID:   candidate.ID,
		CandidateSideHash: candidateHash,
		BindingHash:       bindingHash,
	}, nil
}

func validatePairSpec(pair PairSpec) error {
	if pair.ID == uuid.Nil || pair.DatasetVersionID == uuid.Nil || pair.CaseID == uuid.Nil || pair.ExpectedRequestManifestID == uuid.Nil {
		return errors.New("pair spec identities are required")
	}
	if pair.SampleIndex < 0 || pair.RepeatIndex < 0 {
		return errors.New("pair spec indexes must be non-negative")
	}
	for name, value := range map[string]string{
		"prompt_sha256":                    pair.PromptSHA256,
		"tool_schema_sha256":               pair.ToolSchemaSHA256,
		"expected_request_manifest_sha256": pair.ExpectedRequestManifestSHA256,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("pair spec %s must be a sha256", name)
		}
	}
	if strings.TrimSpace(pair.GraderID) == "" || strings.TrimSpace(pair.GraderVersion) == "" || strings.TrimSpace(pair.Region) == "" || strings.TrimSpace(pair.Protocol) == "" {
		return errors.New("pair spec grader, region and protocol are required")
	}
	seen := make(map[string]struct{}, len(pair.AllowedTreatmentFields))
	for _, field := range pair.AllowedTreatmentFields {
		if _, exists := seen[field]; exists {
			return fmt.Errorf("duplicate treatment field %q", field)
		}
		seen[field] = struct{}{}
		if _, ok := sideTreatmentValues(SideSpec{})[field]; !ok {
			return fmt.Errorf("unsupported treatment field %q", field)
		}
	}
	return nil
}

func validateSideSpec(side SideSpec) error {
	if side.ID == uuid.Nil || (side.Side != "baseline" && side.Side != "candidate") || strings.TrimSpace(side.ModelRoute) == "" {
		return errors.New("side spec identity and model route are required")
	}
	if !validSHA256(side.ModelConfigSHA256) || !validSHA256(side.ProviderParametersSHA256) {
		return errors.New("side spec hashes must be sha256")
	}
	if strings.TrimSpace(side.ExpectedModelAlias) == "" || strings.TrimSpace(side.ExpectedResolvedModel) == "" || strings.TrimSpace(side.RouteProfileVersion) == "" {
		return errors.New("side spec model and route identity are required")
	}
	return nil
}

func sideTreatmentValues(side SideSpec) map[string]string {
	return map[string]string{
		"model_config_sha256":        side.ModelConfigSHA256,
		"expected_model_alias":       side.ExpectedModelAlias,
		"expected_resolved_model":    side.ExpectedResolvedModel,
		"provider_parameters_sha256": side.ProviderParametersSHA256,
	}
}

func comparisonRouteKey(route string) (string, bool) {
	prefix, key, ok := strings.Cut(route, ":")
	if !ok || key == "" || (prefix != "baseline" && prefix != "candidate") {
		return "", false
	}
	return key, true
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func canonicalHash(payload any, label string) ([]byte, string, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, "", fmt.Errorf("marshal %s: %w", label, err)
	}
	canonical, err := jsoncanonicalizer.Transform(encoded)
	if err != nil {
		return nil, "", fmt.Errorf("canonicalize %s: %w", label, err)
	}
	digest := sha256.Sum256(canonical)
	return canonical, hex.EncodeToString(digest[:]), nil
}
