//go:build unit

package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRouteEvidenceEnvelopeGoldenBytesHashAndHMAC(t *testing.T) {
	envelope := routeEvidenceEnvelopeFixture()
	canonical, err := CanonicalizeRouteEvidenceEnvelope(envelope)
	require.NoError(t, err)
	const wantBytes = `{"account_pool_ref":"account_redacted","api_key_id":41,"assignment_id":"018f4f20-3d12-7e50-9000-000000000003","attempts":2,"billed_amount":"0.00012345","billing_status":"complete","canonicalization_version":"rfc8785-v1","channel_ref":"channel_redacted","error_code":null,"evaluation_run_id":"018f4f20-3d12-7e50-9000-000000000001","evidence_revision":3,"fallback_chain":[{"account_pool_ref":"account_1","attempt_index":1,"channel_ref":"channel_1","dispatch_mode":"primary","error_code":"429","finished_at":"2026-07-28T01:02:01.500000Z","outcome":"upstream_failed","parent_attempt_index":null,"provider":"qwen","region":"cn-east","requested_model":"route-a","resolved_model":"model-a","route_rule_hash":"1111111111111111111111111111111111111111111111111111111111111111","started_at":"2026-07-28T01:02:01.000000Z"},{"account_pool_ref":"account_2","attempt_index":2,"channel_ref":"channel_2","dispatch_mode":"fallback","error_code":null,"finished_at":"2026-07-28T01:02:03.000000Z","outcome":"succeeded","parent_attempt_index":1,"provider":"qwen","region":"cn-east","requested_model":"route-a","resolved_model":"model-a","route_rule_hash":"2222222222222222222222222222222222222222222222222222222222222222","started_at":"2026-07-28T01:02:01.500001Z"}],"finish_reason":"stop","finished_at":"2026-07-28T01:02:03.000000Z","gateway_image_digest":"sub2api-gateway@sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff","incomplete_reason":null,"input_tokens":11,"latency_ms":2000,"lease_epoch":7,"output_tokens":7,"provider":"qwen","region":"cn-east","request_allowed_tool_set_sha256":"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee","request_id":"request-1","request_manifest_id":"018f4f20-3d12-7e50-9000-000000000004","request_manifest_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","request_ordinal":0,"request_semantics_id":"018f4f20-3d12-7e50-9000-000000000005","request_semantics_policy_sha256":"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","request_semantics_sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","request_slot_id":"request-0","request_tool_schema_sha256":"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd","requested_model":"route-a","resolved_model":"model-a","route_profile_version":"route-v1","route_trace_id":"018f4f20-3d12-7e50-9000-000000000010","sample_id":"018f4f20-3d12-7e50-9000-000000000002","schema_version":"radar-route-evidence-v1","sealed_at":"2026-07-28T01:02:03.456789Z","signing_key_id":"018f4f20-3d12-7e50-9000-000000000006","started_at":"2026-07-28T01:02:01.000000Z","terminal_at":"2026-07-28T01:02:03.456789Z","transport_status":"succeeded","ttft_ms":120}`
	correctedWantBytes := strings.NewReplacer(
		`"account_pool_ref":"account_redacted","api_key_id"`, `"account_pool_ref":"account_2","api_key_id"`,
		`"channel_ref":"channel_redacted","error_code"`, `"channel_ref":"channel_2","error_code"`,
	).Replace(wantBytes)
	require.Equal(t, correctedWantBytes, string(canonical.Bytes))
	require.Equal(t, "d35bbe7efad5ca5055bc729447c3c009e43a2306d6fea02f1ebb88080f7ba492", canonical.SHA256)
	require.Equal(t, "db59ae86ac8b099fe53742317344ee3230d5744c29820575653f693cb7236fda", SignEvidence(envelope.SchemaVersion, canonical.Bytes, []byte(strings.Repeat("k", 32))))
}

func TestFinalizeRejectsNonContiguousFallbackAttempts(t *testing.T) {
	envelope := routeEvidenceEnvelopeFixture()
	envelope.FallbackChain[0].AttemptIndex = 2

	_, err := CanonicalizeRouteEvidenceEnvelope(envelope)
	require.ErrorIs(t, err, ErrRouteEvidenceFallbackInvalid)
}

func TestRouteEvidenceEnvelopeRejectsFinalRouteDifferentFromLastSuccessfulAttempt(t *testing.T) {
	envelope := routeEvidenceEnvelopeFixture()
	envelope.Provider = envelopeStringPointer("deepseek")

	_, err := CanonicalizeRouteEvidenceEnvelope(envelope)

	require.ErrorIs(t, err, ErrRouteEvidenceFallbackInvalid)
}

func TestRouteEvidenceEnvelopeSucceededRequiresCompleteUsage(t *testing.T) {
	envelope := routeEvidenceEnvelopeFixture()
	envelope.InputTokens = nil

	_, err := CanonicalizeRouteEvidenceEnvelope(envelope)

	require.ErrorIs(t, err, ErrRouteEvidenceIncomplete)
}

