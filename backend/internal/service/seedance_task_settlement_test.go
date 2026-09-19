//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type seedanceSettlementRetryCall struct {
	Lease              AsyncVideoBillingLease
	NextPollAt         time.Time
	ErrorCode          string
	MissingTokenChecks int
}

type seedanceSettlementTerminalCall struct {
	Lease      AsyncVideoBillingLease
	Status     string
	ErrorCode  string
	TerminalAt time.Time
}

type seedanceSettlementTaskRepoStub struct {
	AsyncVideoBillingTaskRepository
	ownedTask           *AsyncVideoBillingTask
	claimedTask         *AsyncVideoBillingTask
	getOwnedCalls       []SeedanceTaskOwner
	getOwnedUpstreamIDs []string
	claimOwnedCalls     []SeedanceTaskOwner
	retryCalls          []seedanceSettlementRetryCall
	terminalCalls       []seedanceSettlementTerminalCall
	settledCalls        []AsyncVideoBillingLease
	markSettledErrors   []error
	markSettledCallNext int
}

func (s *seedanceSettlementTaskRepoStub) GetOwned(_ context.Context, _ string, upstreamTaskID string, userID, apiKeyID int64) (*AsyncVideoBillingTask, error) {
	s.getOwnedCalls = append(s.getOwnedCalls, SeedanceTaskOwner{UserID: userID, APIKeyID: apiKeyID})
	s.getOwnedUpstreamIDs = append(s.getOwnedUpstreamIDs, upstreamTaskID)
	return s.ownedTask, nil
}

func (s *seedanceSettlementTaskRepoStub) ClaimOwned(_ context.Context, _ int64, userID, apiKeyID int64, _ time.Time, _ time.Duration) (*AsyncVideoBillingTask, error) {
	s.claimOwnedCalls = append(s.claimOwnedCalls, SeedanceTaskOwner{UserID: userID, APIKeyID: apiKeyID})
	return s.claimedTask, nil
}

func (s *seedanceSettlementTaskRepoStub) MarkRetry(_ context.Context, lease AsyncVideoBillingLease, nextPollAt time.Time, errorCode string, missingTokenChecks int) error {
	s.retryCalls = append(s.retryCalls, seedanceSettlementRetryCall{
		Lease:              lease,
		NextPollAt:         nextPollAt,
		ErrorCode:          errorCode,
		MissingTokenChecks: missingTokenChecks,
	})
	return nil
}

func (s *seedanceSettlementTaskRepoStub) MarkTerminal(_ context.Context, lease AsyncVideoBillingLease, status, errorCode string, terminalAt time.Time) error {
	s.terminalCalls = append(s.terminalCalls, seedanceSettlementTerminalCall{
		Lease:      lease,
		Status:     status,
		ErrorCode:  errorCode,
		TerminalAt: terminalAt,
	})
	return nil
}

func (s *seedanceSettlementTaskRepoStub) MarkSettled(_ context.Context, lease AsyncVideoBillingLease, _ time.Time) error {
	s.settledCalls = append(s.settledCalls, lease)
	if s.markSettledCallNext >= len(s.markSettledErrors) {
		return nil
	}
	err := s.markSettledErrors[s.markSettledCallNext]
	s.markSettledCallNext++
	return err
}

type seedanceSettlementAPIKeyRepoStub struct {
	APIKeyRepository
	value *APIKey
	err   error
	calls []int64
}

func (s *seedanceSettlementAPIKeyRepoStub) GetByIDIncludeDeleted(_ context.Context, id int64) (*APIKey, error) {
	s.calls = append(s.calls, id)
	return s.value, s.err
}

type seedanceSettlementUserRepoStub struct {
	UserRepository
	value *User
	err   error
	calls []int64
}

func (s *seedanceSettlementUserRepoStub) GetByIDIncludeDeleted(_ context.Context, id int64) (*User, error) {
	s.calls = append(s.calls, id)
	return s.value, s.err
}

type seedanceSettlementAccountRepoStub struct {
	AccountRepository
	value *Account
	err   error
	calls []int64
}

func (s *seedanceSettlementAccountRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	s.calls = append(s.calls, id)
	return s.value, s.err
}

type seedanceSettlementSubscriptionRepoStub struct {
	UserSubscriptionRepository
	value *UserSubscription
	err   error
	calls []int64
}

func (s *seedanceSettlementSubscriptionRepoStub) GetByIDIncludeDeleted(_ context.Context, id int64) (*UserSubscription, error) {
	s.calls = append(s.calls, id)
	return s.value, s.err
}

