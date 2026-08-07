# Radar 可信 Gate 与不可变发布治理实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 migration 198 与 migration 199 的 Gate 能力，使每个发布结论绑定不可变 ReleaseSubject、current Policy/Baseline/Score/Aggregate/Reliability Heads、可独立复算的 Evidence Manifest 和完整 source watermark，并通过短时效单次 Release Authorization 执行发布。

**Architecture:** migration 198 先把 Policy、Baseline、Decision、Waiver 和 ReleaseSubject 切到 append-only storage compatibility。migration 199 加入 Reliability Head 与可信 Evidence 输入后，Gate Evidence Loader 在 repeatable-read 快照内独立复算所有 hash 和 HMAC。Evaluator 使用固定短路顺序追加 Decision，Decision Head 通过 lineage advisory lock 单调推进。Release Verifier 在发布前重新锁定全部 current Head，旧 Decision 发生任何 source 变化时立即失效。

**Tech Stack:** Go 1.24、Gin、PostgreSQL 18、`database/sql`、`sqlmock`、RFC 8785、SHA256、HMAC-SHA256、Docker Compose。

## 范围与安全边界

- 本计划消费 lifecycle 计划的 frozen Pair Binding、writer protocol 与 Run state version，消费 revision pipeline 的 sealed Evidence、Score Head、Aggregate Head、cause set 与 outbox。
- migration 198 只允许 storage compatibility。缺少 migration 199 可信输入时求值固定为 `insufficient_evidence`，不能生成 Release Authorization。
- Decision 只持久化 `recorded|passed|blocked|review_required|insufficient_evidence`。`waived` 只存在于查询投影。
- Policy、Baseline、Decision、Waiver、Break Glass、ReleaseSubject、Reliability Snapshot 与 Authorization 审计均采用追加记录。
- 普通 Waiver 和 Break Glass 不能覆盖 Evidence integrity、Route Identity、tenant boundary、生产凭据、PITR 或回滚能力 hard stop。
- 每个任务先运行指定失败测试，观察预期 RED，完成最小实现后观察 GREEN，最后独立提交。

## 跨计划执行顺序

先完成 Worker 与 Run Lifecycle Tasks 1 至 9，再执行本计划 Tasks 1 至 2。随后执行 Evidence Revision Pipeline Task 1 与本计划 Task 3，完整装配 migration 199。接着执行 Evidence Revision Pipeline Tasks 2 至 8、本计划 Tasks 4 至 8、Evidence Revision Pipeline Task 9、本计划 Task 9。该顺序让 198 storage compatibility、199 trusted inputs 和发布验收保持明确边界。

## 任务依赖图

```text
Task 1 migration 198 governance storage
  -> Task 2 ReleaseSubject and Policy/Baseline Heads
  -> Task 3 migration 199 Reliability and Gate evidence schema
  -> Task 4 Evidence Loader and source watermark
  -> Task 5 deterministic Decision and supersession
  -> Task 6 Waiver and Break Glass
  -> Task 7 Release Authorization
  -> Task 8 Alert and degraded projection
  -> Task 9 cutover and 30-pair acceptance
```

### Task 1: 建立 migration 198 append-only governance storage

**Files:**
- Create: `backend/migrations/198_add_radar_trusted_governance.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Create: `backend/internal/repository/evaluation_governance_repo_integration_test.go`
- Test: `backend/internal/repository/migrations_schema_integration_test.go`

**Consumes:** migration 197 writer protocol、Score Head 兼容读取和 `route_profile_version`。

**Produces:** ReleaseSubject、Policy Head/Event、Baseline Head/Event、Decision Head/supersession、append-only Waiver 与 storage-only trusted mode guard。

- [ ] **Step 1: 写 migration 198 schema 失败测试**

```go
func TestMigration198TrustedGovernanceSchema(t *testing.T) {
    requireTable(t, db, "evaluation_release_subjects")
    requireTable(t, db, "evaluation_gate_policy_heads")
    requireTable(t, db, "evaluation_gate_policy_events")
    requireTable(t, db, "evaluation_baseline_heads")
    requireTable(t, db, "evaluation_baseline_events")
    requireTable(t, db, "evaluation_gate_decision_heads")
    requireTable(t, db, "evaluation_gate_decision_events")
    requireColumns(t, db, "evaluation_gate_decisions", []string{
        "release_subject_hash", "evidence_hash", "source_watermark",
        "supersedes_decision_id", "cause_set_hash",
    })
}
```

测试还要检查 Decision 自然键 `(run_id, policy_id, evidence_hash)`、Decision Head lineage key 和 `supersedes_decision_id` 非空唯一约束。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestMigration198TrustedGovernanceSchema -count=1`

