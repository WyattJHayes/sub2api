package repository

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

type evaluationRepository struct {
	db *sql.DB
}

func NewEvaluationRepository(db *sql.DB) service.EvaluationRepository {
	return &evaluationRepository{db: db}
}

func (r *evaluationRepository) SubmitEvidence(ctx context.Context, input service.EvidenceSubmission, leaseToken string) (*service.EvidenceReceipt, error) {
	return (&evaluationGradingRepository{db: r.db}).SubmitEvidence(ctx, input, leaseToken)
}

func (r *evaluationRepository) CreateRunWithMatrix(ctx context.Context, input service.CreateRunInput) (*service.EvaluationRun, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation repository")
	}
	if input.PlanID == uuid.Nil {
		return nil, errors.New("evaluation plan id is required")
	}
	if !validTriggerSource(input.TriggerSource) {
		return nil, fmt.Errorf("invalid evaluation trigger source %q", input.TriggerSource)
	}
	if tenantID, scoped := radarTenant(ctx); scoped && (input.CreatedBy <= 0 || input.CreatedBy != tenantID) {
		return nil, service.ErrRadarForbidden
	}

	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin evaluation run transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if tenantID, scoped := radarTenant(ctx); scoped {
		var ownerID int64
		if err := tx.QueryRowContext(ctx, `SELECT tenant_id FROM evaluation_plans WHERE id=$1`, input.PlanID).Scan(&ownerID); err != nil {
			return nil, err
		}
		if ownerID != tenantID {
			return nil, service.ErrRadarForbidden
		}
	}

	var (
		datasetStatus string
		matrixJSON    []byte
		budgetLimit   decimal.Decimal
		planState     evaluationPlanControlState
	)
	err = tx.QueryRowContext(ctx, `
		SELECT d.status, p.model_matrix, p.max_run_cost, p.enabled, p.daily_cost_limit,
		       EXISTS (
		         SELECT 1 FROM api_keys k
		         JOIN users u ON u.id = k.user_id
		         LEFT JOIN groups g ON g.id = k.group_id
		         WHERE k.id = p.gateway_api_key_id
		           AND k.is_evaluation = TRUE AND k.status = 'active' AND k.deleted_at IS NULL
		           AND (k.expires_at IS NULL OR k.expires_at > NOW())
		           AND (k.quota = 0 OR k.quota_used < k.quota)
		           AND u.status = 'active' AND u.deleted_at IS NULL
		           AND (g.id IS NULL OR (g.status = 'active' AND g.deleted_at IS NULL))
		       ),
		       COALESCE((
		         SELECT SUM(existing.reserved_cost) FROM evaluation_runs existing
		         WHERE existing.plan_id = p.id
		           AND existing.created_at >= date_trunc('day', NOW())
		       ), 0)
		FROM evaluation_plans p
		JOIN evaluation_dataset_versions d ON d.id = p.dataset_version_id
		WHERE p.id = $1
		FOR UPDATE OF p`, input.PlanID).Scan(
		&datasetStatus, &matrixJSON, &budgetLimit, &planState.enabled,
		&planState.dailyCostLimit, &planState.keyUsable, &planState.dailyReservedCost,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("evaluation plan %s not found", input.PlanID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock evaluation plan: %w", err)
	}
	if datasetStatus != string(service.DatasetStatusPublished) {
		return nil, fmt.Errorf("evaluation dataset for plan %s is not published", input.PlanID)
	}

	matrix, err := evaluationMatrixEntries(matrixJSON)
	if err != nil {
		return nil, err
	}
	cases, err := loadEvaluationCases(ctx, tx, input.PlanID)
	if err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, errors.New("evaluation dataset has no cases")
	}

	totalReservation := decimal.Zero
	for _, evaluationCase := range cases {
		multiplicity := int64(len(matrix) * 2 * evaluationCase.sampleCount)
		totalReservation = totalReservation.Add(evaluationCase.estimatedCost.Mul(decimal.NewFromInt(multiplicity)))
	}
	if totalReservation.GreaterThan(budgetLimit) {
		return nil, fmt.Errorf("%w: reservation %s exceeds budget %s", service.ErrBudgetExceeded, totalReservation, budgetLimit)
	}
	if !runCreationEligible(planState, totalReservation) {
		if !planState.enabled {
			return nil, errors.New("evaluation plan is disabled")
		}
		if !planState.keyUsable {
			return nil, errors.New("evaluation plan has no usable dedicated gateway API key")
		}
		return nil, fmt.Errorf("%w: daily reservation %s plus run reservation %s exceeds daily limit %s",
			service.ErrBudgetExceeded, planState.dailyReservedCost, totalReservation, planState.dailyCostLimit)
	}

	baselineRef, err := marshalJSONObject(input.BaselineRef)
	if err != nil {
		return nil, fmt.Errorf("marshal baseline reference: %w", err)
	}
	candidateRef, err := marshalJSONObject(input.CandidateRef)
	if err != nil {
		return nil, fmt.Errorf("marshal candidate reference: %w", err)
	}
	run := &service.EvaluationRun{
		ID:             uuid.New(),
		PlanID:         input.PlanID,
		Status:         service.RunStatusPending,
		BudgetLimit:    budgetLimit,
		ReservedCost:   totalReservation,
		ContractStatus: "pending",
	}
	var runControlEpoch int64
	if totalReservation.Equal(budgetLimit) {
		run.Status = service.RunStatusBudgetPaused
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_runs (
			id, plan_id, trigger_source, baseline_ref, candidate_ref, status,
			budget_limit, reserved_cost, created_by, tenant_id, route_profile_version
		) VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, NULLIF($9, 0), $9, $10)
		RETURNING created_at, control_epoch`,
		run.ID, run.PlanID, input.TriggerSource, baselineRef, candidateRef,
		run.Status, run.BudgetLimit, run.ReservedCost, input.CreatedBy, radarRouteProfileVersion).Scan(&run.CreatedAt, &runControlEpoch)
	if err != nil {
		return nil, fmt.Errorf("insert evaluation run: %w", err)
	}

	contracts, err := persistEvaluationExperimentContracts(ctx, tx, run.ID, run.CreatedAt, runControlEpoch, cases, matrix)
	if err != nil {
		return nil, err
	}
	run.ContractStatus = "bound"
	run.PairBindings = contracts.Bindings

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_run_events (id, run_id, event_type, payload, actor_type, actor_ref, control_epoch, idempotency_key)
		VALUES ($1, $2, 'run_created', '{}'::jsonb, 'user', NULLIF($3, ''), 0, $4)`,
		uuid.New(), run.ID, strconvFormatInt(input.CreatedBy), hashReconcileKey("run-created:"+run.ID.String())); err != nil {
		return nil, fmt.Errorf("insert run created event: %w", err)
	}
	warningThreshold := budgetLimit.Mul(decimal.NewFromInt(8)).Div(decimal.NewFromInt(10))
	if !totalReservation.LessThan(warningThreshold) {
		if err := insertEvaluationBudgetWarning(ctx, tx, run.ID, runControlEpoch, totalReservation, budgetLimit); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evaluation run transaction: %w", err)
	}
	return run, nil
}

