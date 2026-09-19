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
	"testing"

	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type seedanceDurableTaskRepoStub struct {
	service.AsyncVideoBillingTaskRepository

	mu          sync.Mutex
	recorder    *httptest.ResponseRecorder
	errors      []error
	creates     []service.CreateAsyncVideoBillingTaskInput
	bodyLengths []int
	existing    *service.AsyncVideoBillingTask
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