Expected: FAIL，migration 198 对象不存在。

- [ ] **Step 3: 编写 expand schema 与 append-only trigger**

```sql
CREATE TABLE evaluation_gate_decision_heads (
  run_id uuid NOT NULL REFERENCES evaluation_runs(id),
  policy_id uuid NOT NULL REFERENCES evaluation_gate_policies(id),
  release_subject_hash char(64) NOT NULL,
  decision_id uuid NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
  PRIMARY KEY (run_id, policy_id, release_subject_hash)
);

CREATE UNIQUE INDEX uq_evaluation_gate_decisions_natural
  ON evaluation_gate_decisions (run_id, policy_id, evidence_hash);

CREATE UNIQUE INDEX uq_evaluation_gate_decisions_supersedes
  ON evaluation_gate_decisions (supersedes_decision_id)
  WHERE supersedes_decision_id IS NOT NULL;
```

Policy、Baseline、Decision、Waiver、ReleaseSubject 与 event 表安装 UPDATE/DELETE 拒绝 trigger。Head 只允许专用数据库函数按 expected current ID compare-and-set 推进。

- [ ] **Step 4: 写旧 mutable 行为拒绝测试**

断言 `UPDATE evaluation_gate_decisions SET status='waived'`、修改 activated Policy、UPDATE 旧 Baseline active 状态、修改 ReleaseSubject 和删除 superseded Decision 全部失败。

- [ ] **Step 5: 写 198 storage compatibility 测试**

在没有 migration 199 Evidence metadata 的数据库中创建 Decision 请求，断言结果固定为 `insufficient_evidence`，Release Authorization 表中没有记录。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'TestMigration198|TestTrustedGovernance.*Immutable|TestMigration198StorageCompatibility' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/migrations/198_add_radar_trusted_governance.sql backend/internal/repository/migrations_schema_integration_test.go backend/internal/repository/evaluation_governance_repo_integration_test.go
git commit -m "feat(radar): add trusted governance storage"
```

### Task 2: 实现 ReleaseSubject、Policy Head 与 Baseline Head

**Files:**
- Create: `backend/internal/service/evaluation_release_subject.go`
- Create: `backend/internal/service/evaluation_release_subject_test.go`
- Modify: `backend/internal/service/evaluation_governance.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_test.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Test: `backend/internal/repository/evaluation_governance_repo_test.go`

**Consumes:** Task 1 schema，frozen Run binding 与 Worker image digest。

**Produces:** canonical ReleaseSubject、immutable Policy/Baseline version、activation events、scope Head 和 reevaluation outbox。

- [ ] **Step 1: 写 canonical subject 与 Head 生命周期失败测试**

```go
func TestReleaseSubjectGoldenHashSortsDigestAndRegionSets(t *testing.T)
func TestReleaseSubjectHashChangesForAnyDeployableIdentityChange(t *testing.T)
func TestActivatePolicyAdvancesScopedHeadWithEvent(t *testing.T)
func TestActivateBaselineAdvancesRouteEnvironmentScopeHead(t *testing.T)
func TestPolicyAndBaselineHeadChangeEnqueuesActiveReleaseReevaluation(t *testing.T)
func TestGlobalScopeUsesCanonicalGlobalID(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(ReleaseSubject|ActivatePolicy|ActivateBaseline|PolicyAndBaseline|GlobalScope)' -count=1`

Expected: FAIL，canonical subject 或 Head Repository 尚未定义。

- [ ] **Step 3: 定义 ReleaseSubject 类型与 hash**

```go
type ReleaseSubject struct {
    CandidateModelConfigSHA256 string   `json:"candidate_model_config_sha256"`
    BaselineID                uuid.UUID `json:"baseline_id"`
    DatasetManifestSHA256     string    `json:"dataset_manifest_sha256"`
    RouteProfileVersion       string    `json:"route_profile_version"`
    GatewayImageDigest        string    `json:"gateway_image_digest"`
    ControlPlaneImageDigest   string    `json:"control_plane_image_digest"`
    RunnerImageDigests        []string  `json:"runner_image_digests"`
    GraderImageDigests        []string  `json:"grader_image_digests"`
    StatisticsImageDigests    []string  `json:"statistics_image_digests"`
    AnalysisVersion           string    `json:"analysis_version"`
    RegionSet                 []string  `json:"region_set"`
    DeploymentEnvironment     string    `json:"deployment_environment"`
    ScopeType                 string    `json:"scope_type"`
    ScopeID                   string    `json:"scope_id"`
}
```

