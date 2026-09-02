package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRadarGate30PairAcceptance(t *testing.T) {
	input, anchor := deterministicTrustedGateFixture(t)
	first, err := service.VerifyTrustedGateManifest(input, anchor)
	require.NoError(t, err)
	require.Equal(t, 30, first.PairCount)

	for run := 0; run < 2; run++ {
		got, verifyErr := service.VerifyTrustedGateManifest(input, anchor)
		require.NoError(t, verifyErr)
		require.Equal(t, first.ManifestSHA256, got.ManifestSHA256)
		require.Equal(t, first.SourceWatermark, got.SourceWatermark)
		require.Equal(t, first.DecisionFingerprint, got.DecisionFingerprint)
	}

	mutations := []struct {
		name   string
		mutate func(*service.TrustedGateManifestInput, *service.TrustedGateTrustAnchor)
	}{
		{"pair_binding", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].PairBindingHash = hash("tampered-pair")
		}},
		{"request_semantics", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].RequestSemanticsHash = hash("tampered-semantics")
		}},
		{"evidence_hash", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].EvidenceHash = hash("tampered-evidence")
		}},
		{"evidence_hmac", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].EvidenceHMAC = strings.Repeat("0", 64)
		}},
		{"signing_key_state", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.SigningKey.StateEpoch++
		}},
		{"assignment_ref", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].AssignmentRef = "assignment-tampered"
		}},
		{"score_head", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].ScoreHeadRef = "score-tampered"
		}},
		{"aggregate_head", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].AggregateHeadRef = "aggregate-tampered"
		}},
		{"reliability_snapshot", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].ReliabilitySnapshotRef = "reliability-tampered"
		}},
		{"policy_head", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].PolicyHeadRef = "policy-tampered"
		}},
		{"baseline_head", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].BaselineHeadRef = "baseline-tampered"
		}},
		{"release_subject", func(v *service.TrustedGateManifestInput, _ *service.TrustedGateTrustAnchor) {
			v.Pairs[0].ReleaseSubjectHash = hash("tampered-subject")
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			candidate := cloneTrustedGateInput(input)
			anchorCopy := anchor
			mutation.mutate(&candidate, &anchorCopy)
			_, err := service.VerifyTrustedGateManifest(candidate, anchorCopy)
			require.Error(t, err)
		})
	}
}

func deterministicTrustedGateFixture(t *testing.T) (service.TrustedGateManifestInput, service.TrustedGateTrustAnchor) {
	t.Helper()
	input := service.TrustedGateManifestInput{
		ReleaseSubjectHash: hash("release-subject"),
		PolicyHeadRef:      "policy-head-v1",
		BaselineHeadRef:    "baseline-head-v1",
		SigningKey: service.TrustedGateSigningKey{
			ID: "signing-key-v1", Status: service.EvidenceSigningKeyActive,
			StateEpoch: 7, Key: []byte(strings.Repeat("k", 32)),
		},
	}
	anchor := service.TrustedGateTrustAnchor{
		ReleaseSubjectHash: input.ReleaseSubjectHash,
		PolicyHeadRef:      input.PolicyHeadRef,
		BaselineHeadRef:    input.BaselineHeadRef,
		SigningKeyID:       input.SigningKey.ID,
		SigningKeyStatus:   input.SigningKey.Status,
		SigningKeyEpoch:    input.SigningKey.StateEpoch,
		Pairs:              make(map[string]service.TrustedGatePairAnchor, 30),
	}
	for i := 0; i < 30; i++ {
		pairID := uuid.NewSHA1(uuid.Nil, []byte("pair:"+string(rune('a'+i)))).String()
		canonicalEvidence := []byte(`{"schema_version":"radar-route-evidence-v1","pair":"` + pairID + `"}`)
		pair := service.TrustedGatePairInput{
			PairID: pairID, PairBindingHash: hash("binding:" + pairID), RequestSemanticsHash: hash("semantics:" + pairID),
			EvidenceBytes: canonicalEvidence, EvidenceHash: hashBytes(canonicalEvidence),
			AssignmentRef: "assignment:" + pairID, ScoreHeadRef: "score:" + pairID,
			AggregateHeadRef: "aggregate:" + pairID, ReliabilitySnapshotRef: "reliability:" + pairID,
			PolicyHeadRef: input.PolicyHeadRef, BaselineHeadRef: input.BaselineHeadRef,
			ReleaseSubjectHash: input.ReleaseSubjectHash,
		}
		pair.EvidenceHMAC = service.SignEvidence(service.RouteEvidenceSchemaV1, canonicalEvidence, input.SigningKey.Key)
		input.Pairs = append(input.Pairs, pair)
		anchor.Pairs[pairID] = service.TrustedGatePairAnchor{
			PairBindingHash: pair.PairBindingHash, RequestSemanticsHash: pair.RequestSemanticsHash,
			EvidenceHash: pair.EvidenceHash, AssignmentRef: pair.AssignmentRef, ScoreHeadRef: pair.ScoreHeadRef,
			AggregateHeadRef: pair.AggregateHeadRef, ReliabilitySnapshotRef: pair.ReliabilitySnapshotRef,
			PolicyHeadRef: pair.PolicyHeadRef, BaselineHeadRef: pair.BaselineHeadRef,
			ReleaseSubjectHash: pair.ReleaseSubjectHash,
		}
	}
	return input, anchor
}

func cloneTrustedGateInput(input service.TrustedGateManifestInput) service.TrustedGateManifestInput {
	clone := input
	clone.Pairs = append([]service.TrustedGatePairInput(nil), input.Pairs...)
	clone.SigningKey.Key = append([]byte(nil), input.SigningKey.Key...)
	return clone
}

func hash(value string) string { return hashBytes([]byte(value)) }

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
