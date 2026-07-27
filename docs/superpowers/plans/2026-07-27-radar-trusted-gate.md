# Radar 可信 Gate 与不可变治理实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Gate 只使用数据库中的不可变 policy、score、aggregate 和 route evidence 计算决策，并以追加式、幂等方式保存原始决策和豁免。

**Architecture:** HTTP 请求只提交 `run_id`、`policy_id` 和可选 `baseline_id`。Repository 在一个只读证据快照中加载 policy、run、sample、route evidence、score head、aggregate 和可靠性指标，服务层按固定优先级求值，Repository 使用自然幂等键追加 decision。Waiver 作为独立记录改变有效展示状态，不更新原始 decision。

**Tech Stack:** Go 1.24、Gin、PostgreSQL 18、`database/sql`、`encoding/json`、SHA256、`shopspring/decimal`、sqlmock、Go integration tests、Python 3.12 statistics Worker。

## Global Constraints

- Gate HTTP 请求不得接受 status、rule IDs、Delta、CI、route match、可靠性布尔值、policy 内容或 evidence hash。
- policy 内容由服务端规范化并计算 SHA256；同一 version 不允许覆盖。
- decision、waiver、score 和 aggregate 都是追加式记录，数据库拒绝 UPDATE 和 DELETE。
- 相同 `run_id + policy_id + evidence_hash` 的重试返回同一 decision；证据变化产生新 decision。
- P0、route identity、可靠性 SLO 和证据充分性在 14 天 record-only 期间继续执行。
- route identity 至少验证 run、sample、API key、route trace、requested alias、route profile 和 transport 终态；配置了 `expected_resolved_model` 时必须同时匹配 resolved model。
- 旧 run 的 route profile 为 `legacy-unbound`，只能得到 `insufficient_evidence`。
- 每个能力域和模型对都必须有当前 aggregate。Run 覆盖多个能力域或多个模型对时，还必须有 `capability_domain='global'` 且 `model_route='global'` 的显式全局 aggregate，缺少时只能得到 `insufficient_evidence`。
- 原始 prompt、completion、token、账号 ID、渠道 ID和任意上游正文不能进入 Gate evidence 或响应。
- 所有行为变更先写失败测试并观察预期失败。

---

### Task 1: 增加不可变 Gate 与 Run 路由身份 schema

