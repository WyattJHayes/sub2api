package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin evaluation run transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		datasetStatus string
		matrixJSON    []byte
		budgetLimit   decimal.Decimal
	)
	err = tx.QueryRowContext(ctx, `
		SELECT d.status, p.model_matrix, p.max_run_cost
		FROM evaluation_plans p
		JOIN evaluation_dataset_versions d ON d.id = p.dataset_version_id
		WHERE p.id = $1
		FOR UPDATE OF p`, input.PlanID).Scan(&datasetStatus, &matrixJSON, &budgetLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("evaluation plan %s not found", input.PlanID)
	}
	if err != nil {
		return nil, fmt.Errorf("lock evaluation plan: %w", err)
	}
	if datasetStatus != string(service.DatasetStatusPublished) {
		return nil, fmt.Errorf("evaluation dataset for plan %s is not published", input.PlanID)
	}

	routes, err := matrixRoutes(matrixJSON)
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
		multiplicity := int64(len(routes) * 2 * evaluationCase.sampleCount)
		totalReservation = totalReservation.Add(evaluationCase.estimatedCost.Mul(decimal.NewFromInt(multiplicity)))
	}
	if totalReservation.GreaterThan(budgetLimit) {
		return nil, fmt.Errorf("%w: reservation %s exceeds budget %s", service.ErrBudgetExceeded, totalReservation, budgetLimit)
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
	err = tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_runs (
			id, plan_id, trigger_source, baseline_ref, candidate_ref, status,
			budget_limit, reserved_cost, created_by
		) VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, 'pending', $6, $7, NULLIF($8, 0))
		RETURNING created_at`,
		run.ID, run.PlanID, input.TriggerSource, baselineRef, candidateRef,
		run.BudgetLimit, run.ReservedCost, input.CreatedBy).Scan(&run.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert evaluation run: %w", err)
	}

	for _, evaluationCase := range cases {
		for _, route := range routes {
			for _, side := range []string{"baseline", "candidate"} {
				modelRoute := side + ":" + route
				for sampleIndex := 0; sampleIndex < evaluationCase.sampleCount; sampleIndex++ {
					if err := insertEvaluationSampleAndAssignment(ctx, tx, run.ID, evaluationCase, modelRoute, sampleIndex); err != nil {
						return nil, err
					}
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
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin evaluation assignment claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var workerExists int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM evaluation_workers
		WHERE id = $1 AND status = 'active'
		FOR KEY SHARE`, workerID).Scan(&workerExists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("evaluation worker is unavailable")
	}
	if err != nil {
		return nil, fmt.Errorf("lock evaluation worker: %w", err)
	}

	reclaimed, err := reclaimExpiredAssignment(ctx, tx, capabilities)
	if err != nil {
		return nil, err
	}
	var candidate assignmentCandidate
	if reclaimed != nil {
		candidate = *reclaimed
	} else {
		candidate, err = selectPendingAssignment(ctx, tx, capabilities)
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

	token, tokenHash, err := newLeaseToken()
	if err != nil {
		return nil, err
	}
	lease := &service.AssignmentLease{
		ID:         candidate.id,
		SampleID:   candidate.sampleID,
		RunID:      candidate.runID,
		ModelRoute: candidate.modelRoute,
		Attempt:    candidate.attempt,
		Token:      token,
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
		UPDATE evaluation_samples SET status = 'leased', updated_at = NOW()
		WHERE id = $1`, lease.SampleID); err != nil {
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

func (r *evaluationRepository) RenewLease(ctx context.Context, assignmentID uuid.UUID, leaseToken string, extendBy time.Duration) (time.Time, error) {
	if r == nil || r.db == nil {
		return time.Time{}, errors.New("nil evaluation repository")
	}
	if extendBy <= 0 {
		return time.Time{}, errors.New("evaluation lease extension must be positive")
	}
	var expiresAt time.Time
	err := r.db.QueryRowContext(ctx, `
		UPDATE evaluation_assignments
		SET lease_expires_at = NOW() + $3::interval, heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND lease_token_hash = $2 AND lease_expires_at > NOW()
			AND status IN ('leased', 'running')
		RETURNING lease_expires_at`, assignmentID, hashToken(leaseToken), postgresInterval(extendBy)).Scan(&expiresAt)
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
	tx, err := r.db.BeginTx(ctx, nil)
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
	id            uuid.UUID
	priority      string
	sampleCount   int
	estimatedCost decimal.Decimal
}

func loadEvaluationCases(ctx context.Context, tx *sql.Tx, planID uuid.UUID) ([]evaluationCaseForRun, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.priority, c.sample_count, c.estimated_cost
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
		if err := rows.Scan(&evaluationCase.id, &evaluationCase.priority, &evaluationCase.sampleCount, &evaluationCase.estimatedCost); err != nil {
			return nil, fmt.Errorf("scan evaluation case: %w", err)
		}
		cases = append(cases, evaluationCase)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evaluation cases: %w", err)
	}
	return cases, nil
}

func insertEvaluationSampleAndAssignment(ctx context.Context, tx *sql.Tx, runID uuid.UUID, evaluationCase evaluationCaseForRun, modelRoute string, sampleIndex int) error {
	sampleID := uuid.New()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_samples (
			id, run_id, case_id, model_route, sample_index, priority, status, estimated_cost
		) VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7)`,
		sampleID, runID, evaluationCase.id, modelRoute, sampleIndex, evaluationCase.priority, evaluationCase.estimatedCost); err != nil {
		return fmt.Errorf("insert evaluation sample: %w", err)
	}
	assignmentID := uuid.New()
	idempotencyKey := assignmentIdempotencyKey(runID, evaluationCase.id, modelRoute, sampleIndex, 1)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_assignments (id, sample_id, attempt, idempotency_key, status)
		VALUES ($1, $2, 1, $3, 'pending')`, assignmentID, sampleID, idempotencyKey); err != nil {
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
	id          uuid.UUID
	sampleID    uuid.UUID
	runID       uuid.UUID
	caseID      uuid.UUID
	modelRoute  string
	sampleIndex int
	attempt     int
}

func reclaimExpiredAssignment(ctx context.Context, tx *sql.Tx, capabilities []string) (*assignmentCandidate, error) {
	var expired assignmentCandidate
	err := tx.QueryRowContext(ctx, `
		SELECT a.id, a.sample_id, s.run_id, s.case_id, s.model_route, s.sample_index, a.attempt
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_cases c ON c.id = s.case_id
		WHERE a.status IN ('leased', 'running') AND a.lease_expires_at <= NOW()
			AND (cardinality($1::text[]) = 0 OR c.capability_domain = ANY($1::text[]))
		ORDER BY a.lease_expires_at, a.id
		FOR UPDATE OF a SKIP LOCKED
		LIMIT 1`, pq.Array(capabilities)).Scan(
		&expired.id, &expired.sampleID, &expired.runID, &expired.caseID,
		&expired.modelRoute, &expired.sampleIndex, &expired.attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock expired evaluation assignment: %w", err)
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
		SELECT a.id, a.sample_id, s.run_id, s.case_id, s.model_route, s.sample_index, a.attempt
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_cases c ON c.id = s.case_id
		WHERE a.status = 'pending'
			AND (cardinality($1::text[]) = 0 OR c.capability_domain = ANY($1::text[]))
		ORDER BY s.priority, a.created_at, a.id
		FOR UPDATE OF a SKIP LOCKED
		LIMIT 1`, pq.Array(capabilities)).Scan(
		&candidate.id, &candidate.sampleID, &candidate.runID, &candidate.caseID,
		&candidate.modelRoute, &candidate.sampleIndex, &candidate.attempt)
	if err != nil {
		return assignmentCandidate{}, err
	}
	return candidate, nil
}

func matrixRoutes(matrixJSON []byte) ([]string, error) {
	var matrix []map[string]any
	if err := json.Unmarshal(matrixJSON, &matrix); err != nil {
		return nil, fmt.Errorf("decode evaluation model matrix: %w", err)
	}
	routes := make([]string, 0, len(matrix))
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
		routes = append(routes, route)
	}
	if len(routes) == 0 {
		return nil, errors.New("evaluation model matrix is empty")
	}
	return routes, nil
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
