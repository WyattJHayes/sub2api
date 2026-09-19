package handler

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const seedanceTaskPersistenceRetryDelay = 50 * time.Millisecond

const (
	seedanceDeleteLeaseDuration = time.Minute
	seedanceDeleteRetryDelay    = 5 * time.Second
)

var errSeedanceTaskPersistence = errors.New("seedance task persistence failed")

type seedanceSettlementObserver interface {
	ObserveOwned(
		context.Context,
		service.SeedanceTaskOwner,
		string,
		*service.SeedanceUpstreamResponse,
		time.Time,
	) service.SeedanceSettlementResult
	ProcessClaimed(
		context.Context,
		service.AsyncVideoBillingTask,
		*service.SeedanceUpstreamResponse,
		time.Time,
	) service.SeedanceSettlementResult
}

func (h *OpenAIGatewayHandler) SetSeedanceDurableBilling(
	tasks service.AsyncVideoBillingTaskRepository,
	settlement *service.SeedanceTaskSettlementService,
) {
	h.seedanceTasks = tasks
	h.seedanceSettlement = nil
	if settlement != nil {
		h.seedanceSettlement = settlement
	}
}

// SeedanceTasks exposes Ark's native asynchronous video task protocol.
func (h *OpenAIGatewayHandler) SeedanceTasks(c *gin.Context) {
	if c.Request.Method == http.MethodPost && c.GetHeader("Content-Type") != "" {
		mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if err != nil || mediaType != "application/json" {
			h.errorResponse(c, http.StatusUnsupportedMediaType, "invalid_request_error", "Seedance requires application/json")
			return
		}
	}
	key, ok := middleware.GetAPIKeyFromContext(c)
	if !ok || key.Group == nil || (key.Group.Platform != service.PlatformOpenAI && key.Group.Platform != service.PlatformComposite) {
		h.errorResponse(c, http.StatusForbidden, "permission_error", "Seedance requires an OpenAI or composite group")
		return
	}
	endpoint := service.SeedanceEndpointCreate
	setActualUpstreamEndpoint(c, EndpointSeedanceTasks)
	taskID := ""
	if c.Request.Method != http.MethodPost {
		taskID = service.SeedanceTaskKey(c.Param("task_id"))
		endpoint = service.SeedanceEndpointStatus
		if c.Request.Method == http.MethodDelete {
			endpoint = service.SeedanceEndpointDelete
		}
	}
	h.handleGrokMedia(c, endpoint, taskID)
}

func (h *OpenAIGatewayHandler) forwardSeedanceCreateDurably(
	ctx context.Context,
	c *gin.Context,
	reqLog *zap.Logger,
	account *service.Account,
	apiKey *service.APIKey,
	subject middleware.AuthSubject,
	subscription *service.UserSubscription,
	requestStart time.Time,
	requestModel string,
	body []byte,
) (*service.OpenAIForwardResult, error) {
	upstreamStarted := time.Now()
	response, err := h.gatewayService.CreateSeedanceTask(ctx, account, body)
	service.SetOpsLatencyMs(c, service.OpsUpstreamLatencyMsKey, time.Since(upstreamStarted).Milliseconds())
	if err != nil {
		var upstreamErr *service.SeedanceUpstreamError
		if errors.As(err, &upstreamErr) && upstreamErr.SafeCode == "response_too_large" {
			service.SetOpsUpstreamError(c, http.StatusBadGateway, "upstream response too large", "")
			h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Upstream response too large")
		} else if response != nil && response.StatusCode >= http.StatusMultipleChoices {
			h.writeSeedanceResponse(c, response)
		}
		return nil, err
	}

	if response == nil || response.Result == nil {
		return nil, errors.New("seedance create response is incomplete")
	}
	if err := h.persistSeedanceCreate(
		ctx,
		c,
		reqLog,
		account,
		apiKey,
		subject,
		subscription,
		requestStart,
		requestModel,
		body,
		response.Result,
	); err != nil {
		return response.Result, err
	}

	h.writeSeedanceResponse(c, response)
	return response.Result, nil
}

