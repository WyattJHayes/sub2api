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

	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("control"))
	if err != nil {
		return nil, fmt.Errorf("begin evaluation run transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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
		ID:           uuid.New(),
		PlanID:       input.PlanID,
		Status:       service.RunStatusPending,
		BudgetLimit:  budgetLimit,
		ReservedCost: totalReservation,
	}
	if totalReservation.Equal(budgetLimit) {
		run.Status = service.RunStatusBudgetPaused
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_runs (
			id, plan_id, trigger_source, baseline_ref, candidate_ref, status,
			budget_limit, reserved_cost, created_by
		) VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, NULLIF($9, 0))
		RETURNING created_at`,
		run.ID, run.PlanID, input.TriggerSource, baselineRef, candidateRef,
		run.Status, run.BudgetLimit, run.ReservedCost, input.CreatedBy).Scan(&run.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert evaluation run: %w", err)
	}
	manifest := defaultRequestManifest(run.ID)
	manifestBytes, manifestHash, err := service.CanonicalRequestManifest(manifest)
	if err != nil {
		return nil, fmt.Errorf("build request manifest: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_request_manifests (
			id, schema_version, interaction_type, canonical_manifest_bytes, manifest_sha256
		) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (manifest_sha256) DO NOTHING`, manifest.ID, manifest.SchemaVersion,
		manifest.InteractionType, manifestBytes, manifestHash); err != nil {
		return nil, fmt.Errorf("insert run request manifest: %w", err)
	}
	run.ContractStatus = "bound"
	run.RequestManifestID = manifest.ID
	run.RequestManifestSHA256 = manifestHash

	for _, evaluationCase := range cases {
		for matrixIndex, matrixEntry := range matrix {
			for sampleIndex := 0; sampleIndex < evaluationCase.sampleCount; sampleIndex++ {
				baselineConfig, baselineConfigHash := matrixEntry.configForSide("baseline")
				candidateConfig, candidateConfigHash := matrixEntry.configForSide("candidate")
				baselineSampleID, err := insertEvaluationSampleAndAssignment(
					ctx, tx, run.ID, evaluationCase, "baseline:"+matrixEntry.route,
					baselineConfig, baselineConfigHash, sampleIndex,
				)
				if err != nil {
					return nil, err
				}
				candidateSampleID, err := insertEvaluationSampleAndAssignment(
					ctx, tx, run.ID, evaluationCase, "candidate:"+matrixEntry.route,
					candidateConfig, candidateConfigHash, sampleIndex,
				)
				if err != nil {
					return nil, err
				}
				pair := defaultPairSpec(run, evaluationCase, manifest, matrixIndex, sampleIndex)
				baseline := defaultSideSpec("baseline", matrixEntry.route, baselineConfigHash, baselineSampleID)
				candidate := defaultSideSpec("candidate", matrixEntry.route, candidateConfigHash, candidateSampleID)
				baseline.PairSpecID = pair.ID
				candidate.PairSpecID = pair.ID
				pairBytes, pairHash, err := service.CanonicalPairSpec(pair)
				if err != nil {
					return nil, err
				}
				baselineBytes, baselineHash, err := service.CanonicalSideSpec(baseline)
				if err != nil {
					return nil, err
				}
				candidateBytes, candidateHash, err := service.CanonicalSideSpec(candidate)
				if err != nil {
					return nil, err
				}
				binding, err := service.BuildPairBinding(pair, baseline, candidate)
				if err != nil {
					return nil, err
				}
				if err := insertPairContractTx(ctx, tx, run.ID, manifest, pair, baseline, candidate,
					manifestBytes, manifestHash, pairBytes, pairHash, baselineBytes, baselineHash,
					candidateBytes, candidateHash, binding, nil); err != nil {
					return nil, err
				}
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_run_events (id, run_id, event_type, payload, actor_type, actor_ref)
		VALUES ($1, $2, 'run_created', '{}'::jsonb, 'user', NULLIF($3, ''))`,
		uuid.New(), run.ID, strconvFormatInt(input.CreatedBy)); err != nil {
		return nil, fmt.Errorf("insert run created event: %w", err)
	}
	warningThreshold := budgetLimit.Mul(decimal.NewFromInt(8)).Div(decimal.NewFromInt(10))
	if !totalReservation.LessThan(warningThreshold) {
		if err := insertEvaluationBudgetWarning(ctx, tx, run.ID, totalReservation, budgetLimit); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evaluation run transaction: %w", err)
	}
	return run, nil
}

func defaultRequestManifest(runID uuid.UUID) service.RequestManifest {
	semanticsHash := hashString("request:" + runID.String())
	toolHash := hashString("{}")
	return service.RequestManifest{
		ID:              uuid.New(),
		SchemaVersion:   service.RequestManifestSchemaVersion,
		InteractionType: service.InteractionSingle,
		OrdinalPolicy:   service.OrdinalPolicyExact,
		MinRequests:     1,
		MaxRequests:     1,
		RequestSlots: []service.RequestSlot{{
			SlotID:                         "slot-0",
			OrdinalMin:                     0,
			OrdinalMax:                     0,
			Phase:                          "prompt",
			Required:                       true,
			SemanticsMode:                  service.SemanticsModeExact,
			ExpectedRequestSemanticsSHA256: semanticsHash,
			ToolSchemaSHA256:               toolHash,
			AllowedToolSetSHA256:           toolHash,
			MaxOccurrences:                 1,
		}},
	}
}

func defaultPairSpec(run *service.EvaluationRun, evaluationCase evaluationCaseForRun, manifest service.RequestManifest, repeatIndex, sampleIndex int) service.PairSpec {
	return service.PairSpec{
		ID:                            uuid.New(),
		DatasetVersionID:              evaluationCase.datasetVersionID,
		CaseID:                        evaluationCase.id,
		SampleIndex:                   sampleIndex,
		RepeatIndex:                   repeatIndex,
		ExpectedRequestManifestID:     manifest.ID,
		ExpectedRequestManifestSHA256: run.RequestManifestSHA256,
		PromptSHA256:                  hashString("prompt:" + evaluationCase.id.String()),
		ToolSchemaSHA256:              hashString("{}"),
		GraderID:                      evaluationCase.graderID,
		GraderVersion:                 evaluationCase.graderVersion,
		SamplingPolicy:                "model-config-bound",
		RandomSeed:                    int64(sampleIndex),
		Region:                        "legacy-unbound",
		Protocol:                      "openai-chat",
		TimeBlock:                     run.CreatedAt.UTC().Format(time.RFC3339),
		InterleaveOrder:               "round_robin",
		RetryPolicy:                   "same-route-once",
		AllowedTreatmentFields:        []string{"model_config_sha256"},
	}
}

func defaultSideSpec(side, route, modelConfigHash string, sampleID uuid.UUID) service.SideSpec {
	return service.SideSpec{
		ID:                       uuid.New(),
		SampleID:                 sampleID,
		Side:                     side,
		ModelRoute:               side + ":" + route,
		ModelConfigSHA256:        modelConfigHash,
		ExpectedModelAlias:       route,
		ExpectedResolvedModel:    route,
		RouteProfileVersion:      "legacy-unbound",
		ProviderParametersSHA256: hashString("{}"),
	}
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
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("runner"))
	if err != nil {
		return nil, fmt.Errorf("begin evaluation assignment claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var registeredCapabilities pq.StringArray
	var claimMode string
	err = tx.QueryRowContext(ctx, `
		SELECT capabilities, claim_mode FROM evaluation_workers
		WHERE id = $1 AND status = 'active'
		FOR UPDATE`, workerID).Scan(&registeredCapabilities, &claimMode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("evaluation worker is unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("lock evaluation worker: %w", err)
	}
	if claimMode != "open" {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit paused evaluation assignment claim: %w", err)
		}
		return nil, nil
	}
	authorizedCapabilities := intersectCapabilities(capabilities, registeredCapabilities)
	if len(authorizedCapabilities) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit unauthorized evaluation assignment claim: %w", err)
		}
		return nil, nil
	}

	reclaimed, err := reclaimExpiredAssignment(ctx, tx, authorizedCapabilities)
	if err != nil {
		return nil, err
	}
	var candidate assignmentCandidate
	if reclaimed != nil {
		candidate = *reclaimed
	} else {
		candidate, err = selectPendingAssignment(ctx, tx, authorizedCapabilities)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("commit empty evaluation assignment claim: %w", err)
			}
			return nil, nil
		}
		if err != nil {
			return nil, err
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
	}
	if err := loadAssignmentExecutionContract(ctx, tx, candidate, lease); err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `
		UPDATE evaluation_assignments
		SET status = 'leased', lease_token_hash = $2, leased_by = $3,
			lease_expires_at = NOW() + $4::interval, heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'pending'
		RETURNING lease_expires_at`,
		lease.ID, tokenHash, workerID, postgresInterval(leaseTTL)).Scan(&lease.ExpiresAt)
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

func (r *evaluationRepository) RenewLease(ctx context.Context, assignmentID uuid.UUID, leaseToken string, extendBy time.Duration) (time.Time, error) {
	if r == nil || r.db == nil {
		return time.Time{}, errors.New("nil evaluation repository")
	}
	if extendBy <= 0 {
		return time.Time{}, errors.New("evaluation lease extension must be positive")
	}
	var expiresAt time.Time
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("runner"), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		UPDATE evaluation_assignments
		SET lease_expires_at = NOW() + $3::interval, heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND lease_token_hash = $2 AND lease_expires_at > NOW()
			AND status IN ('leased', 'running')
		RETURNING lease_expires_at`, assignmentID, hashToken(leaseToken), postgresInterval(extendBy)).Scan(&expiresAt)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, service.ErrLeaseFenced
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("renew evaluation assignment lease: %w", err)
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
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("runner"))
	if err != nil {
		return fmt.Errorf("begin evaluation assignment transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var sampleID uuid.UUID
	if assignmentLeaseActive(input.To) {
		err = tx.QueryRowContext(ctx, `
			UPDATE evaluation_assignments
			SET status = $3, heartbeat_at = NOW(), started_at = COALESCE(started_at, NOW()), updated_at = NOW()
			WHERE id = $1 AND lease_token_hash = $2 AND lease_expires_at > NOW()
				AND status IN ('leased', 'running')
			RETURNING sample_id`, input.AssignmentID, hashToken(input.LeaseToken), input.To).Scan(&sampleID)
	} else {
		err = tx.QueryRowContext(ctx, `
			UPDATE evaluation_assignments
			SET status = $3, lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL,
				heartbeat_at = NOW(), finished_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND lease_token_hash = $2 AND lease_expires_at > NOW()
				AND status IN ('leased', 'running')
			RETURNING sample_id`, input.AssignmentID, hashToken(input.LeaseToken), input.To).Scan(&sampleID)
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
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit evaluation assignment transition: %w", err)
	}
	return nil
}

type evaluationCaseForRun struct {
	id               uuid.UUID
	datasetVersionID uuid.UUID
	priority         string
	sampleCount      int
	estimatedCost    decimal.Decimal
	graderID         string
	graderVersion    string
}

func loadEvaluationCases(ctx context.Context, tx *sql.Tx, planID uuid.UUID) ([]evaluationCaseForRun, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.dataset_version_id, c.priority, c.sample_count, c.estimated_cost,
		       c.grader_id, c.grader_version
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
		if err := rows.Scan(&evaluationCase.id, &evaluationCase.datasetVersionID, &evaluationCase.priority,
			&evaluationCase.sampleCount, &evaluationCase.estimatedCost, &evaluationCase.graderID,
			&evaluationCase.graderVersion); err != nil {
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
	runID uuid.UUID,
	evaluationCase evaluationCaseForRun,
	modelRoute string,
	modelConfig []byte,
	modelConfigSHA256 string,
	sampleIndex int,
) (uuid.UUID, error) {
	sampleID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_samples (
			id, run_id, case_id, model_route, model_config, model_config_sha256,
			sample_index, priority, status, estimated_cost
		) VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, $8, 'pending', $9)`,
		sampleID, runID, evaluationCase.id, modelRoute, modelConfig,
		modelConfigSHA256, sampleIndex, evaluationCase.priority, evaluationCase.estimatedCost); err != nil {
		return uuid.Nil, fmt.Errorf("insert evaluation sample: %w", err)
	}
	assignmentID := uuid.New()
	idempotencyKey := assignmentIdempotencyKey(runID, evaluationCase.id, modelRoute, sampleIndex, 1)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status)
		VALUES ($1, $2, 1, $3, 'pending')`, assignmentID, sampleID, idempotencyKey); err != nil {
		return uuid.Nil, fmt.Errorf("insert evaluation assignment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_budget_ledger (id, run_id, sample_id, assignment_id, entry_type, amount, idempotency_key)
		VALUES ($1, $2, $3, $4, 'reservation', $5, $6)`,
		uuid.New(), runID, sampleID, assignmentID, evaluationCase.estimatedCost, hashString("reservation\x00"+assignmentID.String())); err != nil {
		return uuid.Nil, fmt.Errorf("insert evaluation budget reservation: %w", err)
	}
	return sampleID, nil
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
}

func reclaimExpiredAssignment(ctx context.Context, tx *sql.Tx, capabilities []string) (*assignmentCandidate, error) {
	var expired assignmentCandidate
	err := tx.QueryRowContext(ctx, `
		SELECT a.id, a.sample_id, s.run_id, s.case_id, s.model_route,
			s.model_config, s.model_config_sha256, s.priority, s.sample_index, a.attempt
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_cases c ON c.id = s.case_id
		JOIN evaluation_runs r ON r.id = s.run_id
		JOIN evaluation_plans p ON p.id = r.plan_id
		JOIN api_keys k ON k.id = p.gateway_api_key_id
		JOIN users u ON u.id = k.user_id
		LEFT JOIN groups g ON g.id = k.group_id
		WHERE a.status IN ('leased', 'running') AND a.lease_expires_at <= NOW()
			AND c.capability_domain = ANY($1::text[])
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
		LIMIT 1`, pq.Array(capabilities)).Scan(
		&expired.id, &expired.sampleID, &expired.runID, &expired.caseID,
		&expired.modelRoute, &expired.modelConfig, &expired.modelConfigSHA256,
		&expired.priority, &expired.sampleIndex, &expired.attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock expired evaluation assignment: %w", err)
	}
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
		INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status)
		VALUES ($1, $2, $3, $4, 'pending')`,
		replacement.id, expired.sampleID, nextAttempt,
		assignmentIdempotencyKey(expired.runID, expired.caseID, expired.modelRoute, expired.sampleIndex, nextAttempt)); err != nil {
		return nil, fmt.Errorf("create replacement evaluation assignment: %w", err)
	}
	return &replacement, nil
}

func selectPendingAssignment(ctx context.Context, tx *sql.Tx, capabilities []string) (assignmentCandidate, error) {
	var candidate assignmentCandidate
	err := tx.QueryRowContext(ctx, `
		SELECT a.id, a.sample_id, s.run_id, s.case_id, s.model_route,
			s.model_config, s.model_config_sha256, s.priority, s.sample_index, a.attempt
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_cases c ON c.id = s.case_id
		JOIN evaluation_runs r ON r.id = s.run_id
		JOIN evaluation_plans p ON p.id = r.plan_id
		JOIN api_keys k ON k.id = p.gateway_api_key_id
		JOIN users u ON u.id = k.user_id
		LEFT JOIN groups g ON g.id = k.group_id
		WHERE a.status = 'pending'
			AND c.capability_domain = ANY($1::text[])
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
		LIMIT 1`, pq.Array(capabilities)).Scan(
		&candidate.id, &candidate.sampleID, &candidate.runID, &candidate.caseID,
		&candidate.modelRoute, &candidate.modelConfig, &candidate.modelConfigSHA256,
		&candidate.priority, &candidate.sampleIndex, &candidate.attempt)
	if err != nil {
		return assignmentCandidate{}, err
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
		INSERT INTO evaluation_run_events (id, run_id, event_type, payload, actor_type)
		VALUES ($1, $2, 'budget_warning', jsonb_build_object(
			'reserved_cost', $3::text, 'budget_limit', $4::text, 'threshold_percent', 80
		), 'system')`, uuid.New(), runID, reservedCost, budgetLimit); err != nil {
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
