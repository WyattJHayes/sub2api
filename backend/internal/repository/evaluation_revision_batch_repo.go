package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

type frozenGradingRequirement struct {
	sampleID, assignmentID, headEventID uuid.UUID
	scoreRef                            service.ScoreRef
	graderID, graderVersion             string
	routeEvidenceSetHash                string
	artifactManifestHash                string
	assignmentAttempt                   int
}

func (r *radarGovernanceRepository) CreateRevisionBatch(ctx context.Context, input service.CreateRevisionBatchInput) (*service.RevisionBatch, error) {
	if r == nil || r.db == nil || input.RunID == uuid.Nil || input.RequestedBy <= 0 || strings.TrimSpace(input.Reason) == "" {
		return nil, service.ErrRevisionBatchInvalid
	}
	if err := service.ValidateRevisionBatchIdempotencyKey(input.IdempotencyKey); err != nil {
		return nil, err
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin revision batch creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, scoped := radarTenant(ctx); scoped {
		if err := ensureRadarRunTenant(ctx, tx, input.RunID); err != nil {
			return nil, err
		}
	}
	if existing, err := loadRevisionBatchByIdempotencyKey(ctx, tx, input.IdempotencyKey); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.RunID != input.RunID || existing.Reason != strings.TrimSpace(input.Reason) || existing.RequestedBy != input.RequestedBy {
			return nil, service.ErrRevisionBatchConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit revision batch retry: %w", err)
		}
		return existing, nil
	}
	var runStatus service.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM evaluation_runs WHERE id = $1 FOR UPDATE`, input.RunID).Scan(&runStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrRevisionBatchRunNotCompleted
		}
		return nil, fmt.Errorf("lock revision batch run: %w", err)
	}
	if runStatus != service.RunStatusCompleted {
		return nil, service.ErrRevisionBatchRunNotCompleted
	}

	batchID := uuid.New()
	batch := &service.RevisionBatch{}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_revision_batches (
			id, run_id, status, control_epoch, reason, requested_by, idempotency_key, started_at
		) VALUES ($1, $2, 'running', 1, $3, $4, $5, NOW())
		RETURNING id, run_id, status, control_epoch, reason, requested_by, idempotency_key,
			started_at, finished_at, created_at, updated_at`,
		batchID, input.RunID, strings.TrimSpace(input.Reason), input.RequestedBy, input.IdempotencyKey,
	).Scan(&batch.ID, &batch.RunID, &batch.Status, &batch.ControlEpoch, &batch.Reason,
		&batch.RequestedBy, &batch.IdempotencyKey, &batch.StartedAt, &batch.FinishedAt,
		&batch.CreatedAt, &batch.UpdatedAt)
	if err != nil {
		if isRevisionUniqueViolation(err) {
			return nil, service.ErrRevisionBatchConflict
		}
		return nil, fmt.Errorf("insert revision batch: %w", err)
	}
	requirements, err := loadFrozenGradingRequirements(ctx, tx, input.RunID)
	if err != nil {
		return nil, err
	}
	if len(requirements) == 0 {
		return nil, service.ErrRevisionBatchInvalid
	}
	for _, requirement := range requirements {
		if err := insertFrozenGradingRequirement(ctx, tx, batch, requirement, 0, nil); err != nil {
			return nil, err
		}
	}
	if err := insertRevisionBatchEvent(ctx, tx, batch, "created", input.RequestedBy, input.IdempotencyKey, map[string]any{"reason": batch.Reason}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit revision batch creation: %w", err)
	}
	return batch, nil
}

