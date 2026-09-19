package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	seedanceSettlementRetryDelay = 5 * time.Second

	seedanceErrorMissingCompletionTokens = "seedance_missing_completion_tokens"
	seedanceErrorUserMissing             = "seedance_user_missing"
	seedanceErrorAPIKeyMissing           = "seedance_api_key_missing"
	seedanceErrorAccountMissing          = "seedance_account_missing"
	seedanceErrorSubscriptionMissing     = "seedance_subscription_missing"
	seedanceErrorOwnerMismatch           = "seedance_owner_mismatch"
	seedanceErrorDeadlineExceeded        = "seedance_poll_deadline_exceeded"
	seedanceErrorTaskInvalid             = "seedance_task_invalid"
)

type SeedanceTaskOwner struct {
	UserID   int64
	APIKeyID int64
}

type SeedanceSettlementResult struct {
	Settled      bool
	Retried      bool
	Terminal     bool
	DeadLettered bool
	Fenced       bool
	ErrorCode    string
	Err          error
}

type SeedanceTaskSettlementService struct {
	tasks         AsyncVideoBillingTaskRepository
	apiKeys       APIKeyRepository
	users         UserRepository
	accounts      AccountRepository
	subscriptions UserSubscriptionRepository
	usage         *OpenAIGatewayService
	quotaUpdater  APIKeyQuotaUpdater
	leaseDuration time.Duration
	recordUsage   func(context.Context, *OpenAIRecordUsageInput) error
}

func (s *SeedanceTaskSettlementService) ProcessClaimed(
	ctx context.Context,
	task AsyncVideoBillingTask,
	observed *SeedanceUpstreamResponse,
	now time.Time,
) SeedanceSettlementResult {
	if err := validateClaimedSeedanceTask(task, now); err != nil {
		return SeedanceSettlementResult{ErrorCode: seedanceErrorTaskInvalid, Err: err}
	}
	if !task.PollDeadlineAt.IsZero() && !now.Before(task.PollDeadlineAt) {
		return s.deadLetter(ctx, task, now, seedanceErrorDeadlineExceeded)
	}
	if observed == nil {
		return s.retry(ctx, task, now, "seedance_observation_missing", task.MissingTokenChecks, errors.New("seedance observation is nil"))
	}

	switch observed.State {
	case SeedanceObservedFailed:
		return s.markTerminal(ctx, task, now, AsyncVideoBillingStatusFailed, "")
	case SeedanceObservedCancelled:
		return s.markTerminal(ctx, task, now, AsyncVideoBillingStatusCancelled, "")
	case SeedanceObservedPending, "":
		return s.retry(ctx, task, now, "seedance_pending", task.MissingTokenChecks, nil)
	case SeedanceObservedSucceeded:
		// Continue below.
	default:
		return s.retry(ctx, task, now, "seedance_state_unknown", task.MissingTokenChecks, fmt.Errorf("unknown seedance state %q", observed.State))
	}

	if observed.Result == nil || observed.Result.Usage.OutputTokens <= 0 {
		missingChecks := task.MissingTokenChecks + 1
		if missingChecks >= 3 {
			return s.deadLetter(ctx, task, now, seedanceErrorMissingCompletionTokens)
		}
		return s.retry(ctx, task, now, seedanceErrorMissingCompletionTokens, missingChecks, nil)
	}

	user, result := s.loadSeedanceUser(ctx, task, now)
	if result != nil {
		return *result
	}
	apiKey, result := s.loadSeedanceAPIKey(ctx, task, now)
	if result != nil {
		return *result
	}
	account, result := s.loadSeedanceAccount(ctx, task, now)
	if result != nil {
		return *result
	}
	subscription, result := s.loadSeedanceSubscription(ctx, task, now)
	if result != nil {
		return *result
	}
	if !seedanceOwnershipMatches(task, user, apiKey, account, subscription) {
		return s.deadLetter(ctx, task, now, seedanceErrorOwnerMismatch)
	}

	apiKey.User = user
	usageResult := *observed.Result
	usageResult.RequestID = StableGrokVideoBillingRequestID(task.TaskKey)
	usageResult.ResponseID = task.TaskKey
	usageResult.Model = task.Model
	usageResult.BillingModel = task.BillingModel
	usageResult.UpstreamModel = firstNonEmptyString(task.UpstreamModel, usageResult.UpstreamModel)
	usageResult.Duration = now.Sub(task.CreatedAt)

	recordUsage := s.recordUsage
	if recordUsage == nil && s.usage != nil {
		recordUsage = s.usage.RecordUsage
	}
	if recordUsage == nil {
		return s.retry(ctx, task, now, "seedance_usage_unavailable", task.MissingTokenChecks, errors.New("seedance usage recorder is unavailable"))
	}
	if err := recordUsage(ctx, &OpenAIRecordUsageInput{
		Result:             &usageResult,
		APIKey:             apiKey,
		User:               user,
		Account:            account,
		Subscription:       subscription,
		InboundEndpoint:    task.InboundEndpoint,
		UpstreamEndpoint:   task.UpstreamEndpoint,
		RequestPayloadHash: task.RequestPayloadHash,
		APIKeyService:      s.quotaUpdater,
		QuotaPlatform:      task.QuotaPlatform,
		PricingAt:          task.PricingAt,
		ChannelUsageFields: ChannelUsageFields{
			OriginalModel:      task.OriginalModel,
			ChannelMappedModel: task.Model,
		},
	}); err != nil {
		return s.retry(ctx, task, now, "seedance_usage_record_failed", task.MissingTokenChecks, err)
	}

	err := s.tasks.MarkSettled(ctx, seedanceTaskLease(task), now)
	if err != nil {
		return seedanceTransitionFailure(err, "")
	}
	return SeedanceSettlementResult{Settled: true}
}

