//go:build unit

package middleware

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDigestEvaluationGatewayExecutableFailsForUnreadablePath(t *testing.T) {
	_, err := digestEvaluationGatewayExecutable(filepath.Join(t.TempDir(), "missing-gateway"))
	require.Error(t, err)
}

type evaluationEvidenceRepoStub struct {
	transport  service.RouteEvidence
	err        error
	calls      int
	ctxErr     error
	hasExpiry  bool
	evaluation service.EvaluationContext
}

type trustedEvaluationEvidenceRepoStub struct {
	evaluationEvidenceRepoStub
	createInput service.CreateOpenRouteEvidenceInput
	createState service.RouteEvidencePatchState
	createErr   error
	createCalls int
	patches     []service.RouteEvidencePatch
	finalize    service.FinalizeRouteEvidenceInput
	finalCalls  int
	order       []string
}

func (s *trustedEvaluationEvidenceRepoStub) FinalizeRouteEvidence(_ context.Context, input service.FinalizeRouteEvidenceInput) (service.SealedRouteEvidence, error) {
	s.finalCalls++
	s.finalize = input
	s.order = append(s.order, "finalize")
	return service.SealedRouteEvidence{Revision: input.ExpectedRevision + 1}, nil
}

func (s *trustedEvaluationEvidenceRepoStub) FinalizeRouteEvidenceFromTerminalization(context.Context, service.FinalizeRouteEvidenceFromTerminalizationInput) (int, error) {
	return 0, nil
}

func (s *trustedEvaluationEvidenceRepoStub) CreateOpen(_ context.Context, input service.CreateOpenRouteEvidenceInput) (service.RouteEvidencePatchState, error) {
	s.createCalls++
	s.createInput = input
	s.order = append(s.order, "create")
	return s.createState, s.createErr
}

func (s *trustedEvaluationEvidenceRepoStub) PatchRouteEvidence(_ context.Context, _ string, patch service.RouteEvidencePatch) (service.RouteEvidencePatchState, error) {
	s.patches = append(s.patches, patch)
	s.order = append(s.order, "patch")
	s.createState.Revision++
	return s.createState, nil
}

func (s *evaluationEvidenceRepoStub) UpsertTransport(ctx context.Context, evidence service.RouteEvidence) error {
	s.calls++
	s.transport = evidence
	s.ctxErr = ctx.Err()
	_, s.hasExpiry = ctx.Deadline()
	s.evaluation, _ = service.EvaluationContextFromContext(ctx)
	return s.err
}

func (s *evaluationEvidenceRepoStub) AttachBilling(context.Context, string, service.RouteUsageEvidence) error {
	return nil
}

func TestClassifyEvaluationTransportStatus(t *testing.T) {
	failedTrace := service.RouteEvidence{FallbackChain: []service.RouteFallbackEntry{{ErrorCode: "503"}}}

	tests := []struct {
		name       string
		status     int
		requestErr error
		trace      service.RouteEvidence
		want       string
	}{
		{name: "started", status: http.StatusContinue, want: "started"},
		{name: "succeeded", status: http.StatusOK, trace: failedTrace, want: "succeeded"},
		{name: "upstream failure", status: http.StatusBadGateway, trace: failedTrace, want: "upstream_failed"},
		{name: "protocol failure", status: http.StatusBadRequest, want: "protocol_failed"},
		{name: "client cancellation status", status: 499, want: "client_cancelled"},
		{name: "client cancellation context", status: http.StatusBadGateway, requestErr: context.Canceled, trace: failedTrace, want: "client_cancelled"},
		{name: "gateway failure", status: http.StatusInternalServerError, want: "gateway_failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, classifyEvaluationTransportStatus(tt.status, tt.requestErr, tt.trace))
		})
	}
}

