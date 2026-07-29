package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

type evaluationAggregateRepository struct {
	db *sql.DB
}

type frozenAnalysisScoreInput struct {
	service.ScoreRef
	HeadEventID     uuid.UUID `json:"head_event_id"`
	RevisionBatchID uuid.UUID `json:"revision_batch_id,omitempty"`
}

func NewEvaluationAggregateRepository(db *sql.DB) service.EvaluationAggregateRepository {
	return &evaluationAggregateRepository{db: db}
}

func (r *evaluationAggregateRepository) EnsureCellAnalysisJob(ctx context.Context, request service.CellAnalysisJobRequest) (*service.AnalysisJobRevision, error) {
	request.CapabilityDomain = strings.TrimSpace(request.CapabilityDomain)
	request.ModelRoute = service.CanonicalModelRoute(request.ModelRoute)
	request.AnalysisVersion = strings.TrimSpace(request.AnalysisVersion)
	if r == nil || r.db == nil || request.RunID == uuid.Nil || request.CapabilityDomain == "" || request.ModelRoute == "" || request.AnalysisVersion == "" {
		return nil, service.ErrAggregateRevisionInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin cell analysis job: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockAggregateRun(ctx, tx, request.RunID); err != nil {
		return nil, err
	}

	var expected int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM evaluation_pair_specs pair_spec
		JOIN evaluation_cases case_spec ON case_spec.id=pair_spec.case_id
		JOIN evaluation_pair_bindings binding ON binding.pair_spec_id=pair_spec.id
		JOIN evaluation_side_specs baseline_side
		  ON baseline_side.id=binding.baseline_side_spec_id AND baseline_side.side='baseline'
		JOIN evaluation_side_specs candidate_side
		  ON candidate_side.id=binding.candidate_side_spec_id AND candidate_side.side='candidate'
		JOIN evaluation_samples baseline_sample ON baseline_sample.id=baseline_side.sample_id
		JOIN evaluation_samples candidate_sample ON candidate_sample.id=candidate_side.sample_id
		WHERE pair_spec.run_id=$1 AND case_spec.capability_domain=$2
		  AND baseline_sample.model_route='baseline:' || $3
		  AND candidate_sample.model_route='candidate:' || $3`,
		request.RunID, request.CapabilityDomain, request.ModelRoute).Scan(&expected); err != nil {
		return nil, fmt.Errorf("count expected cell pairs: %w", err)
	}
	if expected == 0 {
		return nil, service.ErrAggregatePairsIncomplete
	}

	inputs, err := loadEligibleCellPairInputs(ctx, tx, request)
	if err != nil {
		return nil, err
	}
	if len(inputs) != expected {
		return nil, service.ErrAggregatePairsIncomplete
	}
	inputHash, err := service.CanonicalCellInputSetHash(inputs)
	if err != nil {
		return nil, err
	}
	if existing, err := loadCellAnalysisJobByHash(ctx, tx, request, inputHash); err != nil {
		return nil, err
	} else if existing != nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit existing cell analysis job: %w", err)
		}
		return existing, nil
	}

	job, err := insertCellAnalysisJob(ctx, tx, request, inputs, inputHash)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit cell analysis job: %w", err)
	}
	return job, nil
}

func (r *evaluationAggregateRepository) EnsureGlobalAnalysisJob(ctx context.Context, request service.GlobalAnalysisJobRequest) (*service.AnalysisJobRevision, error) {
	request.AnalysisVersion = strings.TrimSpace(request.AnalysisVersion)
	if r == nil || r.db == nil || request.RunID == uuid.Nil || request.AnalysisVersion == "" {
		return nil, service.ErrAggregateRevisionInvalid
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "api")
	if err != nil {
		return nil, fmt.Errorf("begin global analysis job: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockAggregateRun(ctx, tx, request.RunID); err != nil {
		return nil, err
	}
	inputs, required, err := loadCurrentGlobalInputs(ctx, tx, request)
	if err != nil {
		return nil, err
	}
	if !required {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit global not required: %w", err)
		}
		return nil, nil
	}
	inputHash, err := service.CanonicalGlobalInputSetHash(inputs)
	if err != nil {
		return nil, err
	}
	if existing, err := loadGlobalAnalysisJobByHash(ctx, tx, request, inputHash); err != nil {
		return nil, err
	} else if existing != nil {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit existing global analysis job: %w", err)
		}
		return existing, nil
	}
	job, err := insertGlobalAnalysisJob(ctx, tx, request, inputs, inputHash)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit global analysis job: %w", err)
	}
	return job, nil
}

func lockAggregateRun(ctx context.Context, tx *sql.Tx, runID uuid.UUID) (service.RunStatus, error) {
	var status service.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM evaluation_runs WHERE id=$1 FOR UPDATE`, runID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", service.ErrAggregateRevisionInvalid
		}
		return "", fmt.Errorf("lock aggregate run: %w", err)
	}
	return status, nil
}

