# Seedance Durable Billing Reconciliation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist every accepted Seedance task before returning success, recover and settle it exactly once after restart or concurrent polling, and prevent deletion from erasing a completed but unbilled task.

**Architecture:** Add a provider-scoped PostgreSQL task ledger with leased, fenced state transitions; a pure Seedance upstream client that does not write Gin responses; and one settlement service shared by request handlers and a single-concurrency reconciler. Existing `usage_billing_dedup` remains the final money-side idempotency boundary, while the task ledger owns recovery, ownership isolation, retry, terminal state, and observability.

**Tech Stack:** Go 1.27, PostgreSQL, Ent-backed repositories plus bounded raw SQL for leasing, Gin, Google Wire, go-zero `TimingWheelService`, Testify, repository integration tests, existing OpenAI usage billing.

**Spec:** `docs/superpowers/specs/2026-09-19-seedance-durable-billing-design.md`

## Global Constraints

- Target branch remains the v0.2.7 customization line; this plan does not deploy or mutate production.
- Only `provider = 'seedance'` is enabled; Grok image/video behavior and Redis billing flow remain unchanged.
- Production remains at 2 GB RAM; defaults are one concurrent request and a claim batch of eight.
- Public Seedance URLs, request bodies, successful response bodies, status codes, and headers remain protocol-compatible.
- No prompt, media content, media URL, API key, Cookie, Authorization value, raw request body, or full upstream response may be stored or logged.
- Accepted create responses are not written to the client until the PostgreSQL task row is durable.
- `usage_billing_dedup(request_id, api_key_id)` is the final idempotency boundary for balance, quota, and usage side effects.
- API Key and subscription soft deletion must not exempt already-created tasks from settlement; physically missing ownership data must dead-letter.
- Every leased mutation must match task ID, `lease_token`, and `lease_epoch`; an expired worker must not overwrite a newer worker.
- Disabling the reconciler stops background claims only; GET settlement remains active and re-enabling resumes pending rows.

## Review Focus

1. A task ID reused across users or API keys must remain tenant-isolated; GET and DELETE must return the existing not-found shape without making an upstream request.
2. An upstream create accepted just before a database outage must not be reported as locally successful; persistence retries stay bounded and logs contain only safe identifiers.
3. A worker that loses its lease after billing succeeds must be fenced from terminal writes, while the replacement worker replays billing without a second charge.
4. A freshly created task may return transient 404, but repeated 404 after the consistency grace period must dead-letter rather than retry forever.
5. A successful upstream response without `completion_tokens` must be rechecked exactly three times and then dead-letter, never be marked settled or estimated from duration.

---

## File Map

**Create**

- `backend/migrations/239_async_video_billing_tasks.sql`: durable provider-scoped task ledger, constraints, and pending-claim index.
- `backend/internal/service/async_video_billing_task.go`: task model, status/error constants, lease value, repository contract, and create input.
- `backend/internal/repository/async_video_billing_task_repo.go`: idempotent insert, ownership lookup, due claims, owned claims, and fenced state transitions.
- `backend/internal/repository/async_video_billing_task_repo_integration_test.go`: concurrent claim, expiry recovery, fencing, uniqueness, and tenant isolation integration coverage.
- `backend/internal/repository/api_key_repo_test.go`: pin deleted-key lookup and edge hydration.
- `backend/internal/service/seedance_client.go`: pure create/get/delete client, bounded response reading, normalized terminal state, and upstream error classification.
- `backend/internal/service/seedance_client_test.go`: pure-client protocol, limit, and error classification tests.
- `backend/internal/service/seedance_task_settlement.go`: ownership hydration, observed-state policy, idempotent `RecordUsage`, and ledger transitions.
- `backend/internal/service/seedance_task_settlement_test.go`: settlement, soft-delete, retry, dead-letter, and money-idempotency tests.
- `backend/internal/service/seedance_reconciler_runtime.go`: scheduled, non-overlapping, cancelable batch reconciliation.
- `backend/internal/service/seedance_reconciler_runtime_test.go`: start/stop, restart recovery, dual-runtime, and disabled-mode tests.
- `backend/internal/handler/seedance_durable_billing_test.go`: create durability, GET race, DELETE ordering, and tenant isolation handler tests.

**Modify**

- `backend/internal/repository/migrations_schema_integration_test.go`: assert task columns, constraints, and indexes.
- `backend/internal/service/api_key_service.go`: add `GetByIDIncludeDeleted` to `APIKeyRepository`.
- `backend/internal/repository/api_key_repo.go`: implement soft-delete-inclusive API Key hydration.
- `backend/internal/service/seedance.go`: retain parsing/key helpers and make `ForwardSeedance` a compatibility adapter over the pure client.
- `backend/internal/service/seedance_test.go`: pin response compatibility and error classification.
- `backend/internal/handler/openai_gateway_handler.go`: hold durable Seedance dependencies without expanding every unit-test constructor.
- `backend/internal/handler/wire.go`: inject task repository and settlement service into the handler.
- `backend/internal/handler/seedance.go`: route create/get/delete through durable orchestration.
- `backend/internal/handler/grok_media.go`: isolate Seedance post-forward handling from unchanged Grok Redis handling.
- `backend/internal/config/config.go`: add defaults, bounds, and validation for `gateway.seedance_reconciler`.
- `backend/internal/config/config_test.go`: pin defaults and invalid configurations.
- `backend/internal/repository/wire.go`: provide the new repository.
- `backend/internal/service/wire.go`: provide settlement service and reconciler runtime.
- `backend/cmd/server/wire.go`: stop the runtime during cleanup.
- `backend/cmd/server/wire_gen.go`: regenerate with Wire; do not hand-edit.
- `backend/cmd/server/wire_gen_test.go`: update minimal cleanup wiring coverage.
- `docs/seedance-api.md`: replace the “no background polling” statement with durable reconciliation and operational behavior.
- Canonical YAML example found by `rg -l 'openai_ws:' . --glob '*.yaml'`: document resource-safe defaults.

---

### Task 1: Create the durable task ledger and schema contract

**Files:**
- Create: `backend/migrations/239_async_video_billing_tasks.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`

**Interfaces:**
- Consumes: PostgreSQL migration runner in `backend/internal/repository/migrations.go`.
- Produces: table `async_video_billing_tasks` and index `idx_async_video_billing_tasks_due` used by Task 3.

- [ ] **Step 1: Write the failing schema assertions**

Add this block to `TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate`:

