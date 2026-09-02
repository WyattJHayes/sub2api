package repository

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type artifactBoundaryStore struct {
	metadata          service.ArtifactObjectMetadata
	downloadURL       string
	downloadExpiresAt time.Time
}

func (s artifactBoundaryStore) PresignPut(context.Context, service.ArtifactObjectPutRequest, time.Duration) (*service.ArtifactObjectUpload, error) {
	return nil, nil
}

func (s artifactBoundaryStore) Head(context.Context, string) (*service.ArtifactObjectMetadata, error) {
	metadata := s.metadata
	return &metadata, nil
}

func (s artifactBoundaryStore) PresignGet(context.Context, string, time.Duration) (string, time.Time, error) {
	return s.downloadURL, s.downloadExpiresAt, nil
}

func (s artifactBoundaryStore) Delete(context.Context, string) error { return nil }

func (s artifactBoundaryStore) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("artifact")), nil
}

type artifactBoundaryScanner struct {
	beforeScan func() error
}

func (s artifactBoundaryScanner) Scan(context.Context, string, service.ArtifactObjectMetadata) (service.ArtifactScanResult, error) {
	if err := s.beforeScan(); err != nil {
		return service.ArtifactScanResult{Status: service.ArtifactScanFailed}, err
	}
	return service.ArtifactScanResult{
		Status:    service.ArtifactScanClean,
		Scanner:   "boundary-test",
		Reason:    "clean",
		ScannedAt: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC),
	}, nil
}

func expectArtifactWorkerWriter(mock sqlmock.Sqlmock) {
	identity := defaultEvaluationWriterIdentity("worker")
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO evaluation_writer_sessions").WithArgs(identity.InstanceID, "worker", currentEvaluationWriterProtocolVersion).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_instance_id'").WithArgs(identity.InstanceID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_protocol'").WithArgs("2").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("SELECT set_config\\('app.evaluation_writer_kind'").WithArgs("worker").WillReturnResult(sqlmock.NewResult(0, 1))
}

func artifactReceiptRows(artifactID uuid.UUID, assignmentID uuid.UUID, confirmedAt any) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "object_key", "sha256", "byte_count", "mime_type", "scan_status",
		"scan_reason", "scan_provider", "scanned_at", "confirmed_at", "deleted_at",
	}).AddRow(
		artifactID,
		"evaluation-artifacts/run/sample/"+artifactID.String(),
		testArtifactSHA256,
		int64(8),
		"application/json",
		"pending",
		nil,
		nil,
		nil,
		confirmedAt,
		nil,
	)
}