func TestRouteEvidenceEnvelopeRejectsZeroAttemptsOutsideControlledTerminalization(t *testing.T) {
	envelope := routeEvidenceEnvelopeFixture()
	envelope.Attempts = 0
	envelope.FallbackChain = nil
	envelope.TransportStatus = "upstream_failed"

	_, err := CanonicalizeRouteEvidenceEnvelope(envelope)

	require.ErrorIs(t, err, ErrRouteEvidenceFallbackInvalid)

	envelope.TransportStatus = "protocol_failed"
	_, err = CanonicalizeRouteEvidenceEnvelope(envelope)
	require.NoError(t, err)
}

func TestRouteEvidenceEnvelopeComparesFallbackStartWithParentAttemptFinish(t *testing.T) {
	envelope := routeEvidenceEnvelopeFixture()
	parent := 1
	envelope.FallbackChain[1].DispatchMode = "hedge"
	envelope.FallbackChain[1].Outcome = "upstream_failed"
	envelope.FallbackChain[1].StartedAt = "2026-07-28T01:02:01.100000Z"
	envelope.FallbackChain[1].FinishedAt = "2026-07-28T01:02:02.500000Z"
	envelope.FallbackChain = append(envelope.FallbackChain, RouteEvidenceAttemptEnvelope{
		AttemptIndex: 3, ParentAttemptIndex: &parent, DispatchMode: "fallback",
		RouteRuleHash: strings.Repeat("3", 64), RequestedModel: "route-a", ResolvedModel: "model-a",
		Provider: "qwen", ChannelRef: "channel_2", AccountPoolRef: "account_2", Region: "cn-east",
		Outcome: "succeeded", StartedAt: "2026-07-28T01:02:01.500001Z", FinishedAt: "2026-07-28T01:02:03.000000Z",
	})
	envelope.Attempts = 3

	_, err := CanonicalizeRouteEvidenceEnvelope(envelope)

	require.NoError(t, err)
}

func routeEvidenceEnvelopeFixture() RouteEvidenceEnvelope {
	parent := 1
	return RouteEvidenceEnvelope{
		SchemaVersion: "radar-route-evidence-v1", CanonicalizationVersion: "rfc8785-v1",
		RouteTraceID: "018f4f20-3d12-7e50-9000-000000000010", EvaluationRunID: "018f4f20-3d12-7e50-9000-000000000001",
		SampleID: "018f4f20-3d12-7e50-9000-000000000002", AssignmentID: "018f4f20-3d12-7e50-9000-000000000003",
		RequestOrdinal: 0, LeaseEpoch: 7, RequestManifestID: "018f4f20-3d12-7e50-9000-000000000004",
		RequestManifestSHA256: strings.Repeat("a", 64), RequestSlotID: "request-0",
		RequestSemanticsID: "018f4f20-3d12-7e50-9000-000000000005", RequestSemanticsSHA256: strings.Repeat("b", 64),
		RequestSemanticsPolicySHA256: envelopeStringPointer(strings.Repeat("c", 64)), RequestToolSchemaSHA256: strings.Repeat("d", 64),
		RequestAllowedToolSetSHA256: strings.Repeat("e", 64), EvidenceRevision: 3, APIKeyID: 41,
		RequestID: "request-1", RequestedModel: "route-a", ResolvedModel: envelopeStringPointer("model-a"),
		RouteProfileVersion: "route-v1", GatewayImageDigest: "sub2api-gateway@sha256:" + strings.Repeat("f", 64),
		Provider: envelopeStringPointer("qwen"), ChannelRef: envelopeStringPointer("channel_2"), AccountPoolRef: envelopeStringPointer("account_2"),
		Region: "cn-east", Attempts: 2,
		FallbackChain: []RouteEvidenceAttemptEnvelope{
			{AttemptIndex: 1, DispatchMode: "primary", RouteRuleHash: strings.Repeat("1", 64), RequestedModel: "route-a", ResolvedModel: "model-a", Provider: "qwen", ChannelRef: "channel_1", AccountPoolRef: "account_1", Region: "cn-east", Outcome: "upstream_failed", ErrorCode: envelopeStringPointer("429"), StartedAt: "2026-07-28T01:02:01.000000Z", FinishedAt: "2026-07-28T01:02:01.500000Z"},
			{AttemptIndex: 2, ParentAttemptIndex: &parent, DispatchMode: "fallback", RouteRuleHash: strings.Repeat("2", 64), RequestedModel: "route-a", ResolvedModel: "model-a", Provider: "qwen", ChannelRef: "channel_2", AccountPoolRef: "account_2", Region: "cn-east", Outcome: "succeeded", StartedAt: "2026-07-28T01:02:01.500001Z", FinishedAt: "2026-07-28T01:02:03.000000Z"},
		},
		TransportStatus: "succeeded", FinishReason: envelopeStringPointer("stop"), InputTokens: envelopeIntPointer(11), OutputTokens: envelopeIntPointer(7),
		TTFTMS: envelopeIntPointer(120), LatencyMS: envelopeIntPointer(2000), BillingStatus: "complete", BilledAmount: envelopeStringPointer("0.00012345"),
		StartedAt: "2026-07-28T01:02:01.000000Z", FinishedAt: envelopeStringPointer("2026-07-28T01:02:03.000000Z"),
		TerminalAt: "2026-07-28T01:02:03.456789Z", SealedAt: "2026-07-28T01:02:03.456789Z",
		SigningKeyID: "018f4f20-3d12-7e50-9000-000000000006",
	}
}

func envelopeStringPointer(value string) *string { return &value }
func envelopeIntPointer(value int) *int          { return &value }
