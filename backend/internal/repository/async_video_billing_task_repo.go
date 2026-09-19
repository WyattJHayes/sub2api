package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

const asyncVideoBillingTaskColumns = `
id, provider, upstream_task_id, task_key,
user_id, api_key_id, group_id, account_id, subscription_id,
model, billing_model, upstream_model, original_model, quota_platform,
subscription_billing, pricing_at, request_payload_hash,
inbound_endpoint, upstream_endpoint, status, next_poll_at, poll_deadline_at,
attempt_count, missing_token_checks, lease_token, lease_epoch, lease_expires_at,
last_error_code, last_error_at, terminal_at, settled_at, created_at, updated_at`

type asyncVideoBillingTaskRepository struct {
	db *sql.DB
}

func NewAsyncVideoBillingTaskRepository(db *sql.DB) service.AsyncVideoBillingTaskRepository {
	return &asyncVideoBillingTaskRepository{db: db}
}

type asyncVideoBillingTaskScanner interface {
	Scan(dest ...any) error
}

func scanAsyncVideoBillingTask(scanner asyncVideoBillingTaskScanner) (*service.AsyncVideoBillingTask, error) {
	var task service.AsyncVideoBillingTask
	var groupID, subscriptionID sql.NullInt64
	var upstreamModel, originalModel, quotaPlatform sql.NullString
	var requestPayloadHash, inboundEndpoint, upstreamEndpoint sql.NullString
	var leaseToken uuid.NullUUID
	var leaseExpiresAt, lastErrorAt, terminalAt, settledAt sql.NullTime
	var lastErrorCode sql.NullString

	err := scanner.Scan(
		&task.ID, &task.Provider, &task.UpstreamTaskID, &task.TaskKey,
		&task.UserID, &task.APIKeyID, &groupID, &task.AccountID, &subscriptionID,
		&task.Model, &task.BillingModel, &upstreamModel, &originalModel, &quotaPlatform,
		&task.SubscriptionBilling, &task.PricingAt, &requestPayloadHash,
		&inboundEndpoint, &upstreamEndpoint, &task.Status, &task.NextPollAt, &task.PollDeadlineAt,
		&task.AttemptCount, &task.MissingTokenChecks, &leaseToken, &task.LeaseEpoch, &leaseExpiresAt,
		&lastErrorCode, &lastErrorAt, &terminalAt, &settledAt, &task.CreatedAt, &task.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if groupID.Valid {
		task.GroupID = &groupID.Int64
	}
	if subscriptionID.Valid {
		task.SubscriptionID = &subscriptionID.Int64
	}
	task.UpstreamModel = upstreamModel.String
	task.OriginalModel = originalModel.String
	task.QuotaPlatform = quotaPlatform.String
	task.RequestPayloadHash = requestPayloadHash.String
	task.InboundEndpoint = inboundEndpoint.String
	task.UpstreamEndpoint = upstreamEndpoint.String
	if leaseToken.Valid {
		token := leaseToken.UUID
		task.LeaseToken = &token
	}
	if leaseExpiresAt.Valid {
		value := leaseExpiresAt.Time
		task.LeaseExpiresAt = &value
	}
	task.LastErrorCode = lastErrorCode.String
	if lastErrorAt.Valid {
		value := lastErrorAt.Time
		task.LastErrorAt = &value
	}
	if terminalAt.Valid {
		value := terminalAt.Time
		task.TerminalAt = &value
	}
	if settledAt.Valid {
		value := settledAt.Time
		task.SettledAt = &value
	}
	return &task, nil
}

func nullableAsyncVideoBillingString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (r *asyncVideoBillingTaskRepository) Create(ctx context.Context, input service.CreateAsyncVideoBillingTaskInput) (*service.AsyncVideoBillingTask, error) {
	if err := input.Normalize(); err != nil {
		return nil, err
	}
	row := r.db.QueryRowContext(ctx, `
INSERT INTO async_video_billing_tasks (
    provider, upstream_task_id, task_key,
    user_id, api_key_id, group_id, account_id, subscription_id,
    model, billing_model, upstream_model, original_model, quota_platform,
    subscription_billing, pricing_at, request_payload_hash,
    inbound_endpoint, upstream_endpoint, next_poll_at, poll_deadline_at
) VALUES (
    $1, $2, $3,
    $4, $5, $6, $7, $8,
    $9, $10, $11, $12, $13,
    $14, $15, $16,
    $17, $18, $19, $20
)
ON CONFLICT (provider, upstream_task_id, user_id, api_key_id)
DO UPDATE SET updated_at = async_video_billing_tasks.updated_at
RETURNING `+asyncVideoBillingTaskColumns,
		input.Provider, input.UpstreamTaskID, input.TaskKey,
		input.UserID, input.APIKeyID, input.GroupID, input.AccountID, input.SubscriptionID,
		input.Model, input.BillingModel, nullableAsyncVideoBillingString(input.UpstreamModel), nullableAsyncVideoBillingString(input.OriginalModel), nullableAsyncVideoBillingString(input.QuotaPlatform),
		input.SubscriptionBilling, input.PricingAt, nullableAsyncVideoBillingString(input.RequestPayloadHash),
		nullableAsyncVideoBillingString(input.InboundEndpoint), nullableAsyncVideoBillingString(input.UpstreamEndpoint), input.NextPollAt, input.PollDeadlineAt,
	)
	return scanAsyncVideoBillingTask(row)
}

func (r *asyncVideoBillingTaskRepository) GetOwned(ctx context.Context, provider, upstreamTaskID string, userID, apiKeyID int64) (*service.AsyncVideoBillingTask, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+asyncVideoBillingTaskColumns+`
FROM async_video_billing_tasks
WHERE provider = $1 AND upstream_task_id = $2 AND user_id = $3 AND api_key_id = $4`,
		strings.TrimSpace(provider), strings.TrimSpace(upstreamTaskID), userID, apiKeyID)
	task, err := scanAsyncVideoBillingTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrAsyncVideoBillingTaskNotFound
	}
	return task, err
}

