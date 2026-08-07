# Radar 性能、可靠性、混沌与容灾实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付可复算的多租户推理压测、Reliability Snapshot、性能 Gate、受控故障实验和容灾验收闭环。

**Architecture:** Go 控制面保存不可变 Load Plan、快照、实验和恢复证据，Python 负载发生器通过专用评测身份执行分层流量，网关与账本提供独立计数。Reliability Publisher 在固定窗口内生成直方图与完整错误分母，推进 Head 并触发 Gate。Chaos Controller 只执行已批准目标，Recovery Verifier 用数据 hash 和 deterministic Run 验证恢复结果。

**Tech Stack:** Go 1.26.5、Gin、PostgreSQL 18、`database/sql`、Python 3.12、httpx、Pydantic、pytest、OpenTelemetry、Prometheus、Docker Compose。

## Global Constraints

- 先完成 migration 197、198、199 及对应可信执行计划，再应用 migration 200。
- Load Plan、Reliability Snapshot、Fault Experiment 和 Recovery Evidence 使用 RFC 8785 canonical JSON、SHA256 和追加写存储。
- `request_count` 包含成功、错误、超时和客户端失败，任何分类都不能从分母静默删除。
- 负载发生器只能使用专用评测租户、API Key、渠道、并发和预算。
- Prompt、Completion、隐藏推理、API Key、账号 ID、渠道 ID 和任意上游错误正文不能进入指标标签、告警或导出。
- 生产故障实验初始范围只允许单 Worker 进程终止或单 Worker 网络隔离。
- 每个任务先观察 RED，再实现最小闭环，随后观察 GREEN 并独立提交。

## File Structure

- `backend/migrations/200_add_radar_reliability_and_dr.sql` 保存性能与演练的追加式 schema。
- `backend/internal/service/evaluation_reliability.go` 定义 Load Plan、Snapshot、实验和恢复合同。
- `backend/internal/repository/evaluation_reliability_repo.go` 实现事务、Head、outbox 与查询。
- `backend/internal/handler/admin/evaluation_reliability_handler.go` 暴露受 RBAC 保护的管理 API。
- `radar-worker/src/sub2api_radar/loadgen/` 实现分层负载发生器、直方图和账本对账。
- `radar-worker/src/sub2api_radar/chaos/` 实现实验护栏和恢复验证客户端。
- `deploy/docker-compose.radar-staging.yml` 增加 loadgen 与 chaos profile。

---

### Task 1: 建立 migration 200 可靠性与恢复 schema

**Files:**
- Create: `backend/migrations/200_add_radar_reliability_and_dr.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Test: `backend/internal/repository/evaluation_reliability_repo_integration_test.go`

**Interfaces:**
- Consumes: migration 199 的 `evaluation_runs`、Release Subject、统一 outbox 与 cause relation。
- Produces: `evaluation_load_plans`、`evaluation_reliability_snapshots`、`evaluation_reliability_heads`、`evaluation_fault_experiments`、`evaluation_recovery_evidence`。

- [ ] **Step 1: 写 migration 200 失败测试**

```go
func TestMigration200ReliabilitySchema(t *testing.T) {
    requireTable(t, db, "evaluation_load_plans")
    requireTable(t, db, "evaluation_reliability_snapshots")
    requireTable(t, db, "evaluation_reliability_heads")
    requireTable(t, db, "evaluation_fault_experiments")
    requireTable(t, db, "evaluation_recovery_evidence")
    requireColumns(t, db, "evaluation_reliability_snapshots", []string{
        "run_id", "load_plan_id", "window_start", "window_end",
        "source_watermark", "request_count", "success_count", "error_count",
        "timeout_count", "retry_count", "ttft_histogram_hash",
        "latency_histogram_hash", "p99_latency_ms", "fresh_until", "snapshot_hash",
    })
}
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestMigration200ReliabilitySchema -count=1`

Expected: FAIL，migration 200 对象不存在。

- [ ] **Step 3: 创建追加式 schema**

```sql
CREATE TABLE IF NOT EXISTS evaluation_load_plans (
  id uuid PRIMARY KEY,
  schema_version text NOT NULL CHECK (schema_version = 'radar-load-plan-v1'),
  tenant_id bigint NOT NULL,
  canonical_plan_bytes bytea NOT NULL,
  load_plan_sha256 char(64) NOT NULL UNIQUE,
  status text NOT NULL CHECK (status IN ('draft','published','retired')),
  created_by bigint NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT transaction_timestamp()
);