func TestConfirmArtifactScansOutsideWriterTransaction(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(true)

	assignmentID := uuid.New()
	artifactID := uuid.New()
	leaseToken := "lease-token"
	leaseEpoch := int64(7)
	objectKey := "evaluation-artifacts/run/sample/" + artifactID.String()

	mock.ExpectQuery("SELECT EXISTS").WithArgs(assignmentID, hashToken(leaseToken), leaseEpoch).WillReturnRows(sqlmock.NewRows([]string{"active"}).AddRow(true))
	mock.ExpectQuery("SELECT id, object_key, sha256, byte_count, mime_type, scan_status").WithArgs(artifactID, assignmentID).WillReturnRows(artifactReceiptRows(artifactID, assignmentID, nil))
	mock.ExpectExec("SELECT artifact_external_scan_marker").WillReturnResult(sqlmock.NewResult(0, 1))
	expectArtifactWorkerWriter(mock)
	mock.ExpectQuery("SELECT EXISTS").WithArgs(assignmentID, hashToken(leaseToken), leaseEpoch).WillReturnRows(sqlmock.NewRows([]string{"active"}).AddRow(true))
	mock.ExpectQuery("SELECT id, object_key, sha256, byte_count, mime_type, scan_status.*FOR UPDATE").WithArgs(artifactID, assignmentID).WillReturnRows(artifactReceiptRows(artifactID, assignmentID, nil))
	confirmedAt := time.Date(2026, 7, 30, 12, 0, 1, 0, time.UTC)
	mock.ExpectQuery("UPDATE evaluation_artifacts.*RETURNING confirmed_at").WithArgs(artifactID, "clean", "boundary-test", time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)).WillReturnRows(sqlmock.NewRows([]string{"confirmed_at"}).AddRow(confirmedAt))
	mock.ExpectCommit()

	repo := &evaluationGradingRepository{
		db: db,
		artifactStore: artifactBoundaryStore{metadata: service.ArtifactObjectMetadata{
			ObjectKey: objectKey,
			Bytes:     8,
			MIMEType:  "application/json",
			SHA256:    testArtifactSHA256,
		}},
		artifactScan: artifactBoundaryScanner{beforeScan: func() error {
			_, err := db.ExecContext(context.Background(), "SELECT artifact_external_scan_marker")
			return err
		}},
	}

	receipt, err := repo.ConfirmArtifact(context.Background(), assignmentID, leaseToken, service.ArtifactConfirmation{
		ArtifactID: artifactID,
		ObjectKey:  objectKey,
		SHA256:     testArtifactSHA256,
		Bytes:      8,
		LeaseEpoch: leaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, confirmedAt, receipt.ConfirmedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestBindEvidenceManifestArtifactRequiresTrustedCanonicalMatch(t *testing.T) {
	artifactID := uuid.New()
	confirmedAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	evidence := json.RawMessage(`{"b":2,"a":"<trusted>"}`)
	trusted := service.ArtifactReceipt{
		ID:          artifactID,
		ObjectKey:   "evaluation-artifacts/run/sample/evidence.json",
		SHA256:      "93c7b9b0bf8a4d5da9c943a1d3aa61fe5cfacdfe85b687cfb7a1f8bf17466207",
		Bytes:       23,
		MIMEType:    "application/json",
		ScanStatus:  "clean",
		Scanner:     "clamav",
		ConfirmedAt: confirmedAt,
	}

	digest, boundID, err := bindEvidenceManifestArtifact(evidence, []service.ArtifactReceipt{trusted})
	require.NoError(t, err)
	require.Equal(t, trusted.SHA256, digest)
	require.Equal(t, artifactID, boundID)

	for name, mutate := range map[string]func(*service.ArtifactReceipt){
		"wrong hash":  func(item *service.ArtifactReceipt) { item.SHA256 = strings.Repeat("0", 64) },
		"pending":     func(item *service.ArtifactReceipt) { item.ScanStatus = "pending" },
		"no scanner":  func(item *service.ArtifactReceipt) { item.Scanner = "" },
		"unconfirmed": func(item *service.ArtifactReceipt) { item.ConfirmedAt = time.Time{} },
		"deleted": func(item *service.ArtifactReceipt) {
			deletedAt := confirmedAt.Add(time.Minute)
			item.DeletedAt = &deletedAt
		},
		"wrong MIME": func(item *service.ArtifactReceipt) { item.MIMEType = "text/plain" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := trusted
			mutate(&candidate)
			_, _, err := bindEvidenceManifestArtifact(evidence, []service.ArtifactReceipt{candidate})
			require.ErrorIs(t, err, service.ErrEvidenceMismatch)
		})
	}
}

func TestSubmitEvidenceBindsCleanArtifactBeforeStateTransition(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mock.MatchExpectationsInOrder(true)

	assignmentID := uuid.New()
	sampleID := uuid.New()
	artifactID := uuid.New()
	leaseToken := "lease-token"
	leaseEpoch := int64(7)
	evidence := json.RawMessage(`{"b":2,"a":"<trusted>"}`)
	confirmedAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	expectArtifactWorkerWriter(mock)
	mock.ExpectQuery("SELECT a.sample_id, a.lease_epoch").WithArgs(assignmentID, sampleID, hashToken(leaseToken), leaseEpoch).WillReturnRows(sqlmock.NewRows([]string{"sample_id", "lease_epoch"}).AddRow(sampleID, leaseEpoch))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\)").WithArgs(assignmentID, leaseEpoch).WillReturnRows(sqlmock.NewRows([]string{"count", "unsealed", "wrong_epoch"}).AddRow(1, 0, 0))
	mock.ExpectQuery("SELECT id, object_key, sha256, byte_count, mime_type, scan_status").WithArgs(assignmentID).WillReturnRows(sqlmock.NewRows([]string{
		"id", "object_key", "sha256", "byte_count", "mime_type", "scan_status",
		"scan_reason", "scan_provider", "scanned_at", "confirmed_at", "deleted_at",
	}).AddRow(
		artifactID,
		"evaluation-artifacts/run/sample/evidence.json",
		"93c7b9b0bf8a4d5da9c943a1d3aa61fe5cfacdfe85b687cfb7a1f8bf17466207",
		int64(23),
		"application/json",
		"clean",
		"stream: OK",
		"clamav",
		confirmedAt,
		confirmedAt,
		nil,
	))
	mock.ExpectExec("UPDATE evaluation_assignments").WithArgs(assignmentID, string(evidence)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE evaluation_samples").WithArgs(sampleID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	repo := &evaluationGradingRepository{db: db, artifactStore: artifactBoundaryStore{}}
	receipt, err := repo.SubmitEvidence(context.Background(), service.EvidenceSubmission{
		AssignmentID: assignmentID,
		SampleID:     sampleID,
		Evidence:     evidence,
		LeaseEpoch:   leaseEpoch,
	}, leaseToken)
	require.NoError(t, err)
	require.Equal(t, "93c7b9b0bf8a4d5da9c943a1d3aa61fe5cfacdfe85b687cfb7a1f8bf17466207", receipt.EvidenceManifestSHA256)
	require.Equal(t, []uuid.UUID{artifactID}, receipt.ArtifactIDs)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPresignGradingArtifactReadRequiresOwnedLiveLease(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	leaseID := uuid.New()
	workerID := uuid.New()
	artifactID := uuid.New()
	leaseToken := "grading-lease-token"
	leaseEpoch := int64(11)
	expiresAt := time.Date(2026, 7, 30, 12, 5, 0, 0, time.UTC)
	objectKey := "evaluation-artifacts/run/sample/evidence.json"
	mock.ExpectQuery("FROM evaluation_grading_jobs g.*JOIN evaluation_artifacts ea").WithArgs(
		leaseID, hashToken(leaseToken), workerID, artifactID, leaseEpoch,
	).WillReturnRows(sqlmock.NewRows([]string{
		"id", "object_key", "sha256", "byte_count", "mime_type",
	}).AddRow(
		artifactID,
		objectKey,
		"93c7b9b0bf8a4d5da9c943a1d3aa61fe5cfacdfe85b687cfb7a1f8bf17466207",
		int64(23),
		"application/json",
	))

	repo := &evaluationGradingRepository{
		db: db,
		artifactStore: artifactBoundaryStore{
			downloadURL:       "https://objects.example.test/read?signature=trusted",
			downloadExpiresAt: expiresAt,
		},
	}
	download, err := repo.PresignGradingArtifactRead(
		context.Background(), workerID, leaseID, leaseToken, artifactID, leaseEpoch,
	)
	require.NoError(t, err)
	require.Equal(t, artifactID, download.ArtifactID)
	require.Equal(t, objectKey, download.ObjectKey)
	require.Equal(t, "https://objects.example.test/read?signature=trusted", download.DownloadURL)
	require.Equal(t, expiresAt, download.ExpiresAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPresignGradingArtifactReadAcceptsLegacyZeroLeaseEpoch(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	leaseID := uuid.New()
	workerID := uuid.New()
	artifactID := uuid.New()
	leaseToken := "legacy-grading-lease-token"
	objectKey := "evaluation-artifacts/run/sample/legacy-evidence.json"
	mock.ExpectQuery("FROM evaluation_grading_jobs g.*JOIN evaluation_artifacts ea").WithArgs(
		leaseID, hashToken(leaseToken), workerID, artifactID, int64(0),
	).WillReturnRows(sqlmock.NewRows([]string{
		"id", "object_key", "sha256", "byte_count", "mime_type",
	}).AddRow(
		artifactID,
		objectKey,
		"93c7b9b0bf8a4d5da9c943a1d3aa61fe5cfacdfe85b687cfb7a1f8bf17466207",
		int64(23),
		"application/json",
	))

	repo := &evaluationGradingRepository{
		db: db,
		artifactStore: artifactBoundaryStore{
			downloadURL:       "https://objects.example.test/read?signature=legacy",
			downloadExpiresAt: time.Date(2026, 8, 10, 0, 5, 0, 0, time.UTC),
		},
	}
	download, err := repo.PresignGradingArtifactRead(
		context.Background(), workerID, leaseID, leaseToken, artifactID, 0,
	)
	require.NoError(t, err)
	require.Equal(t, artifactID, download.ArtifactID)
	require.Equal(t, objectKey, download.ObjectKey)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestVerifyArtifactObjectMetadataRequiresExactStoredIdentity(t *testing.T) {
	expected := service.ArtifactObjectMetadata{
		ObjectKey: "evaluation-artifacts/run/sample/artifact",
		Bytes:     42,
		MIMEType:  "application/json",
		SHA256:    testArtifactSHA256,
	}
	require.NoError(t, verifyArtifactObjectMetadata(expected, expected))

	for name, actual := range map[string]service.ArtifactObjectMetadata{
		"key":   {ObjectKey: "evaluation-artifacts/run/sample/other", Bytes: 42, MIMEType: "application/json", SHA256: testArtifactSHA256},
		"bytes": {ObjectKey: expected.ObjectKey, Bytes: 41, MIMEType: expected.MIMEType, SHA256: expected.SHA256},
		"mime":  {ObjectKey: expected.ObjectKey, Bytes: expected.Bytes, MIMEType: "text/plain", SHA256: expected.SHA256},
		"sha":   {ObjectKey: expected.ObjectKey, Bytes: expected.Bytes, MIMEType: expected.MIMEType, SHA256: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, verifyArtifactObjectMetadata(expected, actual), service.ErrArtifactObjectMismatch)
		})
	}
}

func TestArtifactScanResultMapsTerminalStatusToStableErrors(t *testing.T) {
	clean := service.ArtifactScanResult{Status: service.ArtifactScanClean, Scanner: "clamav", ScannedAt: time.Now().UTC()}
	require.NoError(t, artifactScanResultError(clean))
	require.ErrorIs(t, artifactScanResultError(service.ArtifactScanResult{Status: service.ArtifactScanRejected}), service.ErrArtifactScanRejected)
	require.ErrorIs(t, artifactScanResultError(service.ArtifactScanResult{Status: service.ArtifactScanFailed}), service.ErrArtifactScanFailed)
	require.ErrorIs(t, artifactScanResultError(service.ArtifactScanResult{Status: "unknown"}), service.ErrArtifactScanFailed)
}
