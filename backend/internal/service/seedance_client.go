package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type SeedanceObservedState string

const (
	SeedanceObservedPending   SeedanceObservedState = "pending"
	SeedanceObservedSucceeded SeedanceObservedState = "succeeded"
	SeedanceObservedFailed    SeedanceObservedState = "failed"
	SeedanceObservedCancelled SeedanceObservedState = "cancelled"
)

type SeedanceUpstreamResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Result     *OpenAIForwardResult
	State      SeedanceObservedState
}

type SeedanceTaskClient interface {
	CreateSeedanceTask(context.Context, *Account, []byte) (*SeedanceUpstreamResponse, error)
	GetSeedanceTask(context.Context, *Account, string) (*SeedanceUpstreamResponse, error)
	DeleteSeedanceTask(context.Context, *Account, string) (*SeedanceUpstreamResponse, error)
}

type SeedanceUpstreamErrorKind string

const (
	SeedanceUpstreamErrorAuth        SeedanceUpstreamErrorKind = "auth"
	SeedanceUpstreamErrorRateLimited SeedanceUpstreamErrorKind = "rate_limited"
	SeedanceUpstreamErrorNotFound    SeedanceUpstreamErrorKind = "not_found"
	SeedanceUpstreamErrorTemporary   SeedanceUpstreamErrorKind = "temporary"
	SeedanceUpstreamErrorProtocol    SeedanceUpstreamErrorKind = "protocol"
)

type SeedanceUpstreamError struct {
	Kind       SeedanceUpstreamErrorKind
	StatusCode int
	SafeCode   string
	RetryDelay time.Duration
}

func (e *SeedanceUpstreamError) Error() string {
	if e == nil {
		return "seedance upstream error"
	}
	return fmt.Sprintf("seedance upstream %s: status=%d code=%s", e.Kind, e.StatusCode, e.SafeCode)
}

func (s *OpenAIGatewayService) CreateSeedanceTask(ctx context.Context, account *Account, body []byte) (*SeedanceUpstreamResponse, error) {
	return s.doSeedanceTask(ctx, account, SeedanceEndpointCreate, "", body)
}

func (s *OpenAIGatewayService) GetSeedanceTask(ctx context.Context, account *Account, taskID string) (*SeedanceUpstreamResponse, error) {
	return s.doSeedanceTask(ctx, account, SeedanceEndpointStatus, taskID, nil)
}

func (s *OpenAIGatewayService) DeleteSeedanceTask(ctx context.Context, account *Account, taskID string) (*SeedanceUpstreamResponse, error) {
	return s.doSeedanceTask(ctx, account, SeedanceEndpointDelete, taskID, nil)
}

