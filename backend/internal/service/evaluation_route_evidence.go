package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

var ErrRouteEvidenceIdentityConflict = errors.New("route evidence identity conflict")

var (
	ErrRouteEvidenceFieldImmutable = errors.New("route evidence field is immutable once set")
	ErrRouteEvidenceNotOpen        = errors.New("route evidence is not open")
)

var evaluationEvidencePersistenceFailures atomic.Uint64

const routeEvidencePatchMaxRevisionRetries = 3

func RecordEvaluationEvidencePersistenceFailure() {
	evaluationEvidencePersistenceFailures.Add(1)
}

func EvaluationEvidencePersistenceFailureCount() uint64 {
	return evaluationEvidencePersistenceFailures.Load()
}

type EvaluationEvidenceRepository interface {
	UpsertTransport(ctx context.Context, evidence RouteEvidence) error
	AttachBilling(ctx context.Context, traceID string, usage RouteUsageEvidence) error
}

type TrustedEvaluationEvidenceRepository interface {
	CreateOpen(ctx context.Context, input CreateOpenRouteEvidenceInput) (RouteEvidencePatchState, error)
	PatchRouteEvidence(ctx context.Context, traceID string, patch RouteEvidencePatch) (RouteEvidencePatchState, error)
}

type TrustedEvaluationEvidenceFinalizer interface {
	FinalizeRouteEvidence(ctx context.Context, input FinalizeRouteEvidenceInput) (SealedRouteEvidence, error)
	FinalizeRouteEvidenceFromTerminalization(ctx context.Context, input FinalizeRouteEvidenceFromTerminalizationInput) (int, error)
}

// RouteEvidenceTerminalizationRepository exposes pending terminal run events
// to the system-owned evidence finalizer.
type RouteEvidenceTerminalizationRepository interface {
	ListPendingTerminalizations(ctx context.Context, limit int) ([]RouteEvidenceTerminalizationEvent, error)
}

type RouteTraceConfig struct {
	HashKey []byte
	Region  string
}

type RouteAttempt struct {
	Provider      string
	AccountID     int64
	ChannelID     int64
	ResolvedModel string
	Region        string
	ErrorCode     string
}

type RouteFallbackEntry struct {
	Ordinal            int        `json:"ordinal"`
	ParentAttemptIndex *int       `json:"parent_attempt_index"`
	DispatchMode       string     `json:"dispatch_mode"`
	RouteRuleHash      string     `json:"route_rule_hash"`
	RequestedModel     string     `json:"requested_model"`
	Provider           string     `json:"provider"`
	AccountPoolRef     string     `json:"account_pool_ref"`
	ChannelRef         string     `json:"channel_ref"`
	ResolvedModel      string     `json:"resolved_model"`
	Region             string     `json:"region"`
	Outcome            string     `json:"outcome"`
	ErrorCode          string     `json:"error_code"`
	StartedAt          time.Time  `json:"started_at"`
	FinishedAt         *time.Time `json:"finished_at"`
}

type RouteEvidence struct {
	RouteTraceID        string               `json:"route_trace_id"`
	EvaluationRunID     string               `json:"evaluation_run_id"`
	SampleID            string               `json:"sample_id"`
	APIKeyID            int64                `json:"api_key_id"`
	RequestID           string               `json:"request_id"`
	RequestedModel      string               `json:"requested_model"`
	ResolvedModel       string               `json:"resolved_model"`
	RouteProfileVersion string               `json:"route_profile_version"`
	Provider            string               `json:"provider"`
	ChannelRef          string               `json:"channel_ref"`
	AccountPoolRef      string               `json:"account_pool_ref"`
	Region              string               `json:"region"`
	Attempts            int                  `json:"attempts"`
	FallbackChain       []RouteFallbackEntry `json:"fallback_chain"`
	TransportStatus     string               `json:"transport_status"`
	ErrorCode           string               `json:"error_code"`
	StartedAt           time.Time            `json:"started_at"`
	FinishedAt          *time.Time           `json:"finished_at"`
}

