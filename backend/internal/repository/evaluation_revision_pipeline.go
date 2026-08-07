package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

func advanceRevisionRequirementsForOutbox(
	ctx context.Context,
	tx *sql.Tx,
	event *service.EvaluationOutboxEvent,
	causes []service.EvaluationOutboxCause,
) error {
	if event == nil || event.RevisionBatchID == uuid.Nil || event.WorkOrigin != "regrade" {
		return nil
	}
	requirementType := revisionRequirementTypeForEvent(event.EventType)
	if requirementType == "" {
		return nil
	}
	for _, cause := range causes {
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_revision_batch_requirements
			SET status='completed', completed_at=transaction_timestamp(), updated_at=transaction_timestamp()
			WHERE revision_batch_id=$1 AND run_id=$2 AND target_key=$3
			  AND status='pending'`, event.RevisionBatchID, event.RunID, cause.EventID.String()); err != nil {
			return fmt.Errorf("complete revision cause requirement: %w", err)
		}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_revision_batch_requirements (
			id, revision_batch_id, run_id, requirement_type, target_key,
			source_hash, cause_set_hash, recovery_generation
		) VALUES ($1,$2,$3,$4,$5,$6,$7,0)
		ON CONFLICT (revision_batch_id, requirement_type, target_key, recovery_generation) DO NOTHING`,
		uuid.New(), event.RevisionBatchID, event.RunID, requirementType, event.ID.String(),
		event.SourceHash, event.CauseSetHash)
	if err != nil {
		return fmt.Errorf("append revision propagation requirement: %w", err)
	}
	return nil
}

func revisionRequirementTypeForEvent(eventType string) string {
	switch eventType {
	case "cell_recompute":
		return "cell"
	case "global_recompute":
		return "global"
	case "gate_reevaluation":
		return "gate"
	default:
		return ""
	}
}

func completeRevisionRequirementForEvent(ctx context.Context, tx *sql.Tx, eventID uuid.UUID) error {
	var batchID uuid.NullUUID
	var eventType string
	if err := tx.QueryRowContext(ctx, `
		SELECT revision_batch_id, event_type FROM evaluation_outbox_events WHERE id=$1`, eventID).Scan(&batchID, &eventType); err != nil {
		return fmt.Errorf("load completed revision outbox event: %w", err)
	}
	if !batchID.Valid || eventType != "gate_reevaluation" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_revision_batch_requirements
		SET status='completed', completed_at=transaction_timestamp(), updated_at=transaction_timestamp()
		WHERE revision_batch_id=$1 AND target_key=$2 AND status='pending'`, batchID.UUID, eventID.String()); err != nil {
		return fmt.Errorf("complete revision Gate requirement: %w", err)
	}
	return reconcileRevisionBatch(ctx, tx, batchID.UUID)
}

func failRevisionRequirementForEvent(ctx context.Context, tx *sql.Tx, eventID uuid.UUID, failureCode string) error {
	var batchID uuid.NullUUID
	if err := tx.QueryRowContext(ctx, `SELECT revision_batch_id FROM evaluation_outbox_events WHERE id=$1`, eventID).Scan(&batchID); err != nil {
		return fmt.Errorf("load failed revision outbox event: %w", err)
	}
	if !batchID.Valid {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_revision_batch_requirements
		SET status='failed', failure_code=$3, updated_at=transaction_timestamp()
		WHERE revision_batch_id=$1 AND target_key=$2 AND status='pending'`,
		batchID.UUID, eventID.String(), failureCode); err != nil {
		return fmt.Errorf("fail revision propagation requirement: %w", err)
	}
	return reconcileRevisionBatch(ctx, tx, batchID.UUID)
}

