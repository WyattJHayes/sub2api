package middleware

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AuthSubject is the minimal authenticated identity stored in gin context.
// TenantID currently follows the authenticated user until workspace mapping is
// introduced. Radar repositories must still consume it from request context so
// that the ownership boundary remains explicit and centrally enforceable.
type AuthSubject struct {
	UserID      int64
	TenantID    int64
	Concurrency int
}

// WithRadarTenant binds the authenticated Radar tenant to a request context.
func WithRadarTenant(ctx context.Context, tenantID int64) context.Context {
	return service.WithRadarTenant(ctx, tenantID)
}

// RadarTenantID returns the authenticated Radar tenant when one was bound.
func RadarTenantID(ctx context.Context) (int64, bool) {
	return service.RadarTenantID(ctx)
}

// WithRadarActor binds the authenticated Radar actor to a request context.
func WithRadarActor(ctx context.Context, actorID int64) context.Context {
	return service.WithRadarActor(ctx, actorID)
}

// RadarActorID returns the authenticated Radar actor when one was bound.
func RadarActorID(ctx context.Context) (int64, bool) {
	return service.RadarActorID(ctx)
}

// RequireRadarTenant rejects repository calls that omit tenant scope.
func RequireRadarTenant(ctx context.Context) (int64, error) {
	return service.RequireRadarTenant(ctx)
}

func GetAuthSubjectFromContext(c *gin.Context) (AuthSubject, bool) {
	value, exists := c.Get(string(ContextKeyUser))
	if !exists {
		return AuthSubject{}, false
	}
	subject, ok := value.(AuthSubject)
	return subject, ok
}

func GetUserRoleFromContext(c *gin.Context) (string, bool) {
	value, exists := c.Get(string(ContextKeyUserRole))
	if !exists {
		return "", false
	}
	role, ok := value.(string)
	return role, ok
}
