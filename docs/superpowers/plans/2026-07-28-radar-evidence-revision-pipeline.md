# Radar Evidence 与 Revision Pipeline 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 migration 199 的证据与修订链，使网关 Route Evidence 可独立验签，Score 与 Aggregate 全部追加写，completed Run 可以通过 fenced Revision Batch 重评，并让每次 Head 推进沿多父因果 outbox 强制传播。

**Architecture:** Gateway 在上游分发前创建 open evidence，transport 与 billing 通过 compare-and-swap patch 合并，Finalizer 生成 RFC 8785 Envelope、SHA256 与 HMAC 后封存。Grader 追加 Score 并推进完整 ScoreRef Head，Statistics 只消费 frozen ScoreRef 或 SnapshotRef。Revision Batch 承载 regrade epoch、requirements 与恢复代次。统一 outbox 保存所有直接 cause，cell、global、Gate 和 projection 的产物都能回溯至 source Head Event。

**Tech Stack:** Go 1.24、Gin、PostgreSQL 18、`database/sql`、RFC 8785 JSON Canonicalization、SHA256、HMAC-SHA256、Python 3.12、httpx、pytest。

## 范围与不变量

- 本计划消费 migration 197 已提供的 frozen Pair Binding、完整 ScoreRef、Run/Worker epoch 与 writer protocol。
- migration 199 使用 expand-compatible 字段。历史 Route Evidence 永久保持 unsealed 或 legacy，历史 Aggregate 标记 `legacy-untrusted`。
- sealed Evidence、Score、Snapshot、Head Event、cause relation 和 Revision Batch identity 均为追加式数据。业务 UPDATE 或 DELETE 由数据库拒绝。
- Score、Job、Manual Review、Head 与 Snapshot 全部用 `(score_id, score_created_at)`。Aggregate 相关对象全部用 `(snapshot_id, window_start)`。
- 所有 regrade 工作必须含同 Run 的 `revision_batch_id`，initial 工作必须为空。
- 每个任务先观察 RED，再实现最小闭环，随后观察 GREEN 并独立提交。

## 跨计划执行顺序

执行本计划前先完成 Worker 与 Run Lifecycle Tasks 1 至 9，以及 Trusted Gate Tasks 1 至 2。完成本计划 Task 1 后执行 Trusted Gate Task 3，把 Reliability 与 Gate evidence schema 加入同一个 migration 199 文件。随后执行本计划 Tasks 2 至 8、Trusted Gate Tasks 4 至 8、本计划 Task 9、Trusted Gate Task 9。migration 199 staging cutover 只允许在全部 schema 与 protocol-aware writers 已装配后运行。

## 任务依赖图

```text
Task 1 migration 199 expand
  -> Task 2 request semantics and evidence CAS
  -> Task 3 finalize and key epochs
  -> Task 4 ScoreRef and Score Head
  -> Task 5 Revision Batch
  -> Task 6 cell and global revision
  -> Task 7 multi-cause outbox
  -> Task 8 replacement propagation and batch convergence
  -> Task 9 cutover and verification
```

### Task 1: 建立 migration 199 revision schema

**Files:**
- Create: `backend/migrations/199_add_radar_evidence_revision_pipeline.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Modify: `backend/internal/repository/evaluation_route_evidence_repo_integration_test.go`
- Modify: `backend/internal/repository/evaluation_grading_repo_integration_test.go`
- Test: `backend/internal/repository/migrations_schema_integration_test.go`

**Consumes:** migration 197 与 migration 198 schema，目标 writer protocol version。

**Produces:** Request Semantics、versioned Route Evidence、Revision Batch、Batch Requirement、Score Head Event、Analysis frozen inputs、Aggregate Head、统一 outbox 与 cause relation。

- [ ] **Step 1: 写 migration 199 schema 失败测试**

```go
func TestMigration199EvidenceRevisionSchema(t *testing.T) {
    requireTable(t, db, "evaluation_request_semantics")
    requireTable(t, db, "evaluation_revision_batches")
    requireTable(t, db, "evaluation_revision_batch_requirements")
    requireTable(t, db, "evaluation_score_head_events")
    requireTable(t, db, "evaluation_aggregate_heads")
    requireTable(t, db, "evaluation_outbox_events")
    requireTable(t, db, "evaluation_outbox_event_causes")
    requireColumns(t, db, "evaluation_route_evidence", []string{
        "assignment_id", "request_ordinal", "lease_epoch", "evidence_revision",
        "sealed_at", "payload_hash", "signing_key_id", "payload_hmac",
    })
}
```

测试还要验证所有 regrade 表具有 `(revision_batch_id, run_id)` 复合 FK，ScoreRef 与 SnapshotRef 可以命中分区记录。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestMigration199EvidenceRevisionSchema -count=1`