func loadCurrentGlobalInputs(ctx context.Context, tx *sql.Tx, request service.GlobalAnalysisJobRequest) ([]service.GlobalCellInput, bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT case_spec.capability_domain, baseline_sample.model_route
		FROM evaluation_pair_specs pair_spec
		JOIN evaluation_cases case_spec ON case_spec.id=pair_spec.case_id
		JOIN evaluation_pair_bindings binding ON binding.pair_spec_id=pair_spec.id
		JOIN evaluation_side_specs baseline_side
		  ON baseline_side.id=binding.baseline_side_spec_id AND baseline_side.side='baseline'
		JOIN evaluation_samples baseline_sample ON baseline_sample.id=baseline_side.sample_id
		WHERE pair_spec.run_id=$1
		ORDER BY case_spec.capability_domain, baseline_sample.model_route`, request.RunID)
	if err != nil {
		return nil, false, fmt.Errorf("load expected global cells: %w", err)
	}
	type cellKey struct {
		domain, route string
	}
	keys := make([]cellKey, 0)
	for rows.Next() {
		var key cellKey
		if err := rows.Scan(&key.domain, &key.route); err != nil {
			rows.Close()
			return nil, false, fmt.Errorf("scan expected global cell: %w", err)
		}
		key.route = service.CanonicalModelRoute(key.route)
		keys = append(keys, key)
	}
	if err := rows.Close(); err != nil {
		return nil, false, fmt.Errorf("close expected global cells: %w", err)
	}
	if len(keys) <= 1 {
		return nil, false, nil
	}
	inputs := make([]service.GlobalCellInput, 0, len(keys))
	for _, key := range keys {
		var input service.GlobalCellInput
		var batchID uuid.NullUUID
		err := tx.QueryRowContext(ctx, `
			SELECT capability_domain, canonical_model_route, snapshot_id, window_start,
			       aggregate_revision, input_set_hash, aggregate_hash, revision_batch_id
			FROM evaluation_aggregate_heads
			WHERE run_id=$1 AND capability_domain=$2 AND canonical_model_route=$3
			  AND analysis_version=$4`, request.RunID, key.domain, key.route, request.AnalysisVersion).Scan(
			&input.CapabilityDomain, &input.CanonicalModelRoute, &input.Snapshot.ID,
			&input.Snapshot.WindowStart, &input.AggregateRevision, &input.InputSetHash,
			&input.AggregateHash, &batchID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, true, service.ErrAggregatePairsIncomplete
		}
		if err != nil {
			return nil, true, fmt.Errorf("load current global cell head: %w", err)
		}
		if batchID.Valid {
			input.RevisionBatchID = batchID.UUID
		}
		inputs = append(inputs, input)
	}
	return inputs, true, nil
}

func insertGlobalAnalysisJob(ctx context.Context, tx *sql.Tx, request service.GlobalAnalysisJobRequest, inputs []service.GlobalCellInput, inputHash string) (*service.AnalysisJobRevision, error) {
	snapshotRefs := make([]service.SnapshotRef, 0, len(inputs))
	batchIDs := make(map[uuid.UUID]struct{})
	for _, input := range inputs {
		snapshotRefs = append(snapshotRefs, input.Snapshot)
		if input.RevisionBatchID != uuid.Nil {
			batchIDs[input.RevisionBatchID] = struct{}{}
		}
	}
	if len(batchIDs) > 1 {
		return nil, service.ErrAggregateRevisionInvalid
	}
	workOrigin := "initial"
	var revisionBatchID uuid.UUID
	for id := range batchIDs {
		workOrigin = "regrade"
		revisionBatchID = id
	}
	refsJSON, err := json.Marshal(snapshotRefs)
	if err != nil {
		return nil, fmt.Errorf("marshal global snapshot refs: %w", err)
	}
	causeSetHash, err := service.DigestCanonicalJSON(refsJSON)
	if err != nil {
		return nil, fmt.Errorf("hash global cause set: %w", err)
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(aggregate_revision),0)+1
		FROM evaluation_analysis_jobs
		WHERE run_id=$1 AND scope='global' AND analysis_version=$2`,
		request.RunID, request.AnalysisVersion).Scan(&revision); err != nil {
		return nil, fmt.Errorf("allocate global aggregate revision: %w", err)
	}
	job := &service.AnalysisJobRevision{
		ID: uuid.New(), RunID: request.RunID, Scope: "global", CapabilityDomain: "global",
		CanonicalModelRoute: "global", AnalysisVersion: request.AnalysisVersion,
		InputSetHash: inputHash, SnapshotRefs: snapshotRefs, AggregateRevision: revision,
		WorkOrigin: workOrigin, RevisionBatchID: revisionBatchID, CauseSetHash: causeSetHash,
	}
	var batchValue any
	if revisionBatchID != uuid.Nil {
		batchValue = revisionBatchID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_analysis_jobs (
			id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, status, scope, work_origin, revision_batch_id, input_set_hash,
			input_snapshot_refs, aggregate_revision, cause_set_hash
		) VALUES ($1,$2,'global','global','revision',$3,transaction_timestamp(),
			'pending','global',$4,$5,$6,$7::jsonb,$8,$9)`,
		job.ID, job.RunID, job.AnalysisVersion, job.WorkOrigin, batchValue,
		job.InputSetHash, string(refsJSON), job.AggregateRevision, job.CauseSetHash); err != nil {
		return nil, fmt.Errorf("insert global analysis job: %w", err)
	}
	for ordinal, ref := range snapshotRefs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_analysis_job_snapshot_inputs (
				analysis_job_id, input_ordinal, snapshot_id, window_start
			) VALUES ($1,$2,$3,$4)`, job.ID, ordinal, ref.ID, ref.WindowStart); err != nil {
			return nil, fmt.Errorf("insert global analysis snapshot input: %w", err)
		}
	}
	return job, nil
}

