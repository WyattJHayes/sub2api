package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

type evaluationGradingRepository struct {
	db *sql.DB
}

func NewEvaluationGradingRepository(db *sql.DB) service.EvaluationGradingRepository {
	return &evaluationGradingRepository{db: db}
}

func (r *evaluationGradingRepository) AuthenticateRunner(ctx context.Context, token string) (uuid.UUID, error) {
	return r.AuthenticateWorker(ctx, token, "runner")
}

func (r *evaluationGradingRepository) ClaimAssignment(ctx context.Context, workerID uuid.UUID, capabilities []string, leaseTTL time.Duration) (*service.AssignmentLease, error) {
	return (&evaluationRepository{db: r.db}).ClaimAssignment(ctx, workerID, capabilities, leaseTTL)
}

func (r *evaluationGradingRepository) RenewAssignmentLease(ctx context.Context, assignmentID uuid.UUID, leaseToken string, extendBy time.Duration, leaseEpoch ...int64) (time.Time, error) {
	return (&evaluationRepository{db: r.db}).RenewLease(ctx, assignmentID, leaseToken, extendBy, leaseEpoch...)
}

func (r *evaluationGradingRepository) SubmitEvidence(ctx context.Context, input service.EvidenceSubmission, leaseToken string) (*service.EvidenceReceipt, error) {
	if r == nil || r.db == nil || input.AssignmentID == uuid.Nil || input.SampleID == uuid.Nil || strings.TrimSpace(leaseToken) == "" || len(input.Evidence) == 0 {
		return nil, service.ErrLeaseFenced
	}
	digest := sha256.Sum256(bytes.TrimSpace(input.Evidence))
	digestHex := hex.EncodeToString(digest[:])
	var expectedEpoch any
	if input.LeaseEpoch > 0 {
		expectedEpoch = input.LeaseEpoch
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin evidence submission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sampleID uuid.UUID
	var leaseEpoch int64
	err = tx.QueryRowContext(ctx, `
		SELECT a.sample_id, a.lease_epoch
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_runs r ON r.id = s.run_id
		JOIN evaluation_workers w ON w.id = a.leased_by AND w.status = 'active'
		WHERE a.id = $1 AND a.sample_id = $2 AND a.lease_token_hash = $3 AND a.lease_expires_at > NOW()
		  AND a.status IN ('leased', 'running') AND a.lease_epoch = r.control_epoch
		  AND ($4::bigint IS NULL OR a.lease_epoch = $4)
		FOR UPDATE`, input.AssignmentID, input.SampleID, hashToken(leaseToken), expectedEpoch).Scan(&sampleID, &leaseEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrLeaseFenced
	}
	if err != nil {
		return nil, fmt.Errorf("lock assignment for evidence submission: %w", err)
	}
	var evidenceCount, unsealedCount, wrongEpochCount int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE sealed_at IS NULL),
		       COUNT(*) FILTER (WHERE lease_epoch <> $2)
		FROM evaluation_route_evidence
		WHERE assignment_id = $1`, input.AssignmentID, leaseEpoch).Scan(&evidenceCount, &unsealedCount, &wrongEpochCount); err != nil {
		return nil, fmt.Errorf("validate route evidence seal: %w", err)
	}
	if wrongEpochCount > 0 {
		return nil, service.ErrLeaseFenced
	}
	if evidenceCount == 0 || unsealedCount > 0 {
		return nil, service.ErrRouteEvidenceNotSealed
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_assignments
		SET evidence_manifest = $2::jsonb, status = 'evidence_uploaded', heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1`, input.AssignmentID, string(bytes.TrimSpace(input.Evidence))); err != nil {
		return nil, fmt.Errorf("store assignment evidence: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'evidence_uploaded', updated_at = NOW() WHERE id = $1`, sampleID); err != nil {
		return nil, fmt.Errorf("mark sample evidence uploaded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit evidence submission: %w", err)
	}
	return &service.EvidenceReceipt{AssignmentID: input.AssignmentID, EvidenceManifestSHA256: digestHex, AcceptedAt: time.Now().UTC()}, nil
}

func validArtifactSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func artifactUploadURL(objectKey string) string {
	// The staging worker understands this scheme as a local volume target. A
	// production object-store adapter can replace this URL without changing
	// the database or worker protocol.
	return "staging://" + objectKey
}

func (r *evaluationGradingRepository) PresignArtifact(ctx context.Context, assignmentID uuid.UUID, leaseToken string, input service.ArtifactPresignRequest) (*service.ArtifactUpload, error) {
	if r == nil || r.db == nil || assignmentID == uuid.Nil || strings.TrimSpace(leaseToken) == "" || input.Bytes <= 0 || input.Bytes > 1024*1024*1024 || !validArtifactSHA256(input.SHA256) || strings.TrimSpace(input.MIMEType) == "" || len(input.MIMEType) > 100 {
		return nil, service.ErrArtifactInvalid
	}
	input.MIMEType = strings.TrimSpace(input.MIMEType)
	input.SHA256 = strings.TrimSpace(input.SHA256)
	var expectedEpoch any
	if input.LeaseEpoch > 0 {
		expectedEpoch = input.LeaseEpoch
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin artifact presign: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var runID, sampleID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT s.run_id, a.sample_id FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		JOIN evaluation_runs r ON r.id = s.run_id
		JOIN evaluation_workers w ON w.id = a.leased_by AND w.status = 'active'
		WHERE a.id = $1 AND a.lease_token_hash = $2 AND a.lease_expires_at > NOW()
		  AND a.status IN ('leased', 'running') AND a.lease_epoch = r.control_epoch
		  AND ($3::bigint IS NULL OR a.lease_epoch = $3) FOR UPDATE`, assignmentID, hashToken(leaseToken), expectedEpoch).Scan(&runID, &sampleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrLeaseFenced
	}
	if err != nil {
		return nil, fmt.Errorf("lock artifact assignment: %w", err)
	}
	var existing service.ArtifactUpload
	var existingExpires time.Time
	err = tx.QueryRowContext(ctx, `
		SELECT id, object_key, sha256, byte_count, mime_type, retention_deadline
		FROM evaluation_artifacts
		WHERE assignment_id = $1 AND sha256 = $2 AND byte_count = $3 AND mime_type = $4
		ORDER BY created_at DESC LIMIT 1`, assignmentID, input.SHA256, input.Bytes, input.MIMEType).Scan(
		&existing.ID, &existing.ObjectKey, &existing.SHA256, &existing.Bytes, &existing.MIMEType, &existingExpires)
	if err == nil {
		existing.UploadURL = artifactUploadURL(existing.ObjectKey)
		existing.ExpiresAt = existingExpires
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit idempotent artifact presign: %w", err)
		}
		return &existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("find existing artifact presign: %w", err)
	}
	artifactID := uuid.New()
	objectKey := fmt.Sprintf("evaluation-artifacts/%s/%s/%s", runID, sampleID, artifactID)
	var expiresAt time.Time
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_artifacts (id, run_id, sample_id, assignment_id, object_key, sha256, byte_count, mime_type, retention_deadline)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW() + INTERVAL '30 days')
		RETURNING retention_deadline`, artifactID, runID, sampleID, assignmentID, objectKey, input.SHA256, input.Bytes, input.MIMEType).Scan(&expiresAt); err != nil {
		return nil, fmt.Errorf("create artifact upload: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit artifact presign: %w", err)
	}
	return &service.ArtifactUpload{ID: artifactID, ObjectKey: objectKey, UploadURL: artifactUploadURL(objectKey), SHA256: input.SHA256, Bytes: input.Bytes, MIMEType: input.MIMEType, ExpiresAt: expiresAt}, nil
}