CREATE TABLE IF NOT EXISTS evaluation_reliability_snapshots (
  id uuid NOT NULL,
  snapshot_created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
  run_id uuid NOT NULL REFERENCES evaluation_runs(id),
  load_plan_id uuid NOT NULL REFERENCES evaluation_load_plans(id),
  window_start timestamptz NOT NULL,
  window_end timestamptz NOT NULL,
  source_watermark char(64) NOT NULL,
  query_version text NOT NULL,
  slice_key text NOT NULL,
  request_count bigint NOT NULL CHECK (request_count >= 0),
  success_count bigint NOT NULL CHECK (success_count >= 0),
  error_count bigint NOT NULL CHECK (error_count >= 0),
  timeout_count bigint NOT NULL CHECK (timeout_count >= 0),
  retry_count bigint NOT NULL CHECK (retry_count >= 0),
  protocol_error_count bigint NOT NULL CHECK (protocol_error_count >= 0),
  billing_idempotency_failures bigint NOT NULL CHECK (billing_idempotency_failures >= 0),
  ttft_histogram_hash char(64) NOT NULL,
  latency_histogram_hash char(64) NOT NULL,
  p99_latency_ms bigint NOT NULL CHECK (p99_latency_ms >= 0),
  error_rate numeric(12,9) NOT NULL,
  cost_amount numeric(20,8) NOT NULL,
  fresh_until timestamptz NOT NULL,
  snapshot_hash char(64) NOT NULL,
  PRIMARY KEY (id, snapshot_created_at),
  UNIQUE (run_id, load_plan_id, slice_key, query_version, source_watermark)
) PARTITION BY RANGE (snapshot_created_at);
```

同一迁移创建月分区、Head 的完整 SnapshotRef、Fault Experiment 状态事件、Recovery Evidence、immutable trigger 和 Head/outbox 同事务约束。

- [ ] **Step 4: 写约束拒绝测试**

测试 UPDATE Snapshot、DELETE Snapshot、Head 指向错误 Run、负计数、`success_count > request_count`、重复 source watermark、未批准实验进入 running，逐项断言数据库拒绝。

- [ ] **Step 5: 运行 migration 与仓储测试**

Run: `cd backend && go test ./internal/repository -run 'TestMigration200|TestReliabilitySchema' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/migrations/200_add_radar_reliability_and_dr.sql backend/internal/repository/migrations_schema_integration_test.go backend/internal/repository/evaluation_reliability_repo_integration_test.go
git commit -m "feat(radar): add reliability and recovery schema"
```

### Task 2: 实现 Load Plan canonicalization 与管理 API

**Files:**
- Create: `backend/internal/service/evaluation_reliability.go`
- Create: `backend/internal/service/evaluation_reliability_test.go`
- Modify: `backend/internal/service/evaluation_rbac.go`
- Create: `backend/internal/service/evaluation_rbac_test.go`
- Create: `backend/internal/repository/evaluation_reliability_repo.go`
- Create: `backend/internal/repository/evaluation_reliability_repo_test.go`
- Create: `backend/internal/handler/admin/evaluation_reliability_handler.go`
- Create: `backend/internal/handler/admin/evaluation_reliability_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/server/routes/radar_governance_routes_test.go`
- Modify: `backend/internal/handler/handler.go`
- Modify: `backend/internal/repository/wire.go`
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`

**Interfaces:**
- Consumes: `RadarAuthorizer`、评测 Key、route profile 和预算数据。
- Produces: `CreateLoadPlan(ctx, actorID, input) (*RadarLoadPlan, error)`、`PublishLoadPlan(ctx, actorID, id) error`、`GetLoadPlan(ctx, id)`。

- [ ] **Step 1: 写 golden hash 与权限失败测试**