```go
var tasksRegclass sql.NullString
require.NoError(t, tx.QueryRowContext(context.Background(),
    "SELECT to_regclass('public.async_video_billing_tasks')",
).Scan(&tasksRegclass))
require.True(t, tasksRegclass.Valid)
for _, column := range []struct {
    name, dataType string
    nullable bool
}{
    {"provider", "character varying", false},
    {"upstream_task_id", "character varying", false},
    {"task_key", "character varying", false},
    {"pricing_at", "timestamp with time zone", false},
    {"lease_token", "uuid", true},
    {"lease_epoch", "bigint", false},
    {"lease_expires_at", "timestamp with time zone", true},
    {"settled_at", "timestamp with time zone", true},
} {
    requireColumn(t, tx, "async_video_billing_tasks", column.name, column.dataType, 0, column.nullable)
}
requireIndex(t, tx, "async_video_billing_tasks", "async_video_billing_tasks_provider_upstream_owner_key")
requireIndex(t, tx, "async_video_billing_tasks", "idx_async_video_billing_tasks_due")
requireConstraintDefinitionContains(t, tx, "async_video_billing_tasks", "async_video_billing_tasks_status_check",
    "'pending'", "'settled'", "'failed'", "'cancelled'", "'dead_letter'")
```

- [ ] **Step 2: Run the schema test and verify it fails**

Run: `cd backend && go test -tags=integration ./internal/repository -run TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate -count=1`

Expected: FAIL because `public.async_video_billing_tasks` does not exist.

- [ ] **Step 3: Add the idempotent migration**

```sql
CREATE TABLE IF NOT EXISTS async_video_billing_tasks (
    id BIGSERIAL PRIMARY KEY,
    provider VARCHAR(32) NOT NULL,
    upstream_task_id VARCHAR(255) NOT NULL,
    task_key VARCHAR(320) NOT NULL,
    user_id BIGINT NOT NULL,
    api_key_id BIGINT NOT NULL,
    group_id BIGINT,
    account_id BIGINT NOT NULL,
    subscription_id BIGINT,
    model VARCHAR(255) NOT NULL,
    billing_model VARCHAR(255) NOT NULL,
    upstream_model VARCHAR(255),
    original_model VARCHAR(255),
    quota_platform VARCHAR(32),
    subscription_billing BOOLEAN NOT NULL DEFAULT FALSE,
    pricing_at TIMESTAMPTZ NOT NULL,
    request_payload_hash CHAR(64),
    inbound_endpoint VARCHAR(255),
    upstream_endpoint VARCHAR(255),
    status VARCHAR(24) NOT NULL DEFAULT 'pending',
    next_poll_at TIMESTAMPTZ NOT NULL,
    poll_deadline_at TIMESTAMPTZ NOT NULL,
    attempt_count INT NOT NULL DEFAULT 0,
    missing_token_checks INT NOT NULL DEFAULT 0,
    lease_token UUID,
    lease_epoch BIGINT NOT NULL DEFAULT 0,
    lease_expires_at TIMESTAMPTZ,
    last_error_code VARCHAR(80),
    last_error_at TIMESTAMPTZ,
    terminal_at TIMESTAMPTZ,
    settled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT async_video_billing_tasks_provider_check CHECK (provider IN ('seedance')),
    CONSTRAINT async_video_billing_tasks_status_check CHECK (status IN ('pending','settled','failed','cancelled','dead_letter')),
    CONSTRAINT async_video_billing_tasks_attempt_count_check CHECK (attempt_count >= 0),
    CONSTRAINT async_video_billing_tasks_missing_token_checks_check CHECK (missing_token_checks >= 0),
    CONSTRAINT async_video_billing_tasks_deadline_check CHECK (poll_deadline_at >= created_at)
);

CREATE UNIQUE INDEX IF NOT EXISTS async_video_billing_tasks_provider_upstream_owner_key
    ON async_video_billing_tasks(provider, upstream_task_id, user_id, api_key_id);

CREATE INDEX IF NOT EXISTS idx_async_video_billing_tasks_due
    ON async_video_billing_tasks(status, next_poll_at, lease_expires_at, id)
    WHERE status = 'pending';
```

Do not add ownership foreign keys or cascades.

- [ ] **Step 4: Re-run the schema test**

Run: `cd backend && go test -tags=integration ./internal/repository -run TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate -count=1`

Expected: PASS, including the second migration application inside the test.

- [ ] **Step 5: Commit the schema unit**

```bash
git add backend/migrations/239_async_video_billing_tasks.sql backend/internal/repository/migrations_schema_integration_test.go
git commit -m "feat(seedance): add durable billing task ledger"
```

---

### Task 2: Define task-domain types and the repository contract

**Files:**
- Create: `backend/internal/service/async_video_billing_task.go`
- Create: `backend/internal/service/async_video_billing_task_test.go`

**Interfaces:**
- Consumes: standard `context`, `time`, and `github.com/google/uuid`.
- Produces: task model, lease value, create input, status constants, errors, and `AsyncVideoBillingTaskRepository`.

- [ ] **Step 1: Write failing normalization and lease tests**

```go
//go:build unit

func TestCreateAsyncVideoBillingTaskInputNormalize(t *testing.T) {
    input := CreateAsyncVideoBillingTaskInput{Provider: "grok", UpstreamTaskID: "task", UserID: 1, APIKeyID: 2}
    require.ErrorIs(t, input.Normalize(), ErrAsyncVideoBillingProviderUnsupported)
    input.Provider = AsyncVideoBillingProviderSeedance
    input.AccountID = 3
    input.Model = " video "
    input.RequestPayloadHash = strings.Repeat("a", 64)
    input.PricingAt = time.Now()
    input.PollDeadlineAt = input.PricingAt.Add(time.Hour)
    require.NoError(t, input.Normalize())
    require.Equal(t, "video", input.Model)
    require.Equal(t, "video", input.BillingModel)
    require.Equal(t, "seedance:task", input.TaskKey)
}

func TestAsyncVideoBillingLeaseIsValid(t *testing.T) {
    require.False(t, (AsyncVideoBillingLease{}).Valid())
    require.True(t, (AsyncVideoBillingLease{TaskID: 1, Token: uuid.New(), Epoch: 1}).Valid())
}
```

- [ ] **Step 2: Run the unit test and verify it fails**

Run: `cd backend && go test -tags=unit ./internal/service -run 'Test(CreateAsyncVideoBillingTaskInputNormalize|AsyncVideoBillingLease)' -count=1`

Expected: FAIL because the types are undefined.

- [ ] **Step 3: Implement the focused domain file**

Define these exact public contracts:

```go
const (
    AsyncVideoBillingProviderSeedance = "seedance"
    AsyncVideoBillingStatusPending    = "pending"
    AsyncVideoBillingStatusSettled    = "settled"
    AsyncVideoBillingStatusFailed     = "failed"
    AsyncVideoBillingStatusCancelled  = "cancelled"
    AsyncVideoBillingStatusDeadLetter = "dead_letter"
)

var (
    ErrAsyncVideoBillingTaskNotFound = errors.New("async video billing task not found")
    ErrAsyncVideoBillingTaskFenced = errors.New("async video billing task lease fenced")
    ErrAsyncVideoBillingProviderUnsupported = errors.New("async video billing provider unsupported")
)

type AsyncVideoBillingLease struct { TaskID int64; Token uuid.UUID; Epoch int64 }
func (l AsyncVideoBillingLease) Valid() bool { return l.TaskID > 0 && l.Token != uuid.Nil && l.Epoch > 0 }

type AsyncVideoBillingTask struct {
    ID int64
    Provider, UpstreamTaskID, TaskKey string
    UserID, APIKeyID, AccountID int64
    GroupID, SubscriptionID *int64
    Model, BillingModel, UpstreamModel, OriginalModel, QuotaPlatform string
    SubscriptionBilling bool
    PricingAt time.Time
    RequestPayloadHash, InboundEndpoint, UpstreamEndpoint, Status string
    NextPollAt, PollDeadlineAt time.Time
    AttemptCount, MissingTokenChecks int
    LeaseToken *uuid.UUID
    LeaseEpoch int64
    LeaseExpiresAt *time.Time
    LastErrorCode string
    LastErrorAt, TerminalAt, SettledAt *time.Time
    CreatedAt, UpdatedAt time.Time
}

type CreateAsyncVideoBillingTaskInput struct {
    Provider, UpstreamTaskID, TaskKey string
    UserID, APIKeyID, AccountID int64
    GroupID, SubscriptionID *int64
    Model, BillingModel, UpstreamModel, OriginalModel, QuotaPlatform string
    SubscriptionBilling bool
    PricingAt, NextPollAt, PollDeadlineAt time.Time
    RequestPayloadHash, InboundEndpoint, UpstreamEndpoint string
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
```

`Normalize` trims strings, accepts only Seedance, validates positive IDs and a lowercase 64-character hex hash when present, derives `TaskKey` with `SeedanceTaskKey`, fills `BillingModel` from `Model`, and rejects a deadline before `PricingAt`.

- [ ] **Step 4: Run the domain tests**

Run: `cd backend && go test -tags=unit ./internal/service -run 'Test(CreateAsyncVideoBillingTaskInputNormalize|AsyncVideoBillingLease)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit the domain contract**

```bash
git add backend/internal/service/async_video_billing_task.go backend/internal/service/async_video_billing_task_test.go
git commit -m "feat(seedance): define durable billing task contract"
```

---

### Task 3: Implement idempotent persistence, leases, and fencing

**Files:**
- Create: `backend/internal/repository/async_video_billing_task_repo.go`
- Create: `backend/internal/repository/async_video_billing_task_repo_integration_test.go`
- Modify: `backend/internal/repository/wire.go`

**Interfaces:**
- Consumes: Task 2 contracts and the Task 1 table.
- Produces: `NewAsyncVideoBillingTaskRepository(db *sql.DB) service.AsyncVideoBillingTaskRepository`.

- [ ] **Step 1: Write failing integration tests**

```go
func TestAsyncVideoBillingTaskRepositoryCreateIsIdempotentAndOwnerScoped(t *testing.T) {
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
    _, err = repo.GetOwned(context.Background(), "seedance", "same-upstream", 102, 201)
    require.ErrorIs(t, err, service.ErrAsyncVideoBillingTaskNotFound)
}

func TestAsyncVideoBillingTaskRepositoryLeaseExpiryAndFencing(t *testing.T) {
    // Release two goroutines together; assert their total claims equal one.
    // Claim again after expiry; assert higher epoch and different token.
    // MarkRetry with the old lease must return ErrAsyncVideoBillingTaskFenced.
}
```

- [ ] **Step 2: Run integration tests and verify they fail**

Run: `cd backend && go test -tags=integration ./internal/repository -run TestAsyncVideoBillingTaskRepository -count=1`

Expected: FAIL because the repository is undefined.

- [ ] **Step 3: Implement idempotent insert and owner lookup**

Use `database/sql` and one scan helper. Preserve the first snapshot:

```sql
INSERT INTO async_video_billing_tasks (...)
VALUES (...)
ON CONFLICT (provider, upstream_task_id, user_id, api_key_id)
DO UPDATE SET updated_at = async_video_billing_tasks.updated_at
RETURNING ...;
```

`GetOwned` must match all four owner keys and map `sql.ErrNoRows` to `ErrAsyncVideoBillingTaskNotFound`.

- [ ] **Step 4: Implement atomic due claims and fencing**

Use one transaction with `FOR UPDATE SKIP LOCKED` and update candidates to a random token, `lease_epoch + 1`, expiry, and `attempt_count + 1`. `ClaimOwned` must match task ID, user ID, API Key ID, pending status, and absent/expired lease.

Every transition uses:

```sql
WHERE id = $1 AND status = 'pending' AND lease_token = $2 AND lease_epoch = $3
```

If `RowsAffected()` is zero, return `ErrAsyncVideoBillingTaskFenced`. Retry clears lease fields, stores a bounded safe code and next time; terminal and settled transitions clear leases and stamp their terminal time.

- [ ] **Step 5: Register repository and run tests**

Add `NewAsyncVideoBillingTaskRepository` to `repository.ProviderSet`.

Run: `cd backend && go test -tags=integration ./internal/repository -run TestAsyncVideoBillingTaskRepository -count=1`

Expected: PASS for dual claimant, expired lease, stale token, stale epoch, uniqueness, and owner mismatch.

- [ ] **Step 6: Commit repository unit**

```bash
git add backend/internal/repository/async_video_billing_task_repo.go backend/internal/repository/async_video_billing_task_repo_integration_test.go backend/internal/repository/wire.go
git commit -m "feat(seedance): lease durable billing tasks"
```

---

### Task 4: Extract a pure, bounded Seedance upstream client

**Files:**
- Create: `backend/internal/service/seedance_client.go`
- Create: `backend/internal/service/seedance_client_test.go`
- Modify: `backend/internal/service/seedance.go`
- Modify: `backend/internal/service/seedance_test.go`

**Interfaces:**
- Consumes: `OpenAIGatewayService.httpUpstream`, account helpers, `buildSeedanceURL`, and `ParseSeedanceRequest`.
- Produces: `SeedanceTaskClient`, `SeedanceUpstreamResponse`, `SeedanceObservedState`, classified errors, and compatibility `ForwardSeedance`.

- [ ] **Step 1: Write failing pure-client tests**

```go
func TestSeedanceClientDoesNotWriteGinResponse(t *testing.T) {
    upstream := &grokMediaContentUpstreamStub{response: grokMediaContentStatusResponse(`{"id":"task-1","status":"queued"}`)}
    svc := &OpenAIGatewayService{httpUpstream: upstream}
    response, err := svc.CreateSeedanceTask(context.Background(), seedanceTestAccount(), []byte(`{"model":"video","content":[{"type":"text","text":"waves"}]}`))
    require.NoError(t, err)
    require.Equal(t, http.StatusOK, response.StatusCode)
    require.JSONEq(t, `{"id":"task-1","status":"queued"}`, string(response.Body))
    require.Equal(t, "seedance:task-1", response.Result.ResponseID)
}