func loadFrozenGradingRequirements(ctx context.Context, tx *sql.Tx, runID uuid.UUID) ([]frozenGradingRequirement, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT sample.id, assignment.id, assignment.attempt, case_spec.grader_id, case_spec.grader_version,
		       head.score_id, head.score_created_at, head_event.id,
		       score.route_evidence_set_hash, score.artifact_manifest_hash
		FROM evaluation_samples sample
		JOIN evaluation_cases case_spec ON case_spec.id = sample.case_id
		JOIN LATERAL (
			SELECT id, attempt FROM evaluation_assignments current_assignment
			WHERE current_assignment.sample_id = sample.id
			ORDER BY attempt DESC LIMIT 1
		) assignment ON TRUE
		JOIN evaluation_score_heads head
		  ON head.sample_id = sample.id AND head.grader_id = case_spec.grader_id
		JOIN evaluation_scores score
		  ON score.id = head.score_id AND score.created_at = head.score_created_at
		JOIN evaluation_score_head_events head_event
		  ON head_event.sample_id = sample.id AND head_event.grader_id = case_spec.grader_id
		 AND head_event.version = head.version
		WHERE sample.run_id = $1
		ORDER BY sample.id, case_spec.grader_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("load revision grading requirements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	requirements := make([]frozenGradingRequirement, 0)
	for rows.Next() {
		var requirement frozenGradingRequirement
		if err := rows.Scan(&requirement.sampleID, &requirement.assignmentID, &requirement.assignmentAttempt,
			&requirement.graderID, &requirement.graderVersion, &requirement.scoreRef.ID,
			&requirement.scoreRef.CreatedAt, &requirement.headEventID,
			&requirement.routeEvidenceSetHash, &requirement.artifactManifestHash); err != nil {
			return nil, fmt.Errorf("scan revision grading requirement: %w", err)
		}
		requirements = append(requirements, requirement)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate revision grading requirements: %w", err)
	}
	return requirements, nil
}

func insertFrozenGradingRequirement(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch, frozen frozenGradingRequirement, generation int, replaces *uuid.UUID) error {
	canonical, err := json.Marshal(struct {
		RunID                uuid.UUID        `json:"run_id"`
		AssignmentID         uuid.UUID        `json:"assignment_id"`
		PreviousScore        service.ScoreRef `json:"previous_score"`
		GraderID             string           `json:"grader_id"`
		GraderVersion        string           `json:"grader_version"`
		RouteEvidenceSetHash string           `json:"route_evidence_set_hash"`
		ArtifactManifestHash string           `json:"artifact_manifest_hash"`
		Reason               string           `json:"reason"`
	}{batch.RunID, frozen.assignmentID, frozen.scoreRef, frozen.graderID, frozen.graderVersion,
		frozen.routeEvidenceSetHash, frozen.artifactManifestHash, batch.Reason})
	if err != nil {
		return fmt.Errorf("marshal revision grading input: %w", err)
	}
	gradingInputHash, err := service.DigestCanonicalJSON(canonical)
	if err != nil {
		return fmt.Errorf("hash revision grading input: %w", err)
	}
	requirementID := uuid.New()
	targetKey := frozen.sampleID.String() + ":" + frozen.graderID
	causeSetHash := hashString("score-head-event\x00" + frozen.headEventID.String())
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_revision_batch_requirements (
			id, revision_batch_id, run_id, requirement_type, target_key,
			source_assignment_id, previous_score_id, previous_score_created_at,
			grader_id, grader_version, grading_input_hash, source_hash, cause_set_hash,
			recovery_generation, replaces_requirement_id
		) VALUES ($1, $2, $3, 'grading', $4, $5, $6, $7, $8, $9, $10, $10, $11, $12, $13)`,
		requirementID, batch.ID, batch.RunID, targetKey, frozen.assignmentID,
		frozen.scoreRef.ID, frozen.scoreRef.CreatedAt, frozen.graderID, frozen.graderVersion,
		gradingInputHash, causeSetHash, generation, replaces); err != nil {
		return fmt.Errorf("insert revision grading requirement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_grading_jobs (
			id, run_id, sample_id, assignment_id, grader_id, grader_version, attempt,
			status, work_origin, revision_batch_id, grading_input_hash,
			evidence_manifest_hash, recovery_generation
		) VALUES ($1, $2, $3, $4, $5, $6, 1, 'pending', 'regrade', $7, $8, $9, $10)`,
		uuid.New(), batch.RunID, frozen.sampleID, frozen.assignmentID, frozen.graderID,
		frozen.graderVersion, batch.ID, gradingInputHash, frozen.artifactManifestHash, generation); err != nil {
		return fmt.Errorf("insert revision grading job: %w", err)
	}
	return nil
}