func reconcileRevisionBatch(ctx context.Context, tx *sql.Tx, batchID uuid.UUID) error {
	var batch service.RevisionBatch
	if err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, status, control_epoch FROM evaluation_revision_batches
		WHERE id=$1 FOR UPDATE`, batchID).Scan(&batch.ID, &batch.RunID, &batch.Status, &batch.ControlEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrRevisionBatchInvalid
		}
		return fmt.Errorf("lock revision pipeline batch: %w", err)
	}
	if batch.Status == service.RevisionBatchCompleted || batch.Status == service.RevisionBatchFailed || batch.Status == service.RevisionBatchCancelled {
		return nil
	}
	var pending, failed int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FILTER (WHERE status='pending'), COUNT(*) FILTER (WHERE status='failed')
		FROM evaluation_revision_batch_requirements WHERE revision_batch_id=$1`, batch.ID).Scan(&pending, &failed); err != nil {
		return fmt.Errorf("count revision pipeline requirements: %w", err)
	}
	if failed > 0 {
		var headAdvanced bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM evaluation_score_head_events WHERE revision_batch_id=$1)
			    OR EXISTS (SELECT 1 FROM evaluation_aggregate_snapshots WHERE revision_batch_id=$1)`, batch.ID).Scan(&headAdvanced); err != nil {
			return fmt.Errorf("check revision propagation progress: %w", err)
		}
		status := service.RevisionBatchFailed
		if headAdvanced {
			status = service.RevisionBatchBlocked
			if err := observeRevisionInsufficientEvidence(ctx, tx, batch.ID); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE evaluation_revision_batches
			SET status=$2::varchar, finished_at=CASE WHEN $2::varchar='failed' THEN transaction_timestamp() ELSE NULL END,
			    updated_at=transaction_timestamp()
			WHERE id=$1`, batch.ID, status)
		return err
	}
	if pending > 0 {
		return nil
	}
	covered, err := revisionBatchCoverageComplete(ctx, tx, batch.ID, batch.RunID)
	if err != nil {
		return err
	}
	if !covered {
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE evaluation_revision_batches
		SET status='completed', finished_at=transaction_timestamp(), updated_at=transaction_timestamp()
		WHERE id=$1`, batch.ID)
	return err
}

func revisionBatchCoverageComplete(ctx context.Context, tx *sql.Tx, batchID, runID uuid.UUID) (bool, error) {
	var uncovered bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM evaluation_revision_batch_requirements requirement
			JOIN evaluation_outbox_events event
			  ON event.id::text=requirement.target_key AND event.revision_batch_id=requirement.revision_batch_id
			WHERE requirement.revision_batch_id=$1 AND requirement.status='completed' AND (
				(requirement.requirement_type='cell' AND NOT EXISTS (
					SELECT 1 FROM evaluation_score_head_events head_event
					JOIN evaluation_score_heads head
					  ON head.sample_id=head_event.sample_id AND head.grader_id=head_event.grader_id AND head.version=head_event.version
					WHERE head_event.id::text=event.source_id AND head_event.run_id=$2
				)) OR
				(requirement.requirement_type='global' AND NOT EXISTS (
					SELECT 1 FROM evaluation_aggregate_heads head
					WHERE head.run_id=$2 AND head.snapshot_id::text=event.source_id
				)) OR
				(requirement.requirement_type='gate' AND NOT EXISTS (
					SELECT 1
					FROM evaluation_gate_decisions decision
					JOIN evaluation_gate_decision_heads head
					  ON head.run_id=decision.run_id
					 AND head.policy_id=decision.policy_id
					 AND head.release_subject_hash=decision.release_subject_hash
					 AND head.decision_id=decision.id
					WHERE decision.run_id=$2 AND decision.cause_set_hash=requirement.cause_set_hash
				))
			)
		)`, batchID, runID).Scan(&uncovered)
	if err != nil {
		return false, fmt.Errorf("verify revision cause coverage: %w", err)
	}
	if uncovered {
		return false, nil
	}
	var missingFrozenCause bool
	err = tx.QueryRowContext(ctx, `
		WITH RECURSIVE gate_events(event_id) AS (
			SELECT event.id
			FROM evaluation_revision_batch_requirements requirement
			JOIN evaluation_outbox_events event
			  ON event.id::text=requirement.target_key
			 AND event.revision_batch_id=requirement.revision_batch_id
			WHERE requirement.revision_batch_id=$1
			  AND requirement.requirement_type='gate'
			  AND requirement.status='completed'
		), cause_closure(event_id) AS (
			SELECT event_id FROM gate_events
			UNION
			SELECT cause.cause_event_id
			FROM cause_closure closure
			JOIN evaluation_outbox_event_causes cause ON cause.event_id=closure.event_id
		), covered_regrade_heads(head_event_id) AS (
			SELECT head_event.id
			FROM cause_closure closure
			JOIN evaluation_outbox_events event ON event.id=closure.event_id
			JOIN evaluation_score_head_events head_event ON head_event.id::text=event.source_id
			WHERE event.source_type='score_head_event'
		)
		SELECT EXISTS (
			SELECT 1
			FROM evaluation_revision_batch_requirements requirement
			WHERE requirement.revision_batch_id=$1
			  AND requirement.requirement_type='grading'
			  AND requirement.status='completed'
			  AND NOT EXISTS (
				SELECT 1
				FROM evaluation_score_head_events head_event
				JOIN evaluation_grading_jobs job ON job.id=head_event.grading_job_id
				JOIN covered_regrade_heads covered ON covered.head_event_id=head_event.id
				WHERE head_event.revision_batch_id=requirement.revision_batch_id
				  AND head_event.run_id=requirement.run_id
				  AND head_event.source_assignment_id=requirement.source_assignment_id
				  AND head_event.grader_id=requirement.grader_id
				  AND job.recovery_generation=requirement.recovery_generation
			)
		)`, batchID).Scan(&missingFrozenCause)
	if err != nil {
		return false, fmt.Errorf("verify frozen revision cause closure: %w", err)
	}
	return !missingFrozenCause, nil
}

