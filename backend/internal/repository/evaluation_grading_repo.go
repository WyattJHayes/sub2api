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

func (r *evaluationGradingRepository) RenewAssignmentLease(ctx context.Context, assignmentID uuid.UUID, leaseToken string, extendBy time.Duration) (time.Time, error) {
	return (&evaluationRepository{db: r.db}).RenewLease(ctx, assignmentID, leaseToken, extendBy)
}

func (r *evaluationGradingRepository) SubmitEvidence(ctx context.Context, input service.EvidenceSubmission, leaseToken string) (*service.EvidenceReceipt, error) {
	if r == nil || r.db == nil || input.AssignmentID == uuid.Nil || input.SampleID == uuid.Nil || strings.TrimSpace(leaseToken) == "" || len(input.Evidence) == 0 {
		return nil, service.ErrLeaseFenced
	}
	digest := sha256.Sum256(bytes.TrimSpace(input.Evidence))
	digestHex := hex.EncodeToString(digest[:])
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("runner"))
	if err != nil {
		return nil, fmt.Errorf("begin evidence submission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var sampleID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		UPDATE evaluation_assignments
		SET evidence_manifest = $4::jsonb, status = 'evidence_uploaded', heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND sample_id = $2 AND lease_token_hash = $3 AND lease_expires_at > NOW()
		  AND status IN ('leased', 'running')
		RETURNING sample_id`, input.AssignmentID, input.SampleID, hashToken(leaseToken), string(bytes.TrimSpace(input.Evidence))).Scan(&sampleID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrLeaseFenced
	}
	if err != nil {
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
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("runner"))
	if err != nil {
		return nil, fmt.Errorf("begin artifact presign: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var runID, sampleID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		SELECT run_id, sample_id FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		WHERE a.id = $1 AND a.lease_token_hash = $2 AND a.lease_expires_at > NOW()
		  AND a.status IN ('leased', 'running') FOR UPDATE`, assignmentID, hashToken(leaseToken)).Scan(&runID, &sampleID)
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
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("runner"))
	if err != nil {
		return nil, fmt.Errorf("begin artifact confirmation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
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
	var active bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM evaluation_assignments
		 WHERE id = $1 AND lease_token_hash = $2 AND lease_expires_at > NOW()
		   AND status IN ('leased', 'running'))`, assignmentID, hashToken(leaseToken)).Scan(&active); err != nil {
		return nil, fmt.Errorf("check artifact lease: %w", err)
	}
	if !active {
		return nil, service.ErrLeaseFenced
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

func (r *evaluationGradingRepository) CompleteAssignment(ctx context.Context, assignmentID uuid.UUID, leaseToken string) error {
	return (&evaluationRepository{db: r.db}).TransitionAssignment(ctx, service.AssignmentTransition{AssignmentID: assignmentID, LeaseToken: leaseToken, To: service.AssignmentStatusCompleted})
}

func (r *evaluationGradingRepository) FailAssignment(ctx context.Context, assignmentID uuid.UUID, leaseToken, failureClass, failureCode string) error {
	to := service.AssignmentStatusInfraFailed
	if service.FailureClass(strings.TrimSpace(failureClass)) == service.FailureClassUpstream {
		to = service.AssignmentStatusUpstreamFailed
	}
	if service.FailureClass(strings.TrimSpace(failureClass)) == service.FailureClassInvalidEvidence {
		to = service.AssignmentStatusInvalidEvidence
	}
	return (&evaluationRepository{db: r.db}).TransitionAssignment(ctx, service.AssignmentTransition{AssignmentID: assignmentID, LeaseToken: leaseToken, To: to})
}

// EnsureNextGradingPartitions is called by maintenance before a month closes.
// The migration function is idempotent and also works on installations that
// missed the initial three-month provisioning.
func (r *evaluationGradingRepository) EnsureNextGradingPartitions(ctx context.Context, now time.Time) error {
	if r == nil || r.db == nil {
		return errors.New("nil evaluation grading repository")
	}
	month := now.UTC().AddDate(0, 1, 0).Format("2006-01-02")
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("migration"), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `SELECT ensure_evaluation_grading_partitions($1::date)`, month)
		return err
	})
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
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("grader"))
	if err != nil {
		return nil, fmt.Errorf("begin grading lease claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var kind, claimMode string
	var capabilities pq.StringArray
	if err := tx.QueryRowContext(ctx, `
		SELECT worker_kind, capabilities, claim_mode FROM evaluation_workers
		WHERE id = $1 AND status = 'active' FOR UPDATE`, workerID).Scan(&kind, &capabilities, &claimMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("evaluation grader worker is unavailable")
		}
		return nil, fmt.Errorf("lock grader worker: %w", err)
	}
	if kind != "grader" {
		return nil, service.ErrWorkerKindMismatch
	}
	if claimMode != "open" {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit paused grader claim: %w", err)
		}
		return nil, nil
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
	err = tx.QueryRowContext(ctx, `
		SELECT g.id, g.run_id, g.sample_id, g.assignment_id,
		       g.grader_id, g.grader_version, g.attempt,
		       a.evidence_manifest, s.route_trace_id,
		       c.id, c.case_key, c.capability_domain, c.priority, c.weight,
		       c.prompt_spec, c.expected_spec, c.execution_spec,
		       c.grader_id, c.grader_version, c.content_sha256, c.confidentiality
		FROM evaluation_grading_jobs g
		JOIN evaluation_assignments a ON a.id = g.assignment_id
		JOIN evaluation_samples s ON s.id = g.sample_id
		JOIN evaluation_cases c ON c.id = s.case_id
		WHERE g.grader_id = ANY($1::text[])
		  AND a.status IN ('evidence_uploaded', 'completed')
		  AND (g.status = 'pending' OR (g.status = 'leased' AND g.lease_expires_at <= NOW()))
		ORDER BY g.created_at, g.id
		FOR UPDATE OF g SKIP LOCKED
		LIMIT 1`, pq.Array(allowed)).Scan(
		&lease.ID, &lease.RunID, &lease.SampleID, &lease.AssignmentID,
		&lease.GraderID, &lease.GraderVersion, &lease.Attempt,
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
		    lease_expires_at = NOW() + $4::interval, heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1
		RETURNING lease_expires_at`, lease.ID, tokenHash, workerID, postgresInterval(leaseTTL)).Scan(&lease.ExpiresAt); err != nil {
		return nil, fmt.Errorf("lease grading job: %w", err)
	}
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
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_workers SET last_heartbeat_at = NOW(), updated_at = NOW() WHERE id = $1`, workerID); err != nil {
		return nil, fmt.Errorf("heartbeat grader worker: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit grading lease claim: %w", err)
	}
	return &lease, nil
}

func (r *evaluationGradingRepository) HeartbeatGradingLease(ctx context.Context, leaseID uuid.UUID, leaseToken string, extendBy time.Duration) (time.Time, error) {
	if r == nil || r.db == nil {
		return time.Time{}, errors.New("nil evaluation grading repository")
	}
	if leaseID == uuid.Nil || strings.TrimSpace(leaseToken) == "" || extendBy <= 0 {
		return time.Time{}, service.ErrLeaseFenced
	}
	var expires time.Time
	err := WithEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("grader"), func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET lease_expires_at = NOW() + $3::interval, heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'leased' AND lease_token_hash = $2 AND lease_expires_at > NOW()
		RETURNING lease_expires_at`, leaseID, hashToken(leaseToken), postgresInterval(extendBy)).Scan(&expires)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, service.ErrLeaseFenced
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("heartbeat grading lease: %w", err)
	}
	return expires, nil
}

