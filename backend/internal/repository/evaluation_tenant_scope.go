package repository

import (
	"context"
	"database/sql"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

func radarTenant(ctx context.Context) (int64, bool) {
	return service.RadarTenantID(ctx)
}

type radarTenantQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func ensureRadarRunTenant(ctx context.Context, q radarTenantQuery, runID uuid.UUID) error {
	tenantID, scoped := radarTenant(ctx)
	if !scoped {
		return nil
	}
	var ownerID sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_runs WHERE id=$1`, runID).Scan(&ownerID); err != nil {
		return err
	}
	if !ownerID.Valid || ownerID.Int64 != tenantID {
		return service.ErrRadarForbidden
	}
	return nil
}

func ensureRadarWorkerTenant(ctx context.Context, q radarTenantQuery, workerID uuid.UUID) error {
	tenantID, scoped := radarTenant(ctx)
	if !scoped {
		return nil
	}
	var ownerID int64
	if err := q.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_workers WHERE id=$1`, workerID).Scan(&ownerID); err != nil {
		return err
	}
	if ownerID != tenantID {
		return service.ErrRadarForbidden
	}
	return nil
}

func ensureRadarWorkerRunTenant(ctx context.Context, q radarTenantQuery, workerID, runID uuid.UUID) error {
	if workerID == uuid.Nil || runID == uuid.Nil {
		return service.ErrRadarForbidden
	}
	var sameTenant bool
	if err := q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM evaluation_workers w
			JOIN evaluation_runs r ON r.id = $2 AND r.tenant_id = w.tenant_id
			WHERE w.id = $1 AND w.tenant_id > 0
		)`, workerID, runID).Scan(&sameTenant); err != nil {
		return err
	}
	if !sameTenant {
		return service.ErrRadarForbidden
	}
	return nil
}

func ensureScopedRadarWorkerTenant(ctx context.Context, q radarTenantQuery, workerID uuid.UUID) error {
	if _, scoped := radarTenant(ctx); !scoped {
		return nil
	}
	return ensureRadarWorkerTenant(ctx, q, workerID)
}

// ensureRadarExecutionScope binds worker-plane execution records to both the
// authenticated worker and the optional request tenant. Worker requests always
// carry the worker identity after token authentication; the tenant check keeps
// admin-scoped calls consistent when these repository methods are reused.
func ensureRadarExecutionScope(ctx context.Context, q radarTenantQuery, runID uuid.UUID) error {
	if runID == uuid.Nil {
		return service.ErrRadarForbidden
	}
	if workerID, bound := service.RadarWorkerID(ctx); bound {
		if err := ensureRadarWorkerRunTenant(ctx, q, workerID, runID); err != nil {
			return err
		}
	}
	if _, scoped := radarTenant(ctx); scoped {
		if err := ensureRadarRunTenant(ctx, q, runID); err != nil {
			return err
		}
	}
	return nil
}
