package repository

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const routeEvidenceTerminalizationBatchLimit = 100

func (r *evaluationRouteEvidenceRepository) ListPendingTerminalizations(ctx context.Context, limit int) ([]service.RouteEvidenceTerminalizationEvent, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("route evidence terminalization outbox is unavailable")
	}
	if limit < 1 || limit > routeEvidenceTerminalizationBatchLimit {
		limit = routeEvidenceTerminalizationBatchLimit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, run_id, control_epoch
		FROM evaluation_route_evidence_terminalization_outbox
		WHERE processed_at IS NULL
		ORDER BY created_at ASC, id ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	events := make([]service.RouteEvidenceTerminalizationEvent, 0, limit)
	for rows.Next() {
		var event service.RouteEvidenceTerminalizationEvent
		if err := rows.Scan(&event.ID, &event.RunID, &event.ControlEpoch); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}