func (r *evaluationGradingRepository) SubmitScore(ctx context.Context, leaseID uuid.UUID, leaseToken string, submission service.ScoreSubmission) (*service.Score, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation grading repository")
	}
	if leaseID == uuid.Nil || strings.TrimSpace(leaseToken) == "" || submission.SampleID == uuid.Nil {
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

	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("grader"))
	if err != nil {
		return nil, fmt.Errorf("begin score submission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var job struct {
		runID, sampleID, assignmentID             uuid.UUID
		graderID, graderVersion, assignmentStatus string
		leaseHash                                 sql.NullString
		leaseExpires                              sql.NullTime
	}
	err = tx.QueryRowContext(ctx, `
		SELECT g.run_id, g.sample_id, g.assignment_id, g.grader_id, g.grader_version,
		       a.status, g.lease_token_hash, g.lease_expires_at
		FROM evaluation_grading_jobs g
		JOIN evaluation_assignments a ON a.id = g.assignment_id
		WHERE g.id = $1 FOR UPDATE`, leaseID).Scan(
		&job.runID, &job.sampleID, &job.assignmentID, &job.graderID, &job.graderVersion,
		&job.assignmentStatus, &job.leaseHash, &job.leaseExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrLeaseFenced
	}
	if err != nil {
		return nil, fmt.Errorf("load grading lease: %w", err)
	}
	if job.leaseHash.String != hashToken(leaseToken) || !job.leaseExpires.Valid || !job.leaseExpires.Time.After(time.Now()) || job.assignmentStatus != "grading" {
		return nil, service.ErrLeaseFenced
	}
	if job.sampleID != submission.SampleID || job.graderID != strings.TrimSpace(submission.GraderID) || job.graderVersion != strings.TrimSpace(submission.GraderVersion) {
		return nil, service.ErrGraderIdentityMismatch
	}

	var caseGraderID, caseGraderVersion string
	var modelRoute, capabilityDomain string
	if err := tx.QueryRowContext(ctx, `
		SELECT c.grader_id, c.grader_version, s.model_route, c.capability_domain
		FROM evaluation_samples s JOIN evaluation_cases c ON c.id = s.case_id
		WHERE s.id = $1`, job.sampleID).Scan(&caseGraderID, &caseGraderVersion, &modelRoute, &capabilityDomain); err != nil {
		return nil, fmt.Errorf("load grading identity: %w", err)
	}
	if caseGraderID != submission.GraderID || caseGraderVersion != submission.GraderVersion {
		return nil, service.ErrGraderIdentityMismatch
	}
	var expected pq.StringArray
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(array_agg(e.sha256 ORDER BY e.sha256), '{}'::text[])
		FROM evaluation_artifacts e
		WHERE e.assignment_id = $1 AND e.sample_id = $2 AND e.scan_status <> 'rejected'`, job.assignmentID, job.sampleID).Scan(&expected); err != nil {
		return nil, fmt.Errorf("load grading evidence hashes: %w", err)
	}
	if !sameStrings(expected, submission.EvidenceHashes) {
		return nil, service.ErrEvidenceMismatch
	}

	version := 1
	var previousID uuid.UUID
	err = tx.QueryRowContext(ctx, `SELECT version, score_id FROM evaluation_score_heads WHERE sample_id = $1 AND grader_id = $2 FOR UPDATE`, job.sampleID, submission.GraderID).Scan(&version, &previousID)
	if errors.Is(err, sql.ErrNoRows) {
		version = 1
	} else if err != nil {
		return nil, fmt.Errorf("load score head: %w", err)
	} else {
		version++
		if _, err := tx.ExecContext(ctx, `UPDATE evaluation_scores SET is_current = FALSE WHERE id = $1`, previousID); err != nil {
			return nil, fmt.Errorf("clear previous score head: %w", err)
		}
	}
	manualReview := submission.FailureClass == service.FailureClassJudge
	scoreID := uuid.New()
	var createdAt time.Time
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO evaluation_scores (
			id, run_id, sample_id, grader_id, grader_version, version, score, passed,
			failure_class, failure_code, explanation, evidence_hashes, is_current,
			manual_review_required, submission_idempotency_key
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11, $12, TRUE, $13, $14)
		RETURNING created_at`, scoreID, job.runID, job.sampleID, submission.GraderID, submission.GraderVersion,
		version, submission.Score, nullableFailureClass(submission.FailureClass), strings.TrimSpace(submission.FailureCode), strings.TrimSpace(submission.Explanation), pq.Array(submission.EvidenceHashes), manualReview, submissionKey).Scan(&createdAt); err != nil {
		return nil, fmt.Errorf("insert evaluation score: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_score_idempotency (submission_idempotency_key, score_id)
		VALUES ($1, $2)`, submissionKey, scoreID); err != nil {
		return nil, fmt.Errorf("record score idempotency: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_score_heads (sample_id, grader_id, score_id, score_created_at, version, manual_review_required)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (sample_id, grader_id) DO UPDATE SET score_id = EXCLUDED.score_id, score_created_at = EXCLUDED.score_created_at, version = EXCLUDED.version,
			manual_review_required = EXCLUDED.manual_review_required, updated_at = NOW()`, job.sampleID, submission.GraderID, scoreID, createdAt, version, manualReview); err != nil {
		return nil, fmt.Errorf("update score head: %w", err)
	}
	if manualReview {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO evaluation_manual_reviews (id, run_id, sample_id, score_id, reason)
			VALUES ($1, $2, $3, $4, 'judge_disagreement') ON CONFLICT (score_id) DO NOTHING`, uuid.New(), job.runID, job.sampleID, scoreID); err != nil {
			return nil, fmt.Errorf("create manual review: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status = 'completed', score_id = $2, lease_token_hash = NULL, leased_by = NULL,
		    lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE id = $1`, leaseID, scoreID); err != nil {
		return nil, fmt.Errorf("complete grading job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE evaluation_assignments SET status = 'completed', lease_token_hash = NULL,
			leased_by = NULL, lease_expires_at = NULL, finished_at = NOW(), updated_at = NOW()
		WHERE id = $1`, job.assignmentID); err != nil {
		return nil, fmt.Errorf("complete grading assignment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'completed', updated_at = NOW() WHERE id = $1`, job.sampleID); err != nil {
		return nil, fmt.Errorf("complete graded sample: %w", err)
	}
	windowStart := time.Now().UTC().Truncate(24 * time.Hour)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO evaluation_analysis_jobs (
			id, run_id, capability_domain, model_route, "window", analysis_version, window_start, status
		) VALUES ($1, $2, $3, $4, 'daily', 'v1', DATE_TRUNC('day', $5::timestamptz), 'pending')
		ON CONFLICT (run_id, capability_domain, model_route, "window", analysis_version, window_start) DO NOTHING`, uuid.New(), job.runID, capabilityDomain, modelRoute, windowStart); err != nil {
		return nil, fmt.Errorf("enqueue analysis job: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit score submission: %w", err)
	}
	return &service.Score{
		ID: scoreID, RunID: job.runID, SampleID: job.sampleID, GraderID: submission.GraderID,
		GraderVersion: submission.GraderVersion, Version: version, Score: submission.Score,
		Passed: submission.Passed, FailureClass: submission.FailureClass, FailureCode: strings.TrimSpace(submission.FailureCode),
		Explanation: strings.TrimSpace(submission.Explanation), EvidenceHashes: append([]string(nil), submission.EvidenceHashes...),
		IsCurrent: true, ManualReviewRequired: manualReview, CreatedAt: createdAt,
	}, nil
}

func (r *evaluationGradingRepository) FailGradingLease(ctx context.Context, leaseID uuid.UUID, leaseToken, failureClass, failureCode string) error {
	if r == nil || r.db == nil {
		return errors.New("nil evaluation grading repository")
	}
	if leaseID == uuid.Nil || strings.TrimSpace(leaseToken) == "" {
		return service.ErrLeaseFenced
	}
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("grader"))
	if err != nil {
		return fmt.Errorf("begin fail grading lease: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var assignmentID, sampleID uuid.UUID
	err = tx.QueryRowContext(ctx, `
		UPDATE evaluation_grading_jobs
		SET status = 'failed', lease_token_hash = NULL, leased_by = NULL, lease_expires_at = NULL,
			failure_class = NULLIF($3, ''), failure_code = NULLIF($4, ''), finished_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'leased' AND lease_token_hash = $2 AND lease_expires_at > NOW()
		RETURNING assignment_id, sample_id`, leaseID, hashToken(leaseToken), strings.TrimSpace(failureClass), strings.TrimSpace(failureCode)).Scan(&assignmentID, &sampleID)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ErrLeaseFenced
	}
	if err != nil {
		return fmt.Errorf("fail grading lease: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_assignments SET status = 'grading_failed', failure_class = NULLIF($2, ''), failure_code = NULLIF($3, ''), finished_at = NOW(), updated_at = NOW() WHERE id = $1`, assignmentID, strings.TrimSpace(failureClass), strings.TrimSpace(failureCode)); err != nil {
		return fmt.Errorf("fail grading assignment: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE evaluation_samples SET status = 'grading_failed', failure_class = NULLIF($2, ''), failure_code = NULLIF($3, ''), updated_at = NOW() WHERE id = $1`, sampleID, strings.TrimSpace(failureClass), strings.TrimSpace(failureCode)); err != nil {
		return fmt.Errorf("fail graded sample: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit failed grading lease: %w", err)
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
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("statistics"))
	if err != nil {
		return nil, fmt.Errorf("begin analysis lease claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var kind, claimMode string
	var registered pq.StringArray
	if err := tx.QueryRowContext(ctx, `SELECT worker_kind, capabilities, claim_mode FROM evaluation_workers WHERE id = $1 AND status = 'active' FOR UPDATE`, workerID).Scan(&kind, &registered, &claimMode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("statistics worker is unavailable")
		}
		return nil, fmt.Errorf("lock statistics worker: %w", err)
	}
	if kind != "statistics" {
		return nil, service.ErrWorkerKindMismatch
	}
	if claimMode != "open" {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit paused statistics claim: %w", err)
		}
		return nil, nil
	}
	allowed := authorizedWorkerCapabilities(capabilities, registered)
	if len(allowed) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty statistics capability claim: %w", err)
		}
		return nil, nil
	}
	var lease service.AnalysisJobLease
	err = tx.QueryRowContext(ctx, `
		SELECT id, run_id, capability_domain, model_route, "window", analysis_version, window_start
		FROM evaluation_analysis_jobs
		WHERE capability_domain = ANY($1::text[])
		  AND (status = 'pending' OR (status = 'leased' AND lease_expires_at <= NOW()))
		ORDER BY created_at, id FOR UPDATE SKIP LOCKED LIMIT 1`, pq.Array(allowed)).Scan(
		&lease.ID, &lease.RunID, &lease.CapabilityDomain, &lease.ModelRoute, &lease.Window,
		&lease.AnalysisVersion, &lease.WindowStart)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit empty statistics claim: %w", err)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select analysis job: %w", err)
	}
	lease.ScoreIDs, lease.Pairs, lease.History, lease.InvalidFailures, err = loadAnalysisInputs(ctx, tx, lease.RunID, lease.CapabilityDomain, lease.ModelRoute, lease.WindowStart)
	if err != nil {
		return nil, err
	}
	lease.Token, _, err = newLeaseToken()
	if err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `
		UPDATE evaluation_analysis_jobs
		SET status = 'leased', lease_token_hash = $2, leased_by = $3,
			lease_expires_at = NOW() + $4::interval, heartbeat_at = NOW(), updated_at = NOW()
		WHERE id = $1 RETURNING lease_expires_at`, lease.ID, hashToken(lease.Token), workerID, postgresInterval(leaseTTL)).Scan(&lease.ExpiresAt); err != nil {
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

func loadAnalysisInputs(ctx context.Context, tx *sql.Tx, runID uuid.UUID, domain, route string, windowStart time.Time) ([]uuid.UUID, []service.PairedScore, []service.AggregateHistoryPoint, []service.FailureClass, error) {
	baseRoute := strings.TrimPrefix(strings.TrimPrefix(route, "baseline:"), "candidate:")
	rows, err := tx.QueryContext(ctx, `
		SELECT b.case_id, $3, b.sample_index, c.weight,
		       MAX(bs.score) AS baseline_score, MAX(cs.score) AS candidate_score,
		       MAX(bs.id) AS baseline_score_id, MAX(cs.id) AS candidate_score_id
		FROM evaluation_samples b
		JOIN evaluation_cases c ON c.id = b.case_id
		JOIN evaluation_samples d ON d.run_id = b.run_id AND d.case_id = b.case_id
		  AND d.sample_index = b.sample_index AND d.model_route = 'candidate:' || $3
		JOIN evaluation_scores bs ON bs.sample_id = b.id AND bs.is_current = TRUE
		JOIN evaluation_scores cs ON cs.sample_id = d.id AND cs.is_current = TRUE
		WHERE b.run_id = $1 AND b.model_route = 'baseline:' || $3
		  AND c.capability_domain = $2
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
		  AND window = 'daily' AND window_start < $4
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

func (r *evaluationGradingRepository) CompleteAnalysisJob(ctx context.Context, jobID uuid.UUID, leaseToken string, submission service.AggregateSubmission) (*service.AggregateSnapshot, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("nil evaluation grading repository")
	}
	if jobID == uuid.Nil || strings.TrimSpace(leaseToken) == "" {
		return nil, service.ErrAnalysisJobFenced
	}
	tx, err := beginEvaluationWriterTx(ctx, r.db, defaultEvaluationWriterIdentity("statistics"))
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
	}
	err = tx.QueryRowContext(ctx, `
		SELECT run_id, capability_domain, model_route, "window", analysis_version, window_start,
			status, lease_token_hash, lease_expires_at, COALESCE(snapshot_id, '00000000-0000-0000-0000-000000000000')
		FROM evaluation_analysis_jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(
		&job.runID, &job.domain, &job.route, &job.window, &job.version, &job.windowStart,
		&job.status, &job.leaseHash, &job.leaseExpires, &job.snapshotID)
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
	if job.status != "leased" || job.leaseHash.String != hashToken(leaseToken) || !job.leaseExpires.Valid || !job.leaseExpires.Time.After(time.Now()) {
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
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit analysis completion: %w", err)
	}
	return r.loadAggregateSnapshot(ctx, r.db, snapshotID, job.windowStart)
}

func (r *evaluationGradingRepository) findScoreBySubmissionKey(ctx context.Context, key string) (*service.Score, error) {
	var id uuid.UUID
	if err := r.db.QueryRowContext(ctx, `SELECT score_id FROM evaluation_score_idempotency WHERE submission_idempotency_key = $1`, key).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup score idempotency: %w", err)
	}
	return r.loadScore(ctx, r.db, id)
}

func (r *evaluationGradingRepository) loadScore(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id uuid.UUID) (*service.Score, error) {
	var score service.Score
	var passed sql.NullBool
	var failureClass sql.NullString
	var hashes pq.StringArray
	err := q.QueryRowContext(ctx, `
		SELECT id, run_id, sample_id, grader_id, grader_version, version, score,
			passed, failure_class, failure_code, explanation, evidence_hashes,
			is_current, manual_review_required, created_at
		FROM evaluation_scores WHERE id = $1 ORDER BY created_at DESC LIMIT 1`, id).Scan(
		&score.ID, &score.RunID, &score.SampleID, &score.GraderID, &score.GraderVersion,
		&score.Version, &score.Score, &passed, &failureClass, &score.FailureCode,
		&score.Explanation, &hashes, &score.IsCurrent, &score.ManualReviewRequired, &score.CreatedAt)
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