func (r *asyncVideoBillingTaskRepository) ClaimDue(ctx context.Context, provider string, now time.Time, limit int, leaseDuration time.Duration) ([]service.AsyncVideoBillingTask, error) {
	provider = strings.TrimSpace(provider)
	if provider != service.AsyncVideoBillingProviderSeedance {
		return nil, service.ErrAsyncVideoBillingProviderUnsupported
	}
	if now.IsZero() || limit <= 0 || leaseDuration <= 0 {
		return nil, fmt.Errorf("invalid async video billing claim parameters")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
SELECT id
FROM async_video_billing_tasks
WHERE provider = $1
  AND status = 'pending'
  AND next_poll_at <= $2
  AND (lease_expires_at IS NULL OR lease_expires_at <= $2)
ORDER BY next_poll_at, id
FOR UPDATE SKIP LOCKED
LIMIT $3`, provider, now, limit)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	claimed := make([]service.AsyncVideoBillingTask, 0, len(ids))
	for _, id := range ids {
		token := uuid.New()
		row := tx.QueryRowContext(ctx, `
UPDATE async_video_billing_tasks
SET lease_token = $2,
    lease_epoch = lease_epoch + 1,
    lease_expires_at = $3,
    attempt_count = attempt_count + 1,
    updated_at = $4
WHERE id = $1
RETURNING `+asyncVideoBillingTaskColumns, id, token, now.Add(leaseDuration), now)
		task, err := scanAsyncVideoBillingTask(row)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, *task)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func (r *asyncVideoBillingTaskRepository) ClaimOwned(ctx context.Context, taskID, userID, apiKeyID int64, now time.Time, leaseDuration time.Duration) (*service.AsyncVideoBillingTask, error) {
	if taskID <= 0 || userID <= 0 || apiKeyID <= 0 || now.IsZero() || leaseDuration <= 0 {
		return nil, fmt.Errorf("invalid async video billing owned claim parameters")
	}
	token := uuid.New()
	row := r.db.QueryRowContext(ctx, `
UPDATE async_video_billing_tasks
SET lease_token = $5,
    lease_epoch = lease_epoch + 1,
    lease_expires_at = $6,
    attempt_count = attempt_count + 1,
    updated_at = $4
WHERE id = $1
  AND user_id = $2
  AND api_key_id = $3
  AND status = 'pending'
  AND (lease_expires_at IS NULL OR lease_expires_at <= $4)
RETURNING `+asyncVideoBillingTaskColumns,
		taskID, userID, apiKeyID, now, token, now.Add(leaseDuration))
	task, err := scanAsyncVideoBillingTask(row)
	if err == nil {
		return task, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	var exists bool
	if queryErr := r.db.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM async_video_billing_tasks WHERE id = $1 AND user_id = $2 AND api_key_id = $3
)`, taskID, userID, apiKeyID).Scan(&exists); queryErr != nil {
		return nil, queryErr
	}
	if !exists {
		return nil, service.ErrAsyncVideoBillingTaskNotFound
	}
	return nil, service.ErrAsyncVideoBillingTaskFenced
}

