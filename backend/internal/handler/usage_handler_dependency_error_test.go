package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type usageHandlerDeadlineRepoStub struct {
	service.UsageLogRepository
	err error
}

func (s *usageHandlerDeadlineRepoStub) ListWithFilters(context.Context, pagination.PaginationParams, usagestats.UsageLogFilters) ([]service.UsageLog, *pagination.PaginationResult, error) {
	return nil, nil, s.err
}

func newUsageHandlerDeadlineTestRouter(repo service.UsageLogRepository) *gin.Engine {
	gin.SetMode(gin.TestMode)
	usageService := service.NewUsageService(repo, nil, nil, nil)
	handler := NewUsageHandler(usageService, nil, nil, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(middleware2.ContextKeyUser), middleware2.AuthSubject{UserID: 42})
		c.Next()
	})
	router.GET("/api/v1/usage", handler.List)
	return router
}

func TestUsageHandlerListMapsDependencyDeadlineToRetryableUnavailable(t *testing.T) {
	router := newUsageHandlerDeadlineTestRouter(&usageHandlerDeadlineRepoStub{err: fmt.Errorf("redis query: %w", context.DeadlineExceeded)})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/usage", nil)
	resp := httptest.NewRecorder()

	router.ServeHTTP(resp, req)

	require.Equal(t, http.StatusServiceUnavailable, resp.Code)
	require.Equal(t, "1", resp.Header().Get("Retry-After"))
	var envelope struct {
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(resp.Body.Bytes(), &envelope))
	require.NotContains(t, envelope.Message, "redis")
	require.NotContains(t, envelope.Message, "context deadline")
}