type seedanceSettlementQuotaStub struct {
	quotaCalls     int
	rateLimitCalls int
}

func (s *seedanceSettlementQuotaStub) UpdateQuotaUsed(context.Context, int64, float64) error {
	s.quotaCalls++
	return nil
}

func (s *seedanceSettlementQuotaStub) UpdateRateLimitUsage(context.Context, int64, float64) error {
	s.rateLimitCalls++
	return nil
}

type seedanceSettlementHarness struct {
	service       *SeedanceTaskSettlementService
	tasks         *seedanceSettlementTaskRepoStub
	apiKeys       *seedanceSettlementAPIKeyRepoStub
	users         *seedanceSettlementUserRepoStub
	accounts      *seedanceSettlementAccountRepoStub
	subscriptions *seedanceSettlementSubscriptionRepoStub
	quota         *seedanceSettlementQuotaStub
}

func newSeedanceSettlementHarness() *seedanceSettlementHarness {
	groupID := int64(31)
	subscriptionID := int64(41)
	user := &User{ID: 11, Status: StatusActive}
	apiKey := &APIKey{ID: 21, UserID: user.ID, GroupID: &groupID, Status: StatusAPIKeyActive, User: user}
	account := &Account{ID: 51, Status: StatusActive}
	subscription := &UserSubscription{ID: subscriptionID, UserID: user.ID, GroupID: groupID, Status: SubscriptionStatusActive}

	h := &seedanceSettlementHarness{
		tasks:         &seedanceSettlementTaskRepoStub{},
		apiKeys:       &seedanceSettlementAPIKeyRepoStub{value: apiKey},
		users:         &seedanceSettlementUserRepoStub{value: user},
		accounts:      &seedanceSettlementAccountRepoStub{value: account},
		subscriptions: &seedanceSettlementSubscriptionRepoStub{value: subscription},
		quota:         &seedanceSettlementQuotaStub{},
	}
	h.service = &SeedanceTaskSettlementService{
		tasks:         h.tasks,
		apiKeys:       h.apiKeys,
		users:         h.users,
		accounts:      h.accounts,
		subscriptions: h.subscriptions,
		quotaUpdater:  h.quota,
		leaseDuration: time.Minute,
	}
	return h
}

func seedanceSettlementClaimedTask(now time.Time) AsyncVideoBillingTask {
	groupID := int64(31)
	subscriptionID := int64(41)
	leaseToken := uuid.New()
	return AsyncVideoBillingTask{
		ID:                  61,
		Provider:            AsyncVideoBillingProviderSeedance,
		UpstreamTaskID:      "upstream-task-61",
		TaskKey:             SeedanceTaskKey("upstream-task-61"),
		UserID:              11,
		APIKeyID:            21,
		AccountID:           51,
		GroupID:             &groupID,
		SubscriptionID:      &subscriptionID,
		Model:               "seedance-1-0-pro",
		BillingModel:        "seedance-1-0-pro",
		UpstreamModel:       "seedance-1-0-pro-250528",
		OriginalModel:       "seedance-1-0-pro",
		QuotaPlatform:       PlatformOpenAI,
		SubscriptionBilling: true,
		PricingAt:           now.Add(-2 * time.Minute),
		RequestPayloadHash:  "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		InboundEndpoint:     "/v1/videos",
		UpstreamEndpoint:    "/v1/videos",
		Status:              AsyncVideoBillingStatusPending,
		PollDeadlineAt:      now.Add(time.Hour),
		LeaseToken:          &leaseToken,
		LeaseEpoch:          1,
		CreatedAt:           now.Add(-2 * time.Minute),
	}
}

func seedanceSettlementSucceeded(tokens int) *SeedanceUpstreamResponse {
	return &SeedanceUpstreamResponse{
		State: SeedanceObservedSucceeded,
		Result: &OpenAIForwardResult{
			Usage: OpenAIUsage{OutputTokens: tokens},
		},
	}
}

