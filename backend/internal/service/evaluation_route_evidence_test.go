//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type trackedBillingEvidenceRepo struct {
	patches       []RouteEvidencePatch
	finalizations []FinalizeRouteEvidenceInput
	legacyCalls   int
	state         RouteEvidencePatchState
	conflictOnce  bool
}

func (r *trackedBillingEvidenceRepo) UpsertTransport(context.Context, RouteEvidence) error {
	return nil
}

func (r *trackedBillingEvidenceRepo) AttachBilling(context.Context, string, RouteUsageEvidence) error {
	r.legacyCalls++
	return nil
}

func (r *trackedBillingEvidenceRepo) CreateOpen(context.Context, CreateOpenRouteEvidenceInput) (RouteEvidencePatchState, error) {
	return r.state, nil
}

func (r *trackedBillingEvidenceRepo) PatchRouteEvidence(_ context.Context, _ string, patch RouteEvidencePatch) (RouteEvidencePatchState, error) {
	r.patches = append(r.patches, patch)
	if r.conflictOnce {
		r.conflictOnce = false
		r.state.Revision++
		return r.state, &RouteEvidenceRevisionConflict{CurrentRevision: r.state.Revision}
	}
	r.state.Revision++
	if patch.Billing != nil {
		r.state.Billing = *patch.Billing
	}
	return r.state, nil
}

func TestRouteEvidenceRevisionTrackerRetriesRecoverableConflict(t *testing.T) {
	repo := &trackedBillingEvidenceRepo{
		state:        RouteEvidencePatchState{Revision: 0},
		conflictOnce: true,
	}
	tracker := NewRouteEvidenceRevisionTracker(repo, "trace-1", repo.state)
	status := "succeeded"

	state, err := tracker.Patch(context.Background(), RouteEvidencePatch{
		Transport: &TransportPatch{TransportStatus: &status},
	})

	require.NoError(t, err)
	require.Equal(t, int64(2), state.Revision)
	require.Len(t, repo.patches, 2)
	require.Equal(t, int64(0), repo.patches[0].ExpectedRevision)
	require.Equal(t, int64(1), repo.patches[1].ExpectedRevision)
}

func (r *trackedBillingEvidenceRepo) FinalizeRouteEvidence(_ context.Context, input FinalizeRouteEvidenceInput) (SealedRouteEvidence, error) {
	r.finalizations = append(r.finalizations, input)
	return SealedRouteEvidence{Revision: input.ExpectedRevision}, nil
}

func (*trackedBillingEvidenceRepo) FinalizeRouteEvidenceFromTerminalization(context.Context, FinalizeRouteEvidenceFromTerminalizationInput) (int, error) {
	return 0, nil
}

