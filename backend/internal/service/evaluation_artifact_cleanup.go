package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
)

var ErrArtifactCleanupUnavailable = errors.New("evaluation artifact cleanup unavailable")

const artifactCleanupTimerName = "radar:artifact-cleanup"

type ArtifactCleanupCandidate struct {
	ID                uuid.UUID
	ObjectKey         string
	ScanStatus        ArtifactScanStatus
	RetentionDeadline time.Time
}

type ArtifactCleanupResult struct {
	Selected int
	Deleted  int
	Skipped  int
	Failed   int
}

type EvaluationArtifactCleanupRepository interface {
	ListExpiredArtifacts(ctx context.Context, before time.Time, limit int) ([]ArtifactCleanupCandidate, error)
	MarkArtifactDeleted(ctx context.Context, candidate ArtifactCleanupCandidate, deletedAt time.Time) (bool, error)
}

type ArtifactObjectDeleter interface {
	Delete(ctx context.Context, objectKey string) error
}

type ArtifactCleanupScheduler interface {
	ScheduleRecurring(name string, interval time.Duration, callback func())
	Cancel(name string)
}

type EvaluationArtifactCleanupService struct {
	repo      EvaluationArtifactCleanupRepository
	store     ArtifactObjectDeleter
	scheduler ArtifactCleanupScheduler
	interval  time.Duration
	batchSize int
	now       func() time.Time
	workerCtx context.Context
	cancel    context.CancelFunc
	running   int32
	startOnce sync.Once
	stopOnce  sync.Once
}

func NewEvaluationArtifactCleanupService(repo EvaluationArtifactCleanupRepository, store ArtifactObjectDeleter, interval time.Duration, batchSize int) *EvaluationArtifactCleanupService {
	workerCtx, cancel := context.WithCancel(context.Background())
	return &EvaluationArtifactCleanupService{
		repo:      repo,
		store:     store,
		interval:  interval,
		batchSize: batchSize,
		now:       func() time.Time { return time.Now().UTC() },
		workerCtx: workerCtx,
		cancel:    cancel,
	}
}

func (s *EvaluationArtifactCleanupService) SetScheduler(scheduler ArtifactCleanupScheduler) {
	if s != nil {
		s.scheduler = scheduler
	}
}

func (s *EvaluationArtifactCleanupService) Start() {
	if s == nil || s.repo == nil || s.store == nil || s.scheduler == nil {
		return
	}
	s.startOnce.Do(func() {
		s.scheduler.ScheduleRecurring(artifactCleanupTimerName, s.effectiveInterval(), s.runOnce)
	})
}

func (s *EvaluationArtifactCleanupService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.scheduler != nil {
			s.scheduler.Cancel(artifactCleanupTimerName)
		}
	})
}

func (s *EvaluationArtifactCleanupService) effectiveInterval() time.Duration {
	if s.interval <= 0 {
		return 5 * time.Minute
	}
	return s.interval
}

func (s *EvaluationArtifactCleanupService) runOnce() {
	if s == nil || !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&s.running, 0)
	ctx, cancel := context.WithTimeout(s.workerCtx, s.effectiveInterval())
	defer cancel()
	result, err := s.CleanupExpired(ctx)
	if err != nil {
		logger.LegacyPrintf("service.evaluation_artifact_cleanup", "[RadarArtifactCleanup] selected=%d deleted=%d skipped=%d failed=%d err=%v", result.Selected, result.Deleted, result.Skipped, result.Failed, err)
		return
	}
	logger.LegacyPrintf("service.evaluation_artifact_cleanup", "[RadarArtifactCleanup] selected=%d deleted=%d skipped=%d failed=%d", result.Selected, result.Deleted, result.Skipped, result.Failed)
}

func (s *EvaluationArtifactCleanupService) CleanupExpired(ctx context.Context) (ArtifactCleanupResult, error) {
	var result ArtifactCleanupResult
	if s == nil || s.store == nil {
		return result, ErrArtifactObjectStoreUnavailable
	}
	if s.repo == nil {
		return result, ErrArtifactCleanupUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	batchSize := s.batchSize
	if batchSize <= 0 {
		batchSize = 100
	}
	cutoff := s.now().UTC()
	candidates, err := s.repo.ListExpiredArtifacts(ctx, cutoff, batchSize)
	if err != nil {
		return result, fmt.Errorf("list expired evaluation artifacts: %w", err)
	}
	result.Selected = len(candidates)
	errorsByArtifact := make([]error, 0)
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(errorsByArtifact, err)...)
		}
		if candidate.ID == uuid.Nil || strings.TrimSpace(candidate.ObjectKey) == "" {
			result.Failed++
			errorsByArtifact = append(errorsByArtifact, fmt.Errorf("invalid cleanup candidate %s", candidate.ID))
			continue
		}
		if err := s.store.Delete(ctx, candidate.ObjectKey); err != nil && !errors.Is(err, ErrArtifactNotFound) {
			result.Failed++
			errorsByArtifact = append(errorsByArtifact, fmt.Errorf("delete evaluation artifact %s: %w", candidate.ID, err))
			continue
		}
		marked, err := s.repo.MarkArtifactDeleted(ctx, candidate, cutoff)
		if err != nil {
			result.Failed++
			errorsByArtifact = append(errorsByArtifact, fmt.Errorf("mark evaluation artifact %s deleted: %w", candidate.ID, err))
			continue
		}
		if !marked {
			result.Skipped++
			continue
		}
		result.Deleted++
	}
	return result, errors.Join(errorsByArtifact...)
}
