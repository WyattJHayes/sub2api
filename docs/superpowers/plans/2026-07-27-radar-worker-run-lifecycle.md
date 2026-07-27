# Radar Worker 与 Run 生命周期实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为 Radar 增加可审计的 Worker 注册与凭据轮换，并让每个评测 Run 从 `pending` 自动、单调地收敛到可信终态。

**Architecture:** 管理面负责注册、停用和轮换 Worker，私有 Worker 面继续只通过 bearer token hash 鉴权。Repository 内的单一 run reconciler 在同一数据库事务中读取 sample、assignment、grading job 与 analysis job 状态，完成单调状态迁移并追加事件。

**Tech Stack:** Go 1.24、Gin、PostgreSQL 18、`database/sql`、`sqlmock`、Python 3.12、httpx、pytest、Docker Compose。

## Global Constraints

- Worker 明文 token 至少 32 个字符，只允许存在于短生命周期请求缓冲、请求解码和 SHA256 计算过程。审计请求体必须将 token 替换为 `***`，响应、日志、审计持久化 payload 和数据库均不得包含明文。
- `evaluation_workers.token_hash` 保持唯一，API 只返回前 12 个十六进制字符组成的 `token_fingerprint`。
- 同名注册只有在 kind、token hash 和不可变身份一致时幂等成功；换 token 必须调用独立轮换端点。
- Worker kind 只允许 `runner`、`grader`、`statistics`。
- Run 状态只能沿 `pending|budget_paused -> running -> completed|failed|cancelled` 前进。成功领取一个允许执行的 assignment 时，`pending` 或 `budget_paused` 进入 `running`，终态不可重新打开。
- 预留成本恰好等于预算且同时含 P0 与低优先级样本时，Run 先保持 `budget_paused` 并只领取 P0。全部 P0 样本成功完成后自动进入 `running`，随后允许领取已预留成本覆盖的 P1/P2。只有 P0 或没有 P0 的 Run 不进入该暂停流程。
- 失败归约只读取 sample、grading job 和 analysis job 的不可恢复终态。已由新 attempt 接管的历史 assignment 失败不能使 Run 失败。
- 每次 Run 状态变化都在同一事务写入 `evaluation_run_events`，重复 reconciler 调用不能重复写同一状态事件。
- 任何 migration、API 或状态机变更都先写失败测试并观察预期失败。
- 不修改客户流量、客户 API Key、现有 score、aggregate 或 artifact 内容。

---

### Task 1: 增加 Worker 注册元数据和追加式事件