func (r *radarGovernanceRepository) FenceRevisionBatch(ctx context.Context, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	return r.controlRevisionBatch(ctx, input, "fenced", func(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch) error {
		if batch.Status != service.RevisionBatchRunning && batch.Status != service.RevisionBatchPending && batch.Status != service.RevisionBatchBlocked {
			return service.ErrRevisionBatchFenced
		}
		if batch.Status == service.RevisionBatchBlocked {
			repaired, err := repairFailedRevisionRequirements(ctx, tx, batch)
			if err != nil {
				return err
			}
			if repaired == 0 {
				return service.ErrRevisionBatchNotRepairable
			}
			batch.Status = service.RevisionBatchRunning
		}
		batch.ControlEpoch++
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_revision_batches SET status=$3, control_epoch=$2, updated_at=NOW() WHERE id=$1`, batch.ID, batch.ControlEpoch, batch.Status); err != nil {
			return fmt.Errorf("fence revision batch: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_grading_jobs SET status='pending', lease_token_hash=NULL, leased_by=NULL,
				lease_expires_at=NULL, lease_epoch=$2, updated_at=NOW()
			WHERE revision_batch_id=$1 AND status='leased'`, batch.ID, batch.ControlEpoch); err != nil {
			return fmt.Errorf("requeue fenced grading work: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_analysis_jobs SET status='pending', lease_token_hash=NULL, leased_by=NULL,
				lease_expires_at=NULL, lease_epoch=$2, updated_at=NOW()
			WHERE revision_batch_id=$1 AND status='leased'`, batch.ID, batch.ControlEpoch); err != nil {
			return fmt.Errorf("requeue fenced analysis work: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_outbox_events SET status='pending', lease_token_hash=NULL, lease_owner=NULL,
				lease_expires_at=NULL, lease_epoch=$2, available_at=NOW(), updated_at=NOW()
			WHERE revision_batch_id=$1 AND status='leased'`, batch.ID, batch.ControlEpoch); err != nil {
			return fmt.Errorf("requeue fenced outbox work: %w", err)
		}
		return nil
	})
}

func (r *radarGovernanceRepository) ResumeRevisionBatch(ctx context.Context, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	return r.controlRevisionBatch(ctx, input, "resumed", func(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch) error {
		if batch.Status != service.RevisionBatchBlocked {
			return service.ErrRevisionBatchNotRepairable
		}
		var activeLeases, failedRequirements int
		if err := tx.QueryRowContext(ctx, `
			SELECT
			 (SELECT COUNT(*) FROM evaluation_grading_jobs WHERE revision_batch_id=$1 AND status='leased') +
			 (SELECT COUNT(*) FROM evaluation_analysis_jobs WHERE revision_batch_id=$1 AND status='leased') +
			 (SELECT COUNT(*) FROM evaluation_outbox_events WHERE revision_batch_id=$1 AND status='leased'),
			 (SELECT COUNT(*) FROM evaluation_revision_batch_requirements WHERE revision_batch_id=$1 AND status='failed')`, batch.ID).Scan(&activeLeases, &failedRequirements); err != nil {
			return fmt.Errorf("check revision batch resume boundary: %w", err)
		}
		if activeLeases != 0 || failedRequirements != 0 {
			return service.ErrRevisionBatchNotRepairable
		}
		batch.Status = service.RevisionBatchRunning
		_, err := tx.ExecContext(ctx, `UPDATE evaluation_revision_batches SET status='running', updated_at=NOW() WHERE id=$1`, batch.ID)
		return err
	})
}

