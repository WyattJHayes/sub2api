package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
)

var (
	ErrTrustedGatePairCount        = errors.New("trusted gate pair count is incomplete")
	ErrTrustedGatePairBinding      = errors.New("trusted gate pair binding changed")
	ErrTrustedGateRequestSemantics = errors.New("trusted gate request semantics changed")
	ErrTrustedGateEvidence         = errors.New("trusted gate evidence integrity failed")
	ErrTrustedGateSigningKey       = errors.New("trusted gate signing key state changed")
	ErrTrustedGateSourceReference  = errors.New("trusted gate source reference changed")
	ErrTrustedGateReleaseSubject   = errors.New("trusted gate release subject changed")
	ErrTrustedGatePolicyHead       = errors.New("trusted gate policy head changed")
	ErrTrustedGateBaselineHead     = errors.New("trusted gate baseline head changed")
)

// TrustedGateSigningKey contains the verifier-only key material for one
// repeatable read. The key is never included in the manifest or its hashes.
type TrustedGateSigningKey struct {
	ID         string
	Status     EvidenceSigningKeyStatus
	StateEpoch int64
	Key        []byte
}

type TrustedGatePairInput struct {
	PairID                 string
	PairBindingHash        string
	RequestSemanticsHash   string
	EvidenceBytes          []byte
	EvidenceHash           string
	EvidenceHMAC           string
	AssignmentRef          string
	ScoreHeadRef           string
	AggregateHeadRef       string
	ReliabilitySnapshotRef string
	PolicyHeadRef          string
	BaselineHeadRef        string
	ReleaseSubjectHash     string
}

type TrustedGateManifestInput struct {
	Pairs              []TrustedGatePairInput
	ReleaseSubjectHash string
	PolicyHeadRef      string
	BaselineHeadRef    string
	SigningKey         TrustedGateSigningKey
}

type TrustedGatePairAnchor struct {
	PairBindingHash        string
	RequestSemanticsHash   string
	EvidenceHash           string
	AssignmentRef          string
	ScoreHeadRef           string
	AggregateHeadRef       string
	ReliabilitySnapshotRef string
	PolicyHeadRef          string
	BaselineHeadRef        string
	ReleaseSubjectHash     string
}

type TrustedGateTrustAnchor struct {
	Pairs              map[string]TrustedGatePairAnchor
	ReleaseSubjectHash string
	PolicyHeadRef      string
	BaselineHeadRef    string
	SigningKeyID       string
	SigningKeyStatus   EvidenceSigningKeyStatus
	SigningKeyEpoch    int64
}

type TrustedGateManifest struct {
	PairCount           int
	ManifestSHA256      string
	SourceWatermark     string
	DecisionFingerprint string
}

type trustedGateCanonicalPair struct {
	PairID                 string `json:"pair_id"`
	PairBindingHash        string `json:"pair_binding_hash"`
	RequestSemanticsHash   string `json:"request_semantics_hash"`
	EvidenceHash           string `json:"evidence_hash"`
	EvidenceHMAC           string `json:"evidence_hmac"`
	AssignmentRef          string `json:"assignment_ref"`
	ScoreHeadRef           string `json:"score_head_ref"`
	AggregateHeadRef       string `json:"aggregate_head_ref"`
	ReliabilitySnapshotRef string `json:"reliability_snapshot_ref"`
	PolicyHeadRef          string `json:"policy_head_ref"`
	BaselineHeadRef        string `json:"baseline_head_ref"`
	ReleaseSubjectHash     string `json:"release_subject_hash"`
}

type trustedGateCanonicalManifest struct {
	Version            string                     `json:"version"`
	ReleaseSubjectHash string                     `json:"release_subject_hash"`
	PolicyHeadRef      string                     `json:"policy_head_ref"`
	BaselineHeadRef    string                     `json:"baseline_head_ref"`
	SigningKeyID       string                     `json:"signing_key_id"`
	SigningKeyEpoch    int64                      `json:"signing_key_epoch"`
	Pairs              []trustedGateCanonicalPair `json:"pairs"`
}