type RouteUsageEvidence struct {
	InputTokens  int
	OutputTokens int
	TTFT         *int
	Latency      *int
	BilledAmount decimal.Decimal
	FinishReason string
}

type CreateOpenRouteEvidenceInput struct {
	RouteTraceID           string
	RunID                  string
	SampleID               string
	APIKeyID               int64
	RequestID              string
	RequestedModel         string
	RouteProfileVersion    string
	RequestOrdinal         int
	Semantics              RequestSemantics
	GatewayServiceIdentity string
	GatewayImageDigest     string
	Region                 string
	StartedAt              time.Time
}

type RouteEvidenceIdentity struct {
	RouteTraceID   string
	RunID          string
	SampleID       string
	APIKeyID       int64
	AssignmentID   string
	RequestOrdinal int
	LeaseEpoch     int64
}

type FinalizeRouteEvidenceInput struct {
	RouteTraceID     string
	ExpectedRevision int64
	LeaseEpoch       int64
}

type SealedRouteEvidence struct {
	Revision     int64
	PayloadHash  string
	SigningKeyID string
	PayloadHMAC  string
	SealedAt     time.Time
}

type FinalizeRouteEvidenceFromTerminalizationInput struct {
	EventID      uuid.UUID
	RunID        uuid.UUID
	ControlEpoch int64
}

type RouteEvidenceTerminalizationEvent struct {
	ID           uuid.UUID
	RunID        uuid.UUID
	ControlEpoch int64
}

type TransportPatch struct {
	ResolvedModel   *string
	Provider        *string
	ChannelRef      *string
	AccountPoolRef  *string
	Attempts        *int
	FallbackChain   *[]RouteFallbackEntry
	TransportStatus *string
	ErrorCode       *string
	FinishedAt      *time.Time
}

type BillingPatch struct {
	InputTokens   *int
	OutputTokens  *int
	TTFT          *int
	Latency       *int
	BilledAmount  *decimal.Decimal
	FinishReason  *string
	BillingStatus *string
}

type RouteEvidencePatch struct {
	ExpectedRevision int64
	Identity         *RouteEvidenceIdentity
	Transport        *TransportPatch
	Billing          *BillingPatch
}

type RouteEvidencePatchState struct {
	Identity  RouteEvidenceIdentity
	Revision  int64
	Terminal  bool
	Sealed    bool
	Transport TransportPatch
	Billing   BillingPatch
}

type RouteEvidenceRevisionConflict struct {
	CurrentRevision int64
}

type RouteEvidenceRevisionTracker struct {
	mu      sync.Mutex
	repo    TrustedEvaluationEvidenceRepository
	traceID string
	state   RouteEvidencePatchState
}

func NewRouteEvidenceRevisionTracker(repo TrustedEvaluationEvidenceRepository, traceID string, state RouteEvidencePatchState) *RouteEvidenceRevisionTracker {
	return &RouteEvidenceRevisionTracker{repo: repo, traceID: strings.TrimSpace(traceID), state: state}
}