func TestRouteTraceRecordsRedactedAttempts(t *testing.T) {
	hashKey := []byte(strings.Repeat("h", 32))
	trace := NewRouteTrace(EvaluationContext{RouteTraceID: "server-generated-trace"}, RouteTraceConfig{HashKey: hashKey})

	trace.RecordAttempt(RouteAttempt{Provider: "openai", AccountID: 12, ChannelID: 4, ResolvedModel: "gpt-5.4", Region: "cn-east", ErrorCode: "429"})
	trace.RecordAttempt(RouteAttempt{Provider: "openai", AccountID: 13, ChannelID: 4, ResolvedModel: "gpt-5.4", Region: "cn-east"})

	got := trace.Snapshot()
	require.Equal(t, 2, got.Attempts)
	require.Len(t, got.FallbackChain, 2)
	require.Equal(t, 1, got.FallbackChain[0].Ordinal)
	require.Equal(t, "openai", got.FallbackChain[0].Provider)
	require.Equal(t, RedactedResourceRef("account", 12, hashKey), got.FallbackChain[0].AccountPoolRef)
	require.Equal(t, RedactedResourceRef("channel", 4, hashKey), got.FallbackChain[0].ChannelRef)
	require.Equal(t, "429", got.FallbackChain[0].ErrorCode)

	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"account_id":12`)
	require.NotContains(t, string(encoded), `"channel_id":4`)
}

func TestRouteTraceUpdatesLatestAttemptError(t *testing.T) {
	trace := NewRouteTrace(EvaluationContext{}, RouteTraceConfig{HashKey: []byte(strings.Repeat("h", 32))})
	trace.RecordAttempt(RouteAttempt{Provider: "gemini", AccountID: 12, ResolvedModel: "gemini-2.5-pro", Region: "cn-east"})

	trace.RecordLatestAttemptError("503")
	got := trace.Snapshot()

	require.Equal(t, 1, got.Attempts)
	require.Len(t, got.FallbackChain, 1)
	require.Equal(t, "503", got.FallbackChain[0].ErrorCode)
}

func TestRouteTraceBuildsSealReadyAttemptChain(t *testing.T) {
	trace := NewRouteTrace(EvaluationContext{ExpectedModelAlias: "route-a", ExpectedRouteProfile: "route-v42"}, RouteTraceConfig{
		HashKey: []byte(strings.Repeat("h", 32)), Region: "cn-east",
	})
	trace.RecordAttempt(RouteAttempt{Provider: "qwen", AccountID: 12, ChannelID: 4, ResolvedModel: "model-a", Region: "cn-east"})
	trace.RecordLatestAttemptError("429")
	trace.RecordAttempt(RouteAttempt{Provider: "qwen", AccountID: 13, ChannelID: 5, ResolvedModel: "model-a", Region: "cn-east"})
	trace.FinalizeLatestAttempt("succeeded", time.Now())

	chain := trace.Snapshot().FallbackChain
	require.Len(t, chain, 2)
	require.Equal(t, "primary", chain[0].DispatchMode)
	require.Equal(t, "fallback", chain[1].DispatchMode)
	require.Nil(t, chain[0].ParentAttemptIndex)
	require.Equal(t, 1, *chain[1].ParentAttemptIndex)
	require.Equal(t, "route-a", chain[1].RequestedModel)
	require.Len(t, chain[1].RouteRuleHash, 64)
	require.Equal(t, "upstream_failed", chain[0].Outcome)
	require.Equal(t, "succeeded", chain[1].Outcome)
	require.False(t, chain[0].StartedAt.IsZero())
	require.False(t, chain[1].FinishedAt.IsZero())
}

func TestRouteTraceContextRoundTrip(t *testing.T) {
	trace := NewRouteTrace(EvaluationContext{}, RouteTraceConfig{HashKey: []byte(strings.Repeat("h", 32))})
	ctx := WithRouteTrace(context.Background(), trace)

	got, ok := RouteTraceFromContext(ctx)
	require.True(t, ok)
	require.Same(t, trace, got)
}

func TestRouteTraceRecordsAttemptsConcurrently(t *testing.T) {
	const attempts = 32
	trace := NewRouteTrace(EvaluationContext{}, RouteTraceConfig{HashKey: []byte(strings.Repeat("h", 32))})

	var group sync.WaitGroup
	group.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(accountID int64) {
			defer group.Done()
			trace.RecordAttempt(RouteAttempt{Provider: "openai", AccountID: accountID, ResolvedModel: "gpt-5.4", Region: "cn-east"})
		}(int64(i + 1))
	}
	group.Wait()

	got := trace.Snapshot()
	require.Equal(t, attempts, got.Attempts)
	require.Len(t, got.FallbackChain, attempts)
}

func TestPatchRouteEvidenceRejectsStaleRevision(t *testing.T) {
	current := RouteEvidencePatchState{
		Revision:  3,
		Transport: TransportPatch{Provider: stringPointer("qwen")},
	}

	_, err := MergeRouteEvidencePatch(current, RouteEvidencePatch{
		ExpectedRevision: 2,
		Transport:        &TransportPatch{ResolvedModel: stringPointer("qwen3-coder")},
	})

	var conflict *RouteEvidenceRevisionConflict
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, int64(3), conflict.CurrentRevision)
}

func TestPatchRouteEvidenceRejectsTerminalUnsealedState(t *testing.T) {
	_, err := MergeRouteEvidencePatch(RouteEvidencePatchState{
		Revision: 1,
		Terminal: true,
	}, RouteEvidencePatch{
		ExpectedRevision: 1,
		Transport:        &TransportPatch{Provider: stringPointer("qwen")},
	})
	require.ErrorIs(t, err, ErrRouteEvidenceNotOpen)
}

func TestPatchRouteEvidenceAllowsNullToValueOnce(t *testing.T) {
	finishedAt := time.Date(2026, 7, 28, 1, 2, 3, 0, time.UTC)
	current := RouteEvidencePatchState{Revision: 0}
	patch := RouteEvidencePatch{
		ExpectedRevision: 0,
		Transport: &TransportPatch{
			Provider:        stringPointer("deepseek"),
			ResolvedModel:   stringPointer("deepseek-v3.2"),
			FinishedAt:      &finishedAt,
			TransportStatus: stringPointer("succeeded"),
		},
	}

	updated, err := MergeRouteEvidencePatch(current, patch)
	require.NoError(t, err)
	require.Equal(t, int64(1), updated.Revision)
	require.Equal(t, "deepseek", *updated.Transport.Provider)

	patch.ExpectedRevision = updated.Revision
	patch.Transport.Provider = stringPointer("qwen")
	_, err = MergeRouteEvidencePatch(updated, patch)
	require.ErrorIs(t, err, ErrRouteEvidenceFieldImmutable)
}

func TestPatchRouteEvidenceCannotMutateIdentityOrClearField(t *testing.T) {
	current := RouteEvidencePatchState{
		Revision: 1,
		Identity: RouteEvidenceIdentity{
			RouteTraceID: "018f4f20-3d12-7e50-9000-000000000001",
			RunID:        "018f4f20-3d12-7e50-9000-000000000002",
			SampleID:     "018f4f20-3d12-7e50-9000-000000000003",
			AssignmentID: "018f4f20-3d12-7e50-9000-000000000004",
		},
		Transport: TransportPatch{Provider: stringPointer("qwen")},
	}
	mutatedIdentity := current.Identity
	mutatedIdentity.SampleID = "018f4f20-3d12-7e50-9000-000000000099"

	_, err := MergeRouteEvidencePatch(current, RouteEvidencePatch{
		ExpectedRevision: 1,
		Identity:         &mutatedIdentity,
	})
	require.ErrorIs(t, err, ErrRouteEvidenceIdentityConflict)

	_, err = MergeRouteEvidencePatch(current, RouteEvidencePatch{
		ExpectedRevision: 1,
		Transport:        &TransportPatch{Provider: stringPointer("")},
	})
	require.True(t, errors.Is(err, ErrRouteEvidenceFieldImmutable))
}

func TestAttachEvaluationBillingEvidenceUsesTrackedCASPatch(t *testing.T) {
	repo := &trackedBillingEvidenceRepo{state: RouteEvidencePatchState{Revision: 4}}
	tracker := NewRouteEvidenceRevisionTracker(repo, "trace-1", repo.state)
	ctx := WithEvaluationContext(context.Background(), EvaluationContext{RouteTraceID: "trace-1"})
	ctx = WithEvaluationEvidenceRepository(ctx, repo)
	ctx = WithRouteEvidenceRevisionTracker(ctx, tracker)
	ttft, latency := 12, 42

	attachEvaluationBillingEvidence(ctx, &UsageLog{
		InputTokens: 7, OutputTokens: 3, FirstTokenMs: &ttft, DurationMs: &latency,
		ActualCost: 0.00125,
	}, "stop")

	require.Zero(t, repo.legacyCalls)
	require.Len(t, repo.patches, 1)
	patch := repo.patches[0]
	require.Equal(t, int64(4), patch.ExpectedRevision)
	require.Equal(t, 7, *patch.Billing.InputTokens)
	require.Equal(t, 3, *patch.Billing.OutputTokens)
	require.True(t, decimal.RequireFromString("0.00125").Equal(*patch.Billing.BilledAmount))
	require.Equal(t, "complete", *patch.Billing.BillingStatus)
}

func TestAttachEvaluationBillingEvidenceNormalizesMissingFinishReason(t *testing.T) {
	repo := &trackedBillingEvidenceRepo{state: RouteEvidencePatchState{Revision: 4}}
	tracker := NewRouteEvidenceRevisionTracker(repo, "trace-1", repo.state)
	ctx := WithEvaluationContext(context.Background(), EvaluationContext{RouteTraceID: "trace-1"})
	ctx = WithEvaluationEvidenceRepository(ctx, repo)
	ctx = WithRouteEvidenceRevisionTracker(ctx, tracker)

	attachEvaluationBillingEvidence(ctx, &UsageLog{InputTokens: 7, OutputTokens: 3}, "")

	require.Len(t, repo.patches, 1)
	require.NotNil(t, repo.patches[0].Billing.FinishReason)
	require.Equal(t, "completed", *repo.patches[0].Billing.FinishReason)
}

func TestAttachEvaluationBillingEvidenceFinalizesSucceededEvidenceAfterBillingCompletes(t *testing.T) {
	succeeded := "succeeded"
	incomplete := "incomplete"
	repo := &trackedBillingEvidenceRepo{state: RouteEvidencePatchState{
		Identity:  RouteEvidenceIdentity{RouteTraceID: "trace-1", LeaseEpoch: 7},
		Revision:  4,
		Transport: TransportPatch{TransportStatus: &succeeded},
		Billing:   BillingPatch{BillingStatus: &incomplete},
	}}
	tracker := NewRouteEvidenceRevisionTracker(repo, "trace-1", repo.state)
	ctx := WithEvaluationContext(context.Background(), EvaluationContext{RouteTraceID: "trace-1"})
	ctx = WithEvaluationEvidenceRepository(ctx, repo)
	ctx = WithRouteEvidenceRevisionTracker(ctx, tracker)

	attachEvaluationBillingEvidence(ctx, &UsageLog{InputTokens: 7, OutputTokens: 3}, "stop")

	require.Equal(t, []FinalizeRouteEvidenceInput{{
		RouteTraceID: "trace-1", ExpectedRevision: 5, LeaseEpoch: 7,
	}}, repo.finalizations)
}

func stringPointer(value string) *string {
	return &value
}