func (r *evaluationGradingRepository) ConfirmArtifact(ctx context.Context, assignmentID uuid.UUID, leaseToken string, input service.ArtifactConfirmation) (*service.ArtifactReceipt, error) {
	if r == nil || r.db == nil || assignmentID == uuid.Nil || input.ArtifactID == uuid.Nil || strings.TrimSpace(leaseToken) == "" || input.Bytes < 0 || !validArtifactSHA256(input.SHA256) || strings.TrimSpace(input.ObjectKey) == "" {
		return nil, service.ErrArtifactInvalid
	}
	input.ObjectKey = strings.TrimSpace(input.ObjectKey)
	input.SHA256 = strings.TrimSpace(input.SHA256)
	var expectedEpoch any
	if input.LeaseEpoch > 0 {
		expectedEpoch = input.LeaseEpoch
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin artifact confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var active bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM evaluation_assignments a
			JOIN evaluation_samples s ON s.id = a.sample_id
			JOIN evaluation_runs r ON r.id = s.run_id
			JOIN evaluation_workers w ON w.id = a.leased_by AND w.status = 'active'
			WHERE a.id = $1 AND a.lease_token_hash = $2 AND a.lease_expires_at > NOW()
			  AND a.status IN ('leased', 'running') AND a.lease_epoch = r.control_epoch
			  AND ($3::bigint IS NULL OR a.lease_epoch = $3)
		)`, assignmentID, hashToken(leaseToken), expectedEpoch).Scan(&active); err != nil {
		return nil, fmt.Errorf("check artifact lease: %w", err)
	}
	if !active {
		return nil, service.ErrLeaseFenced
	}
	var receipt service.ArtifactReceipt
	var confirmedAt sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT id, object_key, sha256, byte_count, mime_type, scan_status, confirmed_at
		FROM evaluation_artifacts WHERE id = $1 AND assignment_id = $2 FOR UPDATE`, input.ArtifactID, assignmentID).Scan(
		&receipt.ID, &receipt.ObjectKey, &receipt.SHA256, &receipt.Bytes, &receipt.MIMEType, &receipt.ScanStatus, &confirmedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrArtifactNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load artifact confirmation: %w", err)
	}
	if receipt.ObjectKey != input.ObjectKey || receipt.SHA256 != input.SHA256 || receipt.Bytes != input.Bytes {
		return nil, service.ErrArtifactObjectMismatch
	}
	if confirmedAt.Valid {
		receipt.ConfirmedAt = confirmedAt.Time
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit idempotent artifact confirmation: %w", err)
		}
		return &receipt, nil
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE evaluation_artifacts SET confirmed_at = NOW(), scan_status = 'clean'
		WHERE id = $1 RETURNING confirmed_at`, input.ArtifactID).Scan(&receipt.ConfirmedAt); err != nil {
		return nil, fmt.Errorf("confirm artifact: %w", err)
	}
	receipt.ScanStatus = "clean"
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit artifact confirmation: %w", err)
	}
	return &receipt, nil
}

func (r *evaluationGradingRepository) CompleteAssignment(ctx context.Context, assignmentID uuid.UUID, leaseToken string, leaseEpoch ...int64) error {
	var epoch int64
	if len(leaseEpoch) > 0 {
		epoch = leaseEpoch[0]
	}
	return (&evaluationRepository{db: r.db}).TransitionAssignment(ctx, service.AssignmentTransition{AssignmentID: assignmentID, LeaseToken: leaseToken, To: service.AssignmentStatusCompleted, LeaseEpoch: epoch})
}

func (r *evaluationGradingRepository) FailAssignment(ctx context.Context, assignmentID uuid.UUID, leaseToken, failureClass, failureCode string, leaseEpoch ...int64) error {
	to := service.AssignmentStatusInfraFailed
	if service.FailureClass(strings.TrimSpace(failureClass)) == service.FailureClassUpstream {
		to = service.AssignmentStatusUpstreamFailed
	}
	if service.FailureClass(strings.TrimSpace(failureClass)) == service.FailureClassInvalidEvidence {
		to = service.AssignmentStatusInvalidEvidence
	}
	var epoch int64
	if len(leaseEpoch) > 0 {
		epoch = leaseEpoch[0]
	}
	return (&evaluationRepository{db: r.db}).TransitionAssignment(ctx, service.AssignmentTransition{AssignmentID: assignmentID, LeaseToken: leaseToken, To: to, LeaseEpoch: epoch})
}

// EnsureNextGradingPartitions is called by maintenance before a month closes.
// The migration function is idempotent and also works on installations that
// missed the initial three-month provisioning.
func (r *evaluationGradingRepository) EnsureNextGradingPartitions(ctx context.Context, now time.Time) error {
	if r == nil || r.db == nil {
		return errors.New("nil evaluation grading repository")
	}
	month := now.UTC().AddDate(0, 1, 0).Format("2006-01-02")
	_, err := r.db.ExecContext(ctx, `SELECT ensure_evaluation_grading_partitions($1::date)`, month)
	if err != nil {
		return fmt.Errorf("ensure evaluation grading partitions: %w", err)
	}
	return nil
}

func (r *evaluationGradingRepository) AuthenticateWorker(ctx context.Context, token, workerKind string) (uuid.UUID, error) {
	if r == nil || r.db == nil {
		return uuid.Nil, errors.New("nil evaluation grading repository")
	}
	token = strings.TrimSpace(token)
	workerKind = strings.TrimSpace(workerKind)
	if token == "" || workerKind == "" {
		return uuid.Nil, errors.New("evaluation worker credentials are required")
	}
	var id uuid.UUID
	err := r.db.QueryRowContext(ctx, `
		SELECT id FROM evaluation_workers
		WHERE token_hash = $1 AND status = 'active' AND worker_kind = $2`, hashToken(token), workerKind).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("authenticate evaluation worker: %w", err)
	}
	var otherKind string
	otherErr := r.db.QueryRowContext(ctx, `
		SELECT worker_kind FROM evaluation_workers
		WHERE token_hash = $1 AND status = 'active'`, hashToken(token)).Scan(&otherKind)
	if otherErr == nil && otherKind != workerKind {
		return uuid.Nil, service.ErrWorkerKindMismatch
	}
	return uuid.Nil, errors.New("evaluation worker credentials are invalid")
}

func (r *evaluationGradingRepository) ClaimGradingLease(ctx context.Context, workerID uuid.UUID, graderIDs []string, leaseTTL time.Duration) (*service.GradingLease, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation grading repository")
	}
	if workerID == uuid.Nil || leaseTTL <= 0 {
		return nil, errors.New("grader worker and positive lease ttl are required")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin grading lease claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var kind string
	var capabilities pq.StringArray
	var workerImageDigest sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT worker_kind, capabilities, image_digest FROM evaluation_workers
		WHERE id = $1 AND status = 'active' AND claim_mode = 'open' FOR UPDATE`, workerID).Scan(&kind, &capabilities, &workerImageDigest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("evaluation grader worker is unavailable")
		}
		return nil, fmt.Errorf("lock grader worker: %w", err)
	}
	if kind != "grader" {
		return nil, service.ErrWorkerKindMismatch
	}
	allowed := authorizedWorkerCapabilities(graderIDs, capabilities)
	if len(allowed) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty grader capability claim: %w", err)
		}
		return nil, nil
	}

	var lease service.GradingLease
	lease.Case = &service.EvaluationCaseSpec{}
	var routeTrace sql.NullString
	var jobStatus string
	var workOrigin string
	var revisionBatchID sql.NullString
	var gradingInputHash sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT g.id, g.run_id, g.sample_id, g.assignment_id,
		       g.grader_id, g.grader_version, g.attempt,
		       g.status, g.work_origin, g.revision_batch_id, g.grading_input_hash, g.recovery_generation,
		       a.evidence_manifest, s.route_trace_id,
		       c.id, c.case_key, c.capability_domain, c.priority, c.weight,
		       c.prompt_spec, c.expected_spec, c.execution_spec,
		       c.grader_id, c.grader_version, c.content_sha256, c.confidentiality
		FROM evaluation_grading_jobs g
		JOIN evaluation_assignments a ON a.id = g.assignment_id
		JOIN evaluation_samples s ON s.id = g.sample_id
		JOIN evaluation_cases c ON c.id = s.case_id
		JOIN evaluation_runs run ON run.id = g.run_id
		LEFT JOIN evaluation_revision_batches batch
		  ON batch.id = g.revision_batch_id AND batch.run_id = g.run_id
		WHERE g.grader_id = ANY($1::text[])
		  AND a.status IN ('evidence_uploaded', 'completed')
		  AND (g.status = 'pending' OR (g.status = 'leased' AND g.lease_expires_at <= NOW()))
		  AND (
			(g.work_origin = 'regrade' AND run.status = 'completed' AND batch.status = 'running')
			OR
			(g.work_origin <> 'regrade' AND run.status NOT IN ('paused', 'cancelled', 'completed', 'failed'))
		  )
		ORDER BY g.created_at, g.id
		FOR UPDATE OF g SKIP LOCKED
		LIMIT 1`, pq.Array(allowed)).Scan(
		&lease.ID, &lease.RunID, &lease.SampleID, &lease.AssignmentID,
		&lease.GraderID, &lease.GraderVersion, &lease.Attempt, &jobStatus,
		&workOrigin, &revisionBatchID, &gradingInputHash, &lease.RecoveryGeneration,
		&lease.EvidenceManifest, &routeTrace,
		&lease.Case.CaseID, &lease.Case.CaseKey, &lease.Case.CapabilityDomain, &lease.Case.Priority, &lease.Case.Weight,
		&lease.Case.PromptSpec, &lease.Case.ExpectedSpec, &lease.Case.ExecutionSpec,
		&lease.Case.GraderID, &lease.Case.GraderVersion, &lease.Case.ContentSHA256, &lease.Case.Confidentiality)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty grader lease claim: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select grading job: %w", err)
	}
	var runEpoch int64
	var runStatus service.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT control_epoch, status FROM evaluation_runs WHERE id = $1 FOR UPDATE`, lease.RunID).Scan(&runEpoch, &runStatus); err != nil {
		return nil, fmt.Errorf("lock grading run: %w", err)
	}
	lease.WorkOrigin = workOrigin
	lease.GradingInputHash = gradingInputHash.String
	if revisionBatchID.Valid {
		lease.RevisionBatchID, err = uuid.Parse(revisionBatchID.String)
		if err != nil {
			return nil, fmt.Errorf("parse grading revision batch: %w", err)
		}
	}
	if workOrigin == "regrade" {
		var batchStatus service.RevisionBatchStatus
		if lease.RevisionBatchID == uuid.Nil || lease.GradingInputHash == "" {
			return nil, service.ErrRevisionBatchFenced
		}
		if err := tx.QueryRowContext(ctx, `SELECT control_epoch, status FROM evaluation_revision_batches WHERE id=$1 AND run_id=$2 FOR UPDATE`, lease.RevisionBatchID, lease.RunID).Scan(&lease.LeaseEpoch, &batchStatus); err != nil {
			return nil, service.ErrRevisionBatchFenced
		}
		if runStatus != service.RunStatusCompleted || batchStatus != service.RevisionBatchRunning {
			if err := tx.Commit(); err != nil {
				return nil, fmt.Errorf("commit fenced regrade claim: %w", err)
			}
			return nil, nil
		}
	} else {
		lease.LeaseEpoch = runEpoch
	}
	if workOrigin != "regrade" && (runStatus == service.RunStatusPaused || runStatus == service.RunStatusCancelled || runStatus == service.RunStatusCompleted || runStatus == service.RunStatusFailed) {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit paused grading claim: %w", err)
		}
		return nil, nil
	}
	lease.WorkerImageDigest = workerImageDigest.String
	if jobStatus == "leased" && workOrigin != "regrade" {
		lease.WorkOrigin = "reclaimed"
	} else if workOrigin == "" {
		lease.WorkOrigin = "initial"
	}
	if routeTrace.Valid {
		lease.RouteTraceID = routeTrace.String
	}
	artifactRows, artifactErr := tx.QueryContext(ctx, `
		SELECT id, object_key, sha256, byte_count, mime_type, scan_status, confirmed_at
		FROM evaluation_artifacts
		WHERE assignment_id = $1 AND confirmed_at IS NOT NULL AND scan_status = 'clean'
		ORDER BY created_at, id`, lease.AssignmentID)
	if artifactErr != nil {
		return nil, fmt.Errorf("load grading evidence artifacts: %w", artifactErr)
	}
	for artifactRows.Next() {
		var receipt service.ArtifactReceipt
		var confirmedAt sql.NullTime
		if err := artifactRows.Scan(&receipt.ID, &receipt.ObjectKey, &receipt.SHA256, &receipt.Bytes, &receipt.MIMEType, &receipt.ScanStatus, &confirmedAt); err != nil {
			artifactRows.Close()
			return nil, fmt.Errorf("scan grading evidence artifact: %w", err)
		}
		if confirmedAt.Valid {
			receipt.ConfirmedAt = confirmedAt.Time
		}
		lease.Evidence = append(lease.Evidence, receipt)
	}
	if err := artifactRows.Err(); err != nil {
		artifactRows.Close()
		return nil, fmt.Errorf("iterate grading evidence artifacts: %w", err)
	}
	artifactRows.Close()
	lease.Token, _, err = newLeaseToken()
	if err != nil {
		return nil, err
	}
	tokenHash := hashToken(lease.Token)
	if err := tx.QueryRowContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status = 'leased', lease_token_hash = $2, leased_by = $3,
		    lease_expires_at = NOW() + $4::interval, heartbeat_at = NOW(), lease_epoch = $5,
		    worker_image_digest = NULLIF($6, ''), work_origin = NULLIF($7, ''), updated_at = NOW()
		WHERE id = $1
		RETURNING lease_expires_at`, lease.ID, tokenHash, workerID, postgresInterval(leaseTTL), lease.LeaseEpoch, lease.WorkerImageDigest, lease.WorkOrigin).Scan(&lease.ExpiresAt); err != nil {
		return nil, fmt.Errorf("lease grading job: %w", err)
	}
	if lease.WorkOrigin != "regrade" {
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_assignments
			SET status = 'grading', heartbeat_at = NOW(), updated_at = NOW()
			WHERE id = $1 AND status = 'evidence_uploaded'`, lease.AssignmentID); err != nil {
			return nil, fmt.Errorf("mark assignment grading: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_samples SET status = 'grading', updated_at = NOW() WHERE id = $1`, lease.SampleID); err != nil {
			return nil, fmt.Errorf("mark sample grading: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_workers SET last_heartbeat_at = NOW(), updated_at = NOW() WHERE id = $1`, workerID); err != nil {
		return nil, fmt.Errorf("heartbeat grader worker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit grading lease claim: %w", err)
	}
	return &lease, nil
}

func (r *evaluationGradingRepository) HeartbeatGradingLease(ctx context.Context, leaseID uuid.UUID, leaseToken string, extendBy time.Duration, leaseEpoch ...int64) (time.Time, error) {
	if r == nil || r.db == nil {
		return time.Time{}, errors.New("nil evaluation grading repository")
	}
	if leaseID == uuid.Nil || strings.TrimSpace(leaseToken) == "" || extendBy <= 0 {
		return time.Time{}, service.ErrLeaseFenced
	}
	var expires time.Time
	err := withEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("worker"), func(tx *sql.Tx) error {
		query := `
			UPDATE evaluation_grading_jobs g
			SET lease_expires_at = NOW() + $3::interval, heartbeat_at = NOW(), updated_at = NOW()
			WHERE g.id = $1 AND g.status = 'leased'
			  AND g.lease_token_hash = $2 AND g.lease_expires_at > NOW()
			  AND EXISTS (SELECT 1 FROM evaluation_workers w
			              WHERE w.id = g.leased_by AND w.status = 'active')
			  AND ((g.work_origin = 'regrade' AND EXISTS (
			          SELECT 1 FROM evaluation_revision_batches batch
			          WHERE batch.id = g.revision_batch_id AND batch.run_id = g.run_id
			            AND batch.status = 'running' AND g.lease_epoch = batch.control_epoch))
			       OR (COALESCE(g.work_origin, 'initial') <> 'regrade' AND EXISTS (
			          SELECT 1 FROM evaluation_runs run
			          WHERE run.id = g.run_id AND g.lease_epoch = run.control_epoch)))`
		args := []any{leaseID, hashToken(leaseToken), postgresInterval(extendBy)}
		if len(leaseEpoch) > 0 && leaseEpoch[0] > 0 {
			query += ` AND lease_epoch = $4`
			args = append(args, leaseEpoch[0])
		}
		query += ` RETURNING lease_expires_at`
		err := tx.QueryRowContext(ctx, query, args...).Scan(&expires)
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrLeaseFenced
		}
		if err != nil {
			return fmt.Errorf("heartbeat grading lease: %w", err)
		}
		return nil
	})
	if err != nil {
		return time.Time{}, err
	}
	return expires, nil
}