func (r *radarGovernanceRepository) CancelRevisionBatch(ctx context.Context, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	return r.controlRevisionBatch(ctx, input, "cancelled", func(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch) error {
		var advanced int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluation_score_head_events WHERE revision_batch_id=$1`, batch.ID).Scan(&advanced); err != nil {
			return fmt.Errorf("check revision batch propagation: %w", err)
		}
		if advanced != 0 {
			return service.ErrRevisionBatchPropagationRequired
		}
		if batch.Status == service.RevisionBatchCompleted || batch.Status == service.RevisionBatchFailed || batch.Status == service.RevisionBatchCancelled {
			return service.ErrRevisionBatchFenced
		}
		batch.Status = service.RevisionBatchCancelled
		batch.ControlEpoch++
		now := time.Now().UTC()
		batch.FinishedAt = &now
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_grading_jobs
			SET status='failed', failure_code='revision_batch_cancelled', lease_token_hash=NULL,
				leased_by=NULL, lease_expires_at=NULL, lease_epoch=$2, finished_at=NOW(), updated_at=NOW()
			WHERE revision_batch_id=$1 AND status IN ('pending','leased')`, batch.ID, batch.ControlEpoch); err != nil {
			return fmt.Errorf("cancel revision grading work: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_analysis_jobs
			SET status='failed', failure_code='revision_batch_cancelled', lease_token_hash=NULL,
				leased_by=NULL, lease_expires_at=NULL, lease_epoch=$2, finished_at=NOW(), updated_at=NOW()
			WHERE revision_batch_id=$1 AND status IN ('pending','leased')`, batch.ID, batch.ControlEpoch); err != nil {
			return fmt.Errorf("cancel revision analysis work: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_outbox_events
			SET status='dead_letter', last_error_code='revision_batch_cancelled', lease_token_hash=NULL,
				lease_owner=NULL, lease_expires_at=NULL, lease_epoch=$2, updated_at=NOW()
			WHERE revision_batch_id=$1 AND status IN ('pending','leased')`, batch.ID, batch.ControlEpoch); err != nil {
			return fmt.Errorf("cancel revision outbox work: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_revision_batch_requirements
			SET status='failed', failure_code='revision_batch_cancelled', updated_at=NOW()
			WHERE revision_batch_id=$1 AND status='pending'`, batch.ID); err != nil {
			return fmt.Errorf("cancel revision requirements: %w", err)
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE evaluation_revision_batches
			SET status='cancelled', control_epoch=$2, finished_at=NOW(), updated_at=NOW()
			WHERE id=$1`, batch.ID, batch.ControlEpoch)
		return err
	})
}

func (r *radarGovernanceRepository) RepairRevisionBatch(ctx context.Context, input service.RevisionBatchControlInput) (*service.RevisionBatch, error) {
	return r.controlRevisionBatch(ctx, input, "repaired", func(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch) error {
		if batch.Status != service.RevisionBatchBlocked {
			return service.ErrRevisionBatchNotRepairable
		}
		repaired, err := repairFailedRevisionRequirements(ctx, tx, batch)
		if err != nil {
			return err
		}
		if repaired == 0 {
			return service.ErrRevisionBatchNotRepairable
		}
		batch.Status = service.RevisionBatchRunning
		batch.ControlEpoch++
		_, err = tx.ExecContext(ctx, `UPDATE evaluation_revision_batches SET status='running', control_epoch=$2, updated_at=NOW() WHERE id=$1`, batch.ID, batch.ControlEpoch)
		return err
	})
}

func repairFailedRevisionRequirements(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch) (int, error) {
	grading, err := repairFailedRevisionGradingRequirements(ctx, tx, batch)
	if err != nil {
		return 0, err
	}
	propagation, err := repairFailedRevisionPropagationRequirements(ctx, tx, batch)
	if err != nil {
		return 0, err
	}
	return grading + propagation, nil
}

func repairFailedRevisionGradingRequirements(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch) (int, error) {
	rows, err := tx.QueryContext(ctx, `
			SELECT requirement.id, requirement.run_id, requirement.source_assignment_id,
			       requirement.previous_score_id, requirement.previous_score_created_at,
			       requirement.grader_id, requirement.grader_version, requirement.recovery_generation,
			       job.sample_id, score.route_evidence_set_hash, score.artifact_manifest_hash,
			       head_event.id, assignment.attempt
			FROM evaluation_revision_batch_requirements requirement
			JOIN evaluation_grading_jobs job
			  ON job.revision_batch_id=requirement.revision_batch_id
			 AND job.assignment_id=requirement.source_assignment_id
			 AND job.grader_id=requirement.grader_id
			 AND job.recovery_generation=requirement.recovery_generation
			JOIN evaluation_scores score
			  ON score.id=requirement.previous_score_id AND score.created_at=requirement.previous_score_created_at
			JOIN evaluation_score_head_events head_event
			  ON head_event.score_id=score.id AND head_event.score_created_at=score.created_at
			 AND head_event.version=score.version
			JOIN evaluation_assignments assignment ON assignment.id=requirement.source_assignment_id
			WHERE requirement.revision_batch_id=$1 AND requirement.requirement_type='grading'
			  AND requirement.status='failed'
			ORDER BY requirement.id FOR UPDATE OF requirement`, batch.ID)
	if err != nil {
		return 0, fmt.Errorf("load failed revision requirements: %w", err)
	}
	type repair struct {
		oldID      uuid.UUID
		runID      uuid.UUID
		frozen     frozenGradingRequirement
		generation int
	}
	repairs := make([]repair, 0)
	for rows.Next() {
		var item repair
		if err := rows.Scan(&item.oldID, &item.runID, &item.frozen.assignmentID,
			&item.frozen.scoreRef.ID, &item.frozen.scoreRef.CreatedAt,
			&item.frozen.graderID, &item.frozen.graderVersion, &item.generation,
			&item.frozen.sampleID, &item.frozen.routeEvidenceSetHash,
			&item.frozen.artifactManifestHash, &item.frozen.headEventID,
			&item.frozen.assignmentAttempt); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan failed revision requirement: %w", err)
		}
		if item.runID != batch.RunID {
			_ = rows.Close()
			return 0, service.ErrRevisionBatchInvalid
		}
		repairs = append(repairs, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate failed revision requirements: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close failed revision requirements: %w", err)
	}
	for _, item := range repairs {
		if err := insertFrozenGradingRequirement(ctx, tx, batch, item.frozen, item.generation+1, &item.oldID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_revision_batch_requirements SET status='superseded', updated_at=NOW() WHERE id=$1`, item.oldID); err != nil {
			return 0, fmt.Errorf("supersede failed revision requirement: %w", err)
		}
	}
	return len(repairs), nil
}

func (r *radarGovernanceRepository) ApproveCompensatingScoreHead(ctx context.Context, input service.CompensatingScoreHeadInput) (*service.CompensatingScoreHeadResult, error) {
	if r == nil || r.db == nil || input.BatchID == uuid.Nil || input.SampleID == uuid.Nil || input.ScoreRef.ID == uuid.Nil || input.ScoreRef.CreatedAt.IsZero() || input.ActorID <= 0 || strings.TrimSpace(input.GraderID) == "" {
		return nil, service.ErrRevisionBatchInvalid
	}
	if err := service.ValidateRevisionBatchIdempotencyKey(input.IdempotencyKey); err != nil {
		return nil, err
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin compensating head approval: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	batch, err := loadRevisionBatchForUpdate(ctx, tx, input.BatchID)
	if err != nil {
		return nil, err
	}
	var targetRunID, sourceAssignmentID uuid.UUID
	var evidenceSetHash string
	var manualReview bool
	if err := tx.QueryRowContext(ctx, `
		SELECT run_id, source_assignment_id, route_evidence_set_hash, manual_review_required
		FROM evaluation_scores WHERE id=$1 AND created_at=$2 AND sample_id=$3 AND grader_id=$4`,
		input.ScoreRef.ID, input.ScoreRef.CreatedAt, input.SampleID, input.GraderID).Scan(
		&targetRunID, &sourceAssignmentID, &evidenceSetHash, &manualReview); err != nil {
		return nil, service.ErrRevisionBatchInvalid
	}
	if targetRunID != batch.RunID {
		return nil, service.ErrRevisionBatchInvalid
	}
	if batch.Status != service.RevisionBatchRunning && batch.Status != service.RevisionBatchBlocked {
		return nil, service.ErrRevisionBatchFenced
	}
	var frozenTarget bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM evaluation_revision_batch_requirements requirement
			JOIN evaluation_assignments assignment ON assignment.id=requirement.source_assignment_id
			WHERE requirement.revision_batch_id=$1 AND requirement.requirement_type='grading'
			  AND assignment.sample_id=$2 AND requirement.grader_id=$3
			  AND requirement.previous_score_id=$4 AND requirement.previous_score_created_at=$5
		)`, batch.ID, input.SampleID, input.GraderID, input.ScoreRef.ID, input.ScoreRef.CreatedAt).Scan(&frozenTarget); err != nil {
		return nil, fmt.Errorf("validate compensating score target: %w", err)
	}
	if !frozenTarget {
		return nil, service.ErrRevisionBatchInvalid
	}
	targetKey := input.SampleID.String() + ":" + input.GraderID + ":" + input.ScoreRef.ID.String() + ":" + input.ScoreRef.CreatedAt.UTC().Format(time.RFC3339Nano)
	payload := map[string]any{"target_key": targetKey, "sample_id": input.SampleID, "grader_id": input.GraderID, "score_id": input.ScoreRef.ID, "score_created_at": input.ScoreRef.CreatedAt.UTC()}
	replayed, err := loadRevisionBatchEventRetry(ctx, tx, batch.ID, "compensating_head_approved", input.ActorID, input.IdempotencyKey, payload)
	if err != nil {
		return nil, err
	}
	if !replayed {
		if err := insertRevisionBatchEvent(ctx, tx, batch, "compensating_head_approved", input.ActorID, input.IdempotencyKey, payload); err != nil {
			return nil, err
		}
	}
	var approvals int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT actor_id) FROM evaluation_revision_batch_events
		WHERE revision_batch_id=$1 AND event_type='compensating_head_approved'
		  AND payload->>'target_key'=$2`, batch.ID, targetKey).Scan(&approvals); err != nil {
		return nil, fmt.Errorf("count compensating head approvals: %w", err)
	}
	result := &service.CompensatingScoreHeadResult{BatchID: batch.ID, ApprovalCount: approvals, ScoreRef: input.ScoreRef}
	if approvals < 2 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit compensating head approval: %w", err)
		}
		return result, nil
	}
	var current service.ScoreRef
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT score_id, score_created_at, version FROM evaluation_score_heads WHERE sample_id=$1 AND grader_id=$2 FOR UPDATE`, input.SampleID, input.GraderID).Scan(&current.ID, &current.CreatedAt, &version); err != nil {
		return nil, fmt.Errorf("lock compensating score head: %w", err)
	}
	if current == input.ScoreRef {
		if err := ensureCompensatingHeadAppliedEvent(ctx, tx, batch, input.ActorID, input.IdempotencyKey, targetKey, payload); err != nil {
			return nil, err
		}
		result.Applied = true
		result.HeadVersion = version
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return result, nil
	}
	version++
	eventID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_score_head_events (
			id, run_id, sample_id, grader_id, version, previous_score_id, previous_score_created_at,
			score_id, score_created_at, source_assignment_id, route_evidence_set_hash,
			reason, actor_id, revision_batch_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'compensating',$12,$13)`,
		eventID, targetRunID, input.SampleID, input.GraderID, version, current.ID, current.CreatedAt,
		input.ScoreRef.ID, input.ScoreRef.CreatedAt, sourceAssignmentID, evidenceSetHash,
		input.ActorID, batch.ID); err != nil {
		return nil, fmt.Errorf("append compensating score head: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_score_heads SET score_id=$3, score_created_at=$4, version=$5,
			manual_review_required=$6, updated_at=NOW()
		WHERE sample_id=$1 AND grader_id=$2`, input.SampleID, input.GraderID,
		input.ScoreRef.ID, input.ScoreRef.CreatedAt, version, manualReview); err != nil {
		return nil, fmt.Errorf("advance compensating score head: %w", err)
	}
	batchEpoch := batch.ControlEpoch
	if err := enqueueScoreHeadRecompute(ctx, tx, targetRunID, "compensating", "compensating", eventID,
		evidenceSetHash, "regrade", &batch.ID, &batchEpoch); err != nil {
		return nil, err
	}
	if err := ensureCompensatingHeadAppliedEvent(ctx, tx, batch, input.ActorID, input.IdempotencyKey, targetKey, payload); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit compensating score head: %w", err)
	}
	result.Applied = true
	result.HeadVersion = version
	return result, nil
}

