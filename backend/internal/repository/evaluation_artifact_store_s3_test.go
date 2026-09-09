package repository

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

const testArtifactSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testRadarArtifactStorageConfig(endpoint string) *config.RadarArtifactStorageConfig {
	return &config.RadarArtifactStorageConfig{
		Enabled:         true,
		Endpoint:        endpoint,
		Region:          "us-east-1",
		Bucket:          "radar-artifacts",
		AccessKeyID:     "access",
		SecretAccessKey: "secret",
		ForcePathStyle:  true,
		Prefix:          "evaluation-artifacts/",
		PresignExpiry:   int((15 * time.Minute) / time.Second),
	}
}

func TestS3EvaluationArtifactObjectStorePresignPutIncludesIntegrityHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)

	store, err := NewS3EvaluationArtifactObjectStore(context.Background(), testRadarArtifactStorageConfig(server.URL))
	require.NoError(t, err)

	upload, err := store.PresignPut(context.Background(), service.ArtifactObjectPutRequest{
		ObjectKey: "run/sample/artifact",
		Bytes:     42,
		MIMEType:  "application/json",
		SHA256:    testArtifactSHA256,
	}, time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, upload.URL)
	require.Equal(t, "application/json", upload.Headers["Content-Type"])
	require.Equal(t, "42", upload.Headers["Content-Length"])
	require.Equal(t, testArtifactSHA256, upload.Headers["X-Amz-Meta-Sha256"])
	require.Equal(t, "ASNFZ4mrze8BI0VniavN7wEjRWeJq83vASNFZ4mrze8=", upload.Headers["X-Amz-Checksum-Sha256"])
	require.Equal(t, "*", upload.Headers["If-None-Match"])
	require.NotZero(t, upload.ExpiresAt)
	require.Contains(t, upload.URL, "X-Amz-Signature=")
}

func TestS3EvaluationArtifactObjectStoreHeadNormalizesMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodHead, r.Method)
		require.Equal(t, "/radar-artifacts/evaluation-artifacts/run/sample/artifact", r.URL.Path)
		w.Header().Set("Content-Length", "42")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"artifact-etag"`)
		w.Header().Set("X-Amz-Meta-Sha256", testArtifactSHA256)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	store, err := NewS3EvaluationArtifactObjectStore(context.Background(), testRadarArtifactStorageConfig(server.URL))
	require.NoError(t, err)

	metadata, err := store.Head(context.Background(), "run/sample/artifact")
	require.NoError(t, err)
	require.Equal(t, int64(42), metadata.Bytes)
	require.Equal(t, "application/json", metadata.MIMEType)
	require.Equal(t, testArtifactSHA256, metadata.SHA256)
	require.Equal(t, "artifact-etag", metadata.ETag)
}

func TestS3EvaluationArtifactObjectStoreRejectsMissingIntegrityMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "42")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	store, err := NewS3EvaluationArtifactObjectStore(context.Background(), testRadarArtifactStorageConfig(server.URL))
	require.NoError(t, err)

	_, err = store.Head(context.Background(), "run/sample/artifact")
	require.ErrorIs(t, err, service.ErrArtifactObjectMetadataUnavailable)
}

func TestS3EvaluationArtifactObjectStorePresignGetAndDelete(t *testing.T) {
	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	t.Cleanup(server.Close)

	store, err := NewS3EvaluationArtifactObjectStore(context.Background(), testRadarArtifactStorageConfig(server.URL))
	require.NoError(t, err)

	url, expiresAt, err := store.PresignGet(context.Background(), "run/sample/artifact", time.Minute)
	require.NoError(t, err)
	require.Contains(t, url, "X-Amz-Signature=")
	require.True(t, expiresAt.After(time.Now()))

	require.NoError(t, store.Delete(context.Background(), "run/sample/artifact"))
	require.True(t, deleted)
}