func (s *SeedanceTaskSettlementService) ObserveOwned(
	ctx context.Context,
	owner SeedanceTaskOwner,
	upstreamTaskID string,
	observed *SeedanceUpstreamResponse,
	now time.Time,
) SeedanceSettlementResult {
	upstreamTaskID = strings.TrimPrefix(strings.TrimSpace(upstreamTaskID), "seedance:")
	if s == nil || s.tasks == nil || owner.UserID <= 0 || owner.APIKeyID <= 0 || upstreamTaskID == "" || now.IsZero() {
		return SeedanceSettlementResult{Err: errors.New("invalid seedance owned settlement input")}
	}

	task, err := s.tasks.GetOwned(ctx, AsyncVideoBillingProviderSeedance, upstreamTaskID, owner.UserID, owner.APIKeyID)
	if err != nil {
		return SeedanceSettlementResult{Err: err}
	}
	if task == nil {
		return SeedanceSettlementResult{Err: ErrAsyncVideoBillingTaskNotFound}
	}
	if task.UserID != owner.UserID || task.APIKeyID != owner.APIKeyID {
		return SeedanceSettlementResult{Err: ErrAsyncVideoBillingTaskNotFound}
	}

	switch task.Status {
	case AsyncVideoBillingStatusSettled:
		return SeedanceSettlementResult{Settled: true}
	case AsyncVideoBillingStatusFailed, AsyncVideoBillingStatusCancelled:
		return SeedanceSettlementResult{Terminal: true, ErrorCode: task.LastErrorCode}
	case AsyncVideoBillingStatusDeadLetter:
		return SeedanceSettlementResult{DeadLettered: true, ErrorCode: task.LastErrorCode}
	case AsyncVideoBillingStatusPending:
		// Claim below.
	default:
		return SeedanceSettlementResult{ErrorCode: seedanceErrorTaskInvalid, Err: fmt.Errorf("invalid seedance task status %q", task.Status)}
	}

	leaseDuration := s.leaseDuration
	if leaseDuration <= 0 {
		leaseDuration = time.Minute
	}
	claimed, err := s.tasks.ClaimOwned(ctx, task.ID, owner.UserID, owner.APIKeyID, now, leaseDuration)
	if err != nil {
		return seedanceTransitionFailure(err, "")
	}
	if claimed == nil {
		return SeedanceSettlementResult{Err: ErrAsyncVideoBillingTaskNotFound}
	}
	return s.ProcessClaimed(ctx, *claimed, observed, now)
}

