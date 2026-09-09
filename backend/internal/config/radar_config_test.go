//go:build unit

package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadRadarConfigDefaults(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.Radar.Enabled)
	require.Equal(t, 900, cfg.Radar.MaxContextTTLSeconds)
	require.Equal(t, int64(2), cfg.Radar.WriterProtocolVersion)
	require.False(t, cfg.RadarArtifactStorage.Enabled)
	require.Equal(t, 900, cfg.RadarArtifactStorage.PresignExpiry)
	require.Equal(t, 300, cfg.RadarArtifactStorage.CleanupInterval)
	require.Equal(t, 100, cfg.RadarArtifactStorage.CleanupBatchSize)
}

func TestLoadRadarArtifactStorageFromEnvironment(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("RADAR_ARTIFACT_STORAGE_ENABLED", "true")
	t.Setenv("RADAR_ARTIFACT_STORAGE_ENDPOINT", "http://minio:9000")
	t.Setenv("RADAR_ARTIFACT_STORAGE_REGION", "us-east-1")
	t.Setenv("RADAR_ARTIFACT_STORAGE_BUCKET", "radar-artifacts")
	t.Setenv("RADAR_ARTIFACT_STORAGE_ACCESS_KEY_ID", "access")
	t.Setenv("RADAR_ARTIFACT_STORAGE_SECRET_ACCESS_KEY", "secret")
	t.Setenv("RADAR_ARTIFACT_STORAGE_PREFIX", "tenant-artifacts/")
	t.Setenv("RADAR_ARTIFACT_STORAGE_PRESIGN_EXPIRY_SECONDS", "600")
	t.Setenv("RADAR_ARTIFACT_STORAGE_CLAMAV_ADDRESS", "clamav:3310")
	t.Setenv("RADAR_ARTIFACT_STORAGE_CLEANUP_INTERVAL_SECONDS", "120")
	t.Setenv("RADAR_ARTIFACT_STORAGE_CLEANUP_BATCH_SIZE", "50")

	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.RadarArtifactStorage.Active())
	require.Equal(t, "radar-artifacts", cfg.RadarArtifactStorage.Bucket)
	require.Equal(t, 600, cfg.RadarArtifactStorage.PresignExpiry)
	require.Equal(t, 120, cfg.RadarArtifactStorage.CleanupInterval)
	require.Equal(t, 50, cfg.RadarArtifactStorage.CleanupBatchSize)
}

func TestLoadRejectsIncompleteEnabledRadarArtifactStorage(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("RADAR_ARTIFACT_STORAGE_ENABLED", "true")
	t.Setenv("RADAR_ARTIFACT_STORAGE_BUCKET", "radar-artifacts")

	_, err := Load()
	require.ErrorContains(t, err, "radar_artifact_storage")
}

func TestLoadRejectsInvalidRadarArtifactCleanupConfig(t *testing.T) {
	tests := []struct {
		name       string
		interval   string
		batchSize  string
		wantErrKey string
	}{
		{name: "interval too short", interval: "9", batchSize: "100", wantErrKey: "cleanup_interval_seconds"},
		{name: "interval too long", interval: "86401", batchSize: "100", wantErrKey: "cleanup_interval_seconds"},
		{name: "batch size zero", interval: "300", batchSize: "0", wantErrKey: "cleanup_batch_size"},
		{name: "batch size too large", interval: "300", batchSize: "1001", wantErrKey: "cleanup_batch_size"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("RADAR_ARTIFACT_STORAGE_ENABLED", "true")
			t.Setenv("RADAR_ARTIFACT_STORAGE_ENDPOINT", "http://minio:9000")
			t.Setenv("RADAR_ARTIFACT_STORAGE_BUCKET", "radar-artifacts")
			t.Setenv("RADAR_ARTIFACT_STORAGE_ACCESS_KEY_ID", "access")
			t.Setenv("RADAR_ARTIFACT_STORAGE_SECRET_ACCESS_KEY", "secret")
			t.Setenv("RADAR_ARTIFACT_STORAGE_CLAMAV_ADDRESS", "clamav:3310")
			t.Setenv("RADAR_ARTIFACT_STORAGE_CLEANUP_INTERVAL_SECONDS", test.interval)
			t.Setenv("RADAR_ARTIFACT_STORAGE_CLEANUP_BATCH_SIZE", test.batchSize)

			_, err := Load()
			require.ErrorContains(t, err, test.wantErrKey)
		})
	}
}

func TestLoadRejectsObsoleteRadarWriterProtocol(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("RADAR_WRITER_PROTOCOL_VERSION", "1")

	_, err := Load()
	require.ErrorContains(t, err, "radar.writer_protocol_version must be 2")
}

func TestLoadRejectsInvalidRadarWriterInstanceID(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("RADAR_WRITER_INSTANCE_ID", "not-a-uuid")

	_, err := Load()
	require.ErrorContains(t, err, "radar.writer_instance_id must be a UUID")
}