func (r *evaluationGradingRepository) SubmitScore(ctx context.Context, leaseID uuid.UUID, leaseToken string, submission service.ScoreSubmission) (*service.Score, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation grading repository")
	}
	if leaseID == uuid.Nil || strings.TrimSpace(leaseToken) == "" {
		return nil, service.ErrScoreSubmissionInvalid
	}
	if submission.Score.IsNegative() || submission.Score.GreaterThan(decimal.NewFromInt(1)) {
		return nil, service.ErrScoreSubmissionInvalid
	}
	if submission.FailureClass != "" && !submission.FailureClass.Valid() {
		return nil, service.ErrScoreSubmissionInvalid
	}
	submission.EvidenceHashes = canonicalEvidenceHashes(submission.EvidenceHashes)
	submissionKey := scoreSubmissionKey(leaseID, submission)

	// Idempotency is checked before the lease fence. A retried HTTP request
	// after the original transaction committed must receive the same score.
	if existing, err := r.findScoreBySubmissionKey(ctx, submissionKey); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin score submission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var job struct {
		runID, sampleID, assignmentID             uuid.UUID
		graderID, graderVersion, assignmentStatus string
		assignmentAttempt                         int
		workOrigin                                string
		revisionBatchID                           uuid.NullUUID
		gradingInputHash                          sql.NullString
		recoveryGeneration                        int
		leaseHash                                 sql.NullString
		leaseExpires                              sql.NullTime
		leasedBy                                  sql.NullString
		leaseEpoch                                sql.NullInt64
		runEpoch                                  int64
		runStatus                                 string
	}
	err = tx.QueryRowContext(ctx, `
		SELECT g.run_id, g.sample_id, g.assignment_id, g.grader_id, g.grader_version,
		       a.status, a.attempt, g.work_origin, g.revision_batch_id, g.grading_input_hash,
		       g.recovery_generation, g.lease_token_hash, g.lease_expires_at, g.leased_by, g.lease_epoch,
		       run.control_epoch, run.status
		FROM evaluation_grading_jobs g
		JOIN evaluation_assignments a ON a.id = g.assignment_id
		JOIN evaluation_runs run ON run.id = g.run_id
		JOIN evaluation_workers w ON w.id = g.leased_by AND w.status = 'active'
		WHERE g.id = $1 FOR UPDATE`, leaseID).Scan(
		&job.runID, &job.sampleID, &job.assignmentID, &job.graderID, &job.graderVersion,
		&job.assignmentStatus, &job.assignmentAttempt, &job.workOrigin, &job.revisionBatchID,
		&job.gradingInputHash, &job.recoveryGeneration, &job.leaseHash, &job.leaseExpires,
		&job.leasedBy, &job.leaseEpoch, &job.runEpoch, &job.runStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrLeaseFenced
	}
	if err != nil {
		return nil, fmt.Errorf("load grading lease: %w", err)
	}
	isRegrade := job.workOrigin == "regrade"
	assignmentStatusValid := job.assignmentStatus == "grading"
	if isRegrade {
		assignmentStatusValid = job.assignmentStatus == "completed"
	}
	if job.leaseHash.String != hashToken(leaseToken) || !job.leaseExpires.Valid || !job.leaseExpires.Time.After(time.Now()) || !assignmentStatusValid || (submission.LeaseEpoch > 0 && (!job.leaseEpoch.Valid || job.leaseEpoch.Int64 != submission.LeaseEpoch)) {
		return nil, service.ErrLeaseFenced
	}
	if !isRegrade && (job.runStatus == "paused" || job.runStatus == "cancelled" || job.runStatus == "completed" || job.runStatus == "failed" || !job.leaseEpoch.Valid || job.leaseEpoch.Int64 != job.runEpoch) {
		return nil, service.ErrLeaseFenced
	}
	var revisionBatchID *uuid.UUID
	var revisionBatchEpoch *int64
	var requirementID uuid.UUID
	var frozenPrevious service.ScoreRef
	if isRegrade {
		if !job.revisionBatchID.Valid || !job.gradingInputHash.Valid || !job.leaseEpoch.Valid || submission.LeaseEpoch <= 0 {
			return nil, service.ErrRevisionBatchFenced
		}
		var batchStatus service.RevisionBatchStatus
		var batchEpoch int64
		if err := tx.QueryRowContext(ctx, `
			SELECT status, control_epoch
			FROM evaluation_revision_batches
			WHERE id=$1 AND run_id=$2 FOR UPDATE`, job.revisionBatchID.UUID, job.runID).Scan(&batchStatus, &batchEpoch); err != nil {
			return nil, service.ErrRevisionBatchFenced
		}
		if batchStatus != service.RevisionBatchRunning || batchEpoch != job.leaseEpoch.Int64 || submission.LeaseEpoch != batchEpoch {
			return nil, service.ErrRevisionBatchFenced
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT id, previous_score_id, previous_score_created_at
			FROM evaluation_revision_batch_requirements
			WHERE revision_batch_id=$1 AND run_id=$2 AND requirement_type='grading'
			  AND source_assignment_id=$3 AND grader_id=$4 AND grader_version=$5
			  AND grading_input_hash=$6 AND recovery_generation=$7 AND status='pending'
			FOR UPDATE`, job.revisionBatchID.UUID, job.runID, job.assignmentID, job.graderID,
			job.graderVersion, job.gradingInputHash.String, job.recoveryGeneration).Scan(
			&requirementID, &frozenPrevious.ID, &frozenPrevious.CreatedAt); err != nil {
			return nil, service.ErrRevisionBatchFenced
		}
		revisionBatchID = &job.revisionBatchID.UUID
		revisionBatchEpoch = &batchEpoch
	}
	if submission.SampleID != uuid.Nil && job.sampleID != submission.SampleID {
		return nil, service.ErrGraderIdentityMismatch
	}
	if strings.TrimSpace(submission.GraderID) != "" && job.graderID != strings.TrimSpace(submission.GraderID) {
		return nil, service.ErrGraderIdentityMismatch
	}
	if strings.TrimSpace(submission.GraderVersion) != "" && job.graderVersion != strings.TrimSpace(submission.GraderVersion) {
		return nil, service.ErrGraderIdentityMismatch
	}
	var assignmentCurrent bool
	if err := tx.QueryRowContext(ctx, `
		SELECT NOT EXISTS (
			SELECT 1 FROM evaluation_assignments replacement
			WHERE replacement.sample_id = $1 AND replacement.attempt > $2
		)`, job.sampleID, job.assignmentAttempt).Scan(&assignmentCurrent); err != nil {
		return nil, fmt.Errorf("check current score assignment: %w", err)
	}
	if !assignmentCurrent {
		return nil, service.ErrLeaseFenced
	}

	var caseGraderID, caseGraderVersion string
	var modelRoute, capabilityDomain string
	if err := tx.QueryRowContext(ctx, `
		SELECT c.grader_id, c.grader_version, s.model_route, c.capability_domain
		FROM evaluation_samples s JOIN evaluation_cases c ON c.id = s.case_id
		WHERE s.id = $1`, job.sampleID).Scan(&caseGraderID, &caseGraderVersion, &modelRoute, &capabilityDomain); err != nil {
		return nil, fmt.Errorf("load grading identity: %w", err)
	}
	if caseGraderID != job.graderID || caseGraderVersion != job.graderVersion {
		return nil, service.ErrGraderIdentityMismatch
	}
	source, expectedEvidenceHashes, err := loadSealedScoreSource(ctx, tx, job.runID, job.sampleID, job.assignmentID)
	if err != nil {
		return nil, err
	}
	if !sameStrings(expectedEvidenceHashes, submission.EvidenceHashes) {
		return nil, service.ErrEvidenceMismatch
	}

	version := 1
	var previous service.ScoreRef
	err = tx.QueryRowContext(ctx, `SELECT version, score_id, score_created_at FROM evaluation_score_heads WHERE sample_id = $1 AND grader_id = $2 FOR UPDATE`, job.sampleID, job.graderID).Scan(&version, &previous.ID, &previous.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		version = 1
	} else if err != nil {
		return nil, fmt.Errorf("load score head: %w", err)
	} else {
		version++
	}
	if isRegrade && previous != frozenPrevious {
		return nil, service.ErrRevisionBatchFenced
	}
	manualReview := submission.FailureClass == service.FailureClassJudge
	scoreID := uuid.New()
	var createdAt time.Time
	routeEvidenceRefsJSON, err := json.Marshal(source.RouteEvidenceRefs)
	if err != nil {
		return nil, fmt.Errorf("marshal score route evidence refs: %w", err)
	}
	scoreArgs := []any{
		scoreID, job.runID, job.sampleID, job.graderID, job.graderVersion,
		version, submission.Score, submission.Passed, nullableFailureClass(submission.FailureClass),
		strings.TrimSpace(submission.FailureCode), strings.TrimSpace(submission.Explanation),
		pq.Array(submission.EvidenceHashes), manualReview, submissionKey, leaseID,
		source.AssignmentID, source.RouteEvidenceSetHash, routeEvidenceRefsJSON, source.ArtifactManifestHash,
	}
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_scores (
			id, run_id, sample_id, grader_id, grader_version, version, score, passed,
			failure_class, failure_code, explanation, evidence_hashes, manual_review_required,
			submission_idempotency_key, grading_job_id, source_assignment_id,
			route_evidence_set_hash, route_evidence_refs, artifact_manifest_hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11, $12, $13, $14, $15, $16, $17, $18, $19)
		RETURNING created_at`, scoreArgs...).Scan(&createdAt); err != nil {
		return nil, fmt.Errorf("insert evaluation score: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_score_idempotency (submission_idempotency_key, score_id, score_created_at)
		VALUES ($1, $2, $3)`, submissionKey, scoreID, createdAt); err != nil {
		return nil, fmt.Errorf("record score idempotency: %w", err)
	}
	headEventID := uuid.New()
	headReason := "initial"
	headWorkOrigin := "initial"
	if isRegrade {
		headReason = "regrade"
		headWorkOrigin = "regrade"
	}
	var previousID any
	var previousCreatedAt any
	if previous.ID != uuid.Nil {
		previousID = previous.ID
		previousCreatedAt = previous.CreatedAt
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_score_head_events (
			id, run_id, sample_id, grader_id, version, previous_score_id, previous_score_created_at,
			score_id, score_created_at, source_assignment_id, route_evidence_set_hash, reason,
			grading_job_id, revision_batch_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`, headEventID, job.runID, job.sampleID, job.graderID, version,
		previousID, previousCreatedAt, scoreID, createdAt, source.AssignmentID,
		source.RouteEvidenceSetHash, headReason, leaseID, revisionBatchID); err != nil {
		return nil, fmt.Errorf("append score head event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_score_heads (sample_id, grader_id, score_id, score_created_at, version, manual_review_required)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (sample_id, grader_id) DO UPDATE SET score_id = EXCLUDED.score_id,
			score_created_at = EXCLUDED.score_created_at, version = EXCLUDED.version,
			manual_review_required = EXCLUDED.manual_review_required, updated_at = NOW()`, job.sampleID, job.graderID, scoreID, createdAt, version, manualReview); err != nil {
		return nil, fmt.Errorf("update score head: %w", err)
	}
	if err := enqueueScoreHeadRecompute(ctx, tx, job.runID, modelRoute, capabilityDomain, headEventID,
		source.RouteEvidenceSetHash, headWorkOrigin, revisionBatchID, revisionBatchEpoch); err != nil {
		return nil, err
	}
	if manualReview {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_manual_reviews (id, run_id, sample_id, score_id, score_created_at, reason)
			VALUES ($1, $2, $3, $4, $5, 'judge_disagreement') ON CONFLICT (score_id) DO NOTHING`, uuid.New(), job.runID, job.sampleID, scoreID, createdAt); err != nil {
			return nil, fmt.Errorf("create manual review: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status = 'completed', score_id = $2, lease_token_hash = NULL, leased_by = NULL,
			score_created_at = $3, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE id = $1`, leaseID, scoreID, createdAt); err != nil {
		return nil, fmt.Errorf("complete grading job: %w", err)
	}
	if isRegrade {
		result, err := tx.ExecContext(ctx, `
			UPDATE evaluation_revision_batch_requirements
			SET status='completed', completed_at=NOW(), updated_at=NOW()
			WHERE id=$1 AND status='pending'`, requirementID)
		if err != nil {
			return nil, fmt.Errorf("complete revision grading requirement: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return nil, service.ErrRevisionBatchFenced
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE evaluation_assignments SET status = 'completed', lease_token_hash = NULL,
				leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
			WHERE id = $1`, job.assignmentID); err != nil {
			return nil, fmt.Errorf("complete grading assignment: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'completed', updated_at = NOW() WHERE id = $1`, job.sampleID); err != nil {
			return nil, fmt.Errorf("complete graded sample: %w", err)
		}
	}
	if job.leasedBy.Valid {
		if workerID, parseErr := uuid.Parse(job.leasedBy.String); parseErr == nil {
			if _, err := checkRadarWorkerDrainCompletionTx(ctx, tx, workerID, 0, "grading:"+leaseID.String()); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit score submission: %w", err)
	}
	if !isRegrade {
		if _, err := (&evaluationRepository{db: r.db}).ReconcileEvaluationRun(ctx, job.runID); err != nil {
			return nil, fmt.Errorf("reconcile evaluation run after score submission: %w", err)
		}
	}
	return &service.Score{
		ID: scoreID, RunID: job.runID, SampleID: job.sampleID, GraderID: job.graderID,
		GraderVersion: job.graderVersion, Version: version, Score: submission.Score,
		Passed: submission.Passed, FailureClass: submission.FailureClass, FailureCode: strings.TrimSpace(submission.FailureCode),
		Explanation: strings.TrimSpace(submission.Explanation), EvidenceHashes: append([]string(nil), submission.EvidenceHashes...),
		ManualReviewRequired: manualReview, CreatedAt: createdAt, Ref: service.ScoreRef{ID: scoreID, CreatedAt: createdAt}, HeadVersion: version, Source: source,
	}, nil
}

type scoreArtifactRef struct {
	ID        uuid.UUID `json:"id"`
	ObjectKey string    `json:"object_key"`
	SHA256    string    `json:"sha256"`
	Bytes     int64     `json:"bytes"`
	MIMEType  string    `json:"mime_type"`
}

func loadSealedScoreSource(ctx context.Context, tx *sql.Tx, runID, sampleID, assignmentID uuid.UUID) (service.ScoreSource, []string, error) {
	var assignmentLeaseEpoch sql.NullInt64
	if err := tx.QueryRowContext(ctx, `
		SELECT lease_epoch FROM evaluation_assignments WHERE id = $1 FOR KEY SHARE`, assignmentID).Scan(&assignmentLeaseEpoch); err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("load score assignment epoch: %w", err)
	}
	if !assignmentLeaseEpoch.Valid {
		return service.ScoreSource{}, nil, service.ErrEvidenceMismatch
	}
	var manifestHash string
	var manifestBytes []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT pair_spec.request_manifest_sha256, manifest.canonical_manifest_bytes
		FROM evaluation_samples sample
		JOIN evaluation_side_specs side_spec ON side_spec.sample_id = sample.id
		JOIN evaluation_pair_specs pair_spec ON pair_spec.id = side_spec.pair_spec_id
		JOIN evaluation_request_manifests manifest ON manifest.id = pair_spec.request_manifest_id
		WHERE sample.id = $1 AND sample.run_id = $2`, sampleID, runID).Scan(&manifestHash, &manifestBytes); err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("load score request manifest: %w", err)
	}
	var manifest service.RequestManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("decode score request manifest: %w", err)
	}
	canonicalManifest, err := service.CanonicalizeRequestManifest(manifest)
	if err != nil || canonicalManifest.SHA256 != manifestHash || !bytes.Equal(canonicalManifest.Bytes, manifestBytes) {
		return service.ScoreSource{}, nil, service.ErrEvidenceMismatch
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT route_trace_id, request_ordinal, payload_hash, request_manifest_sha256, lease_epoch,
		       sealed_at IS NOT NULL
		FROM evaluation_route_evidence
		WHERE evaluation_run_id = $1 AND sample_id = $2 AND assignment_id = $3
		ORDER BY request_ordinal, route_trace_id`, runID, sampleID, assignmentID)
	if err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("load score route evidence: %w", err)
	}
	defer rows.Close()
	refs := make([]service.RouteEvidenceRef, 0, manifest.MaxRequests)
	ordinals := make(map[int]struct{}, manifest.MaxRequests)
	slotCounts := make([]int, len(manifest.RequestSlots))
	for rows.Next() {
		var ref service.RouteEvidenceRef
		var rowManifestHash string
		var evidenceLeaseEpoch sql.NullInt64
		var sealed bool
		if err := rows.Scan(&ref.RouteTraceID, &ref.RequestOrdinal, &ref.PayloadHash, &rowManifestHash, &evidenceLeaseEpoch, &sealed); err != nil {
			return service.ScoreSource{}, nil, fmt.Errorf("scan score route evidence: %w", err)
		}
		if !sealed || rowManifestHash != manifestHash || !evidenceLeaseEpoch.Valid || evidenceLeaseEpoch.Int64 != assignmentLeaseEpoch.Int64 || !validArtifactSHA256(ref.PayloadHash) {
			return service.ScoreSource{}, nil, service.ErrEvidenceMismatch
		}
		if _, duplicate := ordinals[ref.RequestOrdinal]; duplicate {
			return service.ScoreSource{}, nil, service.ErrEvidenceMismatch
		}
		matchedSlot := -1
		for index, slot := range manifest.RequestSlots {
			if ref.RequestOrdinal >= slot.OrdinalMin && ref.RequestOrdinal <= slot.OrdinalMax {
				matchedSlot = index
				break
			}
		}
		if matchedSlot < 0 {
			return service.ScoreSource{}, nil, service.ErrEvidenceMismatch
		}
		ordinals[ref.RequestOrdinal] = struct{}{}
		slotCounts[matchedSlot]++
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("iterate score route evidence: %w", err)
	}
	if len(refs) < manifest.MinRequests || len(refs) > manifest.MaxRequests {
		return service.ScoreSource{}, nil, service.ErrEvidenceMismatch
	}
	for index, slot := range manifest.RequestSlots {
		if slotCounts[index] > slot.MaxOccurrences || (slot.Required && slotCounts[index] == 0) {
			return service.ScoreSource{}, nil, service.ErrEvidenceMismatch
		}
	}

	artifactRows, err := tx.QueryContext(ctx, `
		SELECT id, object_key, sha256, byte_count, mime_type
		FROM evaluation_artifacts
		WHERE assignment_id = $1 AND sample_id = $2 AND confirmed_at IS NOT NULL AND scan_status = 'clean'
		ORDER BY created_at, id`, assignmentID, sampleID)
	if err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("load score artifacts: %w", err)
	}
	defer artifactRows.Close()
	artifacts := make([]scoreArtifactRef, 0)
	evidenceHashes := make([]string, 0)
	for artifactRows.Next() {
		var artifact scoreArtifactRef
		if err := artifactRows.Scan(&artifact.ID, &artifact.ObjectKey, &artifact.SHA256, &artifact.Bytes, &artifact.MIMEType); err != nil {
			return service.ScoreSource{}, nil, fmt.Errorf("scan score artifact: %w", err)
		}
		if !validArtifactSHA256(artifact.SHA256) {
			return service.ScoreSource{}, nil, service.ErrEvidenceMismatch
		}
		artifacts = append(artifacts, artifact)
		evidenceHashes = append(evidenceHashes, artifact.SHA256)
	}
	if err := artifactRows.Err(); err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("iterate score artifacts: %w", err)
	}
	refsJSON, err := json.Marshal(struct {
		ExpectedManifestHash string                     `json:"expected_manifest_hash"`
		RouteEvidenceRefs    []service.RouteEvidenceRef `json:"route_evidence_refs"`
	}{ExpectedManifestHash: manifestHash, RouteEvidenceRefs: refs})
	if err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("marshal score route evidence refs: %w", err)
	}
	setHash, err := service.DigestCanonicalJSON(refsJSON)
	if err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("hash score route evidence refs: %w", err)
	}
	artifactsJSON, err := json.Marshal(artifacts)
	if err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("marshal score artifact refs: %w", err)
	}
	artifactManifestHash, err := service.DigestCanonicalJSON(artifactsJSON)
	if err != nil {
		return service.ScoreSource{}, nil, fmt.Errorf("hash score artifact manifest: %w", err)
	}
	return service.ScoreSource{
		AssignmentID: assignmentID, RouteEvidenceSetHash: setHash,
		RouteEvidenceRefs: refs, ArtifactManifestHash: artifactManifestHash,
	}, canonicalEvidenceHashes(evidenceHashes), nil
}

