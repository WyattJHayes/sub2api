//go:build unit

package service

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequestSemanticsGoldenHash(t *testing.T) {
	semantics := RequestSemantics{
		SchemaVersion:       RequestSemanticsSchemaV1,
		InteractionType:     "multi_turn",
		SlotID:              "follow-up",
		RequestOrdinal:      1,
		Phase:               "follow_up",
		MessageRoleSequence: []string{"user", "assistant"},
		ContentPartTypes:    [][]string{{"text"}, {"text", "tool_call"}},
		PromptHash:          strings.Repeat("b", 64),
		ToolSchemaHash:      strings.Repeat("e", 64),
		ProvidedToolSetHash: strings.Repeat("c", 64),
		ToolChoicePolicy:    "auto",
		SamplingPolicyHash:  strings.Repeat("d", 64),
		PreviousEvidence: []EvidenceRef{{
			RouteTraceID:   "018f4f20-3d12-7e50-9000-000000000010",
			RequestOrdinal: 0,
			PayloadHash:    strings.Repeat("a", 64),
		}},
	}

	canonical, err := CanonicalizeRequestSemantics(semantics)
	require.NoError(t, err)
	const wantBytes = `{"content_part_types":[["text"],["text","tool_call"]],"interaction_type":"multi_turn","message_role_sequence":["user","assistant"],"phase":"follow_up","previous_evidence_refs":[{"payload_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","request_ordinal":0,"route_trace_id":"018f4f20-3d12-7e50-9000-000000000010"}],"prompt_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","provided_tool_set_hash":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","request_ordinal":1,"sampling_policy_hash":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","schema_version":"radar-request-semantics-v1","slot_id":"follow-up","tool_choice_policy":"auto","tool_schema_hash":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}`
	require.Equal(t, wantBytes, string(canonical.Bytes))
	require.Equal(t, "37ecff60aa4906f9b27e5fbfd7996e10ebd65d834ce87d0d52daa9d92e51f226", canonical.SHA256)
}

func TestRequestSemanticsNormalizesEmptyCollections(t *testing.T) {
	semantics := validRequestSemantics()
	semantics.PreviousEvidence = nil

	canonical, err := CanonicalizeRequestSemantics(semantics)
	require.NoError(t, err)
	require.Contains(t, string(canonical.Bytes), `"previous_evidence_refs":[]`)
	require.NotContains(t, string(canonical.Bytes), `"previous_evidence_refs":null`)
}

func TestCreateOpenRejectsAdapterPolicyWithoutRegisteredVerifier(t *testing.T) {
	registry := NewRequestSemanticsVerifierRegistry()
	canonical, err := CanonicalizeRequestSemantics(validRequestSemantics())
	require.NoError(t, err)

	err = registry.Verify(context.Background(), strings.Repeat("f", 64), canonical)
	require.ErrorIs(t, err, ErrRequestSemanticsVerifierNotRegistered)
}

func TestDeriveSingleRequestSemanticsExcludesModelTreatment(t *testing.T) {
	semantics, err := DeriveSingleRequestSemantics([]byte(`{"model":"route-a-candidate","input":"ping"}`))
	require.NoError(t, err)
	require.Equal(t, "4b8c5bd24500a5df35468b195bc3181fdd9c55380513107ad476740b3a9d81a8", semantics.PromptHash)
	require.Equal(t, "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a", semantics.ToolSchemaHash)
	require.Equal(t, semantics.ToolSchemaHash, semantics.ProvidedToolSetHash)
	require.Equal(t, []string{"user"}, semantics.MessageRoleSequence)
	require.Equal(t, [][]string{{"text"}}, semantics.ContentPartTypes)
}

func validRequestSemantics() RequestSemantics {
	return RequestSemantics{
		SchemaVersion:       RequestSemanticsSchemaV1,
		InteractionType:     "single",
		SlotID:              "request-0",
		RequestOrdinal:      0,
		Phase:               "primary",
		MessageRoleSequence: []string{"user"},
		ContentPartTypes:    [][]string{{"text"}},
		PromptHash:          strings.Repeat("1", 64),
		ToolSchemaHash:      strings.Repeat("2", 64),
		ProvidedToolSetHash: strings.Repeat("3", 64),
		ToolChoicePolicy:    "auto",
		SamplingPolicyHash:  strings.Repeat("4", 64),
		PreviousEvidence:    []EvidenceRef{},
	}
}