func loadGlobalAnalysisJobByHash(ctx context.Context, tx *sql.Tx, request service.GlobalAnalysisJobRequest, inputHash string) (*service.AnalysisJobRevision, error) {
	job := &service.AnalysisJobRevision{}
	var refsJSON json.RawMessage
	var batchID uuid.NullUUID
	err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, scope, capability_domain, model_route, analysis_version,
		       input_set_hash, input_snapshot_refs, aggregate_revision, work_origin,
		       revision_batch_id, cause_set_hash
		FROM evaluation_analysis_jobs
		WHERE run_id=$1 AND analysis_version=$2 AND input_set_hash=$3 AND scope='global'`,
		request.RunID, request.AnalysisVersion, inputHash).Scan(
		&job.ID, &job.RunID, &job.Scope, &job.CapabilityDomain, &job.CanonicalModelRoute,
		&job.AnalysisVersion, &job.InputSetHash, &refsJSON, &job.AggregateRevision,
		&job.WorkOrigin, &batchID, &job.CauseSetHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load global analysis job: %w", err)
	}
	if batchID.Valid {
		job.RevisionBatchID = batchID.UUID
	}
	if err := json.Unmarshal(refsJSON, &job.SnapshotRefs); err != nil {
		return nil, fmt.Errorf("decode global analysis snapshot refs: %w", err)
	}
	return job, nil
}

func loadEligibleCellPairInputs(ctx context.Context, tx *sql.Tx, request service.CellAnalysisJobRequest) ([]service.CellPairInput, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT pair_spec.case_id, pair_spec.sample_index, pair_spec.pair_spec_hash,
		       baseline_side.side_spec_hash, candidate_side.side_spec_hash, binding.pair_binding_hash,
		       case_spec.grader_id, case_spec.grader_version,
		       baseline_head.version, baseline_head.score_id, baseline_head.score_created_at,
		       baseline_score.source_assignment_id, baseline_score.route_evidence_set_hash,
		       candidate_head.version, candidate_head.score_id, candidate_head.score_created_at,
		       candidate_score.source_assignment_id, candidate_score.route_evidence_set_hash,
		       case_spec.weight, baseline_event.id, candidate_event.id,
		       baseline_event.revision_batch_id, candidate_event.revision_batch_id
		FROM evaluation_pair_specs pair_spec
		JOIN evaluation_cases case_spec ON case_spec.id=pair_spec.case_id
		JOIN evaluation_pair_bindings binding ON binding.pair_spec_id=pair_spec.id
		JOIN evaluation_side_specs baseline_side
		  ON baseline_side.id=binding.baseline_side_spec_id AND baseline_side.side='baseline'
		JOIN evaluation_side_specs candidate_side
		  ON candidate_side.id=binding.candidate_side_spec_id AND candidate_side.side='candidate'
		JOIN evaluation_samples baseline_sample ON baseline_sample.id=baseline_side.sample_id
		JOIN evaluation_samples candidate_sample ON candidate_sample.id=candidate_side.sample_id
		JOIN evaluation_score_heads baseline_head
		  ON baseline_head.sample_id=baseline_sample.id AND baseline_head.grader_id=case_spec.grader_id
		JOIN evaluation_score_heads candidate_head
		  ON candidate_head.sample_id=candidate_sample.id AND candidate_head.grader_id=case_spec.grader_id
		JOIN evaluation_scores baseline_score
		  ON baseline_score.id=baseline_head.score_id AND baseline_score.created_at=baseline_head.score_created_at
		JOIN evaluation_scores candidate_score
		  ON candidate_score.id=candidate_head.score_id AND candidate_score.created_at=candidate_head.score_created_at
		JOIN evaluation_assignments baseline_assignment ON baseline_assignment.id=baseline_score.source_assignment_id
		JOIN evaluation_assignments candidate_assignment ON candidate_assignment.id=candidate_score.source_assignment_id
		JOIN evaluation_score_head_events baseline_event
		  ON baseline_event.sample_id=baseline_sample.id AND baseline_event.grader_id=case_spec.grader_id
		 AND baseline_event.version=baseline_head.version
		JOIN evaluation_score_head_events candidate_event
		  ON candidate_event.sample_id=candidate_sample.id AND candidate_event.grader_id=case_spec.grader_id
		 AND candidate_event.version=candidate_head.version
		WHERE pair_spec.run_id=$1 AND case_spec.capability_domain=$2
		  AND baseline_sample.model_route='baseline:' || $3
		  AND candidate_sample.model_route='candidate:' || $3
		  AND baseline_assignment.attempt=(
			SELECT MAX(current_assignment.attempt) FROM evaluation_assignments current_assignment
			WHERE current_assignment.sample_id=baseline_sample.id
		  )
		  AND candidate_assignment.attempt=(
			SELECT MAX(current_assignment.attempt) FROM evaluation_assignments current_assignment
			WHERE current_assignment.sample_id=candidate_sample.id
		  )
		  AND EXISTS (
			SELECT 1 FROM evaluation_route_evidence evidence
			WHERE evidence.assignment_id=baseline_assignment.id
			  AND evidence.sample_id=baseline_sample.id AND evidence.sealed_at IS NOT NULL
		  )
		  AND EXISTS (
			SELECT 1 FROM evaluation_route_evidence evidence
			WHERE evidence.assignment_id=candidate_assignment.id
			  AND evidence.sample_id=candidate_sample.id AND evidence.sealed_at IS NOT NULL
		  )
		ORDER BY pair_spec.case_id, pair_spec.sample_index`, request.RunID, request.CapabilityDomain, request.ModelRoute)
	if err != nil {
		return nil, fmt.Errorf("load eligible cell pairs: %w", err)
	}
	defer rows.Close()
	inputs := make([]service.CellPairInput, 0)
	for rows.Next() {
		var input service.CellPairInput
		var baselineBatch, candidateBatch uuid.NullUUID
		if err := rows.Scan(
			&input.CaseID, &input.SampleIndex, &input.PairSpecHash,
			&input.BaselineSideSpecHash, &input.CandidateSideSpecHash, &input.PairBindingHash,
			&input.GraderID, &input.GraderVersion,
			&input.BaselineHeadVersion, &input.BaselineScore.ID, &input.BaselineScore.CreatedAt,
			&input.BaselineSourceAssignment, &input.BaselineEvidenceSetHash,
			&input.CandidateHeadVersion, &input.CandidateScore.ID, &input.CandidateScore.CreatedAt,
			&input.CandidateSourceAssignment, &input.CandidateEvidenceSetHash,
			&input.CaseWeight, &input.BaselineSourceHeadEventID, &input.CandidateSourceHeadEventID,
			&baselineBatch, &candidateBatch,
		); err != nil {
			return nil, fmt.Errorf("scan eligible cell pair: %w", err)
		}
		if baselineBatch.Valid {
			input.BaselineRevisionBatchID = baselineBatch.UUID
		}
		if candidateBatch.Valid {
			input.CandidateRevisionBatchID = candidateBatch.UUID
		}
		inputs = append(inputs, input)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate eligible cell pairs: %w", err)
	}
	return inputs, nil
}

