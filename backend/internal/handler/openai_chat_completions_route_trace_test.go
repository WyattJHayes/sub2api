//go:build unit

package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpenAIChatCompletionsRecordsEveryUpstreamDispatch(t *testing.T) {
	t.Run("successful primary dispatch", func(t *testing.T) {
		h, _, upstream, router, cleanup := newGrokCredentialFailoverHandler(t, "healthy")
		defer cleanup()
		h.cfg.Radar.Region = "staging"

		trace := service.NewRouteTrace(service.EvaluationContext{
			ExpectedModelAlias:   "grok",
			ExpectedRouteProfile: "route-v1",
		}, service.RouteTraceConfig{HashKey: []byte("route-evidence-test-key"), Region: "staging"})
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewBufferString(
			`{"model":"grok","messages":[{"role":"user","content":"hello"}],"stream":false}`,
		)).WithContext(service.WithRouteTrace(t.Context(), trace))
		req.Header.Set("Content-Type", "application/json")

		router.ServeHTTP(recorder, req)

		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Equal(t, []int64{801}, upstream.accountHits())
		snapshot := trace.Snapshot()
		require.Equal(t, 1, snapshot.Attempts)
		require.Len(t, snapshot.FallbackChain, 1)
		primary := snapshot.FallbackChain[0]
		require.Equal(t, 1, primary.Ordinal)
		require.Nil(t, primary.ParentAttemptIndex)
		require.Equal(t, "primary", primary.DispatchMode)
		require.Equal(t, service.PlatformGrok, primary.Provider)
		require.Equal(t, "grok-4.5", primary.ResolvedModel)
		require.Equal(t, "staging", primary.Region)
		require.NotEmpty(t, primary.AccountPoolRef)
	})

	t.Run("failed primary followed by successful fallback", func(t *testing.T) {
		h, _, upstream, router, cleanup := newGrokCredentialFailoverHandler(t, "first_402")
		defer cleanup()
		h.cfg.Radar.Region = "staging"

		trace := service.NewRouteTrace(service.EvaluationContext{
			ExpectedModelAlias:   "grok",
			ExpectedRouteProfile: "route-v1",
		}, service.RouteTraceConfig{HashKey: []byte("route-evidence-test-key"), Region: "staging"})
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", bytes.NewBufferString(
			`{"model":"grok","messages":[{"role":"user","content":"hello"}],"stream":false}`,
		)).WithContext(service.WithRouteTrace(t.Context(), trace))
		req.Header.Set("Content-Type", "application/json")

		router.ServeHTTP(recorder, req)

		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		require.Equal(t, []int64{801, 802}, upstream.accountHits())
		snapshot := trace.Snapshot()
		require.Equal(t, 2, snapshot.Attempts)
		require.Len(t, snapshot.FallbackChain, 2)
		primary := snapshot.FallbackChain[0]
		require.Equal(t, "upstream_failed", primary.Outcome)
		require.Equal(t, "402", primary.ErrorCode)
		require.NotNil(t, primary.FinishedAt)
		fallback := snapshot.FallbackChain[1]
		require.Equal(t, 2, fallback.Ordinal)
		require.NotNil(t, fallback.ParentAttemptIndex)
		require.Equal(t, 1, *fallback.ParentAttemptIndex)
		require.Equal(t, "fallback", fallback.DispatchMode)
	})
}
