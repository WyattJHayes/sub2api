package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	RouteEvidenceSchemaV1           = "radar-route-evidence-v1"
	RouteEvidenceCanonicalizationV1 = "rfc8785-v1"
	routeEvidenceTimeLayout         = "2006-01-02T15:04:05.000000Z"
)

var (
	ErrRouteEvidenceFallbackInvalid  = errors.New("route evidence fallback chain is invalid")
	ErrRouteEvidenceIncomplete       = errors.New("route evidence is incomplete")
	ErrEvidenceSigningKeyUnavailable = errors.New("evidence signing key is unavailable")
	ErrEvidenceSigningKeyRevoked     = errors.New("evidence signing key is revoked")
	ErrEvidenceSigningKeyConflict    = errors.New("evidence signing key state changed")
	ErrEvidenceSigningKeyTransition  = errors.New("evidence signing key transition is invalid")
	ErrRouteEvidenceSealedConflict   = errors.New("sealed route evidence does not match its signed envelope")
	eightDecimalAmountPattern        = regexp.MustCompile(`^(0|[1-9][0-9]*)\.[0-9]{8}$`)
)

type EvidenceSigningKeyStatus string

const (
	EvidenceSigningKeyActive     EvidenceSigningKeyStatus = "active"
	EvidenceSigningKeyVerifyOnly EvidenceSigningKeyStatus = "verify_only"
	EvidenceSigningKeyRevoked    EvidenceSigningKeyStatus = "revoked"
)

type EvidenceSigningKeyRecord struct {
	ID           uuid.UUID
	KeyReference string
	Status       EvidenceSigningKeyStatus
	StateEpoch   int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	RevokedAt    *time.Time
}

type RotateEvidenceSigningKeyInput struct {
	ID                       uuid.UUID
	KeyReference             string
	ExpectedActiveKeyID      uuid.UUID
	ExpectedActiveStateEpoch int64
}

type TransitionEvidenceSigningKeyInput struct {
	ID                 uuid.UUID
	ExpectedStateEpoch int64
	Status             EvidenceSigningKeyStatus
}

type EvidenceSigningKeyResolver interface {
	ResolveEvidenceSigningKey(context.Context, string) ([]byte, error)
}

type EvidenceSigningKeyResolverFunc func(context.Context, string) ([]byte, error)

func (f EvidenceSigningKeyResolverFunc) ResolveEvidenceSigningKey(ctx context.Context, reference string) ([]byte, error) {
	return f(ctx, reference)
}

type RouteEvidenceAttemptEnvelope struct {
	AttemptIndex       int     `json:"attempt_index"`
	ParentAttemptIndex *int    `json:"parent_attempt_index"`
	DispatchMode       string  `json:"dispatch_mode"`
	RouteRuleHash      string  `json:"route_rule_hash"`
	RequestedModel     string  `json:"requested_model"`
	ResolvedModel      string  `json:"resolved_model"`
	Provider           string  `json:"provider"`
	ChannelRef         string  `json:"channel_ref"`
	AccountPoolRef     string  `json:"account_pool_ref"`
	Region             string  `json:"region"`
	Outcome            string  `json:"outcome"`
	ErrorCode          *string `json:"error_code"`
	StartedAt          string  `json:"started_at"`
	FinishedAt         string  `json:"finished_at"`
}

type RouteEvidenceEnvelope struct {
	SchemaVersion                string                         `json:"schema_version"`
	CanonicalizationVersion      string                         `json:"canonicalization_version"`
	RouteTraceID                 string                         `json:"route_trace_id"`
	EvaluationRunID              string                         `json:"evaluation_run_id"`
	SampleID                     string                         `json:"sample_id"`
	AssignmentID                 string                         `json:"assignment_id"`
	RequestOrdinal               int                            `json:"request_ordinal"`
	LeaseEpoch                   int64                          `json:"lease_epoch"`
	RequestManifestID            string                         `json:"request_manifest_id"`
	RequestManifestSHA256        string                         `json:"request_manifest_sha256"`
	RequestSlotID                string                         `json:"request_slot_id"`
	RequestSemanticsID           string                         `json:"request_semantics_id"`
	RequestSemanticsSHA256       string                         `json:"request_semantics_sha256"`
	RequestSemanticsPolicySHA256 *string                        `json:"request_semantics_policy_sha256"`
	RequestToolSchemaSHA256      string                         `json:"request_tool_schema_sha256"`
	RequestAllowedToolSetSHA256  string                         `json:"request_allowed_tool_set_sha256"`
	EvidenceRevision             int64                          `json:"evidence_revision"`
	APIKeyID                     int64                          `json:"api_key_id"`
	RequestID                    string                         `json:"request_id"`
	RequestedModel               string                         `json:"requested_model"`
	ResolvedModel                *string                        `json:"resolved_model"`
	RouteProfileVersion          string                         `json:"route_profile_version"`
	GatewayImageDigest           string                         `json:"gateway_image_digest"`
	Provider                     *string                        `json:"provider"`
	ChannelRef                   *string                        `json:"channel_ref"`
	AccountPoolRef               *string                        `json:"account_pool_ref"`
	Region                       string                         `json:"region"`
	Attempts                     int                            `json:"attempts"`
	FallbackChain                []RouteEvidenceAttemptEnvelope `json:"fallback_chain"`
	TransportStatus              string                         `json:"transport_status"`
	ErrorCode                    *string                        `json:"error_code"`
	FinishReason                 *string                        `json:"finish_reason"`
	InputTokens                  *int                           `json:"input_tokens"`
	OutputTokens                 *int                           `json:"output_tokens"`
	TTFTMS                       *int                           `json:"ttft_ms"`
	LatencyMS                    *int                           `json:"latency_ms"`
	BillingStatus                string                         `json:"billing_status"`
	BilledAmount                 *string                        `json:"billed_amount"`
	IncompleteReason             *string                        `json:"incomplete_reason"`
	StartedAt                    string                         `json:"started_at"`
	FinishedAt                   *string                        `json:"finished_at"`
	TerminalAt                   string                         `json:"terminal_at"`
	SealedAt                     string                         `json:"sealed_at"`
	SigningKeyID                 string                         `json:"signing_key_id"`
}