func observeRevisionInsufficientEvidence(ctx context.Context, tx *sql.Tx, batchID uuid.UUID) error {
	var tenantID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT r.tenant_id
		FROM evaluation_revision_batches b
		JOIN evaluation_runs r ON r.id=b.run_id
		WHERE b.id=$1`, batchID).Scan(&tenantID); err != nil {
		return fmt.Errorf("load blocked revision tenant: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT
		       COALESCE(NULLIF(event.payload->>'capability_domain',''),'global'),
		       COALESCE(NULLIF(event.payload->>'model_route',''),'global')
		FROM evaluation_revision_batch_requirements requirement
		JOIN evaluation_outbox_events event ON event.id::text=requirement.target_key
		WHERE requirement.revision_batch_id=$1 AND requirement.status='failed'`, batchID)
	if err != nil {
		return fmt.Errorf("load blocked revision scopes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type scope struct{ domain, route string }
	var scopes []scope
	for rows.Next() {
		var item scope
		if err := rows.Scan(&item.domain, &item.route); err != nil {
			return fmt.Errorf("scan blocked revision scope: %w", err)
		}
		if item.domain == "" {
			item.domain = "global"
		}
		if item.route == "" {
			item.route = "global"
		}
		scopes = append(scopes, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate blocked revision scopes: %w", err)
	}
	var policyVersion int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),1) FROM evaluation_gate_policies`).Scan(&policyVersion); err != nil {
		return fmt.Errorf("load blocked revision alert policy: %w", err)
	}
	for _, item := range scopes {
		lockKey := strings.Join([]string{"revision-alert", fmt.Sprint(tenantID), item.route, item.domain, fmt.Sprint(policyVersion)}, ":")
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
			return fmt.Errorf("lock blocked revision alert identity: %w", err)
		}
		var alertID uuid.UUID
		err := tx.QueryRowContext(ctx, `
			UPDATE evaluation_alerts
			SET status='open', severity='P0', acknowledged_at=NULL, resolved_at=NULL,
			    first_seen_at=CASE WHEN status='resolved' THEN transaction_timestamp() ELSE first_seen_at END
			WHERE tenant_id=$1 AND model_route=$2 AND capability_domain=$3
			  AND cause='insufficient_evidence' AND policy_version=$4
			RETURNING id`, tenantID, item.route, item.domain, policyVersion).Scan(&alertID)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `
				INSERT INTO evaluation_alerts (
					id, tenant_id, model_route, capability_domain, cause, policy_version, status, severity
				) VALUES ($1,$2,$3,$4,'insufficient_evidence',$5,'open','P0')
				RETURNING id`, uuid.New(), tenantID, item.route, item.domain, policyVersion).Scan(&alertID)
		}
		if err != nil {
			return fmt.Errorf("observe blocked revision alert: %w", err)
		}
		payload, err := json.Marshal(map[string]any{"revision_batch_id": batchID, "reason_code": "revision_propagation_failed"})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_alert_events (id, alert_id, kind, payload)
			VALUES ($1,$2,'observed',$3::jsonb)`, uuid.New(), alertID, string(payload)); err != nil {
			return fmt.Errorf("append blocked revision alert event: %w", err)
		}
	}
	return nil
}