type revisionBatchMutation func(context.Context, *sql.Tx, *service.RevisionBatch) error

func (r *radarGovernanceRepository) controlRevisionBatch(ctx context.Context, input service.RevisionBatchControlInput, eventType string, mutate revisionBatchMutation) (*service.RevisionBatch, error) {
	if r == nil || r.db == nil || input.BatchID == uuid.Nil || input.ActorID <= 0 || strings.TrimSpace(input.Reason) == "" {
		return nil, service.ErrRevisionBatchInvalid
	}
	if err := service.ValidateRevisionBatchIdempotencyKey(input.IdempotencyKey); err != nil {
		return nil, err
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin revision batch control: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	batch, err := loadRevisionBatchForUpdate(ctx, tx, input.BatchID)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"reason": strings.TrimSpace(input.Reason)}
	replayed, err := loadRevisionBatchEventRetry(ctx, tx, batch.ID, eventType, input.ActorID, input.IdempotencyKey, payload)
	if err != nil {
		return nil, err
	}
	if replayed {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return batch, nil
	}
	if err := mutate(ctx, tx, batch); err != nil {
		return nil, err
	}
	if err := insertRevisionBatchEvent(ctx, tx, batch, eventType, input.ActorID, input.IdempotencyKey, payload); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit revision batch control: %w", err)
	}
	return batch, nil
}

