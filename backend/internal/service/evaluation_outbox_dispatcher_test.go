package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type evaluationOutboxDomainStub struct {
	validated        []EvaluationOutboxEvent
	cellRequest      CellAnalysisJobRequest
	globalRequest    GlobalAnalysisJobRequest
	globalJob        *AnalysisJobRevision
	target           *RadarGateTarget
	gateOutcome      AutomatedRadarGateOutcome
	reconciledRunIDs []uuid.UUID
	err              error
}

func (s *evaluationOutboxDomainStub) ValidateSealedRouteEvidence(_ context.Context, event EvaluationOutboxEvent) error {
	s.validated = append(s.validated, event)
	return s.err
}

func (s *evaluationOutboxDomainStub) EnsureCellAnalysisJob(_ context.Context, request CellAnalysisJobRequest) (*AnalysisJobRevision, error) {
	s.cellRequest = request
	return &AnalysisJobRevision{ID: uuid.New(), RunID: request.RunID}, s.err
}

func (s *evaluationOutboxDomainStub) EnsureGlobalAnalysisJob(_ context.Context, request GlobalAnalysisJobRequest) (*AnalysisJobRevision, error) {
	s.globalRequest = request
	return s.globalJob, s.err
}

func (s *evaluationOutboxDomainStub) ResolveRadarGateTarget(context.Context, uuid.UUID) (*RadarGateTarget, error) {
	return s.target, s.err
}

func (s *evaluationOutboxDomainStub) EvaluateAndProjectRadarGate(_ context.Context, outcome AutomatedRadarGateOutcome) (*RadarGateDecisionRecord, error) {
	s.gateOutcome = outcome
	return &RadarGateDecisionRecord{ID: uuid.New(), RunID: outcome.EventRunID, PolicyID: outcome.Target.PolicyID}, s.err
}

func (s *evaluationOutboxDomainStub) ReconcileEvaluationRun(_ context.Context, runID uuid.UUID) error {
	s.reconciledRunIDs = append(s.reconciledRunIDs, runID)
	return s.err
}

func outboxDispatcherEvent(t *testing.T, eventType, sourceType string, payload any) EvaluationOutboxEvent {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	payloadHash, err := DigestCanonicalJSON(raw)
	require.NoError(t, err)
	return EvaluationOutboxEvent{
		ID: uuid.New(), EventType: eventType, RunID: uuid.New(), ScopeKey: "coding/route-a",
		AnalysisVersion: "v1", SourceType: sourceType, SourceID: uuid.NewString(),
		SourceHash: strings.Repeat("a", 64), PayloadHash: payloadHash, Payload: raw,
		CauseSetHash: strings.Repeat("b", 64), CreatedAt: time.Now().UTC(),
	}
}

func TestEvaluationOutboxDispatcherRejectsPayloadHashMismatch(t *testing.T) {
	event := outboxDispatcherEvent(t, "cell_recompute", "score_head_event", map[string]any{
		"capability_domain": "coding", "model_route": "candidate:route-a", "score_head_event_id": uuid.NewString(),
	})
	event.PayloadHash = strings.Repeat("0", 64)
	dispatcher := NewEvaluationOutboxDispatcher(&evaluationOutboxDomainStub{}, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchDeadLetter, result.Disposition)
	require.Equal(t, "payload_hash_mismatch", result.ErrorCode)
}

func TestEvaluationOutboxDispatcherRejectsMalformedPayload(t *testing.T) {
	event := outboxDispatcherEvent(t, "cell_recompute", "score_head_event", map[string]any{
		"capability_domain": "coding", "model_route": "route-a", "score_head_event_id": uuid.New(),
	})
	event.Payload = json.RawMessage(`{"capability_domain":`)
	dispatcher := NewEvaluationOutboxDispatcher(&evaluationOutboxDomainStub{}, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchDeadLetter, result.Disposition)
	require.Equal(t, "invalid_payload", result.ErrorCode)
}

func TestEvaluationOutboxDispatcherRejectsUnknownPayloadField(t *testing.T) {
	headEventID := uuid.New()
	event := outboxDispatcherEvent(t, "cell_recompute", "score_head_event", map[string]any{
		"capability_domain": "coding", "model_route": "route-a", "score_head_event_id": headEventID,
		"unexpected": true,
	})
	event.SourceID = headEventID.String()
	dispatcher := NewEvaluationOutboxDispatcher(&evaluationOutboxDomainStub{}, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchDeadLetter, result.Disposition)
	require.Equal(t, "payload_schema_invalid", result.ErrorCode)
}