func (r *asyncVideoBillingTaskRepository) MarkRetry(ctx context.Context, lease service.AsyncVideoBillingLease, nextPollAt time.Time, errorCode string, missingTokenChecks int) error {
	if !lease.Valid() || nextPollAt.IsZero() || missingTokenChecks < 0 {
		return fmt.Errorf("invalid async video billing retry transition")
	}
	return r.execFencedTransition(ctx, lease, `
UPDATE async_video_billing_tasks
SET next_poll_at = $4,
    missing_token_checks = $5,
    last_error_code = $6,
    last_error_at = NOW(),
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = NOW()
WHERE id = $1 AND status = 'pending' AND lease_token = $2 AND lease_epoch = $3`,
		nextPollAt, missingTokenChecks, boundedAsyncVideoBillingErrorCode(errorCode))
}

func (r *asyncVideoBillingTaskRepository) MarkTerminal(ctx context.Context, lease service.AsyncVideoBillingLease, status, errorCode string, terminalAt time.Time) error {
	status = strings.TrimSpace(status)
	if !lease.Valid() || terminalAt.IsZero() || !isAsyncVideoBillingTerminalStatus(status) {
		return fmt.Errorf("invalid async video billing terminal transition")
	}
	return r.execFencedTransition(ctx, lease, `
UPDATE async_video_billing_tasks
SET status = $4,
    last_error_code = $5,
    last_error_at = CASE WHEN $5 = '' THEN last_error_at ELSE $6 END,
    terminal_at = $6,
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = $6
WHERE id = $1 AND status = 'pending' AND lease_token = $2 AND lease_epoch = $3`,
		status, boundedAsyncVideoBillingErrorCode(errorCode), terminalAt)
}

func (r *asyncVideoBillingTaskRepository) MarkSettled(ctx context.Context, lease service.AsyncVideoBillingLease, settledAt time.Time) error {
	if !lease.Valid() || settledAt.IsZero() {
		return fmt.Errorf("invalid async video billing settled transition")
	}
	return r.execFencedTransition(ctx, lease, `
UPDATE async_video_billing_tasks
SET status = 'settled',
    terminal_at = $4,
    settled_at = $4,
    lease_token = NULL,
    lease_expires_at = NULL,
    updated_at = $4
WHERE id = $1 AND status = 'pending' AND lease_token = $2 AND lease_epoch = $3`, settledAt)
}

func (r *asyncVideoBillingTaskRepository) execFencedTransition(ctx context.Context, lease service.AsyncVideoBillingLease, query string, args ...any) error {
	params := []any{lease.TaskID, lease.Token, lease.Epoch}
	params = append(params, args...)
	result, err := r.db.ExecContext(ctx, query, params...)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return service.ErrAsyncVideoBillingTaskFenced
	}
	return nil
}

func isAsyncVideoBillingTerminalStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case service.AsyncVideoBillingStatusFailed, service.AsyncVideoBillingStatusCancelled, service.AsyncVideoBillingStatusDeadLetter:
		return true
	default:
		return false
	}
}

func boundedAsyncVideoBillingErrorCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(min(len(value), 80))
	for _, char := range value {
		if builder.Len() >= 80 {
			break
		}
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' {
			builder.WriteByte(byte(char))
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
}
