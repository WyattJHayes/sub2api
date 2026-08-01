# Radar Evaluation Outbox Consumer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Consume every supported `evaluation_outbox_events` row in production and drive route evidence, analysis, Gate projections, and Run lifecycle to a durable terminal state.

**Architecture:** A single `EvaluationOutboxConsumerRuntime` owns polling, leases, heartbeat, bounded concurrency, error classification, and shutdown. An `EvaluationOutboxDispatcher` validates canonical event identity and delegates domain work through focused repository interfaces. PostgreSQL remains the durable queue and consistency boundary, including cause closure, Gate projections, and Run reconciliation.

**Tech Stack:** Go 1.24, PostgreSQL 14 or newer, `database/sql`, Google Wire, Testify, sqlmock, Docker Compose, Python Radar statistics worker.

## Global Constraints

1. Poll interval is 2 seconds, claim batch is 16, maximum concurrency is 4, lease duration is 60 seconds, heartbeat interval is 20 seconds, and handler timeout is 45 seconds.
2. Supported event types are `route_evidence_sealed`, `cell_recompute`, `global_recompute`, and `gate_reevaluation`.
3. `ErrAggregatePairsIncomplete` retries for at most 24 hours. Other transient failures retry at most 8 attempts. Permanent contract failures dead letter immediately.
4. The persistent internal worker name is `radar-control-plane-outbox`.
5. `RADAR_OUTBOX_CONSUMER_MODE` accepts `disabled`, `core`, or `full`, and defaults to `core` when Radar is enabled.
6. Gate decision, decision head, release projection, and alert projection commit in one writer transaction.
7. Initial dead letter fails the Run. Regrade dead letter fails its revision batch without changing a completed Run terminal state.
8. No schema migration is added. Migration 199 already contains the required outbox and release projection columns.
9. Existing uncommitted work in the release worktree must remain intact. Every commit stages explicit paths only.

---

### Task 1: Complete the outbox lease repository protocol

**Files:**

- Modify: `backend/internal/service/evaluation_outbox.go`
- Modify: `backend/internal/repository/evaluation_outbox_repo.go`
- Modify: `backend/internal/repository/evaluation_outbox_repo_test.go`
- Modify: `backend/internal/repository/evaluation_outbox_repo_integration_test.go`

**Interfaces:**

- Consumes: the existing `EvaluationOutboxEvent`, `Claim`, `Heartbeat`, `Complete`, and `DeadLetter` contracts.
- Produces: `Retry(context.Context, uuid.UUID, string, int64, string, time.Duration) error`, `EnsureConsumerWorker(context.Context, string) (uuid.UUID, error)`, and cause-closed claims.

- [ ] **Step 1: Write repository tests that expose the missing protocol**

Add tests that prove the following observable behavior.

```go
func TestEvaluationOutboxRetryReleasesLeaseAndDelaysAvailability(t *testing.T) {
    event := claimOutboxFixture(t)
    require.NoError(t, repo.Retry(ctx, event.ID, event.LeaseToken, event.LeaseEpoch, "aggregate_dependency_pending", 2*time.Second))

    var status string
    var owner uuid.NullUUID
    var availableAt time.Time
    require.NoError(t, integrationDB.QueryRowContext(ctx, `
        SELECT status, lease_owner, available_at
        FROM evaluation_outbox_events WHERE id=$1`, event.ID).Scan(&status, &owner, &availableAt))
    require.Equal(t, "pending", status)
    require.False(t, owner.Valid)
    require.True(t, availableAt.After(time.Now().UTC()))
}

func TestEvaluationOutboxClaimWaitsForEveryCause(t *testing.T) {
    parentA, parentB, child := insertCauseClosureFixture(t)
    completeOutboxFixture(t, parentA)
    require.Empty(t, claimEventByID(t, child.ID))
    completeOutboxFixture(t, parentB)
    require.Equal(t, child.ID, claimEventByID(t, child.ID).ID)
}

func TestEnsureConsumerWorkerIsStableAcrossStarts(t *testing.T) {
    first, err := repo.EnsureConsumerWorker(ctx, "radar-control-plane-outbox")
    require.NoError(t, err)
    second, err := repo.EnsureConsumerWorker(ctx, "radar-control-plane-outbox")
    require.NoError(t, err)
    require.Equal(t, first, second)
}
```

The mutation each test catches is respectively retaining a stale lease, claiming a child after only one parent completes, and creating a different worker identity after restart.

- [ ] **Step 2: Run the focused repository tests and verify RED**

Run:

```bash
cd backend
go test -tags=unit ./internal/repository -run 'TestEvaluationOutboxRetry|TestEvaluationOutboxClaimWaits|TestEnsureConsumerWorker' -count=1
```

Expected result: compilation fails because `Retry` and `EnsureConsumerWorker` are absent, then the cause closure test fails because the current claim query does not inspect `evaluation_outbox_event_causes`.

- [ ] **Step 3: Extend the service interface and implement the transaction rules**

Add these methods to `EvaluationOutboxRepository`.

```go
Retry(context.Context, uuid.UUID, string, int64, string, time.Duration) error
EnsureConsumerWorker(context.Context, string) (uuid.UUID, error)
```

Implement `Retry` through `updateLeased` so fencing, lease owner, batch status, and epoch checks remain identical to `Complete` and `DeadLetter`.

```go
func (r *evaluationOutboxRepository) Retry(ctx context.Context, eventID uuid.UUID, token string, epoch int64, code string, delay time.Duration) error {
    code = strings.TrimSpace(code)
    if code == "" || len(code) > 100 || delay < 0 {
        return service.ErrEvaluationOutboxInvalid
    }
    return r.updateLeased(ctx, eventID, token, epoch, func(ctx context.Context, tx *sql.Tx) error {
        _, err := tx.ExecContext(ctx, `
            UPDATE evaluation_outbox_events
            SET status='pending', available_at=transaction_timestamp()+($2 * INTERVAL '1 millisecond'),
                lease_token_hash=NULL, lease_owner=NULL, lease_expires_at=NULL,
                last_error_code=$3, updated_at=transaction_timestamp()
            WHERE id=$1`, eventID, delay.Milliseconds(), code)
        return err
    })
}
```

Create or load the internal worker with `worker_kind='statistics'`, `status='active'`, `claim_mode='open'`, capability `outbox_consumer`, maximum concurrency 4, and tenant 0. Use an advisory transaction lock on the fixed name before the lookup and insert so concurrent API instances converge on one UUID.

Add the following predicate to both tenant-scoped and unscoped candidate queries and to the row recheck performed under `FOR UPDATE SKIP LOCKED`.

```sql
AND NOT EXISTS (
    SELECT 1
    FROM evaluation_outbox_event_causes c
    JOIN evaluation_outbox_events parent ON parent.id=c.cause_event_id
    WHERE c.event_id=event.id AND parent.status <> 'completed'
)
```

- [ ] **Step 4: Run repository tests and verify GREEN**

Run:

```bash
cd backend
go test -tags=unit ./internal/repository -run 'EvaluationOutbox' -count=1
RADAR_INTEGRATION=1 go test -tags=integration ./internal/repository -run 'EvaluationOutbox|EnsureConsumerWorker' -count=1
```

Expected result: all selected tests pass. A fenced token, wrong owner, expired lease, and old revision epoch still return `ErrEvaluationOutboxFenced` or `ErrRadarForbidden` according to the existing contract.

- [ ] **Step 5: Commit the repository protocol**

```bash
git add backend/internal/service/evaluation_outbox.go \
  backend/internal/repository/evaluation_outbox_repo.go \
  backend/internal/repository/evaluation_outbox_repo_test.go \
  backend/internal/repository/evaluation_outbox_repo_integration_test.go
git commit -m "feat(radar): complete outbox lease protocol"
```

### Task 2: Add canonical dispatch and domain handlers

**Files:**

- Create: `backend/internal/service/evaluation_outbox_dispatcher.go`
- Create: `backend/internal/service/evaluation_outbox_dispatcher_test.go`
- Create: `backend/internal/repository/evaluation_outbox_processing.go`
- Create: `backend/internal/repository/evaluation_outbox_processing_test.go`
- Modify: `backend/internal/repository/wire.go`

**Interfaces:**

- Consumes: `EvaluationAggregateRepository`, durable route evidence, Gate authority, and Run reconciliation.
- Produces: `EvaluationOutboxDispatcher.Dispatch(context.Context, EvaluationOutboxEvent) EvaluationOutboxDispatchResult` and `EvaluationOutboxDomainRepository`.

Define the domain boundary with exact signatures.