type trustedGateSourceWatermark struct {
	Version            string                     `json:"version"`
	ManifestSHA256     string                     `json:"manifest_sha256"`
	ReleaseSubjectHash string                     `json:"release_subject_hash"`
	PolicyHeadRef      string                     `json:"policy_head_ref"`
	BaselineHeadRef    string                     `json:"baseline_head_ref"`
	SigningKeyID       string                     `json:"signing_key_id"`
	SigningKeyEpoch    int64                      `json:"signing_key_epoch"`
	Pairs              []trustedGateCanonicalPair `json:"pairs"`
}

func VerifyTrustedGateManifest(input TrustedGateManifestInput, anchor TrustedGateTrustAnchor) (TrustedGateManifest, error) {
	if len(input.Pairs) == 0 || len(input.Pairs) != len(anchor.Pairs) {
		return TrustedGateManifest{}, ErrTrustedGatePairCount
	}
	if input.ReleaseSubjectHash != anchor.ReleaseSubjectHash {
		return TrustedGateManifest{}, ErrTrustedGateReleaseSubject
	}
	if input.PolicyHeadRef != anchor.PolicyHeadRef {
		return TrustedGateManifest{}, ErrTrustedGatePolicyHead
	}
	if input.BaselineHeadRef != anchor.BaselineHeadRef {
		return TrustedGateManifest{}, ErrTrustedGateBaselineHead
	}
	if input.SigningKey.ID != anchor.SigningKeyID || input.SigningKey.Status != anchor.SigningKeyStatus || input.SigningKey.StateEpoch != anchor.SigningKeyEpoch {
		return TrustedGateManifest{}, ErrTrustedGateSigningKey
	}
	if input.SigningKey.Status != EvidenceSigningKeyActive && input.SigningKey.Status != EvidenceSigningKeyVerifyOnly {
		return TrustedGateManifest{}, ErrTrustedGateSigningKey
	}
	if input.SigningKey.StateEpoch <= 0 || len(input.SigningKey.Key) < 32 || strings.TrimSpace(input.SigningKey.ID) == "" {
		return TrustedGateManifest{}, ErrTrustedGateSigningKey
	}

	canonicalPairs := make([]trustedGateCanonicalPair, 0, len(input.Pairs))
	seen := make(map[string]struct{}, len(input.Pairs))
	for _, pair := range input.Pairs {
		if strings.TrimSpace(pair.PairID) == "" {
			return TrustedGateManifest{}, ErrTrustedGatePairCount
		}
		if _, exists := seen[pair.PairID]; exists {
			return TrustedGateManifest{}, fmt.Errorf("%w: duplicate pair %s", ErrTrustedGatePairCount, pair.PairID)
		}
		seen[pair.PairID] = struct{}{}
		expected, exists := anchor.Pairs[pair.PairID]
		if !exists {
			return TrustedGateManifest{}, fmt.Errorf("%w: unknown pair %s", ErrTrustedGatePairCount, pair.PairID)
		}
		if pair.PairBindingHash != expected.PairBindingHash {
			return TrustedGateManifest{}, ErrTrustedGatePairBinding
		}
		if pair.RequestSemanticsHash != expected.RequestSemanticsHash {
			return TrustedGateManifest{}, ErrTrustedGateRequestSemantics
		}
		if pair.AssignmentRef != expected.AssignmentRef || pair.ScoreHeadRef != expected.ScoreHeadRef || pair.AggregateHeadRef != expected.AggregateHeadRef || pair.ReliabilitySnapshotRef != expected.ReliabilitySnapshotRef {
			return TrustedGateManifest{}, ErrTrustedGateSourceReference
		}
		if pair.PolicyHeadRef != expected.PolicyHeadRef {
			return TrustedGateManifest{}, ErrTrustedGatePolicyHead
		}
		if pair.BaselineHeadRef != expected.BaselineHeadRef {
			return TrustedGateManifest{}, ErrTrustedGateBaselineHead
		}
		if pair.ReleaseSubjectHash != expected.ReleaseSubjectHash || pair.ReleaseSubjectHash != input.ReleaseSubjectHash {
			return TrustedGateManifest{}, ErrTrustedGateReleaseSubject
		}
		if !isTrustedGateHash(pair.PairBindingHash) || !isTrustedGateHash(pair.RequestSemanticsHash) || !isTrustedGateHash(pair.EvidenceHash) {
			return TrustedGateManifest{}, ErrTrustedGateEvidence
		}
		if strings.TrimSpace(pair.AssignmentRef) == "" || strings.TrimSpace(pair.ScoreHeadRef) == "" || strings.TrimSpace(pair.AggregateHeadRef) == "" || strings.TrimSpace(pair.ReliabilitySnapshotRef) == "" {
			return TrustedGateManifest{}, ErrTrustedGateSourceReference
		}
		computedEvidenceHash := hashTrustedGateBytes(pair.EvidenceBytes)
		if computedEvidenceHash != pair.EvidenceHash || pair.EvidenceHash != expected.EvidenceHash || !isTrustedGateHash(pair.EvidenceHMAC) {
			return TrustedGateManifest{}, ErrTrustedGateEvidence
		}
		wantHMAC := SignEvidence(RouteEvidenceSchemaV1, pair.EvidenceBytes, input.SigningKey.Key)
		if !hmac.Equal([]byte(wantHMAC), []byte(pair.EvidenceHMAC)) {
			return TrustedGateManifest{}, ErrTrustedGateEvidence
		}
		canonicalPairs = append(canonicalPairs, trustedGateCanonicalPair{
			PairID: pair.PairID, PairBindingHash: pair.PairBindingHash, RequestSemanticsHash: pair.RequestSemanticsHash,
			EvidenceHash: pair.EvidenceHash, EvidenceHMAC: pair.EvidenceHMAC, AssignmentRef: pair.AssignmentRef,
			ScoreHeadRef: pair.ScoreHeadRef, AggregateHeadRef: pair.AggregateHeadRef,
			ReliabilitySnapshotRef: pair.ReliabilitySnapshotRef, PolicyHeadRef: pair.PolicyHeadRef,
			BaselineHeadRef: pair.BaselineHeadRef, ReleaseSubjectHash: pair.ReleaseSubjectHash,
		})
	}
	sort.Slice(canonicalPairs, func(i, j int) bool { return canonicalPairs[i].PairID < canonicalPairs[j].PairID })
	manifestValue := trustedGateCanonicalManifest{
		Version: "radar-trusted-gate-manifest-v1", ReleaseSubjectHash: input.ReleaseSubjectHash,
		PolicyHeadRef: input.PolicyHeadRef, BaselineHeadRef: input.BaselineHeadRef,
		SigningKeyID: input.SigningKey.ID, SigningKeyEpoch: input.SigningKey.StateEpoch, Pairs: canonicalPairs,
	}
	manifestBytes, err := canonicalJSON(manifestValue)
	if err != nil {
		return TrustedGateManifest{}, fmt.Errorf("canonicalize trusted gate manifest: %w", err)
	}
	manifestHash := hashTrustedGateBytes(manifestBytes)
	watermarkValue := trustedGateSourceWatermark{
		Version: "radar-trusted-gate-watermark-v1", ManifestSHA256: manifestHash,
		ReleaseSubjectHash: input.ReleaseSubjectHash, PolicyHeadRef: input.PolicyHeadRef,
		BaselineHeadRef: input.BaselineHeadRef, SigningKeyID: input.SigningKey.ID,
		SigningKeyEpoch: input.SigningKey.StateEpoch, Pairs: canonicalPairs,
	}
	watermarkBytes, err := canonicalJSON(watermarkValue)
	if err != nil {
		return TrustedGateManifest{}, fmt.Errorf("canonicalize trusted gate watermark: %w", err)
	}
	watermark := hashTrustedGateBytes(watermarkBytes)
	decisionFingerprint := hashTrustedGateBytes([]byte("radar-trusted-gate-decision-v1\x00" + manifestHash + "\x00" + watermark))
	return TrustedGateManifest{PairCount: len(canonicalPairs), ManifestSHA256: manifestHash, SourceWatermark: watermark, DecisionFingerprint: decisionFingerprint}, nil
}

func canonicalJSON(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	contract, err := jsoncanonicalizer.Transform(raw)
	if err != nil {
		return nil, err
	}
	return contract, nil
}

func isTrustedGateHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func hashTrustedGateBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