func TestSeedanceClientClassifiesStatusErrors(t *testing.T) {
    cases := []struct{ status int; kind SeedanceUpstreamErrorKind }{
        {401, SeedanceUpstreamErrorAuth}, {403, SeedanceUpstreamErrorAuth},
        {404, SeedanceUpstreamErrorNotFound}, {429, SeedanceUpstreamErrorRateLimited},
        {500, SeedanceUpstreamErrorTemporary},
    }
    // Return each status from the stub and assert errors.As plus Kind.
}
```

Add an oversized-body case that returns one byte above the configured response limit and expects a protocol error without retaining the full body.

- [ ] **Step 2: Run client tests and verify they fail**

Run: `cd backend && go test -tags=unit ./internal/service -run TestSeedanceClient -count=1`

Expected: FAIL because pure client methods are undefined.

- [ ] **Step 3: Define exact response and client contracts**

```go
type SeedanceObservedState string
const (
    SeedanceObservedPending SeedanceObservedState = "pending"
    SeedanceObservedSucceeded SeedanceObservedState = "succeeded"
    SeedanceObservedFailed SeedanceObservedState = "failed"
    SeedanceObservedCancelled SeedanceObservedState = "cancelled"
)

type SeedanceUpstreamResponse struct {
    StatusCode int
    Header http.Header
    Body []byte
    Result *OpenAIForwardResult
    State SeedanceObservedState
}

type SeedanceTaskClient interface {
    CreateSeedanceTask(context.Context, *Account, []byte) (*SeedanceUpstreamResponse, error)
    GetSeedanceTask(context.Context, *Account, string) (*SeedanceUpstreamResponse, error)
    DeleteSeedanceTask(context.Context, *Account, string) (*SeedanceUpstreamResponse, error)
}
```

Define error kinds `auth`, `rate_limited`, `not_found`, `temporary`, and `protocol`. The error stores only kind, status code, safe code, and retry delay.

- [ ] **Step 4: Move transport/parsing from `ForwardSeedance`**

Preserve model rewrite and headers. Read through `io.LimitReader(limit+1)`. Normalize status:

```go
switch strings.ToLower(gjson.GetBytes(body, "status").String()) {
case "succeeded": return SeedanceObservedSucceeded
case "failed", "expired": return SeedanceObservedFailed
case "cancelled": return SeedanceObservedCancelled
default: return SeedanceObservedPending
}
```

Make `ForwardSeedance` a wrapper that calls the pure method, copies filtered headers/status/body to Gin, and returns `response.Result`.

- [ ] **Step 5: Run client and compatibility tests**

Run: `cd backend && go test -tags=unit ./internal/service -run 'TestSeedance(Client|Native|Status|Validation|Preserves)' -count=1`

Expected: PASS with existing response contracts preserved.

- [ ] **Step 6: Commit client refactor**

```bash
git add backend/internal/service/seedance_client.go backend/internal/service/seedance_client_test.go backend/internal/service/seedance.go backend/internal/service/seedance_test.go
git commit -m "refactor(seedance): extract pure upstream task client"
```

---

### Task 5: Load deleted billing ownership safely

**Files:**
- Modify: `backend/internal/service/api_key_service.go`
- Modify: `backend/internal/repository/api_key_repo.go`
- Create: `backend/internal/repository/api_key_repo_test.go`

**Interfaces:**
- Consumes: Ent `mixins.SkipSoftDelete`, existing entity mapper, user and group edges.
- Produces: `APIKeyRepository.GetByIDIncludeDeleted(ctx context.Context, id int64) (*APIKey, error)`.

- [ ] **Step 1: Write the failing repository test**

Create an API Key, soft-delete it with `DeleteWithAudit`, then assert:

```go
deleted, err := repo.GetByIDIncludeDeleted(context.Background(), key.ID)
require.NoError(t, err)
require.Equal(t, key.ID, deleted.ID)
require.Equal(t, key.UserID, deleted.UserID)
require.NotNil(t, deleted.User)
require.NotNil(t, deleted.Group)
_, err = repo.GetByID(context.Background(), key.ID)
require.ErrorIs(t, err, service.ErrAPIKeyNotFound)
```

Do not assert or log the tombstoned credential value.

- [ ] **Step 2: Run the test and verify interface failure**

Run: `cd backend && go test ./internal/repository -run TestAPIKeyRepository_GetByIDIncludeDeleted -count=1`

Expected: FAIL because the method is absent.

- [ ] **Step 3: Implement deleted-aware loading**

Add the method to the interface and implementation:

```go
func (r *apiKeyRepository) GetByIDIncludeDeleted(ctx context.Context, id int64) (*service.APIKey, error) {
    m, err := r.client.APIKey.Query().Where(apikey.IDEQ(id)).WithUser().WithGroup().Only(mixins.SkipSoftDelete(ctx))
    if err != nil {
        if dbent.IsNotFound(err) { return nil, service.ErrAPIKeyNotFound }
        return nil, err
    }
    return apiKeyEntityToService(m), nil
}
```

Update only compiler-identified interface stubs.

- [ ] **Step 4: Run targeted repository and compile tests**

Run: `cd backend && go test ./internal/repository ./internal/service -run 'TestAPIKeyRepository_GetByIDIncludeDeleted|^$' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit ownership loading**

```bash
git add backend/internal/service/api_key_service.go backend/internal/repository/api_key_repo.go backend/internal/repository/api_key_repo_test.go
git commit -m "feat(seedance): load deleted api key billing ownership"
```

---

### Task 6: Build one fenced settlement service

**Files:**
- Create: `backend/internal/service/seedance_task_settlement.go`
- Create: `backend/internal/service/seedance_task_settlement_test.go`

**Interfaces:**
- Consumes: task repository, deleted-aware user/API Key/subscription repositories, account repository, `OpenAIGatewayService.RecordUsage`, and `APIKeyQuotaUpdater`.
- Produces: `ProcessClaimed` and `ObserveOwned` settlement entry points.

- [ ] **Step 1: Write failing settlement tests**

```go
func TestSeedanceSettlementSuccessBillsOnceWhenStateWriteFails(t *testing.T) {
    // First RecordUsage succeeds; MarkSettled fails. Second attempt receives the
    // same stable request ID; a dedup-aware billing stub asserts one money effect.
}

func TestSeedanceSettlementMissingTokensDeadLettersAfterThreeChecks(t *testing.T) {
    // Checks 1 and 2 retry; check 3 dead-letters with
    // seedance_missing_completion_tokens; RecordUsage is never called.
}

func TestSeedanceSettlementLoadsDeletedAPIKeyAndSubscription(t *testing.T) {}
func TestSeedanceSettlementPhysicallyMissingOwnerDeadLetters(t *testing.T) {}
func TestSeedanceSettlementStaleLeaseCannotSettle(t *testing.T) {}
```

- [ ] **Step 2: Run settlement tests and verify they fail**

Run: `cd backend && go test -tags=unit ./internal/service -run TestSeedanceSettlement -count=1`

Expected: FAIL because the service is undefined.

- [ ] **Step 3: Define exact service contracts**