```go
type RadarGateTarget struct {
    ReleaseSubjectID uuid.UUID
    PolicyID         uuid.UUID
    TenantID         int64
}

type AutomatedRadarGateOutcome struct {
    EventID     uuid.UUID
    EventRunID  uuid.UUID
    CauseSetHash string
    Target      RadarGateTarget
}

type EvaluationOutboxDomainRepository interface {
    ValidateSealedRouteEvidence(context.Context, EvaluationOutboxEvent) error
    EnsureCellAnalysisJob(context.Context, CellAnalysisJobRequest) (*AnalysisJobRevision, error)
    EnsureGlobalAnalysisJob(context.Context, GlobalAnalysisJobRequest) (*AnalysisJobRevision, error)
    ResolveRadarGateTarget(context.Context, uuid.UUID) (*RadarGateTarget, error)
    EvaluateAndProjectRadarGate(context.Context, AutomatedRadarGateOutcome) (*RadarGateDecisionRecord, error)
    ReconcileEvaluationRun(context.Context, uuid.UUID) error
}
```

- [ ] **Step 1: Write dispatcher contract tests**

Cover one real behavior per test.

```go
func TestOutboxDispatcherRejectsPayloadHashMismatch(t *testing.T) {
    event := validCellEvent()
    event.PayloadHash = strings.Repeat("0", 64)
    result := dispatcher.Dispatch(context.Background(), event)
    require.Equal(t, OutboxDispatchDeadLetter, result.Disposition)
    require.Equal(t, "payload_hash_mismatch", result.ErrorCode)
}

func TestOutboxDispatcherCreatesCellJobWithV1Fallback(t *testing.T) {
    event := validCellEventWithoutAnalysisVersion()
    result := dispatcher.Dispatch(context.Background(), event)
    require.Equal(t, OutboxDispatchComplete, result.Disposition)
    require.Equal(t, CellAnalysisJobRequest{
        RunID: event.RunID, CapabilityDomain: "coding", ModelRoute: "route-a", AnalysisVersion: "v1",
    }, domain.cellRequest)
}

func TestHistoricalSingleCellGlobalEventRunsCompatibleGate(t *testing.T) {
    domain.globalJob = nil
    result := dispatcher.Dispatch(context.Background(), validGlobalEvent())
    require.Equal(t, OutboxDispatchComplete, result.Disposition)
    require.Equal(t, validGlobalEvent().ID, domain.gateOutcome.EventID)
}
```

Also test the valid sealed route evidence path, unsupported source and event pair, malformed JSON, unsupported analysis version, no Gate target, `core` mode Gate delay, and `full` mode Gate evaluation.

- [ ] **Step 2: Run dispatcher tests and verify RED**

Run:

```bash
cd backend
go test -tags=unit ./internal/service -run 'OutboxDispatcher|HistoricalSingleCell' -count=1
```

Expected result: compilation fails because the dispatcher and result types do not exist.

- [ ] **Step 3: Implement canonical validation and the handler registry**

Create these result values.

```go
type EvaluationOutboxDispatchDisposition string

const (
    OutboxDispatchComplete   EvaluationOutboxDispatchDisposition = "complete"
    OutboxDispatchRetry      EvaluationOutboxDispatchDisposition = "retry"
    OutboxDispatchDeadLetter EvaluationOutboxDispatchDisposition = "dead_letter"
    OutboxDispatchFenced     EvaluationOutboxDispatchDisposition = "fenced"
)

type EvaluationOutboxDispatchResult struct {
    Disposition EvaluationOutboxDispatchDisposition
    ErrorCode   string
    RetryAfter  time.Duration
}
```

Canonicalize the payload with `DigestCanonicalJSON`, compare it to `PayloadHash`, validate the event and source pair, then decode into strict payload structs using `json.Decoder.DisallowUnknownFields`. Accept `analysis_version` only when empty or `v1`; empty maps to `v1` for staging compatibility.

Call `ReconcileEvaluationRun` only after a handler has durably completed its domain action. For a historical single-cell `global_recompute`, a nil Global Job invokes the Gate path with the original event and cause set.

- [ ] **Step 4: Implement the PostgreSQL domain adapter**

`ValidateSealedRouteEvidence` loads the row by `source_id=route_trace_id` and verifies run ID, `sealed_at`, evidence revision, schema version, and payload hash against the event payload and source identity.

`ResolveRadarGateTarget` returns `(nil, nil)` when the Run has no currently activated Release Subject or its subject scope has no Policy Head. It returns `ErrGovernanceHeadConflict` when multiple active subjects conflict, tenant ownership differs, or authority rows change during the transaction.

Delegate cell and global job creation to `evaluationAggregateRepository`. Delegate Run reconciliation to `evaluationRepository.ReconcileEvaluationRun` and return only its error.

- [ ] **Step 5: Run dispatcher and adapter tests and verify GREEN**

Run:

```bash
cd backend
go test -tags=unit ./internal/service -run 'OutboxDispatcher|HistoricalSingleCell' -count=1
go test -tags=unit ./internal/repository -run 'OutboxProcessing' -count=1
```

Expected result: all selected tests pass, with no production event payload or lease token written to logs.

- [ ] **Step 6: Commit canonical dispatch**

```bash
git add backend/internal/service/evaluation_outbox_dispatcher.go \
  backend/internal/service/evaluation_outbox_dispatcher_test.go \
  backend/internal/repository/evaluation_outbox_processing.go \
  backend/internal/repository/evaluation_outbox_processing_test.go \
  backend/internal/repository/wire.go
git commit -m "feat(radar): dispatch evaluation outbox events"
```

### Task 3: Add the bounded consumer runtime

**Files:**

- Create: `backend/internal/service/evaluation_outbox_runtime.go`
- Create: `backend/internal/service/evaluation_outbox_runtime_test.go`
- Modify: `backend/internal/service/wire.go`

**Interfaces:**

- Consumes: `EvaluationOutboxRepository`, `EvaluationOutboxDispatcher`, `RouteEvidenceTerminalizationScheduler`, and runtime options.
- Produces: `EvaluationOutboxConsumerRuntime.Start()`, `Stop()`, and `ProcessPending(context.Context) EvaluationOutboxConsumerResult`.

- [ ] **Step 1: Write runtime behavior tests**

Use a deterministic in-memory repository fake and real dispatcher result values. Assert state transitions, not mock call presence.

```go
func TestOutboxRuntimeHeartbeatsUntilHandlerCompletes(t *testing.T) {
    repo := newRuntimeRepository(oneLeasedEvent())
    dispatcher := blockingDispatcher()
    runtime := NewEvaluationOutboxConsumerRuntime(repo, dispatcher, testRuntimeOptions())

    done := make(chan struct{})
    go func() { runtime.ProcessPending(context.Background()); close(done) }()
    require.Eventually(t, func() bool { return repo.heartbeatCount() >= 2 }, time.Second, 5*time.Millisecond)
    dispatcher.release()
    <-done
    require.Equal(t, EvaluationOutboxCompleted, repo.status())
}

func TestOutboxRuntimeNeverExceedsConcurrency(t *testing.T) {
    runtime.ProcessPending(context.Background())
    require.LessOrEqual(t, dispatcher.maxObservedConcurrency(), 4)
}
```

Add tests for poll overlap suppression, heartbeat fencing canceling the handler, dependency backoff, 24 hour dependency timeout, transient attempt exhaustion at 8, permanent dead letter, `core` Gate rollout wait without attempt exhaustion, shutdown cancellation preserving the lease, and `Stop` waiting for active goroutines.

- [ ] **Step 2: Run runtime tests and verify RED**

Run:

```bash
cd backend
go test -tags=unit ./internal/service -run 'OutboxRuntime' -count=1
```

Expected result: compilation fails because the runtime is absent.

- [ ] **Step 3: Implement scheduling, heartbeat, and disposition application**

Use a semaphore of size 4, one heartbeat goroutine per event, one handler timeout context per event, an atomic poll guard, and a `sync.WaitGroup` for shutdown. Start by calling `EnsureConsumerWorker` before the first claim. Claim only the four supported event types.

The transient delay table is literal and indexed by the claimed attempt.

```go
var outboxTransientBackoff = []time.Duration{
    time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
    16 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second,
}
```

Dependency retry starts at 2 seconds, doubles to 60 seconds, and dead letters with `aggregate_dependency_timeout` when `time.Since(event.CreatedAt) >= 24*time.Hour`. Runtime shutdown cancellation leaves the leased row untouched. A handler timeout while the worker context remains active calls `Retry`.

Log and aggregate only event ID, event type, disposition, stable error code, count, lag seconds, and latency milliseconds.

- [ ] **Step 4: Run runtime tests and verify GREEN**

Run:

```bash
cd backend
go test -tags=unit ./internal/service -run 'OutboxRuntime' -count=1 -race
```

Expected result: all tests pass under the race detector and leave no blocked goroutines.

- [ ] **Step 5: Commit the runtime**

```bash
git add backend/internal/service/evaluation_outbox_runtime.go \
  backend/internal/service/evaluation_outbox_runtime_test.go \
  backend/internal/service/wire.go
git commit -m "feat(radar): run the evaluation outbox consumer"
```

### Task 4: Make Aggregate propagation and Run terminalization outbox-aware

**Files:**