func TestEvaluationOutboxDispatcherCreatesCellJobWithV1Fallback(t *testing.T) {
	headEventID := uuid.New()
	event := outboxDispatcherEvent(t, "cell_recompute", "score_head_event", map[string]any{
		"capability_domain": "coding", "model_route": "candidate:route-a", "score_head_event_id": headEventID,
	})
	event.SourceID = headEventID.String()
	domain := &evaluationOutboxDomainStub{}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchComplete, result.Disposition)
	require.Equal(t, CellAnalysisJobRequest{
		RunID: event.RunID, CapabilityDomain: "coding", ModelRoute: "route-a", AnalysisVersion: "v1",
	}, domain.cellRequest)
}

func TestEvaluationOutboxDispatcherRejectsUnsupportedAnalysisVersion(t *testing.T) {
	headEventID := uuid.New()
	event := outboxDispatcherEvent(t, "cell_recompute", "score_head_event", map[string]any{
		"capability_domain": "coding", "model_route": "route-a", "score_head_event_id": headEventID,
		"analysis_version": "v2",
	})
	event.SourceID = headEventID.String()
	dispatcher := NewEvaluationOutboxDispatcher(&evaluationOutboxDomainStub{}, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchDeadLetter, result.Disposition)
	require.Equal(t, "unsupported_analysis_version", result.ErrorCode)
}

func TestEvaluationOutboxDispatcherUsesScopeForAssignmentReplacement(t *testing.T) {
	replacementAssignmentID := uuid.New()
	event := outboxDispatcherEvent(t, "cell_recompute", "assignment_replacement", map[string]any{
		"sample_id": uuid.New(), "old_assignment_id": uuid.New(), "replacement_assignment_id": replacementAssignmentID,
		"old_attempt": 1, "source_head_event_id": uuid.New(),
	})
	event.SourceID = replacementAssignmentID.String()
	event.ScopeKey = "reasoning/route-b"
	domain := &evaluationOutboxDomainStub{}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchComplete, result.Disposition)
	require.Equal(t, "reasoning", domain.cellRequest.CapabilityDomain)
	require.Equal(t, "route-b", domain.cellRequest.ModelRoute)
}

func TestEvaluationOutboxDispatcherValidatesSealedEvidenceIdentity(t *testing.T) {
	traceID := uuid.NewString()
	event := outboxDispatcherEvent(t, "route_evidence_sealed", "route_evidence", map[string]any{
		"route_trace_id": traceID, "schema_version": "radar-route-evidence-v1", "evidence_revision": 2,
	})
	event.SourceID = traceID
	domain := &evaluationOutboxDomainStub{}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchComplete, result.Disposition)
	require.Equal(t, []EvaluationOutboxEvent{event}, domain.validated)
}

func TestEvaluationOutboxDispatcherRejectsEventSourceMismatch(t *testing.T) {
	event := outboxDispatcherEvent(t, "cell_recompute", "route_evidence", map[string]any{
		"capability_domain": "coding", "model_route": "route-a", "score_head_event_id": uuid.New(),
	})
	dispatcher := NewEvaluationOutboxDispatcher(&evaluationOutboxDomainStub{}, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchDeadLetter, result.Disposition)
	require.Equal(t, "event_source_mismatch", result.ErrorCode)
}

func TestEvaluationOutboxDispatcherRunsCompatibleGateForHistoricalSingleCellGlobal(t *testing.T) {
	snapshotID := uuid.New()
	event := outboxDispatcherEvent(t, "global_recompute", "aggregate_head", map[string]any{
		"snapshot_id": snapshotID, "capability_domain": "coding", "model_route": "route-a", "analysis_version": "v1",
	})
	event.SourceID = snapshotID.String()
	domain := &evaluationOutboxDomainStub{target: &RadarGateTarget{
		ReleaseSubjectID: uuid.New(), PolicyID: uuid.New(), TenantID: 42,
	}}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeFull)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchComplete, result.Disposition)
	require.Equal(t, GlobalAnalysisJobRequest{RunID: event.RunID, AnalysisVersion: "v1"}, domain.globalRequest)
	require.Equal(t, event.ID, domain.gateOutcome.EventID)
	require.Equal(t, *domain.target, domain.gateOutcome.Target)
}

func TestEvaluationOutboxDispatcherCompletesGateWithoutTarget(t *testing.T) {
	snapshotID := uuid.New()
	event := outboxDispatcherEvent(t, "gate_reevaluation", "aggregate_head", map[string]any{
		"snapshot_id": snapshotID, "capability_domain": "global", "model_route": "global", "analysis_version": "v1",
	})
	event.SourceID = snapshotID.String()
	domain := &evaluationOutboxDomainStub{}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeFull)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchComplete, result.Disposition)
	require.Equal(t, uuid.Nil, domain.gateOutcome.EventID)
}

