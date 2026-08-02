package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type artifactCleanupRepositoryStub struct {
	candidates []ArtifactCleanupCandidate
	listErr    error
	markErr    map[uuid.UUID]error
	marked     []uuid.UUID
	operations *[]string
}

func (s *artifactCleanupRepositoryStub) ListExpiredArtifacts(context.Context, time.Time, int) ([]ArtifactCleanupCandidate, error) {
	return append([]ArtifactCleanupCandidate(nil), s.candidates...), s.listErr
}

func (s *artifactCleanupRepositoryStub) MarkArtifactDeleted(_ context.Context, candidate ArtifactCleanupCandidate, _ time.Time) (bool, error) {
	if s.operations != nil {
		*s.operations = append(*s.operations, "mark:"+candidate.ID.String())
	}
	if err := s.markErr[candidate.ID]; err != nil {
		return false, err
	}
	s.marked = append(s.marked, candidate.ID)
	return true, nil
}

type artifactCleanupStoreStub struct {
	deleteErr  map[string]error
	operations *[]string
}

func (s *artifactCleanupStoreStub) Delete(_ context.Context, objectKey string) error {
	if s.operations != nil {
		*s.operations = append(*s.operations, "delete:"+objectKey)
	}
	return s.deleteErr[objectKey]
}

type artifactCleanupSchedulerStub struct {
	name     string
	interval time.Duration
	callback func()
	canceled string
}

func (s *artifactCleanupSchedulerStub) ScheduleRecurring(name string, interval time.Duration, callback func()) {
	s.name = name
	s.interval = interval
	s.callback = callback
}

func (s *artifactCleanupSchedulerStub) Cancel(name string) {
	s.canceled = name
}

func TestEvaluationArtifactCleanupDeletesExpiredStatusesBeforeMarkingRows(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	statuses := []ArtifactScanStatus{
		ArtifactScanClean,
		"pending",
		ArtifactScanRejected,
		ArtifactScanFailed,
	}
	candidates := make([]ArtifactCleanupCandidate, 0, len(statuses))
	operations := make([]string, 0, len(statuses)*2)
	for index, status := range statuses {
		id := uuid.New()
		candidates = append(candidates, ArtifactCleanupCandidate{
			ID:                id,
			ObjectKey:         "artifacts/" + id.String(),
			ScanStatus:        status,
			RetentionDeadline: now.Add(-time.Duration(index+1) * time.Hour),
		})
	}
	repo := &artifactCleanupRepositoryStub{candidates: candidates, operations: &operations}
	store := &artifactCleanupStoreStub{operations: &operations}
	cleanup := NewEvaluationArtifactCleanupService(repo, store, time.Minute, 100)
	cleanup.now = func() time.Time { return now }

	result, err := cleanup.CleanupExpired(context.Background())

	require.NoError(t, err)
	require.Equal(t, ArtifactCleanupResult{Selected: 4, Deleted: 4}, result)
	require.Len(t, repo.marked, 4)
	for index, candidate := range candidates {
		require.Equal(t, "delete:"+candidate.ObjectKey, operations[index*2])
		require.Equal(t, "mark:"+candidate.ID.String(), operations[index*2+1])
	}
}

func TestEvaluationArtifactCleanupKeepsRowWhenObjectDeleteFails(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	failed := ArtifactCleanupCandidate{ID: uuid.New(), ObjectKey: "artifacts/failed", RetentionDeadline: now.Add(-time.Hour)}
	succeeded := ArtifactCleanupCandidate{ID: uuid.New(), ObjectKey: "artifacts/succeeded", RetentionDeadline: now.Add(-time.Hour)}
	repo := &artifactCleanupRepositoryStub{candidates: []ArtifactCleanupCandidate{failed, succeeded}}
	store := &artifactCleanupStoreStub{deleteErr: map[string]error{failed.ObjectKey: errors.New("store unavailable")}}
	cleanup := NewEvaluationArtifactCleanupService(repo, store, time.Minute, 100)
	cleanup.now = func() time.Time { return now }

	result, err := cleanup.CleanupExpired(context.Background())

	require.ErrorContains(t, err, "store unavailable")
	require.Equal(t, ArtifactCleanupResult{Selected: 2, Deleted: 1, Failed: 1}, result)
	require.Equal(t, []uuid.UUID{succeeded.ID}, repo.marked)
}

