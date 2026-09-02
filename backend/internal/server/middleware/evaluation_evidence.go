package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const evaluationEvidencePersistenceTimeout = 5 * time.Second

var (
	evaluationGatewayDigestOnce sync.Once
	evaluationGatewayDigest     string
	evaluationGatewayDigestErr  error
)

func EvaluationEvidencePersistenceFailureCount() uint64 {
	return service.EvaluationEvidencePersistenceFailureCount()
}

func NewEvaluationEvidenceMiddleware(repo service.EvaluationEvidenceRepository) EvaluationEvidenceMiddleware {
	return EvaluationEvidenceMiddleware(EvaluationEvidence(repo))
}

func EvaluationEvidence(repo service.EvaluationEvidenceRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request == nil {
			c.Next()
			return
		}

		evaluation, ok := service.EvaluationContextFromContext(c.Request.Context())
		if !ok || repo == nil {
			c.Next()
			return
		}

		startedAt := time.Now()
		ctx := service.WithEvaluationEvidenceRepository(c.Request.Context(), repo)
		c.Request = c.Request.WithContext(ctx)
		if trustedRepo, trusted := repo.(service.TrustedEvaluationEvidenceRepository); trusted {
			if !createTrustedRouteEvidenceBeforeDispatch(c, trustedRepo, evaluation, startedAt) {
				return
			}
		}
		c.Next()

		trace, _ := service.RouteTraceFromContext(c.Request.Context())
		snapshot := trace.Snapshot()
		finishedAt := time.Now()
		trace.FinalizeLatestAttempt(classifyEvaluationTransportStatus(c.Writer.Status(), c.Request.Context().Err(), snapshot), finishedAt)
		snapshot = trace.Snapshot()
		evidence := finalizeEvaluationRouteEvidence(c.Request.Context(), evaluation, snapshot, c.Writer.Status(), startedAt, finishedAt)
		persistenceCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), evaluationEvidencePersistenceTimeout)
		defer cancel()
		var persistErr error
		var patchedState service.RouteEvidencePatchState
		if tracker, tracked := service.RouteEvidenceRevisionTrackerFromContext(c.Request.Context()); tracked {
			patchedState, persistErr = tracker.Patch(persistenceCtx, routeEvidenceTransportPatch(evidence))
			if persistErr == nil {
				if finalizer, ok := repo.(service.TrustedEvaluationEvidenceFinalizer); ok {
					_, persistErr = finalizer.FinalizeRouteEvidence(persistenceCtx, service.FinalizeRouteEvidenceInput{
						RouteTraceID: evaluation.RouteTraceID, ExpectedRevision: patchedState.Revision,
						LeaseEpoch: patchedState.Identity.LeaseEpoch,
					})
				}
			}
		} else {
			persistErr = repo.UpsertTransport(persistenceCtx, evidence)
		}
		if persistErr != nil {
			service.RecordEvaluationEvidencePersistenceFailure()
			logger.FromContext(c.Request.Context()).Warn("evaluation route evidence persistence failed",
				zap.String("route_trace_id", evaluation.RouteTraceID),
				zap.Error(persistErr),
			)
		}
	}
}

func createTrustedRouteEvidenceBeforeDispatch(c *gin.Context, repo service.TrustedEvaluationEvidenceRepository, evaluation service.EvaluationContext, startedAt time.Time) bool {
	rawBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		AbortWithError(c, http.StatusUnprocessableEntity, "EVALUATION_REQUEST_SEMANTICS_INVALID", "Evaluation request semantics are invalid")
		return false
	}
	c.Request.Body = io.NopCloser(strings.NewReader(string(rawBody)))
	semantics, err := service.DeriveSingleRequestSemantics(rawBody)
	if err != nil {
		AbortWithError(c, http.StatusUnprocessableEntity, "EVALUATION_REQUEST_SEMANTICS_INVALID", "Evaluation request semantics are invalid")
		return false
	}
	requestID := evaluationEvidenceRequestID(c.Request.Context())
	if requestID == "" {
		requestID = "local:" + uuid.NewString()
	}
	region := "default"
	if trace, ok := service.RouteTraceFromContext(c.Request.Context()); ok {
		if configured := strings.TrimSpace(trace.Snapshot().Region); configured != "" {
			region = configured
		}
	}
	gatewayDigest, err := evaluationGatewayImageDigest()
	if err != nil {
		AbortWithError(c, http.StatusUnprocessableEntity, "EVALUATION_GATEWAY_IDENTITY_UNAVAILABLE", "Evaluation gateway identity is unavailable")
		return false
	}
	opened, err := repo.CreateOpen(c.Request.Context(), service.CreateOpenRouteEvidenceInput{
		RouteTraceID: evaluation.RouteTraceID, RunID: evaluation.RunID, SampleID: evaluation.SampleID,
		APIKeyID: evaluation.APIKeyID, RequestID: requestID, RequestedModel: evaluation.ExpectedModelAlias,
		RouteProfileVersion: evaluation.ExpectedRouteProfile, RequestOrdinal: 0, Semantics: semantics,
		GatewayServiceIdentity: "sub2api-gateway", GatewayImageDigest: gatewayDigest,
		Region: region, StartedAt: startedAt,
	})
	if err != nil {
		AbortWithError(c, http.StatusUnprocessableEntity, "EVALUATION_REQUEST_PROTOCOL_FAILED", "Evaluation request does not match its frozen contract")
		return false
	}
	tracker := service.NewRouteEvidenceRevisionTracker(repo, evaluation.RouteTraceID, opened)
	c.Request = c.Request.WithContext(service.WithRouteEvidenceRevisionTracker(c.Request.Context(), tracker))
	return true
}

