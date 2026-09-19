//go:build unit

package handler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type seedanceDurableTaskRepoStub struct {
	service.AsyncVideoBillingTaskRepository

	mu            sync.Mutex
	recorder      *httptest.ResponseRecorder
	errors        []error
	creates       []service.CreateAsyncVideoBillingTaskInput
	bodyLengths   []int
	existing      *service.AsyncVideoBillingTask
	owned         *service.AsyncVideoBillingTask
	getOwnedErr   error
	getOwnedCalls []seedanceGetOwnedCall
}

type seedanceGetOwnedCall struct {
	provider, upstreamTaskID string
	userID, apiKeyID         int64
}

func (s *seedanceDurableTaskRepoStub) Create(_ context.Context, input service.CreateAsyncVideoBillingTaskInput) (*service.AsyncVideoBillingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates = append(s.creates, input)
	if s.recorder != nil {
		s.bodyLengths = append(s.bodyLengths, s.recorder.Body.Len())
	}
	if len(s.errors) > 0 {
		err := s.errors[0]
		s.errors = s.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	if s.existing != nil {
		copy := *s.existing
		return &copy, nil
	}
	return &service.AsyncVideoBillingTask{ID: 1, TaskKey: input.TaskKey}, nil
}

func (s *seedanceDurableTaskRepoStub) snapshot() ([]service.CreateAsyncVideoBillingTaskInput, []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]service.CreateAsyncVideoBillingTaskInput(nil), s.creates...), append([]int(nil), s.bodyLengths...)
}

func (s *seedanceDurableTaskRepoStub) GetOwned(_ context.Context, provider, upstreamTaskID string, userID, apiKeyID int64) (*service.AsyncVideoBillingTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getOwnedCalls = append(s.getOwnedCalls, seedanceGetOwnedCall{
		provider: provider, upstreamTaskID: upstreamTaskID, userID: userID, apiKeyID: apiKeyID,
	})
	if s.getOwnedErr != nil {
		return nil, s.getOwnedErr
	}
	if s.owned == nil {
		return nil, service.ErrAsyncVideoBillingTaskNotFound
	}
	owned := *s.owned
	return &owned, nil
}

func (s *seedanceDurableTaskRepoStub) getOwnedSnapshot() []seedanceGetOwnedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seedanceGetOwnedCall(nil), s.getOwnedCalls...)
}

type seedanceSettlementObservation struct {
	owner          service.SeedanceTaskOwner
	upstreamTaskID string
	observed       *service.SeedanceUpstreamResponse
}

type seedanceSettlementObserverStub struct {
	mu           sync.Mutex
	observations []seedanceSettlementObservation
	entered      chan struct{}
	release      chan struct{}
	calls        int32
	bills        int32
	once         sync.Once
	result       service.SeedanceSettlementResult
}

func (s *seedanceSettlementObserverStub) ObserveOwned(
	_ context.Context,
	owner service.SeedanceTaskOwner,
	upstreamTaskID string,
	observed *service.SeedanceUpstreamResponse,
	_ time.Time,
) service.SeedanceSettlementResult {
	s.mu.Lock()
	s.observations = append(s.observations, seedanceSettlementObservation{
		owner: owner, upstreamTaskID: upstreamTaskID, observed: observed,
	})
	s.mu.Unlock()
	call := atomic.AddInt32(&s.calls, 1)
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.release != nil {
		if call == 2 {
			close(s.release)
		}
		select {
		case <-s.release:
		case <-time.After(time.Second):
		}
	}
	s.once.Do(func() { atomic.AddInt32(&s.bills, 1) })
	return s.result
}

func (s *seedanceSettlementObserverStub) snapshot() []seedanceSettlementObservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]seedanceSettlementObservation(nil), s.observations...)
}

