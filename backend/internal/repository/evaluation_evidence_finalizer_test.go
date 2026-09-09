package repository

import (
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestFinalizeRejectsTopLevelRouteThatDoesNotMatchFinalAttempt(t *testing.T) {
	finishedAt := time.Date(2026, 7, 28, 1, 0, 1, 0, time.UTC)
	row := routeEvidenceFinalizationRow{
		envelope: service.RouteEvidenceEnvelope{
			SchemaVersion: service.RouteEvidenceSchemaV1, CanonicalizationVersion: service.RouteEvidenceCanonicalizationV1,
			RouteTraceID: uuid.NewString(), EvaluationRunID: uuid.NewString(), SampleID: uuid.NewString(), AssignmentID: uuid.NewString(),
			RequestManifestID: uuid.NewString(), RequestManifestSHA256: finalizerHash("a"), RequestSlotID: "request-0",
			RequestSemanticsID: uuid.NewString(), RequestSemanticsSHA256: finalizerHash("b"),
			RequestToolSchemaSHA256: finalizerHash("c"), RequestAllowedToolSetSHA256: finalizerHash("d"),
			EvidenceRevision: 1, APIKeyID: 1, RequestID: "request-1", RequestedModel: "route-a",
			ResolvedModel: finalizerStringPointer("model-a"), RouteProfileVersion: "route-v1",
			Provider: finalizerStringPointer("qwen"), ChannelRef: finalizerStringPointer("channel-1"),
			AccountPoolRef: finalizerStringPointer("pool-1"), Region: "default", Attempts: 1,
			TransportStatus: "gateway_failed", BillingStatus: "incomplete",
			StartedAt: service.FormatRouteEvidenceTime(finishedAt.Add(-time.Second)), SigningKeyID: uuid.NewString(),
		},
		fallback: []service.RouteFallbackEntry{{
			Ordinal: 1, DispatchMode: "primary", RouteRuleHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RequestedModel: "route-a", ResolvedModel: "model-a", Provider: "qwen", ChannelRef: "channel-1", AccountPoolRef: "pool-1", Region: "default",
			Outcome: "succeeded", StartedAt: finishedAt.Add(-time.Second), FinishedAt: &finishedAt,
		}},
	}
	row.envelope.Provider = finalizerStringPointer("deepseek")

	_, err := canonicalizeRouteEvidenceRow(row, 1, finishedAt, finishedAt, uuid.New())

	require.ErrorIs(t, err, service.ErrRouteEvidenceFallbackInvalid)
}

func finalizerStringPointer(value string) *string { return &value }
func finalizerHash(character string) string       { return strings.Repeat(character, 64) }