func validateClaimedSeedanceTask(task AsyncVideoBillingTask, now time.Time) error {
	if now.IsZero() {
		return errors.New("seedance settlement time is required")
	}
	if task.Provider != AsyncVideoBillingProviderSeedance || task.ID <= 0 || task.UserID <= 0 || task.APIKeyID <= 0 || task.AccountID <= 0 {
		return errors.New("seedance settlement task identity is invalid")
	}
	if strings.TrimSpace(task.TaskKey) == "" || strings.TrimSpace(task.Model) == "" {
		return errors.New("seedance settlement task snapshot is incomplete")
	}
	if !seedanceTaskLease(task).Valid() {
		return errors.New("seedance settlement task lease is invalid")
	}
	return nil
}

func seedanceTaskLease(task AsyncVideoBillingTask) AsyncVideoBillingLease {
	lease := AsyncVideoBillingLease{TaskID: task.ID, Epoch: task.LeaseEpoch}
	if task.LeaseToken != nil {
		lease.Token = *task.LeaseToken
	}
	return lease
}

func (s *SeedanceTaskSettlementService) loadSeedanceUser(ctx context.Context, task AsyncVideoBillingTask, now time.Time) (*User, *SeedanceSettlementResult) {
	if s == nil || s.users == nil {
		result := s.retry(ctx, task, now, "seedance_user_repository_unavailable", task.MissingTokenChecks, errors.New("seedance user repository is unavailable"))
		return nil, &result
	}
	user, err := s.users.GetByIDIncludeDeleted(ctx, task.UserID)
	if errors.Is(err, ErrUserNotFound) || (err == nil && user == nil) {
		result := s.deadLetter(ctx, task, now, seedanceErrorUserMissing)
		return nil, &result
	}
	if err != nil {
		result := s.retry(ctx, task, now, "seedance_user_load_failed", task.MissingTokenChecks, err)
		return nil, &result
	}
	return user, nil
}

func (s *SeedanceTaskSettlementService) loadSeedanceAPIKey(ctx context.Context, task AsyncVideoBillingTask, now time.Time) (*APIKey, *SeedanceSettlementResult) {
	if s == nil || s.apiKeys == nil {
		result := s.retry(ctx, task, now, "seedance_api_key_repository_unavailable", task.MissingTokenChecks, errors.New("seedance api key repository is unavailable"))
		return nil, &result
	}
	apiKey, err := s.apiKeys.GetByIDIncludeDeleted(ctx, task.APIKeyID)
	if errors.Is(err, ErrAPIKeyNotFound) || (err == nil && apiKey == nil) {
		result := s.deadLetter(ctx, task, now, seedanceErrorAPIKeyMissing)
		return nil, &result
	}
	if err != nil {
		result := s.retry(ctx, task, now, "seedance_api_key_load_failed", task.MissingTokenChecks, err)
		return nil, &result
	}
	return apiKey, nil
}

func (s *SeedanceTaskSettlementService) loadSeedanceAccount(ctx context.Context, task AsyncVideoBillingTask, now time.Time) (*Account, *SeedanceSettlementResult) {
	if s == nil || s.accounts == nil {
		result := s.retry(ctx, task, now, "seedance_account_repository_unavailable", task.MissingTokenChecks, errors.New("seedance account repository is unavailable"))
		return nil, &result
	}
	account, err := s.accounts.GetByID(ctx, task.AccountID)
	if errors.Is(err, ErrAccountNotFound) || (err == nil && account == nil) {
		result := s.deadLetter(ctx, task, now, seedanceErrorAccountMissing)
		return nil, &result
	}
	if err != nil {
		result := s.retry(ctx, task, now, "seedance_account_load_failed", task.MissingTokenChecks, err)
		return nil, &result
	}
	return account, nil
}

