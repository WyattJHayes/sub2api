package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

const (
	routeEvidenceTerminalizationTimerName    = "radar:route-evidence-terminalization"
	routeEvidenceTerminalizationBatchSize    = 100
	routeEvidenceTerminalizationPollInterval = time.Minute
)

var ErrRouteEvidenceTerminalizationUnavailable = errors.New("route evidence terminalization runtime is unavailable")

type RouteEvidenceTerminalizationResult struct {
	Selected  int
	Sealed    int
	Processed int
	Failed    int
}

type RouteEvidenceTerminalizationScheduler interface {
	ScheduleRecurring(name string, interval time.Duration, callback func())
	Cancel(name string)
}

// RouteEvidenceTerminalizationRuntime consumes terminal run outbox events
// through the system finalizer, which owns all state changes and transactions.
type RouteEvidenceTerminalizationRuntime struct {
	repo      RouteEvidenceTerminalizationRepository
	finalizer TrustedEvaluationEvidenceFinalizer
	scheduler RouteEvidenceTerminalizationScheduler
	interval  time.Duration
	batchSize int
	workerCtx context.Context
	cancel    context.CancelFunc

	running   int32
	startOnce sync.Once
	stopOnce  sync.Once
	mu        sync.Mutex
	stopped   bool
	wg        sync.WaitGroup
}

func NewRouteEvidenceTerminalizationRuntime(repo RouteEvidenceTerminalizationRepository, finalizer TrustedEvaluationEvidenceFinalizer, interval time.Duration, batchSize int) *RouteEvidenceTerminalizationRuntime {
	workerCtx, cancel := context.WithCancel(context.Background())
	return &RouteEvidenceTerminalizationRuntime{
		repo: repo, finalizer: finalizer, interval: interval, batchSize: batchSize, workerCtx: workerCtx, cancel: cancel,
	}
}

func (s *RouteEvidenceTerminalizationRuntime) SetScheduler(scheduler RouteEvidenceTerminalizationScheduler) {
	if s != nil {
		s.scheduler = scheduler
	}
}

func (s *RouteEvidenceTerminalizationRuntime) Start() {
	if s == nil || s.repo == nil || s.finalizer == nil || s.scheduler == nil {
		return
	}
	s.startOnce.Do(func() {
		s.scheduler.ScheduleRecurring(routeEvidenceTerminalizationTimerName, s.effectiveInterval(), s.trigger)
		s.trigger()
	})
}

func (s *RouteEvidenceTerminalizationRuntime) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		s.mu.Lock()
		s.stopped = true
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
		if s.scheduler != nil {
			s.scheduler.Cancel(routeEvidenceTerminalizationTimerName)
		}
		s.wg.Wait()
	})
}

func (s *RouteEvidenceTerminalizationRuntime) ProcessPending(ctx context.Context) (RouteEvidenceTerminalizationResult, error) {
	var result RouteEvidenceTerminalizationResult
	if s == nil || s.repo == nil || s.finalizer == nil {
		return result, ErrRouteEvidenceTerminalizationUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	events, err := s.repo.ListPendingTerminalizations(ctx, s.effectiveBatchSize())
	if err != nil {
		result.Failed++
		return result, fmt.Errorf("list route evidence terminalizations: %w", err)
	}
	result.Selected = len(events)
	failures := make([]error, 0)
	for _, event := range events {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(append(failures, err)...)
		}
		sealed, err := s.finalizer.FinalizeRouteEvidenceFromTerminalization(ctx, FinalizeRouteEvidenceFromTerminalizationInput{
			EventID: event.ID, RunID: event.RunID, ControlEpoch: event.ControlEpoch,
		})
		if err != nil {
			result.Failed++
			failures = append(failures, fmt.Errorf("finalize route evidence terminalization: %w", err))
			continue
		}
		result.Sealed += sealed
		result.Processed++
	}
	return result, errors.Join(failures...)
}

func (s *RouteEvidenceTerminalizationRuntime) trigger() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		s.runOnce()
	}()
}

func (s *RouteEvidenceTerminalizationRuntime) runOnce() {
	if s == nil || !atomic.CompareAndSwapInt32(&s.running, 0, 1) {
		return
	}
	defer atomic.StoreInt32(&s.running, 0)
	ctx, cancel := context.WithTimeout(s.workerCtx, s.effectiveInterval())
	defer cancel()
	result, err := s.ProcessPending(ctx)
	logRouteEvidenceTerminalization(
		logger.With(zap.String("component", "service.evaluation_terminalization")),
		result,
		err,
	)
}

func logRouteEvidenceTerminalization(log *zap.Logger, result RouteEvidenceTerminalizationResult, err error) {
	if log == nil {
		log = zap.NewNop()
	}
	fields := []zap.Field{
		zap.Int("selected", result.Selected),
		zap.Int("sealed", result.Sealed),
		zap.Int("processed", result.Processed),
		zap.Int("failed", result.Failed),
	}
	if err != nil {
		log.Error("radar route evidence terminalization failed", append(fields, zap.Error(err))...)
		return
	}
	log.Info("radar route evidence terminalization completed", fields...)
}

func (s *RouteEvidenceTerminalizationRuntime) effectiveInterval() time.Duration {
	if s == nil || s.interval <= 0 {
		return routeEvidenceTerminalizationPollInterval
	}
	return s.interval
}

func (s *RouteEvidenceTerminalizationRuntime) effectiveBatchSize() int {
	if s == nil || s.batchSize < 1 || s.batchSize > routeEvidenceTerminalizationBatchSize {
		return routeEvidenceTerminalizationBatchSize
	}
	return s.batchSize
}

func ProvideRouteEvidenceTerminalizationRuntime(repo EvaluationEvidenceRepository, scheduler RouteEvidenceTerminalizationScheduler, cfg *config.Config) *RouteEvidenceTerminalizationRuntime {
	terminalizationRepo, implementsTerminalization := repo.(RouteEvidenceTerminalizationRepository)
	finalizer, implementsFinalizer := repo.(TrustedEvaluationEvidenceFinalizer)
	if !implementsTerminalization {
		terminalizationRepo = nil
	}
	runtime := NewRouteEvidenceTerminalizationRuntime(terminalizationRepo, finalizer, routeEvidenceTerminalizationPollInterval, routeEvidenceTerminalizationBatchSize)
	runtime.SetScheduler(scheduler)
	if cfg != nil && cfg.Radar.Enabled && implementsTerminalization && implementsFinalizer {
		runtime.Start()
	}
	return runtime
}