```go
type SeedanceTaskOwner struct { UserID, APIKeyID int64 }
type SeedanceSettlementResult struct {
    Settled, Retried, Terminal, DeadLettered, Fenced bool
    ErrorCode string
    Err error
}

type SeedanceTaskSettlementService struct {
    tasks AsyncVideoBillingTaskRepository
    apiKeys APIKeyRepository
    users UserRepository
    accounts AccountRepository
    subscriptions UserSubscriptionRepository
    usage *OpenAIGatewayService
    quotaUpdater APIKeyQuotaUpdater
    leaseDuration time.Duration
    recordUsage func(context.Context, *OpenAIRecordUsageInput) error
}

func (s *SeedanceTaskSettlementService) ProcessClaimed(ctx context.Context, task AsyncVideoBillingTask, observed *SeedanceUpstreamResponse, now time.Time) SeedanceSettlementResult
func (s *SeedanceTaskSettlementService) ObserveOwned(ctx context.Context, owner SeedanceTaskOwner, upstreamTaskID string, observed *SeedanceUpstreamResponse, now time.Time) SeedanceSettlementResult
```

Initialize `recordUsage` to `usage.RecordUsage`; tests replace only this seam.

- [ ] **Step 4: Hydrate ownership and bill with stable identity**

Load user and API Key including deleted rows, account with normal `GetByID`, and subscription including deleted when present. Validate every stored ID against hydrated entities. Build usage:

```go
result := *observed.Result
result.RequestID = StableGrokVideoBillingRequestID(task.TaskKey)
result.ResponseID = task.TaskKey
result.Model = task.Model
result.BillingModel = task.BillingModel
result.UpstreamModel = firstNonEmptyString(task.UpstreamModel, result.UpstreamModel)
result.Duration = now.Sub(task.CreatedAt)

err := s.recordUsage(ctx, &OpenAIRecordUsageInput{
    Result: &result, APIKey: apiKey, User: user, Account: account,
    Subscription: subscription, InboundEndpoint: task.InboundEndpoint,
    UpstreamEndpoint: task.UpstreamEndpoint, RequestPayloadHash: task.RequestPayloadHash,
    APIKeyService: s.quotaUpdater, QuotaPlatform: task.QuotaPlatform,
    PricingAt: task.PricingAt,
    ChannelUsageFields: ChannelUsageFields{OriginalModel: task.OriginalModel, ChannelMappedModel: task.Model},
})
```

Only call `MarkSettled` after billing returns nil. Failed/cancelled are terminal without billing. Missing ownership, auth, deadline expiry, or the third missing-token response dead-letters. Temporary errors retry.

- [ ] **Step 5: Run settlement tests**

Run: `cd backend && go test -tags=unit ./internal/service -run TestSeedanceSettlement -count=1`

Expected: PASS, including replay after a failed ledger write.

- [ ] **Step 6: Commit settlement boundary**

```bash
git add backend/internal/service/seedance_task_settlement.go backend/internal/service/seedance_task_settlement_test.go
git commit -m "feat(seedance): reconcile durable task settlement"
```

---

### Task 7: Implement retry policy and reconciler runtime

**Files:**
- Create: `backend/internal/service/seedance_reconciler_runtime.go`
- Create: `backend/internal/service/seedance_reconciler_runtime_test.go`

**Interfaces:**
- Consumes: `ClaimDue`, pure `GetSeedanceTask`, settlement `ProcessClaimed`, and `RouteEvidenceTerminalizationScheduler`.
- Produces: `SeedanceReconcilerRuntime`, options, run result, `Start`, `Stop`, and `ProcessDue`.

- [ ] **Step 1: Write failing retry and lifecycle tests**

```go
func TestSeedanceReconcilerTransient404UsesGraceThenDeadLetters(t *testing.T) {}
func TestSeedanceReconcilerPreventsOverlappingRuns(t *testing.T) {}
func TestSeedanceReconcilerTwoRuntimesClaimOneTask(t *testing.T) {}
func TestSeedanceReconcilerStopCancelsRequestAndRestartRecovers(t *testing.T) {}
```

The first test asserts a 404 inside a 60-second grace window retries as `seedance_not_found_eventual`; a later 404 dead-letters as `seedance_not_found_persistent`.

- [ ] **Step 2: Run reconciler tests and verify they fail**

Run: `cd backend && go test -tags=unit ./internal/service -run TestSeedanceReconciler -count=1`

Expected: FAIL because the runtime is undefined.

- [ ] **Step 3: Implement bounded options and retry delay**

```go
type SeedanceReconcilerOptions struct {
    Enabled bool
    PollInterval, RequestTimeout, LeaseDuration, NotFoundGrace time.Duration
    ClaimBatch, MaxConcurrency int
}
type SeedanceReconcilerRunResult struct { Selected, Settled, Retried, Terminal, DeadLettered, Fenced int }

func seedanceRetryDelay(attempt int, jitter float64) time.Duration {
    sequence := []time.Duration{5*time.Second, 10*time.Second, 20*time.Second, 30*time.Second, time.Minute}
    base := 5 * time.Minute
    if attempt > 0 && attempt <= len(sequence) { base = sequence[attempt-1] }
    jitter = max(-0.1, min(0.1, jitter))
    return time.Duration(float64(base) * (1 + jitter))
}
```

Inject `now func() time.Time` and `jitter func() float64` in tests.

- [ ] **Step 4: Implement non-overlapping cancelable processing**

Follow `RouteEvidenceTerminalizationRuntime`: atomic local run guard, worker context, wait group, scheduler cancel, immediate first trigger, and stop waiting for in-flight work. Use a bounded semaphore even when configured concurrency exceeds one.

Each claimed row gets a request-timeout context, loads its account, calls `GetSeedanceTask`, maps only safe error kinds, then delegates ledger mutation to settlement. Empty polls log at debug level.

- [ ] **Step 5: Run runtime tests**

Run: `cd backend && go test -tags=unit ./internal/service -run TestSeedanceReconciler -count=1`

Expected: PASS for 404 policy, overlap prevention, dual runtime, cancellation, and restart recovery.

- [ ] **Step 6: Commit runtime logic**

```bash
git add backend/internal/service/seedance_reconciler_runtime.go backend/internal/service/seedance_reconciler_runtime_test.go
git commit -m "feat(seedance): add recoverable billing reconciler"
```

---

### Task 8: Persist create results before exposing success

**Files:**
- Create: `backend/internal/handler/seedance_durable_billing_test.go`
- Modify: `backend/internal/handler/openai_gateway_handler.go`
- Modify: `backend/internal/handler/seedance.go`
- Modify: `backend/internal/handler/grok_media.go`

**Interfaces:**
- Consumes: pure `CreateSeedanceTask`, task repository `Create`, and existing handler authentication/account selection.
- Produces: handler fields `seedanceTasks` and `seedanceSettlement`, plus `SetSeedanceDurableBilling`.

- [ ] **Step 1: Write failing durability tests**

