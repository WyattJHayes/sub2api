# Radar Worker 与 Run 可信生命周期实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 migration 197、冻结实验输入、受控 Worker 生命周期、Run 控制面、统一 lease fencing 和 failure-first Reconciler，使所有 initial work 在可审计的状态机内单调收敛。

**Architecture:** PostgreSQL 保存不可变 Request Manifest、PairSpec、SideSpec、Pair Binding、Run transition event 与 writer protocol。Go Repository 统一采用 Run 优先锁序，claim 时复制 `control_epoch`，所有 heartbeat 与提交再次验证 epoch。管理 API 只表达 pause、resume、cancel、fence 和 Worker 控制意图，Run Reconciler 是终态归约的唯一写入者。

**Tech Stack:** Go 1.24、Gin、PostgreSQL 18、`database/sql`、`sqlmock`、Python 3.12、httpx、pytest、Docker Compose。

## 范围与执行纪律

- 本计划只覆盖 G1 的 migration 197 与 initial work 生命周期。Route Evidence sealing、regrade、Aggregate revision 和 Gate 由后续计划消费本计划产物。
- 每个 Radar 写事务先执行 `SET LOCAL app.evaluation_writer_protocol` 与 `SET LOCAL app.evaluation_writer_instance_id`，随后按 Run、Assignment 或 Job、产物的顺序加锁。
- 明文 Worker token 只允许存在于请求解码、短生命周期缓冲和 SHA256 计算过程。响应、日志、审计和数据库不得保存明文。
- 状态转换与对应 Run Event 位于同一事务。deferred constraint trigger 在提交前校验一一对应关系。
- `completed`、`failed`、`cancelled` 没有出边。completed Run 的 regrade 由独立 Revision Batch 承载。
- 手工执行每个任务时，必须先观察指定 RED，再写生产代码，再观察 GREEN，最后执行该任务提交。

## 跨计划执行顺序

G1 三份计划采用唯一顺序：先完成本计划 Tasks 1 至 9；再完成 Trusted Gate Tasks 1 至 2；随后完成 Evidence Revision Pipeline Task 1 和 Trusted Gate Task 3；接着完成 Evidence Revision Pipeline Tasks 2 至 8 与 Trusted Gate Tasks 4 至 8；最后执行 Evidence Revision Pipeline Task 9 和 Trusted Gate Task 9。该顺序保证 migration 197、198、199 单调前进，并在 migration 199 staging cutover 前装配全部 schema 与 protocol-aware writers。

## 任务依赖图

```text
Task 1 migration 197 expand
  -> Task 2 writer protocol
  -> Task 3 frozen request binding
  -> Task 4 worker control
  -> Task 5 run control API
  -> Task 6 lease fencing
  -> Task 7 reconciler
  -> Task 8 cutover acceptance
  -> Task 9 full verification
```

### Task 1: 建立 migration 197 expand schema

