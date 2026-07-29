package service

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const contractHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestCanonicalRequestManifestGoldenHash(t *testing.T) {
	manifest := RequestManifest{
		SchemaVersion:   RequestManifestSchemaVersion,
		InteractionType: InteractionSingle,
		OrdinalPolicy:   OrdinalPolicyExact,
		MinRequests:     1,
		MaxRequests:     1,
		RequestSlots: []RequestSlot{{
			SlotID:                         "slot-0",
			OrdinalMin:                     0,
			OrdinalMax:                     0,
			Phase:                          "prompt",
			Required:                       true,
			SemanticsMode:                  SemanticsModeExact,
			ExpectedRequestSemanticsSHA256: contractHash,
			ToolSchemaSHA256:               contractHash,
			AllowedToolSetSHA256:           contractHash,
			MaxOccurrences:                 1,
		}},
	}

	canonical, digest, err := CanonicalRequestManifest(manifest)
	require.NoError(t, err)
	require.Equal(t, `{"interaction_type":"single","max_requests":1,"min_requests":1,"ordinal_policy":"exact","request_slots":[{"allowed_tool_set_sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","expected_request_semantics_sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","max_occurrences":1,"ordinal_max":0,"ordinal_min":0,"phase":"prompt","required":true,"semantics_mode":"exact","slot_id":"slot-0","tool_schema_sha256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}],"schema_version":"radar-request-manifest-v1"}`, string(canonical))
	require.Equal(t, "df0786b5e423c19bcdc94925aee52142ce6314267ae0372207f37e2bfddeacfd", digest)
}

func TestCanonicalRequestManifestRejectsAmbiguousSlots(t *testing.T) {
	base := RequestManifest{
		SchemaVersion:   RequestManifestSchemaVersion,
		InteractionType: InteractionMultiTurn,
		OrdinalPolicy:   OrdinalPolicyContiguousBounded,
		MinRequests:     1,
		MaxRequests:     2,
		RequestSlots: []RequestSlot{
			{SlotID: "a", OrdinalMin: 0, OrdinalMax: 1, SemanticsMode: SemanticsModeExact, ExpectedRequestSemanticsSHA256: contractHash, MaxOccurrences: 1},
			{SlotID: "b", OrdinalMin: 1, OrdinalMax: 1, SemanticsMode: SemanticsModeExact, ExpectedRequestSemanticsSHA256: contractHash, MaxOccurrences: 1},
		},
	}
	_, _, err := CanonicalRequestManifest(base)
	require.ErrorContains(t, err, "overlap")

	base.RequestSlots[1].OrdinalMin = 3
	base.RequestSlots[1].OrdinalMax = 3
	_, _, err = CanonicalRequestManifest(base)
	require.ErrorContains(t, err, "ordinal")
}

func TestCanonicalRequestManifestRejectsExactAndPolicyHashTogether(t *testing.T) {
	manifest := RequestManifest{
		SchemaVersion:   RequestManifestSchemaVersion,
		InteractionType: InteractionSingle,
		OrdinalPolicy:   OrdinalPolicyExact,
		MinRequests:     1,
		MaxRequests:     1,
		RequestSlots: []RequestSlot{{
			SlotID:                         "slot-0",
			OrdinalMin:                     0,
			OrdinalMax:                     0,
			SemanticsMode:                  SemanticsModeExact,
			ExpectedRequestSemanticsSHA256: contractHash,
			RequestSemanticsPolicySHA256:   contractHash,
			MaxOccurrences:                 1,
		}},
	}
	_, _, err := CanonicalRequestManifest(manifest)
	require.ErrorContains(t, err, "exact semantics")
}

func TestPairBindingRejectsUnapprovedTreatmentDifference(t *testing.T) {
	pair := validPairSpec(nil)
	baseline := validSideSpec("baseline:route", contractHash, "base")
	candidate := validSideSpec("candidate:route", strings.Repeat("f", 64), "base")

	_, err := BuildPairBinding(pair, baseline, candidate)
	require.ErrorContains(t, err, "model_config_sha256")
}

func TestPairBindingAllowsApprovedTreatmentDifference(t *testing.T) {
	pair := validPairSpec([]string{"model_config_sha256", "expected_model_alias", "expected_resolved_model"})
	baseline := validSideSpec("baseline:route", contractHash, "base")
	candidate := validSideSpec("candidate:route", strings.Repeat("f", 64), "candidate")

	binding, err := BuildPairBinding(pair, baseline, candidate)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, binding.ID)
	require.Len(t, binding.PairSpecHash, 64)
	require.Len(t, binding.BindingHash, 64)
	_, err = hex.DecodeString(binding.BindingHash)
	require.NoError(t, err)
}

func TestCanonicalPairSpecSortsTreatmentAllowlist(t *testing.T) {
	pair := validPairSpec([]string{"expected_resolved_model", "model_config_sha256", "expected_model_alias"})
	_, firstHash, err := CanonicalPairSpec(pair)
	require.NoError(t, err)
	pair.AllowedTreatmentFields = []string{"model_config_sha256", "expected_model_alias", "expected_resolved_model"}
	_, secondHash, err := CanonicalPairSpec(pair)
	require.NoError(t, err)
	require.Equal(t, firstHash, secondHash)
}

func validPairSpec(allowed []string) PairSpec {
	return PairSpec{
		ID:                            uuid.New(),
		DatasetVersionID:              uuid.New(),
		CaseID:                        uuid.New(),
		SampleIndex:                   0,
		RepeatIndex:                   0,
		PromptSHA256:                  contractHash,
		ToolSchemaSHA256:              contractHash,
		ExpectedRequestManifestID:     uuid.New(),
		ExpectedRequestManifestSHA256: contractHash,
		GraderID:                      "grader",
		GraderVersion:                 "v1",
		SamplingPolicy:                "temperature=0",
		RandomSeed:                    42,
		Region:                        "us-east",
		Protocol:                      "openai-chat",
		TimeBlock:                     "2026-07-29T00:00Z",
		InterleaveOrder:               "round_robin",
		RetryPolicy:                   "same-route-once",
		AllowedTreatmentFields:        allowed,
	}
}

func validSideSpec(route, configHash, alias string) SideSpec {
	return SideSpec{
		ID:                       uuid.New(),
		SampleID:                 uuid.New(),
		Side:                     strings.Split(route, ":")[0],
		ModelRoute:               route,
		ModelConfigSHA256:        configHash,
		ExpectedModelAlias:       alias,
		ExpectedResolvedModel:    alias + "-resolved",
		RouteProfileVersion:      "route-v1",
		ProviderParametersSHA256: contractHash,
	}
}