Expected: FAIL，migration 199 对象不存在。

- [ ] **Step 3: 编写 expand schema 与约束**

```sql
CREATE TABLE evaluation_request_semantics (
  id uuid PRIMARY KEY,
  schema_version text NOT NULL,
  canonical_semantics_bytes bytea NOT NULL,
  request_semantics_sha256 char(64) NOT NULL,
  created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
  UNIQUE (id, request_semantics_sha256),
  UNIQUE (request_semantics_sha256)
);

CREATE UNIQUE INDEX uq_evaluation_route_evidence_assignment_ordinal
  ON evaluation_route_evidence (assignment_id, request_ordinal)
  WHERE assignment_id IS NOT NULL;

CREATE UNIQUE INDEX uq_evaluation_revision_batches_active_run
  ON evaluation_revision_batches (run_id)
  WHERE status IN ('pending', 'running', 'blocked');
```

同一 migration 增加 sealed row 与 revision object 的 immutable trigger、ScoreRef/SnapshotRef 复合外键、initial/regrade check、cause event 时序检查和 outbox insert-only 字段保护。

- [ ] **Step 4: 写数据库拒绝测试**

尝试修改 sealed Evidence identity、UPDATE Score、DELETE Snapshot、跨 Run 绑定 Batch、创建 regrade job 且 Batch 为空、创建 initial job 且 Batch 非空、cause 指向未来 event，逐项断言事务失败。