type CanonicalRouteEvidenceEnvelope struct {
	Envelope RouteEvidenceEnvelope
	Bytes    []byte
	SHA256   string
}

func CanonicalizeRouteEvidenceEnvelope(envelope RouteEvidenceEnvelope) (CanonicalRouteEvidenceEnvelope, error) {
	if envelope.FallbackChain == nil {
		envelope.FallbackChain = []RouteEvidenceAttemptEnvelope{}
	}
	if err := validateRouteEvidenceEnvelope(envelope); err != nil {
		return CanonicalRouteEvidenceEnvelope{}, err
	}
	canonical, err := canonicalizeContract(envelope)
	if err != nil {
		return CanonicalRouteEvidenceEnvelope{}, fmt.Errorf("canonicalize route evidence envelope: %w", err)
	}
	return CanonicalRouteEvidenceEnvelope{Envelope: envelope, Bytes: canonical.Bytes, SHA256: canonical.SHA256}, nil
}

func SignEvidence(schema string, canonical, key []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(schema))
	_, _ = mac.Write([]byte{0x0a})
	_, _ = mac.Write(canonical)
	return hex.EncodeToString(mac.Sum(nil))
}

func FormatRouteEvidenceTime(value time.Time) string {
	return value.UTC().Format(routeEvidenceTimeLayout)
}