func enqueueScoreHeadRecompute(ctx context.Context, tx *sql.Tx, runID uuid.UUID, modelRoute, capabilityDomain string, headEventID uuid.UUID, evidenceSetHash, workOrigin string, revisionBatchID *uuid.UUID, _ *int64) error {
	payload := map[string]any{
		"capability_domain":   capabilityDomain,
		"model_route":         modelRoute,
		"score_head_event_id": headEventID.String(),
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal score recompute payload: %w", err)
	}
	input := service.EnqueueEvaluationOutboxInput{
		EventType: "cell_recompute", RunID: runID,
		ScopeKey:        capabilityDomain + "/" + service.CanonicalModelRoute(modelRoute),
		AnalysisVersion: "score-head-v1", SourceType: "score_head_event",
		SourceID:   headEventID.String(),
		SourceHash: hashString("score-head-event\x00" + headEventID.String() + "\x00" + evidenceSetHash),
		Payload:    payloadJSON,
		WorkOrigin: workOrigin,
	}
	if revisionBatchID != nil {
		input.RevisionBatchID = *revisionBatchID
	}
	if _, err := enqueueEvaluationOutbox(ctx, tx, input); err != nil {
		return fmt.Errorf("enqueue score recompute: %w", err)
	}
	return nil
}

func (r *evaluationGradingRepository) FailGradingLease(ctx context.Context, leaseID uuid.UUID, leaseToken, failureClass, failureCode string, leaseEpoch ...int64) error {
	if r == nil || r.db == nil {
		return errors.New("nil evaluation grading repository")
	}
	if leaseID == uuid.Nil || strings.TrimSpace(leaseToken) == "" {
		return service.ErrLeaseFenced
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return fmt.Errorf("begin fail grading lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var job struct {
		runID, assignmentID, sampleID       uuid.UUID
		graderID, graderVersion, workOrigin string
		revisionBatchID                     uuid.NullUUID
		gradingInputHash                    sql.NullString
		recoveryGeneration                  int
		workerID                            sql.NullString
		leaseEpoch                          sql.NullInt64
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT run_id, assignment_id, sample_id, grader_id, grader_version, work_origin,
		       revision_batch_id, grading_input_hash, recovery_generation, leased_by, lease_epoch
		FROM evaluation_grading_jobs
		WHERE id=$1 AND status='leased' AND lease_token_hash=$2 AND lease_expires_at > NOW()
		FOR UPDATE`, leaseID, hashToken(leaseToken)).Scan(
		&job.runID, &job.assignmentID, &job.sampleID, &job.graderID, &job.graderVersion,
		&job.workOrigin, &job.revisionBatchID, &job.gradingInputHash,
		&job.recoveryGeneration, &job.workerID, &job.leaseEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return service.ErrLeaseFenced
		}
		return fmt.Errorf("load grading worker lease: %w", err)
	}
	isRegrade := job.workOrigin == "regrade"
	var expectedEpoch any
	if len(leaseEpoch) > 0 && leaseEpoch[0] > 0 {
		expectedEpoch = leaseEpoch[0]
	}
	var requirementID uuid.UUID
	if isRegrade {
		if !job.revisionBatchID.Valid || !job.gradingInputHash.Valid || !job.leaseEpoch.Valid || expectedEpoch == nil {
			return service.ErrRevisionBatchFenced
		}
		var batchStatus service.RevisionBatchStatus
		var batchEpoch int64
		if err := tx.QueryRowContext(ctx, `
			SELECT status, control_epoch FROM evaluation_revision_batches
			WHERE id=$1 AND run_id=$2 FOR UPDATE`, job.revisionBatchID.UUID, job.runID).Scan(&batchStatus, &batchEpoch); err != nil ||
			batchStatus != service.RevisionBatchRunning || batchEpoch != job.leaseEpoch.Int64 || leaseEpoch[0] != batchEpoch {
			return service.ErrRevisionBatchFenced
		}
		if err := tx.QueryRowContext(ctx, `
			SELECT id FROM evaluation_revision_batch_requirements
			WHERE revision_batch_id=$1 AND run_id=$2 AND requirement_type='grading'
			  AND source_assignment_id=$3 AND grader_id=$4 AND grader_version=$5
			  AND grading_input_hash=$6 AND recovery_generation=$7 AND status='pending'
			FOR UPDATE`, job.revisionBatchID.UUID, job.runID, job.assignmentID, job.graderID,
			job.graderVersion, job.gradingInputHash.String, job.recoveryGeneration).Scan(&requirementID); err != nil {
			return service.ErrRevisionBatchFenced
		}
	}
	err = tx.QueryRowContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status = 'failed', lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL,
			failure_class = NULLIF($3, ''), failure_code = NULLIF($4, ''), finished_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'leased' AND lease_token_hash = $2 AND lease_expires_at > NOW()
		  AND ($5::bigint IS NULL OR lease_epoch = $5)
		RETURNING id`, leaseID, hashToken(leaseToken), strings.TrimSpace(failureClass), strings.TrimSpace(failureCode), expectedEpoch).Scan(new(uuid.UUID))
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrLeaseFenced
	}
	if err != nil {
		return fmt.Errorf("fail grading lease: %w", err)
	}
	if isRegrade {
		result, err := tx.ExecContext(ctx, `
			UPDATE evaluation_revision_batch_requirements
			SET status='failed', failure_code=NULLIF($2,''), updated_at=NOW()
			WHERE id=$1 AND status='pending'`, requirementID, strings.TrimSpace(failureCode))
		if err != nil {
			return fmt.Errorf("fail revision grading requirement: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return service.ErrRevisionBatchFenced
		}
		if err := reconcileRevisionBatch(ctx, tx, job.revisionBatchID.UUID); err != nil {
			return err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_assignments SET status = 'grading_failed', lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL, failure_class = NULLIF($2, ''), failure_code = NULLIF($3, ''), finished_at = NOW(), updated_at = NOW() WHERE id = $1`, job.assignmentID, strings.TrimSpace(failureClass), strings.TrimSpace(failureCode)); err != nil {
			return fmt.Errorf("fail grading assignment: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'grading_failed', failure_class = NULLIF($2, ''), failure_code = NULLIF($3, ''), updated_at = NOW() WHERE id = $1`, job.sampleID, strings.TrimSpace(failureClass), strings.TrimSpace(failureCode)); err != nil {
			return fmt.Errorf("fail graded sample: %w", err)
		}
	}
	if job.workerID.Valid {
		if parsed, parseErr := uuid.Parse(job.workerID.String); parseErr == nil {
			if _, err := checkRadarWorkerDrainCompletionTx(ctx, tx, parsed, 0, "grading-fail:"+leaseID.String()); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed grading lease: %w", err)
	}
	if !isRegrade {
		if _, err := (&evaluationRepository{db: r.db}).ReconcileEvaluationRun(ctx, job.runID); err != nil {
			return fmt.Errorf("reconcile evaluation run after grading failure: %w", err)
		}
	}
	return nil
}

func (r *evaluationGradingRepository) ClaimAnalysisJob(ctx context.Context, workerID uuid.UUID, capabilities []string, leaseTTL time.Duration) (*service.AnalysisJobLease, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation grading repository")
	}
	if workerID == uuid.Nil || leaseTTL <= 0 {
		return nil, errors.New("statistics worker and positive lease ttl are required")
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin analysis lease claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var kind string
	var registered pq.StringArray
	var workerImageDigest sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT worker_kind, capabilities, image_digest FROM evaluation_workers WHERE id = $1 AND status = 'active' AND claim_mode = 'open' FOR UPDATE`, workerID).Scan(&kind, &registered, &workerImageDigest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("statistics worker is unavailable")
		}
		return nil, fmt.Errorf("lock statistics worker: %w", err)
	}
	if kind != "statistics" {
		return nil, service.ErrWorkerKindMismatch
	}
	allowed := authorizedWorkerCapabilities(capabilities, registered)
	if len(allowed) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty statistics capability claim: %w", err)
		}
		return nil, nil
	}
	var lease service.AnalysisJobLease
	var jobStatus string
	var revisionBatchID uuid.NullUUID
	var inputSetHash sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT job.id, job.run_id, job.capability_domain, job.model_route, job."window",
		       job.analysis_version, job.window_start, job.status, job.scope, job.work_origin,
		       job.revision_batch_id, job.input_set_hash, job.aggregate_revision
		FROM evaluation_analysis_jobs job
		JOIN evaluation_runs run ON run.id=job.run_id
		LEFT JOIN evaluation_revision_batches batch ON batch.id=job.revision_batch_id
		WHERE job.capability_domain = ANY($1::text[])
		  AND (job.status = 'pending' OR (job.status = 'leased' AND job.lease_expires_at <= NOW()))
		  AND (
			(job.work_origin IN ('initial','reclaimed')
			 AND run.status NOT IN ('paused','cancelled','completed','failed'))
			OR (job.work_origin='regrade' AND batch.status='running')
		  )
		ORDER BY job.created_at, job.id FOR UPDATE OF job SKIP LOCKED LIMIT 1`, pq.Array(allowed)).Scan(
		&lease.ID, &lease.RunID, &lease.CapabilityDomain, &lease.ModelRoute, &lease.Window,
		&lease.AnalysisVersion, &lease.WindowStart, &jobStatus, &lease.Scope, &lease.WorkOrigin,
		&revisionBatchID, &inputSetHash, &lease.AggregateRevision)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty statistics claim: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select analysis job: %w", err)
	}
	if revisionBatchID.Valid {
		lease.RevisionBatchID = revisionBatchID.UUID
		var batchStatus service.RevisionBatchStatus
		if err := tx.QueryRowContext(ctx, `
			SELECT control_epoch, status FROM evaluation_revision_batches
			WHERE id=$1 AND run_id=$2 FOR UPDATE`, revisionBatchID.UUID, lease.RunID).Scan(
			&lease.LeaseEpoch, &batchStatus); err != nil || batchStatus != service.RevisionBatchRunning {
			return nil, service.ErrAnalysisJobFenced
		}
	} else {
		var runStatus service.RunStatus
		if err := tx.QueryRowContext(ctx, `SELECT control_epoch, status FROM evaluation_runs WHERE id = $1 FOR UPDATE`, lease.RunID).Scan(&lease.LeaseEpoch, &runStatus); err != nil {
			return nil, fmt.Errorf("lock analysis run: %w", err)
		}
	}
	lease.WorkerImageDigest = workerImageDigest.String
	if inputSetHash.Valid {
		lease.InputSetHash = inputSetHash.String
		lease.ScoreRefs, lease.SnapshotRefs, err = loadFrozenAnalysisJobRefs(ctx, tx, lease.ID)
		if err != nil {
			return nil, err
		}
		lease.ScoreIDs = make([]uuid.UUID, 0, len(lease.ScoreRefs))
		for _, ref := range lease.ScoreRefs {
			lease.ScoreIDs = append(lease.ScoreIDs, ref.ID)
		}
		if lease.Scope == "cell" {
			lease.Pairs, err = loadFrozenAnalysisPairs(ctx, tx, lease.ID, lease.ModelRoute)
			if err != nil {
				return nil, err
			}
		}
	} else {
		lease.ScoreIDs, lease.Pairs, lease.History, lease.InvalidFailures, err = loadAnalysisInputs(
			ctx, tx, lease.RunID, lease.CapabilityDomain, lease.ModelRoute, lease.WindowStart,
		)
		if err != nil {
			return nil, err
		}
	}
	lease.Token, _, err = newLeaseToken()
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE evaluation_analysis_jobs
		SET status = 'leased', lease_token_hash = $2, leased_by = $3,
			lease_expires_at = NOW() + $4::interval, heartbeat_at = NOW(), lease_epoch = $5,
			worker_image_digest = NULLIF($6, ''), work_origin = NULLIF($7, ''), updated_at = NOW()
		WHERE id = $1 RETURNING lease_expires_at`, lease.ID, hashToken(lease.Token), workerID, postgresInterval(leaseTTL), lease.LeaseEpoch, lease.WorkerImageDigest, lease.WorkOrigin).Scan(&lease.ExpiresAt); err != nil {
		return nil, fmt.Errorf("lease analysis job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_workers SET last_heartbeat_at = NOW(), updated_at = NOW() WHERE id = $1`, workerID); err != nil {
		return nil, fmt.Errorf("heartbeat statistics worker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit analysis lease claim: %w", err)
	}
	return &lease, nil
}