**Files:**
- Create: `backend/migrations/198_add_radar_trusted_gate.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Test: `backend/internal/repository/migrations_schema_integration_test.go`

**Interfaces:**
- Produces: `evaluation_runs.route_profile_version VARCHAR(100) NOT NULL`。
- Changes: decision 幂等约束从 `(run_id, policy_id)` 改为 `(run_id, policy_id, evidence_hash)`。
- Produces: policy、decision 和 waiver 数据库不可变触发器。

- [ ] **Step 1: 写 schema 与不可变性失败测试**

测试创建两个相同 run/policy、不同 evidence hash 的 decision 成功；相同三元组重复失败；更新 policy JSON、decision status 或 waiver reason 都被数据库触发器拒绝；score 和 aggregate 的 UPDATE、DELETE 都被拒绝；只更新 policy `retired_at` 允许。

```go
func TestTrustedGateMigrationAllowsAppendOnlyReevaluation(t *testing.T)
func TestTrustedGateMigrationRejectsPolicyMutation(t *testing.T)
func TestTrustedGateMigrationRejectsDecisionAndWaiverMutation(t *testing.T)
func TestTrustedGateMigrationRejectsScoreAndAggregateDeletion(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestTrustedGateMigration -count=1`

Expected: FAIL，旧唯一约束阻止第二条 decision，更新仍可成功。

- [ ] **Step 3: 编写 migration 198**

迁移必须完成：

```sql
ALTER TABLE evaluation_runs
    ADD COLUMN IF NOT EXISTS route_profile_version VARCHAR(100) NOT NULL DEFAULT 'legacy-unbound';

ALTER TABLE evaluation_gate_decisions
    DROP CONSTRAINT IF EXISTS evaluation_gate_decisions_run_id_policy_id_key;

CREATE UNIQUE INDEX IF NOT EXISTS idx_evaluation_gate_decisions_idempotency
    ON evaluation_gate_decisions (run_id, policy_id, evidence_hash);
```

policy trigger 允许 `retired_at` 从 NULL 变为时间值，其余字段不可变。decision、waiver、score 和 aggregate trigger 对不允许的 UPDATE 与 DELETE 一律抛错。迁移不改写历史 decision 内容。

- [ ] **Step 4: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run TestTrustedGateMigration -count=1`

Expected: PASS。

- [ ] **Step 5: 提交本任务**

```bash
git add backend/migrations/198_add_radar_trusted_gate.sql backend/internal/repository/migrations_schema_integration_test.go
git commit -m "feat(radar): make gate evidence append only"
```

### Task 2: 固化 Gate policy 类型、校验和状态优先级

**Files:**
- Modify: `backend/internal/service/evaluation_gate_service.go`
- Modify: `backend/internal/service/evaluation_gate_service_test.go`
- Test: `backend/internal/service/evaluation_gate_service_test.go`

**Interfaces:**
- Produces: 带 JSON tag 的 `RadarGatePolicy`。
- Produces: `ValidateRadarGatePolicy(policy RadarGatePolicy) error`。
- Consumes: 只由服务端证据构造的 `RadarGateInput`。

- [ ] **Step 1: 写优先级和校验失败测试**

必须覆盖：

```go
func TestEvaluateRadarGateMissingEvidenceBeatsRecordOnly(t *testing.T)
func TestEvaluateRadarGateP0BlocksDuringRecordOnly(t *testing.T)
func TestEvaluateRadarGateRouteMismatchBlocksDuringRecordOnly(t *testing.T)
func TestEvaluateRadarGateReliabilityBlocksDuringRecordOnly(t *testing.T)
func TestEvaluateRadarGateQualityOnlyRecordsDuringCalibration(t *testing.T)
func TestEvaluateRadarGateNegativeEstimateRequestsReview(t *testing.T)
func TestValidateRadarGatePolicyRejectsUnsafeRanges(t *testing.T)
```

策略固定字段：

```go
type RadarGatePolicy struct {
    ObservationDays       int     `json:"observation_days"`
    CriticalDomainDeltaPP float64 `json:"critical_domain_delta_pp"`
    AggregateDeltaPP      float64 `json:"aggregate_delta_pp"`
    ConfidenceLevel       float64 `json:"confidence_level"`
    RequireCIExcludeZero  bool    `json:"require_ci_exclude_zero"`
    MinimumPairs          int     `json:"minimum_pairs"`
    MaxP99MS              float64 `json:"max_p99_ms"`
    MaxErrorRate          float64 `json:"max_error_rate"`
}
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service -run 'TestEvaluateRadarGate|TestValidateRadarGatePolicy' -count=1`

Expected: record-only P0 测试 FAIL，缺少字段和校验函数。

- [ ] **Step 3: 实现固定短路顺序**

顺序必须为 evidence、P0、route、reliability、record-only、critical quality、aggregate quality、judge disagreement、negative trend、pass。`waived` 不由 `EvaluateRadarGate` 返回。

校验范围为 observation days 0 到 90、阈值负 100 到 0、confidence 0.8 到小于 1、minimum pairs 1 到 100000、P99 大于 0、error rate 0 到 1。

- [ ] **Step 4: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service -run 'TestEvaluateRadarGate|TestValidateRadarGatePolicy' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交本任务**

```bash
git add backend/internal/service/evaluation_gate_service.go backend/internal/service/evaluation_gate_service_test.go
git commit -m "fix(radar): define deterministic gate precedence"
```

### Task 3: 冻结 Run 的 route profile

**Files:**
- Modify: `backend/internal/service/evaluation_repository.go`
- Modify: `backend/internal/repository/evaluation_repo.go`
- Modify: `backend/internal/repository/evaluation_repo_test.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/internal/handler/handler.go`
- Test: `backend/internal/repository/evaluation_repo_test.go`
- Test: `backend/internal/handler/admin/evaluation_governance_handler_test.go`

**Interfaces:**
- Changes: `CreateRunInput` 增加 `RouteProfileVersion string`。
- Changes: `RadarGovernanceHandler` 注入 `*config.Config`，忽略任何客户端 route profile 字段。

- [ ] **Step 1: 写失败测试**

Handler 测试设置 config 为 `staging-v1`，请求只含 plan 与 baseline/candidate ref，断言 Repository 收到 `RouteProfileVersion == "staging-v1"`。Repository SQL 测试断言该值写入 run。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/handler/admin ./internal/repository -run 'Test.*StartRun.*RouteProfile|Test.*CreateRun.*RouteProfile' -count=1`

Expected: FAIL，CreateRunInput 没有该字段。

- [ ] **Step 3: 实现服务端冻结**

Radar 启用时 route profile 为空直接返回 503。创建 run 的 INSERT 写入 `route_profile_version`，run 事件 payload 同时记录该值。客户端同名 JSON 字段不会覆盖 config。

- [ ] **Step 4: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/handler/admin ./internal/repository -run 'Test.*StartRun.*RouteProfile|Test.*CreateRun.*RouteProfile' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交本任务**

```bash
git add backend/internal/service/evaluation_repository.go backend/internal/repository/evaluation_repo.go backend/internal/repository/evaluation_repo_test.go backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go backend/internal/handler/wire.go backend/internal/handler/handler.go
git commit -m "feat(radar): freeze route profile on runs"
```

### Task 4: 从数据库构建可信 Gate evidence

**Files:**
- Create: `backend/internal/repository/evaluation_gate_evidence_repo.go`
- Create: `backend/internal/repository/evaluation_gate_evidence_repo_integration_test.go`
- Modify: `backend/internal/service/evaluation_governance.go`
- Test: `backend/internal/repository/evaluation_gate_evidence_repo_integration_test.go`

**Interfaces:**
- Produces: `LoadRadarGateEvidence(ctx context.Context, runID uuid.UUID, policy RadarGatePolicy) (*RadarGateEvidence, error)`。
- Produces: `RadarGateEvidence.Input RadarGateInput`、`Evidence json.RawMessage`、`EvidenceHash string`。

- [ ] **Step 1: 写可信来源失败测试**

建立一个 run、两个 sample、匹配和错配的 route evidence、immutable scores 与 aggregate。覆盖：完整证据成功；requested alias 错配；route profile 错配；API key 错配；缺少 route evidence；不足 minimum pairs；P0 protocol/safety failure；manual review；P99 和 error rate 越界。

```go
func TestLoadRadarGateEvidenceAcceptsCompleteBoundEvidence(t *testing.T)
func TestLoadRadarGateEvidenceRejectsRouteIdentityMismatch(t *testing.T)
func TestLoadRadarGateEvidenceMarksMissingRowsInsufficient(t *testing.T)
func TestLoadRadarGateEvidenceDerivesP0JudgeAndReliability(t *testing.T)
func TestLoadRadarGateEvidenceRequiresGlobalAggregateForMultiDomain(t *testing.T)
func TestLoadRadarGateEvidenceRequiresGlobalAggregateForMultipleModelPairs(t *testing.T)
```

测试还要提交同一 sample 的第二版 score，证明 Worker 生命周期计划建立的 score head 语义在迁移 198 生效后仍可成功。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestLoadRadarGateEvidence -count=1`

Expected: FAIL，loader 尚不存在。

- [ ] **Step 3: 实现 route identity 查询**

每个 sample 必须恰有一条 evidence，并满足：

```text
e.evaluation_run_id = s.run_id
e.sample_id = s.id
e.api_key_id = p.gateway_api_key_id
e.route_trace_id = s.route_trace_id
e.requested_model = s.model_config->>'route'
e.route_profile_version = r.route_profile_version
e.transport_status = 'succeeded'
e.finished_at IS NOT NULL
```

`model_config.expected_resolved_model` 存在时再要求 `e.resolved_model` 相等。`legacy-unbound`、缺行或缺少成功终态令 evidence insufficient；任一明确错配令 route match 为 false。

- [ ] **Step 4: 实现质量、P0、Judge 与可靠性推导**

质量只读取当前 score head 指向的 score 和每个能力域、模型对的最新 immutable aggregate。`effective_pair_count`、`evidence_sufficiency`、Delta 与 CI 字段缺失或类型错误令 evidence insufficient。缺少任一能力域或模型对的 aggregate 同样令 evidence insufficient。P0 协议或安全失败由 candidate P0 sample 与 score failure class 推导。Judge disagreement 来自 current score 的 `manual_review_required`。

单能力域且单模型对时，该能力域的 candidate aggregate 同时提供 critical 和 aggregate 指标。存在多个能力域或多个模型对时，critical 指标取所有能力域 candidate aggregate 中最差的 Delta 及其对应 CI，全局 aggregate 指标只读取 `capability_domain='global'` 且 `model_route='global'` 的显式快照。显式全局快照不能替代任何缺失的能力域或模型对快照。

Gate loader 的所有当前分数读取都以 score head 为准，不读取 `evaluation_scores.is_current`。

P99 使用 `percentile_cont(0.99)` 计算成功 route evidence 的 `latency_ms`，error rate 使用非 succeeded 行数除以全部 bound evidence。任何缺失 latency 的成功行令 reliability evidence insufficient。比较阈值后设置 `ReliabilitySLOBreached`。

- [ ] **Step 5: 生成规范化 Evidence 和服务端 hash**

Evidence 只包含 UUID、model config SHA256、route profile、脱敏 route refs、aggregate ID、score ID、计数、Delta、CI、P99、error rate 和计算版本。按确定字段顺序 JSON marshal 后计算 SHA256。测试同一数据库快照重复加载得到完全相同字节与 hash。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run TestLoadRadarGateEvidence -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/repository/evaluation_gate_evidence_repo.go backend/internal/repository/evaluation_gate_evidence_repo_integration_test.go backend/internal/service/evaluation_governance.go
git commit -m "feat(radar): derive gate evidence server side"
```

### Task 5: 让 policy 创建与 decision 写入不可变且幂等

**Files:**
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_test.go`
- Modify: `backend/internal/service/evaluation_governance.go`
- Test: `backend/internal/repository/evaluation_governance_repo_test.go`

**Interfaces:**
- Changes: `CreateGatePolicy` 计算后的 canonical policy 与 hash 由调用方传入，Repository 只 INSERT。
- Changes: `RecordGateDecision` 增加 `EvaluatedBy int64` 或将 actor 写入 evidence 固定字段。
- Changes: `WaiveGateDecision` 不更新 decision。

- [ ] **Step 1: 写失败测试**

覆盖相同 version 不同 policy 返回 conflict；相同 evidence 决策重试返回原 ID；不同 evidence hash 追加新 ID；waiver 后原 decision status 保持 blocked；过期 waiver 不改变有效状态。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run 'Test(CreateGatePolicy|RecordGateDecision|WaiveGateDecision)' -count=1`

Expected: FAIL，现有 SQL 使用 conflict update 并改写 decision status。

- [ ] **Step 3: 替换 upsert 与 waiver update**

policy 使用纯 INSERT，将唯一冲突映射为 `ErrRadarPolicyVersionConflict`。decision 先按三元自然键查询，存在则返回，缺失则 INSERT；并发唯一冲突后重新查询。waiver 只 INSERT，禁止执行 `UPDATE evaluation_gate_decisions SET status='waived'`。

- [ ] **Step 4: 更新有效状态 projection**

`ListGates` 返回最新 decision，并通过未过期 waiver 的 `EXISTS` 计算展示状态 `waived`。原始 decision 的详情仍返回 `blocked` 或 `review_required`，并列出 waiver 引用。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'Test(CreateGatePolicy|RecordGateDecision|WaiveGateDecision|ListGates)' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/repository/evaluation_governance_repo.go backend/internal/repository/evaluation_governance_repo_test.go backend/internal/service/evaluation_governance.go
git commit -m "fix(radar): preserve immutable gate history"
```

### Task 6: 收窄 Gate HTTP 契约并执行服务端求值

**Files:**
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Modify: `backend/internal/server/routes/radar_governance_routes_test.go`
- Modify: `frontend/src/api/admin/radar.ts`
- Modify: `frontend/src/api/__tests__/admin.radar.spec.ts`
- Test: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Test: `frontend/src/api/__tests__/admin.radar.spec.ts`

**Interfaces:**
- Produces: `POST /api/v1/admin/radar/gates/evaluate` body 只含 `run_id`、`policy_id`、可选 `baseline_id`。
- Changes: policy 创建请求不再接受 `policy_hash`。

- [ ] **Step 1: 写 HTTP 失败测试**

请求体：

```json
{
  "run_id":"11111111-1111-1111-1111-111111111111",
  "policy_id":"22222222-2222-2222-2222-222222222222"
}
```

Repository stub 返回可信 evidence。测试加入恶意 `status:"passed"`、宽松 `policy`、伪造 `route_match:true` 和 `evidence_hash` 字段，断言它们不影响结果且不会传入 Repository。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/handler/admin ./internal/server/routes -run 'TestRadar.*Gate' -count=1`

Run: `cd frontend && pnpm test:run src/api/__tests__/admin.radar.spec.ts`

Expected: 后端旧请求合同测试 FAIL，前端缺少新窄合同。

- [ ] **Step 3: 实现服务端 pipeline**

Handler 顺序固定为授权、解析 IDs、加载已存 policy、加载可信 evidence、调用 `EvaluateRadarGate`、追加 decision、返回 decision 与脱敏 evidence summary。actor 只来自认证上下文。删除未注册的任意 status record handler 和所有客户端 gate input 类型。

- [ ] **Step 4: policy hash 服务端生成**

Handler decode typed policy，调用 `ValidateRadarGatePolicy`，使用 `json.Marshal` 的规范字段顺序计算 SHA256，忽略客户端任何 hash 字段。响应返回服务端 hash。

- [ ] **Step 5: 更新前端 API 类型并运行测试**

Run: `cd backend && go test ./internal/handler/admin ./internal/server/routes -run 'TestRadar.*Gate' -count=1`

Run: `cd frontend && pnpm test:run src/api/__tests__/admin.radar.spec.ts`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go backend/internal/server/routes/radar_governance_routes_test.go frontend/src/api/admin/radar.ts frontend/src/api/__tests__/admin.radar.spec.ts
git commit -m "fix(radar): evaluate gates from trusted evidence"
```

### Task 7: 补齐 30-pair Gate 集成验收

**Files:**
- Create: `backend/internal/repository/evaluation_trusted_gate_integration_test.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_integration_test.go`
- Test: `backend/internal/repository/evaluation_trusted_gate_integration_test.go`

**Interfaces:**
- Consumes: migration 198、可信 evidence loader、Gate service、append-only Repository。
- Produces: deterministic synthetic acceptance proof。

- [ ] **Step 1: 写端到端数据库失败测试**

种入 30 对完成 sample、60 个 score、匹配 route evidence 和一个 immutable aggregate：baseline 1、candidate 0、Delta -100、CI -100 到 -100。断言 enforcement 后 `blocked`，record-only 时 `recorded`，P0 或 route mismatch 在 record-only 时仍 `blocked`。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestTrustedGateThirtyPairLifecycle -count=1`

Expected: FAIL，完整 pipeline 尚未闭合。

- [ ] **Step 3: 完成最小集成夹具和断言**

测试同时断言 policy hash 可复算、evidence hash 可复算、重复 evaluate 返回同一 decision ID、改变 evidence 后追加新 ID、原始 blocked decision 在 waiver 后未被更新。

- [ ] **Step 4: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run TestTrustedGateThirtyPairLifecycle -count=1`

Expected: PASS。

- [ ] **Step 5: 提交本任务**

```bash
git add backend/internal/repository/evaluation_trusted_gate_integration_test.go backend/internal/repository/evaluation_governance_repo_integration_test.go
git commit -m "test(radar): prove trusted thirty pair gate"
```

### Task 8: 更新合同文档与全量验证

**Files:**
- Modify: `docs/model-quality-radar-configuration.md`
- Modify: `docs/radar-production-runbook.md`
- Modify: `docs/radar-platform-test-architecture.md`

- [ ] **Step 1: 更新 API 示例和 migration checksum 表**

删除客户端 policy、input 和 evidence hash 示例，改为窄 Gate 请求。加入 migrations 197 和 198 的 normalized SHA256。说明旧 run 为 `legacy-unbound`，无法进入 enforcement。

- [ ] **Step 2: 运行 Go 完整相关包**

Run: `cd backend && go test ./internal/repository ./internal/service ./internal/handler/admin ./internal/handler/internal ./internal/server/routes -count=1`

Expected: PASS。

- [ ] **Step 3: 运行 Worker 质量门**

Run: `cd radar-worker && .venv/bin/pytest -q`

Run: `cd radar-worker && .venv/bin/ruff check src tests`

Run: `cd radar-worker && .venv/bin/mypy src`

Expected: 全部退出 0。

- [ ] **Step 4: 运行前端测试、lint 和 build**

使用 Node 22.22.3 与 Corepack pnpm 9.15.9：

```bash
PATH=/Users/weijiahao/.nvm/versions/node/v22.22.3/bin:$PATH /Users/weijiahao/.nvm/versions/node/v22.22.3/bin/corepack pnpm test:run
PATH=/Users/weijiahao/.nvm/versions/node/v22.22.3/bin:$PATH /Users/weijiahao/.nvm/versions/node/v22.22.3/bin/corepack pnpm lint:check
PATH=/Users/weijiahao/.nvm/versions/node/v22.22.3/bin:$PATH /Users/weijiahao/.nvm/versions/node/v22.22.3/bin/corepack pnpm build
```

Run from: `frontend`

Expected: 三条命令退出 0。

- [ ] **Step 5: 执行静态检查并保存证据**

Run: `git diff --check`

Expected: 退出 0。

在 SDD ledger 记录 migrations 197 和 198 checksum、Gate 合同、30-pair 集成结果、测试数量以及尚未取得的远端运行证据。取得这些本地证据后才恢复 staging 上传步骤。