func TestEvaluationArtifactCleanupMarksMissingObjectDeleted(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	candidate := ArtifactCleanupCandidate{ID: uuid.New(), ObjectKey: "artifacts/missing", RetentionDeadline: now.Add(-time.Hour)}
	repo := &artifactCleanupRepositoryStub{candidates: []ArtifactCleanupCandidate{candidate}}
	store := &artifactCleanupStoreStub{deleteErr: map[string]error{candidate.ObjectKey: ErrArtifactNotFound}}
	cleanup := NewEvaluationArtifactCleanupService(repo, store, time.Minute, 100)
	cleanup.now = func() time.Time { return now }

	result, err := cleanup.CleanupExpired(context.Background())

	require.NoError(t, err)
	require.Equal(t, ArtifactCleanupResult{Selected: 1, Deleted: 1}, result)
	require.Equal(t, []uuid.UUID{candidate.ID}, repo.marked)
}

func TestEvaluationArtifactCleanupRejectsMissingDependencies(t *testing.T) {
	result, err := NewEvaluationArtifactCleanupService(nil, nil, time.Minute, 100).CleanupExpired(context.Background())

	require.ErrorIs(t, err, ErrArtifactObjectStoreUnavailable)
	require.Zero(t, result)
}

func TestEvaluationArtifactCleanupSchedulesAndStopsRecurringCleanup(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	candidate := ArtifactCleanupCandidate{ID: uuid.New(), ObjectKey: "artifacts/scheduled", RetentionDeadline: now.Add(-time.Hour)}
	repo := &artifactCleanupRepositoryStub{candidates: []ArtifactCleanupCandidate{candidate}}
	store := &artifactCleanupStoreStub{}
	scheduler := &artifactCleanupSchedulerStub{}
	cleanup := NewEvaluationArtifactCleanupService(repo, store, 5*time.Minute, 25)
	cleanup.now = func() time.Time { return now }
	cleanup.SetScheduler(scheduler)

	cleanup.Start()

	require.Equal(t, "radar:artifact-cleanup", scheduler.name)
	require.Equal(t, 5*time.Minute, scheduler.interval)
	require.NotNil(t, scheduler.callback)
	scheduler.callback()
	require.Equal(t, []uuid.UUID{candidate.ID}, repo.marked)

	cleanup.Stop()
	require.Equal(t, scheduler.name, scheduler.canceled)
}

func TestProvideEvaluationArtifactCleanupServiceUsesEnabledConfig(t *testing.T) {
	repo := &artifactCleanupRepositoryStub{}
	store := &artifactCleanupStoreStub{}
	scheduler := &artifactCleanupSchedulerStub{}
	cfg := &config.Config{RadarArtifactStorage: config.RadarArtifactStorageConfig{
		Enabled:          true,
		Bucket:           "radar-artifacts",
		AccessKeyID:      "access",
		SecretAccessKey:  "secret",
		CleanupInterval:  120,
		CleanupBatchSize: 50,
	}}

	cleanup := ProvideEvaluationArtifactCleanupService(repo, store, scheduler, cfg)

	require.Equal(t, 120*time.Second, cleanup.interval)
	require.Equal(t, 50, cleanup.batchSize)
	require.Equal(t, artifactCleanupTimerName, scheduler.name)
	require.Equal(t, 120*time.Second, scheduler.interval)
	cleanup.Stop()
}

func TestProvideEvaluationArtifactCleanupServiceDoesNotScheduleWhenDisabled(t *testing.T) {
	scheduler := &artifactCleanupSchedulerStub{}

	cleanup := ProvideEvaluationArtifactCleanupService(&artifactCleanupRepositoryStub{}, &artifactCleanupStoreStub{}, scheduler, &config.Config{})

	require.Empty(t, scheduler.name)
	cleanup.Stop()
}
