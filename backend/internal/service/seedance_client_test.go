//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSeedanceClientDoesNotWriteGinResponse(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task-1","status":"queued"}`)}
	svc := &OpenAIGatewayService{httpUpstream: upstream}
	response, err := svc.CreateSeedanceTask(context.Background(), seedanceTestAccount(), []byte(`{"model":"video","content":[{"type":"text","text":"waves"}]}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.JSONEq(t, `{"id":"task-1","status":"queued"}`, string(response.Body))
	require.Equal(t, "seedance:task-1", response.Result.ResponseID)
}

func TestSeedanceClientClassifiesStatusErrors(t *testing.T) {
	cases := []struct {
		status int
		kind   SeedanceUpstreamErrorKind
	}{
		{http.StatusUnauthorized, SeedanceUpstreamErrorAuth},
		{http.StatusForbidden, SeedanceUpstreamErrorAuth},
		{http.StatusNotFound, SeedanceUpstreamErrorNotFound},
		{http.StatusTooManyRequests, SeedanceUpstreamErrorRateLimited},
		{http.StatusInternalServerError, SeedanceUpstreamErrorTemporary},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"error":{"code":"UpstreamFailure"}}`)}
			upstream.response.StatusCode = tc.status
			if tc.status == http.StatusTooManyRequests {
				upstream.response.Header.Set("Retry-After", "2")
			}
			svc := &OpenAIGatewayService{httpUpstream: upstream}
			response, err := svc.GetSeedanceTask(context.Background(), seedanceTestAccount(), "task-1")
			require.Error(t, err)
			require.NotNil(t, response)
			var upstreamErr *SeedanceUpstreamError
			require.True(t, errors.As(err, &upstreamErr))
			require.Equal(t, tc.kind, upstreamErr.Kind)
			require.Equal(t, tc.status, upstreamErr.StatusCode)
			require.Equal(t, "UpstreamFailure", upstreamErr.SafeCode)
		})
	}
}

func TestSeedanceClientRejectsOversizedResponseWithoutRetainingBody(t *testing.T) {
	upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(strings.Repeat("x", 5))}
	cfg := &config.Config{}
	cfg.Gateway.UpstreamResponseReadMaxBytes = 4
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	response, err := svc.GetSeedanceTask(context.Background(), seedanceTestAccount(), "task-1")
	require.Error(t, err)
	require.NotNil(t, response)
	require.Empty(t, response.Body)
	var upstreamErr *SeedanceUpstreamError
	require.True(t, errors.As(err, &upstreamErr))
	require.Equal(t, SeedanceUpstreamErrorProtocol, upstreamErr.Kind)
	require.Equal(t, "response_too_large", upstreamErr.SafeCode)
}
