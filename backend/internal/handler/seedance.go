package handler

import (
	"context"
	"errors"
	"mime"
	"net/http"
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

var errSeedanceTaskPersistence = errors.New("seedance task persistence failed")

type seedanceSettlementObserver interface {
	ObserveOwned(
		context.Context,
		service.SeedanceTaskOwner,
		string,
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

	for attempt := 1; attempt <= 2; attempt++ {
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
		zap.String("error_code", "seedance_task_persistence_failed"),
		zap.String("task_key", result.ResponseID),
		zap.Int64("account_id", account.ID),
		zap.Int64("user_id", subject.UserID),
		zap.Int64("api_key_id", apiKey.ID),
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
