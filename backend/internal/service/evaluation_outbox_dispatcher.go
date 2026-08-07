package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

type EvaluationOutboxConsumerMode string

const (
	EvaluationOutboxConsumerModeDisabled EvaluationOutboxConsumerMode = "disabled"
	EvaluationOutboxConsumerModeCore     EvaluationOutboxConsumerMode = "core"
	EvaluationOutboxConsumerModeFull     EvaluationOutboxConsumerMode = "full"
)

type EvaluationOutboxDispatchDisposition string

const (
	EvaluationOutboxDispatchComplete   EvaluationOutboxDispatchDisposition = "complete"
	EvaluationOutboxDispatchRetry      EvaluationOutboxDispatchDisposition = "retry"
	EvaluationOutboxDispatchDeadLetter EvaluationOutboxDispatchDisposition = "dead_letter"
	EvaluationOutboxDispatchFenced     EvaluationOutboxDispatchDisposition = "fenced"
)

type EvaluationOutboxDispatchResult struct {
	Disposition EvaluationOutboxDispatchDisposition
	ErrorCode   string
	RetryAfter  time.Duration
}

type RadarGateTarget struct {
	ReleaseSubjectID uuid.UUID
	PolicyID         uuid.UUID
	TenantID         int64
}

type AutomatedRadarGateOutcome struct {
	EventID      uuid.UUID
	EventRunID   uuid.UUID
	CauseSetHash string
	Target       RadarGateTarget
}

type EvaluationOutboxDomainRepository interface {
	ValidateSealedRouteEvidence(context.Context, EvaluationOutboxEvent) error
	EnsureCellAnalysisJob(context.Context, CellAnalysisJobRequest) (*AnalysisJobRevision, error)
	EnsureGlobalAnalysisJob(context.Context, GlobalAnalysisJobRequest) (*AnalysisJobRevision, error)
	ResolveRadarGateTarget(context.Context, uuid.UUID) (*RadarGateTarget, error)
	EvaluateAndProjectRadarGate(context.Context, AutomatedRadarGateOutcome) (*RadarGateDecisionRecord, error)
	ReconcileEvaluationRun(context.Context, uuid.UUID) error
}

type EvaluationOutboxDispatcher struct {
	domain EvaluationOutboxDomainRepository
	mode   EvaluationOutboxConsumerMode
}

func NewEvaluationOutboxDispatcher(domain EvaluationOutboxDomainRepository, mode EvaluationOutboxConsumerMode) *EvaluationOutboxDispatcher {
	return &EvaluationOutboxDispatcher{domain: domain, mode: mode}
}

func (d *EvaluationOutboxDispatcher) Dispatch(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	if d == nil || d.domain == nil {
		return outboxRetry("dispatcher_unavailable", 0)
	}
	if event.ID == uuid.Nil || event.RunID == uuid.Nil || strings.TrimSpace(event.SourceType) == "" ||
		strings.TrimSpace(event.SourceID) == "" || !isLowerHexSHA256(event.SourceHash) ||
		!isLowerHexSHA256(event.PayloadHash) {
		return outboxDeadLetter("invalid_event_identity")
	}
	if !json.Valid(event.Payload) {
		return outboxDeadLetter("invalid_payload")
	}
	payloadHash, err := DigestCanonicalJSON(event.Payload)
	if err != nil {
		return outboxDeadLetter("invalid_payload")
	}
	if payloadHash != event.PayloadHash {
		return outboxDeadLetter("payload_hash_mismatch")
	}

	switch event.EventType + "\x00" + event.SourceType {
	case "route_evidence_sealed\x00route_evidence":
		return d.dispatchRouteEvidence(ctx, event)
	case "cell_recompute\x00score_head_event":
		return d.dispatchScoreHead(ctx, event)
	case "cell_recompute\x00assignment_replacement":
		return d.dispatchAssignmentReplacement(ctx, event)
	case "global_recompute\x00aggregate_head":
		return d.dispatchGlobalAggregate(ctx, event)
	case "gate_reevaluation\x00aggregate_head":
		return d.dispatchAggregateGate(ctx, event)
	case "gate_reevaluation\x00reliability_head_event":
		return d.dispatchReliabilityGate(ctx, event)
	case "gate_reevaluation\x00evidence_signing_key_state":
		return d.dispatchSigningKeyGate(ctx, event)
	default:
		return outboxDeadLetter("event_source_mismatch")
	}
}

func (d *EvaluationOutboxDispatcher) AfterComplete(ctx context.Context, event EvaluationOutboxEvent) error {
	if d == nil || d.domain == nil || event.RunID == uuid.Nil {
		return ErrEvaluationOutboxInvalid
	}
	return d.domain.ReconcileEvaluationRun(ctx, event.RunID)
}

type routeEvidenceSealedPayload struct {
	RouteTraceID     string `json:"route_trace_id"`
	SchemaVersion    string `json:"schema_version"`
	EvidenceRevision int64  `json:"evidence_revision"`
}