func TestEvaluationOutboxDispatcherDelaysConfiguredGateInCoreMode(t *testing.T) {
	snapshotID := uuid.New()
	event := outboxDispatcherEvent(t, "gate_reevaluation", "aggregate_head", map[string]any{
		"snapshot_id": snapshotID, "capability_domain": "global", "model_route": "global", "analysis_version": "v1",
	})
	event.SourceID = snapshotID.String()
	domain := &evaluationOutboxDomainStub{target: &RadarGateTarget{
		ReleaseSubjectID: uuid.New(), PolicyID: uuid.New(), TenantID: 42,
	}}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeCore)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchRetry, result.Disposition)
	require.Equal(t, "gate_full_mode_required", result.ErrorCode)
	require.Equal(t, time.Minute, result.RetryAfter)
}

func TestEvaluationOutboxDispatcherProjectsReliabilityGateInFullMode(t *testing.T) {
	snapshotID := uuid.New()
	headEventID := uuid.New()
	event := outboxDispatcherEvent(t, "gate_reevaluation", "reliability_head_event", map[string]any{
		"reliability_profile_id": "interactive", "slice_key": "all", "snapshot_id": snapshotID,
		"snapshot_hash": strings.Repeat("c", 64),
	})
	event.SourceID = headEventID.String()
	domain := &evaluationOutboxDomainStub{target: &RadarGateTarget{
		ReleaseSubjectID: uuid.New(), PolicyID: uuid.New(), TenantID: 42,
	}}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeFull)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchComplete, result.Disposition)
	require.Equal(t, event.ID, domain.gateOutcome.EventID)
	require.Equal(t, event.RunID, domain.gateOutcome.EventRunID)
	require.Equal(t, event.CauseSetHash, domain.gateOutcome.CauseSetHash)
	require.Equal(t, *domain.target, domain.gateOutcome.Target)
}

func TestEvaluationOutboxDispatcherProjectsSigningKeyGateInFullMode(t *testing.T) {
	keyID := uuid.New()
	event := outboxDispatcherEvent(t, "gate_reevaluation", "evidence_signing_key_state", map[string]any{
		"signing_key_id": keyID, "status": EvidenceSigningKeyRevoked, "state_epoch": int64(3),
	})
	event.SourceID = keyID.String() + ":3"
	domain := &evaluationOutboxDomainStub{target: &RadarGateTarget{
		ReleaseSubjectID: uuid.New(), PolicyID: uuid.New(), TenantID: 42,
	}}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeFull)

	result := dispatcher.Dispatch(context.Background(), event)

	require.Equal(t, EvaluationOutboxDispatchComplete, result.Disposition)
	require.Equal(t, event.ID, domain.gateOutcome.EventID)
}

func TestEvaluationOutboxDispatcherClassifiesDependencyAndFencingErrors(t *testing.T) {
	headEventID := uuid.New()
	event := outboxDispatcherEvent(t, "cell_recompute", "score_head_event", map[string]any{
		"capability_domain": "coding", "model_route": "route-a", "score_head_event_id": headEventID,
	})
	event.SourceID = headEventID.String()

	dependency := NewEvaluationOutboxDispatcher(
		&evaluationOutboxDomainStub{err: ErrAggregatePairsIncomplete}, EvaluationOutboxConsumerModeCore,
	).Dispatch(context.Background(), event)
	require.Equal(t, EvaluationOutboxDispatchRetry, dependency.Disposition)
	require.Equal(t, "aggregate_dependency_pending", dependency.ErrorCode)

	fenced := NewEvaluationOutboxDispatcher(
		&evaluationOutboxDomainStub{err: ErrEvaluationOutboxFenced}, EvaluationOutboxConsumerModeCore,
	).Dispatch(context.Background(), event)
	require.Equal(t, EvaluationOutboxDispatchFenced, fenced.Disposition)
	require.Equal(t, "outbox_fenced", fenced.ErrorCode)
}

func TestEvaluationOutboxDispatcherReconcilesOnlyAfterComplete(t *testing.T) {
	event := outboxDispatcherEvent(t, "route_evidence_sealed", "route_evidence", map[string]any{
		"route_trace_id": "trace-1", "schema_version": "radar-route-evidence-v1", "evidence_revision": 1,
	})
	event.SourceID = "trace-1"
	domain := &evaluationOutboxDomainStub{}
	dispatcher := NewEvaluationOutboxDispatcher(domain, EvaluationOutboxConsumerModeCore)

	require.Equal(t, EvaluationOutboxDispatchComplete, dispatcher.Dispatch(context.Background(), event).Disposition)
	require.Empty(t, domain.reconciledRunIDs)
	require.NoError(t, dispatcher.AfterComplete(context.Background(), event))
	require.Equal(t, []uuid.UUID{event.RunID}, domain.reconciledRunIDs)
}