func TestLoadRadarConfigFromEnvironment(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("RADAR_ENABLED", "true")
	t.Setenv("RADAR_SIGNING_SECRET", strings.Repeat("s", 32))
	t.Setenv("RADAR_HASHING_SECRET", strings.Repeat("h", 32))
	t.Setenv("RADAR_MAX_CONTEXT_TTL_SECONDS", "600")
	t.Setenv("RADAR_REGION", "cn-east")
	t.Setenv("RADAR_ROUTE_PROFILE_VERSION", "route-v42")

	cfg, err := Load()
	require.NoError(t, err)
	require.True(t, cfg.Radar.Enabled)
	require.Equal(t, strings.Repeat("s", 32), cfg.Radar.SigningSecret)
	require.Equal(t, strings.Repeat("h", 32), cfg.Radar.HashingSecret)
	require.Equal(t, 600, cfg.Radar.MaxContextTTLSeconds)
	require.Equal(t, "cn-east", cfg.Radar.Region)
	require.Equal(t, "route-v42", cfg.Radar.RouteProfileVersion)
}

func TestLoadRadarConfigFromOperationalEnvironmentNames(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("RADAR_ENABLED", "true")
	t.Setenv("RADAR_CONTEXT_SIGNING_KEY", strings.Repeat("c", 32))
	t.Setenv("RADAR_EVIDENCE_HASH_KEY", strings.Repeat("e", 32))
	t.Setenv("RADAR_MAX_CONTEXT_TTL_SECONDS", "600")
	t.Setenv("RADAR_REGION", "cn-east")
	t.Setenv("RADAR_ROUTE_PROFILE_VERSION", "route-v42")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("c", 32), cfg.Radar.SigningSecret)
	require.Equal(t, strings.Repeat("e", 32), cfg.Radar.HashingSecret)
}

func TestLoadRadarConfigOperationalNamesTakePrecedenceOverLegacyNames(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("RADAR_ENABLED", "true")
	t.Setenv("RADAR_CONTEXT_SIGNING_KEY", strings.Repeat("c", 32))
	t.Setenv("RADAR_SIGNING_SECRET", strings.Repeat("s", 32))
	t.Setenv("RADAR_EVIDENCE_HASH_KEY", strings.Repeat("e", 32))
	t.Setenv("RADAR_HASHING_SECRET", strings.Repeat("h", 32))
	t.Setenv("RADAR_REGION", "cn-east")
	t.Setenv("RADAR_ROUTE_PROFILE_VERSION", "route-v42")

	cfg, err := Load()
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("c", 32), cfg.Radar.SigningSecret)
	require.Equal(t, strings.Repeat("e", 32), cfg.Radar.HashingSecret)
}

func TestLoadRejectsInvalidEnabledRadarConfig(t *testing.T) {
	tests := []struct {
		name    string
		signing string
		hashing string
		ttl     string
		wantErr string
	}{
		{name: "missing signing secret", hashing: strings.Repeat("h", 32), ttl: "900", wantErr: "radar.signing_secret"},
		{name: "short signing secret", signing: strings.Repeat("s", 31), hashing: strings.Repeat("h", 32), ttl: "900", wantErr: "radar.signing_secret"},
		{name: "missing hashing secret", signing: strings.Repeat("s", 32), ttl: "900", wantErr: "radar.hashing_secret"},
		{name: "short hashing secret", signing: strings.Repeat("s", 32), hashing: strings.Repeat("h", 31), ttl: "900", wantErr: "radar.hashing_secret"},
		{name: "zero TTL", signing: strings.Repeat("s", 32), hashing: strings.Repeat("h", 32), ttl: "0", wantErr: "radar.max_context_ttl_seconds"},
		{name: "TTL over hard limit", signing: strings.Repeat("s", 32), hashing: strings.Repeat("h", 32), ttl: "901", wantErr: "radar.max_context_ttl_seconds"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("RADAR_ENABLED", "true")
			t.Setenv("RADAR_SIGNING_SECRET", test.signing)
			t.Setenv("RADAR_HASHING_SECRET", test.hashing)
			t.Setenv("RADAR_MAX_CONTEXT_TTL_SECONDS", test.ttl)

			_, err := Load()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestLoadRejectsEnabledRadarWithoutRouteIdentity(t *testing.T) {
	tests := []struct {
		name         string
		region       string
		routeProfile string
		wantErr      string
	}{
		{name: "missing region", routeProfile: "route-v42", wantErr: "radar.region"},
		{name: "missing route profile", region: "cn-east", wantErr: "radar.route_profile_version"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("RADAR_ENABLED", "true")
			t.Setenv("RADAR_SIGNING_SECRET", strings.Repeat("s", 32))
			t.Setenv("RADAR_HASHING_SECRET", strings.Repeat("h", 32))
			t.Setenv("RADAR_REGION", test.region)
			t.Setenv("RADAR_ROUTE_PROFILE_VERSION", test.routeProfile)

			_, err := Load()
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}
