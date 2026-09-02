package middleware

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// WithRadarTenant keeps the middleware package compatible with Radar handlers
// while the authoritative tenant context lives in the service package.
func WithRadarTenant(ctx context.Context, tenantID int64) context.Context {
	return service.WithRadarTenant(ctx, tenantID)
}

// RadarTenantID returns the tenant bound to the request context.
func RadarTenantID(ctx context.Context) (int64, bool) {
	return service.RadarTenantID(ctx)
}

// WithRadarActor binds the authenticated actor to a Radar request context.
func WithRadarActor(ctx context.Context, actorID int64) context.Context {
	return service.WithRadarActor(ctx, actorID)
}

// RadarActorID returns the actor bound to the request context.
func RadarActorID(ctx context.Context) (int64, bool) {
	return service.RadarActorID(ctx)
}