func (h *OpenAIGatewayHandler) persistSeedanceCreate(
	ctx context.Context,
	c *gin.Context,
	reqLog *zap.Logger,
	account *service.Account,
	apiKey *service.APIKey,
	subject middleware.AuthSubject,
	subscription *service.UserSubscription,
	requestStart time.Time,
	requestModel string,
	body []byte,
	result *service.OpenAIForwardResult,
) error {
	persistenceStarted := time.Now()
	if h == nil || h.seedanceTasks == nil || result == nil || account == nil || apiKey == nil {
		return errSeedanceTaskPersistence
	}

	var subscriptionID *int64
	if subscription != nil {
		value := subscription.ID
		subscriptionID = &value
	}
	now := time.Now()
	input := service.CreateAsyncVideoBillingTaskInput{
		Provider:            service.AsyncVideoBillingProviderSeedance,
		UpstreamTaskID:      strings.TrimPrefix(result.ResponseID, "seedance:"),
		TaskKey:             result.ResponseID,
		UserID:              subject.UserID,
		APIKeyID:            apiKey.ID,
		GroupID:             apiKey.GroupID,
		AccountID:           account.ID,
		SubscriptionID:      subscriptionID,
		Model:               requestModel,
		BillingModel:        firstNonEmptyString(result.BillingModel, requestModel),
		UpstreamModel:       result.UpstreamModel,
		OriginalModel:       clientRequestedModel(c, requestModel),
		QuotaPlatform:       service.QuotaPlatform(ctx, apiKey),
		SubscriptionBilling: subscription != nil,
		PricingAt:           requestStart,
		NextPollAt:          now.Add(5 * time.Second),
		PollDeadlineAt:      requestStart.Add(24 * time.Hour),
		RequestPayloadHash:  service.HashUsageRequestPayload(body),
		InboundEndpoint:     GetInboundEndpoint(c),
		UpstreamEndpoint:    firstNonEmptyString(result.UpstreamEndpoint, GetUpstreamEndpoint(c, account.Platform)),
	}

	attemptCount := 0
	for attempt := 1; attempt <= 2; attempt++ {
		attemptCount++
		if _, err := h.seedanceTasks.Create(ctx, input); err == nil {
			return nil
		}
		if attempt == 1 {
			timer := time.NewTimer(seedanceTaskPersistenceRetryDelay)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				attempt = 2
			case <-timer.C:
			}
		}
	}

	reqLog.Error("seedance_task_persistence_failed",
		zap.String("provider", service.AsyncVideoBillingProviderSeedance),
		zap.String("error_code", "seedance_task_persistence_failed"),
		zap.String("task_id", result.ResponseID),
		zap.Int64("account_id", account.ID),
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
		zap.Int("attempt_count", attemptCount),
		zap.Int64("elapsed_ms", max(time.Since(persistenceStarted).Milliseconds(), 0)),
		zap.String("status", "persistence_failed"),
	)
	return errSeedanceTaskPersistence
}

func (h *OpenAIGatewayHandler) writeSeedanceResponse(c *gin.Context, response *service.SeedanceUpstreamResponse) {
	if c == nil || response == nil {
		return
	}
	responseheaders.WriteFilteredHeaders(c.Writer.Header(), response.Header, seedanceResponseHeaderFilter(h.cfg))
	contentType := strings.TrimSpace(response.Header.Get("Content-Type"))
	if contentType == "" {
		contentType = "application/json"
	}
	c.Data(response.StatusCode, contentType, response.Body)
}

func (h *OpenAIGatewayHandler) forwardSeedanceStatusObserved(
	ctx context.Context,
	c *gin.Context,
	account *service.Account,
	taskID string,
) (*service.OpenAIForwardResult, *service.SeedanceUpstreamResponse, error) {
	upstreamStarted := time.Now()
	response, err := h.gatewayService.GetSeedanceTask(ctx, account, taskID)
	service.SetOpsLatencyMs(c, service.OpsUpstreamLatencyMsKey, time.Since(upstreamStarted).Milliseconds())
	if err != nil {
		var upstreamErr *service.SeedanceUpstreamError
		if errors.As(err, &upstreamErr) && upstreamErr.SafeCode == "response_too_large" {
			service.SetOpsUpstreamError(c, http.StatusBadGateway, "upstream response too large", "")
			h.errorResponse(c, http.StatusBadGateway, "upstream_error", "Upstream response too large")
		} else if response != nil && response.StatusCode >= http.StatusMultipleChoices {
			h.writeSeedanceResponse(c, response)
		}
		return nil, response, err
	}
	if response == nil || response.Result == nil {
		return nil, response, errors.New("seedance status response is incomplete")
	}
	h.writeSeedanceResponse(c, response)
	return response.Result, response, nil
}