func loadFrozenAnalysisJobRefs(ctx context.Context, tx *sql.Tx, jobID uuid.UUID) ([]service.ScoreRef, []service.SnapshotRef, error) {
	scoreRows, err := tx.QueryContext(ctx, `
		SELECT score_id, score_created_at
		FROM evaluation_analysis_job_score_inputs
		WHERE analysis_job_id=$1 ORDER BY input_ordinal`, jobID)
	if err != nil {
		return nil, nil, fmt.Errorf("load frozen analysis score refs: %w", err)
	}
	scoreRefs := make([]service.ScoreRef, 0)
	for scoreRows.Next() {
		var ref service.ScoreRef
		if err := scoreRows.Scan(&ref.ID, &ref.CreatedAt); err != nil {
			scoreRows.Close()
			return nil, nil, fmt.Errorf("scan frozen analysis score ref: %w", err)
		}
		scoreRefs = append(scoreRefs, ref)
	}
	if err := scoreRows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close frozen analysis score refs: %w", err)
	}
	snapshotRows, err := tx.QueryContext(ctx, `
		SELECT snapshot_id, window_start
		FROM evaluation_analysis_job_snapshot_inputs
		WHERE analysis_job_id=$1 ORDER BY input_ordinal`, jobID)
	if err != nil {
		return nil, nil, fmt.Errorf("load frozen analysis snapshot refs: %w", err)
	}
	snapshotRefs := make([]service.SnapshotRef, 0)
	for snapshotRows.Next() {
		var ref service.SnapshotRef
		if err := snapshotRows.Scan(&ref.ID, &ref.WindowStart); err != nil {
			snapshotRows.Close()
			return nil, nil, fmt.Errorf("scan frozen analysis snapshot ref: %w", err)
		}
		snapshotRefs = append(snapshotRefs, ref)
	}
	if err := snapshotRows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close frozen analysis snapshot refs: %w", err)
	}
	return scoreRefs, snapshotRefs, nil
}