- [ ] **Step 5: 运行 schema 测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'TestMigration199|TestEvaluation.*Immutable|TestEvaluation.*CompositeRef' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/migrations/199_add_radar_evidence_revision_pipeline.sql backend/internal/repository/migrations_schema_integration_test.go backend/internal/repository/evaluation_route_evidence_repo_integration_test.go backend/internal/repository/evaluation_grading_repo_integration_test.go
git commit -m "feat(radar): add evidence revision schema"
```

### Task 2: 实现 Request Semantics 与 Route Evidence CreateOpen/CAS patch

**Files:**
- Create: `backend/internal/service/evaluation_request_semantics.go`
- Create: `backend/internal/service/evaluation_request_semantics_test.go`
- Modify: `backend/internal/service/evaluation_route_evidence.go`
- Modify: `backend/internal/service/evaluation_route_evidence_test.go`
- Modify: `backend/internal/repository/evaluation_route_evidence_repo.go`
- Modify: `backend/internal/repository/evaluation_route_evidence_repo_test.go`
- Modify: `backend/internal/repository/evaluation_route_evidence_repo_integration_test.go`
- Modify: `backend/internal/server/middleware/evaluation_evidence.go`
- Modify: `backend/internal/server/middleware/evaluation_evidence_test.go`
- Test: `backend/internal/repository/evaluation_route_evidence_repo_test.go`

**Consumes:** Task 1 schema，migration 197 的 Request Manifest、PairSpec、Assignment lease epoch。

**Produces:** canonical Request Semantics、分发前 CreateOpen、transport/billing CAS patch、有限冲突错误。

- [ ] **Step 1: 写语义与 merge matrix 失败测试**

```go
func TestRequestSemanticsGoldenHash(t *testing.T)
func TestCreateOpenRejectsExactSlotMismatchBeforeDispatch(t *testing.T)
func TestCreateOpenRejectsAdapterPolicyWithoutRegisteredVerifier(t *testing.T)
func TestPatchRouteEvidenceRejectsStaleRevision(t *testing.T)
func TestPatchRouteEvidenceAllowsNullToValueOnce(t *testing.T)
func TestPatchRouteEvidenceCannotMutateIdentityOrClearField(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/server/middleware -run 'Test(RequestSemantics|CreateOpen|PatchRouteEvidence)' -count=1`

Expected: FAIL，当前 Repository 仍使用 mutable upsert 或缺少 revision。

- [ ] **Step 3: 定义 canonical Request Semantics**

```go
type RequestSemantics struct {
    SchemaVersion       string        `json:"schema_version"`
    InteractionType     string        `json:"interaction_type"`
    SlotID              string        `json:"slot_id"`
    RequestOrdinal      int           `json:"request_ordinal"`
    Phase               string        `json:"phase"`
    MessageRoleSequence []string      `json:"message_role_sequence"`
    ContentPartTypes    [][]string    `json:"content_part_types"`
    PromptHash          string        `json:"prompt_hash"`
    ToolSchemaHash      string        `json:"tool_schema_hash"`
    ProvidedToolSetHash string        `json:"provided_tool_set_hash"`
    ToolChoicePolicy    string        `json:"tool_choice_policy"`
    SamplingPolicyHash  string        `json:"sampling_policy_hash"`
    PreviousEvidence   []EvidenceRef `json:"previous_evidence_refs"`
}
```

类型中不包含 Prompt、工具参数或 Completion 正文。canonical bytes 使用 RFC 8785，数据库保存 bytes 与 hash，外部响应只返回 ID 与 hash。

- [ ] **Step 4: 实现分发前 CreateOpen**

CreateOpen 先锁 Run，再锁 current Assignment，复算 manifest，按 ordinal 唯一匹配 slot，验证 exact hash 或调用注册 verifier。网关 service identity、request ID、image digest、tool schema 与 allowed tool set 均为必填。失败时在同一受控事务封存 `protocol_failed` evidence，随后禁止上游分发。

- [ ] **Step 5: 用显式 CAS 替换 upsert**

```go
type RouteEvidencePatch struct {
    ExpectedRevision int64
    Transport        *TransportPatch
    Billing          *BillingPatch
}

type RouteEvidenceRevisionConflict struct {
    CurrentRevision int64
}
```

patch 只针对已存在 open row。行锁内执行 merge matrix，合法变化将 revision 增加一。同值重试返回当前 revision。stale revision 返回 409 与 current revision。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/server/middleware -run 'Test(RequestSemantics|CreateOpen|PatchRouteEvidence)' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_request_semantics.go backend/internal/service/evaluation_request_semantics_test.go backend/internal/service/evaluation_route_evidence.go backend/internal/service/evaluation_route_evidence_test.go backend/internal/repository/evaluation_route_evidence_repo.go backend/internal/repository/evaluation_route_evidence_repo_test.go backend/internal/repository/evaluation_route_evidence_repo_integration_test.go backend/internal/server/middleware/evaluation_evidence.go backend/internal/server/middleware/evaluation_evidence_test.go
git commit -m "feat(radar): version open route evidence"
```

### Task 3: 封存 RouteEvidenceEnvelope 并实现 signing key state epoch

**Files:**
- Create: `backend/internal/service/evaluation_evidence_envelope.go`
- Create: `backend/internal/service/evaluation_evidence_envelope_test.go`
- Create: `backend/internal/repository/evaluation_evidence_finalizer.go`
- Create: `backend/internal/repository/evaluation_evidence_finalizer_test.go`
- Modify: `backend/internal/service/evaluation_route_evidence.go`
- Modify: `backend/internal/repository/evaluation_route_evidence_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_test.go`
- Modify: `backend/internal/server/middleware/evaluation_evidence.go`
- Test: `backend/internal/repository/evaluation_evidence_finalizer_test.go`

**Consumes:** Task 2 open evidence 与 frozen identity，Run terminalization outbox。

**Produces:** RFC 8785 Envelope、SHA256、HMAC、normal/system Finalize、sealed retry、key `state_epoch`。

- [ ] **Step 1: 写 Envelope golden 与并发失败测试**

golden fixture 固定 UUID、UTC 六位微秒、八位小数 billed amount、null 字段和 fallback chain。并发测试覆盖 Finalize 与 cancel 的两种锁获取顺序。

```go
func TestRouteEvidenceEnvelopeGoldenBytesHashAndHMAC(t *testing.T)
func TestFinalizeRejectsNonContiguousFallbackAttempts(t *testing.T)
func TestFinalizeThenCancelKeepsSealedEvidence(t *testing.T)
func TestCancelThenFinalizeReturnsLeaseFenced(t *testing.T)
func TestSealedRetryRequiresIdenticalServerGeneratedEnvelope(t *testing.T)
func TestRevokedSigningKeyInvalidatesEvidence(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(RouteEvidenceEnvelope|Finalize|CancelThenFinalize|SealedRetry|RevokedSigningKey)' -count=1`

Expected: FAIL，Envelope 或 Finalizer 尚未定义。

- [ ] **Step 3: 实现完整 Envelope 类型与 canonicalizer**

类型必须显式列出 G1 10.4 节的全部字段，所有键都参与编码，可空字段编码为 JSON null。HMAC domain separator 固定如下。

```go
func SignEvidence(schema string, canonical []byte, key []byte) string {
    mac := hmac.New(sha256.New, key)
    mac.Write([]byte(schema))
    mac.Write([]byte{0x0a})
    mac.Write(canonical)
    return hex.EncodeToString(mac.Sum(nil))
}
```

- [ ] **Step 4: 实现 normal 与 system Finalize**

normal Finalize 验证 current Assignment、Run 非终态、lease epoch、expected revision、billing completeness 和 fallback invariants。system Finalizer 只接受匹配 terminalization outbox Event ID 与 current epoch。两者都选择 active key、使用 database transaction time、revision 增加一并原子封存。

- [ ] **Step 5: 实现 signing key 状态事件**

active、verify_only、revoked 每次变化增加不可回退 `state_epoch`，状态变更与 gate reevaluation outbox 同事务。常规轮换保留 verify_only key。revoked key 触发完整性告警并使引用证据无法进入可信 Gate。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(RouteEvidenceEnvelope|Finalize|CancelThenFinalize|SealedRetry|RevokedSigningKey)' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_evidence_envelope.go backend/internal/service/evaluation_evidence_envelope_test.go backend/internal/repository/evaluation_evidence_finalizer.go backend/internal/repository/evaluation_evidence_finalizer_test.go backend/internal/service/evaluation_route_evidence.go backend/internal/repository/evaluation_route_evidence_repo.go backend/internal/repository/evaluation_governance_repo.go backend/internal/repository/evaluation_governance_repo_test.go backend/internal/server/middleware/evaluation_evidence.go
git commit -m "feat(radar): seal signed route evidence"
```

### Task 4: 切换 ScoreRef、Score Head 与 eligible projection

**Files:**
- Modify: `backend/internal/service/evaluation_grading.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Create: `backend/internal/repository/evaluation_grading_repo_test.go`
- Modify: `backend/internal/repository/evaluation_grading_repo_integration_test.go`
- Modify: `backend/internal/handler/internal/radar_grader_handler.go`
- Modify: `backend/internal/handler/internal/radar_grader_handler_test.go`
- Modify: `radar-worker/src/sub2api_radar/models.py`
- Modify: `radar-worker/src/sub2api_radar/grader.py`
- Modify: `radar-worker/tests/test_graders.py`
- Test: `backend/internal/repository/evaluation_grading_repo_integration_test.go`

**Consumes:** Task 3 sealed Evidence refs 与 set hash。

**Produces:** immutable Score、完整 ScoreRef、Score Head Event、eligible current Head 与重算 event。

- [ ] **Step 1: 写 Head 推进失败测试**

```go
func TestSubmitScoreUsesCompositeScoreRef(t *testing.T)
func TestSubmitScoreRequiresCurrentAssignmentAndSealedEvidenceSet(t *testing.T)
func TestSubmitScoreAppendsHeadEventAndOutboxAtomically(t *testing.T)
func TestAssignmentReplacementImmediatelyRemovesOldHeadEligibility(t *testing.T)
func TestScoreZeroRemainsSuccessfulEligibleResult(t *testing.T)
func TestProductionCodeNeverUpdatesScoreIsCurrent(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal -run 'Test(SubmitScore|AssignmentReplacement|ScoreZero|ProductionCodeNever)' -count=1`

Expected: FAIL，当前业务仍依赖 `is_current` 或缺完整 locator。

- [ ] **Step 3: 定义 ScoreRef 与 evidence binding**

```go
type ScoreRef struct {
    ID        uuid.UUID `json:"score_id"`
    CreatedAt time.Time `json:"score_created_at"`
}

type ScoreSource struct {
    AssignmentID        uuid.UUID
    RouteEvidenceSetHash string
    RouteEvidenceRefs   []RouteEvidenceRef
    ArtifactManifestHash string
}
```

Evidence refs 按 request ordinal 排序，set hash 同时绑定 expected manifest hash、Trace ID、ordinal 与 payload hash。

- [ ] **Step 4: 原子推进 Head**

事务锁定 `(sample_id, grader_id)` Head，验证 fixed grader identity、current Assignment、sealed Evidence set 与 version，依次插入 Score、Head Event、推进 Head、计算 pair completeness、插入 recompute outbox。单侧推进只产生 outbox，不创建半对 Analysis Job。

- [ ] **Step 5: 更新 Worker 提交协议**

Worker 只提交 grading result、固定 input identity 和 lease，不选择 Score version、revision 或 Head。API 返回 server-generated ScoreRef 与 Head version。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal -run 'Test(SubmitScore|AssignmentReplacement|ScoreZero|ProductionCodeNever)' -count=1 && cd ../radar-worker && uv run pytest tests/test_grader.py -q`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_grading.go backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_grading_repo_test.go backend/internal/repository/evaluation_grading_repo_integration_test.go backend/internal/handler/internal/radar_grader_handler.go backend/internal/handler/internal/radar_grader_handler_test.go radar-worker/src/sub2api_radar/models.py radar-worker/src/sub2api_radar/grader.py radar-worker/tests/test_graders.py
git commit -m "feat(radar): advance immutable score heads"
```

### Task 5: 实现 Revision Batch、recovery generation 与 regrade fencing

**Files:**
- Create: `backend/internal/service/evaluation_revision_batch.go`
- Create: `backend/internal/repository/evaluation_revision_batch_repo.go`
- Create: `backend/internal/repository/evaluation_revision_batch_repo_test.go`
- Modify: `backend/internal/service/evaluation_governance.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/server/routes/radar_governance_routes_test.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Test: `backend/internal/repository/evaluation_revision_batch_repo_test.go`

**Consumes:** Task 4 eligible Score Head 与 G1 Run completed terminal state。

**Produces:** Batch 创建、fence、resume、cancel、requirement freeze、regrade Job identity、compensating Head 审批入口。

- [ ] **Step 1: 写 Batch 状态与幂等失败测试**

```go
func TestCreateRevisionBatchRequiresCompletedRun(t *testing.T)
func TestCreateRevisionBatchFreezesGradingRequirements(t *testing.T)
func TestRunAllowsOnlyOneActiveRevisionBatch(t *testing.T)
func TestRevisionBatchFenceIncrementsEpochAndRequeuesSafeWork(t *testing.T)
func TestBlockedBatchRepairCreatesNextRecoveryGeneration(t *testing.T)
func TestBatchCancelRejectsAfterEligibleHeadAdvance(t *testing.T)
func TestCompensatingHeadRequiresTwoDistinctApprovers(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/handler/admin -run 'Test.*RevisionBatch|TestBlockedBatch|TestBatchCancel|TestCompensatingHead' -count=1`

Expected: FAIL，Batch 类型与 API 缺失。

- [ ] **Step 3: 实现 Batch 与 requirement 收敛器**

```go
type RevisionBatchStatus string
const (
    RevisionBatchPending RevisionBatchStatus = "pending"
    RevisionBatchRunning RevisionBatchStatus = "running"
    RevisionBatchBlocked RevisionBatchStatus = "blocked"
    RevisionBatchCompleted RevisionBatchStatus = "completed"
    RevisionBatchFailed RevisionBatchStatus = "failed"
    RevisionBatchCancelled RevisionBatchStatus = "cancelled"
)
```

创建时冻结 Assignment、prior ScoreRef、grader identity、`grading_input_hash`。普通重试固定 generation 0。blocked repair 在审批 event 后创建下一 generation 的 replacement requirement，并将旧 failed requirement 标为 superseded。

- [ ] **Step 4: 实现 epoch 和取消边界**

fence 递增 Batch epoch，旧 Grading、Analysis 与 outbox lease 立即失效。resume 只接受无 active lease 且无 failed grading requirement 的 blocked Batch。Head 尚未推进时允许 cancel；Head 已推进时返回 `revision_batch_propagation_required`。

- [ ] **Step 5: 暴露受 RBAC 保护的 API**

实现创建 Batch、`/:id/fence`、`/:id/resume`、`/:id/cancel`、`/:id/repair` 与 compensating Head 审批。每个写请求使用 64 位 idempotency key，actor 从认证上下文获取。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository ./internal/handler/admin ./internal/server/routes -run 'Test.*RevisionBatch|TestBlockedBatch|TestBatchCancel|TestCompensatingHead' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_revision_batch.go backend/internal/repository/evaluation_revision_batch_repo.go backend/internal/repository/evaluation_revision_batch_repo_test.go backend/internal/service/evaluation_governance.go backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go backend/internal/server/routes/admin.go backend/internal/server/routes/radar_governance_routes_test.go backend/internal/repository/evaluation_grading_repo.go
git commit -m "feat(radar): orchestrate revision batches"
```

### Task 6: 实现 cell/global Analysis Job 与 Aggregate Head

**Files:**
- Create: `backend/internal/service/evaluation_aggregate_revision.go`
- Create: `backend/internal/service/evaluation_aggregate_revision_test.go`
- Create: `backend/internal/repository/evaluation_aggregate_repo.go`
- Create: `backend/internal/repository/evaluation_aggregate_repo_test.go`
- Create: `backend/internal/repository/evaluation_aggregate_repo_integration_test.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/handler/internal/radar_grader_handler.go`
- Modify: `radar-worker/src/sub2api_radar/statistics/service.py`
- Modify: `radar-worker/tests/test_statistics.py`
- Test: `backend/internal/repository/evaluation_aggregate_repo_test.go`

**Consumes:** Task 4 Score Head Event，Task 5 regrade Batch context。

**Produces:** deterministic `input_set_hash`、cell/global Job、immutable Snapshot、Aggregate Head 与 stale job fencing。

- [ ] **Step 1: 写 canonical input 与 stale job 失败测试**

```go
func TestCellInputSetHashIgnoresInputOrder(t *testing.T)
func TestCellJobRequiresCompleteBaselineCandidatePairs(t *testing.T)
func TestCellSnapshotRejectsDifferentScoreRefSet(t *testing.T)
func TestStaleCellJobCannotAdvanceAggregateHead(t *testing.T)
func TestMultiCellRunRequiresExplicitGlobalSnapshot(t *testing.T)
func TestGlobalInputHashChangesWhenAnyCellHeadChanges(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(Cell|StaleCell|MultiCell|GlobalInput)' -count=1`

Expected: FAIL，旧 Analysis Job 缺 frozen input hash 或 Head。

- [ ] **Step 3: 实现 canonical cell identity 与 input hash**

cell key 固定为 Run ID、capability domain、去除一个 side 前缀后的 canonical model route。Score 输入按 G1 12.2 字段稳定排序，完整 pair 数等于 expected pair 数且大于零。

```go
type SnapshotRef struct {
    ID          uuid.UUID `json:"snapshot_id"`
    WindowStart time.Time `json:"window_start"`
}
```

- [ ] **Step 4: 实现 Job 与 Snapshot 提交验证**

Job 固化 scope、work origin、Batch ID、input hash、ScoreRefs、SnapshotRefs、aggregate revision、analysis version 与 cause set hash。Statistics Worker 只能提交 job 固化集合。Repository 追加 Snapshot 后重新计算 current eligible set；匹配时推进 Head，stale 时只保留历史 Snapshot。

- [ ] **Step 5: 实现 global/global 聚合**

预期 cell 从 frozen PairSpec 与 SideSpec 导出。多 cell Run 只有全部 current cell Snapshot 匹配时才创建 global Job。单 cell Run 显式记录 global not required。global Snapshot 保存所有 source SnapshotRef 与直接 cause。

- [ ] **Step 6: 更新 Statistics Worker**

Worker 不计算 revision，不选择输入，不推进 Head。它按返回顺序读取 frozen refs，输出有限 metrics 与 analysis result，提交时回传完整 input hash。

- [ ] **Step 7: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(Cell|StaleCell|MultiCell|GlobalInput)' -count=1 && cd ../radar-worker && uv run pytest tests/test_statistics_service.py -q`

Expected: PASS。

- [ ] **Step 8: 提交本任务**

```bash
git add backend/internal/service/evaluation_aggregate_revision.go backend/internal/service/evaluation_aggregate_revision_test.go backend/internal/repository/evaluation_aggregate_repo.go backend/internal/repository/evaluation_aggregate_repo_test.go backend/internal/repository/evaluation_aggregate_repo_integration_test.go backend/internal/repository/evaluation_grading_repo.go backend/internal/handler/internal/radar_grader_handler.go radar-worker/src/sub2api_radar/statistics/service.py radar-worker/tests/test_statistics.py
git commit -m "feat(radar): version cell and global aggregates"
```

### Task 7: 实现多父事件 outbox 与 cause closure

**Files:**
- Create: `backend/internal/service/evaluation_outbox.go`
- Create: `backend/internal/service/evaluation_outbox_test.go`
- Create: `backend/internal/repository/evaluation_outbox_repo.go`
- Create: `backend/internal/repository/evaluation_outbox_repo_test.go`
- Create: `backend/internal/repository/evaluation_outbox_repo_integration_test.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/repository/evaluation_aggregate_repo.go`
- Modify: `backend/internal/repository/evaluation_route_evidence_repo.go`
- Test: `backend/internal/repository/evaluation_outbox_repo_test.go`

**Consumes:** Tasks 3、4、6 的 seal 与 Head events，Task 5 Batch epoch。

**Produces:** stable dedup key、multi-cause relation、cause set hash、leased consumer、dead letter replay。

- [ ] **Step 1: 写 causation 与 fencing 失败测试**

```go
func TestOutboxDedupKeyIsStableAcrossRetry(t *testing.T)
func TestGlobalEventRecordsEveryCellCause(t *testing.T)
func TestCauseSetHashIsStableAcrossInputOrder(t *testing.T)
func TestRegradeOutboxRequiresSameRunBatch(t *testing.T)
func TestBatchFenceRejectsOldOutboxHandlerCommit(t *testing.T)
func TestDeadLetterReplayKeepsEventIdentityAndCauses(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(Outbox|GlobalEvent|CauseSet|RegradeOutbox|BatchFence|DeadLetter)' -count=1`

Expected: FAIL，统一 outbox Repository 尚未实现。

- [ ] **Step 3: 实现 canonical dedup 与 cause set**

```go
func OutboxDedupKey(eventType string, runID uuid.UUID, scopeKey, analysisVersion, sourceHash string) string
func CauseSetHash(causes []uuid.UUID) string
```

cause IDs 去重后按 UUID bytes 排序。合流 event 保存每个直接 cause relation，`causation_id` 与 `cause_set_hash` 从同一排序集合导出。根 event 从自身 immutable source tuple 生成。

- [ ] **Step 4: 实现 leased consumer fencing**

claim 使用 `FOR UPDATE SKIP LOCKED`。regrade event 先锁 Batch并复制 current epoch。heartbeat 与 handler commit 再次验证 Batch running、lease token、expiry 和 epoch。fence 后 reaper 将旧 epoch event 恢复 pending，新 claim 使用新 epoch。

- [ ] **Step 5: 原子接入所有 source 推进**

Route Evidence seal、Score Head、Aggregate Head、signing key state 和控制面 Head 推进均在同事务插入 outbox。业务 writer 只可更新 status、attempt、lease、有限 error code 与时间。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(Outbox|GlobalEvent|CauseSet|RegradeOutbox|BatchFence|DeadLetter)' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_outbox.go backend/internal/service/evaluation_outbox_test.go backend/internal/repository/evaluation_outbox_repo.go backend/internal/repository/evaluation_outbox_repo_test.go backend/internal/repository/evaluation_outbox_repo_integration_test.go backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_aggregate_repo.go backend/internal/repository/evaluation_route_evidence_repo.go
git commit -m "feat(radar): preserve revision causation"
```

### Task 8: 收敛 Batch requirements 与 assignment replacement 传播

**Files:**
- Create: `backend/internal/repository/evaluation_revision_pipeline.go`
- Create: `backend/internal/repository/evaluation_revision_pipeline_test.go`
- Modify: `backend/internal/repository/evaluation_revision_batch_repo.go`
- Modify: `backend/internal/repository/evaluation_repo.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/repository/evaluation_aggregate_repo.go`
- Modify: `backend/internal/repository/evaluation_outbox_repo.go`
- Modify: `backend/internal/integration/radar_p0_e2e_test.go`
- Test: `backend/internal/repository/evaluation_revision_pipeline_test.go`

**Consumes:** Tasks 4 至 7 的 Head、Batch、Snapshot 与 cause chain。

**Produces:** replacement 后旧 Head 失格、完整 cell/global/Gate requirement、blocked/recovery/completed 单调收敛。

- [ ] **Step 1: 写端到端状态失败测试**

```go
func TestReplacementInvalidatesOldScoreAndAggregateHeads(t *testing.T)
func TestHeadAdvanceAppendsCellGlobalAndGateRequirements(t *testing.T)
func TestPartialPropagationFailureBlocksBatch(t *testing.T)
func TestRepairSupersedesFailedRequirementAndCompletesOriginalCauseChain(t *testing.T)
func TestBatchCompletesOnlyAfterAllCurrentHeadsAndDecisionCoverCauseSet(t *testing.T)
func TestNewBatchIsNotSuppressedByHistoricalGeneration(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/integration -run 'Test(ReplacementInvalidates|HeadAdvanceAppends|PartialPropagation|RepairSupersedes|BatchCompletes|NewBatch)' -count=1`

Expected: FAIL，缺少 pipeline 收敛器或 replacement 传播。

- [ ] **Step 3: 实现 replacement 事务**

创建更高 Assignment attempt 时，立即让旧 source 的 Score Head 退出 eligible projection，取消绑定旧 Assignment 的 initial Grading Job，标记 stale Analysis Job，写带旧 Head Event 的 recompute outbox。历史 Score 与 Snapshot 保持不可变。

- [ ] **Step 4: 实现 requirement closure**

Score Head 推进追加 cell requirement，cell Head 推进追加 global 或 Gate requirement，global Head 推进追加 Gate requirement。每条 requirement 保存 source hash、cause set hash 与状态。failed requirement 只有插入 replacement 后才能 superseded。

- [ ] **Step 5: 实现 Batch Reconciler**

无 Head 推进且 grading 永久失败时 Batch failed。任何 Head 已推进且传播故障时 Batch blocked，可信 Gate 投影转为 insufficient evidence。只有全部 frozen 和派生 requirements 被 completed 或合法 superseded，current Head 与 Decision 覆盖完整 cause set 时 Batch completed。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository ./internal/integration -run 'Test(ReplacementInvalidates|HeadAdvanceAppends|PartialPropagation|RepairSupersedes|BatchCompletes|NewBatch)' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/repository/evaluation_revision_pipeline.go backend/internal/repository/evaluation_revision_pipeline_test.go backend/internal/repository/evaluation_revision_batch_repo.go backend/internal/repository/evaluation_repo.go backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_aggregate_repo.go backend/internal/repository/evaluation_outbox_repo.go backend/internal/integration/radar_p0_e2e_test.go
git commit -m "feat(radar): converge revision propagation"
```

### Task 9: 完成 migration 199 cutover 与 revision pipeline 验证

**Files:**
- Create: `backend/scripts/radar_migration_199_cutover.sh`
- Create: `backend/internal/integration/radar_revision_pipeline_e2e_test.go`
- Create: `docs/superpowers/evidence/radar-g1-evidence-revision-verification.md`
- Modify: `docs/radar-staging-runbook.md`
- Test: `backend/internal/integration/radar_revision_pipeline_e2e_test.go`

**Consumes:** Tasks 1 至 8 全部产物、Trusted Gate Tasks 1 至 8、migration 197 writer guard。

**Produces:** Gateway、Worker、scheduler、reaper、outbox 全部归零的 199 cutover 证明，以及 initial/regrade 全链路验收。

- [ ] **Step 1: 写 cutover 与 pipeline 失败验收**

测试在各类 writer 至少保留一个 session 或 lease 时断言 closed 前检查失败。全量归零后完成 199 schema 切换，运行 initial Score 到 Decision，以及 completed Run regrade 到新 Decision 的链路。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/integration -run TestRadarRevisionPipelineE2E -count=1`

Expected: FAIL，cutover 脚本或完整 pipeline 未连接。

- [ ] **Step 3: 实现 199 cutover 脚本**

脚本暂停 evaluation traffic 和新 claim，进入 draining，等待 Gateway evidence writer、replacement、Grader、Statistics、reaper、scheduler、outbox consumer、数据库事务和 advisory lock 全部归零，再由 migration owner closed、迁移、提升 protocol、reopen。任一步失败保持 closed。

- [ ] **Step 4: 运行全量后端与 Worker 测试**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/internal ./internal/handler/admin ./internal/integration -count=1 && cd ../radar-worker && uv run ruff check . && uv run mypy src && uv run pytest -q`

Expected: PASS。

- [ ] **Step 5: 运行竞态与重复性测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository ./internal/integration -run 'Test.*(Finalize|RevisionBatch|Outbox|Aggregate|RevisionPipeline)' -count=20`

Expected: PASS，无重复 Head、重复 event、cause 丢失或 stale writer 提交成功。

- [ ] **Step 6: 写验证证据**

记录 commit、migration checksum、protocol version、golden Envelope hash、测试命令摘要、dead letter replay、Batch fence 和 assignment replacement 的实测结果。证据文档不得包含 Prompt、Completion、token 或真实账号渠道。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/scripts/radar_migration_199_cutover.sh backend/internal/integration/radar_revision_pipeline_e2e_test.go docs/superpowers/evidence/radar-g1-evidence-revision-verification.md docs/radar-staging-runbook.md
git commit -m "test(radar): prove evidence revision pipeline"
```

## 完成标准

- 每次上游分发前已经创建绑定 Assignment、ordinal、manifest、slot、semantics 和 lease epoch 的 open Evidence。
- patch 遵守 merge matrix 与 expected revision，Finalize 产生可独立重建的 RFC 8785 bytes、SHA256 和 HMAC。
- signing key 状态变化递增 state epoch，revoked key 立即让引用证据失效。
- 所有 current Score 与 Aggregate 查询只读 Head，并使用完整 ScoreRef 或 SnapshotRef。
- regrade 只在 active Revision Batch 中运行，fence 后旧 Grading、Analysis 和 outbox 提交全部失败。
- cell/global Snapshot 固化输入，stale job 可留存历史产物且无法推进 Head。
- 每次 Head 推进与 outbox 同事务，多父 cause 集合可稳定复算并贯穿 Batch requirement。
- assignment replacement、部分传播失败、dead letter replay 和 recovery generation 均能收敛到可解释终态。
- migration 199 cutover 证明所有相关 writer、lease、事务和 lock 归零后才切换协议。