```go
func TestSeedanceCreatePersistsBeforeWritingSuccess(t *testing.T) {
    // The repository stub records recorder.Body.Len() during Create.
    // Assert it is zero, then assert the final response equals upstream JSON.
}

func TestSeedanceCreatePersistenceFailureDoesNotReturnUpstreamSuccess(t *testing.T) {
    // Upstream returns a task ID; repository fails twice; expect 503 and never
    // the upstream success body.
}

func TestSeedanceCreateDuplicatePersistenceReturnsSuccess(t *testing.T) {
    // Idempotent repository returns the existing row and the upstream response once.
}
```

- [ ] **Step 2: Run tests and verify ordering failure**

Run: `cd backend && go test -tags=unit ./internal/handler -run TestSeedanceCreate -count=1`

Expected: FAIL because `ForwardSeedance` writes before persistence.

- [ ] **Step 3: Add optional durable dependencies without expanding unit constructors**

```go
func (h *OpenAIGatewayHandler) SetSeedanceDurableBilling(
    tasks service.AsyncVideoBillingTaskRepository,
    settlement *service.SeedanceTaskSettlementService,
) {
    h.seedanceTasks = tasks
    h.seedanceSettlement = settlement
}
```

Production wiring must set both. A Seedance create with either dependency absent fails closed with 503 rather than returning an untracked task.

- [ ] **Step 4: Persist the create-time snapshot before copying the response**

After the pure upstream call returns:

```go
input := service.CreateAsyncVideoBillingTaskInput{
    Provider: service.AsyncVideoBillingProviderSeedance,
    UpstreamTaskID: strings.TrimPrefix(response.Result.ResponseID, "seedance:"),
    TaskKey: response.Result.ResponseID,
    UserID: subject.UserID,
    APIKeyID: apiKey.ID,
    GroupID: apiKey.GroupID,
    AccountID: account.ID,
    SubscriptionID: subscriptionID(subscription),
    Model: requestModel,
    BillingModel: firstNonEmptyString(response.Result.BillingModel, requestModel),
    UpstreamModel: response.Result.UpstreamModel,
    OriginalModel: clientRequestedModel(c, requestModel),
    QuotaPlatform: service.QuotaPlatform(requestCtx, apiKey),
    SubscriptionBilling: subscription != nil,
    PricingAt: requestStart,
    NextPollAt: time.Now().Add(5 * time.Second),
    PollDeadlineAt: requestStart.Add(24 * time.Hour),
    RequestPayloadHash: service.HashUsageRequestPayload(body),
    InboundEndpoint: GetInboundEndpoint(c),
    UpstreamEndpoint: response.Result.UpstreamEndpoint,
}
```

Retry persistence once after a context-aware delay no longer than 100 ms. On final failure, log only safe task/account/owner IDs and an internal error code, then return 503. Keep Redis pending/binding writes as post-PostgreSQL compatibility best-effort; they cannot determine success.

- [ ] **Step 5: Run create and lifecycle tests**

Run: `cd backend && go test -tags=unit ./internal/handler -run 'TestSeedance(Create|HandlerLifecycle)' -count=1`

Expected: PASS and the ordering assertion proves durability precedes response writing.

- [ ] **Step 6: Commit create durability**

```bash
git add backend/internal/handler/seedance_durable_billing_test.go backend/internal/handler/openai_gateway_handler.go backend/internal/handler/seedance.go backend/internal/handler/grok_media.go
git commit -m "feat(seedance): persist tasks before returning success"
```

---

### Task 9: Route GET through shared durable settlement with Redis fallback

**Files:**
- Modify: `backend/internal/handler/seedance_durable_billing_test.go`
- Modify: `backend/internal/handler/seedance.go`
- Modify: `backend/internal/handler/grok_media.go`

**Interfaces:**
- Consumes: owner-scoped task lookup, `ObserveOwned`, pure GET, and legacy Redis settlement for pre-migration rows.
- Produces: synchronous durable GET settlement and explicit compatibility fallback.

- [ ] **Step 1: Add failing GET race and isolation tests**

```go
func TestSeedanceGetAndReconcilerRaceBillsOnce(t *testing.T) {
    // Grant one lease while handler and reconciler race. Assert one money effect.
}

func TestSeedanceGetOwnerMismatchDoesNotCallUpstream(t *testing.T) {
    // Same upstream ID belongs to another user/API key. Expect 404 and zero calls.
}

func TestSeedanceGetLegacyRedisPendingStillSettles(t *testing.T) {
    // PostgreSQL row is absent, Redis pending exists, legacy path bills once.
}
```

- [ ] **Step 2: Run GET tests and verify they fail**

Run: `cd backend && go test -tags=unit ./internal/handler -run TestSeedanceGet -count=1`

Expected: FAIL because GET depends only on Redis.

- [ ] **Step 3: Require ownership before upstream lookup**

Call `GetOwned("seedance", strippedTaskID, subject.UserID, apiKey.ID)` before account selection or upstream forwarding. On not found, permit only the existing Redis owner binding as a legacy path. If neither exists, return the current 404 body without calling upstream.

- [ ] **Step 4: Settle the observed response through the common service**

After pure GET, preserve the original upstream response, then call `ObserveOwned`. A fenced or terminal row is a no-op. If the durable row is absent but Redis pending exists, use `prepareSeedanceCompletionBilling` plus `recordGrokMediaUsage` and log `seedance_legacy_redis_fallback` with numeric IDs only.

A retryable billing error after upstream success does not replace the public GET response because the pending durable row remains recoverable.

- [ ] **Step 5: Run GET and lifecycle tests**

Run: `cd backend && go test -tags=unit ./internal/handler -run 'TestSeedance(Get|HandlerLifecycle)' -count=1`

Expected: PASS with one money effect under handler/runtime concurrency.

- [ ] **Step 6: Commit GET settlement**

```bash
git add backend/internal/handler/seedance_durable_billing_test.go backend/internal/handler/seedance.go backend/internal/handler/grok_media.go
git commit -m "feat(seedance): settle status through durable ledger"
```

---

### Task 10: Make DELETE query, settle, then delete

**Files:**
- Modify: `backend/internal/handler/seedance_durable_billing_test.go`
- Modify: `backend/internal/handler/seedance.go`
- Modify: `backend/internal/handler/grok_media.go`

**Interfaces:**
- Consumes: owner claim, pure GET, `ProcessClaimed`, and pure DELETE.
- Produces: protected DELETE ordering and cancellation terminalization.

- [ ] **Step 1: Add failing DELETE ordering tests**

```go
func TestSeedanceDeleteSucceededTaskSettlesBeforeDelete(t *testing.T) {
    require.Equal(t, []string{"status", "billing", "settled", "delete"}, events)
}

func TestSeedanceDeleteStatusFailureDoesNotDelete(t *testing.T) {
    // Status returns timeout/500; expect a retryable error and deleteCalls == 0.
}

func TestSeedanceDeletePendingTaskMarksCancelledAfterDelete(t *testing.T) {
    // Pending status, delete 204, then MarkTerminal(cancelled); no billing.
}

func TestSeedanceDeleteOwnerMismatchIsInvisible(t *testing.T) {
    // Expect 404 and zero status/delete calls.
}
```

