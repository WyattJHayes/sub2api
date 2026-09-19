package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	AsyncVideoBillingProviderSeedance = "seedance"
	AsyncVideoBillingStatusPending    = "pending"
	AsyncVideoBillingStatusSettled    = "settled"
	AsyncVideoBillingStatusFailed     = "failed"
	AsyncVideoBillingStatusCancelled  = "cancelled"
	AsyncVideoBillingStatusDeadLetter = "dead_letter"
)

var (
	ErrAsyncVideoBillingTaskNotFound        = errors.New("async video billing task not found")
	ErrAsyncVideoBillingTaskFenced          = errors.New("async video billing task lease fenced")
	ErrAsyncVideoBillingProviderUnsupported = errors.New("async video billing provider unsupported")
)

type AsyncVideoBillingLease struct {
	TaskID int64
	Token  uuid.UUID
	Epoch  int64
}

func (l AsyncVideoBillingLease) Valid() bool {
	return l.TaskID > 0 && l.Token != uuid.Nil && l.Epoch > 0
}

type AsyncVideoBillingTask struct {
	ID                                  int64
	Provider, UpstreamTaskID, TaskKey   string
	UserID, APIKeyID, AccountID         int64
	GroupID, SubscriptionID             *int64
	Model, BillingModel, UpstreamModel  string
	OriginalModel, QuotaPlatform        string
	SubscriptionBilling                 bool
	PricingAt                           time.Time
	RequestPayloadHash, InboundEndpoint string
	UpstreamEndpoint, Status            string
	NextPollAt, PollDeadlineAt          time.Time
	AttemptCount, MissingTokenChecks    int
	LeaseToken                          *uuid.UUID
	LeaseEpoch                          int64
	LeaseExpiresAt                      *time.Time
	LastErrorCode                       string
	LastErrorAt, TerminalAt, SettledAt  *time.Time
	CreatedAt, UpdatedAt                time.Time
}

type CreateAsyncVideoBillingTaskInput struct {
	Provider, UpstreamTaskID, TaskKey     string
	UserID, APIKeyID, AccountID           int64
	GroupID, SubscriptionID               *int64
	Model, BillingModel, UpstreamModel    string
	OriginalModel, QuotaPlatform          string
	SubscriptionBilling                   bool
	PricingAt, NextPollAt, PollDeadlineAt time.Time
	RequestPayloadHash, InboundEndpoint   string
	UpstreamEndpoint                      string
}

func (in *CreateAsyncVideoBillingTaskInput) Normalize() error {
	if in == nil {
		return fmt.Errorf("async video billing task input is nil")
	}
	in.Provider = strings.TrimSpace(in.Provider)
	in.UpstreamTaskID = strings.TrimSpace(in.UpstreamTaskID)
	in.Model = strings.TrimSpace(in.Model)
	in.BillingModel = strings.TrimSpace(in.BillingModel)
	in.UpstreamModel = strings.TrimSpace(in.UpstreamModel)
	in.OriginalModel = strings.TrimSpace(in.OriginalModel)
	in.QuotaPlatform = strings.TrimSpace(in.QuotaPlatform)
	in.RequestPayloadHash = strings.TrimSpace(in.RequestPayloadHash)
	in.InboundEndpoint = strings.TrimSpace(in.InboundEndpoint)
	in.UpstreamEndpoint = strings.TrimSpace(in.UpstreamEndpoint)

	if in.Provider != AsyncVideoBillingProviderSeedance {
		return ErrAsyncVideoBillingProviderUnsupported
	}
	if in.UpstreamTaskID == "" {
		return fmt.Errorf("async video billing upstream task ID is required")
	}
	if in.UserID <= 0 || in.APIKeyID <= 0 || in.AccountID <= 0 {
		return fmt.Errorf("async video billing owner IDs must be positive")
	}
	if in.GroupID != nil && *in.GroupID <= 0 {
		return fmt.Errorf("async video billing group ID must be positive")
	}
	if in.SubscriptionID != nil && *in.SubscriptionID <= 0 {
		return fmt.Errorf("async video billing subscription ID must be positive")
	}
	if in.Model == "" {
		return fmt.Errorf("async video billing model is required")
	}
	if in.PricingAt.IsZero() {
		return fmt.Errorf("async video billing pricing time is required")
	}
	if in.PollDeadlineAt.IsZero() || in.PollDeadlineAt.Before(in.PricingAt) {
		return fmt.Errorf("async video billing poll deadline must not precede pricing time")
	}
	if in.RequestPayloadHash != "" && !isLowerHexSHA256(in.RequestPayloadHash) {
		return fmt.Errorf("async video billing request payload hash must be lowercase SHA-256")
	}

	in.TaskKey = SeedanceTaskKey(in.UpstreamTaskID)
	if in.BillingModel == "" {
		in.BillingModel = in.Model
	}
	if in.NextPollAt.IsZero() {
		in.NextPollAt = in.PricingAt
	}
	return nil
}

type AsyncVideoBillingTaskRepository interface {
	Create(context.Context, CreateAsyncVideoBillingTaskInput) (*AsyncVideoBillingTask, error)
	GetOwned(ctx context.Context, provider, upstreamTaskID string, userID, apiKeyID int64) (*AsyncVideoBillingTask, error)
	ClaimDue(ctx context.Context, provider string, now time.Time, limit int, leaseDuration time.Duration) ([]AsyncVideoBillingTask, error)
	ClaimOwned(ctx context.Context, taskID, userID, apiKeyID int64, now time.Time, leaseDuration time.Duration) (*AsyncVideoBillingTask, error)
	MarkRetry(ctx context.Context, lease AsyncVideoBillingLease, nextPollAt time.Time, errorCode string, missingTokenChecks int) error
	MarkTerminal(ctx context.Context, lease AsyncVideoBillingLease, status, errorCode string, terminalAt time.Time) error
	MarkSettled(ctx context.Context, lease AsyncVideoBillingLease, settledAt time.Time) error
}