func (d *EvaluationOutboxDispatcher) dispatchRouteEvidence(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	var payload routeEvidenceSealedPayload
	if err := decodeOutboxPayload(event.Payload, &payload); err != nil {
		return outboxDeadLetter("payload_schema_invalid")
	}
	if strings.TrimSpace(payload.RouteTraceID) == "" || strings.TrimSpace(payload.SchemaVersion) == "" ||
		payload.EvidenceRevision <= 0 || payload.RouteTraceID != event.SourceID {
		return outboxDeadLetter("source_identity_mismatch")
	}
	return classifyOutboxDomainError(d.domain.ValidateSealedRouteEvidence(ctx, event))
}

type scoreHeadRecomputePayload struct {
	CapabilityDomain string    `json:"capability_domain"`
	ModelRoute       string    `json:"model_route"`
	ScoreHeadEventID uuid.UUID `json:"score_head_event_id"`
	AnalysisVersion  string    `json:"analysis_version,omitempty"`
}

func (d *EvaluationOutboxDispatcher) dispatchScoreHead(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	var payload scoreHeadRecomputePayload
	if err := decodeOutboxPayload(event.Payload, &payload); err != nil {
		return outboxDeadLetter("payload_schema_invalid")
	}
	version, ok := compatibleAnalysisVersion(payload.AnalysisVersion)
	if !ok {
		return outboxDeadLetter("unsupported_analysis_version")
	}
	domain, route, ok := canonicalCellScope(payload.CapabilityDomain, payload.ModelRoute)
	if !ok || payload.ScoreHeadEventID == uuid.Nil || payload.ScoreHeadEventID.String() != event.SourceID {
		return outboxDeadLetter("source_identity_mismatch")
	}
	_, err := d.domain.EnsureCellAnalysisJob(ctx, CellAnalysisJobRequest{
		RunID: event.RunID, CapabilityDomain: domain, ModelRoute: route, AnalysisVersion: version,
	})
	return classifyOutboxDomainError(err)
}

type assignmentReplacementPayload struct {
	SampleID                uuid.UUID `json:"sample_id"`
	OldAssignmentID         uuid.UUID `json:"old_assignment_id"`
	ReplacementAssignmentID uuid.UUID `json:"replacement_assignment_id"`
	OldAttempt              int       `json:"old_attempt"`
	SourceHeadEventID       uuid.UUID `json:"source_head_event_id"`
}

func (d *EvaluationOutboxDispatcher) dispatchAssignmentReplacement(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	var payload assignmentReplacementPayload
	if err := decodeOutboxPayload(event.Payload, &payload); err != nil {
		return outboxDeadLetter("payload_schema_invalid")
	}
	domain, route, ok := splitOutboxScope(event.ScopeKey)
	if !ok || payload.SampleID == uuid.Nil || payload.OldAssignmentID == uuid.Nil ||
		payload.ReplacementAssignmentID == uuid.Nil || payload.SourceHeadEventID == uuid.Nil ||
		payload.OldAttempt < 0 || payload.ReplacementAssignmentID.String() != event.SourceID {
		return outboxDeadLetter("source_identity_mismatch")
	}
	_, err := d.domain.EnsureCellAnalysisJob(ctx, CellAnalysisJobRequest{
		RunID: event.RunID, CapabilityDomain: domain, ModelRoute: route, AnalysisVersion: "v1",
	})
	return classifyOutboxDomainError(err)
}

type aggregateHeadPayload struct {
	SnapshotID       uuid.UUID `json:"snapshot_id"`
	CapabilityDomain string    `json:"capability_domain"`
	ModelRoute       string    `json:"model_route"`
	AnalysisVersion  string    `json:"analysis_version,omitempty"`
}

func (d *EvaluationOutboxDispatcher) dispatchGlobalAggregate(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	var payload aggregateHeadPayload
	if err := decodeOutboxPayload(event.Payload, &payload); err != nil {
		return outboxDeadLetter("payload_schema_invalid")
	}
	version, valid := compatibleAnalysisVersion(payload.AnalysisVersion)
	if !valid {
		return outboxDeadLetter("unsupported_analysis_version")
	}
	if payload.SnapshotID == uuid.Nil || payload.SnapshotID.String() != event.SourceID {
		return outboxDeadLetter("source_identity_mismatch")
	}
	job, err := d.domain.EnsureGlobalAnalysisJob(ctx, GlobalAnalysisJobRequest{
		RunID: event.RunID, AnalysisVersion: version,
	})
	if err != nil {
		return classifyOutboxDomainError(err)
	}
	if job != nil {
		return outboxComplete()
	}
	return d.dispatchGate(ctx, event)
}

func (d *EvaluationOutboxDispatcher) dispatchAggregateGate(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	var payload aggregateHeadPayload
	if err := decodeOutboxPayload(event.Payload, &payload); err != nil {
		return outboxDeadLetter("payload_schema_invalid")
	}
	if _, valid := compatibleAnalysisVersion(payload.AnalysisVersion); !valid {
		return outboxDeadLetter("unsupported_analysis_version")
	}
	if payload.SnapshotID == uuid.Nil || payload.SnapshotID.String() != event.SourceID {
		return outboxDeadLetter("source_identity_mismatch")
	}
	return d.dispatchGate(ctx, event)
}