- Modify: `backend/internal/repository/evaluation_aggregate_repo.go`
- Modify: `backend/internal/repository/evaluation_aggregate_repo_integration_test.go`
- Modify: `backend/internal/repository/evaluation_run_reconciler.go`
- Modify: `backend/internal/repository/evaluation_repo_integration_test.go`
- Modify: `backend/internal/repository/evaluation_outbox_repo.go`
- Modify: `backend/internal/repository/evaluation_outbox_repo_integration_test.go`

**Interfaces:**

- Consumes: completed Analysis Jobs and outbox event terminal states.
- Produces: direct single-cell Gate events, analysis and outbox Run barriers, and initial pipeline failure facts.

- [ ] **Step 1: Write regression tests for the two missing barriers**

```go
func TestSingleCellAggregateHeadEnqueuesGateDirectly(t *testing.T) {
    snapshot := completeSingleCellAnalysis(t)
    var gateCount, globalCount int
    require.NoError(t, integrationDB.QueryRow(`
        SELECT COUNT(*) FILTER (WHERE event_type='gate_reevaluation'),
               COUNT(*) FILTER (WHERE event_type='global_recompute')
        FROM evaluation_outbox_events WHERE run_id=$1 AND source_id=$2`, runID, snapshot.ID).
        Scan(&gateCount, &globalCount))
    require.Equal(t, 1, gateCount)
    require.Zero(t, globalCount)
}

func TestRunWaitsForAnalysisAndSupportedOutbox(t *testing.T) {
    seedCurrentAggregateCoverage(t, runID)
    insertPendingAnalysisAndOutbox(t, runID)
    record, err := repo.ReconcileEvaluationRun(ctx, runID)
    require.NoError(t, err)
    require.Equal(t, service.RunStatusRunning, record.Status)
}

func TestInitialOutboxDeadLetterFailsRun(t *testing.T) {
    event := claimInitialOutbox(t, runID)
    require.NoError(t, outbox.DeadLetter(ctx, event.ID, event.LeaseToken, event.LeaseEpoch, "invalid_payload"))
    require.Equal(t, service.RunStatusFailed, loadRunStatus(t, runID))
}
```

- [ ] **Step 2: Run the regression tests and verify RED**

Run:

```bash
cd backend
RADAR_INTEGRATION=1 go test -tags=integration ./internal/repository \
  -run 'SingleCellAggregateHeadEnqueuesGateDirectly|RunWaitsForAnalysisAndSupportedOutbox|InitialOutboxDeadLetterFailsRun' -count=1
```

Expected result: the first test sees `global_recompute`, the second Run completes too early, and the third Run remains running.

- [ ] **Step 3: Select the next Aggregate event from durable scope facts**

In `enqueueAggregateHeadProgress`, when `scope == "cell"`, count current cell heads for the Run and analysis version. Emit `gate_reevaluation` when exactly one cell exists and `global_recompute` when more than one exists. Keep the existing Global Aggregate behavior that always emits `gate_reevaluation`.

- [ ] **Step 4: Extend Run facts and initial dead letter behavior**

Add to `PendingWork` the count of Analysis Jobs in `pending`, `leased`, or `running` plus supported outbox rows in `pending` or `leased`.

Load an initial outbox dead letter as an unrecoverable pipeline failure before transition selection. On `DeadLetter`, call `ReconcileEvaluationRun` after the outbox transaction commits when `work_origin='initial'`. Preserve the existing revision requirement failure for `work_origin='regrade'`.

- [ ] **Step 5: Run repository tests and verify GREEN**

Run:

```bash
cd backend
RADAR_INTEGRATION=1 go test -tags=integration ./internal/repository \
  -run 'Aggregate|ReconcileEvaluationRun|EvaluationOutbox' -count=1
```

Expected result: all selected tests pass, a completed regrade Run remains completed, and its revision batch transitions to failed.

- [ ] **Step 6: Commit propagation and terminalization**

```bash
git add backend/internal/repository/evaluation_aggregate_repo.go \
  backend/internal/repository/evaluation_aggregate_repo_integration_test.go \
  backend/internal/repository/evaluation_run_reconciler.go \
  backend/internal/repository/evaluation_repo_integration_test.go \
  backend/internal/repository/evaluation_outbox_repo.go \
  backend/internal/repository/evaluation_outbox_repo_integration_test.go
git commit -m "fix(radar): gate run completion on outbox propagation"
```

### Task 5: Make automatic Gate evaluation authoritative and atomic

**Files:**

- Modify: `backend/internal/service/evaluation_gate_service.go`
- Modify: `backend/internal/repository/evaluation_reliability_gate.go`
- Modify: `backend/internal/repository/evaluation_reliability_gate_test.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_test.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_integration_test.go`
- Modify: `backend/internal/service/evaluation_outbox_dispatcher_test.go`