func newSeedanceDurableCreateRequest(
	t *testing.T,
	repo *seedanceDurableTaskRepoStub,
) (*OpenAIGatewayHandler, *gin.Context, *httptest.ResponseRecorder, *grokMediaSlotUpstream) {
	t.Helper()
	h, _, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	h.SetSeedanceDurableBilling(repo, &service.SeedanceTaskSettlementService{})

	upstream.call = func(req *http.Request, _ int64) (*http.Response, error) {
		require.Equal(t, http.MethodPost, req.Method)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}, "X-Request-Id": []string{"seedance-upstream"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"task-durable","status":"queued"}`)),
		}, nil
	}

	c, recorder := grokMediaSlotContext(context.Background(), true)
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.Group.Platform = service.PlatformOpenAI
	body := `{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v3/contents/generations/tasks", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	repo.recorder = recorder
	return h, c, recorder, upstream
}

func TestSeedanceCreatePersistsBeforeWritingSuccess(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{}
	h, c, recorder, upstream := newSeedanceDurableCreateRequest(t, repo)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"id":"task-durable","status":"queued"}`, recorder.Body.String())
	require.Equal(t, "seedance-upstream", recorder.Header().Get("X-Request-Id"))
	creates, bodyLengths := repo.snapshot()
	require.Len(t, creates, 1)
	require.Equal(t, []int{0}, bodyLengths)
	require.Equal(t, "task-durable", creates[0].UpstreamTaskID)
	require.Equal(t, "seedance:task-durable", creates[0].TaskKey)
	require.Equal(t, 1, upstream.calls)
}

func TestSeedanceCreatePersistenceFailureDoesNotReturnUpstreamSuccess(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{errors: []error{errors.New("database unavailable"), errors.New("database unavailable")}}
	h, c, recorder, upstream := newSeedanceDurableCreateRequest(t, repo)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), "task-durable")
	creates, bodyLengths := repo.snapshot()
	require.Len(t, creates, 2)
	require.Equal(t, []int{0, 0}, bodyLengths)
	require.Equal(t, 1, upstream.calls)
}

func TestSeedanceCreateDuplicatePersistenceReturnsSuccess(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{existing: &service.AsyncVideoBillingTask{ID: 9, TaskKey: "seedance:task-durable"}}
	h, c, recorder, upstream := newSeedanceDurableCreateRequest(t, repo)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"id":"task-durable","status":"queued"}`, recorder.Body.String())
	creates, bodyLengths := repo.snapshot()
	require.Len(t, creates, 1)
	require.Equal(t, []int{0}, bodyLengths)
	require.Equal(t, 1, upstream.calls)
}

func TestSeedanceCreateWithoutDurableDependenciesFailsClosed(t *testing.T) {
	h, slots, _, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	c, recorder := grokMediaSlotContext(context.Background(), true)
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.Group.Platform = service.PlatformOpenAI
	c.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/v3/contents/generations/tasks",
		strings.NewReader(`{"model":"doubao-seedance","content":[{"type":"text","text":"waves"}]}`),
	)
	c.Request.Header.Set("Content-Type", "application/json")

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	require.Zero(t, upstream.calls)
	slots.assertReleased(t)
}

func TestSetSeedanceDurableBillingNilSettlementKeepsDependencyAbsent(t *testing.T) {
	h := &OpenAIGatewayHandler{}
	h.SetSeedanceDurableBilling(&seedanceDurableTaskRepoStub{}, nil)

	require.True(t, h.seedanceSettlement == nil)
}