func (h *OpenAIGatewayHandler) forwardSeedanceDeleteProtected(
	ctx context.Context,
	c *gin.Context,
	reqLog *zap.Logger,
	account *service.Account,
	task *service.AsyncVideoBillingTask,
	taskID string,
) (*service.OpenAIForwardResult, error) {
	if h == nil || h.seedanceTasks == nil || h.seedanceSettlement == nil || account == nil || task == nil {
		err := errors.New("seedance delete dependencies are unavailable")
		h.writeSeedanceDeleteRetryableError(c, err)
		return nil, err
	}

	now := time.Now()
	claimed := *task
	hasLease := false
	switch task.Status {
	case service.AsyncVideoBillingStatusPending:
		claimedTask, err := h.seedanceTasks.ClaimOwned(
			ctx,
			task.ID,
			task.UserID,
			task.APIKeyID,
			now,
			seedanceDeleteLeaseDuration,
		)
		if err != nil || claimedTask == nil {
			if err == nil {
				err = service.ErrAsyncVideoBillingTaskNotFound
			}
			h.writeSeedanceDeleteRetryableError(c, err)
			return nil, err
		}
		claimed = *claimedTask
		hasLease = true
	case service.AsyncVideoBillingStatusSettled,
		service.AsyncVideoBillingStatusFailed,
		service.AsyncVideoBillingStatusCancelled:
		// These terminal states no longer need a lease to protect billing.
	case service.AsyncVideoBillingStatusDeadLetter:
		err := errors.New("seedance task billing is unresolved")
		h.writeSeedanceDeleteRetryableError(c, err)
		return nil, err
	default:
		err := errors.New("seedance task billing status is invalid")
		h.writeSeedanceDeleteRetryableError(c, err)
		return nil, err
	}

	statusResponse, err := h.gatewayService.GetSeedanceTask(ctx, account, taskID)
	if err != nil {
		if hasLease {
			h.releaseSeedanceDeleteLease(ctx, reqLog, claimed, seedanceDeleteErrorCode(err))
		}
		h.writeSeedanceDeleteRetryableError(c, err)
		return nil, err
	}
	if statusResponse == nil || statusResponse.Result == nil {
		err = errors.New("seedance delete status response is incomplete")
		if hasLease {
			h.releaseSeedanceDeleteLease(ctx, reqLog, claimed, "seedance_delete_status_incomplete")
		}
		h.writeSeedanceDeleteRetryableError(c, err)
		return nil, err
	}

	if statusResponse.State == service.SeedanceObservedSucceeded && task.Status != service.AsyncVideoBillingStatusSettled {
		if !hasLease {
			err = errors.New("seedance completed task is not claimable for settlement")
			h.writeSeedanceDeleteRetryableError(c, err)
			return nil, err
		}
		settlement := h.seedanceSettlement.ProcessClaimed(ctx, claimed, statusResponse, now)
		hasLease = false
		if !settlement.Settled {
			if settlement.Fenced && h.seedanceDeleteTaskIsSettled(ctx, claimed) {
				// A concurrent execution completed the same idempotent settlement.
			} else {
				err = settlement.Err
				if err == nil {
					err = errors.New("seedance settlement did not complete")
				}
				h.writeSeedanceDeleteRetryableError(c, err)
				return nil, err
			}
		}
	}

	deleteResponse, err := h.gatewayService.DeleteSeedanceTask(ctx, account, taskID)
	if err != nil {
		if hasLease {
			h.releaseSeedanceDeleteLease(ctx, reqLog, claimed, seedanceDeleteErrorCode(err))
		}
		if deleteResponse != nil && deleteResponse.StatusCode >= http.StatusMultipleChoices {
			h.writeSeedanceResponse(c, deleteResponse)
		} else {
			h.writeSeedanceDeleteRetryableError(c, err)
		}
		return nil, err
	}
	if deleteResponse == nil || deleteResponse.Result == nil {
		err = errors.New("seedance delete response is incomplete")
		if hasLease {
			h.releaseSeedanceDeleteLease(ctx, reqLog, claimed, "seedance_delete_response_incomplete")
		}
		h.writeSeedanceDeleteRetryableError(c, err)
		return nil, err
	}

	if hasLease {
		terminalStatus := service.AsyncVideoBillingStatusCancelled
		switch statusResponse.State {
		case service.SeedanceObservedFailed:
			terminalStatus = service.AsyncVideoBillingStatusFailed
		case service.SeedanceObservedCancelled:
			terminalStatus = service.AsyncVideoBillingStatusCancelled
		}
		if err = h.seedanceTasks.MarkTerminal(
			ctx,
			seedanceTaskLeaseForHandler(claimed),
			terminalStatus,
			"",
			time.Now(),
		); err != nil {
			h.writeSeedanceDeleteRetryableError(c, err)
			return nil, err
		}
	}

	h.writeSeedanceResponse(c, deleteResponse)
	return deleteResponse.Result, nil
}

