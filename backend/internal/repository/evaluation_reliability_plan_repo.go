package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

type radarReliabilityRepository struct {
	db *sql.DB
}

func NewRadarReliabilityRepository(db *sql.DB) service.RadarReliabilityRepository {
	return &radarReliabilityRepository{db: db}
}

func (r *radarReliabilityRepository) valid() error {
	if r == nil || r.db == nil {
		return errors.New("nil radar reliability repository")
	}
	return nil
}

func (r *radarReliabilityRepository) ListPermissions(ctx context.Context, actorID int64) ([]service.RadarPermission, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	return (&radarGovernanceRepository{db: r.db}).ListPermissions(ctx, actorID)
}

func (r *radarReliabilityRepository) Require(ctx context.Context, actorID int64, permission service.RadarPermission) error {
	if err := r.valid(); err != nil {
		return err
	}
	return (&radarGovernanceRepository{db: r.db}).Require(ctx, actorID, permission)
}

func (r *radarReliabilityRepository) CreateLoadPlan(ctx context.Context, input service.RadarLoadPlanInput, actorID int64) (*service.RadarLoadPlanRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if actorID <= 0 {
		return nil, errors.New("load plan creator is required")
	}
	tenantID, err := service.RequireRadarTenant(ctx)
	if err != nil {
		return nil, err
	}
	if input.TenantID != tenantID || tenantID != actorID {
		return nil, service.ErrRadarForbidden
	}
	canonical, err := service.CanonicalLoadPlan(input)
	if err != nil {
		return nil, err
	}

	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin load plan creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record := &service.RadarLoadPlanRecord{}
	if err := scanRadarLoadPlan(tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_load_plans (
			id, schema_version, tenant_id, canonical_plan_bytes, load_plan_sha256,
			status, created_by
		) VALUES ($1, $2, $3, $4, $5, 'draft', $6)
		RETURNING id, schema_version, tenant_id, canonical_plan_bytes, load_plan_sha256,
			status, created_by, published_at, created_at, updated_at`,
		uuid.New(), canonical.SchemaVersion, canonical.TenantID, canonical.CanonicalBytes,
		canonical.SHA256, actorID), record); err != nil {
		return nil, fmt.Errorf("insert load plan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit load plan creation: %w", err)
	}
	return record, nil
}

func (r *radarReliabilityRepository) PublishLoadPlan(ctx context.Context, id uuid.UUID, actorID int64) (*service.RadarLoadPlanRecord, error) {
	tenantID, err := service.RequireRadarTenant(ctx)
	if err != nil {
		return nil, err
	}
	return r.PublishLoadPlanForTenant(ctx, id, tenantID, actorID)
}

func (r *radarReliabilityRepository) PublishLoadPlanForTenant(ctx context.Context, id uuid.UUID, tenantID, actorID int64) (*service.RadarLoadPlanRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if id == uuid.Nil || tenantID <= 0 || actorID <= 0 {
		return nil, errors.New("load plan and publisher are required")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin load plan publication: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record := &service.RadarLoadPlanRecord{}
	if err := scanRadarLoadPlan(tx.QueryRowContext(ctx, `
		SELECT id, schema_version, tenant_id, canonical_plan_bytes, load_plan_sha256,
			status, created_by, published_at, created_at, updated_at
		FROM evaluation_load_plans
		WHERE id = $1 AND tenant_id = $2
		FOR UPDATE`, id, tenantID), record); err != nil {
		return nil, err
	}
	if record.Status == "published" {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit idempotent load plan publication: %w", err)
		}
		return record, nil
	}
	if record.Status != "draft" {
		return nil, fmt.Errorf("load plan status %q cannot be published", record.Status)
	}
	if err := scanRadarLoadPlan(tx.QueryRowContext(ctx, `
		UPDATE evaluation_load_plans
		SET status='published', published_at=transaction_timestamp(), updated_at=transaction_timestamp()
		WHERE id=$1 AND tenant_id=$2
		RETURNING id, schema_version, tenant_id, canonical_plan_bytes, load_plan_sha256,
			status, created_by, published_at, created_at, updated_at`, id, tenantID), record); err != nil {
		return nil, fmt.Errorf("publish load plan: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit load plan publication: %w", err)
	}
	return record, nil
}

func (r *radarReliabilityRepository) GetLoadPlan(ctx context.Context, id uuid.UUID) (*service.RadarLoadPlanRecord, error) {
	tenantID, err := service.RequireRadarTenant(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetLoadPlanForTenant(ctx, id, tenantID)
}

func (r *radarReliabilityRepository) GetLoadPlanForTenant(ctx context.Context, id uuid.UUID, tenantID int64) (*service.RadarLoadPlanRecord, error) {
	if err := r.valid(); err != nil {
		return nil, err
	}
	if id == uuid.Nil || tenantID <= 0 {
		return nil, errors.New("load plan is required")
	}
	record := &service.RadarLoadPlanRecord{}
	if err := scanRadarLoadPlan(r.db.QueryRowContext(ctx, `
		SELECT id, schema_version, tenant_id, canonical_plan_bytes, load_plan_sha256,
			status, created_by, published_at, created_at, updated_at
		FROM evaluation_load_plans
		WHERE id = $1 AND ($2 = 0 OR tenant_id = $2)`, id, tenantID), record); err != nil {
		return nil, fmt.Errorf("get load plan: %w", err)
	}
	return record, nil
}

type radarLoadPlanScanner interface {
	Scan(dest ...any) error
}

func scanRadarLoadPlan(scanner radarLoadPlanScanner, record *service.RadarLoadPlanRecord) error {
	var canonical []byte
	var publishedAt sql.NullTime
	if err := scanner.Scan(
		&record.ID, &record.SchemaVersion, &record.TenantID, &canonical, &record.LoadPlanSHA256,
		&record.Status, &record.CreatedBy, &publishedAt, &record.CreatedAt, &record.UpdatedAt,
	); err != nil {
		return err
	}
	record.CanonicalPlan = append(record.CanonicalPlan[:0], canonical...)
	if publishedAt.Valid {
		published := publishedAt.Time
		record.PublishedAt = &published
	} else {
		record.PublishedAt = nil
	}
	return nil
}

var _ service.RadarReliabilityRepository = (*radarReliabilityRepository)(nil)
var _ service.RadarTenantScopedReliabilityRepository = (*radarReliabilityRepository)(nil)