func TestEvaluationEvidencePersistsFinalizedRouteAfterHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &evaluationEvidenceRepoStub{}
	evaluation := service.EvaluationContext{
		RunID:                "018f4f20-3d12-7e50-9000-000000000001",
		SampleID:             "018f4f20-3d12-7e50-9000-000000000002",
		ExpectedModelAlias:   "expected-model",
		ExpectedRouteProfile: "route-v42",
		APIKeyID:             41,
		RouteTraceID:         "trace-server-generated",
	}
	trace := service.NewRouteTrace(evaluation, service.RouteTraceConfig{HashKey: []byte("test-route-hash-key")})
	trace.RecordAttempt(service.RouteAttempt{
		Provider:      "qwen",
		AccountID:     91,
		ChannelID:     17,
		ResolvedModel: "qwen3-coder-2026-07",
		Region:        "cn-east",
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		ctx := service.WithEvaluationContext(c.Request.Context(), evaluation)
		ctx = service.WithRouteTrace(ctx, trace)
		ctx = context.WithValue(ctx, ctxkey.ClientRequestID, "client-request-123")
		ctx = service.WithCompositeRouteDecision(ctx, service.CompositeRouteDecision{
			Matched:        true,
			PublicModel:    "public-coder-alias",
			UpstreamModel:  "qwen3-coder-2026-07",
			TargetPlatform: "qwen",
		})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	router.Use(EvaluationEvidence(repo))
	router.GET("/test", func(c *gin.Context) {
		injected, ok := service.EvaluationEvidenceRepositoryFromContext(c.Request.Context())
		require.True(t, ok)
		require.Same(t, repo, injected)
		c.Status(http.StatusNoContent)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/test", nil)
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, 1, repo.calls)
	got := repo.transport
	require.Equal(t, evaluation.RouteTraceID, got.RouteTraceID)
	require.Equal(t, evaluation.RunID, got.EvaluationRunID)
	require.Equal(t, evaluation.SampleID, got.SampleID)
	require.Equal(t, evaluation.APIKeyID, got.APIKeyID)
	require.Equal(t, "client:client-request-123", got.RequestID)
	require.Equal(t, "public-coder-alias", got.RequestedModel)
	require.Equal(t, "qwen3-coder-2026-07", got.ResolvedModel)
	require.Equal(t, evaluation.ExpectedRouteProfile, got.RouteProfileVersion)
	require.Equal(t, "qwen", got.Provider)
	require.Equal(t, "cn-east", got.Region)
	require.Equal(t, 1, got.Attempts)
	require.Len(t, got.FallbackChain, 1)
	require.Equal(t, "succeeded", got.TransportStatus)
	require.WithinDuration(t, time.Now(), got.StartedAt, 2*time.Second)
	require.NotNil(t, got.FinishedAt)
}

func TestCreateOpenRejectsExactSlotMismatchBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &trustedEvaluationEvidenceRepoStub{createErr: service.ErrRequestSemanticsMismatch}
	dispatchCalls := 0
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(service.WithEvaluationContext(c.Request.Context(), service.EvaluationContext{
			RunID: "018f4f20-3d12-7e50-9000-000000000001", SampleID: "018f4f20-3d12-7e50-9000-000000000002",
			ExpectedModelAlias: "route-a", ExpectedRouteProfile: "radar-route-profile-v1",
			APIKeyID: 41, RouteTraceID: "018f4f20-3d12-7e50-9000-000000000003",
		}))
		c.Next()
	})
	router.Use(EvaluationEvidence(repo))
	router.POST("/v1/responses", func(c *gin.Context) {
		dispatchCalls++
		c.Status(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"route-a","input":"ping"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusUnprocessableEntity, recorder.Code)
	require.Equal(t, 1, repo.createCalls)
	require.Equal(t, 0, dispatchCalls)
	require.Empty(t, repo.patches)
}

func TestEvaluationEvidenceCreatesOpenBeforeDispatchAndPatchesAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &trustedEvaluationEvidenceRepoStub{createState: service.RouteEvidencePatchState{Revision: 0}}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		ctx := service.WithEvaluationContext(c.Request.Context(), service.EvaluationContext{
			RunID: "018f4f20-3d12-7e50-9000-000000000001", SampleID: "018f4f20-3d12-7e50-9000-000000000002",
			ExpectedModelAlias: "route-a", ExpectedRouteProfile: "radar-route-profile-v1",
			APIKeyID: 41, RouteTraceID: "018f4f20-3d12-7e50-9000-000000000003",
		})
		ctx = service.WithRouteTrace(ctx, service.NewRouteTrace(service.EvaluationContext{}, service.RouteTraceConfig{Region: "default"}))
		ctx = context.WithValue(ctx, ctxkey.RequestID, "request-1")
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	router.Use(EvaluationEvidence(repo))
	router.POST("/v1/responses", func(c *gin.Context) {
		repo.order = append(repo.order, "dispatch")
		c.Status(http.StatusOK)
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"route-a","input":"ping"}`))
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, []string{"create", "dispatch", "patch", "finalize"}, repo.order)
	require.Len(t, repo.patches, 1)
	require.Equal(t, int64(0), repo.patches[0].ExpectedRevision)
	require.Equal(t, "succeeded", *repo.patches[0].Transport.TransportStatus)
	require.Equal(t, 1, repo.finalCalls)
	require.Equal(t, int64(1), repo.finalize.ExpectedRevision)
}

func TestFinalizeEvaluationRouteEvidencePreservesConfiguredRegionBeforeFirstAttempt(t *testing.T) {
	evaluation := service.EvaluationContext{
		RunID:                "018f4f20-3d12-7e50-9000-000000000001",
		SampleID:             "018f4f20-3d12-7e50-9000-000000000002",
		ExpectedModelAlias:   "expected-model",
		ExpectedRouteProfile: "route-v42",
		APIKeyID:             41,
		RouteTraceID:         "trace-before-routing",
	}
	trace := service.NewRouteTrace(evaluation, service.RouteTraceConfig{
		HashKey: []byte("test-route-hash-key"),
		Region:  "cn-east",
	})

	evidence := finalizeEvaluationRouteEvidence(
		context.Background(),
		evaluation,
		trace.Snapshot(),
		http.StatusInternalServerError,
		time.Now().Add(-time.Second),
		time.Now(),
	)

	require.Equal(t, "gateway_failed", evidence.TransportStatus)
	require.Equal(t, "cn-east", evidence.Region)
	require.Zero(t, evidence.Attempts)
	require.Empty(t, evidence.FallbackChain)
}

func TestEvaluationEvidencePersistenceFailureDoesNotChangeResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &evaluationEvidenceRepoStub{err: errors.New("database unavailable")}
	before := EvaluationEvidencePersistenceFailureCount()

	router := gin.New()
	router.Use(func(c *gin.Context) {
		evaluation := service.EvaluationContext{
			RunID: "018f4f20-3d12-7e50-9000-000000000001", SampleID: "018f4f20-3d12-7e50-9000-000000000002",
			ExpectedModelAlias: "model", ExpectedRouteProfile: "route-v42", APIKeyID: 41, RouteTraceID: "trace-server-generated",
		}
		ctx := service.WithEvaluationContext(c.Request.Context(), evaluation)
		ctx = service.WithRouteTrace(ctx, service.NewRouteTrace(evaluation, service.RouteTraceConfig{HashKey: []byte("test-route-hash-key")}))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	router.Use(EvaluationEvidence(repo))
	router.GET("/test", func(c *gin.Context) {
		c.JSON(http.StatusAccepted, gin.H{"ok": true})
	})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/test", nil))

	require.Equal(t, http.StatusAccepted, recorder.Code)
	require.JSONEq(t, `{"ok":true}`, recorder.Body.String())
	require.Equal(t, before+1, EvaluationEvidencePersistenceFailureCount())
}

func TestEvaluationEvidenceClientCancellationPersistsWithDetachedBoundedContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &evaluationEvidenceRepoStub{}
	evaluation := service.EvaluationContext{
		RunID: "018f4f20-3d12-7e50-9000-000000000001", SampleID: "018f4f20-3d12-7e50-9000-000000000002",
		ExpectedModelAlias: "model", ExpectedRouteProfile: "route-v42", APIKeyID: 41, RouteTraceID: "trace-cancelled",
	}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		ctx := service.WithEvaluationContext(c.Request.Context(), evaluation)
		ctx = service.WithRouteTrace(ctx, service.NewRouteTrace(evaluation, service.RouteTraceConfig{HashKey: []byte("test-route-hash-key")}))
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	})
	router.Use(EvaluationEvidence(repo))
	router.GET("/test", func(c *gin.Context) {
		cancel, ok := c.Request.Context().Value(cancelEvaluationRequestContextKey{}).(context.CancelFunc)
		require.True(t, ok)
		cancel()
		c.Status(499)
	})

	requestCtx, cancel := context.WithCancel(context.Background())
	requestCtx = context.WithValue(requestCtx, cancelEvaluationRequestContextKey{}, context.CancelFunc(cancel))
	request := httptest.NewRequest(http.MethodGet, "/test", nil).WithContext(requestCtx)
	router.ServeHTTP(httptest.NewRecorder(), request)

	require.Equal(t, 1, repo.calls)
	require.NoError(t, repo.ctxErr, "persistence must be detached from client cancellation")
	require.True(t, repo.hasExpiry, "detached persistence must remain bounded")
	require.Equal(t, evaluation, repo.evaluation, "detachment must retain verified evaluation values")
	require.Equal(t, "client_cancelled", repo.transport.TransportStatus)
}

type cancelEvaluationRequestContextKey struct{}
