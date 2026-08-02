package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

type evaluationArtifactCleanupRepository struct {
	db *sql.DB
}

func NewEvaluationArtifactCleanupRepository(db *sql.DB) service.EvaluationArtifactCleanupRepository {
	return &evaluationArtifactCleanupRepository{db: db}
}

func (r *evaluationArtifactCleanupRepository) ListExpiredArtifacts(ctx context.Context, before time.Time, limit int) ([]service.ArtifactCleanupCandidate, error) {
	if r == nil || r.db == nil {
		return nil, service.ErrArtifactCleanupUnavailable
	}
	if before.IsZero() {
		return nil, service.ErrArtifactInvalid
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	query := `
		SELECT id, object_key, scan_status, retention_deadline
		FROM evaluation_artifacts
		WHERE deleted_at IS NULL AND retention_deadline <= $1
		ORDER BY retention_deadline, id
		LIMIT $2`
	args := []any{before, limit}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query = strings.Replace(query, "\t\tORDER BY", "\t\tAND tenant_id = $3\n\t\tORDER BY", 1)
		args = append(args, tenantID)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list expired evaluation artifacts: %w", err)
	}
	defer rows.Close()
	candidates := make([]service.ArtifactCleanupCandidate, 0)
	for rows.Next() {
		var candidate service.ArtifactCleanupCandidate
		var scanStatus string
		if err := rows.Scan(&candidate.ID, &candidate.ObjectKey, &scanStatus, &candidate.RetentionDeadline); err != nil {
			return nil, fmt.Errorf("scan expired evaluation artifact: %w", err)
		}
		candidate.ScanStatus = service.ArtifactScanStatus(scanStatus)
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate expired evaluation artifacts: %w", err)
	}
	return candidates, nil
}

func (r *evaluationArtifactCleanupRepository) MarkArtifactDeleted(ctx context.Context, candidate service.ArtifactCleanupCandidate, deletedAt time.Time) (bool, error) {
	if r == nil || r.db == nil {
		return false, service.ErrArtifactCleanupUnavailable
	}
	if candidate.ID == uuid.Nil || strings.TrimSpace(candidate.ObjectKey) == "" || candidate.RetentionDeadline.IsZero() || deletedAt.IsZero() {
		return false, service.ErrArtifactInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return false, fmt.Errorf("begin evaluation artifact cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	query := `
		UPDATE evaluation_artifacts
		SET deleted_at = $4
		WHERE id = $1 AND object_key = $2 AND retention_deadline = $3
		  AND deleted_at IS NULL AND retention_deadline <= $4`
	args := []any{candidate.ID, candidate.ObjectKey, candidate.RetentionDeadline, deletedAt}
	if tenantID, scoped := radarTenant(ctx); scoped {
		query += " AND tenant_id = $5"
		args = append(args, tenantID)
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("mark evaluation artifact deleted: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read evaluation artifact cleanup result: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit evaluation artifact cleanup: %w", err)
	}
	return affected == 1, nil
}
