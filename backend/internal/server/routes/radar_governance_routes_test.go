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
		RadarGovernance:  adminhandler.NewRadarGovernanceHandler(nil),
		RadarReliability: adminhandler.NewRadarReliabilityHandler(nil),
	}}
	adminAuth := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) { c.Next() })
	auditLog := servermiddleware.AuditLogMiddleware(func(c *gin.Context) { c.Next() })
	stepUp := servermiddleware.StepUpAuthMiddleware(func(c *gin.Context) { c.Next() })

	require.NotPanics(t, func() {
		RegisterAdminRoutes(router.Group("/api/v1"), handlers, adminAuth, auditLog, stepUp, nil, nil)
	})

	routes := map[string]bool{}
	for _, route := range router.Routes() {
		routes[route.Method+" "+route.Path] = true
	}
	for _, route := range []string{
		http.MethodGet + " /api/v1/admin/radar/overview",
		http.MethodPost + " /api/v1/admin/radar/models",
		http.MethodDelete + " /api/v1/admin/radar/models/:alias",
		http.MethodPost + " /api/v1/admin/radar/datasets",
		http.MethodPost + " /api/v1/admin/radar/datasets/:id/publish",
		http.MethodPost + " /api/v1/admin/radar/plans",
		http.MethodPost + " /api/v1/admin/radar/runs",
		http.MethodPost + " /api/v1/admin/radar/runs/:id/pause",
		http.MethodPost + " /api/v1/admin/radar/runs/:id/resume",
		http.MethodPost + " /api/v1/admin/radar/runs/:id/cancel",
		http.MethodPost + " /api/v1/admin/radar/runs/:id/fence",
		http.MethodGet + " /api/v1/admin/radar/runs/:id/reliability-facts",
		http.MethodPost + " /api/v1/admin/radar/revision-batches",
		http.MethodPost + " /api/v1/admin/radar/revision-batches/:id/fence",
		http.MethodPost + " /api/v1/admin/radar/revision-batches/:id/resume",
		http.MethodPost + " /api/v1/admin/radar/revision-batches/:id/cancel",
		http.MethodPost + " /api/v1/admin/radar/revision-batches/:id/repair",
		http.MethodPost + " /api/v1/admin/radar/revision-batches/:id/compensating-head/approve",
		http.MethodPost + " /api/v1/admin/radar/gates/evaluate",
		http.MethodPost + " /api/v1/admin/radar/policies",
		http.MethodPost + " /api/v1/admin/radar/policies/:id/approve",
		http.MethodPost + " /api/v1/admin/radar/policies/:id/activate",
		http.MethodPost + " /api/v1/admin/radar/release-subjects",
		http.MethodGet + " /api/v1/admin/radar/release-subjects/:id",
		http.MethodPost + " /api/v1/admin/radar/release-subjects/:id/activate",
		http.MethodPost + " /api/v1/admin/radar/release-subjects/:id/revoke",
		http.MethodPost + " /api/v1/admin/radar/alerts/observe",
		http.MethodPost + " /api/v1/admin/radar/reliability/load-plans",
		http.MethodPost + " /api/v1/admin/radar/reliability/load-plans/:id/publish",
		http.MethodGet + " /api/v1/admin/radar/reliability/load-plans/:id",
		http.MethodPost + " /api/v1/admin/radar/alerts/:id/acknowledge",
		http.MethodPost + " /api/v1/admin/radar/workers",
		http.MethodPost + " /api/v1/admin/radar/workers/:id/rotate-token",
		http.MethodPost + " /api/v1/admin/radar/workers/:id/pause-claims",
		http.MethodPost + " /api/v1/admin/radar/workers/:id/resume-claims",
		http.MethodPost + " /api/v1/admin/radar/workers/:id/drain",
		http.MethodPost + " /api/v1/admin/radar/workers/:id/disable",
	} {
		require.Truef(t, routes[route], "missing route %s", route)
	}
}