func TestSeedanceSettlementSuccessBillsOnceWhenStateWriteFails(t *testing.T) {
	now := time.Date(2026, time.September, 19, 8, 0, 0, 0, time.UTC)
	h := newSeedanceSettlementHarness()
	ledgerWriteErr := errors.New("settled state write failed")
	h.tasks.markSettledErrors = []error{ledgerWriteErr, nil}

	seen := make(map[string]struct{})
	moneyEffects := 0
	var requestIDs []string
	h.service.recordUsage = func(_ context.Context, input *OpenAIRecordUsageInput) error {
		require.NotNil(t, input)
		require.NotNil(t, input.Result)
		requestIDs = append(requestIDs, input.Result.RequestID)
		if _, exists := seen[input.Result.RequestID]; !exists {
			seen[input.Result.RequestID] = struct{}{}
			moneyEffects++
		}
		return nil
	}

	task := seedanceSettlementClaimedTask(now)
	first := h.service.ProcessClaimed(context.Background(), task, seedanceSettlementSucceeded(120), now)
	require.ErrorIs(t, first.Err, ledgerWriteErr)
	require.False(t, first.Settled)

	replacementLease := uuid.New()
	task.LeaseToken = &replacementLease
	task.LeaseEpoch++
	second := h.service.ProcessClaimed(context.Background(), task, seedanceSettlementSucceeded(120), now.Add(time.Second))
	require.NoError(t, second.Err)
	require.True(t, second.Settled)
	require.Equal(t, 1, moneyEffects, "stable request identity must make replay money-idempotent")
	require.Equal(t, []string{
		StableGrokVideoBillingRequestID(task.TaskKey),
		StableGrokVideoBillingRequestID(task.TaskKey),
	}, requestIDs)
	require.Len(t, h.tasks.settledCalls, 2)
}

func TestSeedanceSettlementMissingTokensDeadLettersAfterThreeChecks(t *testing.T) {
	now := time.Date(2026, time.September, 19, 8, 0, 0, 0, time.UTC)
	h := newSeedanceSettlementHarness()
	recordUsageCalls := 0
	h.service.recordUsage = func(context.Context, *OpenAIRecordUsageInput) error {
		recordUsageCalls++
		return nil
	}

	for check := 0; check < 3; check++ {
		task := seedanceSettlementClaimedTask(now)
		task.MissingTokenChecks = check
		leaseToken := uuid.New()
		task.LeaseToken = &leaseToken
		task.LeaseEpoch = int64(check + 1)

		result := h.service.ProcessClaimed(context.Background(), task, seedanceSettlementSucceeded(0), now.Add(time.Duration(check)*time.Second))
		if check < 2 {
			require.NoError(t, result.Err)
			require.True(t, result.Retried)
			require.False(t, result.DeadLettered)
			continue
		}
		require.NoError(t, result.Err)
		require.True(t, result.DeadLettered)
		require.Equal(t, "seedance_missing_completion_tokens", result.ErrorCode)
	}

	require.Zero(t, recordUsageCalls)
	require.Len(t, h.tasks.retryCalls, 2)
	require.Equal(t, 1, h.tasks.retryCalls[0].MissingTokenChecks)
	require.Equal(t, 2, h.tasks.retryCalls[1].MissingTokenChecks)
	require.Len(t, h.tasks.terminalCalls, 1)
	require.Equal(t, AsyncVideoBillingStatusDeadLetter, h.tasks.terminalCalls[0].Status)
	require.Equal(t, "seedance_missing_completion_tokens", h.tasks.terminalCalls[0].ErrorCode)
}

func TestSeedanceSettlementLoadsDeletedAPIKeyAndSubscription(t *testing.T) {
	now := time.Date(2026, time.September, 19, 8, 0, 0, 0, time.UTC)
	h := newSeedanceSettlementHarness()
	deletedAt := now.Add(-time.Minute)
	h.users.value.DeletedAt = &deletedAt
	h.apiKeys.value.Key = ""
	h.subscriptions.value.DeletedAt = &deletedAt

	var captured *OpenAIRecordUsageInput
	h.service.recordUsage = func(_ context.Context, input *OpenAIRecordUsageInput) error {
		captured = input
		return nil
	}

	task := seedanceSettlementClaimedTask(now)
	result := h.service.ProcessClaimed(context.Background(), task, seedanceSettlementSucceeded(80), now)
	require.NoError(t, result.Err)
	require.True(t, result.Settled)
	require.Equal(t, []int64{task.UserID}, h.users.calls)
	require.Equal(t, []int64{task.APIKeyID}, h.apiKeys.calls)
	require.Equal(t, []int64{*task.SubscriptionID}, h.subscriptions.calls)
	require.Same(t, h.users.value, captured.User)
	require.Same(t, h.apiKeys.value, captured.APIKey)
	require.Same(t, h.subscriptions.value, captured.Subscription)
	require.Same(t, h.accounts.value, captured.Account)
	require.Same(t, h.quota, captured.APIKeyService)
	require.Equal(t, task.PricingAt, captured.PricingAt)
	require.Equal(t, task.RequestPayloadHash, captured.RequestPayloadHash)
	require.Equal(t, task.OriginalModel, captured.OriginalModel)
	require.Equal(t, task.Model, captured.ChannelMappedModel)
}