func (r *evaluationRepository) ClaimAssignment(ctx context.Context, workerID uuid.UUID, capabilities []string, leaseTTL time.Duration) (*service.AssignmentLease, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation repository")
	}
	if workerID == uuid.Nil {
		return nil, errors.New("evaluation worker id is required")
	}
	if leaseTTL <= 0 {
		return nil, errors.New("evaluation lease ttl must be positive")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin evaluation assignment claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var registeredCapabilities pq.StringArray
	var workerImageDigest sql.NullString
	var workerTenantID int64
	err = tx.QueryRowContext(ctx, `
	SELECT capabilities, image_digest, tenant_id FROM evaluation_workers
		WHERE id = $1 AND status = 'active' AND claim_mode = 'open'
		FOR UPDATE`, workerID).Scan(&registeredCapabilities, &workerImageDigest, &workerTenantID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("evaluation worker is unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("lock evaluation worker: %w", err)
	}
	authorizedCapabilities := intersectCapabilities(capabilities, registeredCapabilities)
	if len(authorizedCapabilities) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit unauthorized evaluation assignment claim: %w", err)
		}
		return nil, nil
	}

	reclaimed, err := reclaimExpiredAssignment(ctx, tx, authorizedCapabilities, workerTenantID)
	if err != nil {
		return nil, err
	}
	var candidate assignmentCandidate
	if reclaimed != nil {
		candidate = *reclaimed
	} else {
		candidate, err = selectPendingAssignment(ctx, tx, authorizedCapabilities, workerTenantID)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("commit empty evaluation assignment claim: %w", err)
			}
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
	}
	eligible, eligibilityErr := lockRunLeaseEligibility(ctx, tx, candidate.runID, candidate.priority)
	if eligibilityErr != nil {
		return nil, eligibilityErr
	}
	if !eligible {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit ineligible evaluation assignment claim: %w", err)
		}
		return nil, nil
	}
	var runEpoch, runStateVersion int64
	var runStatus service.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT control_epoch, state_version, status FROM evaluation_runs WHERE id = $1 FOR UPDATE`, candidate.runID).Scan(&runEpoch, &runStateVersion, &runStatus); err != nil {
		return nil, fmt.Errorf("load evaluation run lease epoch: %w", err)
	}
	if workerIDFromContext, bound := service.RadarWorkerID(ctx); bound {
		if workerIDFromContext != workerID || workerTenantID <= 0 {
			return nil, service.ErrRadarForbidden
		}
	}
	if runStatus == service.RunStatusPaused || runStatus == service.RunStatusCancelled || runStatus == service.RunStatusCompleted || runStatus == service.RunStatusFailed {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit terminal evaluation assignment claim: %w", err)
		}
		return nil, nil
	}
	if candidate.leaseEpoch != runEpoch {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit stale evaluation assignment claim: %w", err)
		}
		return nil, nil
	}
	canonicalConfig, err := canonicalizeModelConfig(candidate.modelConfig)
	if err != nil {
		return nil, fmt.Errorf("canonicalize evaluation sample %s model configuration: %w", candidate.sampleID, err)
	}
	if hashString(string(canonicalConfig)) != candidate.modelConfigSHA256 {
		return nil, fmt.Errorf("evaluation sample %s model configuration digest mismatch", candidate.sampleID)
	}

	token, tokenHash, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	lease := &service.AssignmentLease{
		ID:                candidate.id,
		SampleID:          candidate.sampleID,
		RunID:             candidate.runID,
		ModelRoute:        candidate.modelRoute,
		ModelConfig:       append(json.RawMessage(nil), canonicalConfig...),
		ModelConfigSHA256: candidate.modelConfigSHA256,
		Attempt:           candidate.attempt,
		Token:             token,
		LeaseEpoch:        runEpoch,
		WorkerImageDigest: workerImageDigest.String,
		WorkOrigin:        candidate.workOrigin,
	}
	if err := loadAssignmentExecutionContract(ctx, tx, candidate, lease); err != nil {
		return nil, err
	}
	if runStatus == service.RunStatusPending {
		transitionVersion := runStateVersion + 1
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_run_events
				(id, run_id, event_type, payload, actor_type, transition_version, from_status, to_status, control_epoch, idempotency_key)
			VALUES ($1, $2, 'run_started', jsonb_build_object('assignment_id', $3::uuid), 'system', $4, 'pending', 'running', $5, $6)
			ON CONFLICT (idempotency_key) DO NOTHING`,
			uuid.New(), candidate.runID, candidate.id, transitionVersion, runEpoch,
			runTransitionIdempotencyKey(candidate.runID, transitionVersion, service.RunStatusPending, service.RunStatusRunning, runEpoch)); err != nil {
			return nil, fmt.Errorf("record evaluation run started event: %w", err)
		}
	}
	err = tx.QueryRowContext(ctx, `
		UPDATE evaluation_assignments
		SET status = 'leased', lease_token_hash = $2, leased_by = $3,
			lease_expires_at = NOW() + $4::interval, heartbeat_at = NOW(), lease_epoch = $5,
			worker_image_digest = NULLIF($6, ''), work_origin = NULLIF($7, ''), updated_at = NOW()
		WHERE id = $1 AND status = 'pending' AND lease_epoch = $8
		RETURNING lease_expires_at`,
		lease.ID, tokenHash, workerID, postgresInterval(leaseTTL), lease.LeaseEpoch, lease.WorkerImageDigest, lease.WorkOrigin, candidate.leaseEpoch).Scan(&lease.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("evaluation assignment became unavailable while locked")
	}
	if err != nil {
		return nil, fmt.Errorf("lease evaluation assignment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_samples
		SET status = 'leased', route_trace_id = $2, updated_at = NOW()
		WHERE id = $1`, lease.SampleID, lease.RouteTraceID); err != nil {
		return nil, fmt.Errorf("mark evaluation sample leased: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_runs
		SET started_at = COALESCE(started_at, NOW()),
			status = CASE WHEN status = 'pending' THEN 'running' ELSE status END,
			updated_at = NOW()
		WHERE id = $1`, candidate.runID); err != nil {
		return nil, fmt.Errorf("mark evaluation run started: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_workers SET last_heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'active'`, workerID); err != nil {
		return nil, fmt.Errorf("heartbeat evaluation worker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evaluation assignment claim: %w", err)
	}
	return lease, nil
}

func loadAssignmentExecutionContract(
	ctx context.Context,
	tx *sql.Tx,
	candidate assignmentCandidate,
	lease *service.AssignmentLease,
) error {
	lease.Case = &service.EvaluationCaseSpec{}
	lease.RouteConfig = append(json.RawMessage(nil), lease.ModelConfig...)
	lease.RouteTraceID = uuid.NewString()
	err := tx.QueryRowContext(ctx, `
		SELECT c.id, c.case_key, c.capability_domain, c.priority, c.weight,
		       c.prompt_spec, c.expected_spec, c.execution_spec,
		       c.grader_id, c.grader_version, c.content_sha256, c.confidentiality,
		       d.id, d.dataset_key, d.version, d.manifest_sha256, k.id, k.key
		FROM evaluation_cases c
		JOIN evaluation_dataset_versions d ON d.id = c.dataset_version_id
		JOIN evaluation_runs r ON r.id = $1
		JOIN evaluation_plans p ON p.id = r.plan_id AND p.dataset_version_id = d.id
		JOIN api_keys k ON k.id = p.gateway_api_key_id
		JOIN users u ON u.id = k.user_id
		LEFT JOIN groups g ON g.id = k.group_id
		WHERE c.id = $2
		  AND d.status = 'published'
		  AND k.is_evaluation = TRUE
		  AND k.status = 'active' AND k.deleted_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > NOW())
		  AND (k.quota = 0 OR k.quota_used < k.quota)
		  AND u.status = 'active' AND u.deleted_at IS NULL
		  AND (g.id IS NULL OR (g.status = 'active' AND g.deleted_at IS NULL))`,
		candidate.runID, candidate.caseID).Scan(
		&lease.Case.CaseID, &lease.Case.CaseKey, &lease.Case.CapabilityDomain,
		&lease.Case.Priority, &lease.Case.Weight, &lease.Case.PromptSpec,
		&lease.Case.ExpectedSpec, &lease.Case.ExecutionSpec, &lease.Case.GraderID,
		&lease.Case.GraderVersion, &lease.Case.ContentSHA256,
		&lease.Case.Confidentiality, &lease.DatasetVersionID, &lease.DatasetKey,
		&lease.DatasetVersion, &lease.DatasetManifestSHA256,
		&lease.GatewayAPIKeyID, &lease.GatewayAPIKey,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("evaluation plan has no usable dedicated gateway API key")
	}
	if err != nil {
		return fmt.Errorf("load evaluation assignment execution contract: %w", err)
	}
	return nil
}

func (r *evaluationRepository) RenewLease(ctx context.Context, assignmentID uuid.UUID, leaseToken string, extendBy time.Duration, leaseEpoch ...int64) (time.Time, error) {
	if r == nil || r.db == nil {
		return time.Time{}, errors.New("nil evaluation repository")
	}
	if extendBy <= 0 {
		return time.Time{}, errors.New("evaluation lease extension must be positive")
	}
	var expiresAt time.Time
	err := withEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("worker"), func(tx *sql.Tx) error {
		query := `
			UPDATE evaluation_assignments a
			SET lease_expires_at = NOW() + $3::interval, heartbeat_at = NOW(), updated_at = NOW()
			FROM evaluation_samples s
			JOIN evaluation_runs r ON r.id = s.run_id
			WHERE a.id = $1 AND a.sample_id = s.id AND a.lease_token_hash = $2 AND a.lease_expires_at > NOW()
				AND a.status IN ('leased', 'running') AND a.lease_epoch = r.control_epoch
				AND EXISTS (
					SELECT 1 FROM evaluation_workers w
					WHERE w.id = a.leased_by AND w.status = 'active' AND w.tenant_id = r.tenant_id
					)`
		args := []any{assignmentID, hashToken(leaseToken), postgresInterval(extendBy)}
		nextArg := 4
		if workerID, bound := service.RadarWorkerID(ctx); bound {
			query += fmt.Sprintf(` AND a.leased_by = $%d`, nextArg)
			args = append(args, workerID)
			nextArg++
		}
		if len(leaseEpoch) > 0 && leaseEpoch[0] > 0 {
			query += fmt.Sprintf(` AND a.lease_epoch = $%d`, nextArg)
			args = append(args, leaseEpoch[0])
		}
		query += ` RETURNING lease_expires_at`
		err := tx.QueryRowContext(ctx, query, args...).Scan(&expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrLeaseFenced
		}
		if err != nil {
			return fmt.Errorf("renew evaluation assignment lease: %w", err)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return expiresAt, nil
}

func (r *evaluationRepository) TransitionAssignment(ctx context.Context, input service.AssignmentTransition) error {
	if r == nil || r.db == nil {
		return errors.New("nil evaluation repository")
	}
	if input.AssignmentID == uuid.Nil || input.LeaseToken == "" || !input.To.Valid() {
		return errors.New("invalid evaluation assignment transition")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return fmt.Errorf("begin evaluation assignment transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var expectedEpoch any
	if input.LeaseEpoch > 0 {
		expectedEpoch = input.LeaseEpoch
	}

	var sampleID uuid.UUID
	var runID uuid.UUID
	var releasedWorkerID uuid.UUID
	if !assignmentLeaseActive(input.To) {
		var leasedBy sql.NullString
		assignmentQuery := `
			SELECT a.leased_by
			FROM evaluation_assignments a
			JOIN evaluation_samples s ON s.id = a.sample_id
			JOIN evaluation_runs r ON r.id = s.run_id
			JOIN evaluation_workers w ON w.id = a.leased_by AND w.status = 'active' AND w.tenant_id = r.tenant_id
			WHERE a.id = $1 AND a.lease_token_hash = $2 AND a.lease_expires_at > NOW()
			  AND a.status IN ('leased', 'running') AND a.lease_epoch = r.control_epoch
			  AND ($3::bigint IS NULL OR a.lease_epoch = $3)`
		assignmentArgs := []any{input.AssignmentID, hashToken(input.LeaseToken), expectedEpoch}
		if workerID, bound := service.RadarWorkerID(ctx); bound {
			assignmentQuery += ` AND a.leased_by = $4`
			assignmentArgs = append(assignmentArgs, workerID)
		}
		assignmentQuery += ` FOR UPDATE`
		if err := tx.QueryRowContext(ctx, assignmentQuery, assignmentArgs...).Scan(&leasedBy); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return service.ErrLeaseFenced
			}
			return fmt.Errorf("load evaluation assignment worker: %w", err)
		}
		if leasedBy.Valid {
			releasedWorkerID, err = uuid.Parse(leasedBy.String)
			if err != nil {
				return fmt.Errorf("parse evaluation assignment worker: %w", err)
			}
		}
	}
	if assignmentLeaseActive(input.To) {
		assignmentQuery := `
			UPDATE evaluation_assignments a
			SET status = $3, heartbeat_at = NOW(), started_at = COALESCE(started_at, NOW()), updated_at = NOW()
			FROM evaluation_samples s
			JOIN evaluation_runs r ON r.id = s.run_id
			JOIN evaluation_workers w ON w.status = 'active' AND w.tenant_id = r.tenant_id
			WHERE a.id = $1 AND a.sample_id = s.id AND a.lease_token_hash = $2 AND a.lease_expires_at > NOW()
				AND a.leased_by = w.id
				AND a.status IN ('leased', 'running') AND a.lease_epoch = r.control_epoch
				AND ($4::bigint IS NULL OR a.lease_epoch = $4)`
		assignmentArgs := []any{input.AssignmentID, hashToken(input.LeaseToken), input.To, expectedEpoch}
		if workerID, bound := service.RadarWorkerID(ctx); bound {
			assignmentQuery += ` AND a.leased_by = $5`
			assignmentArgs = append(assignmentArgs, workerID)
		}
		assignmentQuery += ` RETURNING a.sample_id`
		err = tx.QueryRowContext(ctx, assignmentQuery, assignmentArgs...).Scan(&sampleID)
	} else {
		assignmentQuery := `
			UPDATE evaluation_assignments a
			SET status = $3, lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL,
				heartbeat_at = NOW(), finished_at = NOW(), updated_at = NOW()
			FROM evaluation_samples s
			JOIN evaluation_runs r ON r.id = s.run_id
			JOIN evaluation_workers w ON w.status = 'active' AND w.tenant_id = r.tenant_id
			WHERE a.id = $1 AND a.sample_id = s.id AND a.lease_token_hash = $2 AND a.lease_expires_at > NOW()
				AND a.leased_by = w.id
				AND a.status IN ('leased', 'running') AND a.lease_epoch = r.control_epoch
				AND ($4::bigint IS NULL OR a.lease_epoch = $4)`
		assignmentArgs := []any{input.AssignmentID, hashToken(input.LeaseToken), input.To, expectedEpoch}
		if workerID, bound := service.RadarWorkerID(ctx); bound {
			assignmentQuery += ` AND a.leased_by = $5`
			assignmentArgs = append(assignmentArgs, workerID)
		}
		assignmentQuery += ` RETURNING a.sample_id`
		err = tx.QueryRowContext(ctx, assignmentQuery, assignmentArgs...).Scan(&sampleID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrLeaseFenced
	}
	if err != nil {
		return fmt.Errorf("transition evaluation assignment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_samples SET status = $2, updated_at = NOW() WHERE id = $1`, sampleID, input.To); err != nil {
		return fmt.Errorf("transition evaluation sample: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT run_id FROM evaluation_samples WHERE id = $1`, sampleID).Scan(&runID); err != nil {
		return fmt.Errorf("load evaluation run for reconcile: %w", err)
	}
	if releasedWorkerID != uuid.Nil {
		if _, err := checkRadarWorkerDrainCompletionTx(ctx, tx, releasedWorkerID, 0, "assignment:"+input.AssignmentID.String()); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit evaluation assignment transition: %w", err)
	}
	if _, err := r.ReconcileEvaluationRun(ctx, runID); err != nil {
		return fmt.Errorf("reconcile evaluation run after assignment transition: %w", err)
	}
	return nil
}

type evaluationCaseForRun struct {
	id               uuid.UUID
	datasetVersionID uuid.UUID
	priority         string
	sampleCount      int
	estimatedCost    decimal.Decimal
	promptSpec       []byte
	executionSpec    []byte
	graderID         string
	graderVersion    string
}

func loadEvaluationCases(ctx context.Context, tx *sql.Tx, planID uuid.UUID) ([]evaluationCaseForRun, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, p.dataset_version_id, c.priority, c.sample_count, c.estimated_cost,
			c.prompt_spec, c.execution_spec, c.grader_id, c.grader_version
		FROM evaluation_cases c
		JOIN evaluation_plans p ON p.dataset_version_id = c.dataset_version_id
		WHERE p.id = $1
		ORDER BY c.case_key`, planID)
	if err != nil {
		return nil, fmt.Errorf("load evaluation cases: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var cases []evaluationCaseForRun
	for rows.Next() {
		var evaluationCase evaluationCaseForRun
		if err := rows.Scan(&evaluationCase.id, &evaluationCase.datasetVersionID, &evaluationCase.priority, &evaluationCase.sampleCount, &evaluationCase.estimatedCost,
			&evaluationCase.promptSpec, &evaluationCase.executionSpec, &evaluationCase.graderID, &evaluationCase.graderVersion); err != nil {
			return nil, fmt.Errorf("scan evaluation case: %w", err)
		}
		cases = append(cases, evaluationCase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evaluation cases: %w", err)
	}
	return cases, nil
}

func insertEvaluationSampleAndAssignment(
	ctx context.Context,
	tx *sql.Tx,
	sampleID uuid.UUID,
	runID uuid.UUID,
	evaluationCase evaluationCaseForRun,
	modelRoute string,
	modelConfig []byte,
	modelConfigSHA256 string,
	sampleIndex int,
	leaseEpoch int64,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_samples (
			id, run_id, case_id, model_route, model_config, model_config_sha256,
			sample_index, priority, status, estimated_cost
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, 'pending', $9)`,
		sampleID, runID, evaluationCase.id, modelRoute, modelConfig,
		modelConfigSHA256, sampleIndex, evaluationCase.priority, evaluationCase.estimatedCost); err != nil {
		return fmt.Errorf("insert evaluation sample: %w", err)
	}
	assignmentID := uuid.New()
	idempotencyKey := assignmentIdempotencyKey(runID, evaluationCase.id, modelRoute, sampleIndex, 1)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_assignments (
			id, sample_id, attempt, idempotency_key, status, lease_epoch, work_origin
		) VALUES ($1, $2, 1, $3, 'pending', $4, 'initial')`, assignmentID, sampleID, idempotencyKey, leaseEpoch); err != nil {
		return fmt.Errorf("insert evaluation assignment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_budget_ledger (id, run_id, sample_id, assignment_id, entry_type, amount, idempotency_key)
		VALUES ($1, $2, $3, $4, 'reservation', $5, $6)`,
		uuid.New(), runID, sampleID, assignmentID, evaluationCase.estimatedCost, hashString("reservation\x00"+assignmentID.String())); err != nil {
		return fmt.Errorf("insert evaluation budget reservation: %w", err)
	}
	return nil
}

type assignmentCandidate struct {
	id                uuid.UUID
	sampleID          uuid.UUID
	runID             uuid.UUID
	caseID            uuid.UUID
	modelRoute        string
	modelConfig       []byte
	modelConfigSHA256 string
	priority          string
	sampleIndex       int
	attempt           int
	leaseEpoch        int64
	workOrigin        string
}

func reclaimExpiredAssignment(ctx context.Context, tx *sql.Tx, capabilities []string, workerTenantID int64) (*assignmentCandidate, error) {
	var expired assignmentCandidate
	err := tx.QueryRowContext(ctx, `
		SELECT a.id, a.sample_id, s.run_id, s.case_id, s.model_route,
			s.model_config, s.model_config_sha256, s.priority, s.sample_index, a.attempt,
			r.control_epoch, COALESCE(a.work_origin, 'initial')
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_cases c ON c.id = s.case_id
		JOIN evaluation_runs r ON r.id = s.run_id
		JOIN evaluation_plans p ON p.id = r.plan_id
		JOIN api_keys k ON k.id = p.gateway_api_key_id
		JOIN users u ON u.id = k.user_id
		LEFT JOIN groups g ON g.id = k.group_id
		WHERE a.status IN ('leased', 'running') AND a.lease_expires_at <= NOW()
			AND a.lease_epoch = r.control_epoch
			AND c.capability_domain = ANY($1::text[])
			AND ($2::bigint = 0 OR r.tenant_id = $2)
			AND p.enabled = TRUE
			AND k.is_evaluation = TRUE AND k.status = 'active' AND k.deleted_at IS NULL
			AND (k.expires_at IS NULL OR k.expires_at > NOW())
			AND (k.quota = 0 OR k.quota_used < k.quota)
			AND u.status = 'active' AND u.deleted_at IS NULL
			AND (g.id IS NULL OR (g.status = 'active' AND g.deleted_at IS NULL))
			AND (SELECT COUNT(*) FROM evaluation_assignments active
			     JOIN evaluation_samples active_sample ON active_sample.id = active.sample_id
			     JOIN evaluation_runs active_run ON active_run.id = active_sample.run_id
			     WHERE active_run.plan_id = p.id AND active.status IN ('leased', 'running')
			       AND active.lease_expires_at > NOW()) < p.max_concurrency
			AND (
				(r.status IN ('pending', 'running') AND r.reserved_cost < r.budget_limit)
				OR (r.status IN ('pending', 'running', 'budget_paused')
					AND r.reserved_cost = r.budget_limit AND s.priority = 'P0')
			)
		ORDER BY a.lease_expires_at, a.id
		FOR UPDATE OF a SKIP LOCKED
		LIMIT 1`, pq.Array(capabilities), workerTenantID).Scan(
		&expired.id, &expired.sampleID, &expired.runID, &expired.caseID,
		&expired.modelRoute, &expired.modelConfig, &expired.modelConfigSHA256,
		&expired.priority, &expired.sampleIndex, &expired.attempt, &expired.leaseEpoch, &expired.workOrigin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock expired evaluation assignment: %w", err)
	}
	replacementOrigin := expired.workOrigin
	expired.workOrigin = "reclaimed"
	eligible, err := lockRunLeaseEligibility(ctx, tx, expired.runID, expired.priority)
	if err != nil {
		return nil, err
	}
	if !eligible {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_assignments
		SET status = 'infra_failed', lease_token_hash = NULL, leased_by = NULL,
			lease_expires_at = NULL, heartbeat_at = NULL, failure_class = 'infrastructure',
			failure_code = 'lease_expired', finished_at = NOW(), updated_at = NOW()
		WHERE id = $1`, expired.id); err != nil {
		return nil, fmt.Errorf("expire evaluation assignment: %w", err)
	}
	nextAttempt := expired.attempt + 1
	replacement := expired
	replacement.id = uuid.New()
	replacement.attempt = nextAttempt
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status, lease_epoch, work_origin)
		VALUES ($1, $2, $3, $4, 'pending', $5, $6)`,
		replacement.id, expired.sampleID, nextAttempt,
		assignmentIdempotencyKey(expired.runID, expired.caseID, expired.modelRoute, expired.sampleIndex, nextAttempt), expired.leaseEpoch, replacementOrigin); err != nil {
		return nil, fmt.Errorf("create replacement evaluation assignment: %w", err)
	}
	if err := propagateAssignmentReplacement(ctx, tx, expired.runID, expired.sampleID, expired.id, replacement.id, expired.attempt); err != nil {
		return nil, err
	}
	return &replacement, nil
}

func selectPendingAssignment(ctx context.Context, tx *sql.Tx, capabilities []string, workerTenantID int64) (assignmentCandidate, error) {
	var candidate assignmentCandidate
	err := tx.QueryRowContext(ctx, `
		SELECT a.id, a.sample_id, s.run_id, s.case_id, s.model_route,
			s.model_config, s.model_config_sha256, s.priority, s.sample_index, a.attempt,
			r.control_epoch, COALESCE(a.work_origin, 'initial')
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_cases c ON c.id = s.case_id
		JOIN evaluation_runs r ON r.id = s.run_id
		JOIN evaluation_plans p ON p.id = r.plan_id
		JOIN api_keys k ON k.id = p.gateway_api_key_id
		JOIN users u ON u.id = k.user_id
		LEFT JOIN groups g ON g.id = k.group_id
		WHERE a.status = 'pending' AND a.lease_epoch = r.control_epoch
			AND c.capability_domain = ANY($1::text[])
			AND ($2::bigint = 0 OR r.tenant_id = $2)
			AND p.enabled = TRUE
			AND k.is_evaluation = TRUE AND k.status = 'active' AND k.deleted_at IS NULL
			AND (k.expires_at IS NULL OR k.expires_at > NOW())
			AND (k.quota = 0 OR k.quota_used < k.quota)
			AND u.status = 'active' AND u.deleted_at IS NULL
			AND (g.id IS NULL OR (g.status = 'active' AND g.deleted_at IS NULL))
			AND (SELECT COUNT(*) FROM evaluation_assignments active
			     JOIN evaluation_samples active_sample ON active_sample.id = active.sample_id
			     JOIN evaluation_runs active_run ON active_run.id = active_sample.run_id
			     WHERE active_run.plan_id = p.id AND active.status IN ('leased', 'running')
			       AND active.lease_expires_at > NOW()) < p.max_concurrency
			AND (
				(r.status IN ('pending', 'running') AND r.reserved_cost < r.budget_limit)
				OR (r.status IN ('pending', 'running', 'budget_paused')
					AND r.reserved_cost = r.budget_limit AND s.priority = 'P0')
			)
		ORDER BY s.priority, a.created_at, a.id
		FOR UPDATE OF a SKIP LOCKED
		LIMIT 1`, pq.Array(capabilities), workerTenantID).Scan(
		&candidate.id, &candidate.sampleID, &candidate.runID, &candidate.caseID,
		&candidate.modelRoute, &candidate.modelConfig, &candidate.modelConfigSHA256,
		&candidate.priority, &candidate.sampleIndex, &candidate.attempt, &candidate.leaseEpoch, &candidate.workOrigin)
	if err != nil {
		return assignmentCandidate{}, err
	}
	if candidate.workOrigin == "" {
		candidate.workOrigin = "initial"
	}
	return candidate, nil
}

type evaluationMatrixEntry struct {
	route                 string
	baselineConfig        []byte
	baselineConfigSHA256  string
	candidateConfig       []byte
	candidateConfigSHA256 string
}

func (entry evaluationMatrixEntry) configForSide(side string) ([]byte, string) {
	if side == "baseline" {
		return entry.baselineConfig, entry.baselineConfigSHA256
	}
	return entry.candidateConfig, entry.candidateConfigSHA256
}

func evaluationMatrixEntries(matrixJSON []byte) ([]evaluationMatrixEntry, error) {
	var matrix []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(matrixJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&matrix); err != nil {
		return nil, fmt.Errorf("decode evaluation model matrix: %w", err)
	}
	entries := make([]evaluationMatrixEntry, 0, len(matrix))
	seen := make(map[string]struct{}, len(matrix))
	for _, entry := range matrix {
		route := ""
		for _, key := range []string{"route", "model_route", "model", "id"} {
			if value, ok := entry[key].(string); ok && strings.TrimSpace(value) != "" {
				route = strings.TrimSpace(value)
				break
			}
		}
		if route == "" {
			return nil, errors.New("evaluation model matrix entry has no route")
		}
		if _, exists := seen[route]; exists {
			return nil, fmt.Errorf("evaluation model matrix duplicates route %q", route)
		}
		seen[route] = struct{}{}
		baseline, err := evaluationMatrixSideConfig(entry, "baseline", route)
		if err != nil {
			return nil, err
		}
		candidate, err := evaluationMatrixSideConfig(entry, "candidate", route)
		if err != nil {
			return nil, err
		}
		baselineHash := hashString(string(baseline))
		candidateHash := hashString(string(candidate))
		if baselineHash == candidateHash {
			return nil, fmt.Errorf("evaluation model matrix entry %q has identical baseline and candidate configurations", route)
		}
		entries = append(entries, evaluationMatrixEntry{
			route:          route,
			baselineConfig: baseline, baselineConfigSHA256: baselineHash,
			candidateConfig: candidate, candidateConfigSHA256: candidateHash,
		})
	}
	if len(entries) == 0 {
		return nil, errors.New("evaluation model matrix is empty")
	}
	return entries, nil
}

func evaluationMatrixSideConfig(entry map[string]any, side, pairRoute string) ([]byte, error) {
	value, ok := entry[side]
	if !ok {
		return nil, fmt.Errorf("evaluation model matrix entry %q has no %s configuration", pairRoute, side)
	}
	configMap, ok := value.(map[string]any)
	if !ok || len(configMap) == 0 {
		return nil, fmt.Errorf("evaluation model matrix entry %q has invalid %s configuration", pairRoute, side)
	}
	modelRoute := ""
	for _, key := range []string{"route", "model_route", "model", "id"} {
		if route, ok := configMap[key].(string); ok && strings.TrimSpace(route) != "" {
			modelRoute = strings.TrimSpace(route)
			break
		}
	}
	if modelRoute == "" {
		return nil, fmt.Errorf("evaluation model matrix entry %q %s configuration has no route", pairRoute, side)
	}
	raw, err := json.Marshal(configMap)
	if err != nil {
		return nil, fmt.Errorf("marshal evaluation model matrix entry %q %s configuration: %w", pairRoute, side, err)
	}
	config, err := canonicalizeModelConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize evaluation model matrix entry %q %s configuration: %w", pairRoute, side, err)
	}
	return config, nil
}

func canonicalizeModelConfig(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode JSON: multiple values")
		}
		return nil, fmt.Errorf("decode JSON suffix: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal JSON: %w", err)
	}
	return canonical, nil
}

func intersectCapabilities(requested, registered []string) []string {
	allowed := make(map[string]struct{}, len(registered))
	for _, capability := range registered {
		allowed[capability] = struct{}{}
	}
	result := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, capability := range requested {
		if _, ok := allowed[capability]; !ok {
			continue
		}
		if _, duplicate := seen[capability]; duplicate {
			continue
		}
		seen[capability] = struct{}{}
		result = append(result, capability)
	}
	return result
}

func lockRunLeaseEligibility(ctx context.Context, tx *sql.Tx, runID uuid.UUID, priority string) (bool, error) {
	var status service.RunStatus
	var reservedCost, budgetLimit decimal.Decimal
	var planID uuid.UUID
	state := evaluationPlanControlState{}
	if err := tx.QueryRowContext(ctx, `
		SELECT r.status, r.reserved_cost, r.budget_limit, p.id, p.enabled, p.max_concurrency,
		       TRUE
		FROM evaluation_runs r
		JOIN evaluation_plans p ON p.id = r.plan_id
		JOIN api_keys k ON k.id = p.gateway_api_key_id
		JOIN users u ON u.id = k.user_id
		LEFT JOIN groups g ON g.id = k.group_id
		WHERE r.id = $1
		  AND k.is_evaluation = TRUE AND k.status = 'active' AND k.deleted_at IS NULL
		  AND (k.expires_at IS NULL OR k.expires_at > NOW())
		  AND (k.quota = 0 OR k.quota_used < k.quota)
		  AND u.status = 'active' AND u.deleted_at IS NULL
		  AND (g.id IS NULL OR (g.status = 'active' AND g.deleted_at IS NULL))
		FOR UPDATE OF r, p`, runID).Scan(
		&status, &reservedCost, &budgetLimit, &planID, &state.enabled,
		&state.maxConcurrency, &state.keyUsable,
	); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lock evaluation run budget: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_runs r ON r.id = s.run_id
		WHERE r.plan_id = $1 AND a.status IN ('leased', 'running')
		  AND a.lease_expires_at > NOW()`, planID).Scan(&state.activeLeases); err != nil {
		return false, fmt.Errorf("count active evaluation plan leases: %w", err)
	}
	if !assignmentLeaseEligible(state) {
		return false, nil
	}
	if (status == service.RunStatusPending || status == service.RunStatusRunning) && reservedCost.LessThan(budgetLimit) {
		return true, nil
	}
	return priority == string(service.CasePriorityP0) && reservedCost.Equal(budgetLimit) &&
		(status == service.RunStatusPending || status == service.RunStatusRunning || status == service.RunStatusBudgetPaused), nil
}

type evaluationPlanControlState struct {
	enabled           bool
	keyUsable         bool
	dailyCostLimit    decimal.Decimal
	dailyReservedCost decimal.Decimal
	maxConcurrency    int
	activeLeases      int
}

func runCreationEligible(state evaluationPlanControlState, reservation decimal.Decimal) bool {
	return state.enabled && state.keyUsable && state.dailyCostLimit.GreaterThan(decimal.Zero) &&
		state.dailyReservedCost.Add(reservation).LessThanOrEqual(state.dailyCostLimit)
}

func assignmentLeaseEligible(state evaluationPlanControlState) bool {
	return state.enabled && state.keyUsable && state.maxConcurrency > 0 &&
		state.activeLeases < state.maxConcurrency
}

func insertEvaluationBudgetWarning(
	ctx context.Context,
	tx *sql.Tx,
	runID uuid.UUID,
	controlEpoch int64,
	reservedCost decimal.Decimal,
	budgetLimit decimal.Decimal,
) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_budget_ledger (
			id, run_id, entry_type, amount, idempotency_key
		) VALUES ($1, $2, 'warning', $3, $4)`,
		uuid.New(), runID, reservedCost, hashString("budget_warning\x00"+runID.String())); err != nil {
		return fmt.Errorf("insert evaluation budget warning ledger entry: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_run_events (
			id, run_id, event_type, payload, actor_type, control_epoch, idempotency_key
		)
		VALUES ($1, $2, 'budget_warning', jsonb_build_object(
			'reserved_cost', $3::text, 'budget_limit', $4::text, 'threshold_percent', 80
		), 'system', $5, $6)`, uuid.New(), runID, reservedCost, budgetLimit, controlEpoch,
		hashReconcileKey("budget-warning:"+runID.String())); err != nil {
		return fmt.Errorf("insert evaluation budget warning event: %w", err)
	}
	return nil
}

func assignmentIdempotencyKey(runID, caseID uuid.UUID, modelRoute string, sampleIndex, attempt int) string {
	return hashString(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", runID, caseID, modelRoute, sampleIndex, attempt))
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func hashToken(token string) string {
	return hashString(token)
}

func newLeaseToken() (string, string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", fmt.Errorf("generate evaluation lease token: %w", err)
	}
	token := hex.EncodeToString(bytes)
	return token, hashToken(token), nil
}

func postgresInterval(value time.Duration) string {
	return fmt.Sprintf("%d microseconds", value.Microseconds())
}

func assignmentLeaseActive(status service.AssignmentStatus) bool {
	return status == service.AssignmentStatusLeased || status == service.AssignmentStatusRunning
}

func validTriggerSource(source string) bool {
	switch source {
	case "manual", "cron", "release", "event", "recovery":
		return true
	default:
		return false
	}
}

func marshalJSONObject(value map[string]any) ([]byte, error) {
	if value == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(value)
}

func strconvFormatInt(value int64) string {
	if value == 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}