func (h *OpenAIGatewayHandler) releaseSeedanceDeleteLease(
	ctx context.Context,
	reqLog *zap.Logger,
	task service.AsyncVideoBillingTask,
	errorCode string,
) {
	err := h.seedanceTasks.MarkRetry(
		ctx,
		seedanceTaskLeaseForHandler(task),
		time.Now().Add(seedanceDeleteRetryDelay),
		errorCode,
		task.MissingTokenChecks,
	)
	if err != nil && reqLog != nil {
		reqLog.Warn("seedance_delete_lease_release_failed",
			zap.Int64("task_id", task.ID),
			zap.Int64("user_id", task.UserID),
			zap.Int64("api_key_id", task.APIKeyID),
			zap.String("error_code", errorCode),
		)
	}
}

func (h *OpenAIGatewayHandler) seedanceDeleteTaskIsSettled(ctx context.Context, task service.AsyncVideoBillingTask) bool {
	current, err := h.seedanceTasks.GetOwned(
		ctx,
		service.AsyncVideoBillingProviderSeedance,
		task.UpstreamTaskID,
		task.UserID,
		task.APIKeyID,
	)
	return err == nil && current != nil && current.Status == service.AsyncVideoBillingStatusSettled
}

func seedanceTaskLeaseForHandler(task service.AsyncVideoBillingTask) service.AsyncVideoBillingLease {
	lease := service.AsyncVideoBillingLease{TaskID: task.ID, Epoch: task.LeaseEpoch}
	if task.LeaseToken != nil {
		lease.Token = *task.LeaseToken
	}
	return lease
}

func seedanceDeleteErrorCode(err error) string {
	var upstreamErr *service.SeedanceUpstreamError
	if errors.As(err, &upstreamErr) && strings.TrimSpace(upstreamErr.SafeCode) != "" {
		return strings.TrimSpace(upstreamErr.SafeCode)
	}
	return "seedance_delete_upstream_failed"
}

func (h *OpenAIGatewayHandler) writeSeedanceDeleteRetryableError(c *gin.Context, err error) {
	if c == nil || service.IsResponseCommitted(c) {
		return
	}
	var upstreamErr *service.SeedanceUpstreamError
	if errors.As(err, &upstreamErr) && upstreamErr.RetryDelay > 0 {
		seconds := int((upstreamErr.RetryDelay + time.Second - 1) / time.Second)
		c.Header("Retry-After", strconv.Itoa(max(seconds, 1)))
	}
	h.errorResponse(c, http.StatusServiceUnavailable, "upstream_error", "Seedance task status is temporarily unavailable; retry deletion later")
}

func seedanceResponseHeaderFilter(cfg *config.Config) *responseheaders.CompiledHeaderFilter {
	if cfg == nil {
		return nil
	}
	return responseheaders.CompileHeaderFilter(cfg.Security.ResponseHeaders)
}

// Ark reports actual completion tokens. Never infer tokens from duration or use
// Grok's per-second video tariff. Repeated polls share the durable task dedup key.
func prepareSeedanceCompletionBilling(ctx context.Context, h *OpenAIGatewayHandler, key *service.APIKey, subject middleware.AuthSubject, taskID string, result *service.OpenAIForwardResult) *service.OpenAIForwardResult {
	if result == nil || result.Usage.OutputTokens <= 0 {
		return nil
	}
	pending, err := h.gatewayService.LoadGrokVideoPendingBilling(ctx, taskID, subject.UserID, key.ID)
	if err != nil || pending == nil {
		return nil
	}
	claimed, err := h.gatewayService.ClaimGrokVideoBilling(ctx, taskID, subject.UserID, key.ID)
	if err != nil || !claimed {
		return nil
	}
	merged := *result
	merged.Model = pending.Model
	merged.BillingModel = firstNonEmptyString(pending.BillingModel, pending.Model)
	merged.UpstreamModel = firstNonEmptyString(pending.UpstreamModel, result.UpstreamModel)
	merged.RequestID = service.StableGrokVideoBillingRequestID(taskID)
	merged.ResponseID = taskID
	merged.Duration = service.GrokVideoE2EDuration(pending.CreatedAt, time.Now())
	return &merged
}