func routeEvidenceTransportPatch(evidence service.RouteEvidence) service.RouteEvidencePatch {
	transportStatus := evidence.TransportStatus
	patch := service.TransportPatch{TransportStatus: &transportStatus, FinishedAt: evidence.FinishedAt}
	if evidence.ResolvedModel != "" {
		patch.ResolvedModel = routeEvidenceMiddlewareStringPointer(evidence.ResolvedModel)
	}
	if evidence.Provider != "" {
		patch.Provider = routeEvidenceMiddlewareStringPointer(evidence.Provider)
	}
	if evidence.ChannelRef != "" {
		patch.ChannelRef = routeEvidenceMiddlewareStringPointer(evidence.ChannelRef)
	}
	if evidence.AccountPoolRef != "" {
		patch.AccountPoolRef = routeEvidenceMiddlewareStringPointer(evidence.AccountPoolRef)
	}
	if evidence.Attempts > 0 {
		patch.Attempts = &evidence.Attempts
	}
	if len(evidence.FallbackChain) > 0 {
		fallback := append([]service.RouteFallbackEntry(nil), evidence.FallbackChain...)
		patch.FallbackChain = &fallback
	}
	if evidence.ErrorCode != "" {
		patch.ErrorCode = routeEvidenceMiddlewareStringPointer(evidence.ErrorCode)
	}
	return service.RouteEvidencePatch{Transport: &patch}
}

func evaluationGatewayImageDigest() (string, error) {
	evaluationGatewayDigestOnce.Do(func() {
		executable, err := os.Executable()
		if err != nil {
			evaluationGatewayDigestErr = err
			return
		}
		evaluationGatewayDigest, evaluationGatewayDigestErr = digestEvaluationGatewayExecutable(executable)
	})
	return evaluationGatewayDigest, evaluationGatewayDigestErr
}

func digestEvaluationGatewayExecutable(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open evaluation gateway executable: %w", err)
	}
	defer func() { _ = file.Close() }()
	digest := sha256.New()
	written, err := io.Copy(digest, file)
	if err != nil {
		return "", fmt.Errorf("hash evaluation gateway executable: %w", err)
	}
	if written == 0 {
		return "", errors.New("evaluation gateway executable is empty")
	}
	return "sub2api-gateway@sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func routeEvidenceMiddlewareStringPointer(value string) *string {
	return &value
}

func finalizeEvaluationRouteEvidence(
	ctx context.Context,
	evaluation service.EvaluationContext,
	snapshot service.RouteEvidence,
	status int,
	startedAt time.Time,
	finishedAt time.Time,
) service.RouteEvidence {
	requestedModel := evaluation.ExpectedModelAlias
	if model, ok := service.RequestedPublicModelFromContext(ctx); ok {
		requestedModel = model
	}
	resolvedModel, _ := service.ResolvedUpstreamModelFromContext(ctx)
	provider, _ := service.ResolvedTargetPlatformFromContext(ctx)

	var latest service.RouteFallbackEntry
	if count := len(snapshot.FallbackChain); count > 0 {
		latest = snapshot.FallbackChain[count-1]
	}
	if resolvedModel == "" {
		resolvedModel = latest.ResolvedModel
	}
	if provider == "" {
		provider = latest.Provider
	}
	region := latest.Region
	if region == "" {
		region = snapshot.Region
	}

	return service.RouteEvidence{
		RouteTraceID:        evaluation.RouteTraceID,
		EvaluationRunID:     evaluation.RunID,
		SampleID:            evaluation.SampleID,
		APIKeyID:            evaluation.APIKeyID,
		RequestID:           evaluationEvidenceRequestID(ctx),
		RequestedModel:      requestedModel,
		ResolvedModel:       resolvedModel,
		RouteProfileVersion: evaluation.ExpectedRouteProfile,
		Provider:            provider,
		ChannelRef:          latest.ChannelRef,
		AccountPoolRef:      latest.AccountPoolRef,
		Region:              region,
		Attempts:            snapshot.Attempts,
		FallbackChain:       append([]service.RouteFallbackEntry(nil), snapshot.FallbackChain...),
		TransportStatus:     classifyEvaluationTransportStatus(status, ctx.Err(), snapshot),
		ErrorCode:           latest.ErrorCode,
		StartedAt:           startedAt,
		FinishedAt:          &finishedAt,
	}
}

func classifyEvaluationTransportStatus(status int, requestErr error, trace service.RouteEvidence) string {
	if status == 499 || errors.Is(requestErr, context.Canceled) {
		return "client_cancelled"
	}
	if status >= http.StatusOK && status < http.StatusBadRequest {
		return "succeeded"
	}
	if count := len(trace.FallbackChain); count > 0 && strings.TrimSpace(trace.FallbackChain[count-1].ErrorCode) != "" {
		return "upstream_failed"
	}
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return "protocol_failed"
	}
	if status >= http.StatusInternalServerError {
		return "gateway_failed"
	}
	return "started"
}

func evaluationEvidenceRequestID(ctx context.Context) string {
	if value, _ := ctx.Value(ctxkey.ClientRequestID).(string); strings.TrimSpace(value) != "" {
		return "client:" + strings.TrimSpace(value)
	}
	if value, _ := ctx.Value(ctxkey.RequestID).(string); strings.TrimSpace(value) != "" {
		return "local:" + strings.TrimSpace(value)
	}
	return ""
}