func loadAnalysisInputs(ctx context.Context, tx *sql.Tx, runID uuid.UUID, domain, route string, windowStart time.Time) ([]uuid.UUID, []service.PairedScore, []service.AggregateHistoryPoint, []service.FailureClass, error) {
	baseRoute := strings.TrimPrefix(strings.TrimPrefix(route, "baseline:"), "candidate:")
	rows, err := tx.QueryContext(ctx, `
		SELECT b.case_id, $3, b.sample_index, c.weight,
		       MAX(bs.score) AS baseline_score, MAX(cs.score) AS candidate_score,
		       MIN(bs.id::text)::uuid AS baseline_score_id,
		       MIN(cs.id::text)::uuid AS candidate_score_id
		FROM evaluation_samples b
		JOIN evaluation_cases c ON c.id = b.case_id
		JOIN evaluation_samples d ON d.run_id = b.run_id AND d.case_id = b.case_id
		  AND d.sample_index = b.sample_index AND d.model_route = 'candidate:' || $3
		JOIN evaluation_score_heads bsh ON bsh.sample_id = b.id
		JOIN evaluation_scores bs ON bs.id = bsh.score_id AND bs.created_at = bsh.score_created_at
		JOIN evaluation_score_heads csh ON csh.sample_id = d.id
		JOIN evaluation_scores cs ON cs.id = csh.score_id AND cs.created_at = csh.score_created_at
		JOIN evaluation_assignments ba ON ba.id = bs.source_assignment_id
		JOIN evaluation_assignments ca ON ca.id = cs.source_assignment_id
		WHERE b.run_id = $1 AND b.model_route = 'baseline:' || $3
		  AND c.capability_domain = $2
		  AND bsh.grader_id = c.grader_id
		  AND csh.grader_id = c.grader_id
		  AND ba.attempt = (SELECT MAX(current_assignment.attempt) FROM evaluation_assignments current_assignment WHERE current_assignment.sample_id = b.id)
		  AND ca.attempt = (SELECT MAX(current_assignment.attempt) FROM evaluation_assignments current_assignment WHERE current_assignment.sample_id = d.id)
		  AND EXISTS (
			SELECT 1 FROM evaluation_route_evidence evidence
			WHERE evidence.assignment_id = ba.id AND evidence.sample_id = b.id AND evidence.sealed_at IS NOT NULL
		  )
		  AND EXISTS (
			SELECT 1 FROM evaluation_route_evidence evidence
			WHERE evidence.assignment_id = ca.id AND evidence.sample_id = d.id AND evidence.sealed_at IS NOT NULL
		  )
		GROUP BY b.case_id, b.sample_index, c.weight
		ORDER BY b.case_id, b.sample_index`, runID, domain, baseRoute)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load analysis paired scores: %w", err)
	}
	defer rows.Close()
	pairs := make([]service.PairedScore, 0)
	scoreIDs := make([]uuid.UUID, 0)
	for rows.Next() {
		var pair service.PairedScore
		var baselineID, candidateID uuid.UUID
		if err := rows.Scan(&pair.CaseID, &pair.ModelRoute, &pair.SampleIndex, &pair.Weight, &pair.BaselineScore, &pair.CandidateScore, &baselineID, &candidateID); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("scan analysis paired score: %w", err)
		}
		pairs = append(pairs, pair)
		scoreIDs = append(scoreIDs, baselineID, candidateID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("iterate analysis paired scores: %w", err)
	}
	historyRows, err := tx.QueryContext(ctx, `
		SELECT aggregate->>'delta_pp'
		FROM evaluation_aggregate_snapshots
		WHERE run_id = $1 AND capability_domain = $2 AND model_route = $3
		  AND "window" = 'daily' AND window_start < $4
		ORDER BY window_start DESC LIMIT 30`, runID, domain, route, windowStart)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load analysis history: %w", err)
	}
	defer historyRows.Close()
	history := make([]service.AggregateHistoryPoint, 0)
	for historyRows.Next() {
		var raw sql.NullString
		if err := historyRows.Scan(&raw); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("scan analysis history: %w", err)
		}
		if raw.Valid && raw.String != "" {
			value, err := decimal.NewFromString(raw.String)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("parse analysis history delta: %w", err)
			}
			history = append(history, service.AggregateHistoryPoint{DeltaPP: value})
		}
	}
	if err := historyRows.Err(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("iterate analysis history: %w", err)
	}
	failureRows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT s.failure_class
		FROM evaluation_samples s
		JOIN evaluation_cases c ON c.id = s.case_id
		WHERE s.run_id = $1 AND c.capability_domain = $2
		  AND (s.model_route = 'baseline:' || $3 OR s.model_route = 'candidate:' || $3)
		  AND s.failure_class IS NOT NULL
		ORDER BY s.failure_class`, runID, domain, baseRoute)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load analysis invalid failures: %w", err)
	}
	defer failureRows.Close()
	invalidFailures := make([]service.FailureClass, 0)
	for failureRows.Next() {
		var value string
		if err := failureRows.Scan(&value); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("scan analysis invalid failure: %w", err)
		}
		invalidFailures = append(invalidFailures, service.FailureClass(value))
	}
	if err := failureRows.Err(); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("iterate analysis invalid failures: %w", err)
	}
	return scoreIDs, pairs, history, invalidFailures, nil
}

func (r *evaluationGradingRepository) CompleteAnalysisJob(ctx context.Context, jobID uuid.UUID, leaseToken string, submission service.AggregateSubmission, leaseEpoch ...int64) (*service.AggregateSnapshot, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation grading repository")
	}
	if jobID == uuid.Nil || strings.TrimSpace(leaseToken) == "" {
		return nil, service.ErrAnalysisJobFenced
	}
	if submission.LeaseEpoch == 0 && len(leaseEpoch) > 0 {
		submission.LeaseEpoch = leaseEpoch[0]
	}
	var revisioned bool
	if err := r.db.QueryRowContext(ctx, `
		SELECT input_set_hash IS NOT NULL FROM evaluation_analysis_jobs WHERE id=$1`, jobID).Scan(&revisioned); err == nil && revisioned {
		return r.completeRevisionAnalysisJob(ctx, jobID, leaseToken, submission)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("detect revision analysis job: %w", err)
	}
	tx, err := beginRadarWriterTx(ctx, r.db, "worker")
	if err != nil {
		return nil, fmt.Errorf("begin analysis completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var job struct {
		runID, snapshotID              uuid.UUID
		domain, route, window, version string
		windowStart                    time.Time
		status                         string
		leaseHash                      sql.NullString
		leaseExpires                   sql.NullTime
		leasedBy                       sql.NullString
		leaseEpoch                     sql.NullInt64
		runEpoch                       int64
		runStatus                      string
		workerStatus                   sql.NullString
	}
	err = tx.QueryRowContext(ctx, `
		SELECT run_id, capability_domain, model_route, "window", analysis_version, window_start,
			job.status, job.lease_token_hash, job.lease_expires_at, job.leased_by, job.lease_epoch,
			run.control_epoch, run.status, w.status,
			COALESCE(job.snapshot_id, '00000000-0000-0000-0000-000000000000')
		FROM evaluation_analysis_jobs job
		JOIN evaluation_runs run ON run.id = job.run_id
		LEFT JOIN evaluation_workers w ON w.id = job.leased_by
		WHERE job.id = $1 FOR UPDATE`, jobID).Scan(
		&job.runID, &job.domain, &job.route, &job.window, &job.version, &job.windowStart,
		&job.status, &job.leaseHash, &job.leaseExpires, &job.leasedBy, &job.leaseEpoch,
		&job.runEpoch, &job.runStatus, &job.workerStatus, &job.snapshotID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAnalysisJobFenced
	}
	if err != nil {
		return nil, fmt.Errorf("load analysis job: %w", err)
	}
	if submission.RunID == uuid.Nil {
		submission.RunID = job.runID
	}
	if submission.RunID != job.runID {
		return nil, service.ErrAggregateRunMismatch
	}
	if len(submission.ScoreIDs) == 0 {
		return nil, service.ErrScoreSubmissionInvalid
	}
	if job.status == "completed" && job.snapshotID != uuid.Nil {
		return r.loadAggregateSnapshot(ctx, tx, job.snapshotID, job.windowStart)
	}
	if job.status != "leased" || job.leaseHash.String != hashToken(leaseToken) || !job.leaseExpires.Valid || !job.leaseExpires.Time.After(time.Now()) || !job.workerStatus.Valid || job.workerStatus.String != "active" || (submission.LeaseEpoch > 0 && (!job.leaseEpoch.Valid || job.leaseEpoch.Int64 != submission.LeaseEpoch)) || !job.leaseEpoch.Valid || job.leaseEpoch.Int64 != job.runEpoch || job.runStatus == "paused" || job.runStatus == "cancelled" || job.runStatus == "completed" || job.runStatus == "failed" {
		return nil, service.ErrAnalysisJobFenced
	}
	ids := uniqueUUIDs(submission.ScoreIDs)
	idStrings := make([]string, 0, len(ids))
	for _, id := range ids {
		idStrings = append(idStrings, id.String())
	}
	var found, foreign, manual int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT s.id), COUNT(DISTINCT s.id) FILTER (WHERE s.run_id <> $2),
			COUNT(DISTINCT s.id) FILTER (WHERE s.manual_review_required)
		FROM evaluation_scores s WHERE s.id = ANY($1::uuid[])`, pq.Array(idStrings), job.runID).Scan(&found, &foreign, &manual); err != nil {
		return nil, fmt.Errorf("validate aggregate scores: %w", err)
	}
	if found != len(ids) || foreign > 0 {
		return nil, service.ErrAggregateRunMismatch
	}
	if manual > 0 {
		return nil, service.ErrScoreSubmissionInvalid
	}
	if len(submission.Aggregate) == 0 {
		submission.Aggregate = json.RawMessage(`{}`)
	}
	var aggregateJSON any
	if !json.Valid(submission.Aggregate) || json.Unmarshal(submission.Aggregate, &aggregateJSON) != nil {
		return nil, service.ErrScoreSubmissionInvalid
	}
	snapshotID := uuid.New()
	if job.snapshotID != uuid.Nil {
		snapshotID = job.snapshotID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_aggregate_snapshots (
			id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, score_ids, aggregate
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb)
		ON CONFLICT (run_id, capability_domain, model_route, "window", analysis_version, window_start)
		DO NOTHING
		`, snapshotID, job.runID, job.domain, job.route, job.window, job.version, job.windowStart, pq.Array(idStrings), submission.Aggregate); err != nil {
		if gradingUniqueViolation(err) {
			return r.loadAggregateSnapshot(ctx, tx, snapshotID, job.windowStart)
		}
		return nil, fmt.Errorf("insert aggregate snapshot: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT id FROM evaluation_aggregate_snapshots WHERE run_id = $1 AND capability_domain = $2 AND model_route = $3 AND "window" = $4 AND analysis_version = $5 AND window_start = $6`, job.runID, job.domain, job.route, job.window, job.version, job.windowStart).Scan(&snapshotID); err != nil {
		return nil, fmt.Errorf("load aggregate snapshot id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_analysis_jobs SET status = 'completed', snapshot_id = $2,
			lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE id = $1`, jobID, snapshotID); err != nil {
		return nil, fmt.Errorf("complete analysis job: %w", err)
	}
	if job.leasedBy.Valid {
		if workerID, parseErr := uuid.Parse(job.leasedBy.String); parseErr == nil {
			if _, err := checkRadarWorkerDrainCompletionTx(ctx, tx, workerID, 0, "analysis:"+jobID.String()); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit analysis completion: %w", err)
	}
	if _, err := (&evaluationRepository{db: r.db}).ReconcileEvaluationRun(ctx, job.runID); err != nil {
		return nil, fmt.Errorf("reconcile evaluation run after analysis completion: %w", err)
	}
	return r.loadAggregateSnapshot(ctx, r.db, snapshotID, job.windowStart)
}

func (r *evaluationGradingRepository) findScoreBySubmissionKey(ctx context.Context, key string) (*service.Score, error) {
	var ref service.ScoreRef
	if err := r.db.QueryRowContext(ctx, `
		SELECT score_id, score_created_at
		FROM evaluation_score_idempotency
		WHERE submission_idempotency_key = $1`, key).Scan(&ref.ID, &ref.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup score idempotency: %w", err)
	}
	return r.loadScore(ctx, r.db, ref)
}

func (r *evaluationGradingRepository) loadScore(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, ref service.ScoreRef) (*service.Score, error) {
	var score service.Score
	var passed sql.NullBool
	var failureClass sql.NullString
	var hashes pq.StringArray
	var sourceAssignmentID uuid.UUID
	var routeEvidenceRefs json.RawMessage
	err := q.QueryRowContext(ctx, `
		SELECT id, run_id, sample_id, grader_id, grader_version, version, score,
			passed, failure_class, failure_code, explanation, evidence_hashes,
			manual_review_required, created_at, source_assignment_id,
			route_evidence_set_hash, route_evidence_refs, artifact_manifest_hash
		FROM evaluation_scores WHERE id = $1 AND created_at = $2`, ref.ID, ref.CreatedAt).Scan(
		&score.ID, &score.RunID, &score.SampleID, &score.GraderID, &score.GraderVersion,
		&score.Version, &score.Score, &passed, &failureClass, &score.FailureCode,
		&score.Explanation, &hashes, &score.ManualReviewRequired, &score.CreatedAt, &sourceAssignmentID,
		&score.Source.RouteEvidenceSetHash, &routeEvidenceRefs, &score.Source.ArtifactManifestHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load score: %w", err)
	}
	if passed.Valid {
		score.Passed = &passed.Bool
	}
	if failureClass.Valid {
		score.FailureClass = service.FailureClass(failureClass.String)
	}
	score.EvidenceHashes = append([]string(nil), hashes...)
	score.Ref = service.ScoreRef{ID: score.ID, CreatedAt: score.CreatedAt}
	score.HeadVersion = score.Version
	score.Source.AssignmentID = sourceAssignmentID
	if err := json.Unmarshal(routeEvidenceRefs, &score.Source.RouteEvidenceRefs); err != nil {
		return nil, fmt.Errorf("decode score route evidence refs: %w", err)
	}
	return &score, nil
}

func (r *evaluationGradingRepository) loadAggregateSnapshot(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uuid.UUID, windowStart time.Time) (*service.AggregateSnapshot, error) {
	var snapshot service.AggregateSnapshot
	var ids pq.StringArray
	if err := q.QueryRowContext(ctx, `
		SELECT id, run_id, capability_domain, model_route, "window", analysis_version,
			window_start, score_ids, aggregate, created_at
		FROM evaluation_aggregate_snapshots WHERE id = $1 AND window_start = $2`, id, windowStart).Scan(
		&snapshot.ID, &snapshot.RunID, &snapshot.CapabilityDomain, &snapshot.ModelRoute,
		&snapshot.Window, &snapshot.AnalysisVersion, &snapshot.WindowStart, &ids,
		&snapshot.Aggregate, &snapshot.CreatedAt); err != nil {
		return nil, fmt.Errorf("load aggregate snapshot: %w", err)
	}
	for _, raw := range ids {
		if parsed, err := uuid.Parse(raw); err == nil {
			snapshot.ScoreIDs = append(snapshot.ScoreIDs, parsed)
		}
	}
	return &snapshot, nil
}

func authorizedWorkerCapabilities(requested, registered []string) []string {
	registeredSet := make(map[string]struct{}, len(registered))
	for _, value := range registered {
		if value = strings.TrimSpace(value); value != "" {
			registeredSet[value] = struct{}{}
		}
	}
	if len(requested) == 0 {
		requested = registered
	}
	result := make([]string, 0, len(requested))
	seen := make(map[string]struct{}, len(requested))
	for _, value := range requested {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := registeredSet[value]; ok {
			if _, duplicate := seen[value]; !duplicate {
				seen[value] = struct{}{}
				result = append(result, value)
			}
		}
	}
	return result
}

func canonicalEvidenceHashes(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func sameStrings(left, right []string) bool {
	left = canonicalEvidenceHashes(left)
	right = canonicalEvidenceHashes(right)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func scoreSubmissionKey(leaseID uuid.UUID, submission service.ScoreSubmission) string {
	passed := ""
	if submission.Passed != nil {
		passed = fmt.Sprintf("%t", *submission.Passed)
	}
	return hashString(strings.Join([]string{
		"score", leaseID.String(), submission.SampleID.String(), submission.GraderID,
		submission.GraderVersion, submission.Score.String(), passed,
		string(submission.FailureClass), submission.FailureCode, submission.Explanation,
		strings.Join(canonicalEvidenceHashes(submission.EvidenceHashes), ","),
	}, "\x00"))
}

func nullableFailureClass(value service.FailureClass) string {
	return strings.TrimSpace(string(value))
}

func uniqueUUIDs(values []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(values))
	result := make([]uuid.UUID, 0, len(values))
	for _, value := range values {
		if value == uuid.Nil {
			continue
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

func gradingUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}