func repairFailedRevisionPropagationRequirements(ctx context.Context, tx *sql.Tx, batch *service.RevisionBatch) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, requirement_type, target_key, source_hash, cause_set_hash, recovery_generation
		FROM evaluation_revision_batch_requirements
		WHERE revision_batch_id=$1 AND status='failed' AND requirement_type<>'grading'
		ORDER BY id FOR UPDATE`, batch.ID)
	if err != nil {
		return 0, fmt.Errorf("load failed revision propagation requirements: %w", err)
	}
	type failedRequirement struct {
		id                         uuid.UUID
		requirementType, targetKey string
		sourceHash, causeSetHash   string
		recoveryGeneration         int
	}
	var failed []failedRequirement
	for rows.Next() {
		var item failedRequirement
		if err := rows.Scan(&item.id, &item.requirementType, &item.targetKey, &item.sourceHash, &item.causeSetHash, &item.recoveryGeneration); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan failed revision propagation requirement: %w", err)
		}
		failed = append(failed, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("iterate failed revision propagation requirements: %w", err)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, item := range failed {
		var eventStatus service.EvaluationOutboxStatus
		if err := tx.QueryRowContext(ctx, `
			SELECT status FROM evaluation_outbox_events
			WHERE id::text=$1 AND revision_batch_id=$2
			FOR UPDATE`, item.targetKey, batch.ID).Scan(&eventStatus); err != nil ||
			eventStatus != service.EvaluationOutboxDeadLetter {
			return 0, service.ErrRevisionBatchNotRepairable
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_revision_batch_requirements (
				id, revision_batch_id, run_id, requirement_type, target_key,
				source_hash, cause_set_hash, recovery_generation, replaces_requirement_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, uuid.New(), batch.ID, batch.RunID,
			item.requirementType, item.targetKey, item.sourceHash, item.causeSetHash,
			item.recoveryGeneration+1, item.id); err != nil {
			return 0, fmt.Errorf("insert revision propagation replacement: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_revision_batch_requirements
			SET status='superseded', updated_at=transaction_timestamp() WHERE id=$1`, item.id); err != nil {
			return 0, fmt.Errorf("supersede revision propagation requirement: %w", err)
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE evaluation_outbox_events
			SET status='pending', available_at=transaction_timestamp(), last_error_code=NULL,
			    lease_token_hash=NULL, lease_owner=NULL, lease_expires_at=NULL,
			    updated_at=transaction_timestamp()
			WHERE id::text=$1 AND revision_batch_id=$2 AND status='dead_letter'`, item.targetKey, batch.ID)
		if err != nil {
			return 0, fmt.Errorf("requeue revision propagation event: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return 0, service.ErrRevisionBatchNotRepairable
		}
	}
	return len(failed), nil
}

func propagateAssignmentReplacement(
	ctx context.Context,
	tx *sql.Tx,
	runID, sampleID, oldAssignmentID, replacementID uuid.UUID,
	oldAttempt int,
) error {
	if runID == uuid.Nil || sampleID == uuid.Nil || oldAssignmentID == uuid.Nil || replacementID == uuid.Nil || oldAttempt < 1 {
		return service.ErrRevisionBatchInvalid
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status='failed', failure_code='assignment_replaced', lease_token_hash=NULL,
		    leased_by=NULL, lease_expires_at=NULL, finished_at=transaction_timestamp(), updated_at=transaction_timestamp()
		WHERE run_id=$1 AND assignment_id=$2 AND work_origin<>'regrade' AND status IN ('pending','leased')`, runID, oldAssignmentID); err != nil {
		return fmt.Errorf("cancel replaced assignment grading: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		WITH RECURSIVE invalid_scores(score_id, score_created_at) AS (
			SELECT head.score_id, head.score_created_at
			FROM evaluation_score_heads head
			JOIN evaluation_score_head_events head_event
			  ON head_event.sample_id=head.sample_id
			 AND head_event.grader_id=head.grader_id
			 AND head_event.version=head.version
			JOIN evaluation_assignments source_assignment ON source_assignment.id=head_event.source_assignment_id
			JOIN evaluation_assignments replacement ON replacement.id=$3
			WHERE head.sample_id=$2 AND source_assignment.attempt < replacement.attempt
		), snapshot_closure(analysis_job_id, snapshot_id, window_start) AS (
			SELECT input.analysis_job_id, input.snapshot_id, input.window_start
			FROM evaluation_analysis_job_snapshot_inputs input
			UNION
			SELECT closure.analysis_job_id, source.source_snapshot_id, source.source_window_start
			FROM snapshot_closure closure
			JOIN evaluation_aggregate_snapshot_sources source
			  ON source.snapshot_id=closure.snapshot_id
			 AND source.snapshot_window_start=closure.window_start
		), affected_jobs(analysis_job_id) AS (
			SELECT input.analysis_job_id
			FROM evaluation_analysis_job_score_inputs input
			JOIN invalid_scores invalid
			  ON invalid.score_id=input.score_id AND invalid.score_created_at=input.score_created_at
			UNION
			SELECT closure.analysis_job_id
			FROM snapshot_closure closure
			JOIN evaluation_aggregate_snapshot_score_inputs input
			  ON input.snapshot_id=closure.snapshot_id
			 AND input.snapshot_window_start=closure.window_start
			JOIN invalid_scores invalid
			  ON invalid.score_id=input.score_id AND invalid.score_created_at=input.score_created_at
		)
		UPDATE evaluation_analysis_jobs job
		SET status='failed', failure_code='assignment_replaced', lease_token_hash=NULL,
		    leased_by=NULL, lease_expires_at=NULL, finished_at=transaction_timestamp(), updated_at=transaction_timestamp()
		WHERE job.run_id=$1 AND job.work_origin<>'regrade' AND job.status IN ('pending','leased')
		  AND job.id IN (SELECT analysis_job_id FROM affected_jobs)`, runID, sampleID, replacementID); err != nil {
		return fmt.Errorf("stale replaced assignment analysis: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT head_event.id, outbox.id, sample.model_route, case_spec.capability_domain
		FROM evaluation_score_heads head
		JOIN evaluation_score_head_events head_event
		  ON head_event.sample_id=head.sample_id AND head_event.grader_id=head.grader_id AND head_event.version=head.version
		JOIN evaluation_outbox_events outbox
		  ON outbox.run_id=head_event.run_id AND outbox.source_type='score_head_event' AND outbox.source_id=head_event.id::text
		JOIN evaluation_samples sample ON sample.id=head_event.sample_id
		JOIN evaluation_cases case_spec ON case_spec.id=sample.case_id
		JOIN evaluation_assignments source_assignment ON source_assignment.id=head_event.source_assignment_id
		JOIN evaluation_assignments replacement ON replacement.id=$3
		WHERE head_event.run_id=$1 AND head_event.sample_id=$2
		  AND source_assignment.attempt < replacement.attempt
		ORDER BY head_event.id`, runID, sampleID, replacementID)
	if err != nil {
		return fmt.Errorf("load replaced assignment heads: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type replacedHead struct {
		headEventID, causeEventID uuid.UUID
		modelRoute, domain        string
	}
	var heads []replacedHead
	for rows.Next() {
		var item replacedHead
		if err := rows.Scan(&item.headEventID, &item.causeEventID, &item.modelRoute, &item.domain); err != nil {
			return fmt.Errorf("scan replaced assignment head: %w", err)
		}
		heads = append(heads, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate replaced assignment heads: %w", err)
	}
	for _, head := range heads {
		payload, err := json.Marshal(map[string]any{
			"sample_id": sampleID, "old_assignment_id": oldAssignmentID,
			"replacement_assignment_id": replacementID, "old_attempt": oldAttempt,
			"source_head_event_id": head.headEventID,
		})
		if err != nil {
			return err
		}
		_, err = enqueueEvaluationOutbox(ctx, tx, service.EnqueueEvaluationOutboxInput{
			EventType: "cell_recompute", RunID: runID,
			ScopeKey:        head.domain + "/" + service.CanonicalModelRoute(head.modelRoute),
			AnalysisVersion: "assignment-replacement-v1", SourceType: "assignment_replacement",
			SourceID: replacementID.String(),
			SourceHash: hashString(strings.Join([]string{
				"assignment-replacement", runID.String(), sampleID.String(), oldAssignmentID.String(), replacementID.String(), head.headEventID.String(), fmt.Sprint(oldAttempt),
			}, "\x00")),
			Payload: payload,
			Causes:  []service.EvaluationOutboxCause{{EventID: head.causeEventID, SourceHeadEventID: head.headEventID}},
		})
		if err != nil {
			return fmt.Errorf("enqueue assignment replacement recompute: %w", err)
		}
	}
	return nil
}