```go
func TestCanonicalLoadPlanGoldenHash(t *testing.T) {
    input := service.RadarLoadPlanInput{
        TenantID: 7, Environment: "staging", RouteProfileVersion: "route-v42",
        ModelAliases: []string{"qwen-plus", "deepseek-chat", "deepseek-chat"},
        Regions: []string{"cn-east"}, ConcurrencyLevels: []int{50, 1, 10},
        InputTokenBuckets: []int{128, 2048}, OutputTokenBuckets: []int{64, 512},
        WarmupSeconds: 120, MeasurementSeconds: 600,
        MinimumValidRequests: 100, MaxRunCost: decimal.RequireFromString("10.00000000"),
        MaxConcurrency: 50, ClientImageDigest: "sha256:" + strings.Repeat("a", 64),
        GeneratorVersion: "loadgen-v1",
    }
    got, err := service.CanonicalLoadPlan(input)
    require.NoError(t, err)
    require.Equal(t, []string{"deepseek-chat", "qwen-plus"}, got.ModelAliases)
    require.Equal(t, []int{1, 10, 50}, got.ConcurrencyLevels)
    require.Len(t, got.SHA256, 64)
}
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/handler/admin -run 'TestCanonicalLoadPlan|TestCreateLoadPlan' -count=1`

Expected: FAIL，合同与 Handler 尚未定义。

- [ ] **Step 3: 定义稳定类型和校验**

```go
type RadarLoadPlanInput struct {
    TenantID int64 `json:"tenant_id"`
    Environment string `json:"environment"`
    RouteProfileVersion string `json:"route_profile_version"`
    ModelAliases []string `json:"model_aliases"`
    Regions []string `json:"regions"`
    TrafficMode string `json:"traffic_mode"`
    ConcurrencyLevels []int `json:"concurrency_levels"`
    InputTokenBuckets []int `json:"input_token_buckets"`
    OutputTokenBuckets []int `json:"output_token_buckets"`
    WarmupSeconds int `json:"warmup_seconds"`
    MeasurementSeconds int `json:"measurement_seconds"`
    MinimumValidRequests int `json:"minimum_valid_requests"`
    MaxRunCost decimal.Decimal `json:"max_run_cost"`
    MaxConcurrency int `json:"max_concurrency"`
    ClientImageDigest string `json:"client_image_digest"`
    GeneratorVersion string `json:"generator_version"`
}
```

`CanonicalLoadPlan` 去重排序集合字段，拒绝空模型、非正并发、超预算、未注册镜像和不属于评测租户的 Key。服务端保存 canonical bytes 与 hash。

- [ ] **Step 4: 接入路由与 RBAC**

新增 `load_plan_manage` 权限，`quality_admin` 可以创建与发布，`viewer` 只能读取。注册：

```go
radar.POST("/reliability/load-plans", h.Admin.RadarReliability.CreateLoadPlan)
radar.POST("/reliability/load-plans/:id/publish", h.Admin.RadarReliability.PublishLoadPlan)
radar.GET("/reliability/load-plans/:id", h.Admin.RadarReliability.GetLoadPlan)
```