func (s *OpenAIGatewayService) doSeedanceTask(ctx context.Context, account *Account, endpoint GrokMediaEndpoint, taskID string, body []byte) (*SeedanceUpstreamResponse, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, &SeedanceUpstreamError{Kind: SeedanceUpstreamErrorTemporary, SafeCode: "transport_unavailable"}
	}
	if account == nil || !account.SupportsOpenAIEndpointCapability(OpenAIEndpointCapabilitySeedance) || !endpoint.IsSeedance() {
		return nil, fmt.Errorf("seedance requires an OpenAI API key account with a custom base URL")
	}
	base, err := s.validateUpstreamBaseURL(account.GetCredential("base_url"))
	if err != nil {
		return nil, err
	}
	taskID = strings.TrimPrefix(strings.TrimSpace(taskID), "seedance:")
	target, err := buildSeedanceURL(base, endpoint, taskID)
	if err != nil {
		return nil, err
	}

	model, upstreamModel := "", ""
	method := http.MethodGet
	switch endpoint {
	case SeedanceEndpointCreate:
		info, parseErr := ParseSeedanceRequest(body)
		if parseErr != nil {
			return nil, parseErr
		}
		model = info.Model
		upstreamModel = account.GetMappedModel(model)
		body, err = sjson.SetBytes(body, "model", upstreamModel)
		if err != nil {
			return nil, err
		}
		method = http.MethodPost
	case SeedanceEndpointDelete:
		method = http.MethodDelete
	}

	token := strings.TrimSpace(account.GetCredential("api_key"))
	if token == "" {
		return nil, fmt.Errorf("seedance account missing api_key")
	}
	request, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	account.ApplyHeaderOverrides(request.Header)
	proxy := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxy = account.Proxy.URL()
	}

	started := time.Now()
	upstreamResponse, err := s.httpUpstream.Do(request, proxy, account.ID, account.Concurrency)
	if err != nil {
		return nil, &SeedanceUpstreamError{Kind: SeedanceUpstreamErrorTemporary, SafeCode: "transport_error"}
	}
	defer func() { _ = upstreamResponse.Body.Close() }()
	response := &SeedanceUpstreamResponse{
		StatusCode: upstreamResponse.StatusCode,
		Header:     upstreamResponse.Header.Clone(),
	}
	responseBody, err := readUpstreamResponseBodyLimited(upstreamResponse.Body, resolveUpstreamResponseReadLimit(s.cfg))
	if err != nil {
		if errors.Is(err, ErrUpstreamResponseBodyTooLarge) {
			return response, &SeedanceUpstreamError{Kind: SeedanceUpstreamErrorProtocol, StatusCode: upstreamResponse.StatusCode, SafeCode: "response_too_large"}
		}
		return response, &SeedanceUpstreamError{Kind: SeedanceUpstreamErrorTemporary, StatusCode: upstreamResponse.StatusCode, SafeCode: "response_read_error"}
	}
	response.Body = responseBody
	response.State = normalizeSeedanceObservedState(responseBody)
	if upstreamResponse.StatusCode >= http.StatusMultipleChoices {
		return response, classifySeedanceUpstreamStatus(upstreamResponse.StatusCode, upstreamResponse.Header, responseBody)
	}

	result := &OpenAIForwardResult{
		Model:           model,
		BillingModel:    model,
		UpstreamModel:   upstreamModel,
		Duration:        time.Since(started),
		ResponseHeaders: upstreamResponse.Header.Clone(),
	}
	switch endpoint {
	case SeedanceEndpointCreate:
		id := strings.TrimSpace(gjson.GetBytes(responseBody, "id").String())
		if id == "" {
			return response, &SeedanceUpstreamError{Kind: SeedanceUpstreamErrorProtocol, StatusCode: upstreamResponse.StatusCode, SafeCode: "missing_task_id"}
		}
		result.ResponseID = SeedanceTaskKey(id)
	case SeedanceEndpointStatus:
		result.ResponseID = SeedanceTaskKey(taskID)
		result.UpstreamModel = strings.TrimSpace(gjson.GetBytes(responseBody, "model").String())
		if response.State == SeedanceObservedSucceeded {
			result.Usage.OutputTokens = max(0, int(gjson.GetBytes(responseBody, "usage.completion_tokens").Int()))
		}
	}
	response.Result = result
	return response, nil
}

func normalizeSeedanceObservedState(body []byte) SeedanceObservedState {
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "status").String())) {
	case "succeeded":
		return SeedanceObservedSucceeded
	case "failed", "expired":
		return SeedanceObservedFailed
	case "cancelled":
		return SeedanceObservedCancelled
	default:
		return SeedanceObservedPending
	}
}

func classifySeedanceUpstreamStatus(statusCode int, header http.Header, body []byte) *SeedanceUpstreamError {
	kind := SeedanceUpstreamErrorProtocol
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		kind = SeedanceUpstreamErrorAuth
	case statusCode == http.StatusNotFound:
		kind = SeedanceUpstreamErrorNotFound
	case statusCode == http.StatusTooManyRequests:
		kind = SeedanceUpstreamErrorRateLimited
	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusTooEarly || statusCode >= http.StatusInternalServerError:
		kind = SeedanceUpstreamErrorTemporary
	}
	code := safeSeedanceErrorCode(gjson.GetBytes(body, "error.code").String())
	if code == "" {
		code = fmt.Sprintf("http_%d", statusCode)
	}
	return &SeedanceUpstreamError{
		Kind:       kind,
		StatusCode: statusCode,
		SafeCode:   code,
		RetryDelay: parseSeedanceRetryAfter(header.Get("Retry-After"), time.Now()),
	}
}

func safeSeedanceErrorCode(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	builder.Grow(min(len(value), 80))
	for _, char := range value {
		if builder.Len() >= 80 {
			break
		}
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			_ = builder.WriteByte(byte(char))
		} else {
			_ = builder.WriteByte('_')
		}
	}
	return builder.String()
}

func parseSeedanceRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	when, err := http.ParseTime(value)
	if err != nil || !when.After(now) {
		return 0
	}
	return when.Sub(now)
}