**Interfaces:**

- Consumes: an active Gate target and trusted evidence loader output.
- Produces: one atomic automated Gate outcome transaction and corrected observation timestamps.

- [ ] **Step 1: Write tests for the discovered trust and projection defects**

```go
func TestLoadRadarGateReliabilityCopiesObservedAtIntoContextAndInput(t *testing.T) {
    loaded, err := repo.LoadRadarGateReliability(ctx, runID, policyID)
    require.NoError(t, err)
    require.False(t, loaded.ObservedAt.IsZero())
    require.Equal(t, loaded.ObservedAt, loaded.Input.ObservedAt)
}

func TestNonReliabilityPolicyAcceptsEmptySnapshotWatermark(t *testing.T) {
    watermark := validGateWatermarkWithoutSnapshotRefs(t)
    require.NoError(t, validateGateReliabilityWatermark(ctx, tx, runID, policyID, watermark))
}

func TestAutomatedGateOutcomeUsesRunTenantAndCommitsEveryProjection(t *testing.T) {
    decision, err := repo.EvaluateAndProjectRadarGate(ctx, outcome)
    require.NoError(t, err)
    require.Equal(t, runTenantID, loadDecisionTenant(t, decision.ID))
    require.Equal(t, decision.ID, loadDecisionHead(t, outcome.Target))
    require.Equal(t, decision.ID, loadReleaseProjection(t, outcome.Target.ReleaseSubjectID).DecisionID)
    require.Equal(t, outcome.EventID, loadReleaseProjection(t, outcome.Target.ReleaseSubjectID).LastOutboxEventID)
}
```

Add rollback coverage by forcing the alert write to fail and asserting that decision, head, and release projection counts remain unchanged.

- [ ] **Step 2: Run Gate tests and verify RED**

Run:

```bash
cd backend
go test -tags=unit ./internal/repository -run 'RadarGateReliability|AutomatedGateOutcome' -count=1
RADAR_INTEGRATION=1 go test -tags=integration ./internal/repository -run 'AutomatedGateOutcome' -count=1
```

Expected result: the loader exposes a zero observation time, non-reliability watermark validation rejects empty refs, and no atomic projection method exists.

- [ ] **Step 3: Correct trusted loader timestamps and watermark rules**

Set both `authoritativeInput.ObservedAt` and `RadarGateReliabilityContext.ObservedAt` to the repeatable-read transaction timestamp. During watermark validation, require snapshot refs only when the stored policy document contains a non-null `reliability` section.

- [ ] **Step 4: Refactor Gate writes onto one transaction helper**

Extract the current decision insert and decision head advance into a helper that accepts `*sql.Tx`. `RecordGateDecision` keeps using that helper. `EvaluateAndProjectRadarGate` performs the following in one `beginRadarWriterTx(ctx, db, "api")` transaction.

1. Load the Run tenant and require it to equal the target tenant.
2. Recheck target authority and load trusted reliability context.
3. Evaluate with `EvaluateRadarGate` and build the canonical evidence envelope.
4. Insert or load the idempotent decision and advance its head.
5. Upsert `evaluation_release_projections` with status `pending` for `passed` and `recorded`, otherwise `blocked`.
6. Resolve matching automatic Gate alerts for `passed` and `recorded`; observe a stable alert for blocked, review, or insufficient evidence status.
7. Persist the event ID, source watermark digest, and cause set hash on the release projection.

Alert payload contains run ID, decision ID, policy ID, rule ID, evidence hash, outbox event ID, and cause set hash. It excludes evidence bodies and credentials.

- [ ] **Step 5: Run Gate tests and verify GREEN**

Run:

```bash
cd backend
go test -tags=unit ./internal/service ./internal/repository \
  -run 'Gate|OutboxDispatcher' -count=1
RADAR_INTEGRATION=1 go test -tags=integration ./internal/repository \
  -run 'Gate|AutomatedGateOutcome' -count=1
```

Expected result: all tests pass and the forced alert failure proves transaction rollback.

- [ ] **Step 6: Commit automatic Gate evaluation**

```bash
git add backend/internal/service/evaluation_gate_service.go \
  backend/internal/repository/evaluation_reliability_gate.go \
  backend/internal/repository/evaluation_reliability_gate_test.go \
  backend/internal/repository/evaluation_governance_repo.go \
  backend/internal/repository/evaluation_governance_repo_test.go \
  backend/internal/repository/evaluation_governance_repo_integration_test.go \
  backend/internal/service/evaluation_outbox_dispatcher_test.go
git commit -m "feat(radar): project automatic gate outcomes atomically"
```