- [ ] **Step 2: Run DELETE tests and verify they fail**

Run: `cd backend && go test -tags=unit ./internal/handler -run TestSeedanceDelete -count=1`

Expected: FAIL because DELETE forwards directly.

- [ ] **Step 3: Implement the protected sequence**

1. Load the owner-scoped row.
2. Claim it with a short lease.
3. Query status using its account.
4. If succeeded, call `ProcessClaimed`; proceed only when settled or already deduplicated.
5. If pending/failed/cancelled, keep the lease through deletion.
6. Call pure DELETE and copy its response.
7. Mark a still-pending task `cancelled` only after delete succeeds.

A status-query error calls `MarkRetry`, skips DELETE, and returns a retryable mapped error. A delete error also releases the task through `MarkRetry` so the reconciler can continue.

- [ ] **Step 4: Run DELETE and lifecycle tests**

Run: `cd backend && go test -tags=unit ./internal/handler -run 'TestSeedance(Delete|HandlerLifecycle)' -count=1`

Expected: PASS and ordered events prove settlement precedes deletion.

- [ ] **Step 5: Commit safe deletion**

```bash
git add backend/internal/handler/seedance_durable_billing_test.go backend/internal/handler/seedance.go backend/internal/handler/grok_media.go
git commit -m "fix(seedance): settle completed tasks before deletion"
```

---

### Task 11: Add 2 GB-safe reconciler configuration and validation

**Files:**
- Modify: `backend/internal/config/config.go`
- Modify: `backend/internal/config/config_test.go`
- Modify: canonical YAML example found by `rg -l 'openai_ws:' . --glob '*.yaml'`

**Interfaces:**
- Consumes: Viper defaults and `Config.Validate`.
- Produces: `GatewaySeedanceReconcilerConfig` at `Config.Gateway.SeedanceReconciler`.

- [ ] **Step 1: Write failing default and bound tests**

```go
func TestLoadDefaultSeedanceReconcilerConfig(t *testing.T) {
    cfg := loadMinimalValidConfig(t)
    require.True(t, cfg.Gateway.SeedanceReconciler.Enabled)
    require.Equal(t, 5, cfg.Gateway.SeedanceReconciler.PollIntervalSeconds)
    require.Equal(t, 8, cfg.Gateway.SeedanceReconciler.ClaimBatch)
    require.Equal(t, 1, cfg.Gateway.SeedanceReconciler.MaxConcurrency)
    require.Equal(t, 20, cfg.Gateway.SeedanceReconciler.RequestTimeoutSeconds)
    require.Equal(t, 60, cfg.Gateway.SeedanceReconciler.LeaseSeconds)
    require.Equal(t, 24, cfg.Gateway.SeedanceReconciler.PollDeadlineHours)
}

func TestValidateSeedanceReconcilerBounds(t *testing.T) {
    // Cases: interval=0, batch=65, concurrency=5, timeout=0,
    // lease<=timeout, deadline=0. Each returns a key-specific error.
}
```

- [ ] **Step 2: Run config tests and verify they fail**

Run: `cd backend && go test ./internal/config -run 'Test(LoadDefaultSeedance|ValidateSeedance)' -count=1`

Expected: FAIL because the block is absent.

- [ ] **Step 3: Add type, defaults, and bounds**

```go
type GatewaySeedanceReconcilerConfig struct {
    Enabled bool `mapstructure:"enabled"`
    PollIntervalSeconds int `mapstructure:"poll_interval_seconds"`
    ClaimBatch int `mapstructure:"claim_batch"`
    MaxConcurrency int `mapstructure:"max_concurrency"`
    RequestTimeoutSeconds int `mapstructure:"request_timeout_seconds"`
    LeaseSeconds int `mapstructure:"lease_seconds"`
    PollDeadlineHours int `mapstructure:"poll_deadline_hours"`
}
```

Add it as `GatewayConfig.SeedanceReconciler`. Defaults are `true, 5, 8, 1, 20, 60, 24`. Validate interval 1–300, batch 1–64, concurrency 1–4, timeout 1–120, lease 2–600 and strictly greater than timeout, deadline 1–168.

- [ ] **Step 4: Document YAML and run tests**

```yaml
gateway:
  seedance_reconciler:
    enabled: true
    poll_interval_seconds: 5
    claim_batch: 8
    max_concurrency: 1
    request_timeout_seconds: 20
    lease_seconds: 60
    poll_deadline_hours: 24
```

Run: `cd backend && go test ./internal/config -run 'Test(LoadDefaultSeedance|ValidateSeedance)' -count=1`

Expected: PASS.

- [ ] **Step 5: Commit configuration**

```bash
git add backend/internal/config/config.go backend/internal/config/config_test.go
example_file="$(rg -l 'openai_ws:' . --glob '*.yaml' | head -n 1)"
test -n "$example_file" && git add "$example_file"
git commit -m "feat(seedance): configure bounded billing reconciliation"
```

---

### Task 12: Wire settlement and runtime into startup and shutdown

**Files:**
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/cmd/server/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`
- Modify: `backend/cmd/server/wire_gen_test.go`

**Interfaces:**
- Consumes: repository provider, settlement/runtime constructors, config, scheduler, and handler setter.
- Produces: production-created settlement/runtime, configured handler, and cleanup stop.

- [ ] **Step 1: Write failing provider/lifecycle tests**

```go
func TestProvideSeedanceReconcilerRuntimeDisabledDoesNotSchedule(t *testing.T) {}
func TestProvideSeedanceReconcilerRuntimeEnabledSchedulesAndStops(t *testing.T) {}
```

Update `TestProvideCleanup_WithMinimalDependencies_NoPanic` with a runtime and assert cleanup stops it.

- [ ] **Step 2: Run provider tests and verify they fail**

Run: `cd backend && go test -tags=unit ./internal/service ./cmd/server -run 'TestProvideSeedance|TestProvideCleanup' -count=1`

Expected: FAIL because providers and cleanup dependency are absent.

- [ ] **Step 3: Add exact providers**

```go
func ProvideSeedanceTaskSettlementService(
    tasks AsyncVideoBillingTaskRepository,
    apiKeys APIKeyRepository,
    users UserRepository,
    accounts AccountRepository,
    subscriptions UserSubscriptionRepository,
    usage *OpenAIGatewayService,
    apiKeyService *APIKeyService,
    cfg *config.Config,
) *SeedanceTaskSettlementService

func ProvideSeedanceReconcilerRuntime(
    tasks AsyncVideoBillingTaskRepository,
    settlement *SeedanceTaskSettlementService,
    client *OpenAIGatewayService,
    scheduler RouteEvidenceTerminalizationScheduler,
    cfg *config.Config,
) *SeedanceReconcilerRuntime
```

`ProvideOpenAIGatewayHandler` receives task repository and settlement, then calls `SetSeedanceDurableBilling`.

- [ ] **Step 4: Add cleanup and regenerate Wire**

Add `seedanceReconciler *service.SeedanceReconcilerRuntime` to `provideCleanup`, with a parallel stop step named `SeedanceReconcilerRuntime`.

Run: `cd backend && wire ./cmd/server`

Expected: generated file contains repository, settlement, runtime, handler injection, and cleanup argument. Never hand-edit generated wiring.

- [ ] **Step 5: Run provider and generated-wiring tests**

Run: `cd backend && go test -tags=unit ./internal/service ./cmd/server -run 'TestProvideSeedance|TestProvideCleanup' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit wiring**

