//go:build integration

package repository

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type lifecycleArtifactStore struct {
	metadata service.ArtifactObjectMetadata
}

func (s *lifecycleArtifactStore) PresignPut(_ context.Context, request service.ArtifactObjectPutRequest, _ time.Duration) (*service.ArtifactObjectUpload, error) {
	s.metadata = service.ArtifactObjectMetadata{
		ObjectKey: request.ObjectKey,
		Bytes:     request.Bytes,
		MIMEType:  request.MIMEType,
		SHA256:    request.SHA256,
		ETag:      "test-etag",
	}
	return &service.ArtifactObjectUpload{URL: "https://objects.example.test/upload", Headers: map[string]string{"X-Amz-Meta-Sha256": request.SHA256}, ExpiresAt: time.Now().UTC().Add(time.Minute)}, nil
}

func (s *lifecycleArtifactStore) Head(context.Context, string) (*service.ArtifactObjectMetadata, error) {
	metadata := s.metadata
	return &metadata, nil
}

func (s *lifecycleArtifactStore) PresignGet(context.Context, string, time.Duration) (string, time.Time, error) {
	return "https://objects.example.test/read", time.Now().UTC().Add(time.Minute), nil
}

func (s *lifecycleArtifactStore) Delete(context.Context, string) error { return nil }

func (s *lifecycleArtifactStore) Open(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("artifact")), nil
}

type lifecycleArtifactScanner struct {
	result service.ArtifactScanResult
	err    error
}

func (s lifecycleArtifactScanner) Scan(context.Context, string, service.ArtifactObjectMetadata) (service.ArtifactScanResult, error) {
	return s.result, s.err
}

