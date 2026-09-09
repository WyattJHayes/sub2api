//go:build integration

package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestEvaluationArtifactLiveE2EAcceptsCleanAndRejectsEICAR(t *testing.T) {
	if os.Getenv("RADAR_ARTIFACT_LIVE_E2E") != "1" {
		t.Skip("set RADAR_ARTIFACT_LIVE_E2E=1 to exercise live MinIO and ClamAV")
	}

	storageConfig := &config.RadarArtifactStorageConfig{
		Enabled:         true,
		Endpoint:        requiredLiveArtifactEnv(t, "RADAR_ARTIFACT_STORAGE_ENDPOINT"),
		Region:          requiredLiveArtifactEnv(t, "RADAR_ARTIFACT_STORAGE_REGION"),
		Bucket:          requiredLiveArtifactEnv(t, "RADAR_ARTIFACT_STORAGE_BUCKET"),
		AccessKeyID:     requiredLiveArtifactEnv(t, "RADAR_ARTIFACT_STORAGE_ACCESS_KEY_ID"),
		SecretAccessKey: requiredLiveArtifactEnv(t, "RADAR_ARTIFACT_STORAGE_SECRET_ACCESS_KEY"),
		ForcePathStyle:  true,
		Prefix:          "evaluation-artifacts/",
		PresignExpiry:   300,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	store, err := NewS3EvaluationArtifactObjectStore(ctx, storageConfig)
	require.NoError(t, err)
	scanner, err := NewClamAVArtifactScanner(
		store,
		requiredLiveArtifactEnv(t, "RADAR_ARTIFACT_STORAGE_CLAMAV_ADDRESS"),
		90*time.Second,
	)
	require.NoError(t, err)

	t.Run("clean artifact", func(t *testing.T) {
		payload := []byte(`{"output":"pong","status":"clean"}`)
		receipt := uploadAndConfirmLiveArtifact(t, ctx, store, scanner, payload, "application/json")
		require.Equal(t, string(service.ArtifactScanClean), receipt.ScanStatus)
		require.Equal(t, "clamav", receipt.Scanner)
		require.False(t, receipt.ConfirmedAt.IsZero())

		downloadURL, _, err := store.PresignGet(ctx, receipt.ObjectKey, time.Minute)
		require.NoError(t, err)
		response, err := http.Get(downloadURL)
		require.NoError(t, err)
		defer response.Body.Close()
		require.Equal(t, http.StatusOK, response.StatusCode)
		downloaded, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, payload, downloaded)
	})

	t.Run("EICAR artifact", func(t *testing.T) {
		payload := []byte(`X5O!P%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*`)
		receipt, err := uploadAndConfirmLiveArtifactResult(t, ctx, store, scanner, payload, "application/octet-stream")
		require.ErrorIs(t, err, service.ErrArtifactScanRejected)
		require.Equal(t, string(service.ArtifactScanRejected), receipt.ScanStatus)
		require.Equal(t, "clamav", receipt.Scanner)
		require.Contains(t, receipt.ScanReason, "FOUND")
		require.True(t, receipt.ConfirmedAt.IsZero())
	})
}

func uploadAndConfirmLiveArtifact(
	t *testing.T,
	ctx context.Context,
	store service.EvaluationArtifactObjectStore,
	scanner service.ArtifactScanner,
	payload []byte,
	mimeType string,
) *service.ArtifactReceipt {
	t.Helper()
	receipt, err := uploadAndConfirmLiveArtifactResult(t, ctx, store, scanner, payload, mimeType)
	require.NoError(t, err)
	return receipt
}

func uploadAndConfirmLiveArtifactResult(
	t *testing.T,
	ctx context.Context,
	store service.EvaluationArtifactObjectStore,
	scanner service.ArtifactScanner,
	payload []byte,
	mimeType string,
) (*service.ArtifactReceipt, error) {
	t.Helper()
	_, lease, _ := createOpenRouteEvidenceFixture(t)
	repo := NewEvaluationGradingRepositoryWithArtifactDependencies(integrationDB, store, scanner).(service.EvaluationArtifactRepository)
	digest := fmt.Sprintf("%x", sha256.Sum256(payload))
	upload, err := repo.PresignArtifact(ctx, lease.ID, lease.Token, service.ArtifactPresignRequest{
		MIMEType:   mimeType,
		Bytes:      int64(len(payload)),
		SHA256:     digest,
		LeaseEpoch: lease.LeaseEpoch,
	})
	require.NoError(t, err)
	uploaded := false
	t.Cleanup(func() {
		if uploaded {
			require.NoError(t, store.Delete(context.Background(), upload.ObjectKey))
		}
	})

	request, err := http.NewRequestWithContext(ctx, http.MethodPut, upload.UploadURL, bytes.NewReader(payload))
	require.NoError(t, err)
	request.ContentLength = int64(len(payload))
	for name, value := range upload.UploadHeaders {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 8*1024))
	require.NoError(t, err)
	require.Equalf(t, http.StatusOK, response.StatusCode, "object upload failed: %s", strings.TrimSpace(string(responseBody)))
	uploaded = true

	return repo.ConfirmArtifact(ctx, lease.ID, lease.Token, service.ArtifactConfirmation{
		ArtifactID: upload.ID,
		ObjectKey:  upload.ObjectKey,
		SHA256:     upload.SHA256,
		Bytes:      upload.Bytes,
		LeaseEpoch: lease.LeaseEpoch,
	})
}

func requiredLiveArtifactEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	require.NotEmpty(t, value, "%s is required for live artifact E2E", name)
	return value
}