集合去重排序后使用 RFC 8785 与 SHA256。外部提交的 hash 只用于 optimistic validation，服务端始终重算并保存 canonical bytes 与 hash。

- [ ] **Step 4: 实现 Policy/Baseline 激活事务**

Policy Head key 为 environment、scope type、canonical scope ID。Baseline Head 额外包含 comparison route key。激活事务锁定完整 key，校验批准主体、生效和到期时间，追加 event，推进 Head，插入相关 active Release reevaluation outbox。

- [ ] **Step 5: 暴露创建、激活、撤销 API**

API 使用独立 version create 与 Head action。历史回放路由只读，不能生成发布授权。Release Gate 请求中的 Policy 和 Baseline ID 必须命中 current Head。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin ./internal/server/routes -run 'Test(ReleaseSubject|ActivatePolicy|ActivateBaseline|PolicyAndBaseline|GlobalScope)' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_release_subject.go backend/internal/service/evaluation_release_subject_test.go backend/internal/service/evaluation_governance.go backend/internal/repository/evaluation_governance_repo.go backend/internal/repository/evaluation_governance_repo_test.go backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go backend/internal/server/routes/admin.go
git commit -m "feat(radar): version release governance heads"
```

### Task 3: 扩展 migration 199 Reliability 与 Gate evidence schema

**Files:**
- Modify: `backend/migrations/199_add_radar_evidence_revision_pipeline.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Create: `backend/internal/repository/evaluation_reliability_repo.go`
- Create: `backend/internal/repository/evaluation_reliability_repo_test.go`
- Create: `backend/internal/repository/evaluation_reliability_repo_integration_test.go`
- Test: `backend/internal/repository/evaluation_reliability_repo_test.go`

**Consumes:** Evidence Revision Pipeline Task 1 的 migration 199 基础 schema，Task 2 ReleaseSubject/Head。

**Produces:** immutable Reliability Snapshot/Head、Evidence Manifest storage、Release Authorization、Break Glass 与 Release projection schema。

- [ ] **Step 1: 写 Gate schema 失败测试**

```go
func TestMigration199GateEvidenceSchema(t *testing.T) {
    requireTable(t, db, "evaluation_reliability_snapshots")
    requireTable(t, db, "evaluation_reliability_heads")
    requireTable(t, db, "evaluation_gate_evidence_manifests")
    requireTable(t, db, "evaluation_release_authorizations")
    requireTable(t, db, "evaluation_break_glass_requests")
    requireTable(t, db, "evaluation_release_projections")
}
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestMigration199GateEvidenceSchema -count=1`

Expected: FAIL，Gate 子表尚未加入 migration 199。

- [ ] **Step 3: 扩展 migration 199**

Reliability 自然键固定为 Run、profile、slice、UTC window、source hash。Head key 固定为 Run、profile、slice。Evidence Manifest 保存 canonical bytes、evidence hash、source watermark、loader version、ReleaseSubject hash 与 cause set hash。Authorization 保存 Decision、Subject、watermark、waiver IDs、nonce、expiry 与 consumed_at。

- [ ] **Step 4: 实现 Reliability Publisher**

```go
type ReliabilitySnapshotInput struct {
    RunID          uuid.UUID
    ProfileID      string
    SliceKey       string
    WindowStart    time.Time
    WindowEnd      time.Time
    QueryVersion   string
    SourceHash     string
    Metrics        ReliabilityMetrics
    FreshUntil     time.Time
}
```

Publisher 在同一事务插入 Snapshot、推进 Head、写 Head event 与 Gate reevaluation outbox。窗口统一为 UTC 半开区间，客户取消独立计数，上游失败进入 error rate。

- [ ] **Step 5: 写 freshness 与样本量测试**