func (t *RouteEvidenceRevisionTracker) Patch(ctx context.Context, patch RouteEvidencePatch) (RouteEvidencePatchState, error) {
	if t == nil || t.repo == nil || t.traceID == "" {
		return RouteEvidencePatchState{}, ErrRouteEvidenceNotOpen
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for attempt := 0; attempt < routeEvidencePatchMaxRevisionRetries; attempt++ {
		patch.ExpectedRevision = t.state.Revision
		updated, err := t.repo.PatchRouteEvidence(ctx, t.traceID, patch)
		if err == nil {
			t.state = updated
			return updated, nil
		}
		var conflict *RouteEvidenceRevisionConflict
		if !errors.As(err, &conflict) {
			return t.state, err
		}
		if conflict.CurrentRevision < t.state.Revision {
			return t.state, err
		}
		t.state.Revision = conflict.CurrentRevision
		if ctx != nil && ctx.Err() != nil {
			return t.state, ctx.Err()
		}
	}
	return t.state, &RouteEvidenceRevisionConflict{CurrentRevision: t.state.Revision}
}

func (t *RouteEvidenceRevisionTracker) State() RouteEvidencePatchState {
	if t == nil {
		return RouteEvidencePatchState{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

func (e *RouteEvidenceRevisionConflict) Error() string {
	return fmt.Sprintf("route evidence revision conflict: current revision is %d", e.CurrentRevision)
}

func MergeRouteEvidencePatch(current RouteEvidencePatchState, patch RouteEvidencePatch) (RouteEvidencePatchState, error) {
	if current.Terminal || current.Sealed {
		return current, ErrRouteEvidenceNotOpen
	}
	if patch.Identity != nil {
		return current, ErrRouteEvidenceIdentityConflict
	}

	updated := current
	changed := false
	var err error
	if patch.Transport != nil {
		changed, err = mergeTransportPatch(&updated.Transport, *patch.Transport)
		if err != nil {
			return current, err
		}
	}
	if patch.Billing != nil {
		billingChanged, mergeErr := mergeBillingPatch(&updated.Billing, *patch.Billing)
		if mergeErr != nil {
			return current, mergeErr
		}
		changed = changed || billingChanged
	}
	if !changed {
		return current, nil
	}
	if patch.ExpectedRevision != current.Revision {
		return current, &RouteEvidenceRevisionConflict{CurrentRevision: current.Revision}
	}
	updated.Revision++
	return updated, nil
}

func mergeTransportPatch(current *TransportPatch, patch TransportPatch) (bool, error) {
	changed := false
	fields := []struct {
		current any
		patch   any
		assign  func()
	}{
		{current.ResolvedModel, patch.ResolvedModel, func() { current.ResolvedModel = patch.ResolvedModel }},
		{current.Provider, patch.Provider, func() { current.Provider = patch.Provider }},
		{current.ChannelRef, patch.ChannelRef, func() { current.ChannelRef = patch.ChannelRef }},
		{current.AccountPoolRef, patch.AccountPoolRef, func() { current.AccountPoolRef = patch.AccountPoolRef }},
		{current.Attempts, patch.Attempts, func() { current.Attempts = patch.Attempts }},
		{current.FallbackChain, patch.FallbackChain, func() { current.FallbackChain = patch.FallbackChain }},
		{current.ErrorCode, patch.ErrorCode, func() { current.ErrorCode = patch.ErrorCode }},
		{current.FinishedAt, patch.FinishedAt, func() { current.FinishedAt = patch.FinishedAt }},
	}
	for _, field := range fields {
		fieldChanged, err := mergeSetOnceField(field.current, field.patch, field.assign)
		if err != nil {
			return false, err
		}
		changed = changed || fieldChanged
	}
	statusChanged, err := mergeStatusField(&current.TransportStatus, patch.TransportStatus, "started", map[string]struct{}{
		"succeeded": {}, "upstream_failed": {}, "gateway_failed": {}, "protocol_failed": {}, "client_cancelled": {},
	})
	return changed || statusChanged, err
}

func mergeBillingPatch(current *BillingPatch, patch BillingPatch) (bool, error) {
	changed := false
	fields := []struct {
		current any
		patch   any
		assign  func()
	}{
		{current.InputTokens, patch.InputTokens, func() { current.InputTokens = patch.InputTokens }},
		{current.OutputTokens, patch.OutputTokens, func() { current.OutputTokens = patch.OutputTokens }},
		{current.TTFT, patch.TTFT, func() { current.TTFT = patch.TTFT }},
		{current.Latency, patch.Latency, func() { current.Latency = patch.Latency }},
		{current.BilledAmount, patch.BilledAmount, func() { current.BilledAmount = patch.BilledAmount }},
		{current.FinishReason, patch.FinishReason, func() { current.FinishReason = patch.FinishReason }},
	}
	for _, field := range fields {
		fieldChanged, err := mergeSetOnceField(field.current, field.patch, field.assign)
		if err != nil {
			return false, err
		}
		changed = changed || fieldChanged
	}
	statusChanged, err := mergeStatusField(&current.BillingStatus, patch.BillingStatus, "incomplete", map[string]struct{}{
		"complete": {}, "not_applicable": {},
	})
	return changed || statusChanged, err
}

func mergeSetOnceField(current, patch any, assign func()) (bool, error) {
	if isNilPatchValue(patch) {
		return false, nil
	}
	if patchClearsValue(patch) {
		return false, ErrRouteEvidenceFieldImmutable
	}
	if isNilPatchValue(current) {
		assign()
		return true, nil
	}
	if reflect.DeepEqual(current, patch) {
		return false, nil
	}
	return false, ErrRouteEvidenceFieldImmutable
}

func mergeStatusField(current **string, patch *string, initial string, terminals map[string]struct{}) (bool, error) {
	if patch == nil {
		return false, nil
	}
	value := strings.TrimSpace(*patch)
	if value == "" {
		return false, ErrRouteEvidenceFieldImmutable
	}
	if *current == nil {
		if value != initial {
			if _, ok := terminals[value]; !ok {
				return false, ErrRouteEvidenceFieldImmutable
			}
		}
		*current = patch
		return true, nil
	}
	if strings.TrimSpace(**current) == value {
		return false, nil
	}
	if strings.TrimSpace(**current) == initial {
		if _, ok := terminals[value]; ok {
			*current = patch
			return true, nil
		}
	}
	return false, ErrRouteEvidenceFieldImmutable
}

func isNilPatchValue(value any) bool {
	if value == nil {
		return true
	}
	ref := reflect.ValueOf(value)
	return ref.Kind() == reflect.Pointer && ref.IsNil()
}

func patchClearsValue(value any) bool {
	ref := reflect.ValueOf(value)
	if ref.Kind() != reflect.Pointer || ref.IsNil() {
		return false
	}
	elem := ref.Elem()
	return elem.Kind() == reflect.String && strings.TrimSpace(elem.String()) == ""
}

type evaluationEvidenceRepositoryContextKey struct{}

type routeEvidenceRevisionTrackerContextKey struct{}

func WithEvaluationEvidenceRepository(ctx context.Context, repo EvaluationEvidenceRepository) context.Context {
	return context.WithValue(ctx, evaluationEvidenceRepositoryContextKey{}, repo)
}

func EvaluationEvidenceRepositoryFromContext(ctx context.Context) (EvaluationEvidenceRepository, bool) {
	repo, ok := ctx.Value(evaluationEvidenceRepositoryContextKey{}).(EvaluationEvidenceRepository)
	return repo, ok && repo != nil
}

func WithRouteEvidenceRevisionTracker(ctx context.Context, tracker *RouteEvidenceRevisionTracker) context.Context {
	return context.WithValue(ctx, routeEvidenceRevisionTrackerContextKey{}, tracker)
}

func RouteEvidenceRevisionTrackerFromContext(ctx context.Context) (*RouteEvidenceRevisionTracker, bool) {
	tracker, ok := ctx.Value(routeEvidenceRevisionTrackerContextKey{}).(*RouteEvidenceRevisionTracker)
	return tracker, ok && tracker != nil
}

func attachEvaluationBillingEvidence(ctx context.Context, usageLog *UsageLog, finishReason string) {
	if usageLog == nil {
		return
	}
	evaluation, ok := EvaluationContextFromContext(ctx)
	if !ok {
		return
	}
	repo, ok := EvaluationEvidenceRepositoryFromContext(ctx)
	if !ok {
		return
	}
	finishReason = strings.TrimSpace(finishReason)
	if finishReason == "" {
		finishReason = "completed"
	}

	usage := RouteUsageEvidence{
		InputTokens:  usageLog.InputTokens,
		OutputTokens: usageLog.OutputTokens,
		TTFT:         usageLog.FirstTokenMs,
		Latency:      usageLog.DurationMs,
		BilledAmount: decimal.NewFromFloat(usageLog.ActualCost),
		FinishReason: finishReason,
	}
	var err error
	if tracker, tracked := RouteEvidenceRevisionTrackerFromContext(ctx); tracked {
		inputTokens, outputTokens := usage.InputTokens, usage.OutputTokens
		billedAmount := usage.BilledAmount
		billingStatus := "complete"
		patch := BillingPatch{
			InputTokens: &inputTokens, OutputTokens: &outputTokens,
			TTFT: usage.TTFT, Latency: usage.Latency,
			BilledAmount: &billedAmount, BillingStatus: &billingStatus,
		}
		if usage.FinishReason != "" {
			finishReason := usage.FinishReason
			patch.FinishReason = &finishReason
		}
		var state RouteEvidencePatchState
		state, err = tracker.Patch(ctx, RouteEvidencePatch{Billing: &patch})
		if err == nil && routeEvidenceReadyForFinalization(state) {
			if finalizer, ok := repo.(TrustedEvaluationEvidenceFinalizer); ok {
				_, err = finalizer.FinalizeRouteEvidence(ctx, FinalizeRouteEvidenceInput{
					RouteTraceID: evaluation.RouteTraceID, ExpectedRevision: state.Revision,
					LeaseEpoch: state.Identity.LeaseEpoch,
				})
			}
		}
	} else {
		err = repo.AttachBilling(ctx, evaluation.RouteTraceID, usage)
	}
	if err != nil {
		RecordEvaluationEvidencePersistenceFailure()
		logger.FromContext(ctx).Warn("evaluation billing evidence persistence failed",
			zap.String("route_trace_id", evaluation.RouteTraceID),
			zap.Error(err),
		)
	}
}

func routeEvidenceReadyForFinalization(state RouteEvidencePatchState) bool {
	if state.Terminal || state.Sealed || state.Transport.TransportStatus == nil {
		return false
	}
	transportStatus := strings.TrimSpace(*state.Transport.TransportStatus)
	if transportStatus == "" || transportStatus == "started" {
		return false
	}
	if transportStatus != "succeeded" {
		return true
	}
	return state.Billing.BillingStatus != nil &&
		strings.TrimSpace(*state.Billing.BillingStatus) == "complete"
}

// RouteTrace collects only redacted routing information for one evaluation request.
type RouteTrace struct {
	mu             sync.Mutex
	hashKey        []byte
	requestedModel string
	routeRuleHash  string
	evidence       RouteEvidence
}

func NewRouteTrace(evaluation EvaluationContext, cfg RouteTraceConfig) *RouteTrace {
	ruleDigest := sha256.Sum256([]byte(strings.TrimSpace(evaluation.ExpectedRouteProfile)))
	return &RouteTrace{
		hashKey:        append([]byte(nil), cfg.HashKey...),
		requestedModel: strings.TrimSpace(evaluation.ExpectedModelAlias),
		routeRuleHash:  hex.EncodeToString(ruleDigest[:]),
		evidence: RouteEvidence{
			Region: strings.TrimSpace(cfg.Region),
		},
	}
}

func (t *RouteTrace) RecordAttempt(attempt RouteAttempt) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now().UTC()
	if last := len(t.evidence.FallbackChain) - 1; last >= 0 && t.evidence.FallbackChain[last].FinishedAt == nil {
		finished := now
		t.evidence.FallbackChain[last].FinishedAt = &finished
		if t.evidence.FallbackChain[last].Outcome == "" {
			t.evidence.FallbackChain[last].Outcome = "upstream_failed"
		}
	}
	ordinal := len(t.evidence.FallbackChain) + 1
	var parent *int
	dispatchMode := "primary"
	if ordinal > 1 {
		value := ordinal - 1
		parent = &value
		dispatchMode = "fallback"
	}

	entry := RouteFallbackEntry{
		Ordinal:            ordinal,
		ParentAttemptIndex: parent,
		DispatchMode:       dispatchMode,
		RouteRuleHash:      t.routeRuleHash,
		RequestedModel:     t.requestedModel,
		Provider:           strings.TrimSpace(attempt.Provider),
		AccountPoolRef:     RedactedResourceRef("account", attempt.AccountID, t.hashKey),
		ChannelRef:         RedactedResourceRef("channel", attempt.ChannelID, t.hashKey),
		ResolvedModel:      strings.TrimSpace(attempt.ResolvedModel),
		Region:             strings.TrimSpace(attempt.Region),
		ErrorCode:          strings.TrimSpace(attempt.ErrorCode),
		StartedAt:          now,
	}
	if entry.ErrorCode != "" {
		entry.Outcome = "upstream_failed"
	}
	t.evidence.FallbackChain = append(t.evidence.FallbackChain, entry)
	t.evidence.Attempts = len(t.evidence.FallbackChain)
}

func (t *RouteTrace) RecordLatestAttemptError(errorCode string) {
	if t == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if last := len(t.evidence.FallbackChain) - 1; last >= 0 {
		t.evidence.FallbackChain[last].ErrorCode = strings.TrimSpace(errorCode)
		t.evidence.FallbackChain[last].Outcome = "upstream_failed"
		finished := time.Now().UTC()
		t.evidence.FallbackChain[last].FinishedAt = &finished
	}
}

func (t *RouteTrace) FinalizeLatestAttempt(outcome string, finishedAt time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	last := len(t.evidence.FallbackChain) - 1
	if last < 0 {
		return
	}
	entry := &t.evidence.FallbackChain[last]
	if finishedAt.Before(entry.StartedAt) {
		finishedAt = entry.StartedAt
	}
	finishedAt = finishedAt.UTC()
	entry.FinishedAt = &finishedAt
	switch strings.TrimSpace(outcome) {
	case "succeeded", "upstream_failed", "protocol_failed", "gateway_failed":
		entry.Outcome = strings.TrimSpace(outcome)
	case "client_cancelled":
		entry.Outcome = "cancelled"
	default:
		entry.Outcome = "gateway_failed"
	}
}

func (t *RouteTrace) Snapshot() RouteEvidence {
	if t == nil {
		return RouteEvidence{}
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	var latest RouteFallbackEntry
	if count := len(t.evidence.FallbackChain); count > 0 {
		latest = t.evidence.FallbackChain[count-1]
	}

	return RouteEvidence{
		ResolvedModel:  latest.ResolvedModel,
		Provider:       latest.Provider,
		ChannelRef:     latest.ChannelRef,
		AccountPoolRef: latest.AccountPoolRef,
		Attempts:       t.evidence.Attempts,
		FallbackChain:  append([]RouteFallbackEntry(nil), t.evidence.FallbackChain...),
		Region:         t.evidence.Region,
	}
}

func RedactedResourceRef(kind string, id int64, key []byte) string {
	if id <= 0 {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(strings.TrimSpace(kind)))
	_, _ = mac.Write([]byte{':'})
	_, _ = mac.Write([]byte(strconv.FormatInt(id, 10)))
	return strings.TrimSpace(kind) + "_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func WithRouteTrace(ctx context.Context, trace *RouteTrace) context.Context {
	return updateRequestMetadata(ctx, false, func(md *RequestMetadata) {
		md.RouteTrace = trace
	}, nil)
}

func RouteTraceFromContext(ctx context.Context) (*RouteTrace, bool) {
	if md := metadataFromContext(ctx); md != nil && md.RouteTrace != nil {
		return md.RouteTrace, true
	}
	return nil, false
}