type reliabilityHeadPayload struct {
	ProfileID    string    `json:"reliability_profile_id"`
	SliceKey     string    `json:"slice_key"`
	SnapshotID   uuid.UUID `json:"snapshot_id"`
	SnapshotHash string    `json:"snapshot_hash"`
}

func (d *EvaluationOutboxDispatcher) dispatchReliabilityGate(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	var payload reliabilityHeadPayload
	if err := decodeOutboxPayload(event.Payload, &payload); err != nil {
		return outboxDeadLetter("payload_schema_invalid")
	}
	if strings.TrimSpace(payload.ProfileID) == "" || strings.TrimSpace(payload.SliceKey) == "" ||
		payload.SnapshotID == uuid.Nil || !isLowerHexSHA256(payload.SnapshotHash) {
		return outboxDeadLetter("source_identity_mismatch")
	}
	if _, err := uuid.Parse(event.SourceID); err != nil {
		return outboxDeadLetter("source_identity_mismatch")
	}
	return d.dispatchGate(ctx, event)
}

type signingKeyStatePayload struct {
	SigningKeyID uuid.UUID                `json:"signing_key_id"`
	Status       EvidenceSigningKeyStatus `json:"status"`
	StateEpoch   int64                    `json:"state_epoch"`
}

func (d *EvaluationOutboxDispatcher) dispatchSigningKeyGate(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	var payload signingKeyStatePayload
	if err := decodeOutboxPayload(event.Payload, &payload); err != nil {
		return outboxDeadLetter("payload_schema_invalid")
	}
	validStatus := payload.Status == EvidenceSigningKeyActive || payload.Status == EvidenceSigningKeyVerifyOnly ||
		payload.Status == EvidenceSigningKeyRevoked
	if payload.SigningKeyID == uuid.Nil || payload.StateEpoch <= 0 || !validStatus ||
		event.SourceID != payload.SigningKeyID.String()+":"+fmt.Sprint(payload.StateEpoch) {
		return outboxDeadLetter("source_identity_mismatch")
	}
	return d.dispatchGate(ctx, event)
}

func (d *EvaluationOutboxDispatcher) dispatchGate(ctx context.Context, event EvaluationOutboxEvent) EvaluationOutboxDispatchResult {
	target, err := d.domain.ResolveRadarGateTarget(ctx, event.RunID)
	if err != nil {
		return classifyOutboxDomainError(err)
	}
	if target == nil {
		return outboxComplete()
	}
	if d.mode != EvaluationOutboxConsumerModeFull {
		return outboxRetry("gate_full_mode_required", time.Minute)
	}
	_, err = d.domain.EvaluateAndProjectRadarGate(ctx, AutomatedRadarGateOutcome{
		EventID: event.ID, EventRunID: event.RunID, CauseSetHash: event.CauseSetHash, Target: *target,
	})
	return classifyOutboxDomainError(err)
}

func decodeOutboxPayload(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrEvaluationOutboxInvalid
	}
	return nil
}

func compatibleAnalysisVersion(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "v1", true
	}
	return value, value == "v1"
}

func canonicalCellScope(domain, route string) (string, string, bool) {
	domain = strings.TrimSpace(domain)
	route = CanonicalModelRoute(strings.TrimSpace(route))
	return domain, route, domain != "" && route != ""
}

func splitOutboxScope(scope string) (string, string, bool) {
	domain, route, found := strings.Cut(strings.TrimSpace(scope), "/")
	if !found {
		return "", "", false
	}
	return canonicalCellScope(domain, route)
}

func classifyOutboxDomainError(err error) EvaluationOutboxDispatchResult {
	if err == nil {
		return outboxComplete()
	}
	switch {
	case errors.Is(err, ErrEvaluationOutboxFenced):
		return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchFenced, ErrorCode: "outbox_fenced"}
	case errors.Is(err, ErrAggregatePairsIncomplete):
		return outboxRetry("aggregate_dependency_pending", 0)
	case errors.Is(err, ErrAggregateInputMismatch),
		errors.Is(err, ErrAggregateRevisionInvalid),
		errors.Is(err, ErrRouteEvidenceSealedConflict),
		errors.Is(err, ErrRouteEvidenceIdentityConflict),
		errors.Is(err, ErrEvaluationOutboxInvalid):
		return outboxDeadLetter("domain_contract_invalid")
	default:
		return outboxRetry("outbox_handler_failed", 0)
	}
}

func outboxComplete() EvaluationOutboxDispatchResult {
	return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchComplete}
}

func outboxRetry(code string, delay time.Duration) EvaluationOutboxDispatchResult {
	return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchRetry, ErrorCode: code, RetryAfter: delay}
}

func outboxDeadLetter(code string) EvaluationOutboxDispatchResult {
	return EvaluationOutboxDispatchResult{Disposition: EvaluationOutboxDispatchDeadLetter, ErrorCode: code}
}