func insertCellAnalysisJob(ctx context.Context, tx *sql.Tx, request service.CellAnalysisJobRequest, inputs []service.CellPairInput, inputHash string) (*service.AnalysisJobRevision, error) {
	scoreRefs := make([]service.ScoreRef, 0, len(inputs)*2)
	frozenRefs := make([]frozenAnalysisScoreInput, 0, len(inputs)*2)
	headEventIDs := make([]string, 0, len(inputs)*2)
	batchIDs := make(map[uuid.UUID]struct{})
	for _, input := range inputs {
		scoreRefs = append(scoreRefs, input.BaselineScore, input.CandidateScore)
		frozenRefs = append(frozenRefs,
			frozenAnalysisScoreInput{ScoreRef: input.BaselineScore, HeadEventID: input.BaselineSourceHeadEventID, RevisionBatchID: input.BaselineRevisionBatchID},
			frozenAnalysisScoreInput{ScoreRef: input.CandidateScore, HeadEventID: input.CandidateSourceHeadEventID, RevisionBatchID: input.CandidateRevisionBatchID},
		)
		headEventIDs = append(headEventIDs, input.BaselineSourceHeadEventID.String(), input.CandidateSourceHeadEventID.String())
		if input.BaselineRevisionBatchID != uuid.Nil {
			batchIDs[input.BaselineRevisionBatchID] = struct{}{}
		}
		if input.CandidateRevisionBatchID != uuid.Nil {
			batchIDs[input.CandidateRevisionBatchID] = struct{}{}
		}
	}
	if len(batchIDs) > 1 {
		return nil, service.ErrAggregateRevisionInvalid
	}
	workOrigin := "initial"
	var revisionBatchID uuid.UUID
	for id := range batchIDs {
		workOrigin = "regrade"
		revisionBatchID = id
	}
	sort.Strings(headEventIDs)
	causeJSON, err := json.Marshal(headEventIDs)
	if err != nil {
		return nil, fmt.Errorf("marshal cell cause set: %w", err)
	}
	causeSetHash, err := service.DigestCanonicalJSON(causeJSON)
	if err != nil {
		return nil, fmt.Errorf("hash cell cause set: %w", err)
	}
	refsJSON, err := json.Marshal(frozenRefs)
	if err != nil {
		return nil, fmt.Errorf("marshal cell score refs: %w", err)
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(aggregate_revision), 0) + 1
		FROM evaluation_analysis_jobs
		WHERE run_id=$1 AND capability_domain=$2 AND model_route=$3
		  AND analysis_version=$4 AND scope='cell'`, request.RunID, request.CapabilityDomain,
		request.ModelRoute, request.AnalysisVersion).Scan(&revision); err != nil {
		return nil, fmt.Errorf("allocate cell aggregate revision: %w", err)
	}
	job := &service.AnalysisJobRevision{
		ID: uuid.New(), RunID: request.RunID, Scope: "cell", CapabilityDomain: request.CapabilityDomain,
		CanonicalModelRoute: request.ModelRoute, AnalysisVersion: request.AnalysisVersion,
		InputSetHash: inputHash, ScoreRefs: scoreRefs, AggregateRevision: revision,
		WorkOrigin: workOrigin, RevisionBatchID: revisionBatchID, CauseSetHash: causeSetHash,
	}
	var batchValue any
	if revisionBatchID != uuid.Nil {
		batchValue = revisionBatchID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_analysis_jobs (
			id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, status, scope, work_origin, revision_batch_id, input_set_hash,
			input_score_refs, aggregate_revision, cause_set_hash
		) VALUES ($1,$2,$3,$4,'revision',$5,transaction_timestamp(),'pending','cell',$6,$7,$8,$9::jsonb,$10,$11)`,
		job.ID, job.RunID, job.CapabilityDomain, job.CanonicalModelRoute, job.AnalysisVersion,
		job.WorkOrigin, batchValue, job.InputSetHash, string(refsJSON), job.AggregateRevision,
		job.CauseSetHash); err != nil {
		return nil, fmt.Errorf("insert cell analysis job: %w", err)
	}
	for ordinal, ref := range scoreRefs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_analysis_job_score_inputs (
				analysis_job_id, input_ordinal, score_id, score_created_at
			) VALUES ($1,$2,$3,$4)`, job.ID, ordinal, ref.ID, ref.CreatedAt); err != nil {
			return nil, fmt.Errorf("insert cell analysis score input: %w", err)
		}
	}
	return job, nil
}

func loadCellAnalysisJobByHash(ctx context.Context, tx *sql.Tx, request service.CellAnalysisJobRequest, inputHash string) (*service.AnalysisJobRevision, error) {
	job := &service.AnalysisJobRevision{}
	var refsJSON json.RawMessage
	var batchID uuid.NullUUID
	err := tx.QueryRowContext(ctx, `
		SELECT id, run_id, scope, capability_domain, model_route, analysis_version,
		       input_set_hash, input_score_refs, aggregate_revision, work_origin,
		       revision_batch_id, cause_set_hash
		FROM evaluation_analysis_jobs
		WHERE run_id=$1 AND capability_domain=$2 AND model_route=$3
		  AND analysis_version=$4 AND input_set_hash=$5 AND scope='cell'`,
		request.RunID, request.CapabilityDomain, request.ModelRoute, request.AnalysisVersion, inputHash).Scan(
		&job.ID, &job.RunID, &job.Scope, &job.CapabilityDomain, &job.CanonicalModelRoute,
		&job.AnalysisVersion, &job.InputSetHash, &refsJSON, &job.AggregateRevision,
		&job.WorkOrigin, &batchID, &job.CauseSetHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load cell analysis job: %w", err)
	}
	if batchID.Valid {
		job.RevisionBatchID = batchID.UUID
	}
	if err := json.Unmarshal(refsJSON, &job.ScoreRefs); err != nil {
		return nil, fmt.Errorf("decode cell analysis score refs: %w", err)
	}
	return job, nil
}

func (r *evaluationGradingRepository) completeRevisionAnalysisJob(ctx context.Context, jobID uuid.UUID, leaseToken string, submission service.AggregateSubmission) (*service.AggregateSnapshot, error) {
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin revision analysis completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var job struct {
		runID, snapshotID                                 uuid.UUID
		domain, route, window, version, scope, workOrigin string
		windowStart                                       time.Time
		status                                            string
		leaseHash                                         sql.NullString
		leaseExpires                                      sql.NullTime
		leasedBy                                          sql.NullString
		workerStatus                                      sql.NullString
		leaseEpoch                                        sql.NullInt64
		revisionBatchID                                   uuid.NullUUID
		inputSetHash, causeSetHash                        string
		aggregateRevision                                 int64
		frozenScoreJSON                                   json.RawMessage
		frozenSnapshotJSON                                json.RawMessage
	}
	err = tx.QueryRowContext(ctx, `
		SELECT job.run_id, job.capability_domain, job.model_route, job."window", job.analysis_version,
		       job.window_start, job.status, job.lease_token_hash, job.lease_expires_at, job.leased_by,
		       w.status, job.lease_epoch, COALESCE(job.snapshot_id,'00000000-0000-0000-0000-000000000000'),
		       job.scope, job.work_origin, job.revision_batch_id, job.input_set_hash, job.aggregate_revision,
		       job.cause_set_hash, job.input_score_refs, job.input_snapshot_refs
		FROM evaluation_analysis_jobs job
		LEFT JOIN evaluation_workers w ON w.id = job.leased_by
		WHERE job.id=$1 FOR UPDATE`, jobID).Scan(
		&job.runID, &job.domain, &job.route, &job.window, &job.version, &job.windowStart,
		&job.status, &job.leaseHash, &job.leaseExpires, &job.leasedBy, &job.workerStatus, &job.leaseEpoch,
		&job.snapshotID, &job.scope, &job.workOrigin, &job.revisionBatchID, &job.inputSetHash,
		&job.aggregateRevision, &job.causeSetHash, &job.frozenScoreJSON, &job.frozenSnapshotJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAnalysisJobFenced
	}
	if err != nil {
		return nil, fmt.Errorf("load revision analysis job: %w", err)
	}
	if job.status == "completed" && job.snapshotID != uuid.Nil {
		return loadRevisionAggregateSnapshot(ctx, tx, job.snapshotID, job.windowStart)
	}
	if job.status != "leased" || job.leaseHash.String != hashToken(leaseToken) ||
		!job.leaseExpires.Valid || !job.leaseExpires.Time.After(time.Now()) ||
		!job.workerStatus.Valid || job.workerStatus.String != "active" ||
		!job.leaseEpoch.Valid || submission.LeaseEpoch != job.leaseEpoch.Int64 {
		return nil, service.ErrAnalysisJobFenced
	}
	if job.revisionBatchID.Valid {
		var status service.RevisionBatchStatus
		var epoch int64
		if err := tx.QueryRowContext(ctx, `
			SELECT status, control_epoch FROM evaluation_revision_batches
			WHERE id=$1 AND run_id=$2 FOR UPDATE`, job.revisionBatchID.UUID, job.runID).Scan(&status, &epoch); err != nil ||
			status != service.RevisionBatchRunning || epoch != job.leaseEpoch.Int64 {
			return nil, service.ErrAnalysisJobFenced
		}
	} else {
		var epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT control_epoch FROM evaluation_runs WHERE id=$1 FOR UPDATE`, job.runID).Scan(&epoch); err != nil || epoch != job.leaseEpoch.Int64 {
			return nil, service.ErrAnalysisJobFenced
		}
	}
	if submission.RunID != uuid.Nil && submission.RunID != job.runID {
		return nil, service.ErrAggregateRunMismatch
	}
	if submission.InputSetHash != job.inputSetHash {
		return nil, service.ErrAggregateInputMismatch
	}
	var frozenScores []frozenAnalysisScoreInput
	if err := json.Unmarshal(job.frozenScoreJSON, &frozenScores); err != nil {
		return nil, fmt.Errorf("decode frozen analysis scores: %w", err)
	}
	var frozenSnapshots []service.SnapshotRef
	if err := json.Unmarshal(job.frozenSnapshotJSON, &frozenSnapshots); err != nil {
		return nil, fmt.Errorf("decode frozen analysis snapshots: %w", err)
	}
	expectedScores := make([]service.ScoreRef, 0, len(frozenScores))
	for _, input := range frozenScores {
		expectedScores = append(expectedScores, input.ScoreRef)
	}
	if !sameScoreRefSet(expectedScores, submission.ScoreRefs) || !sameSnapshotRefSet(frozenSnapshots, submission.SnapshotRefs) {
		return nil, service.ErrAggregateInputMismatch
	}
	if len(submission.Aggregate) == 0 || !json.Valid(submission.Aggregate) {
		return nil, service.ErrScoreSubmissionInvalid
	}
	aggregateHash, err := service.DigestCanonicalJSON(submission.Aggregate)
	if err != nil {
		return nil, service.ErrScoreSubmissionInvalid
	}
	snapshotID := uuid.New()
	scoreIDs := make([]string, 0, len(expectedScores))
	for _, ref := range expectedScores {
		scoreIDs = append(scoreIDs, ref.ID.String())
	}
	scoreJSON, err := json.Marshal(expectedScores)
	if err != nil {
		return nil, fmt.Errorf("marshal aggregate score refs: %w", err)
	}
	headEventIDs := make([]string, 0, len(frozenScores))
	originBatchSet := make(map[string]struct{})
	for _, input := range frozenScores {
		if input.HeadEventID != uuid.Nil {
			headEventIDs = append(headEventIDs, input.HeadEventID.String())
		}
		if input.RevisionBatchID != uuid.Nil {
			originBatchSet[input.RevisionBatchID.String()] = struct{}{}
		}
	}
	for _, ref := range frozenSnapshots {
		var sourceEvents, sourceBatches pq.StringArray
		if err := tx.QueryRowContext(ctx, `
			SELECT source_head_event_ids, origin_revision_batch_ids
			FROM evaluation_aggregate_snapshots
			WHERE id=$1 AND window_start=$2`, ref.ID, ref.WindowStart).Scan(
			&sourceEvents, &sourceBatches); err != nil {
			return nil, fmt.Errorf("load global aggregate lineage: %w", err)
		}
		headEventIDs = append(headEventIDs, sourceEvents...)
		for _, id := range sourceBatches {
			originBatchSet[id] = struct{}{}
		}
	}
	headEventIDs = uniqueSortedStrings(headEventIDs)
	originBatchIDs := make([]string, 0, len(originBatchSet))
	for id := range originBatchSet {
		originBatchIDs = append(originBatchIDs, id)
	}
	sort.Strings(originBatchIDs)
	var batchValue any
	if job.revisionBatchID.Valid {
		batchValue = job.revisionBatchID.UUID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_aggregate_snapshots (
			id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, score_ids, aggregate, analysis_job_id, revision_batch_id,
			input_set_hash, aggregate_revision, aggregate_hash, score_refs,
			source_head_event_ids, origin_revision_batch_ids, cause_set_hash
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::jsonb,$10,$11,$12,$13,$14,$15::jsonb,$16,$17,$18)`,
		snapshotID, job.runID, job.domain, job.route, job.window, job.version,
		job.windowStart, pq.Array(scoreIDs), string(submission.Aggregate), jobID, batchValue,
		job.inputSetHash, job.aggregateRevision, aggregateHash, string(scoreJSON),
		pq.Array(headEventIDs), pq.Array(originBatchIDs), job.causeSetHash); err != nil {
		return nil, fmt.Errorf("insert revision aggregate snapshot: %w", err)
	}
	for ordinal, ref := range expectedScores {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_aggregate_snapshot_score_inputs (
				snapshot_id, snapshot_window_start, input_ordinal, score_id, score_created_at
			) VALUES ($1,$2,$3,$4,$5)`, snapshotID, job.windowStart, ordinal, ref.ID, ref.CreatedAt); err != nil {
			return nil, fmt.Errorf("insert aggregate snapshot score input: %w", err)
		}
	}
	for ordinal, ref := range frozenSnapshots {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_aggregate_snapshot_sources (
				snapshot_id, snapshot_window_start, source_ordinal, source_snapshot_id, source_window_start
			) VALUES ($1,$2,$3,$4,$5)`, snapshotID, job.windowStart, ordinal, ref.ID, ref.WindowStart); err != nil {
			return nil, fmt.Errorf("insert aggregate snapshot source: %w", err)
		}
	}
	headAdvanced, err := currentAnalysisInputMatches(ctx, tx, job.runID, job.scope, job.domain, job.route, job.version, job.inputSetHash)
	if err != nil {
		return nil, err
	}
	if headAdvanced {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_aggregate_heads (
				run_id, capability_domain, canonical_model_route, analysis_version,
				snapshot_id, window_start, aggregate_revision, input_set_hash,
				aggregate_hash, revision_batch_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (run_id, capability_domain, canonical_model_route, analysis_version)
			DO UPDATE SET snapshot_id=EXCLUDED.snapshot_id, window_start=EXCLUDED.window_start,
				aggregate_revision=EXCLUDED.aggregate_revision, input_set_hash=EXCLUDED.input_set_hash,
				aggregate_hash=EXCLUDED.aggregate_hash, revision_batch_id=EXCLUDED.revision_batch_id,
				updated_at=transaction_timestamp()
				WHERE evaluation_aggregate_heads.aggregate_revision < EXCLUDED.aggregate_revision`,
			job.runID, job.domain, job.route, job.version, snapshotID, job.windowStart,
			job.aggregateRevision, job.inputSetHash, aggregateHash, batchValue)
		if err != nil {
			return nil, fmt.Errorf("advance aggregate head: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("read aggregate head advance result: %w", err)
		}
		headAdvanced = affected == 1
		if headAdvanced {
			if err := enqueueAggregateHeadProgress(
				ctx, tx, job.runID, job.scope, job.domain, job.route, job.version,
				snapshotID, aggregateHash, job.workOrigin, job.revisionBatchID, headEventIDs,
				frozenSnapshots,
			); err != nil {
				return nil, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_analysis_jobs
		SET status='completed', snapshot_id=$2, lease_token_hash=NULL, leased_by=NULL,
			lease_expires_at=NULL, finished_at=NOW(), updated_at=NOW()
		WHERE id=$1`, jobID, snapshotID); err != nil {
		return nil, fmt.Errorf("complete revision analysis job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit revision analysis completion: %w", err)
	}
	snapshot, err := loadRevisionAggregateSnapshot(ctx, r.db, snapshotID, job.windowStart)
	if err != nil {
		return nil, err
	}
	snapshot.HeadAdvanced = headAdvanced
	return snapshot, nil
}

func enqueueAggregateHeadProgress(
	ctx context.Context,
	tx *sql.Tx,
	runID uuid.UUID,
	scope, domain, route, analysisVersion string,
	snapshotID uuid.UUID,
	aggregateHash, workOrigin string,
	revisionBatchID uuid.NullUUID,
	sourceHeadEventIDs []string,
	sourceSnapshots []service.SnapshotRef,
) error {
	eventType := "global_recompute"
	scopeKey := domain + "/" + route
	sourceType := "score_head_event"
	sourceIDs := append([]string(nil), sourceHeadEventIDs...)
	attachHeadEvent := true
	if scope == "global" {
		eventType = "gate_reevaluation"
		scopeKey = "global/global"
		sourceType = "aggregate_head"
		sourceIDs = make([]string, 0, len(sourceSnapshots))
		for _, ref := range sourceSnapshots {
			sourceIDs = append(sourceIDs, ref.ID.String())
		}
		attachHeadEvent = false
	}
	causes, err := loadAggregateProgressCauses(ctx, tx, runID, sourceType, sourceIDs, attachHeadEvent)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		SnapshotID       uuid.UUID `json:"snapshot_id"`
		CapabilityDomain string    `json:"capability_domain"`
		ModelRoute       string    `json:"model_route"`
		AnalysisVersion  string    `json:"analysis_version"`
	}{snapshotID, domain, route, analysisVersion})
	if err != nil {
		return fmt.Errorf("marshal aggregate head outbox payload: %w", err)
	}
	input := service.EnqueueEvaluationOutboxInput{
		EventType: eventType, RunID: runID, ScopeKey: scopeKey, AnalysisVersion: analysisVersion,
		SourceType: "aggregate_head", SourceID: snapshotID.String(),
		SourceHash: hashString("aggregate-head\x00" + snapshotID.String() + "\x00" + aggregateHash),
		Payload:    payload, WorkOrigin: workOrigin, Causes: causes,
	}
	if revisionBatchID.Valid {
		input.RevisionBatchID = revisionBatchID.UUID
	}
	if _, err := enqueueEvaluationOutbox(ctx, tx, input); err != nil {
		return fmt.Errorf("enqueue aggregate head progress: %w", err)
	}
	return nil
}

func loadAggregateProgressCauses(
	ctx context.Context,
	tx *sql.Tx,
	runID uuid.UUID,
	sourceType string,
	sourceIDs []string,
	attachHeadEvent bool,
) ([]service.EvaluationOutboxCause, error) {
	sourceIDs = uniqueSortedStrings(sourceIDs)
	if len(sourceIDs) == 0 {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, source_id FROM evaluation_outbox_events
		WHERE run_id=$1 AND source_type=$2 AND source_id=ANY($3::text[])
		ORDER BY source_id`, runID, sourceType, pq.Array(sourceIDs))
	if err != nil {
		return nil, fmt.Errorf("load aggregate progress causes: %w", err)
	}
	defer rows.Close()
	bySource := make(map[string]service.EvaluationOutboxCause, len(sourceIDs))
	for rows.Next() {
		var cause service.EvaluationOutboxCause
		var sourceID string
		if err := rows.Scan(&cause.EventID, &sourceID); err != nil {
			return nil, fmt.Errorf("scan aggregate progress cause: %w", err)
		}
		if attachHeadEvent {
			parsed, err := uuid.Parse(sourceID)
			if err != nil {
				return nil, service.ErrEvaluationOutboxInvalid
			}
			cause.SourceHeadEventID = parsed
		}
		if _, exists := bySource[sourceID]; exists {
			return nil, service.ErrEvaluationOutboxDedupConflict
		}
		bySource[sourceID] = cause
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate aggregate progress causes: %w", err)
	}
	if len(bySource) != len(sourceIDs) {
		return nil, service.ErrEvaluationOutboxInvalid
	}
	causes := make([]service.EvaluationOutboxCause, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		causes = append(causes, bySource[sourceID])
	}
	return causes, nil
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func loadFrozenAnalysisPairs(ctx context.Context, tx *sql.Tx, jobID uuid.UUID, canonicalRoute string) ([]service.PairedScore, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT input.input_ordinal, score.id, score.created_at, score.score,
		       sample.case_id, sample.sample_index, sample.model_route, case_spec.weight
		FROM evaluation_analysis_job_score_inputs input
		JOIN evaluation_scores score
		  ON score.id=input.score_id AND score.created_at=input.score_created_at
		JOIN evaluation_samples sample ON sample.id=score.sample_id
		JOIN evaluation_cases case_spec ON case_spec.id=sample.case_id
		WHERE input.analysis_job_id=$1 ORDER BY input.input_ordinal`, jobID)
	if err != nil {
		return nil, fmt.Errorf("load frozen analysis pairs: %w", err)
	}
	defer rows.Close()
	type side struct {
		ordinal     int
		ref         service.ScoreRef
		score       decimal.Decimal
		caseID      uuid.UUID
		sampleIndex int
		modelRoute  string
		weight      decimal.Decimal
	}
	sides := make([]side, 0)
	for rows.Next() {
		var item side
		if err := rows.Scan(&item.ordinal, &item.ref.ID, &item.ref.CreatedAt, &item.score,
			&item.caseID, &item.sampleIndex, &item.modelRoute, &item.weight); err != nil {
			return nil, fmt.Errorf("scan frozen analysis pair: %w", err)
		}
		sides = append(sides, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate frozen analysis pairs: %w", err)
	}
	if len(sides)%2 != 0 {
		return nil, service.ErrAggregateInputMismatch
	}
	pairs := make([]service.PairedScore, 0, len(sides)/2)
	for index := 0; index < len(sides); index += 2 {
		baseline, candidate := sides[index], sides[index+1]
		if baseline.caseID != candidate.caseID || baseline.sampleIndex != candidate.sampleIndex ||
			service.CanonicalModelRoute(baseline.modelRoute) != service.CanonicalModelRoute(candidate.modelRoute) ||
			!strings.HasPrefix(baseline.modelRoute, "baseline:") || !strings.HasPrefix(candidate.modelRoute, "candidate:") {
			return nil, service.ErrAggregateInputMismatch
		}
		pairs = append(pairs, service.PairedScore{
			CaseID: baseline.caseID, ModelRoute: canonicalRoute, SampleIndex: baseline.sampleIndex,
			Weight: baseline.weight, BaselineScore: baseline.score, CandidateScore: candidate.score,
		})
	}
	return pairs, nil
}

func currentAnalysisInputMatches(ctx context.Context, tx *sql.Tx, runID uuid.UUID, scope, domain, route, analysisVersion, expectedHash string) (bool, error) {
	if scope == "global" {
		inputs, required, err := loadCurrentGlobalInputs(ctx, tx, service.GlobalAnalysisJobRequest{
			RunID: runID, AnalysisVersion: analysisVersion,
		})
		if err != nil || !required {
			return false, err
		}
		currentHash, err := service.CanonicalGlobalInputSetHash(inputs)
		if err != nil {
			return false, err
		}
		return currentHash == expectedHash, nil
	}
	if scope != "cell" {
		return false, service.ErrAggregateRevisionInvalid
	}
	inputs, err := loadEligibleCellPairInputs(ctx, tx, service.CellAnalysisJobRequest{
		RunID: runID, CapabilityDomain: domain, ModelRoute: route, AnalysisVersion: analysisVersion,
	})
	if err != nil || len(inputs) == 0 {
		return false, err
	}
	currentHash, err := service.CanonicalCellInputSetHash(inputs)
	if err != nil {
		return false, err
	}
	return currentHash == expectedHash, nil
}

func sameScoreRefSet(expected, actual []service.ScoreRef) bool {
	if len(expected) != len(actual) {
		return false
	}
	left := append([]service.ScoreRef(nil), expected...)
	right := append([]service.ScoreRef(nil), actual...)
	sort.Slice(left, func(i, j int) bool { return scoreRefLess(left[i], left[j]) })
	sort.Slice(right, func(i, j int) bool { return scoreRefLess(right[i], right[j]) })
	for index := range left {
		if left[index].ID != right[index].ID || !left[index].CreatedAt.Equal(right[index].CreatedAt) {
			return false
		}
	}
	return true
}

func scoreRefLess(left, right service.ScoreRef) bool {
	if left.ID != right.ID {
		return left.ID.String() < right.ID.String()
	}
	return left.CreatedAt.Before(right.CreatedAt)
}

func sameSnapshotRefSet(expected, actual []service.SnapshotRef) bool {
	if len(expected) != len(actual) {
		return false
	}
	left := append([]service.SnapshotRef(nil), expected...)
	right := append([]service.SnapshotRef(nil), actual...)
	sort.Slice(left, func(i, j int) bool { return snapshotRefLess(left[i], left[j]) })
	sort.Slice(right, func(i, j int) bool { return snapshotRefLess(right[i], right[j]) })
	for index := range left {
		if left[index].ID != right[index].ID || !left[index].WindowStart.Equal(right[index].WindowStart) {
			return false
		}
	}
	return true
}

func snapshotRefLess(left, right service.SnapshotRef) bool {
	if left.ID != right.ID {
		return left.ID.String() < right.ID.String()
	}
	return left.WindowStart.Before(right.WindowStart)
}

func loadRevisionAggregateSnapshot(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uuid.UUID, windowStart time.Time) (*service.AggregateSnapshot, error) {
	var snapshot service.AggregateSnapshot
	var scoreIDs pq.StringArray
	var scoreJSON json.RawMessage
	if err := q.QueryRowContext(ctx, `
		SELECT id, run_id, capability_domain, model_route, "window", analysis_version,
		       window_start, score_ids, aggregate, created_at, input_set_hash,
		       aggregate_revision, aggregate_hash, score_refs
		FROM evaluation_aggregate_snapshots WHERE id=$1 AND window_start=$2`, id, windowStart).Scan(
		&snapshot.ID, &snapshot.RunID, &snapshot.CapabilityDomain, &snapshot.ModelRoute,
		&snapshot.Window, &snapshot.AnalysisVersion, &snapshot.WindowStart, &scoreIDs,
		&snapshot.Aggregate, &snapshot.CreatedAt, &snapshot.InputSetHash,
		&snapshot.AggregateRevision, &snapshot.AggregateHash, &scoreJSON); err != nil {
		return nil, fmt.Errorf("load revision aggregate snapshot: %w", err)
	}
	for _, raw := range scoreIDs {
		if parsed, err := uuid.Parse(raw); err == nil {
			snapshot.ScoreIDs = append(snapshot.ScoreIDs, parsed)
		}
	}
	if err := json.Unmarshal(scoreJSON, &snapshot.ScoreRefs); err != nil {
		return nil, fmt.Errorf("decode aggregate snapshot score refs: %w", err)
	}
	var sourceJSON json.RawMessage
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(jsonb_agg(jsonb_build_object(
			'snapshot_id', source_snapshot_id,
			'window_start', source_window_start
		) ORDER BY source_ordinal), '[]'::jsonb)
		FROM evaluation_aggregate_snapshot_sources
		WHERE snapshot_id=$1 AND snapshot_window_start=$2`, id, windowStart).Scan(&sourceJSON)
	if err != nil {
		return nil, fmt.Errorf("load aggregate snapshot sources: %w", err)
	}
	if err := json.Unmarshal(sourceJSON, &snapshot.SourceSnapshots); err != nil {
		return nil, fmt.Errorf("decode aggregate snapshot sources: %w", err)
	}
	snapshot.Ref = service.SnapshotRef{ID: snapshot.ID, WindowStart: snapshot.WindowStart}
	if err := q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM evaluation_aggregate_heads
			WHERE run_id=$1 AND capability_domain=$2 AND canonical_model_route=$3
			  AND analysis_version=$4 AND snapshot_id=$5 AND window_start=$6
		)`, snapshot.RunID, snapshot.CapabilityDomain, snapshot.ModelRoute,
		snapshot.AnalysisVersion, snapshot.ID, snapshot.WindowStart).Scan(&snapshot.HeadAdvanced); err != nil {
		return nil, fmt.Errorf("load aggregate snapshot head state: %w", err)
	}
	return &snapshot, nil
}
