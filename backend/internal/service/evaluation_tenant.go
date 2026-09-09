package service

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

var ErrRadarTenantRequired = errors.New("radar tenant is required")

type radarTenantContextKey struct{}

type radarActorContextKey struct{}

type radarWorkerContextKey struct{}

// WithRadarTenant binds an authenticated tenant to a Radar repository call.
func WithRadarTenant(ctx context.Context, tenantID int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, radarTenantContextKey{}, tenantID)
}

// RadarTenantID returns the tenant bound to a Radar request.
func RadarTenantID(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	tenantID, ok := ctx.Value(radarTenantContextKey{}).(int64)
	return tenantID, ok && tenantID > 0
}

// RequireRadarTenant makes an omitted tenant scope an explicit error.
func RequireRadarTenant(ctx context.Context) (int64, error) {
	tenantID, ok := RadarTenantID(ctx)
	if !ok {
		return 0, ErrRadarTenantRequired
	}
	return tenantID, nil
}

// WithRadarActor binds the authenticated actor to a Radar repository call.
// Tenant and actor are separate identities when an administrator manages
// another user's role binding.
func WithRadarActor(ctx context.Context, actorID int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if actorID <= 0 {
		return ctx
	}
	return context.WithValue(ctx, radarActorContextKey{}, actorID)
}

// RadarActorID returns the authenticated actor bound to a Radar request.
func RadarActorID(ctx context.Context) (int64, bool) {
	if ctx == nil {
		return 0, false
	}
	actorID, ok := ctx.Value(radarActorContextKey{}).(int64)
	return actorID, ok && actorID > 0
}

// WithRadarWorkerID binds the worker authenticated for a private Radar request.
// Lease mutations use this identity to prevent a valid lease token from being
// replayed by a different worker.
func WithRadarWorkerID(ctx context.Context, workerID uuid.UUID) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if workerID == uuid.Nil {
		return ctx
	}
	return context.WithValue(ctx, radarWorkerContextKey{}, workerID)
}

// RadarWorkerID returns the worker bound to a private Radar request.
func RadarWorkerID(ctx context.Context) (uuid.UUID, bool) {
	if ctx == nil {
		return uuid.Nil, false
	}
	workerID, ok := ctx.Value(radarWorkerContextKey{}).(uuid.UUID)
	return workerID, ok && workerID != uuid.Nil
}