成功延迟切片少于 200、质量切片少于 30、required slice 缺失或过期均返回 sufficiency failure；ongoing confirmed P0 incident 返回 blocked signal。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'TestMigration199Gate|TestReliability' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/migrations/199_add_radar_evidence_revision_pipeline.sql backend/internal/repository/migrations_schema_integration_test.go backend/internal/repository/evaluation_reliability_repo.go backend/internal/repository/evaluation_reliability_repo_test.go backend/internal/repository/evaluation_reliability_repo_integration_test.go
git commit -m "feat(radar): add trusted gate evidence schema"
```

### Task 4: 实现 Evidence Loader 与完整 source_watermark

**Files:**
- Create: `backend/internal/service/evaluation_gate_evidence.go`
- Create: `backend/internal/service/evaluation_gate_evidence_test.go`
- Create: `backend/internal/repository/evaluation_gate_evidence_loader.go`
- Create: `backend/internal/repository/evaluation_gate_evidence_loader_test.go`
- Create: `backend/internal/repository/evaluation_gate_evidence_loader_integration_test.go`
- Modify: `backend/internal/service/evaluation_gate_service.go`
- Test: `backend/internal/repository/evaluation_gate_evidence_loader_test.go`

**Consumes:** current Policy/Baseline Heads、frozen bindings、sealed Evidence、eligible Score Heads、matching Aggregate Heads、current Reliability Heads 与 ReleaseSubject。

**Produces:** repeatable-read Evidence Manifest、独立 hash/HMAC 验证、stable evidence hash、完整 source watermark 与有限 insufficiency reasons。

- [ ] **Step 1: 写 Loader golden 与负向失败测试**

```go
func TestGateEvidenceManifestGoldenHashAndWatermark(t *testing.T)
func TestGateEvidenceLoaderRejectsLegacyUnboundRun(t *testing.T)
func TestGateEvidenceLoaderRecomputesManifestSemanticsAndEvidenceHashes(t *testing.T)
func TestGateEvidenceLoaderRejectsRevokedOrInvalidHMAC(t *testing.T)
func TestGateEvidenceLoaderRequiresEveryExpectedCellAndGlobal(t *testing.T)
func TestUnrelatedDatabaseWriteDoesNotChangeSourceWatermark(t *testing.T)
func TestAssignmentKeySubjectOrReliabilityChangeChangesWatermark(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'TestGateEvidence|TestUnrelatedDatabase|TestAssignmentKeySubject' -count=1`

Expected: FAIL，Loader 仍信任存储 hash 或缺完整 watermark。

- [ ] **Step 3: 定义 Manifest 类型与稳定排序**

```go
type GateEvidenceManifest struct {
    Policy                 PolicyEvidence          `json:"policy"`
    Baseline               BaselineEvidence        `json:"baseline"`
    Run                    RunEvidence             `json:"run"`
    PairBindings           []PairBindingEvidence   `json:"pair_bindings"`
    Assignments            []AssignmentEvidence    `json:"assignments"`
    Scores                 []ScoreHeadEvidence      `json:"scores"`
    CellSnapshots          []AggregateHeadEvidence `json:"cell_snapshots"`
    GlobalSnapshot         *AggregateHeadEvidence  `json:"global_snapshot"`
    RouteEvidence          []RouteEvidenceRef       `json:"route_evidence"`
    SigningKeys            []SigningKeyEvidence    `json:"signing_keys"`
    Reliability            []ReliabilityEvidence   `json:"reliability"`
    WorkerJobs             []WorkerJobEvidence     `json:"worker_jobs"`
    ReleaseSubjectHash     string                  `json:"release_subject_hash"`
    LoaderVersion          string                  `json:"loader_version"`
    SourceWatermark        string                  `json:"source_watermark"`
}
```

每个集合按 G1 14.3 的领域主键排序，可选 global 显式编码 null。Manifest 不可表达 Prompt、Completion、token、真实账号、真实渠道或上游正文。

- [ ] **Step 4: 实现 repeatable-read Loader**

事务使用 read-only repeatable-read。Loader 从 canonical bytes 重算 manifest、PairSpec、SideSpec、Binding 与 Request Semantics hash，从数据库字段重建 RouteEvidenceEnvelope 并重算 SHA256/HMAC。任何缺行、类型错误、ordinal 缺失、stale Head、HMAC 失败或 required reliability 缺失均产生有限 reason。

- [ ] **Step 5: 实现 source watermark**

watermark 输入包含 Run state version、ReleaseSubject hash、Policy/Baseline Head identity 与 activation event、AssignmentRef 和 epoch、eligible Score Head set、完整 ScoreRef、Evidence set、Request Manifest/Semantics、cell/global SnapshotRef、revision/hash/cause set、sealed Evidence revision/hash、signing key state epoch、Reliability SnapshotRef/hash 和 Loader version。严禁使用全局 transaction ID。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'TestGateEvidence|TestUnrelatedDatabase|TestAssignmentKeySubject' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_gate_evidence.go backend/internal/service/evaluation_gate_evidence_test.go backend/internal/repository/evaluation_gate_evidence_loader.go backend/internal/repository/evaluation_gate_evidence_loader_test.go backend/internal/repository/evaluation_gate_evidence_loader_integration_test.go backend/internal/service/evaluation_gate_service.go
git commit -m "feat(radar): load verifiable gate evidence"
```

### Task 5: 实现固定求值顺序、Decision supersession 与 Decision Head

**Files:**
- Modify: `backend/internal/service/evaluation_gate_service.go`
- Modify: `backend/internal/service/evaluation_governance.go`
- Create: `backend/internal/service/evaluation_gate_service_test.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_test.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Test: `backend/internal/service/evaluation_gate_service_test.go`

**Consumes:** Task 4 canonical Evidence Manifest 与 insufficiency reasons。

**Produces:** deterministic Gate status、Decision natural idempotency、lineage supersession 与 current Decision Head。

- [ ] **Step 1: 写求值优先级和并发失败测试**

```go
func TestGateEvaluationShortCircuitOrder(t *testing.T)
func TestRecordOnlyStillBlocksP0RouteReliabilityAndInsufficiency(t *testing.T)
func TestDecisionNaturalKeyReturnsExistingDecision(t *testing.T)
func TestDecisionSupersessionNeverUpdatesPriorDecision(t *testing.T)
func TestConcurrentDecisionWritersCannotForkLineage(t *testing.T)
func TestScoreHeadAdvanceMakesOldDecisionImmediatelyStale(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'Test(GateEvaluation|RecordOnly|Decision|ConcurrentDecision|ScoreHeadAdvance)' -count=1`

Expected: FAIL，旧求值路径缺 Head 或会更新现有 Decision。

- [ ] **Step 3: 实现固定短路 evaluator**

```go
var gateRuleOrder = []RuleClass{
    EvidenceSufficiency,
    P0Integrity,
    RouteIdentity,
    ReliabilitySLO,
    RecordOnlyObservation,
    CriticalDomainQuality,
    GlobalQuality,
    JudgeDisagreement,
    NegativeTrend,
    Pass,
}
```

规则输出有限 rule ID、status、metric refs 与 reason code。record-only 只抑制统计质量阻断，P0、Route Identity、Reliability 和 sufficiency 始终执行。

- [ ] **Step 4: 实现追加 Decision 与 Head 推进**

Repository 对 `(run_id, policy_id, release_subject_hash)` 获取事务级 advisory lock，重读 Decision Head，按 `(run_id, policy_id, evidence_hash)` 幂等插入新 Decision，写 supersession event 并推进 Head。prior Decision 永不更新。

- [ ] **Step 5: 收窄 HTTP 合同**

Gate evaluate API 只接收 Run ID、current Policy ID、ReleaseSubject ID 与 idempotency key。Evidence、状态和 watermark 全由服务端加载。历史 replay 使用独立只读路由，响应明确 `authorization_eligible=false`。

- [ ] **Step 6: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin -run 'Test(GateEvaluation|RecordOnly|Decision|ConcurrentDecision|ScoreHeadAdvance)' -count=1`

Expected: PASS。

- [ ] **Step 7: 提交本任务**

```bash
git add backend/internal/service/evaluation_gate_service.go backend/internal/service/evaluation_gate_service_test.go backend/internal/service/evaluation_governance.go backend/internal/repository/evaluation_governance_repo.go backend/internal/repository/evaluation_governance_repo_test.go backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go
git commit -m "feat(radar): supersede trusted gate decisions"
```

### Task 6: 实现普通 Waiver 与 Break Glass hard stop

**Files:**
- Create: `backend/internal/service/evaluation_release_exception.go`
- Create: `backend/internal/service/evaluation_release_exception_test.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo_test.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Test: `backend/internal/service/evaluation_release_exception_test.go`

**Consumes:** Task 5 immutable current Decision 与 ReleaseSubject。

**Produces:** append-only Waiver、四眼审批、到期投影、三主体 Break Glass、不可绕过 hard stop。

- [ ] **Step 1: 写豁免边界失败测试**

```go
func TestWaiverNeverMutatesDecisionStatus(t *testing.T)
func TestWaiverRequiresRiskOwnerDifferentFromReleaseManager(t *testing.T)
func TestWaiverExpiresOnTimeDecisionOrSubjectChange(t *testing.T)
func TestWaiverRejectsHardStopRules(t *testing.T)
func TestBreakGlassRequiresThreeDistinctApprovers(t *testing.T)
func TestBreakGlassCannotOverrideIntegrityIdentityTenantPITROrRollback(t *testing.T)
func TestP0BreakGlassOnlyAllowsRollbackToTrustedExistingRelease(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin -run 'Test(Waiver|BreakGlass|P0BreakGlass)' -count=1`

Expected: FAIL，旧 Waiver 仍可能更新 Decision status 或缺 hard stop。

- [ ] **Step 3: 实现 append-only Waiver 投影**

Waiver 固定 Decision、允许 rule IDs、原因、风险负责人、缓解措施、复测计划、ReleaseSubject、到期时间与批准人。查询时验证 current Decision Head、Subject、expiry 和 retest deadline，动态生成 waived projection。

- [ ] **Step 4: 实现 Break Glass 工作流**

Break Glass 固定事件编号、短时效、回滚目标、Platform Admin、Release Manager、安全负责人和自动告警 event。审批主体必须不同。rule classifier 在写入前执行 hard stop，无法识别的 rule 默认拒绝。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin -run 'Test(Waiver|BreakGlass|P0BreakGlass)' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/service/evaluation_release_exception.go backend/internal/service/evaluation_release_exception_test.go backend/internal/repository/evaluation_governance_repo.go backend/internal/repository/evaluation_governance_repo_test.go backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/handler/admin/evaluation_governance_handler_test.go backend/internal/server/routes/admin.go
git commit -m "feat(radar): enforce release exception boundaries"
```

### Task 7: 实现 Release Verifier 与单次 Authorization

**Files:**
- Create: `backend/internal/service/evaluation_release_verifier.go`
- Create: `backend/internal/service/evaluation_release_verifier_test.go`
- Create: `backend/internal/repository/evaluation_release_repo.go`
- Create: `backend/internal/repository/evaluation_release_repo_test.go`
- Modify: `backend/internal/handler/admin/evaluation_governance_handler.go`
- Modify: `backend/internal/server/routes/admin.go`
- Test: `backend/internal/service/evaluation_release_verifier_test.go`

**Consumes:** current Policy/Baseline/Score/Aggregate/Reliability/Decision Heads，Task 6 有效 exception 投影。

**Produces:** 发布前 Head 复核、短时效 nonce Authorization、deployment outbox、executor compare-and-set consumption。

- [ ] **Step 1: 写 stale 与消费失败测试**

```go
func TestReleaseVerifierRejectsDecisionWhenAnyHeadChanged(t *testing.T)
func TestReleaseVerifierRejectsSubjectMismatchOrExpiredEvidence(t *testing.T)
func TestReleaseVerifierIncludesOnlyCurrentlyValidWaivers(t *testing.T)
func TestReleaseAuthorizationIsShortLivedAndSingleUse(t *testing.T)
func TestDeploymentExecutorRechecksHeadsBeforeConsumption(t *testing.T)
func TestDeploymentCannotConsumeDecisionDirectly(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'TestRelease|TestDeployment' -count=1`

Expected: FAIL，Verifier 或 Authorization 尚未定义。

- [ ] **Step 3: 实现同事务 Verifier**

Verifier 先锁 Release，再按稳定顺序锁 current Policy、Baseline、Score、Aggregate、Reliability 和 Decision Heads，验证 Decision 固定的 IDs、hashes、activation events、SnapshotRefs 与 source watermark 全部仍 current。freshness 在事务时间判定。

```go
type ReleaseAuthorization struct {
    ID                 uuid.UUID
    DecisionID         uuid.UUID
    ReleaseSubjectHash string
    SourceWatermark    string
    WaiverIDs          []uuid.UUID
    NonceHash          string
    IssuedAt           time.Time
    ExpiresAt          time.Time
}
```

- [ ] **Step 4: 写 Authorization 与 deployment outbox**

通过复核后，同事务追加短时效 Authorization 与 deployment outbox。明文 nonce 只返回一次，数据库保存 hash。executor 在外部变更前重新验证 Head，使用 `consumed_at IS NULL` compare-and-set 消费。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'TestRelease|TestDeployment' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/service/evaluation_release_verifier.go backend/internal/service/evaluation_release_verifier_test.go backend/internal/repository/evaluation_release_repo.go backend/internal/repository/evaluation_release_repo_test.go backend/internal/handler/admin/evaluation_governance_handler.go backend/internal/server/routes/admin.go
git commit -m "feat(radar): authorize verified releases"
```

### Task 8: 接通 Alert、Decision supersession 与 degraded Release projection

**Files:**
- Modify: `backend/internal/service/evaluation_alert_service.go`
- Create: `backend/internal/service/evaluation_release_projection.go`
- Create: `backend/internal/service/evaluation_release_projection_test.go`
- Modify: `backend/internal/repository/evaluation_outbox_repo.go`
- Modify: `backend/internal/repository/evaluation_release_repo.go`
- Modify: `backend/internal/repository/evaluation_release_repo_test.go`
- Modify: `backend/internal/integration/radar_p0_e2e_test.go`
- Test: `backend/internal/service/evaluation_release_projection_test.go`

**Consumes:** revision pipeline cause outbox，Task 5 Decision Head，Task 7 active Release。

**Produces:** Decision ID 幂等 Alert/projection、Head 变化时旧 Decision 失效、degraded Release 与可追踪 causation chain。

- [ ] **Step 1: 写 projection 失败测试**

```go
func TestBlockedDecisionCreatesAlertAndDegradesMatchingRelease(t *testing.T)
func TestInsufficientEvidenceDecisionDegradesDeployedRelease(t *testing.T)
func TestPassedDecisionDoesNotClearDegradedWithoutRecoveryAction(t *testing.T)
func TestProjectionRetryIsIdempotentByDecisionAndProjectionType(t *testing.T)
func TestScoreHeadToDecisionAlertReleasePreservesCauseClosure(t *testing.T)
func TestHeadChangesInvalidateAuthorizationBeforeNewDecisionExists(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/integration -run 'Test(BlockedDecision|InsufficientEvidenceDecision|PassedDecision|ProjectionRetry|ScoreHeadToDecision|HeadChanges)' -count=1`

Expected: FAIL，旧投影依赖可变 Decision 或没有完整 cause。

- [ ] **Step 3: 实现 projection consumer**

projection dedup key 固定 Decision ID 与 projection type。blocked 或 insufficient evidence 使匹配已部署 Release 进入 degraded，并创建 Alert。passed 不自动解除 degraded，需要显式 recovery action 与新 Authorization。

- [ ] **Step 4: 实现旧 Decision 即时失效检查**

任何 Policy、Baseline、Score、Aggregate、Reliability、signing key、Assignment 或 ReleaseSubject Head/source 变化时，Verifier 在新 Decision 产生前也会拒绝旧 Authorization，并将 current Gate projection 显示为 insufficient evidence。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/integration -run 'Test(BlockedDecision|InsufficientEvidenceDecision|PassedDecision|ProjectionRetry|ScoreHeadToDecision|HeadChanges)' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交本任务**

```bash
git add backend/internal/service/evaluation_alert_service.go backend/internal/service/evaluation_release_projection.go backend/internal/service/evaluation_release_projection_test.go backend/internal/repository/evaluation_outbox_repo.go backend/internal/repository/evaluation_release_repo.go backend/internal/repository/evaluation_release_repo_test.go backend/internal/integration/radar_p0_e2e_test.go
git commit -m "feat(radar): degrade stale model releases"
```

### Task 9: 完成 198/199 cutover 与 30-pair deterministic Gate 验收

**Files:**
- Create: `backend/scripts/radar_migration_198_cutover.sh`
- Modify: `backend/scripts/radar_migration_199_cutover.sh`
- Create: `backend/internal/integration/radar_trusted_gate_e2e_test.go`
- Create: `backend/internal/integration/radar_gate_30_pair_acceptance_test.go`
- Create: `docs/superpowers/evidence/radar-g1-trusted-gate-verification.md`
- Modify: `docs/radar-staging-runbook.md`
- Test: `backend/internal/integration/radar_gate_30_pair_acceptance_test.go`

**Consumes:** Tasks 1 至 8，生命周期和 Evidence Revision Pipeline 的 cutover 产物。

**Produces:** writer quiescence 证明、30-pair deterministic Gate、stale Decision 拒绝、Authorization 与 degraded projection 的可复核证据。

- [ ] **Step 1: 写 cutover 失败测试**

198 在 Run start、Gate evaluate、Policy、Baseline、Waiver、Release writer 或 evaluation lease 非零时不得 closed。199 额外要求 Gateway、replacement、Grader、Statistics、reaper、scheduler 与 outbox consumer 全部归零。

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/integration -run 'TestRadarTrustedGateE2E|TestRadarGate30PairAcceptance' -count=1`

Expected: FAIL，cutover 或 deterministic fixture 尚未完成。

- [ ] **Step 3: 实现 198/199 cutover 脚本**

脚本进入 draining 后读取 writer sessions、active lease、数据库事务和 advisory lock 的真实计数。归零后由 migration owner 获取全局 lock，closed，替换约束与 protocol version，执行 schema acceptance，reopen。任一步失败保持 closed。

- [ ] **Step 4: 建立 30-pair golden fixture**

fixture 固定 30 个有效 pair、两侧 sealed Evidence、ScoreRef、cell/global SnapshotRef、Reliability Snapshot、Policy、Baseline、ReleaseSubject 和 expected Decision。运行两次必须得到相同 manifest bytes、watermark、evidence hash、Decision status 与 rule results。

- [ ] **Step 5: 覆盖关键负向矩阵**

逐一篡改 Pair Binding、Request Semantics、Evidence hash/HMAC、signing key state、AssignmentRef、Score Head、Aggregate Head、Reliability freshness、Policy Head、Baseline Head、ReleaseSubject，断言旧 Decision 无法授权。再覆盖普通 Waiver hard stop、Break Glass 三主体和 Authorization 重复消费。

- [ ] **Step 6: 运行全量验证**

Run: `cd backend && gofmt -w internal/service/evaluation_* internal/repository/evaluation_* internal/handler/admin/evaluation_governance_handler.go && go test ./internal/service ./internal/repository ./internal/handler/admin ./internal/server/routes ./internal/integration -count=1`

Expected: PASS。

- [ ] **Step 7: 重复 deterministic acceptance 并确认 GREEN**

Run: `cd backend && go test ./internal/integration -run TestRadarGate30PairAcceptance -count=20`

Expected: PASS，20 次输出 hash 和 verdict 完全一致。

- [ ] **Step 8: 写验证证据**

证据记录 commit、198/199 migration checksum、writer protocol versions、30-pair manifest/evidence hashes、每个负向案例的有限 reason、Authorization 消费结果和 degraded projection。不得记录 Prompt、Completion、token、HMAC key、真实账号或渠道。

- [ ] **Step 9: 提交本任务**

```bash
git add backend/scripts/radar_migration_198_cutover.sh backend/scripts/radar_migration_199_cutover.sh backend/internal/integration/radar_trusted_gate_e2e_test.go backend/internal/integration/radar_gate_30_pair_acceptance_test.go docs/superpowers/evidence/radar-g1-trusted-gate-verification.md docs/radar-staging-runbook.md
git commit -m "test(radar): prove deterministic trusted gate"
```

## 完成标准

- migration 198 只启用 append-only storage compatibility，migration 199 完成前不能产生发布 Authorization。
- ReleaseSubject 的部署对象、环境、镜像、baseline、dataset、route profile 和 analysis identity 全部进入 canonical hash。
- Policy、Baseline、Reliability 与 Decision current 状态全部由 Head 表达，历史版本保持不可变。
- Evidence Loader 在一致快照中独立复算 manifest、semantics、Envelope hash/HMAC、Score/Aggregate refs 和完整 source watermark。
- 任一 current source 变化都会让旧 Decision 立即失去发布资格，且无关数据库写入不改变 watermark。
- Decision supersession 不更新旧 Decision，完整 lineage key 在并发求值下不会分叉。
- Waiver 与 Break Glass 不能覆盖 hard stop，P0 Break Glass 只允许回滚到已有可信 Release。
- Deployment executor 只消费短时效、单次 Authorization，并在外部变更前再次验证所有 Head。
- 新 blocked 或 insufficient evidence Decision 会创建 Alert，并将匹配已部署 Release 投影为 degraded。
- 30-pair fixture 连续运行得到稳定 hash 与 verdict，198/199 cutover 证明所有相关 writer、lease、事务和 lock 已归零。