**Files:**
- Create: `backend/migrations/197_add_radar_worker_lifecycle.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Test: `backend/internal/repository/migrations_schema_integration_test.go`

**Interfaces:**
- Produces: `evaluation_workers.region`、`image_digest`、`registered_by`、`registered_at`、`disabled_at`。
- Produces: `evaluation_worker_events(id, worker_id, event_type, actor_id, token_fingerprint, payload, created_at)`。

- [ ] **Step 1: 写迁移失败测试**

在 schema 集成测试中断言新增列、`evaluation_worker_events` 表、事件类型约束和 `worker_id, created_at` 索引存在。测试还要断言 Worker 事件表没有明文 token 列。

```go
requireColumns(t, db, "evaluation_workers", []string{
    "region", "image_digest", "registered_by", "registered_at", "disabled_at",
})
requireColumns(t, db, "evaluation_worker_events", []string{
    "id", "worker_id", "event_type", "actor_id", "token_fingerprint", "payload", "created_at",
})
requireNoColumn(t, db, "evaluation_worker_events", "token")
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestMigrationsSchema -count=1`

Expected: FAIL，提示 `evaluation_worker_events` 或 `region` 不存在。

- [ ] **Step 3: 编写 migration 197**

迁移必须：

```sql
ALTER TABLE evaluation_workers
    ADD COLUMN IF NOT EXISTS region VARCHAR(64) NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS image_digest VARCHAR(128) NOT NULL DEFAULT 'unknown',
    ADD COLUMN IF NOT EXISTS registered_by BIGINT REFERENCES users(id),
    ADD COLUMN IF NOT EXISTS registered_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS evaluation_worker_events (
    id UUID PRIMARY KEY,
    worker_id UUID NOT NULL REFERENCES evaluation_workers(id),
    event_type VARCHAR(32) NOT NULL CHECK (event_type IN (
        'registered', 'metadata_updated', 'token_rotated', 'enabled', 'disabled'
    )),
    actor_id BIGINT REFERENCES users(id),
    token_fingerprint CHAR(12),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_evaluation_worker_events_worker_created
    ON evaluation_worker_events (worker_id, created_at DESC);
```

- [ ] **Step 4: 运行迁移测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run TestMigrationsSchema -count=1`

Expected: PASS。

- [ ] **Step 5: 提交本任务**

```bash
git add backend/migrations/197_add_radar_worker_lifecycle.sql backend/internal/repository/migrations_schema_integration_test.go
git commit -m "feat(radar): add worker lifecycle schema"
```

### Task 2: 实现幂等注册、轮换和状态控制 Repository

**Files:**
- Modify: `backend/internal/service/evaluation_governance.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_test.go`
- Test: `backend/internal/repository/evaluation_governance_repo_test.go`

**Interfaces:**
- Produces: `RegisterRadarWorker(ctx context.Context, input RadarWorkerRegistrationInput) (*RadarWorkerRecord, error)`。
- Produces: `RotateRadarWorkerToken(ctx context.Context, workerID uuid.UUID, token string, actorID int64) (*RadarWorkerRecord, error)`。
- Produces: `SetRadarWorkerEnabled(ctx context.Context, workerID uuid.UUID, enabled bool, actorID int64) (*RadarWorkerRecord, error)`。
- Produces: `ErrRadarWorkerConflict`、`ErrRadarWorkerTokenConflict`。

- [ ] **Step 1: 写 Repository 失败测试**

覆盖以下行为：

```go
func TestRegisterRadarWorkerCreatesWorkerAndRedactedEvent(t *testing.T)
func TestRegisterRadarWorkerRetryReturnsExistingWorker(t *testing.T)
func TestRegisterRadarWorkerRejectsSameNameWithDifferentKindOrToken(t *testing.T)
func TestRotateRadarWorkerTokenWritesHashAndFingerprintOnly(t *testing.T)
func TestSetRadarWorkerEnabledWritesAppendOnlyEvent(t *testing.T)
```

测试 token 固定为 `0123456789abcdef0123456789abcdef`。断言 SQL 参数只包含其 SHA256 与前 12 位 fingerprint，任何参数都不等于明文 token。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run 'Test(Register|Rotate|Set)RadarWorker' -count=1`

Expected: FAIL，提示接口或方法尚未定义。

- [ ] **Step 3: 增加服务类型**

```go
type RadarWorkerRegistrationInput struct {
    Name           string
    WorkerKind     string
    Token          string
    Capabilities   []string
    MaxConcurrency int
    Region         string
    ImageDigest    string
    ActorID        int64
}

type RadarWorkerRecord struct {
    ID               uuid.UUID `json:"id"`
    Name             string    `json:"name"`
    WorkerKind       string    `json:"worker_kind"`
    Status           string    `json:"status"`
    Capabilities     []string  `json:"capabilities"`
    MaxConcurrency   int       `json:"max_concurrency"`
    Region           string    `json:"region"`
    ImageDigest      string    `json:"image_digest"`
    TokenFingerprint string    `json:"token_fingerprint"`
    RegisteredAt     time.Time `json:"registered_at"`
}
```

输入规范化规则为名称、region、digest 和 capability 去除首尾空白，capability 去重排序。`image_digest` 接受 `sha256:` 加 64 位小写十六进制，staging 本地构建可使用 `binary-sha256:` 加 64 位摘要。

- [ ] **Step 4: 实现事务和冲突语义**

注册先按 name `FOR UPDATE` 查询。没有记录时插入 Worker 与 `registered` 事件；存在记录时核对 kind 和 token hash，完全一致则返回已有记录，元数据变化时更新安全元数据并写 `metadata_updated`。不同 kind 或 token 返回 409 对应的领域错误。

轮换端点锁定 Worker，拒绝与其他 Worker 的 hash 冲突，更新 hash 后写 `token_rotated` 事件。状态端点只在状态真正变化时更新并写 `enabled` 或 `disabled` 事件。

- [ ] **Step 5: 运行 Repository 测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'Test(Register|Rotate|Set)RadarWorker' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/service/evaluation_governance.go backend/internal/repository/evaluation_governance_repo.go backend/internal/repository/evaluation_governance_repo_test.go
git commit -m "feat(radar): manage worker registrations"
```

### Task 3: 暴露受 RBAC 保护的 Worker 管理 API

**Files:**
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/server/routes/radar_governance_routes_test.go`
- Modify: `backend/internal/server/middleware/audit_log_test.go`
- Test: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Test: `backend/internal/server/routes/radar_governance_routes_test.go`

**Interfaces:**
- Produces: `POST /api/v1/admin/radar/workers`。
- Produces: `POST /api/v1/admin/radar/workers/:id/rotate-token`。
- Produces: `POST /api/v1/admin/radar/workers/:id/enable` 和 `/disable`。
- Consumes: `PermissionWorkerManage`。

- [ ] **Step 1: 写路由和 Handler 失败测试**

注册请求使用完整固定值：

```json
{
  "name":"radar-runner-staging",
  "worker_kind":"runner",
  "token":"0123456789abcdef0123456789abcdef",
  "capabilities":["openai_chat"],
  "max_concurrency":1,
  "region":"staging",
  "image_digest":"binary-sha256:bc039a2a4cb048f42f0da0ff8baa68ad625ae850e8b3228f98fae06191f17b61"
}
```

断言响应不含 `token` 或 `token_hash`，权限不足返回 403，同名冲突返回 409，未知 UUID 返回 404。

注册和轮换请求还要经过真实 Admin audit middleware。捕获的审计记录不得包含固定明文 token，请求体中的 `token` 必须为 `***`，响应和审计 extra 同样不得出现明文。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/handler/admin ./internal/server/routes -run 'TestRadar.*Worker' -count=1`

Expected: FAIL，路由返回 404 或 Handler 方法缺失。

- [ ] **Step 3: 实现请求验证与错误映射**

Gin binding 约束：token `min=32`，max concurrency `gte=1,lte=1000`，capabilities 至少一个，region 必填，digest 必填。所有动作先调用 `require(..., PermissionWorkerManage)`，actor ID 只来自认证上下文。

- [ ] **Step 4: 注册路由并运行测试**

Run: `cd backend && go test ./internal/handler/admin ./internal/server/routes -run 'TestRadar.*Worker' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交本任务**

```bash
git add backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go backend/internal/server/routes/admin.go backend/internal/server/routes/radar_governance_routes_test.go backend/internal/server/middleware/audit_log_test.go
git commit -m "feat(radar): expose worker management API"
```

### Task 4: 建立 Run 单调状态 reconciler

**Files:**
- Create: `backend/internal/repository/evaluation_run_lifecycle.go`
- Create: `backend/internal/repository/evaluation_run_lifecycle_test.go`
- Modify: `backend/internal/repository/evaluation_repo.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/repository/evaluation_repo_integration_test.go`
- Test: `backend/internal/repository/evaluation_run_lifecycle_test.go`
- Test: `backend/internal/repository/evaluation_repo_integration_test.go`

**Interfaces:**
- Produces: `markEvaluationRunStarted(ctx context.Context, tx *sql.Tx, runID uuid.UUID) error`。
- Produces: `reconcileEvaluationRun(ctx context.Context, tx *sql.Tx, runID uuid.UUID) (service.RunStatus, error)`。

- [ ] **Step 1: 写状态机失败测试**

覆盖九条路径：首次成功 claim 将 pending 变为 running；budget_paused 首次 P0 claim 只设置 started_at 并保持暂停；全部 P0 成功后进入 running 并允许 P1/P2 claim；只有 P0 或没有 P0 的 exact-budget Run 不会死锁；尚有 pending sample 时保持当前非终态；所有 sample、grading job 和 analysis job 完成且 aggregate 单元完整时变为 completed；任一不可恢复 terminal failure 即使仍有 pending 工作也使 run 变为 failed；存在失败的历史 assignment 但 replacement attempt 仍活跃时保持 running；终态重复 reconcile 不变化且不重复事件。

```go
func TestEvaluationRunLifecycleStartsOnFirstLease(t *testing.T)
func TestEvaluationRunLifecycleDrainsP0BeforeResumingExactBudgetRun(t *testing.T)
func TestEvaluationRunLifecycleExactBudgetRunWithoutMixedPrioritiesDoesNotDeadlock(t *testing.T)
func TestEvaluationRunLifecycleWaitsForAllJobs(t *testing.T)
func TestEvaluationRunLifecycleCompletesExactlyOnce(t *testing.T)
func TestEvaluationRunLifecycleRequiresEveryAggregateCellAndGlobalSnapshot(t *testing.T)
func TestEvaluationRunLifecycleFailsOnTerminalFailure(t *testing.T)
func TestEvaluationRunLifecycleIgnoresRetriedAssignmentFailure(t *testing.T)
func TestEvaluationRunLifecycleNeverReopensTerminalRun(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestEvaluationRunLifecycle -count=1`

Expected: FAIL，reconciler 尚不存在。

- [ ] **Step 3: 实现状态归约规则**

同一事务锁定 `evaluation_runs`。`markEvaluationRunStarted` 对 `pending` 设置 `running`，对 `budget_paused` 只设置 `started_at`，并写一次 `run_started` 事件。只有实际成功领取 assignment 才能触发该变化。

创建 Run 时，只有预留成本等于预算且数据集同时包含 P0 与 P1/P2 才使用 `budget_paused`。claim 查询在该状态只允许 P0。最后一个 P0 sample 成功完成时，reconciler 写 `run_resumed` 并切换到 `running`；`running` 在 `reserved_cost <= budget_limit` 时允许领取其余已预留 assignment。

`reconcileEvaluationRun` 的顺序固定为：

1. 当前状态是 completed、failed 或 cancelled 时直接返回。
2. 当前状态是 paused 时保持 paused。
3. 所有 sample 为 cancelled 时进入 cancelled。
4. 任一 sample、grading job 或 analysis job 为不可恢复失败终态时立即进入 failed。
5. `budget_paused` 且仍有 P0 sample 非终态时保持暂停；全部 P0 成功时写 `run_resumed` 并进入 running。
6. 任一 sample、当前有效 assignment、grading job 或 analysis job 仍为非终态时保持当前状态。
7. 每个预期 `(capability_domain, canonical_model_route)` 都有 immutable aggregate；预期单元多于一个时还必须有 `global/global` aggregate。全部工作与 aggregate 覆盖完成后进入 completed。
8. 状态改变时设置 `finished_at` 并写一个 `run_completed`、`run_failed` 或 `run_cancelled` 事件。

“当前有效 assignment”指每个 sample 的最高 attempt。较早 attempt 的 `infra_failed`、`upstream_failed` 或 fenced 结果只作为历史证据保留。

- [ ] **Step 4: 在所有事务边界调用 reconciler**

首次 assignment claim 后调用 `markEvaluationRunStarted`。assignment fail、score submission、grading fail、analysis fail 和 analysis complete 后调用 `reconcileEvaluationRun`。score submission 完成最后一个 P0 sample 时必须立即恢复 exact-budget Run。score 为 0 仍属于成功完成，不得触发 failed。

- [ ] **Step 5: 运行定向与集成测试**

Run: `cd backend && go test ./internal/repository -run 'TestEvaluationRunLifecycle|TestEvaluationRepository' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/repository/evaluation_run_lifecycle.go backend/internal/repository/evaluation_run_lifecycle_test.go backend/internal/repository/evaluation_repo.go backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_repo_integration_test.go
git commit -m "feat(radar): reconcile evaluation run lifecycle"
```

### Task 5: 增加 Analysis Job 失败闭环

**Files:**
- Modify: `backend/internal/service/evaluation_grading.go`
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/repository/evaluation_grading_repo_integration_test.go`
- Modify: `backend/internal/handler/internal/radar_grader_handler.go`
- Modify: `backend/internal/handler/internal/radar_grader_handler_test.go`
- Modify: `backend/internal/server/routes/radar_worker.go`
- Modify: `backend/internal/server/routes/radar_worker_test.go`
- Modify: `radar-worker/src/sub2api_radar/control_plane.py`
- Modify: `radar-worker/src/sub2api_radar/statistics/service.py`
- Modify: `radar-worker/tests/test_control_plane.py`
- Modify: `radar-worker/tests/test_worker_loops.py`

**Interfaces:**
- Produces: `FailAnalysisJob(ctx context.Context, jobID uuid.UUID, leaseToken, failureCode string) error`。
- Produces: `POST /internal/radar/v1/analysis-jobs/:id/fail`。
- Produces: `ControlPlaneClient.fail_analysis(...)`。

- [ ] **Step 1: 写 Go 与 Python 失败测试**

断言有效 lease 可以把 analysis job 置为 failed、清空 lease 字段、写 failure code 并使 run 收敛为 failed。过期或错误 token 返回 fenced。Python 统计循环遇到分析异常时调用 fail endpoint，fenced 只记录告警。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal ./internal/server/routes -run 'Test.*Analysis.*Fail' -count=1`

Run: `cd radar-worker && .venv/bin/pytest tests/test_control_plane.py tests/test_worker_loops.py -k analysis -q`

Expected: 两组测试都 FAIL，失败 API 尚不存在。

- [ ] **Step 3: 实现 fenced 失败事务和路由**

Repository 只允许 `status='leased'`、token hash 匹配且 lease 未过期的 job 失败。更新后在同一事务调用 run reconciler。HTTP 缺失 token 返回 401，fenced 返回 409，非法 failure code 返回 400。

- [ ] **Step 4: 实现 Worker 客户端上报**

统计循环捕获普通异常后调用：

```python
await self.client.fail_analysis(lease.id, lease.lease_token, "analysis_failed")
```

日志只含 lease UUID 和有限错误码，不含聚合 payload 或 token。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal ./internal/server/routes -run 'Test.*Analysis.*Fail' -count=1`

Run: `cd radar-worker && .venv/bin/pytest tests/test_control_plane.py tests/test_worker_loops.py -k analysis -q`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/service/evaluation_grading.go backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_grading_repo_integration_test.go backend/internal/handler/internal/radar_grader_handler.go backend/internal/handler/internal/radar_grader_handler_test.go backend/internal/server/routes/radar_worker.go backend/internal/server/routes/radar_worker_test.go radar-worker/src/sub2api_radar/control_plane.py radar-worker/src/sub2api_radar/statistics/service.py radar-worker/tests/test_control_plane.py radar-worker/tests/test_worker_loops.py
git commit -m "feat(radar): close analysis failure lifecycle"
```

### Task 6: 规范化 Aggregate 生产并生成全局快照

**Files:**
- Modify: `backend/internal/repository/evaluation_grading_repo.go`
- Modify: `backend/internal/repository/evaluation_grading_repo_integration_test.go`
- Modify: `backend/internal/service/evaluation_grading.go`
- Modify: `radar-worker/deploy/docker-compose.staging.yml`
- Modify: `radar-worker/src/sub2api_radar/statistics/service.py`
- Modify: `radar-worker/tests/test_statistics_service.py`
- Modify: `radar-worker/tests/test_worker_loops.py`

**Interfaces:**
- Produces: canonical analysis cell `(run_id, capability_domain, canonical_model_route)`。
- Produces: reserved global cell `capability_domain='global'`、`model_route='global'`。
- Consumes: current score heads and complete expected pair coverage。

- [ ] **Step 1: 写完整配对与全局聚合失败测试**

覆盖以下行为：单侧 score 完成时不入队；同一 cell 的全部 baseline/candidate current score head 完整后只入队一次；analysis job 的 model route 去除 `baseline:` 和 `candidate:` 前缀；单 cell 完成后无需 global；多能力域或多模型 cell 全部完成后入队一个 global job；缺少任一 cell 时不得提前入队 global；global worker 输入包含所有 cell 的完整配对与 case weight；同一 sample 的第二版 score 只追加记录并推进 score head，旧 score 字节保持不变。

```go
func TestSubmitScoreEnqueuesCanonicalAnalysisOnlyAfterCompletePairs(t *testing.T)
func TestCompleteAnalysisJobEnqueuesGlobalOnlyAfterEveryCell(t *testing.T)
func TestLoadAnalysisInputsGlobalUsesAllExpectedPairs(t *testing.T)
func TestSubmitScoreVersioningNeverUpdatesPreviousScore(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run 'Test(SubmitScoreEnqueuesCanonical|CompleteAnalysisJobEnqueuesGlobal|LoadAnalysisInputsGlobal)' -count=1`

Run: `cd radar-worker && .venv/bin/pytest tests/test_statistics_service.py tests/test_worker_loops.py -k 'aggregate or global' -q`

Expected: FAIL，现有实现按单侧前缀路由过早入队，且没有 global job。

- [ ] **Step 3: 实现 canonical cell 入队条件**

canonical model route 固定为 sample route 去除一个 `baseline:` 或 `candidate:` 前缀后的值。提交 score 后，Repository 通过 `evaluation_score_heads` 计算该 cell 的预期 pair 数与当前完整 pair 数；只有二者相等且大于零时才插入 analysis job。job 使用 canonical route、固定 `window='run'` 和 Run 的 `created_at` 作为稳定 window start，数据库唯一键保证重试只产生一项。

新增 score 版本时只 INSERT 新记录并原子更新 `evaluation_score_heads`，不得 UPDATE 旧 score 的 `is_current` 或其他字段。所有新增 current score 查询都通过 score head join。

- [ ] **Step 4: 实现 global job 和输入**

完成 cell analysis 后，Repository 比较 Run 的预期 cell 集合与已有 immutable cell snapshot 集合。完全相等且 cell 数大于一时插入唯一 `global/global` analysis job。global job 同样使用 `window='run'` 和 Run 的 `created_at`。global 输入读取所有 cell 的 baseline/candidate current score head、原始 case weight 和稳定排序，不接受部分集合。statistics Worker 对该输入复用同一配对统计核心，输出显式全局 aggregate。Worker 的默认 analysis capabilities 必须包含 `global`。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'Test(SubmitScoreEnqueuesCanonical|CompleteAnalysisJobEnqueuesGlobal|LoadAnalysisInputsGlobal)' -count=1`

Run: `cd radar-worker && .venv/bin/pytest tests/test_statistics_service.py tests/test_worker_loops.py -k 'aggregate or global' -q`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/repository/evaluation_grading_repo.go backend/internal/repository/evaluation_grading_repo_integration_test.go backend/internal/service/evaluation_grading.go radar-worker/deploy/docker-compose.staging.yml radar-worker/src/sub2api_radar/statistics/service.py radar-worker/tests/test_statistics_service.py radar-worker/tests/test_worker_loops.py
git commit -m "feat(radar): build complete aggregate cells"
```

### Task 7: 更新 staging 注册与验收文档

**Files:**
- Modify: `docs/model-quality-radar-configuration.md`
- Modify: `docs/radar-production-runbook.md`
- Modify: `deploy/docker-compose.radar-staging.yml`
- Create: `deploy/radar-staging.env.example`

**Interfaces:**
- Consumes: Worker 管理 API。
- Produces: 不输出 token 的三 Worker 注册顺序和 rollback drain 查询。

- [ ] **Step 1: 为 Compose 渲染写失败检查**

确认三类 Worker 的 ID、kind、region、capability 与镜像摘要都能映射到注册 payload，且 `.env` 仍是唯一 token 来源。

Run: `install -m 600 deploy/.env.example /private/tmp/radar-compose-check.env`

Run: `docker compose --env-file /private/tmp/radar-compose-check.env -f deploy/docker-compose.radar-staging.yml config --quiet`

Expected before dedicated Radar example and metadata changes: FAIL，提示缺少 Radar 必填变量或缺少新字段映射。

- [ ] **Step 2: 更新文档和 Compose 元数据**

Runbook 规定先注册 runner、grader、statistics，再启动对应容器。注册响应只保存 UUID、kind、region、image digest 和 fingerprint。token 继续由 root-owned `0600` 环境文件提供。新增 `deploy/radar-staging.env.example`，只包含显然无效的占位值，不包含任何本地或远端凭据。注册 statistics Worker 时 capabilities 必须包含 `global`，`RADAR_ANALYSIS_CAPABILITIES` 同步包含 `global`。

- [ ] **Step 3: 验证 Compose 与文档合同**

Run: `install -m 600 deploy/radar-staging.env.example /private/tmp/radar-compose-check.env`

Run: `docker compose --env-file /private/tmp/radar-compose-check.env -f deploy/docker-compose.radar-staging.yml config --quiet`

Run: `git diff --check`

Expected: 两条命令退出 0。

- [ ] **Step 4: 提交本任务**

```bash
git add docs/model-quality-radar-configuration.md docs/radar-production-runbook.md deploy/docker-compose.radar-staging.yml deploy/radar-staging.env.example
git commit -m "docs(radar): document worker bootstrap lifecycle"
```

### Task 8: 完整验证

**Files:**
- Verify only.

- [ ] **Step 1: 运行 Go 定向包测试**

Run: `cd backend && go test ./internal/repository ./internal/handler/admin ./internal/handler/internal ./internal/server/routes -count=1`

Expected: PASS。

- [ ] **Step 2: 运行 Worker 质量门**

Run: `cd radar-worker && .venv/bin/pytest -q`

Run: `cd radar-worker && .venv/bin/ruff check src tests`

Run: `cd radar-worker && .venv/bin/mypy src`

Expected: pytest、Ruff、Mypy 全部退出 0。

- [ ] **Step 3: 运行静态差异检查**

Run: `git diff --check`

Expected: 退出 0。

- [ ] **Step 4: 保存执行证据**

在 SDD ledger 记录 migration 197 checksum、全部测试命令、测试数量、提交范围和仍未执行的远端验收。只有取得 staging runtime 证据后才能把本计划标为完成。