### Task 6: Wire configuration, lifecycle, and worker capabilities

**Files:**

- Modify: `backend/internal/config/config.go`
- Modify: `backend/internal/config/radar_config_test.go`
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/internal/repository/wire.go`
- Modify: `backend/cmd/server/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`
- Modify: `backend/cmd/server/wire_gen_test.go`
- Modify: `deploy/docker-compose.radar-staging.yml`
- Modify: `radar-worker/deploy/docker-compose.staging.yml`

**Interfaces:**

- Consumes: `RADAR_OUTBOX_CONSUMER_MODE` and `TimingWheelService`.
- Produces: startup and cleanup ownership for one consumer runtime.

- [ ] **Step 1: Write config and cleanup tests**

```go
func TestLoadRadarOutboxConsumerModeDefaultsToCore(t *testing.T) {
    resetViperWithJWTSecret(t)
    cfg, err := Load()
    require.NoError(t, err)
    require.Equal(t, "core", cfg.Radar.OutboxConsumerMode)
}

func TestLoadRejectsInvalidRadarOutboxConsumerMode(t *testing.T) {
    resetViperWithJWTSecret(t)
    t.Setenv("RADAR_OUTBOX_CONSUMER_MODE", "unsafe")
    _, err := Load()
    require.ErrorContains(t, err, "radar.outbox_consumer_mode")
}
```

Extend the cleanup test with a runtime whose scheduler records cancellation, then assert `provideCleanup` stops it.

- [ ] **Step 2: Run config and Wire tests and verify RED**

Run:

```bash
cd backend
go test -tags=unit ./internal/config ./cmd/server \
  -run 'RadarOutboxConsumerMode|ProvideCleanup' -count=1
```

Expected result: configuration fields and cleanup arguments are absent.

- [ ] **Step 3: Add mode validation and provider wiring**

Add `OutboxConsumerMode string` to `RadarConfig`, default it to `core`, and validate the exact three values. `ProvideEvaluationOutboxConsumerRuntime` starts only when Radar is enabled and mode is not `disabled`.

Register the new repository and service providers and bind the scheduler to `TimingWheelService`. Add the runtime to `provideCleanup` immediately after route evidence terminalization so shutdown cancels new claims before database resources close.

Regenerate Wire with the repository's existing generation command, then retain only graph changes caused by these providers.

- [ ] **Step 4: Add deployment configuration and Global worker capability**

Set the control plane environment to:

```yaml
RADAR_OUTBOX_CONSUMER_MODE: ${RADAR_OUTBOX_CONSUMER_MODE:-core}
```

Set both staging statistics defaults to include `global`.

```yaml
RADAR_ANALYSIS_CAPABILITIES: ${RADAR_ANALYSIS_CAPABILITIES:-coding,reasoning,safety,tool_use,protocol,global}
```

- [ ] **Step 5: Run config, Wire, and compose validation and verify GREEN**

Run:

```bash
cd backend
go test -tags=unit ./internal/config ./cmd/server -run 'Radar|ProvideCleanup' -count=1
go test -tags=unit ./internal/service -run 'OutboxRuntime' -count=1
cd ..
docker compose -f deploy/docker-compose.radar-staging.yml config --quiet
docker compose -f radar-worker/deploy/docker-compose.staging.yml config --quiet
```

Expected result: Go tests pass and both Compose files validate when their required staging variables are supplied.

- [ ] **Step 6: Commit configuration and lifecycle wiring**

```bash
git add backend/internal/config/config.go backend/internal/config/radar_config_test.go \
  backend/internal/service/wire.go backend/internal/repository/wire.go \
  backend/cmd/server/wire.go backend/cmd/server/wire_gen.go \
  backend/cmd/server/wire_gen_test.go deploy/docker-compose.radar-staging.yml \
  radar-worker/deploy/docker-compose.staging.yml