func loadRevisionBatchEventRetry(ctx context.Context, tx *sql.Tx, batchID uuid.UUID, eventType string, actorID int64, key string, payload any) (bool, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("marshal revision batch retry payload: %w", err)
	}
	var matches bool
	err = tx.QueryRowContext(ctx, `
		SELECT revision_batch_id=$2 AND event_type=$3 AND actor_id=$4 AND payload=$5::jsonb
		FROM evaluation_revision_batch_events
		WHERE idempotency_key=$1`, key, batchID, eventType, actorID, string(encoded)).Scan(&matches)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load revision batch event retry: %w", err)
	}
	if !matches {
		return false, service.ErrRevisionBatchConflict
	}
	return true, nil
}

func ensureCompensatingHeadAppliedEvent(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch, actorID int64, approvalKey, targetKey string, payload any) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM evaluation_revision_batch_events
			WHERE revision_batch_id=$1 AND event_type='compensating_head_applied'
			  AND payload->>'target_key'=$2
		)`, batch.ID, targetKey).Scan(&exists); err != nil {
		return fmt.Errorf("load compensating head application: %w", err)
	}
	if exists {
		return nil
	}
	return insertRevisionBatchEvent(ctx, tx, batch, "compensating_head_applied", actorID, hashString(approvalKey+":applied"), payload)
}

func insertRevisionBatchEvent(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch, eventType string, actorID int64, key string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal revision batch event: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_revision_batch_events (
			id, revision_batch_id, run_id, event_type, actor_id, control_epoch, idempotency_key, payload
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb)
		ON CONFLICT (idempotency_key) DO NOTHING`, uuid.New(), batch.ID, batch.RunID,
		eventType, actorID, batch.ControlEpoch, key, string(encoded))
	if err != nil {
		return fmt.Errorf("insert revision batch event: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect revision batch event insert: %w", err)
	}
	if affected == 0 {
		replayed, err := loadRevisionBatchEventRetry(ctx, tx, batch.ID, eventType, actorID, key, payload)
		if err != nil {
			return err
		}
		if !replayed {
			return service.ErrRevisionBatchConflict
		}
	}
	return nil
}

func loadRevisionBatchForUpdate(ctx context.Context, tx *sql.Tx, id uuid.UUID) (*service.RevisionBatch, error) {
	batch := &service.RevisionBatch{}
	if err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, status, control_epoch, reason, requested_by, idempotency_key,
		       started_at, finished_at, created_at, updated_at
		FROM evaluation_revision_batches WHERE id=$1 FOR UPDATE`, id).Scan(
		&batch.ID, &batch.RunID, &batch.Status, &batch.ControlEpoch, &batch.Reason,
		&batch.RequestedBy, &batch.IdempotencyKey, &batch.StartedAt, &batch.FinishedAt,
		&batch.CreatedAt, &batch.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, service.ErrRevisionBatchInvalid
		}
		return nil, fmt.Errorf("load revision batch: %w", err)
	}
	return batch, nil
}

func loadRevisionBatchByIdempotencyKey(ctx context.Context, tx *sql.Tx, key string) (*service.RevisionBatch, error) {
	var id uuid.UUID
	if err := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_revision_batches WHERE idempotency_key=$1`, key).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("load revision batch retry: %w", err)
	}
	return loadRevisionBatchForUpdate(ctx, tx, id)
}

func isRevisionUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}