- [ ] **Step 5: 运行单元与路由测试**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin ./internal/server/routes -run 'Test.*LoadPlan|TestRadarReliabilityRoutes' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/internal/service/evaluation_reliability.go backend/internal/service/evaluation_reliability_test.go backend/internal/service/evaluation_rbac.go backend/internal/service/evaluation_rbac_test.go backend/internal/repository/evaluation_reliability_repo.go backend/internal/repository/evaluation_reliability_repo_test.go backend/internal/handler/admin/evaluation_reliability_handler.go backend/internal/handler/admin/evaluation_reliability_handler_test.go backend/internal/server/routes/admin.go backend/internal/server/routes/radar_governance_routes_test.go backend/internal/handler/handler.go backend/internal/repository/wire.go backend/internal/service/wire.go backend/internal/handler/wire.go backend/cmd/server/wire_gen.go
git commit -m "feat(radar): add immutable load plans"
```

### Task 3: 发布可信 Reliability Snapshot 与推进 Head

**Files:**
- Modify: `backend/internal/service/evaluation_reliability.go`
- Modify: `backend/internal/repository/evaluation_reliability_repo.go`
- Create: `backend/internal/repository/evaluation_reliability_repo_integration_test.go`
- Create: `backend/internal/handler/internal/radar_reliability_handler.go`
- Create: `backend/internal/handler/internal/radar_reliability_handler_test.go`
- Modify: `backend/internal/handler/handler.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/internal/server/routes/radar_worker.go`
- Modify: `backend/internal/server/routes/radar_worker_test.go`
- Modify: `backend/internal/server/router.go`
- Modify: `backend/cmd/server/wire_gen.go`

**Interfaces:**
- Consumes: 冻结窗口、网关请求计数、账单事件、直方图 artifact hash 和 migration 199 outbox。
- Produces: `PublishReliabilitySnapshot(ctx, submission) (*RadarReliabilitySnapshot, error)` 与 current `ReliabilityHeadRef`。

- [ ] **Step 1: 写分母、hash 与 stale Head 失败测试**

```go
func TestPublishReliabilitySnapshotRejectsIncompleteDenominator(t *testing.T)
func TestPublishReliabilitySnapshotRejectsUnreconciledBilling(t *testing.T)
func TestPublishReliabilitySnapshotIsIdempotentByWatermark(t *testing.T)
func TestOlderReliabilitySnapshotCannotReplaceCurrentHead(t *testing.T)
func TestSnapshotHeadAndOutboxCommitAtomically(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run 'TestPublishReliability|TestSnapshotHead' -count=1`

Expected: FAIL，Publisher 尚不存在。

- [ ] **Step 3: 定义提交合同**

```go
type ReliabilitySnapshotSubmission struct {
    RunID uuid.UUID
    LoadPlanID uuid.UUID
    WindowStart time.Time
    WindowEnd time.Time
    SourceWatermark string
    QueryVersion string
    SliceKey string
    RequestCount int64
    SuccessCount int64
    ErrorCount int64
    TimeoutCount int64
    RetryCount int64
    ProtocolErrorCount int64
    BillingIdempotencyFailures int64
    TTFTHistogramHash string
    LatencyHistogramHash string
    P99LatencyMS int64
    ErrorRate decimal.Decimal
    CostAmount decimal.Decimal
    FreshUntil time.Time
}
```

校验 `success + terminal failures == request_count`，retry 单独计数且不扩张请求分母。Repository 在 repeatable-read 事务中复算 source watermark 和 snapshot hash，插入 Snapshot、推进完整 SnapshotRef Head、写 `reliability_head_advanced` outbox。

- [ ] **Step 4: 暴露内部发布端点**

注册 `POST /internal/radar/v1/reliability-snapshots`，只允许 `statistics` worker kind、active worker token 和与 Run 匹配的 image digest。调用方不能提交 `snapshot_hash` 或 Head 版本。

- [ ] **Step 5: 运行集成测试**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal -run 'TestPublishReliability|TestReliabilitySnapshot' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/internal/service/evaluation_reliability.go backend/internal/repository/evaluation_reliability_repo.go backend/internal/repository/evaluation_reliability_repo_integration_test.go backend/internal/handler/internal/radar_reliability_handler.go backend/internal/handler/internal/radar_reliability_handler_test.go backend/internal/handler/handler.go backend/internal/handler/wire.go backend/internal/server/routes/radar_worker.go backend/internal/server/routes/radar_worker_test.go backend/internal/server/router.go backend/cmd/server/wire_gen.go
git commit -m "feat(radar): publish trusted reliability snapshots"
```

### Task 4: 实现多租户负载发生器与直方图

**Files:**
- Create: `radar-worker/src/sub2api_radar/loadgen/__init__.py`
- Create: `radar-worker/src/sub2api_radar/loadgen/models.py`
- Create: `radar-worker/src/sub2api_radar/loadgen/histogram.py`
- Create: `radar-worker/src/sub2api_radar/loadgen/runner.py`
- Create: `radar-worker/src/sub2api_radar/loadgen/publisher.py`
- Create: `radar-worker/tests/test_loadgen_histogram.py`
- Create: `radar-worker/tests/test_loadgen_runner.py`
- Modify: `radar-worker/pyproject.toml`
- Modify: `radar-worker/src/sub2api_radar/config.py`

**Interfaces:**
- Consumes: published Load Plan、受控评测 Key、Gateway OpenAI 兼容端点。
- Produces: 固定 bucket 直方图 artifact、完整分母、账本对账数据和 Snapshot submission。

- [ ] **Step 1: 写直方图和分母失败测试**

```python
def test_histogram_hash_is_order_independent() -> None:
    left = FixedHistogram.observe_many([100, 10, 50])
    right = FixedHistogram.observe_many([50, 100, 10])
    assert left.canonical_bytes() == right.canonical_bytes()
    assert left.sha256() == right.sha256()

def test_window_keeps_timeout_in_request_denominator() -> None:
    window = ReliabilityWindow()
    window.record_success(ttft_ms=10, latency_ms=100, cost="0.01")
    window.record_timeout()
    assert window.request_count == 2
    assert window.success_count == 1
    assert window.timeout_count == 1
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd radar-worker && uv run pytest tests/test_loadgen_histogram.py tests/test_loadgen_runner.py -q`

Expected: FAIL，`sub2api_radar.loadgen` 不存在。

- [ ] **Step 3: 实现固定直方图和调度器**

```python
class LoadCell(BaseModel):
    model_alias: str
    region: str
    concurrency: int = Field(gt=0)
    input_tokens: int = Field(gt=0)
    output_tokens: int = Field(gt=0)
    streaming: bool

class ReliabilityWindow(BaseModel):
    request_count: int = 0
    success_count: int = 0
    error_count: int = 0
    timeout_count: int = 0
    retry_count: int = 0
    protocol_error_count: int = 0
    billing_idempotency_failures: int = 0
```

Runner 用 `asyncio.TaskGroup` 控制每个 cell 的并发，先完成 120 秒 warmup，再执行 600 秒测量或达到最小请求数。每个请求携带 `run_id`、`load_cell_id` 和评测 token。客户端丢包、取消、解析失败和服务端错误都进入有限分类。

- [ ] **Step 4: 实现 artifact 与 Snapshot 发布**

Publisher 先上传直方图 canonical bytes，确认 SHA256，再提交 Snapshot。提交前断言账单已完成对账、所有请求都有终态、`fresh_until = window_end + 5m`。

- [ ] **Step 5: 运行 Python 质量门**

Run: `cd radar-worker && uv run pytest tests/test_loadgen_histogram.py tests/test_loadgen_runner.py -q && uv run ruff check src tests && uv run mypy src`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add radar-worker/src/sub2api_radar/loadgen radar-worker/tests/test_loadgen_histogram.py radar-worker/tests/test_loadgen_runner.py radar-worker/pyproject.toml radar-worker/src/sub2api_radar/config.py radar-worker/uv.lock
git commit -m "feat(radar): add multi-tenant reliability load generator"
```

### Task 5: 将 Reliability Head 接入可信 Gate

**Files:**
- Modify: `backend/internal/service/evaluation_gate_service.go`
- Create: `backend/internal/service/evaluation_gate_service_test.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Create: `backend/internal/repository/evaluation_governance_repo_integration_test.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`

**Interfaces:**
- Consumes: migration 199 Gate Evidence Loader、current Reliability Head、Policy SLO 阈值。
- Produces: `slo.reliability.*`、`cost.per_success` 和 `evidence.reliability_freshness` rule results。

- [ ] **Step 1: 写 Gate 短路失败测试**

```go
func TestGateRejectsMissingReliabilityHead(t *testing.T)
func TestGateRejectsExpiredReliabilitySnapshot(t *testing.T)
func TestGateBlocksP99BeforeQualityRules(t *testing.T)
func TestGateBlocksBillingIdempotencyFailure(t *testing.T)
func TestReliabilityHeadChangeInvalidatesOldDecision(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'TestGate.*Reliability|TestReliabilityHeadChange' -count=1`

Expected: FAIL，Gate 仍只消费布尔 `ReliabilitySLOBreached` 或缺少 current Head。

- [ ] **Step 3: 加载并验证 SnapshotRef**

Evidence Loader 在同一 repeatable-read 事务中锁定 `snapshot_id + snapshot_created_at + snapshot_hash + head_version`，复算直方图 hash、错误率和 source watermark。过期、切片不完整、账单未对账或 query version 不允许都返回 `insufficient_evidence`。

- [ ] **Step 4: 实现固定可靠性短路顺序**

```go
if !evidence.ReliabilityFresh {
    return insufficient("evidence.reliability_freshness")
}
if evidence.BillingIdempotencyFailures > 0 {
    return blocked("billing.idempotency")
}
if evidence.P99LatencyMS > policy.MaxP99LatencyMS {
    return blocked("slo.reliability.p99")
}
if evidence.ErrorRate.GreaterThan(policy.MaxErrorRate) {
    return blocked("slo.reliability.error_rate")
}
```

- [ ] **Step 5: 运行 Gate 回归测试**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin -run 'Test.*Gate|Test.*Reliability' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/internal/service/evaluation_gate_service.go backend/internal/service/evaluation_gate_service_test.go backend/internal/repository/evaluation_governance_repo.go backend/internal/repository/evaluation_governance_repo_integration_test.go backend/internal/handler/admin/evaluation_governance_handler.go
git commit -m "feat(radar): enforce reliability evidence in release gates"
```

### Task 6: 实现受控 Fault Experiment 与 Recovery Verifier

**Files:**
- Modify: `backend/internal/service/evaluation_reliability.go`
- Modify: `backend/internal/repository/evaluation_reliability_repo.go`
- Modify: `backend/internal/handler/admin/evaluation_reliability_handler.go`
- Create: `radar-worker/src/sub2api_radar/chaos/controller.py`
- Create: `radar-worker/src/sub2api_radar/chaos/recovery.py`
- Create: `radar-worker/tests/test_chaos_controller.py`
- Create: `radar-worker/tests/test_recovery_verifier.py`
- Modify: `deploy/docker-compose.radar-staging.yml`

**Interfaces:**
- Consumes: 已批准 Experiment、目标 selector、实时 SLO、备份与 deterministic acceptance API。
- Produces: append-only Experiment events、自动停止事件、Recovery Evidence、RPO/RTO 报告。

- [ ] **Step 1: 写护栏失败测试**

```python
@pytest.mark.parametrize("signal", [
    {"customer_error_rate_delta": 0.006},
    {"customer_p99_ratio": 1.21},
    {"control_plane_availability": 0.998},
    {"data_hash_consistent": False},
    {"alert_delivery_ok": False},
])
def test_guardrail_stops_experiment(signal: dict[str, object]) -> None:
    assert GuardrailPolicy.default().must_stop(signal)
```

Go 测试还要拒绝单次多目标、未审批、生产数据库切换和影响范围超过一个 Worker 的初始实验。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd radar-worker && uv run pytest tests/test_chaos_controller.py tests/test_recovery_verifier.py -q`

Expected: FAIL，chaos 模块不存在。

- [ ] **Step 3: 实现实验状态机**

```text
draft -> approved -> running -> stopping -> completed
                     |             |
                     v             v
                   aborted       failed
```

每个转换保存 actor、service identity、指标快照、cause event 和时间。Controller 只调用固定 action registry，首版只包含 `worker_process_stop` 与 `worker_network_isolation`。

- [ ] **Step 4: 实现恢复证明**

Recovery Verifier 计算最后持久事务时间、可用对象版本、声明 failover 时间和 deterministic acceptance 完成时间，输出 RPO/RTO。它还验证 Worker 重注册、lease 回收、Score 不重复、Evidence hash、账本和告警可达性。

- [ ] **Step 5: 运行 Go、Python 与 Compose 测试**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin -run 'Test.*FaultExperiment|Test.*RecoveryEvidence' -count=1`

Run: `cd radar-worker && uv run pytest tests/test_chaos_controller.py tests/test_recovery_verifier.py -q`

Run: `docker compose -f deploy/docker-compose.radar-staging.yml --profile chaos config --quiet`

Expected: 全部 PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/internal/service/evaluation_reliability.go backend/internal/repository/evaluation_reliability_repo.go backend/internal/handler/admin/evaluation_reliability_handler.go radar-worker/src/sub2api_radar/chaos radar-worker/tests/test_chaos_controller.py radar-worker/tests/test_recovery_verifier.py deploy/docker-compose.radar-staging.yml
git commit -m "feat(radar): add controlled fault and recovery verification"
```

### Task 7: 完成峰值两倍、Gate 与恢复验收

**Files:**
- Create: `backend/internal/integration/radar_reliability_e2e_test.go`
- Create: `deploy/radar/load-plan-staging.json`
- Create: `deploy/radar/fault-experiment-worker-stop.json`
- Modify: `docs/radar-production-runbook.md`
- Modify: `docs/model-quality-radar-configuration.md`
- Modify: `.github/workflows/backend-ci.yml`

**Interfaces:**
- Consumes: Tasks 1 至 6 全部产物、staging 隔离身份和 synthetic upstream。
- Produces: 性能基线报告、峰值两倍报告、Fault Experiment、Recovery Evidence 和可信 Gate Decision。

- [ ] **Step 1: 写 E2E 验收测试**

```go
func TestRadarReliabilityE2E(t *testing.T) {
    run := seedDeterministicRadarRun(t, 30)
    snapshot := publishReliabilitySnapshot(t, run.ID, reliabilityFixture{
        RequestCount: 600, SuccessCount: 600, P99LatencyMS: 200,
    })
    decision := evaluateTrustedGate(t, run.ID)
    require.Equal(t, "passed", decision.Status)
    publishReliabilitySnapshot(t, run.ID, reliabilityFixture{
        RequestCount: 600, SuccessCount: 570, ErrorCount: 30, P99LatencyMS: 2000,
    })
    require.Equal(t, "blocked", currentDecision(t, run.ID).Status)
    require.NotEqual(t, snapshot.SnapshotHash, currentReliabilityHead(t, run.ID).SnapshotHash)
}
```

- [ ] **Step 2: 运行 E2E 并确认 RED**

Run: `cd backend && go test ./internal/integration -run TestRadarReliabilityE2E -count=1 -v`

Expected: FAIL，验收 fixture 或集成装配尚未完成。

- [ ] **Step 3: 完成 staging profile 与 Runbook**

配置并发 1、10、50、100、峰值和峰值两倍，输入桶 128、2K、8K、32K，输出桶 64、512、2K。Runbook 写明预算告警、停止护栏、恢复查询、RPO/RTO 计算和回切批准。

- [ ] **Step 4: 执行全量质量门**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin ./internal/server/routes ./internal/integration -run 'Test.*(Reliability|LoadPlan|FaultExperiment|Recovery)' -count=1`

Run: `cd radar-worker && uv run pytest -q && uv run ruff check src tests && uv run mypy src`

Run: `docker compose -f deploy/docker-compose.radar-staging.yml --profile chaos config --quiet`

Expected: 全部 PASS，无 dead letter、无活跃 lease、无重复 Score、快照和 Gate hash 可复算。

- [ ] **Step 5: 提交任务**

```bash
git add backend/internal/integration/radar_reliability_e2e_test.go deploy/radar/load-plan-staging.json deploy/radar/fault-experiment-worker-stop.json docs/radar-production-runbook.md docs/model-quality-radar-configuration.md .github/workflows/backend-ci.yml
git commit -m "test(radar): verify reliability chaos and recovery gates"
```

## 完成标准

1. 每个 Reliability Snapshot 都能由网关、引擎、账本和直方图 artifact 独立复算。
2. 过期、缺失、账单未对账和不完整分母进入 `insufficient_evidence`。
3. P99、错误率、计费幂等和成本超限按固定顺序阻断 Gate。
4. 多租户峰值两倍压测证明限流、公平性、预算和客户流量隔离。
5. Fault Experiment 具备双审批、实时停止、回滚和不可变事件链。
6. Recovery Evidence 证明 RPO、RTO、lease 回收、Score 不重复、Evidence hash 和账本一致。
