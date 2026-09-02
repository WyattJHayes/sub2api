package service

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestCanonicalRequestManifestGoldenHash(t *testing.T) {
	manifest := validRequestManifest()
	canonical, err := CanonicalizeRequestManifest(manifest)
	if err != nil {
		t.Fatalf("CanonicalizeRequestManifest() error = %v", err)
	}

	const wantBytes = `{"interaction_type":"single","max_requests":1,"min_requests":1,"ordinal_policy":"exact","request_slots":[{"allowed_tool_set_sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","expected_request_semantics_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","max_occurrences":1,"ordinal_max":0,"ordinal_min":0,"phase":"primary","required":true,"semantics_mode":"exact","slot_id":"request-0","tool_schema_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}],"schema_version":"radar-request-manifest-v1"}`
	const wantHash = "3ffa6dd8ed87891bd322cbdf26b0f9fbac93136a9dcb067d81d4ee4d6b8727b4"
	if got := string(canonical.Bytes); got != wantBytes {
		t.Errorf("canonical bytes = %s, want %s", got, wantBytes)
	}
	if got := canonical.SHA256; got != wantHash {
		t.Errorf("canonical hash = %s, want %s", got, wantHash)
	}
}

func TestRequestManifestRejectsAmbiguousSlots(t *testing.T) {
	base := validRequestManifest()
	base.InteractionType = "agent"
	base.OrdinalPolicy = "contiguous_bounded"
	base.MinRequests = 2
	base.MaxRequests = 2
	base.RequestSlots = []RequestSlot{
		validRequestSlot("request-0", 0, 1),
		validRequestSlot("request-1", 1, 1),
	}
	if _, err := CanonicalizeRequestManifest(base); err == nil {
		t.Fatal("CanonicalizeRequestManifest() error = nil, want overlapping slot rejection")
	}

	base.RequestSlots = []RequestSlot{
		validRequestSlot("request-0", 0, 0),
		validRequestSlot("request-2", 2, 2),
	}
	if _, err := CanonicalizeRequestManifest(base); err == nil {
		t.Fatal("CanonicalizeRequestManifest() error = nil, want non-contiguous ordinal rejection")
	}

	base = validRequestManifest()
	base.RequestSlots[0].RequestSemanticsPolicySHA256 = strings.Repeat("b", 64)
	if _, err := CanonicalizeRequestManifest(base); err == nil {
		t.Fatal("CanonicalizeRequestManifest() error = nil, want exact and policy hash conflict rejection")
	}
}

func TestPairBindingRejectsUnapprovedTreatmentDifference(t *testing.T) {
	pair := validPairSpec()
	baseline := validSideSpec("baseline", "baseline:comparison-route")
	candidate := validSideSpec("candidate", "candidate:comparison-route")
	candidate.RouteProfileVersion = "route-profile-v2"

	if _, err := BindEvaluationPair(pair, baseline, candidate); err == nil {
		t.Fatal("BindEvaluationPair() error = nil, want unapproved treatment difference rejection")
	}
}

func validRequestManifest() RequestManifest {
	return RequestManifest{
		SchemaVersion:   RequestManifestSchemaV1,
		InteractionType: "single",
		OrdinalPolicy:   "exact",
		MinRequests:     1,
		MaxRequests:     1,
		RequestSlots:    []RequestSlot{validRequestSlot("request-0", 0, 0)},
	}
}

func validRequestSlot(id string, ordinalMin, ordinalMax int) RequestSlot {
	return RequestSlot{
		SlotID:                         id,
		OrdinalMin:                     ordinalMin,
		OrdinalMax:                     ordinalMax,
		Phase:                          "primary",
		Required:                       true,
		SemanticsMode:                  "exact",
		ExpectedRequestSemanticsSHA256: strings.Repeat("a", 64),
		ToolSchemaSHA256:               strings.Repeat("d", 64),
		AllowedToolSetSHA256:           strings.Repeat("e", 64),
		MaxOccurrences:                 1,
	}
}

func validPairSpec() PairSpec {
	return PairSpec{
		DatasetVersionID:              uuid.New(),
		CaseID:                        uuid.New(),
		SampleIndex:                   0,
		RepeatIndex:                   0,
		PromptSHA256:                  strings.Repeat("1", 64),
		ToolSchemaSHA256:              strings.Repeat("2", 64),
		ExpectedRequestManifestID:     uuid.New(),
		ExpectedRequestManifestSHA256: strings.Repeat("3", 64),
		GraderID:                      "exact-grader",
		GraderVersion:                 "v1",
		SamplingPolicy:                "fixed",
		RandomSeed:                    "seed-1",
		Region:                        "cn-north-1",
		Protocol:                      "responses-v1",
		TimeBlock:                     "2026-07-27T00:00:00Z",
		InterleaveOrder:               "baseline-first",
		RetryPolicy:                   "none",
		AllowedTreatmentFields:        []string{"model_config_sha256"},
	}
}

func validSideSpec(side, route string) SideSpec {
	return SideSpec{
		Side:                     side,
		ModelRoute:               route,
		ModelConfigSHA256:        strings.Repeat("4", 64),
		ExpectedModelAlias:       "model-alias",
		ExpectedResolvedModel:    "model-resolved",
		RouteProfileVersion:      "route-profile-v1",
		ProviderParametersSHA256: strings.Repeat("5", 64),
	}
}