func TestEvaluationArtifactLifecycleConfirmsOnlyCleanScannedObject(t *testing.T) {
	ctx := context.Background()
	_, lease, _ := createOpenRouteEvidenceFixture(t)
	store := &lifecycleArtifactStore{}
	scanner := lifecycleArtifactScanner{result: service.ArtifactScanResult{
		Status: service.ArtifactScanClean, Scanner: "clamav", Reason: "stream: OK", ScannedAt: time.Now().UTC(),
	}}
	repo := NewEvaluationGradingRepositoryWithArtifactDependencies(integrationDB, store, scanner).(service.EvaluationArtifactRepository)

	upload, err := repo.PresignArtifact(ctx, lease.ID, lease.Token, service.ArtifactPresignRequest{
		MIMEType: "application/json", Bytes: 8, SHA256: testArtifactSHA256, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	receipt, err := repo.ConfirmArtifact(ctx, lease.ID, lease.Token, service.ArtifactConfirmation{
		ArtifactID: upload.ID, ObjectKey: upload.ObjectKey, SHA256: upload.SHA256, Bytes: upload.Bytes, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	require.Equal(t, "clean", receipt.ScanStatus)
	require.Equal(t, "clamav", receipt.Scanner)
	require.False(t, receipt.ConfirmedAt.IsZero())

	var status, provider, reason string
	var confirmedAt, scannedAt time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT scan_status, scan_provider, scan_reason, scanned_at, confirmed_at
		FROM evaluation_artifacts WHERE id=$1`, upload.ID).Scan(&status, &provider, &reason, &scannedAt, &confirmedAt))
	require.Equal(t, "clean", status)
	require.Equal(t, "clamav", provider)
	require.Equal(t, "stream: OK", reason)
	require.False(t, scannedAt.IsZero())
	require.False(t, confirmedAt.IsZero())
}

func TestEvaluationArtifactLifecyclePersistsRejectedScanWithoutConfirmation(t *testing.T) {
	ctx := context.Background()
	_, lease, _ := createOpenRouteEvidenceFixture(t)
	store := &lifecycleArtifactStore{}
	scanner := lifecycleArtifactScanner{result: service.ArtifactScanResult{
		Status: service.ArtifactScanRejected, Scanner: "clamav", Reason: "Eicar FOUND", ScannedAt: time.Now().UTC(),
	}}
	repo := NewEvaluationGradingRepositoryWithArtifactDependencies(integrationDB, store, scanner).(service.EvaluationArtifactRepository)
	upload, err := repo.PresignArtifact(ctx, lease.ID, lease.Token, service.ArtifactPresignRequest{
		MIMEType: "application/json", Bytes: 8, SHA256: testArtifactSHA256, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)

	receipt, err := repo.ConfirmArtifact(ctx, lease.ID, lease.Token, service.ArtifactConfirmation{
		ArtifactID: upload.ID, ObjectKey: upload.ObjectKey, SHA256: upload.SHA256, Bytes: upload.Bytes, LeaseEpoch: lease.LeaseEpoch,
	})
	require.ErrorIs(t, err, service.ErrArtifactScanRejected)
	require.Equal(t, "rejected", receipt.ScanStatus)

	var status string
	var confirmed bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT scan_status, confirmed_at IS NOT NULL FROM evaluation_artifacts WHERE id=$1`, upload.ID).Scan(&status, &confirmed))
	require.Equal(t, "rejected", status)
	require.False(t, confirmed)
}

func TestEvaluationArtifactLifecycleRejectsObjectMetadataMismatch(t *testing.T) {
	ctx := context.Background()
	_, lease, _ := createOpenRouteEvidenceFixture(t)
	store := &lifecycleArtifactStore{}
	repo := NewEvaluationGradingRepositoryWithArtifactDependencies(integrationDB, store, lifecycleArtifactScanner{result: service.ArtifactScanResult{Status: service.ArtifactScanClean}}).(service.EvaluationArtifactRepository)
	upload, err := repo.PresignArtifact(ctx, lease.ID, lease.Token, service.ArtifactPresignRequest{
		MIMEType: "application/json", Bytes: 8, SHA256: testArtifactSHA256, LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	store.metadata.Bytes++

	_, err = repo.ConfirmArtifact(ctx, lease.ID, lease.Token, service.ArtifactConfirmation{
		ArtifactID: upload.ID, ObjectKey: upload.ObjectKey, SHA256: upload.SHA256, Bytes: upload.Bytes, LeaseEpoch: lease.LeaseEpoch,
	})
	require.ErrorIs(t, err, service.ErrArtifactObjectMismatch)

	var status string
	var confirmed bool
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT scan_status, confirmed_at IS NOT NULL FROM evaluation_artifacts WHERE id=$1`, upload.ID).Scan(&status, &confirmed))
	require.Equal(t, "pending", status, fmt.Sprintf("unexpected status %s", status))
	require.False(t, confirmed)
}

func TestEvaluationArtifactCleanupSelectsExpiredStatusesAndMarksIdempotently(t *testing.T) {
	ctx := context.Background()
	fixture := createEvaluationRepositoryFixture(t, 1, []string{"route-a"}, 1)
	run, err := NewEvaluationRepository(integrationDB).CreateRunWithMatrix(ctx, service.CreateRunInput{
		PlanID: fixture.planID, TriggerSource: "manual", CreatedBy: fixture.userID,
	})
	require.NoError(t, err)
	var assignmentID, sampleID uuid.UUID
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT a.id, a.sample_id
		FROM evaluation_assignments a
		JOIN evaluation_samples s ON s.id = a.sample_id
		WHERE s.run_id = $1
		ORDER BY a.created_at, a.id
		LIMIT 1`, run.ID).Scan(&assignmentID, &sampleID))
	cutoff := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	expiredIDs := make(map[uuid.UUID]service.ArtifactScanStatus)
	for index, status := range []service.ArtifactScanStatus{
		service.ArtifactScanClean,
		"pending",
		service.ArtifactScanRejected,
		service.ArtifactScanFailed,
	} {
		artifactID := uuid.New()
		expiredIDs[artifactID] = status
		_, err := integrationDB.ExecContext(ctx, `
			INSERT INTO evaluation_artifacts (
				id, run_id, sample_id, assignment_id, object_key, sha256, byte_count,
				mime_type, scan_status, retention_deadline, confirmed_at
			) VALUES ($1, $2, $3, $4, $5, $6, 8, 'application/json', $7, $8, $9)`,
			artifactID, run.ID, sampleID, assignmentID, "cleanup/"+artifactID.String(),
			testArtifactSHA256, status, cutoff.Add(-time.Duration(index+1)*time.Hour),
			func() any {
				if status == service.ArtifactScanClean {
					return cutoff.Add(-2 * time.Hour)
				}
				return nil
			}())
		require.NoError(t, err)
	}

	futureID := uuid.New()
	deletedID := uuid.New()
	_, err = integrationDB.ExecContext(ctx, `
		INSERT INTO evaluation_artifacts (
			id, run_id, sample_id, assignment_id, object_key, sha256, byte_count,
			mime_type, scan_status, retention_deadline, deleted_at
		) VALUES
			($1, $3, $4, $5, $6, $7, 8, 'application/json', 'pending', $8, NULL),
			($2, $3, $4, $5, $9, $7, 8, 'application/json', 'rejected', $10, $11)`,
		futureID, deletedID, run.ID, sampleID, assignmentID,
		"cleanup/"+futureID.String(), testArtifactSHA256, cutoff.Add(time.Hour),
		"cleanup/"+deletedID.String(), cutoff.Add(-time.Hour), cutoff.Add(-30*time.Minute))
	require.NoError(t, err)

	repo := NewEvaluationArtifactCleanupRepository(integrationDB)
	candidates, err := repo.ListExpiredArtifacts(ctx, cutoff, 100)
	require.NoError(t, err)
	require.Len(t, candidates, len(expiredIDs))
	for _, candidate := range candidates {
		require.Equal(t, expiredIDs[candidate.ID], candidate.ScanStatus)
	}

	marked, err := repo.MarkArtifactDeleted(ctx, candidates[0], cutoff)
	require.NoError(t, err)
	require.True(t, marked)
	marked, err = repo.MarkArtifactDeleted(ctx, candidates[0], cutoff)
	require.NoError(t, err)
	require.False(t, marked)

	var deletedAt time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx, `
		SELECT deleted_at FROM evaluation_artifacts WHERE id = $1`, candidates[0].ID).Scan(&deletedAt))
	require.Equal(t, cutoff, deletedAt.UTC())
}