```bash
git add backend/internal/service/wire.go backend/internal/handler/wire.go backend/cmd/server/wire.go backend/cmd/server/wire_gen.go backend/cmd/server/wire_gen_test.go
git commit -m "feat(seedance): wire reconciler lifecycle"
```

---

### Task 13: Add safe operational logs and redaction tests

**Files:**
- Modify: `backend/internal/service/seedance_reconciler_runtime.go`
- Modify: `backend/internal/service/seedance_reconciler_runtime_test.go`
- Modify: `backend/internal/handler/seedance_durable_billing_test.go`

**Interfaces:**
- Consumes: Zap observer pattern from `evaluation_terminalization_runtime.go`.
- Produces: safe events `seedance_reconciler_poll`, `seedance_task_claimed`, `seedance_task_retried`, `seedance_task_settled`, `seedance_task_terminal`, `seedance_task_dead_lettered`, and `seedance_task_persistence_failed`.

- [ ] **Step 1: Write failing redaction tests**

```go
func TestSeedanceReconcilerLogsExcludeSensitivePayloads(t *testing.T) {
    secrets := []string{"sk-secret-value", "Bearer abc", "private prompt", "https://cdn.example/private.mp4"}
    // Feed sentinels through an upstream error and account fixture.
    // Joined observer messages/fields contain none of them.
    // Safe fields include provider, task_id, account_id, attempt_count, error_code.
}
```

Add an equivalent create-persistence failure test in the handler package.

- [ ] **Step 2: Run tests and verify they fail**

Run: `cd backend && go test -tags=unit ./internal/service ./internal/handler -run 'TestSeedance.*LogsExcludeSensitive' -count=1`

Expected: FAIL until named safe events exist.

- [ ] **Step 3: Emit bounded structured logs**

Log stable task key, provider, numeric ownership IDs, attempt count, elapsed milliseconds, status, and safe error code. Never attach upstream-derived error text. Empty poll is debug, transitions info, retry warn, and dead letter/persistence failure error.

- [ ] **Step 4: Run all Seedance tests**

Run: `cd backend && go test -tags=unit ./internal/service ./internal/handler -run TestSeedance -count=1`

Expected: PASS with all sentinel strings absent.

- [ ] **Step 5: Commit observability**

```bash
git add backend/internal/service/seedance_reconciler_runtime.go backend/internal/service/seedance_reconciler_runtime_test.go backend/internal/handler/seedance_durable_billing_test.go
git commit -m "feat(seedance): add safe reconciliation telemetry"
```

---

### Task 14: Document behavior and run full regression gates

**Files:**
- Modify: `docs/seedance-api.md`
- Test: all modified backend packages and repository integration suite.

**Interfaces:**
- Consumes: completed Tasks 1–13.
- Produces: operator-facing recovery/rollback documentation and a verified implementation branch.

- [ ] **Step 1: Update API and operations documentation**

Replace the “no background polling” statement with:

```markdown
- Accepted tasks are persisted before a successful create response is returned.
- The service polls pending Seedance tasks in a bounded reconciler and resumes them after restart.
- GET and reconciler observations share one idempotent settlement path.
- DELETE queries status and settles a completed task before deletion.
- Disabling `gateway.seedance_reconciler.enabled` stops background claims but preserves rows and GET settlement.
- Dead-letter rows require diagnosis and are not silently billed or discarded.
```

State explicitly that a process crash between upstream acceptance and local insert remains an external-system limitation.

- [ ] **Step 2: Format and run focused tests**

Run:

```bash
cd backend
gofmt -w internal/service/async_video_billing_task.go \
  internal/service/seedance_client.go \
  internal/service/seedance_task_settlement.go \
  internal/service/seedance_reconciler_runtime.go \
  internal/repository/async_video_billing_task_repo.go \
  internal/handler/seedance.go
go test -tags=unit ./internal/service ./internal/handler ./cmd/server -count=1
go test ./internal/config ./internal/repository -count=1
```

Expected: PASS.

- [ ] **Step 3: Run schema and lease integration tests**

Run:

```bash
cd backend
go test -tags=integration ./internal/repository \
  -run 'TestMigrationsRunner_IsIdempotent_AndSchemaIsUpToDate|TestAsyncVideoBillingTaskRepository' \
  -count=1
```

Expected: PASS. If the integration database is unavailable, record an explicit blocker and do not claim completion.

- [ ] **Step 4: Run full backend and Grok non-regression tests**

Run:

```bash
cd backend
go test ./... -count=1
go test -tags=unit ./... -count=1
go test -tags=unit ./internal/handler ./internal/service \
  -run 'TestGrokMedia|TestPrepareGrokVideo|TestSeedance' -count=1
```

Expected: PASS; Grok tests prove its Redis pending/claim path is unchanged.

- [ ] **Step 5: Inspect diff and sensitive-string matches**

Run:

```bash
git diff --check
git status --short
rg -n 'Authorization|Cookie|prompt|video_url|api_key' \
  backend/internal/service/seedance_* \
  backend/internal/repository/async_video_billing_task_repo.go \
  backend/internal/handler/seedance*.go
```

Expected: clean diff; each search match is protocol parsing, redaction test input, or an explicit prohibition, never storage/logging.

- [ ] **Step 6: Commit documentation and final verified state**

```bash
git add docs/seedance-api.md
git commit -m "docs(seedance): describe durable billing recovery"
git status --short
```

Expected: clean working tree. Do not push, build images, apply migrations, or deploy until separately approved.

---

## Implementation Completion Criteria

- Accepted create is durable before client success is written.
- A client that never polls produces exactly one settled usage record after upstream success.
- Restart, local trigger overlap, and multiple service instances do not double-charge or lose pending rows.
- A stale token or epoch cannot mutate a task reclaimed by another worker.
- Billing success followed by ledger-write failure replays safely through `usage_billing_dedup`.
- Missing tokens dead-letter after three checks and are never estimated.
- Fresh 404 retries in the grace window; persistent 404 dead-letters.
- Deleted API Keys and subscriptions remain billable; physically missing ownership dead-letters.
- DELETE skips deletion on preflight failure and settles observed success first.
- Cross-user and cross-API-key lookups do not call upstream or reveal task existence.
- Disabling the runtime preserves GET settlement and later recovery.
- Existing Grok media behavior remains unchanged.
- Sensitive request, credential, prompt, response, and media data never enter storage or logs.
- Targeted unit, integration, full backend, Wire, formatting, and diff checks pass before release consideration.