func newSeedanceDurableGetRequest(
	t *testing.T,
	repo *seedanceDurableTaskRepoStub,
	settlement *seedanceSettlementObserverStub,
) (*OpenAIGatewayHandler, *gin.Context, *httptest.ResponseRecorder, *grokMediaSlotBindings, *grokMediaSlotUpstream) {
	t.Helper()
	h, _, bindings, upstream := newGrokMediaSlotHandler(t, false, false, service.PlatformOpenAI)
	h.seedanceTasks = repo
	h.seedanceSettlement = settlement
	groupID := int64(24)
	require.NoError(t, h.gatewayService.BindGrokMediaVideoRequestAccount(
		context.Background(), &groupID, "seedance:task", 10, 20, 1,
	))
	upstream.call = func(req *http.Request, accountID int64) (*http.Response, error) {
		require.Equal(t, http.MethodGet, req.Method)
		require.Equal(t, int64(1), accountID)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"id":"task","status":"succeeded","model":"doubao-seedance","usage":{"completion_tokens":20}}`)),
		}, nil
	}
	c, recorder := grokMediaSlotContext(context.Background(), false)
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.Group.Platform = service.PlatformOpenAI
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/task", nil)
	c.Params = gin.Params{{Key: "task_id", Value: "task"}}
	return h, c, recorder, bindings, upstream
}

func TestSeedanceGetAndReconcilerRaceBillsOnce(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: &service.AsyncVideoBillingTask{
		ID: 1, Provider: service.AsyncVideoBillingProviderSeedance, UpstreamTaskID: "task",
		TaskKey: "seedance:task", UserID: 10, APIKeyID: 20, AccountID: 1,
		Status: service.AsyncVideoBillingStatusPending,
	}}
	settlement := &seedanceSettlementObserverStub{
		entered: make(chan struct{}, 2), release: make(chan struct{}),
		result: service.SeedanceSettlementResult{Settled: true},
	}
	h, c, recorder, _, upstream := newSeedanceDurableGetRequest(t, repo, settlement)

	reconcilerDone := make(chan struct{})
	go func() {
		defer close(reconcilerDone)
		settlement.ObserveOwned(
			context.Background(),
			service.SeedanceTaskOwner{UserID: 10, APIKeyID: 20},
			"task",
			&service.SeedanceUpstreamResponse{State: service.SeedanceObservedSucceeded},
			time.Now(),
		)
	}()
	<-settlement.entered

	h.SeedanceTasks(c)
	<-reconcilerDone

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, int32(2), atomic.LoadInt32(&settlement.calls))
	require.Equal(t, int32(1), atomic.LoadInt32(&settlement.bills))
	observations := settlement.snapshot()
	require.Len(t, observations, 2)
	require.Equal(t, service.SeedanceTaskOwner{UserID: 10, APIKeyID: 20}, observations[1].owner)
	require.Equal(t, "task", observations[1].upstreamTaskID)
	require.NotNil(t, observations[1].observed)
	require.Equal(t, service.SeedanceObservedSucceeded, observations[1].observed.State)
	require.Equal(t, 20, observations[1].observed.Result.Usage.OutputTokens)
	require.Equal(t, []seedanceGetOwnedCall{{
		provider: service.AsyncVideoBillingProviderSeedance, upstreamTaskID: "task", userID: 10, apiKeyID: 20,
	}}, repo.getOwnedSnapshot())
}

func TestSeedanceGetSettlementRetryKeepsUpstreamResponse(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: &service.AsyncVideoBillingTask{
		ID: 3, Provider: service.AsyncVideoBillingProviderSeedance, UpstreamTaskID: "task",
		TaskKey: "seedance:task", UserID: 10, APIKeyID: 20, AccountID: 1,
		Status: service.AsyncVideoBillingStatusPending,
	}}
	settlement := &seedanceSettlementObserverStub{result: service.SeedanceSettlementResult{
		Retried: true, ErrorCode: "seedance_usage_record_failed", Err: errors.New("database unavailable"),
	}}
	h, c, recorder, _, upstream := newSeedanceDurableGetRequest(t, repo, settlement)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.JSONEq(t, `{"id":"task","status":"succeeded","model":"doubao-seedance","usage":{"completion_tokens":20}}`, recorder.Body.String())
	require.Equal(t, 1, upstream.calls)
	require.Equal(t, int32(1), atomic.LoadInt32(&settlement.calls))
}

func TestSeedanceGetOwnerMismatchDoesNotCallUpstream(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{owned: &service.AsyncVideoBillingTask{
		ID: 2, Provider: service.AsyncVideoBillingProviderSeedance, UpstreamTaskID: "task",
		TaskKey: "seedance:task", UserID: 11, APIKeyID: 20, AccountID: 1,
		Status: service.AsyncVideoBillingStatusPending,
	}}
	settlement := &seedanceSettlementObserverStub{}
	h, c, recorder, _, upstream := newSeedanceDurableGetRequest(t, repo, settlement)

	h.SeedanceTasks(c)

	require.Equal(t, http.StatusNotFound, recorder.Code, recorder.Body.String())
	require.Zero(t, upstream.calls)
	require.Zero(t, atomic.LoadInt32(&settlement.calls))
}

func TestSeedanceGetLegacyRedisPendingStillSettles(t *testing.T) {
	repo := &seedanceDurableTaskRepoStub{getOwnedErr: service.ErrAsyncVideoBillingTaskNotFound}
	settlement := &seedanceSettlementObserverStub{}
	h, c, recorder, bindings, upstream := newSeedanceDurableGetRequest(t, repo, settlement)
	require.NoError(t, h.gatewayService.StoreGrokVideoPendingBilling(
		context.Background(),
		"seedance:task",
		10,
		20,
		service.GrokVideoPendingBilling{Model: "doubao-seedance", BillingModel: "doubao-seedance"},
	))

	h.SeedanceTasks(c)
	second, secondRecorder := grokMediaSlotContext(context.Background(), false)
	secondKey, ok := middleware.GetAPIKeyFromContext(second)
	require.True(t, ok)
	secondKey.Group.Platform = service.PlatformOpenAI
	second.Request = httptest.NewRequest(http.MethodGet, "/api/v3/contents/generations/tasks/task", nil)
	second.Params = gin.Params{{Key: "task_id", Value: "task"}}
	h.SeedanceTasks(second)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, http.StatusOK, secondRecorder.Code, secondRecorder.Body.String())
	require.Equal(t, 2, upstream.calls)
	require.Zero(t, atomic.LoadInt32(&settlement.calls))
	require.Len(t, bindings.billed, 1)
}