**Files:**
- Create: `backend/migrations/197_add_radar_trusted_lifecycle.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Modify: `backend/internal/repository/evaluation_grading_repo_integration_test.go`
- Test: `backend/internal/repository/migrations_schema_integration_test.go`

**Consumes:** migrations 190 至 196 的 Radar schema。

**Produces:** 冻结输入表、Run 控制字段、Worker event、完整 ScoreRef、lease epoch、writer protocol 表与数据库约束。

- [ ] **Step 1: 写 schema 失败测试**

新增 `TestMigration197TrustedLifecycleSchema`，检查四类冻结输入表、`evaluation_schema_cutovers`、`evaluation_writer_sessions`、Worker event、Run 控制列、三类工作的 lease 列和 `evaluation_score_heads.score_created_at`。

```go
requireColumns(t, db, "evaluation_runs", []string{
    "budget_mode", "paused_from_status", "pause_reason", "control_epoch",
    "state_version", "cancelled_at", "cancelled_by", "route_profile_version",
})
requireColumns(t, db, "evaluation_score_heads", []string{"score_id", "score_created_at"})
requireCompositeForeignKey(t, db, "evaluation_score_heads", []string{"score_id", "score_created_at"})
requireTable(t, db, "evaluation_request_manifests")
requireTable(t, db, "evaluation_pair_specs")
requireTable(t, db, "evaluation_side_specs")
requireTable(t, db, "evaluation_pair_bindings")
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test -tags integration ./internal/repository -run TestMigration197TrustedLifecycleSchema -count=1`

Expected: FAIL，首个缺失对象为 migration 197 表或列。

- [ ] **Step 3: 编写 expand migration**

迁移采用 nullable 或兼容默认值扩展现有行，新 Run 通过应用层写可信字段。关键约束写成数据库检查与复合外键。

```sql
ALTER TABLE evaluation_runs
  ADD COLUMN budget_mode text NOT NULL DEFAULT 'normal',
  ADD COLUMN paused_from_status text,
  ADD COLUMN pause_reason text,
  ADD COLUMN control_epoch bigint NOT NULL DEFAULT 0,
  ADD COLUMN state_version bigint NOT NULL DEFAULT 0,
  ADD COLUMN cancelled_at timestamptz,
  ADD COLUMN cancelled_by bigint REFERENCES users(id),
  ADD COLUMN route_profile_version text NOT NULL DEFAULT 'legacy-unbound';

ALTER TABLE evaluation_score_heads
  ADD COLUMN score_created_at timestamptz;

ALTER TABLE evaluation_score_heads
  ADD CONSTRAINT fk_evaluation_score_heads_score_ref
  FOREIGN KEY (score_id, score_created_at)
  REFERENCES evaluation_scores(id, created_at) DEFERRABLE INITIALLY DEFERRED;