func validateRouteEvidenceEnvelope(envelope RouteEvidenceEnvelope) error {
	if envelope.SchemaVersion != RouteEvidenceSchemaV1 || envelope.CanonicalizationVersion != RouteEvidenceCanonicalizationV1 {
		return errors.New("route evidence envelope schema is invalid")
	}
	for name, value := range map[string]string{
		"route trace": envelope.RouteTraceID, "run": envelope.EvaluationRunID, "sample": envelope.SampleID,
		"assignment": envelope.AssignmentID, "manifest": envelope.RequestManifestID,
		"semantics": envelope.RequestSemanticsID, "signing key": envelope.SigningKeyID,
	} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed.String() != value {
			return fmt.Errorf("route evidence envelope %s UUID is invalid", name)
		}
	}
	for name, value := range map[string]string{
		"manifest": envelope.RequestManifestSHA256, "semantics": envelope.RequestSemanticsSHA256,
		"tool schema":      envelope.RequestToolSchemaSHA256,
		"allowed tool set": envelope.RequestAllowedToolSetSHA256,
	} {
		if !validSHA256(value) {
			return fmt.Errorf("route evidence envelope %s hash is invalid", name)
		}
	}
	if envelope.RequestSemanticsPolicySHA256 != nil && !validSHA256(*envelope.RequestSemanticsPolicySHA256) {
		return errors.New("route evidence envelope semantics policy hash is invalid")
	}
	if envelope.RequestOrdinal < 0 || envelope.LeaseEpoch < 0 || envelope.EvidenceRevision < 1 || envelope.APIKeyID <= 0 ||
		strings.TrimSpace(envelope.RequestSlotID) == "" || strings.TrimSpace(envelope.RequestID) == "" ||
		strings.TrimSpace(envelope.RequestedModel) == "" || strings.TrimSpace(envelope.RouteProfileVersion) == "" ||
		strings.TrimSpace(envelope.Region) == "" {
		return errors.New("route evidence envelope identity is incomplete")
	}
	if envelope.BilledAmount != nil && !eightDecimalAmountPattern.MatchString(*envelope.BilledAmount) {
		return errors.New("route evidence billed amount must use eight decimal places")
	}
	startedAt, err := parseRouteEvidenceTime(envelope.StartedAt)
	if err != nil {
		return err
	}
	terminalAt, err := parseRouteEvidenceTime(envelope.TerminalAt)
	if err != nil {
		return err
	}
	sealedAt, err := parseRouteEvidenceTime(envelope.SealedAt)
	if err != nil {
		return err
	}
	if terminalAt.Before(startedAt) || sealedAt.Before(terminalAt) {
		return errors.New("route evidence envelope times are not monotonic")
	}
	if envelope.Attempts != len(envelope.FallbackChain) {
		return ErrRouteEvidenceFallbackInvalid
	}
	if envelope.Attempts == 0 {
		switch envelope.TransportStatus {
		case "protocol_failed", "client_cancelled", "gateway_failed":
			return nil
		default:
			return ErrRouteEvidenceFallbackInvalid
		}
	}
	finishedAttempts := make([]time.Time, 0, len(envelope.FallbackChain))
	for index, attempt := range envelope.FallbackChain {
		if attempt.AttemptIndex != index+1 || !validSHA256(attempt.RouteRuleHash) || strings.TrimSpace(attempt.RequestedModel) == "" ||
			strings.TrimSpace(attempt.ResolvedModel) == "" || strings.TrimSpace(attempt.Provider) == "" || strings.TrimSpace(attempt.Region) == "" {
			return ErrRouteEvidenceFallbackInvalid
		}
		if index == 0 {
			if attempt.ParentAttemptIndex != nil || attempt.DispatchMode != "primary" {
				return ErrRouteEvidenceFallbackInvalid
			}
		} else if attempt.ParentAttemptIndex == nil || *attempt.ParentAttemptIndex < 1 || *attempt.ParentAttemptIndex >= attempt.AttemptIndex {
			return ErrRouteEvidenceFallbackInvalid
		}
		if attempt.DispatchMode != "primary" && attempt.DispatchMode != "retry" && attempt.DispatchMode != "fallback" && attempt.DispatchMode != "hedge" {
			return ErrRouteEvidenceFallbackInvalid
		}
		if !validRouteEvidenceOutcome(attempt.Outcome) {
			return ErrRouteEvidenceFallbackInvalid
		}
		attemptStarted, startErr := parseRouteEvidenceTime(attempt.StartedAt)
		attemptFinished, finishErr := parseRouteEvidenceTime(attempt.FinishedAt)
		if startErr != nil || finishErr != nil || attemptFinished.Before(attemptStarted) {
			return ErrRouteEvidenceFallbackInvalid
		}
		if index > 0 && attempt.DispatchMode != "hedge" && attemptStarted.Before(finishedAttempts[*attempt.ParentAttemptIndex-1]) {
			return ErrRouteEvidenceFallbackInvalid
		}
		finishedAttempts = append(finishedAttempts, attemptFinished)
	}
	finalAttempt := envelope.FallbackChain[len(envelope.FallbackChain)-1]
	if envelope.TransportStatus == "succeeded" {
		found := false
		for index := len(envelope.FallbackChain) - 1; index >= 0; index-- {
			if envelope.FallbackChain[index].Outcome == "succeeded" {
				finalAttempt = envelope.FallbackChain[index]
				found = true
				break
			}
		}
		if !found || envelope.BillingStatus != "complete" || envelope.FinishReason == nil ||
			envelope.InputTokens == nil || envelope.OutputTokens == nil || envelope.LatencyMS == nil ||
			envelope.BilledAmount == nil || envelope.FinishedAt == nil {
			return ErrRouteEvidenceIncomplete
		}
	}
	if !sameEnvelopeString(envelope.ResolvedModel, finalAttempt.ResolvedModel) ||
		!sameEnvelopeString(envelope.Provider, finalAttempt.Provider) ||
		!sameEnvelopeString(envelope.ChannelRef, finalAttempt.ChannelRef) ||
		!sameEnvelopeString(envelope.AccountPoolRef, finalAttempt.AccountPoolRef) ||
		envelope.Region != finalAttempt.Region {
		return ErrRouteEvidenceFallbackInvalid
	}
	return nil
}

func sameEnvelopeString(value *string, expected string) bool {
	if value == nil {
		return expected == ""
	}
	return *value == expected
}

func parseRouteEvidenceTime(value string) (time.Time, error) {
	parsed, err := time.Parse(routeEvidenceTimeLayout, value)
	if err != nil || parsed.Format(routeEvidenceTimeLayout) != value {
		return time.Time{}, errors.New("route evidence time must be UTC with six fractional digits")
	}
	return parsed, nil
}

func validRouteEvidenceOutcome(value string) bool {
	switch value {
	case "succeeded", "upstream_failed", "protocol_failed", "gateway_failed", "cancelled":
		return true
	default:
		return false
	}
}