git commit -m "feat(radar): wire the outbox consumer lifecycle"
```

### Task 7: Replace manual E2E progression and deploy staging

**Files:**

- Modify: `backend/internal/integration/radar_revision_pipeline_e2e_test.go`
- Create: `backend/internal/integration/radar_outbox_consumer_e2e_test.go`
- Modify: `docs/superpowers/evidence/radar-g1-evidence-revision-verification.md`

**Interfaces:**

- Consumes: the complete production consumer and existing staging Compose project.
- Produces: end-to-end proof for initial, regrade, no-Gate, and full-Gate paths.

- [ ] **Step 1: Write the E2E test against a real consumer cycle**

Remove `completeRadarOutbox` as the mechanism that advances the revision pipeline. Start an `EvaluationOutboxConsumerRuntime` with short test intervals and production repositories, then wait for the observable database state.

```go
require.Eventually(t, func() bool {
    var pending, leased, dead int
    require.NoError(t, db.QueryRow(`
        SELECT COUNT(*) FILTER (WHERE status='pending'),
               COUNT(*) FILTER (WHERE status='leased'),
               COUNT(*) FILTER (WHERE status='dead_letter')
        FROM evaluation_outbox_events WHERE run_id=$1`, runID).Scan(&pending, &leased, &dead))
    return pending == 0 && leased == 0 && dead == 0
}, 30*time.Second, 100*time.Millisecond)
```

Cover multi-cell Cell to Global to Gate, direct single-cell Gate, historical single-cell Global compatibility, no-Gate Run completion, full Gate projections, duplicate delivery idempotency, and revision batch epoch fencing.

- [ ] **Step 2: Run E2E tests and verify RED, then GREEN after integration fixes**

Run:

```bash
cd backend
RADAR_E2E=1 go test -tags=e2e ./internal/integration \
  -run 'RadarOutboxConsumer|RadarRevisionPipeline' -count=1 -timeout=10m
```

Expected RED result: the first consumer-driven transition that remains incomplete identifies a real integration gap. Apply only the smallest production fix with a focused failing regression test, then rerun until all selected E2E tests pass.

- [ ] **Step 3: Run local release verification**

Run:

```bash
cd backend
go test -tags=unit ./internal/service ./internal/repository ./internal/config ./cmd/server -count=1
go test ./internal/service ./internal/repository ./internal/config ./cmd/server -count=1
go build ./cmd/server
cd ..
git diff --check
```

Expected result: every command exits 0 and `git diff --check` prints no output.

- [ ] **Step 4: Commit E2E proof**

```bash
git add backend/internal/integration/radar_revision_pipeline_e2e_test.go \
  backend/internal/integration/radar_outbox_consumer_e2e_test.go \
  docs/superpowers/evidence/radar-g1-evidence-revision-verification.md
git commit -m "test(radar): verify automatic outbox propagation"
```

- [ ] **Step 5: Build and deploy core mode to staging**

Build the control-plane and worker images from the verified commit. Preserve the existing environment and set `RADAR_OUTBOX_CONSUMER_MODE=core`. Before replacing containers, record the current image digest, container IDs, restart counts, Run counts, outbox counts, Analysis Job counts, Aggregate counts, and free disk space.

Deploy with the existing Compose project at:

```text
/opt/sub2api-builds/radar-release-20260801-f59afc0c/deploy/docker-compose.radar-staging.yml
```

Observe Run `2719e76a-f573-4c89-bc6c-2c07d1ad8d68` until all supported outbox events leave `pending` and `leased`, Analysis Jobs and Aggregate Heads exist, the Run reaches `completed`, service health remains stable for 60 seconds, and container restart counts stay unchanged.

- [ ] **Step 6: Deploy full mode and verify atomic Gate projections**

Set `RADAR_OUTBOX_CONSUMER_MODE=full`, restart only the control plane, and execute a test Run with an approved Policy Head, active Release Subject, and required reliability evidence. Verify one decision, one current decision head, one release projection, the expected alert status, and no duplicates after replaying the event.

- [ ] **Step 7: Record evidence and retain a verified rollback**

Append commands, timestamps, image digests, database counts, health output, and Run IDs to `radar-g1-evidence-revision-verification.md`. Keep `sub2api/radar-control-plane:rollback-pre-grading-reclaim-20260801` until core and full observations both pass. If an acceptance condition fails, restore that image through the same Compose file and leave outbox rows intact for replay.

## Plan Self-Review

1. Spec coverage maps lease semantics to Task 1, dispatch to Task 2, runtime behavior to Task 3, propagation and Run barriers to Task 4, Gate trust and projections to Task 5, configuration and lifecycle to Task 6, and end-to-end rollout to Task 7.
2. Placeholder scan found no deferred implementation markers or undefined follow-up work.
3. Type review confirms dispatcher inputs use existing `EvaluationOutboxEvent`, Aggregate requests use existing `CellAnalysisJobRequest` and `GlobalAnalysisJobRequest`, and the new Gate target and outcome types are defined before their consumers.
4. Staging rollout preserves the existing rollback image and does not add a migration.