func TestSeedanceSettlementPhysicallyMissingOwnerDeadLetters(t *testing.T) {
	now := time.Date(2026, time.September, 19, 8, 0, 0, 0, time.UTC)
	h := newSeedanceSettlementHarness()
	h.users.value = nil
	h.users.err = ErrUserNotFound
	recordUsageCalls := 0
	h.service.recordUsage = func(context.Context, *OpenAIRecordUsageInput) error {
		recordUsageCalls++
		return nil
	}

	result := h.service.ProcessClaimed(context.Background(), seedanceSettlementClaimedTask(now), seedanceSettlementSucceeded(80), now)
	require.NoError(t, result.Err)
	require.True(t, result.DeadLettered)
	require.Equal(t, "seedance_user_missing", result.ErrorCode)
	require.Zero(t, recordUsageCalls)
	require.Len(t, h.tasks.terminalCalls, 1)
	require.Equal(t, AsyncVideoBillingStatusDeadLetter, h.tasks.terminalCalls[0].Status)
	require.Equal(t, "seedance_user_missing", h.tasks.terminalCalls[0].ErrorCode)
}

func TestSeedanceSettlementUnavailableSubscriptionRepositoryRetries(t *testing.T) {
	now := time.Date(2026, time.September, 19, 8, 0, 0, 0, time.UTC)
	h := newSeedanceSettlementHarness()
	h.service.subscriptions = nil
	recordUsageCalls := 0
	h.service.recordUsage = func(context.Context, *OpenAIRecordUsageInput) error {
		recordUsageCalls++
		return nil
	}

	result := h.service.ProcessClaimed(context.Background(), seedanceSettlementClaimedTask(now), seedanceSettlementSucceeded(80), now)
	require.Error(t, result.Err)
	require.True(t, result.Retried)
	require.False(t, result.DeadLettered)
	require.Equal(t, "seedance_subscription_repository_unavailable", result.ErrorCode)
	require.Zero(t, recordUsageCalls)
	require.Len(t, h.tasks.retryCalls, 1)
	require.Empty(t, h.tasks.terminalCalls)
}

func TestSeedanceSettlementStaleLeaseCannotSettle(t *testing.T) {
	now := time.Date(2026, time.September, 19, 8, 0, 0, 0, time.UTC)
	h := newSeedanceSettlementHarness()
	h.tasks.markSettledErrors = []error{ErrAsyncVideoBillingTaskFenced}
	recordUsageCalls := 0
	h.service.recordUsage = func(context.Context, *OpenAIRecordUsageInput) error {
		recordUsageCalls++
		return nil
	}

	result := h.service.ProcessClaimed(context.Background(), seedanceSettlementClaimedTask(now), seedanceSettlementSucceeded(80), now)
	require.ErrorIs(t, result.Err, ErrAsyncVideoBillingTaskFenced)
	require.True(t, result.Fenced)
	require.False(t, result.Settled)
	require.Equal(t, 1, recordUsageCalls, "billing may win the race, but the stale lease must not mark the ledger settled")
	require.Len(t, h.tasks.settledCalls, 1)
}

func TestSeedanceSettlementObserveOwnedClaimsBeforeProcessing(t *testing.T) {
	now := time.Date(2026, time.September, 19, 8, 0, 0, 0, time.UTC)
	h := newSeedanceSettlementHarness()
	owned := seedanceSettlementClaimedTask(now)
	owned.LeaseToken = nil
	owned.LeaseEpoch = 0
	claimed := seedanceSettlementClaimedTask(now)
	h.tasks.ownedTask = &owned
	h.tasks.claimedTask = &claimed
	h.service.recordUsage = func(context.Context, *OpenAIRecordUsageInput) error { return nil }

	owner := SeedanceTaskOwner{UserID: claimed.UserID, APIKeyID: claimed.APIKeyID}
	result := h.service.ObserveOwned(context.Background(), owner, claimed.UpstreamTaskID, seedanceSettlementSucceeded(80), now)

	require.NoError(t, result.Err)
	require.True(t, result.Settled)
	require.Equal(t, []SeedanceTaskOwner{owner}, h.tasks.getOwnedCalls)
	require.Equal(t, []string{claimed.UpstreamTaskID}, h.tasks.getOwnedUpstreamIDs)
	require.Equal(t, []SeedanceTaskOwner{owner}, h.tasks.claimOwnedCalls)
	require.Len(t, h.tasks.settledCalls, 1)
}
