package routes

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	adminhandler "github.com/Wei-Shaw/sub2api/internal/handler/admin"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRadarGovernanceActionRoutesDoNotConflictWithAlertIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{
		RadarGovernance: adminhandler.NewRadarGovernanceHandler(nil),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })

	require.NotPanics(t, func() {
		RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil)
	})

	routes := map[string]bool{}
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		http.MethodPost + " /api/v1/admin/radar/datasets",
		http.MethodPost + " /api/v1/admin/radar/datasets/:id/publish",
		http.MethodPost + " /api/v1/admin/radar/plans",
		http.MethodPost + " /api/v1/admin/radar/runs",
		http.MethodPost + " /api/v1/admin/radar/gates/evaluate",
		http.MethodPost + " /api/v1/admin/radar/alerts/observe",
		http.MethodPost + " /api/v1/admin/radar/alerts/:id/acknowledge",
	} {
		require.Truef(t, routes[route], "missing route %s", route)
	}
}