```

同一 migration 创建不可变对象的 UPDATE/DELETE 拒绝 trigger、Run 状态图 trigger、transition event deferred constraint trigger，以及 writer protocol audit trigger。历史 Score Head 回填必须按 `(score_id, created_at)` 唯一命中，缺行或重复 UUID 通过异常中止 migration。

- [ ] **Step 4: 增加 migration 失败保护测试**

在独立事务构造缺失 ScoreRef、非法终态重开、缺少 transition event、修改 PairSpec 和删除 Request Manifest，逐项断言数据库拒绝提交。

- [ ] **Step 5: 运行 schema 与约束测试并确认 GREEN**

Run: `cd backend && go test -tags integration ./internal/repository -run 'TestMigration197|TestEvaluationScoreHeadRef' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/migrations/197_add_radar_trusted_lifecycle.sql backend/internal/repository/migrations_schema_integration_test.go backend/internal/repository/evaluation_grading_repo_integration_test.go
git commit -m "feat(radar): add trusted lifecycle schema"
```

### Task 2: 接入 Radar writer protocol 与 cutover guard

**Files:**
- Create: `backend/internal/repository/evaluation_writer_protocol.go`
- Create: `backend/internal/repository/evaluation_writer_protocol_test.go`
- Modify: `backend/internal/repository/evaluation_repo.go`
- Modify: `backend/internal/repository/evaluation_management_repo.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/repository/evaluation_route_evidence_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/config/config.go`
- Test: `backend/internal/repository/evaluation_writer_protocol_test.go`

**Consumes:** Task 1 的 cutover 与 writer session 表。

**Produces:** `WithEvaluationWriterTx`、session heartbeat、audit/enforce guard、受控 503 `radar_cutover_active`。

- [ ] **Step 1: 写 writer guard 失败测试**

覆盖 audit 模式记录旧 writer、enforce 模式拒绝旧 protocol、draining 拒绝新业务写、closed 仅允许 migration owner、兼容 writer 正常提交。

```go
func TestEvaluationWriterAuditAllowsAndRecordsOldProtocol(t *testing.T)
func TestEvaluationWriterEnforceRejectsOldProtocol(t *testing.T)
func TestEvaluationWriterDrainingRejectsNewWrite(t *testing.T)
func TestEvaluationWriterClosedAllowsMigrationOwnerOnly(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestEvaluationWriter -count=1`

Expected: FAIL，提示 writer protocol helper 未定义。

- [ ] **Step 3: 实现事务入口与领域错误**

```go
type EvaluationWriterIdentity struct {
    InstanceID      string
    Kind            string
    ProtocolVersion int64
}

func (r *Repository) WithEvaluationWriterTx(
    ctx context.Context,
    identity EvaluationWriterIdentity,
    fn func(*sql.Tx) error,
) error
```

helper 在事务内注册或刷新 session，设置两个 transaction-local 值，再执行业务回调。数据库返回 cutover guard 错误时映射为 `ErrRadarCutoverActive`，API 返回 503 和有限错误码。

- [ ] **Step 4: 把现有 Radar writer 收口到统一入口**

替换 Run create/start、claim、heartbeat、complete、fail、grading、statistics、evidence 和 governance 的直接 `BeginTx`。读取路径保持不变。测试通过 sqlmock 断言业务 SQL 前已经设置 writer identity。

- [ ] **Step 5: 运行 Repository 测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'TestEvaluationWriter|Test.*Radar.*Repository' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/repository/evaluation_writer_protocol.go backend/internal/repository/evaluation_writer_protocol_test.go backend/internal/repository/evaluation_repo.go backend/internal/repository/evaluation_management_repo.go backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_route_evidence_repo.go backend/internal/repository/evaluation_governance_repo.go backend/internal/config/config.go
git commit -m "feat(radar): enforce writer protocol"
```

### Task 3: 冻结 Request Manifest、PairSpec、SideSpec 与 Pair Binding

**Files:**
- Create: `backend/internal/service/evaluation_experiment_contract.go`
- Create: `backend/internal/service/evaluation_experiment_contract_test.go`
- Create: `backend/internal/repository/evaluation_experiment_contract_repo.go`
- Create: `backend/internal/repository/evaluation_experiment_contract_repo_test.go`
- Modify: `backend/internal/service/evaluation.go`
- Modify: `backend/internal/repository/evaluation_repo.go`
- Modify: `backend/internal/repository/evaluation_repo_integration_test.go`
- Test: `backend/internal/service/evaluation_experiment_contract_test.go`

**Consumes:** Task 1 的冻结输入表与 Task 2 的 writer transaction。

**Produces:** RFC 8785 canonical manifest、PairSpec、两条 SideSpec、Pair Binding，Run 开放 claim 前的完整绑定。

- [ ] **Step 1: 写 canonicalization 与验证失败测试**

固定 golden fixture，验证键序变化得到相同 hash，slot 重叠、ordinal 不连续、exact 与 policy hash 同时存在、额外 treatment 字段均被拒绝。

```go
func TestCanonicalRequestManifestGoldenHash(t *testing.T)
func TestRequestManifestRejectsAmbiguousSlots(t *testing.T)
func TestPairBindingRejectsUnapprovedTreatmentDifference(t *testing.T)
func TestCreateRunRollsBackWhenBindingIsIncomplete(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(CanonicalRequest|RequestManifest|PairBinding|CreateRunRolls)' -count=1`

Expected: FAIL，类型或 canonicalizer 尚未定义。

- [ ] **Step 3: 定义显式类型并实现 RFC 8785**

```go
type RequestManifest struct {
    SchemaVersion   string        `json:"schema_version"`
    InteractionType string        `json:"interaction_type"`
    OrdinalPolicy   string        `json:"ordinal_policy"`
    MinRequests     int           `json:"min_requests"`
    MaxRequests     int           `json:"max_requests"`
    RequestSlots    []RequestSlot `json:"request_slots"`
}

type PairBindingRef struct {
    PairSpecID      uuid.UUID
    PairSpecHash    string
    BaselineSideID uuid.UUID
    CandidateSideID uuid.UUID
    BindingHash     string
}
```

使用仓库依赖中的 RFC 8785 实现或引入单一受维护依赖，并添加 golden bytes 测试。hash 输入只包含规格规定字段，Prompt 和工具参数正文在类型层面不可表达。

- [ ] **Step 4: 原子接入 Run 创建**

Run create 事务按 manifest、pair、baseline side、candidate side、binding、samples、assignments 的固定顺序写入。SideSpec 的 sample FK 依赖 deferred constraint。创建完成前 assignment 不可被 claim。

- [ ] **Step 5: 验证历史与新 Run 的读模型**

新 Run 返回 IDs 与 hashes。历史 Run 缺任一对象时返回 `contract_status=legacy-unbound`。外部 API 不返回 canonical bytes。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(CanonicalRequest|RequestManifest|PairBinding|CreateRunRolls|EvaluationRun)' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_experiment_contract.go backend/internal/service/evaluation_experiment_contract_test.go backend/internal/repository/evaluation_experiment_contract_repo.go backend/internal/repository/evaluation_experiment_contract_repo_test.go backend/internal/service/evaluation.go backend/internal/repository/evaluation_repo.go backend/internal/repository/evaluation_repo_integration_test.go
git commit -m "feat(radar): freeze evaluation experiment contracts"
```

### Task 4: 完成 Worker 注册、轮换、pause claims、drain 与 disable

**Files:**
- Modify: `backend/internal/service/evaluation_governance.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_test.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/server/routes/radar_governance_routes_test.go`
- Modify: `backend/internal/server/middleware/audit_log_test.go`
- Test: `backend/internal/repository/evaluation_governance_repo_test.go`

**Consumes:** Task 1 Worker lifecycle schema，Task 2 writer transaction。

**Produces:** Worker 幂等注册、token rotation、claim mode、drain 完成条件、disable 鉴权失效和不可变事件。

- [ ] **Step 1: 写 Repository 与 API 失败测试**

```go
func TestRegisterRadarWorkerIsIdentityIdempotent(t *testing.T)
func TestRotateRadarWorkerTokenInvalidatesOldBearer(t *testing.T)
func TestPauseWorkerClaimsKeepsInflightLeaseValid(t *testing.T)
func TestDrainWorkerCompletesAfterActiveLeaseCountZero(t *testing.T)
func TestDisableWorkerRejectsHeartbeatImmediately(t *testing.T)
func TestWorkerAuditRedactsPlaintextToken(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/handler/admin ./internal/server/routes -run 'Test.*Worker' -count=1`

Expected: FAIL，缺少控制动作或审计脱敏。

- [ ] **Step 3: 实现 Worker identity 与状态语义**

注册 identity 固定 name、kind、region、image digest、排序去重后的 immutable capabilities。相同 identity 与 token hash 幂等返回；同名 identity 或 token 不同返回 409。轮换原子替换 hash，保留 Worker ID 和 lease，旧 bearer 立即失效。

```go
type WorkerClaimMode string
const (
    WorkerClaimsOpen WorkerClaimMode = "open"
    WorkerClaimsPaused WorkerClaimMode = "paused"
    WorkerClaimsDraining WorkerClaimMode = "draining"
)
```

disable 使 bearer 鉴权立即失败。pause 和 drain 保留现有 lease。drain 查询 active lease 总数并在归零时追加 `drain_completed` event。

- [ ] **Step 4: 暴露 RBAC API 并验证脱敏**

实现 `POST /api/v1/admin/radar/workers`、`rotate-token`、`pause-claims`、`resume-claims`、`drain`、`disable`。所有动作消费 `PermissionWorkerManage` 与 64 位 idempotency key。响应只返回 12 位 fingerprint。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository ./internal/handler/admin ./internal/server/routes ./internal/server/middleware -run 'Test.*Worker' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/service/evaluation_governance.go backend/internal/repository/evaluation_governance_repo.go backend/internal/repository/evaluation_governance_repo_test.go backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go backend/internal/server/routes/admin.go backend/internal/server/routes/radar_governance_routes_test.go backend/internal/server/middleware/audit_log_test.go
git commit -m "feat(radar): control trusted workers"
```

### Task 5: 实现 Run pause、resume、cancel 与 fence API

**Files:**
- Create: `backend/internal/repository/evaluation_run_control.go`
- Create: `backend/internal/repository/evaluation_run_control_test.go`
- Modify: `backend/internal/service/evaluation_management.go`
- Modify: `backend/internal/service/evaluation_rbac.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Test: `backend/internal/repository/evaluation_run_control_test.go`

**Consumes:** Task 1 状态、epoch 与 event 约束，Task 2 writer transaction。

**Produces:** 四个幂等 Run 控制动作、`PermissionRunControl`、受影响工作计数、replacement IDs 与 Event ID。

- [ ] **Step 1: 写状态矩阵失败测试**

覆盖每个非终态的 pause、resume、cancel、fence，终态 409，重复 idempotency key 返回原响应，不同 payload 重用 key 返回冲突。

```go
func TestPauseRunPreservesInflightLeaseAndEpoch(t *testing.T)
func TestResumeRunRecomputesP0Readiness(t *testing.T)
func TestCancelRunIncrementsEpochAndCancelsOpenWork(t *testing.T)
func TestFenceRunReplacesRetryableRunnerWork(t *testing.T)
func TestRunControlIdempotencyReturnsOriginalResult(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/handler/admin -run 'Test(Pause|Resume|Cancel|Fence|RunControl)' -count=1`

Expected: FAIL，控制 Repository 或路由缺失。

- [ ] **Step 3: 实现 Run 优先事务**

```go
type RunControlResult struct {
    RunID             uuid.UUID
    FromStatus        string
    ToStatus          string
    PreviousEpoch     int64
    CurrentEpoch      int64
    AffectedWorkCount int
    ReplacementIDs    []uuid.UUID
    EventID           uuid.UUID
}
```

pause 保存 `paused_from_status` 且不改变 epoch。resume 重新计算 failure 与 P0 readiness。cancel 递增 epoch，取消非终态工作并写 evidence terminalization outbox。fence 保持业务状态并递增 epoch，按 retry policy 创建 replacement，无法重试时进入 failed。

- [ ] **Step 4: 实现 API、权限与审计**

四个端点路径为 `/api/v1/admin/radar/runs/:id/{pause,resume,cancel,fence}`。请求含 64 位 idempotency key 与有限 reason code。actor 只从认证上下文读取。审计只保存 scope、reason、epoch 与计数。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository ./internal/handler/admin ./internal/server/routes -run 'Test(Pause|Resume|Cancel|Fence|RunControl)' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/repository/evaluation_run_control.go backend/internal/repository/evaluation_run_control_test.go backend/internal/service/evaluation_management.go backend/internal/service/evaluation_rbac.go backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go backend/internal/server/routes/admin.go
git commit -m "feat(radar): add fenced run controls"
```

### Task 6: 把 Assignment、Grading 与 Analysis 统一接入 lease epoch fencing

**Files:**
- Modify: `backend/internal/repository/evaluation_repo.go`
- Modify: `backend/internal/repository/evaluation_repo_integration_test.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/repository/evaluation_grading_repo_integration_test.go`
- Modify: `backend/internal/service/evaluation_repository.go`
- Modify: `backend/internal/handler/internal/radar_grader_handler.go`
- Modify: `backend/internal/handler/internal/radar_grader_handler_test.go`
- Modify: `radar-worker/src/sub2api_radar/models.py`
- Modify: `radar-worker/src/sub2api_radar/control_plane.py`
- Modify: `radar-worker/tests/test_control_plane.py`
- Test: `backend/internal/repository/evaluation_repo_integration_test.go`

**Consumes:** Task 4 Worker active/claim mode，Task 5 Run epoch。

**Produces:** claim 复制 epoch，heartbeat/complete/fail 的统一 `lease_fenced` 合同，Worker 对 epoch 的透明回传。

- [ ] **Step 1: 写 fencing 失败测试**

覆盖 bearer、Worker status、claim mode、lease token、expiry、worker kind、job status、work origin 和 epoch。对外统一返回 `lease_fenced`，内部指标保留有限 reason label。

```go
func TestAssignmentCompleteRejectsOldRunEpoch(t *testing.T)
func TestGradingHeartbeatRejectsDisabledWorker(t *testing.T)
func TestAnalysisClaimSkipsPausedRun(t *testing.T)
func TestBudgetPausedClaimReturnsOnlyP0(t *testing.T)
func TestFirstClaimSetsStartedAtExactlyOnce(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal -run 'Test.*(Epoch|Fenced|Paused|FirstClaim)' -count=1 && cd ../radar-worker && uv run pytest tests/test_control_plane.py -q`

Expected: Go 或 Python 至少一项失败，响应缺少 epoch 或 Repository 未验证 epoch。

- [ ] **Step 3: 统一 claim 和提交锁序**

claim 先锁 Run，再用 `FOR UPDATE SKIP LOCKED` 选择 Assignment 或 Job，复制 `control_epoch`、Worker image digest 和 hashed lease token。initial work 只允许 Run 处于该阶段可提交状态。pause 阻止新 claim，同时允许已有 lease 完成。

- [ ] **Step 4: 更新 Worker 协议模型**

```python
class Lease(BaseModel):
    lease_token: str
    lease_epoch: int
    worker_image_digest: str
    expires_at: datetime
```

Worker 在 heartbeat、complete、fail 原样提交 `lease_epoch`。收到 `lease_fenced` 后停止该工作，不自动用旧 payload 重试。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal -run 'Test.*(Epoch|Fenced|Paused|FirstClaim)' -count=1 && cd ../radar-worker && uv run pytest tests/test_control_plane.py -q`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/repository/evaluation_repo.go backend/internal/repository/evaluation_repo_integration_test.go backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_grading_repo_integration_test.go backend/internal/service/evaluation_repository.go backend/internal/handler/internal/radar_grader_handler.go backend/internal/handler/internal/radar_grader_handler_test.go radar-worker/src/sub2api_radar/models.py radar-worker/src/sub2api_radar/control_plane.py radar-worker/tests/test_control_plane.py
git commit -m "feat(radar): fence initial work leases"
```

### Task 7: 实现 failure-first Run Reconciler 与 exact P0 drain

**Files:**
- Create: `backend/internal/repository/evaluation_run_reconciler.go`
- Create: `backend/internal/repository/evaluation_run_reconciler_test.go`
- Modify: `backend/internal/repository/evaluation_repo.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/repository/evaluation_repo_integration_test.go`
- Test: `backend/internal/repository/evaluation_run_reconciler_test.go`

**Consumes:** Task 3 完整 Pair Binding，Task 6 fenced initial work 与完整 ScoreRef。

**Produces:** 唯一 Run terminal reducer、exact P0 drain、单次 transition event、失败时工作处置和 terminalization outbox。

- [ ] **Step 1: 写归约状态表失败测试**

至少覆盖 normal first claim、exact budget mixed priority、P0-only、non-P0-only、paused readiness、零分成功、replacement 存活、不可恢复失败优先、完整 Aggregate 后完成、重复 reconcile 幂等。

```go
func TestReconcileFailsBeforeConsideringPendingWork(t *testing.T)
func TestReconcileIgnoresSupersededAssignmentFailure(t *testing.T)
func TestExactP0DrainRequiresSampleAssignmentAndScoreHead(t *testing.T)
func TestPausedRunRecordsReadinessWithoutTransition(t *testing.T)
func TestReconcileCompletesOnlyWithCurrentAggregateCoverage(t *testing.T)
func TestReconcileTerminalRetryDoesNotDuplicateEvent(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run 'TestReconcile|TestExactP0' -count=1`

Expected: FAIL，Reconciler 尚未定义或仍使用旧完成判断。

- [ ] **Step 3: 实现 pure decision 与事务 wrapper**

```go
type RunReconcileFacts struct {
    Status               string
    Started              bool
    UnrecoverableFailure *FailureCause
    P0Expected           int
    P0Successful         int
    P0Active             int
    PendingWork          int
    CurrentCoverageOK    bool
}

func decideRunTransition(f RunReconcileFacts) RunTransition
func (r *Repository) ReconcileEvaluationRun(ctx context.Context, runID uuid.UUID) (RunRecord, error)
```

decision 顺序固定为 terminal no-op、paused 保留、不可恢复失败、exact P0 readiness、pending work、current Aggregate coverage、completed。零分属于成功结果。

- [ ] **Step 4: 接入所有 initial work 终点**

Assignment、Grading、Analysis 的 complete/fail 与 lease reaper 在同一业务事务末尾调用 Reconciler。失败转换递增 epoch、取消余下非终态工作、清 lease、追加唯一 event 和 evidence terminalization outbox。

- [ ] **Step 5: 运行并发与幂等测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'TestReconcile|TestExactP0|TestEvaluationRunLifecycleConcurrency' -count=20`

Expected: PASS，重复运行无 event 重复和死锁。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/repository/evaluation_run_reconciler.go backend/internal/repository/evaluation_run_reconciler_test.go backend/internal/repository/evaluation_repo.go backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_repo_integration_test.go
git commit -m "feat(radar): reconcile trusted run lifecycle"
```

### Task 8: 编写 migration 197 分阶段 cutover 验收

**Files:**
- Create: `backend/scripts/radar_migration_197_cutover.sh`
- Create: `backend/scripts/radar_migration_197_acceptance.sh`
- Create: `backend/internal/integration/radar_writer_cutover_e2e_test.go`
- Modify: `radar-worker/deploy/docker-compose.staging.yml`
- Create: `docs/radar-staging-runbook.md`
- Test: `backend/internal/integration/radar_writer_cutover_e2e_test.go`

**Consumes:** Tasks 1 至 7 的 schema、writer identity、lease 和状态机。

**Produces:** audit clean window、drain、closed、enforce、reopen 的可重复操作与机器可判定证据。

- [ ] **Step 1: 写 cutover 失败验收**

测试先启动旧 protocol writer 与一个 active lease，断言 drain 无法完成；停止旧 writer 并完成 lease 后才能 closed；enforce 后旧 writer 被拒绝，兼容 writer 可恢复写入。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/integration -run TestRadarWriterCutover197 -count=1`

Expected: FAIL，cutover helper 或脚本不存在。

- [ ] **Step 3: 实现脚本的只前进状态检查**

脚本每一步读取数据库真实 session、lease、transaction 与 advisory lock 计数。任一非零值退出 1 并保持 closed。脚本不得通过删除 session 伪造归零。

```bash
./scripts/radar_migration_197_cutover.sh audit
./scripts/radar_migration_197_cutover.sh drain
./scripts/radar_migration_197_cutover.sh close
./scripts/radar_migration_197_cutover.sh enforce
./scripts/radar_migration_197_cutover.sh reopen
./scripts/radar_migration_197_acceptance.sh
```

- [ ] **Step 4: 补充 staging runbook**

记录进入条件、每步查询、停止条件、可回滚镜像协议版本、evaluation context 503 行为、客户生产流量隔离、失败时保持 closed 的操作。

- [ ] **Step 5: 运行验收并确认 GREEN**

Run: `cd backend && go test ./internal/integration -run TestRadarWriterCutover197 -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/scripts/radar_migration_197_cutover.sh backend/scripts/radar_migration_197_acceptance.sh backend/internal/integration/radar_writer_cutover_e2e_test.go radar-worker/deploy/docker-compose.staging.yml docs/radar-staging-runbook.md
git commit -m "test(radar): prove migration 197 cutover"
```

### Task 9: 执行生命周期全量验证并保存证据

**Files:**
- Create: `docs/superpowers/evidence/radar-g1-lifecycle-verification.md`
- Modify: `docs/radar-staging-runbook.md`
- Test: `backend/internal/integration/radar_p0_e2e_test.go`

**Consumes:** Tasks 1 至 8 全部产物。

**Produces:** 可复核的测试输出摘要、schema hash、镜像 digest、协议版本和残余风险记录。

- [ ] **Step 1: 运行证据完整性检查并确认 RED**

Run: `test -f docs/superpowers/evidence/radar-g1-lifecycle-verification.md`

Expected: FAIL，验证证据尚未创建。

- [ ] **Step 2: 运行静态与单元测试**

Run: `cd backend && gofmt -w internal/repository/evaluation_* internal/service/evaluation_* internal/handler/admin/evaluation_governance_handler.go internal/handler/internal/radar_grader_handler.go && go test ./internal/service ./internal/repository ./internal/handler/admin ./internal/handler/internal ./internal/server/routes -count=1`

Expected: PASS。

- [ ] **Step 3: 运行 Worker 测试**

Run: `cd radar-worker && uv run ruff check . && uv run mypy src && uv run pytest -q`

Expected: PASS。

- [ ] **Step 4: 运行 migration 与 P0 集成测试**

Run: `cd backend && go test ./internal/repository ./internal/integration -run 'TestMigration197|TestRadarWriterCutover197|TestRadarP0' -count=1`

Expected: PASS。

- [ ] **Step 5: 检查敏感数据与旧 current 写路径**

Run: `rg -n 'token_hash|plaintext_token|UPDATE evaluation_scores.*is_current|SET is_current' backend/internal radar-worker/src`

Expected: token hash 只出现在授权持久化路径，零个生产路径继续更新 Score `is_current`，测试 fixture 命中需逐条说明。

- [ ] **Step 6: 写验证证据**

证据文件记录执行时间、commit、migration checksum、writer protocol version、命令结果、失败重试、已知限制。不得写 token、Prompt、Completion 或 canonical evidence payload。

- [ ] **Step 7: 运行证据完整性检查并确认 GREEN**

Run: `rg -n 'commit|migration checksum|writer protocol version|known limitations' docs/superpowers/evidence/radar-g1-lifecycle-verification.md`

Expected: PASS，四类必要证据全部存在且无敏感 payload。

- [ ] **Step 8: 提交本任务**

```bash
git add docs/superpowers/evidence/radar-g1-lifecycle-verification.md docs/radar-staging-runbook.md
git commit -m "docs(radar): record lifecycle verification"
```

## 完成标准

- migration 197 可以从 migrations 196 的真实 schema 升级，完整 ScoreRef 回填失败时会中止。
- 新 Run 在开放 claim 前已经冻结 Request Manifest、PairSpec、两条 SideSpec 和 Pair Binding。
- Worker pause、drain、disable、rotation 的 bearer 与 lease 语义全部经过测试。
- pause 不改变 epoch，cancel、failure 和 fence 按合同递增 epoch，旧 lease 的所有写路径统一被拒绝。
- exact P0 drain 无死锁，failure-first 归约优先于 pending 判断，终态 event 恰好一条。
- writer guard 完成 audit、clean window、drain、closed、enforce、reopen 的全链路验收。
- 后续 Evidence Revision Pipeline 可以消费 frozen binding、ScoreRef、lease epoch 与 writer protocol，无需重写本计划的表或状态语义。
