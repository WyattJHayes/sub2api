//go:build integration

package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func asyncVideoTaskFixture(t *testing.T, userID, apiKeyID int64, upstreamTaskID string) service.CreateAsyncVideoBillingTaskInput {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	return service.CreateAsyncVideoBillingTaskInput{
		Provider:           service.AsyncVideoBillingProviderSeedance,
		UpstreamTaskID:     upstreamTaskID,
		UserID:             userID,
		APIKeyID:           apiKeyID,
		AccountID:          301,
		Model:              "video",
		BillingModel:       "video",
		UpstreamModel:      "ep-seedance",
		PricingAt:          now,
		NextPollAt:         now,
		PollDeadlineAt:     now.Add(24 * time.Hour),
		RequestPayloadHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		InboundEndpoint:    "/api/v3/contents/generations/tasks",
		UpstreamEndpoint:   "/api/v3/contents/generations/tasks",
	}
}

func resetAsyncVideoBillingTasks(t *testing.T) {
	t.Helper()
	_, err := integrationDB.ExecContext(context.Background(), "TRUNCATE TABLE async_video_billing_tasks RESTART IDENTITY")
	require.NoError(t, err)
}

func TestAsyncVideoBillingTaskRepositoryCreateIsIdempotentAndOwnerScoped(t *testing.T) {
	resetAsyncVideoBillingTasks(t)
	repo := NewAsyncVideoBillingTaskRepository(integrationDB)
	input := asyncVideoTaskFixture(t, 101, 201, "same-upstream")
	first, err := repo.Create(context.Background(), input)
	require.NoError(t, err)
	second, err := repo.Create(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)

	other := input
	other.UserID, other.APIKeyID = 102, 202
	otherTask, err := repo.Create(context.Background(), other)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, otherTask.ID)

	_, err = repo.GetOwned(context.Background(), service.AsyncVideoBillingProviderSeedance, "same-upstream", 102, 201)
	require.ErrorIs(t, err, service.ErrAsyncVideoBillingTaskNotFound)
}

func TestAsyncVideoBillingTaskRepositoryLeaseExpiryAndFencing(t *testing.T) {
	resetAsyncVideoBillingTasks(t)
	repo := NewAsyncVideoBillingTaskRepository(integrationDB)
	task, err := repo.Create(context.Background(), asyncVideoTaskFixture(t, 111, 211, "lease-upstream"))
	require.NoError(t, err)

	now := task.NextPollAt.Add(time.Second)
	const leaseDuration = time.Minute
	start := make(chan struct{})
	claims := make([][]service.AsyncVideoBillingTask, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range claims {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			claims[index], errs[index] = repo.ClaimDue(context.Background(), service.AsyncVideoBillingProviderSeedance, now, 1, leaseDuration)
		}(i)
	}
	close(start)
	wg.Wait()

	totalClaims := 0
	var firstClaim service.AsyncVideoBillingTask
	for i := range claims {
		require.NoError(t, errs[i])
		totalClaims += len(claims[i])
		if len(claims[i]) == 1 {
			firstClaim = claims[i][0]
		}
	}
	require.Equal(t, 1, totalClaims)
	require.NotNil(t, firstClaim.LeaseToken)
	require.EqualValues(t, 1, firstClaim.LeaseEpoch)

	reclaimed, err := repo.ClaimDue(context.Background(), service.AsyncVideoBillingProviderSeedance, now.Add(2*leaseDuration), 1, leaseDuration)
	require.NoError(t, err)
	require.Len(t, reclaimed, 1)
	require.NotNil(t, reclaimed[0].LeaseToken)
	require.Greater(t, reclaimed[0].LeaseEpoch, firstClaim.LeaseEpoch)
	require.NotEqual(t, *firstClaim.LeaseToken, *reclaimed[0].LeaseToken)

	oldLease := service.AsyncVideoBillingLease{TaskID: firstClaim.ID, Token: *firstClaim.LeaseToken, Epoch: firstClaim.LeaseEpoch}
	err = repo.MarkRetry(context.Background(), oldLease, now.Add(3*leaseDuration), "temporary", 0)
	require.ErrorIs(t, err, service.ErrAsyncVideoBillingTaskFenced)

	staleToken := service.AsyncVideoBillingLease{TaskID: reclaimed[0].ID, Token: uuid.New(), Epoch: reclaimed[0].LeaseEpoch}
	err = repo.MarkRetry(context.Background(), staleToken, now.Add(3*leaseDuration), "temporary", 0)
	require.ErrorIs(t, err, service.ErrAsyncVideoBillingTaskFenced)

	staleEpoch := service.AsyncVideoBillingLease{TaskID: reclaimed[0].ID, Token: *reclaimed[0].LeaseToken, Epoch: reclaimed[0].LeaseEpoch - 1}
	err = repo.MarkRetry(context.Background(), staleEpoch, now.Add(3*leaseDuration), "temporary", 0)
	require.ErrorIs(t, err, service.ErrAsyncVideoBillingTaskFenced)

	currentLease := service.AsyncVideoBillingLease{TaskID: reclaimed[0].ID, Token: *reclaimed[0].LeaseToken, Epoch: reclaimed[0].LeaseEpoch}
	require.NoError(t, repo.MarkRetry(context.Background(), currentLease, now.Add(3*leaseDuration), "temporary", 0))
}
