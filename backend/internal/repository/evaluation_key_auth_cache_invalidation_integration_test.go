//go:build integration

package repository

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestEnableEvaluationKeyInvalidatesCachedOrdinaryKeyBeforeEvaluationRequest(t *testing.T) {
	ctx := context.Background()
	gin.SetMode(gin.TestMode)

	suffix := time.Now().UnixNano()
	user := mustCreateUser(t, integrationEntClient, &service.User{
		Email:       fmt.Sprintf("evaluation-cache-%d@example.com", suffix),
		Concurrency: 1,
	})
	keyValue := fmt.Sprintf("sk-evaluation-cache-%d", suffix)
	key := mustCreateApiKey(t, integrationEntClient, &service.APIKey{
		UserID: user.ID,
		Key:    keyValue,
		Name:   "evaluation-cache",
		Status: service.StatusActive,
	})
	clearInvalidations := func() {
		_, err := integrationDB.ExecContext(ctx, `
			DELETE FROM auth_cache_invalidation_outbox
			WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')`, keyValue)
		require.NoError(t, err)
	}
	t.Cleanup(clearInvalidations)
	t.Cleanup(func() {
		_, err := integrationDB.ExecContext(ctx, "DELETE FROM evaluation_key_events WHERE api_key_id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM api_keys WHERE id = $1", key.ID)
		require.NoError(t, err)
		_, err = integrationDB.ExecContext(ctx, "DELETE FROM users WHERE id = $1", user.ID)
		require.NoError(t, err)
	})

	cache := NewAPIKeyCache(testRedis(t))
	cfg := &config.Config{
		RunMode: config.RunModeSimple,
		APIKeyAuth: config.APIKeyAuthCacheConfig{
			L2TTLSeconds: 300,
		},
		Radar: config.RadarConfig{
			Enabled:              true,
			SigningSecret:        strings.Repeat("s", 32),
			HashingSecret:        strings.Repeat("h", 32),
			MaxContextTTLSeconds: 300,
		},
	}
	apiKeyService := service.NewAPIKeyService(
		NewAPIKeyRepository(integrationEntClient, integrationDB), nil, nil, nil, nil, cache, cfg,
	)

	token := mustSignEvaluationContext(t, cfg.Radar.SigningSecret, key.ID)
	router := gin.New()
	router.Use(servermiddleware.NewAPIKeyAuthMiddleware(apiKeyService, nil, cfg))
	router.POST("/evaluation", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	// The first request materializes an ordinary-key snapshot in the shared auth cache.
	require.Equal(t, http.StatusForbidden, evaluationRequestStatus(router, keyValue, token))

	governance := NewRadarGovernanceRepository(integrationDB)
	enabled, err := governance.EnableEvaluationKey(ctx, key.ID, user.ID)
	require.NoError(t, err)
	require.True(t, enabled.IsEvaluation)

	var queued int
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM auth_cache_invalidation_outbox
		WHERE cache_key = encode(sha256(convert_to($1, 'UTF8')), 'hex')`, keyValue).Scan(&queued))
	require.Equal(t, 1, queued, "evaluation-key enablement must enqueue cache invalidation")

	worker := service.NewAuthCacheInvalidationWorker(
		NewAuthCacheInvalidationOutboxRepository(integrationDB), cache, apiKeyService,
	)
	worker.Start()
	t.Cleanup(worker.Stop)

	require.Eventually(t, func() bool {
		return evaluationRequestStatus(router, keyValue, token) == http.StatusNoContent
	}, 3*time.Second, 20*time.Millisecond, "the consumed invalidation must evict the ordinary-key snapshot")
}

func mustSignEvaluationContext(t *testing.T, secret string, apiKeyID int64) string {
	t.Helper()
	signer, err := service.NewEvaluationContextSigner([]byte(secret), 5*time.Minute)
	require.NoError(t, err)
	now := time.Now().UTC()
	token, err := signer.Sign(service.EvaluationContext{
		RunID:                 "018f4f20-3d12-7e50-9000-000000000701",
		SampleID:              "018f4f20-3d12-7e50-9000-000000000702",
		DatasetVersionID:      "018f4f20-3d12-7e50-9000-000000000703",
		DatasetKey:            "evaluation-cache-invalidation",
		DatasetVersion:        "dataset-v1",
		DatasetManifestSHA256: strings.Repeat("d", 64),
		ExpectedModelAlias:    "public-coder",
		ExpectedRouteProfile:  "route-v1",
		APIKeyID:              apiKeyID,
		IssuedAt:              now.Add(-time.Second),
		ExpiresAt:             now.Add(2 * time.Minute),
	})
	require.NoError(t, err)
	return token
}

func evaluationRequestStatus(router http.Handler, apiKey, token string) int {
	req := httptest.NewRequest(http.MethodPost, "/evaluation", nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("X-Sub2API-Evaluation-Token", token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	return response.Code
}
