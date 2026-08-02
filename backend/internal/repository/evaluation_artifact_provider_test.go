package repository

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func activeArtifactStorageConfig() *config.Config {
	return &config.Config{RadarArtifactStorage: config.RadarArtifactStorageConfig{
		Enabled:         true,
		Bucket:          "radar-artifacts",
		AccessKeyID:     "access",
		SecretAccessKey: "secret",
		ScanMode:        "clamav",
	}}
}

func TestProvideEvaluationGradingRepositoryAllowsDisabledArtifactStorage(t *testing.T) {
	repo, err := ProvideEvaluationGradingRepository(nil, &config.Config{}, nil, nil)

	require.NoError(t, err)
	concrete, ok := repo.(*evaluationGradingRepository)
	require.True(t, ok)
	require.Nil(t, concrete.artifactStore)
	require.Nil(t, concrete.artifactScan)
}

func TestProvideEvaluationGradingRepositoryRejectsMissingRequiredArtifactDependencies(t *testing.T) {
	cfg := &config.Config{Radar: config.RadarConfig{Enabled: true}}

	_, err := ProvideEvaluationGradingRepository(nil, cfg, nil, nil)

	require.ErrorIs(t, err, service.ErrArtifactObjectStoreUnavailable)
}

func TestProvideEvaluationGradingRepositoryReusesArtifactDependencies(t *testing.T) {
	cfg := activeArtifactStorageConfig()
	store := &artifactBoundaryStore{}
	scanner := &artifactBoundaryScanner{beforeScan: func() error { return nil }}

	repo, err := ProvideEvaluationGradingRepository(nil, cfg, store, scanner)

	require.NoError(t, err)
	concrete, ok := repo.(*evaluationGradingRepository)
	require.True(t, ok)
	require.Same(t, store, concrete.artifactStore)
	require.Same(t, scanner, concrete.artifactScan)
}