func (s *SeedanceTaskSettlementService) loadSeedanceSubscription(ctx context.Context, task AsyncVideoBillingTask, now time.Time) (*UserSubscription, *SeedanceSettlementResult) {
	if task.SubscriptionID == nil {
		if task.SubscriptionBilling {
			result := s.deadLetter(ctx, task, now, seedanceErrorOwnerMismatch)
			return nil, &result
		}
		return nil, nil
	}
	if !task.SubscriptionBilling {
		result := s.deadLetter(ctx, task, now, seedanceErrorOwnerMismatch)
		return nil, &result
	}
	if s == nil || s.subscriptions == nil {
		result := s.retry(ctx, task, now, "seedance_subscription_repository_unavailable", task.MissingTokenChecks, errors.New("seedance subscription repository is unavailable"))
		return nil, &result
	}
	subscription, err := s.subscriptions.GetByIDIncludeDeleted(ctx, *task.SubscriptionID)
	if errors.Is(err, ErrSubscriptionNotFound) || (err == nil && subscription == nil) {
		result := s.deadLetter(ctx, task, now, seedanceErrorSubscriptionMissing)
		return nil, &result
	}
	if err != nil {
		result := s.retry(ctx, task, now, "seedance_subscription_load_failed", task.MissingTokenChecks, err)
		return nil, &result
	}
	return subscription, nil
}

func seedanceOwnershipMatches(task AsyncVideoBillingTask, user *User, apiKey *APIKey, account *Account, subscription *UserSubscription) bool {
	if user == nil || apiKey == nil || account == nil {
		return false
	}
	if user.ID != task.UserID || apiKey.ID != task.APIKeyID || apiKey.UserID != task.UserID || account.ID != task.AccountID {
		return false
	}
	if !sameOptionalInt64(task.GroupID, apiKey.GroupID) {
		return false
	}
	if task.SubscriptionID == nil {
		return subscription == nil && !task.SubscriptionBilling
	}
	return subscription != nil && subscription.ID == *task.SubscriptionID && subscription.UserID == task.UserID && task.GroupID != nil && subscription.GroupID == *task.GroupID
}

func sameOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (s *SeedanceTaskSettlementService) retry(ctx context.Context, task AsyncVideoBillingTask, now time.Time, errorCode string, missingTokenChecks int, cause error) SeedanceSettlementResult {
	if s == nil || s.tasks == nil {
		if cause != nil {
			return SeedanceSettlementResult{ErrorCode: errorCode, Err: cause}
		}
		return SeedanceSettlementResult{ErrorCode: errorCode, Err: errors.New("seedance task repository is unavailable")}
	}
	err := s.tasks.MarkRetry(ctx, seedanceTaskLease(task), now.Add(seedanceSettlementRetryDelay), errorCode, missingTokenChecks)
	if err != nil {
		return seedanceTransitionFailure(err, errorCode)
	}
	return SeedanceSettlementResult{Retried: true, ErrorCode: errorCode, Err: cause}
}

func (s *SeedanceTaskSettlementService) deadLetter(ctx context.Context, task AsyncVideoBillingTask, now time.Time, errorCode string) SeedanceSettlementResult {
	return s.markTerminal(ctx, task, now, AsyncVideoBillingStatusDeadLetter, errorCode)
}

func (s *SeedanceTaskSettlementService) markTerminal(ctx context.Context, task AsyncVideoBillingTask, now time.Time, status, errorCode string) SeedanceSettlementResult {
	if s == nil || s.tasks == nil {
		return SeedanceSettlementResult{ErrorCode: errorCode, Err: errors.New("seedance task repository is unavailable")}
	}
	err := s.tasks.MarkTerminal(ctx, seedanceTaskLease(task), status, errorCode, now)
	if err != nil {
		return seedanceTransitionFailure(err, errorCode)
	}
	if status == AsyncVideoBillingStatusDeadLetter {
		return SeedanceSettlementResult{DeadLettered: true, ErrorCode: errorCode}
	}
	return SeedanceSettlementResult{Terminal: true, ErrorCode: errorCode}
}

func seedanceTransitionFailure(err error, errorCode string) SeedanceSettlementResult {
	return SeedanceSettlementResult{
		Fenced:    errors.Is(err, ErrAsyncVideoBillingTaskFenced),
		ErrorCode: errorCode,
		Err:       err,
	}
}
