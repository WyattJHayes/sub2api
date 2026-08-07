# Radar 精调训练与模型产物集成实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付从受控数据上传、SFT/DPO/RLHF 训练、checkpoint 恢复、模型产物签名到自动 Radar Gate 的完整后训练链路。

**Architecture:** Go Training Control Plane 保存数据、任务、Attempt、Checkpoint、Artifact 和资源账本的不可变事实。独立 Python `training-worker` 通过 fenced lease 执行算法插件，模型权重与数据存放于隔离对象存储。任务成功后控制面根据已批准 baseline 和 evaluation policy 创建 candidate Release Subject 与 Radar Run，发布投影只引用可信 Gate Decision。

**Tech Stack:** Go 1.26.5、Gin、PostgreSQL 18、`database/sql`、S3 兼容对象存储、Python 3.12、PyTorch 2.x、Transformers 4.x、TRL、Pydantic、pytest、Vue 3、TypeScript、Vitest。

## Global Constraints

- 先完成 migration 197 至 200，训练 schema 使用 migration 201。
- Dataset、Training、Checkpoint 和 Model Artifact manifest 使用 RFC 8785 canonical JSON、SHA256 和追加写存储。
- 训练 Worker 不能写 Radar Score、Aggregate、Reliability Snapshot、Gate Decision 或发布状态。
- 每次启动、抢占和恢复创建新 attempt，所有写入携带 `task_id + attempt_no + lease_epoch`。
- 数据、任务、GPU 账本、Artifact、Radar Run 和发布投影必须带 `tenant_id` 与 scope 校验。
- 原始训练数据、Prompt、Completion、凭据、对象路径和隐藏推理不能进入 Dashboard、日志或指标标签。
- 治理 manifest 保留 400 天，原始训练对象按租户批准策略保留，删除操作必须写入不可变 deletion event。
- 首发算法为 SFT；DPO 与 RLHF 使用同一插件合同并在本计划内完成可执行 Adapter 与恢复测试。
- 每个任务先观察 RED，再实现最小闭环，随后观察 GREEN 并独立提交。

## File Structure

- `backend/migrations/201_add_radar_training_control_plane.sql` 保存训练控制面 schema。
- `backend/internal/service/evaluation_training.go` 定义训练领域合同、状态机和权限。
- `backend/internal/repository/evaluation_training_repo.go` 实现 Dataset、Task、Attempt、Checkpoint、Artifact 与账本事务。
- `backend/internal/handler/admin/evaluation_training_handler.go` 暴露管理 API。
- `backend/internal/handler/internal/radar_training_worker_handler.go` 暴露私有 Worker lease API。
- `training-worker/` 独立承载 PyTorch、Transformers 和 TRL 依赖。
- `frontend/src/features/radar-training/` 提供数据、任务、产物和 Gate 状态视图。

---

### Task 1: 建立 migration 201 训练控制面 schema

**Files:**
- Create: `backend/migrations/201_add_radar_training_control_plane.sql`
- Modify: `backend/internal/repository/migrations_schema_integration_test.go`
- Create: `backend/internal/repository/evaluation_training_repo_integration_test.go`

**Interfaces:**
- Consumes: tenant、users、migration 199 Release Subject 与 outbox、对象 artifact 基础设施。
- Produces: Dataset、Training Task、Attempt、Checkpoint、Model Artifact、GPU Ledger、Radar Acceptance Link。

- [ ] **Step 1: 写 migration 201 失败测试**

```go
func TestMigration201TrainingSchema(t *testing.T) {
    requireTable(t, db, "training_datasets")
    requireTable(t, db, "training_dataset_events")
    requireTable(t, db, "training_tasks")
    requireTable(t, db, "training_task_events")
    requireTable(t, db, "training_attempts")
    requireTable(t, db, "training_checkpoints")
    requireTable(t, db, "training_model_artifacts")
    requireTable(t, db, "training_gpu_ledger")
    requireTable(t, db, "training_radar_acceptances")
}
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository -run TestMigration201TrainingSchema -count=1`

Expected: FAIL，migration 201 表不存在。

- [ ] **Step 3: 创建 Dataset 与 Task schema**

```sql
CREATE TABLE IF NOT EXISTS training_datasets (
  id uuid PRIMARY KEY,
  tenant_id bigint NOT NULL,
  schema_version text NOT NULL CHECK (schema_version = 'radar-training-dataset-v1'),
  canonical_manifest_bytes bytea NOT NULL,
  dataset_manifest_sha256 char(64) NOT NULL,
  status text NOT NULL CHECK (status IN ('uploaded','scanning','rejected','approved','frozen','deleted')),
  object_manifest_sha256 char(64) NOT NULL,
  approval_event_id uuid,
  created_by bigint NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
  UNIQUE (tenant_id, dataset_manifest_sha256)
);

CREATE TABLE IF NOT EXISTS training_tasks (
  id uuid PRIMARY KEY,
  tenant_id bigint NOT NULL,
  schema_version text NOT NULL CHECK (schema_version = 'radar-training-manifest-v1'),
  canonical_manifest_bytes bytea NOT NULL,
  training_manifest_sha256 char(64) NOT NULL,
  dataset_id uuid NOT NULL REFERENCES training_datasets(id),
  algorithm text NOT NULL CHECK (algorithm IN ('sft','dpo','rlhf')),
  status text NOT NULL CHECK (status IN ('draft','queued','running','paused','validating','succeeded','failed','cancelled')),
  control_epoch bigint NOT NULL DEFAULT 0,
  budget_amount numeric(20,8) NOT NULL CHECK (budget_amount > 0),
  created_by bigint NOT NULL REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
  UNIQUE (tenant_id, training_manifest_sha256),
  UNIQUE (id, tenant_id)
);
```

同一 migration 创建 append-only events、Attempt partial unique lease、Checkpoint lineage、Artifact digest、GPU debit/release 配对约束、Radar Acceptance 幂等键、租户复合外键和 immutable trigger。

- [ ] **Step 4: 写数据库拒绝测试**

验证跨租户 Dataset 引用、修改 manifest、恢复到未验证 Checkpoint、重复 GPU release、旧 epoch Attempt 完成、成功任务被覆盖、相同 acceptance key 创建两个 Run 均失败。

- [ ] **Step 5: 运行 schema 测试并确认 GREEN**

Run: `cd backend && go test ./internal/repository -run 'TestMigration201|TestTrainingSchema' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/migrations/201_add_radar_training_control_plane.sql backend/internal/repository/migrations_schema_integration_test.go backend/internal/repository/evaluation_training_repo_integration_test.go
git commit -m "feat(training): add immutable training control schema"
```

### Task 2: 实现数据上传、扫描、切分和批准

**Files:**
- Create: `backend/internal/service/evaluation_training.go`
- Create: `backend/internal/service/evaluation_training_test.go`
- Modify: `backend/internal/service/evaluation_rbac.go`
- Modify: `backend/internal/service/evaluation_rbac_test.go`
- Create: `backend/internal/repository/evaluation_training_repo.go`
- Create: `backend/internal/repository/evaluation_training_repo_test.go`
- Create: `backend/internal/handler/admin/evaluation_training_handler.go`
- Create: `backend/internal/handler/admin/evaluation_training_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`
- Modify: `backend/internal/server/routes/radar_governance_routes_test.go`
- Modify: `backend/internal/handler/handler.go`
- Modify: `backend/internal/repository/wire.go`
- Modify: `backend/internal/service/wire.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/cmd/server/wire_gen.go`

**Interfaces:**
- Consumes: 对象存储 presign/HEAD、malware/PII scanner、Radar RBAC 与 tenant scope。
- Produces: `CreateTrainingDataset`、`ConfirmDatasetUpload`、`PublishDatasetScan`、`ApproveTrainingDataset`。

- [ ] **Step 1: 写 manifest 与扫描状态失败测试**

```go
func TestTrainingDatasetManifestRejectsLeakageAcrossSplits(t *testing.T)
func TestConfirmDatasetUploadRejectsObjectHashMismatch(t *testing.T)
func TestApproveTrainingDatasetRequiresCleanPIIAndPoisonReports(t *testing.T)
func TestTrainingDatasetAccessRejectsCrossTenantActor(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin -run 'Test.*TrainingDataset' -count=1`

Expected: FAIL，训练数据合同尚未定义。

- [ ] **Step 3: 定义 Dataset Manifest**

```go
type TrainingDatasetManifest struct {
    SchemaVersion string `json:"schema_version"`
    DatasetID uuid.UUID `json:"dataset_id"`
    TenantID int64 `json:"tenant_id"`
    ObjectManifestSHA256 string `json:"object_manifest_sha256"`
    Format string `json:"format"`
    SampleCount int64 `json:"sample_count"`
    LanguageDistributionHash string `json:"language_distribution_hash"`
    LengthDistributionHash string `json:"length_distribution_hash"`
    SafetyLabelDistributionHash string `json:"safety_label_distribution_hash"`
    DedupPolicyHash string `json:"dedup_policy_hash"`
    SplitManifestSHA256 string `json:"split_manifest_sha256"`
    PIIScanReportSHA256 string `json:"pii_scan_report_sha256"`
    PoisonScanReportSHA256 string `json:"poison_scan_report_sha256"`
    LicenseManifestSHA256 string `json:"license_manifest_sha256"`
    ApprovalEventID uuid.UUID `json:"approval_event_id"`
}
```

服务器验证对象 HEAD、大小、SHA256、JSONL schema、重复率、PII、秘密、恶意 payload、许可证和 split 泄漏。扫描结果保存 verifier ID、version、input hash 和有限 reason code。

- [ ] **Step 4: 注册受控管理路由**

```go
training.POST("/datasets", h.Admin.RadarTraining.CreateDataset)
training.POST("/datasets/:id/uploads:confirm", h.Admin.RadarTraining.ConfirmUpload)
training.POST("/datasets/:id/scans", h.Admin.RadarTraining.PublishScan)
training.POST("/datasets/:id/approve", h.Admin.RadarTraining.ApproveDataset)
training.GET("/datasets/:id", h.Admin.RadarTraining.GetDataset)
```

创建和扫描需要 `training_dataset_manage`，批准需要独立的 `quality_admin`，同一 actor 不能同时提交扫描和批准。

- [ ] **Step 5: 运行测试并确认 GREEN**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler/admin ./internal/server/routes -run 'Test.*TrainingDataset|TestRadarTrainingRoutes' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/internal/service/evaluation_training.go backend/internal/service/evaluation_training_test.go backend/internal/service/evaluation_rbac.go backend/internal/service/evaluation_rbac_test.go backend/internal/repository/evaluation_training_repo.go backend/internal/repository/evaluation_training_repo_test.go backend/internal/handler/admin/evaluation_training_handler.go backend/internal/handler/admin/evaluation_training_handler_test.go backend/internal/server/routes/admin.go backend/internal/server/routes/radar_governance_routes_test.go backend/internal/handler/handler.go backend/internal/repository/wire.go backend/internal/service/wire.go backend/internal/handler/wire.go backend/cmd/server/wire_gen.go
git commit -m "feat(training): govern datasets and scan approvals"
```

### Task 3: 实现 Training Task、Attempt、lease 与资源账本

**Files:**
- Modify: `backend/internal/service/evaluation_training.go`
- Modify: `backend/internal/repository/evaluation_training_repo.go`
- Modify: `backend/internal/repository/evaluation_training_repo_integration_test.go`
- Create: `backend/internal/handler/internal/radar_training_worker_handler.go`
- Create: `backend/internal/handler/internal/radar_training_worker_handler_test.go`
- Create: `backend/internal/server/routes/radar_training_worker.go`
- Create: `backend/internal/server/routes/radar_training_worker_test.go`
- Modify: `backend/internal/handler/handler.go`
- Modify: `backend/internal/handler/wire.go`
- Modify: `backend/internal/server/router.go`
- Modify: `backend/cmd/server/wire_gen.go`

**Interfaces:**
- Consumes: approved Dataset、算法插件 registry、GPU quota、预算和 Worker workload identity。
- Produces: `CreateTrainingTask`、`ClaimTrainingAttempt`、`HeartbeatTrainingAttempt`、`PauseTrainingTask`、`CompleteTrainingAttempt`、`FailTrainingAttempt`。

- [ ] **Step 1: 写状态机与 fencing 失败测试**

```go
func TestTrainingTaskStateMachine(t *testing.T)
func TestClaimTrainingAttemptUsesSkipLocked(t *testing.T)
func TestOldTrainingLeaseEpochCannotHeartbeatOrComplete(t *testing.T)
func TestGPUAllocationAndReleaseAreExactlyOnce(t *testing.T)
func TestBudgetExhaustionStopsLowPriorityClaim(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal -run 'Test.*Training(Task|Attempt|GPU|Budget)' -count=1`

Expected: FAIL，Attempt lease 尚未实现。

- [ ] **Step 3: 定义 manifest 与 lease DTO**

```go
type TrainingManifest struct {
    SchemaVersion string `json:"schema_version"`
    TrainingTaskID uuid.UUID `json:"training_task_id"`
    TenantID int64 `json:"tenant_id"`
    Algorithm string `json:"algorithm"`
    AlgorithmPluginID string `json:"algorithm_plugin_id"`
    AlgorithmPluginVersion string `json:"algorithm_plugin_version"`
    BaseModelArtifactDigest string `json:"base_model_artifact_digest"`
    DatasetManifestSHA256 string `json:"dataset_manifest_sha256"`
    CodeRevision string `json:"code_revision"`
    RuntimeImageDigest string `json:"runtime_image_digest"`
    HardwareTopology string `json:"hardware_topology"`
    HyperparametersHash string `json:"hyperparameters_hash"`
    RandomSeed int64 `json:"random_seed"`
    EvaluationPolicyID uuid.UUID `json:"evaluation_policy_id"`
    BudgetAmount decimal.Decimal `json:"budget_amount"`
}

type TrainingLease struct {
    TaskID uuid.UUID `json:"task_id"`
    AttemptNo int `json:"attempt_no"`
    LeaseToken string `json:"lease_token"`
    LeaseEpoch int64 `json:"lease_epoch"`
    LeaseExpiresAt time.Time `json:"lease_expires_at"`
    Manifest TrainingManifest `json:"manifest"`
    ResumeCheckpoint *CheckpointRef `json:"resume_checkpoint,omitempty"`
}
```

- [ ] **Step 4: 实现锁序与资源事务**

claim 锁定 Task、Attempt、GPU quota 和预算行，使用 `FOR UPDATE SKIP LOCKED`。成功事务创建 Attempt、递增 attempt_no、保存 lease hash、写 GPU allocation 和任务事件。可恢复 failure 在三次以内把 Task 送回 queued 并创建新 attempt，超过三次或不可恢复 failure 把 Task 转为 failed。complete/fail 先验证 worker、token、expiry、epoch 和 Task 状态，再写 release 账本。

- [ ] **Step 5: 注册私有 Worker 路由**

```go
worker.POST("/training-leases:claim", h.ClaimTrainingAttempt)
worker.POST("/training-leases/:id/heartbeat", h.HeartbeatTrainingAttempt)
worker.POST("/training-leases/:id/checkpoints", h.SubmitCheckpoint)
worker.POST("/training-leases/:id/complete", h.CompleteTrainingAttempt)
worker.POST("/training-leases/:id/fail", h.FailTrainingAttempt)
```

- [ ] **Step 6: 运行集成测试并提交**

Run: `cd backend && go test ./internal/repository ./internal/handler/internal ./internal/server/routes -run 'Test.*Training' -count=1`

Expected: PASS。

```bash
git add backend/internal/service/evaluation_training.go backend/internal/repository/evaluation_training_repo.go backend/internal/repository/evaluation_training_repo_integration_test.go backend/internal/handler/internal/radar_training_worker_handler.go backend/internal/handler/internal/radar_training_worker_handler_test.go backend/internal/server/routes/radar_training_worker.go backend/internal/server/routes/radar_training_worker_test.go backend/internal/handler/handler.go backend/internal/handler/wire.go backend/internal/server/router.go backend/cmd/server/wire_gen.go
git commit -m "feat(training): add fenced training attempts and gpu ledger"
```

### Task 4: 建立独立 Training Worker 与 SFT/DPO/RLHF 插件

**Files:**
- Create: `training-worker/pyproject.toml`
- Create: `training-worker/Dockerfile`
- Create: `training-worker/src/sub2api_training/__init__.py`
- Create: `training-worker/src/sub2api_training/config.py`
- Create: `training-worker/src/sub2api_training/control_plane.py`
- Create: `training-worker/src/sub2api_training/models.py`
- Create: `training-worker/src/sub2api_training/runner.py`
- Create: `training-worker/src/sub2api_training/plugins/base.py`
- Create: `training-worker/src/sub2api_training/plugins/sft.py`
- Create: `training-worker/src/sub2api_training/plugins/dpo.py`
- Create: `training-worker/src/sub2api_training/plugins/rlhf.py`
- Create: `training-worker/tests/test_plugins.py`
- Create: `training-worker/tests/test_runner_recovery.py`

**Interfaces:**
- Consumes: Task 3 `TrainingLease`、verified Dataset artifact、base model digest 和 checkpoint ref。
- Produces: 训练指标、checkpoint submission、资源用量和终态 Attempt result。

- [ ] **Step 1: 写插件合同失败测试**

```python
@pytest.mark.parametrize("algorithm", ["sft", "dpo", "rlhf"])
def test_registered_plugin_builds_reproducible_command(algorithm: str) -> None:
    plugin = registry.get(algorithm)
    first = plugin.build_command(fixture_manifest(algorithm))
    second = plugin.build_command(fixture_manifest(algorithm))
    assert first == second
    assert "--seed" in first
    assert "--dataset-manifest-sha256" in first

async def test_heartbeat_loss_stops_training_and_reports_fenced() -> None:
    runner = TrainingRunner(client=fenced_client(), executor=fake_executor())
    result = await runner.run_once()
    assert result == RunResult.FENCED
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd training-worker && uv run pytest -q`

Expected: FAIL，training-worker 包不存在。

- [ ] **Step 3: 创建依赖与严格配置**

```toml
[project]
name = "sub2api-training-worker"
requires-python = ">=3.12,<3.15"
dependencies = [
  "httpx>=0.28,<0.29",
  "pydantic>=2.11,<3",
  "pydantic-settings>=2.9,<3",
  "torch>=2.7,<3",
  "transformers>=4.53,<5",
  "datasets>=4,<5",
  "trl>=0.19,<1",
]

[project.optional-dependencies]
dev = [
  "pytest>=8.4,<9",
  "pytest-asyncio>=1.0,<2",
  "respx>=0.22,<0.23",
  "ruff>=0.12,<0.13",
  "mypy>=1.16,<2",
]
```

配置要求 Worker token、control plane URL、image digest、region、checkpoint 临时目录和对象存储受控 endpoint。日志过滤 Authorization、对象 URL query 和数据正文。

- [ ] **Step 4: 实现统一插件协议**

```python
class TrainingPlugin(Protocol):
    algorithm: Literal["sft", "dpo", "rlhf"]
    def validate(self, manifest: TrainingManifest) -> None: ...
    def build_command(self, manifest: TrainingManifest) -> tuple[str, ...]: ...
    def checkpoint_candidates(self, workdir: Path) -> tuple[Path, ...]: ...
    def validate_output(self, workdir: Path) -> ModelOutput: ...
```

SFT 使用 supervised labels；DPO 要求 chosen/rejected pair；RLHF 要求 reward model digest、KL policy 和 rollout budget。三个插件都固定 seed、镜像、代码 revision、Dataset hash 和依赖 lock hash。

- [ ] **Step 5: 实现 crash-safe runner**

Runner 将 lease 写入本地 state，心跳失败立即终止进程树。收到 checkpoint 信号时先同步文件，再计算 SHA256 并调用 presign/confirm。重启只恢复控制面返回的 verified checkpoint。

- [ ] **Step 6: 运行 Python 质量门并提交**

Run: `cd training-worker && uv run pytest -q && uv run ruff check src tests && uv run mypy src`

Expected: PASS。

```bash
git add training-worker
git commit -m "feat(training): add sft dpo and rlhf worker plugins"
```

### Task 5: 实现 Checkpoint、Model Artifact、签名与模型仓库

**Files:**
- Modify: `backend/internal/service/evaluation_training.go`
- Modify: `backend/internal/repository/evaluation_training_repo.go`
- Create: `backend/internal/repository/evaluation_training_artifact_repo.go`
- Create: `backend/internal/repository/evaluation_training_artifact_repo_test.go`
- Modify: `backend/internal/handler/internal/radar_training_worker_handler.go`
- Create: `backend/internal/handler/admin/evaluation_model_registry_handler.go`
- Modify: `backend/internal/server/routes/admin.go`
- Create: `training-worker/src/sub2api_training/artifacts.py`
- Create: `training-worker/tests/test_artifacts.py`

**Interfaces:**
- Consumes: Attempt lease、对象存储、signing key service、Smoke runtime。
- Produces: verified CheckpointRef、`radar-model-artifact-v1`、Model Registry 状态。

- [ ] **Step 1: 写 artifact 完整性失败测试**

```go
func TestConfirmTrainingCheckpointRejectsDigestMismatch(t *testing.T)
func TestCheckpointParentMustBelongToSameTask(t *testing.T)
func TestModelArtifactRequiresTokenizerConfigSBOMAndSignature(t *testing.T)
func TestRevokedSigningKeyInvalidatesCurrentModelArtifact(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/repository ./internal/handler -run 'Test.*(Checkpoint|ModelArtifact)' -count=1`

Expected: FAIL，Artifact repository 尚未实现。

- [ ] **Step 3: 定义 Model Artifact**

```go
type ModelArtifactManifest struct {
    SchemaVersion string `json:"schema_version"`
    ArtifactID uuid.UUID `json:"artifact_id"`
    TenantID int64 `json:"tenant_id"`
    ParentModelArtifactDigest string `json:"parent_model_artifact_digest"`
    TrainingManifestSHA256 string `json:"training_manifest_sha256"`
    CheckpointLineage []string `json:"checkpoint_lineage"`
    WeightsDigest string `json:"weights_digest"`
    TokenizerDigest string `json:"tokenizer_digest"`
    ConfigDigest string `json:"config_digest"`
    RuntimeImageDigest string `json:"runtime_image_digest"`
    SBOMDigest string `json:"sbom_digest"`
    SignatureRef string `json:"signature_ref"`
    ValidationReportSHA256 string `json:"validation_report_sha256"`
    ArtifactDigest string `json:"artifact_digest"`
}
```

- [ ] **Step 4: 实现上传与验证顺序**

每个对象执行 quarantine upload、HEAD、大小、SHA256、malware scan、signature verify 和 retention event。Model Artifact 还要完成加载、Tokenizer、配置、量化和固定 Prompt Smoke，验证报告加入 Artifact digest。

- [ ] **Step 5: 运行 Go 与 Python 测试**

Run: `cd backend && go test ./internal/repository ./internal/handler -run 'Test.*(Checkpoint|ModelArtifact)' -count=1`

Run: `cd training-worker && uv run pytest tests/test_artifacts.py -q`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/internal/service/evaluation_training.go backend/internal/repository/evaluation_training_repo.go backend/internal/repository/evaluation_training_artifact_repo.go backend/internal/repository/evaluation_training_artifact_repo_test.go backend/internal/handler/internal/radar_training_worker_handler.go backend/internal/handler/admin/evaluation_model_registry_handler.go backend/internal/server/routes/admin.go training-worker/src/sub2api_training/artifacts.py training-worker/tests/test_artifacts.py
git commit -m "feat(training): verify checkpoints and signed model artifacts"
```

### Task 6: 自动创建 Release Subject、Radar Run 与发布投影

**Files:**
- Create: `backend/internal/service/evaluation_training_acceptance.go`
- Create: `backend/internal/service/evaluation_training_acceptance_test.go`
- Modify: `backend/internal/repository/evaluation_training_repo.go`
- Create: `backend/internal/repository/evaluation_training_acceptance_repo_test.go`
- Modify: `backend/internal/service/evaluation_management.go`
- Modify: `backend/internal/repository/evaluation_management_repo.go`
- Modify: `backend/internal/repository/evaluation_governance_repo.go`

**Interfaces:**
- Consumes: succeeded Task、validated Artifact、approved Baseline Head、evaluation policy 和 Radar Run API。
- Produces: candidate Release Subject、baseline/candidate Run、Acceptance Link、Gate Decision projection。

- [ ] **Step 1: 写自动验收幂等失败测试**

```go
func TestTrainingAcceptanceKeyIsStable(t *testing.T) {
    key := TrainingAcceptanceKey(trainingHash, artifactDigest, policyID, baselineID)
    require.Len(t, key, 64)
    require.Equal(t, key, TrainingAcceptanceKey(trainingHash, artifactDigest, policyID, baselineID))
}

func TestTrainingCannotChooseUnapprovedBaseline(t *testing.T)
func TestTrainingAcceptanceCreatesOneRadarRunPerKey(t *testing.T)
func TestTrainingProjectionCannotOverrideGateDecision(t *testing.T)
```

- [ ] **Step 2: 运行测试并确认 RED**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'TestTrainingAcceptance|TestTrainingProjection' -count=1`

Expected: FAIL，Acceptance Orchestrator 不存在。

- [ ] **Step 3: 实现服务端编排**

```go
func (s *TrainingAcceptanceService) Start(
    ctx context.Context,
    taskID uuid.UUID,
) (*TrainingRadarAcceptance, error)
```

事务加载 succeeded Task、current validated Artifact、current Baseline Head 和 Policy。它生成 Release Subject，固定 Dataset/Pair/Side/Request Manifest，然后用 acceptance key 创建 Run。训练调用方只传 Task ID。

- [ ] **Step 4: 传播 Gate 结果**

Gate Head 变化通过 outbox 更新 `training_radar_acceptances.decision_ref` 与只读发布投影。blocked、insufficient evidence 和 expired Decision 都不能进入 `release_eligible`。

- [ ] **Step 5: 运行服务与仓储测试**

Run: `cd backend && go test ./internal/service ./internal/repository -run 'TestTrainingAcceptance|TestTrainingProjection' -count=1`

Expected: PASS。

- [ ] **Step 6: 提交任务**

```bash
git add backend/internal/service/evaluation_training_acceptance.go backend/internal/service/evaluation_training_acceptance_test.go backend/internal/repository/evaluation_training_repo.go backend/internal/repository/evaluation_training_acceptance_repo_test.go backend/internal/service/evaluation_management.go backend/internal/repository/evaluation_management_repo.go backend/internal/repository/evaluation_governance_repo.go
git commit -m "feat(training): gate model artifacts through radar"
```

### Task 7: 建立训练控制台与端到端恢复验收

**Files:**
- Create: `frontend/src/features/radar-training/api.ts`
- Create: `frontend/src/features/radar-training/types.ts`
- Create: `frontend/src/features/radar-training/TrainingDatasetsView.vue`
- Create: `frontend/src/features/radar-training/TrainingTasksView.vue`
- Create: `frontend/src/features/radar-training/ModelArtifactsView.vue`
- Create: `frontend/src/features/radar-training/__tests__/TrainingFlow.spec.ts`
- Modify: `frontend/src/router/index.ts`
- Modify: `frontend/src/api/admin/radar.ts`
- Create: `backend/internal/integration/radar_training_e2e_test.go`
- Modify: `deploy/docker-compose.radar-staging.yml`
- Create: `deploy/radar/training/sft-smoke-manifest.json`
- Modify: `docs/radar-production-runbook.md`

**Interfaces:**
- Consumes: Tasks 1 至 6 API、独立 training-worker、staging 对象存储和 synthetic model runtime。
- Produces: 数据上传到 blocked Gate 的可视化流程、checkpoint 恢复报告和审计证据。

- [ ] **Step 1: 写前端权限与流程失败测试**

```ts
it('keeps artifact release disabled until current gate passes', async () => {
  server.use(mockTrainingTask({ status: 'succeeded', gate_status: 'blocked' }))
  const wrapper = mount(TrainingTasksView, testApp())
  await flushPromises()
  expect(wrapper.get('[data-test="release-model"]').attributes('disabled')).toBeDefined()
})
```

测试 Viewer 无上传按钮、Quality Admin 可批准 Dataset、Test Operator 可启动任务、Release Manager 才能批准发布。

- [ ] **Step 2: 写后端 E2E 失败测试**

```go
func TestTrainingSFTCheckpointRecoveryAndRadarGate(t *testing.T) {
    dataset := uploadScannedDataset(t, fixtureWithPIIAndDuplicates())
    approved := approveRevisedDataset(t, dataset.ID)
    task := startSmallSFT(t, approved.ID)
    checkpoint := interruptAndRecoverTrainingWorker(t, task.ID)
    require.Equal(t, "verified", checkpoint.Status)
    artifact := completeSignedModelArtifact(t, task.ID)
    acceptance := waitForTrainingRadarAcceptance(t, artifact.ID)
    require.Equal(t, "blocked", acceptance.GateStatus)
}
```

- [ ] **Step 3: 运行测试并确认 RED**

Run: `cd frontend && npm run test:run -- frontend/src/features/radar-training/__tests__/TrainingFlow.spec.ts`

Run: `cd backend && go test ./internal/integration -run TestTrainingSFTCheckpointRecoveryAndRadarGate -count=1 -v`

Expected: FAIL，UI、staging Worker 或 fixture 尚未装配。

- [ ] **Step 4: 实现三页控制台与审计链接**

页面提供 Dataset 扫描状态、Task attempt 时间线、Checkpoint lineage、GPU/费用账本、Artifact digest 和 current Gate。危险动作使用确认对话框、RBAC 和 correlation ID，不显示原始数据或对象 URL。

- [ ] **Step 5: 执行全量验收**

Run: `cd backend && go test ./internal/service ./internal/repository ./internal/handler ./internal/server/routes ./internal/integration -run 'Test.*Training' -count=1`

Run: `cd training-worker && uv run pytest -q && uv run ruff check src tests && uv run mypy src`

Run: `cd frontend && npm run test:run -- frontend/src/features/radar-training/__tests__/TrainingFlow.spec.ts && npm run typecheck`

Run: `docker compose -f deploy/docker-compose.radar-staging.yml --profile training config --quiet`

Expected: 全部 PASS，PII 数据被拒绝、checkpoint 恢复成功、GPU 账本归零、模型产物可验签、Radar Run 完整、退化候选被 Gate 阻断。

- [ ] **Step 6: 提交任务**

```bash
git add frontend/src/features/radar-training frontend/src/router/index.ts frontend/src/api/admin/radar.ts backend/internal/integration/radar_training_e2e_test.go deploy/docker-compose.radar-staging.yml deploy/radar/training/sft-smoke-manifest.json docs/radar-production-runbook.md
git commit -m "test(training): verify upload training recovery and radar gate"
```

## 完成标准

1. 数据上传、扫描、切分、批准、训练、Checkpoint、Artifact 和 Gate 全部可由 manifest hash 串联。
2. SFT、DPO、RLHF 共用稳定插件协议，固定 seed、镜像、代码、Dataset 和依赖锁。
3. Worker 中断后只从 verified checkpoint 恢复，旧 epoch 无法写入。
4. GPU 与费用账本对每个 attempt 完整配对，预算硬上限能停止新工作。
5. Model Artifact 具备权重、Tokenizer、配置、SBOM、签名和 Smoke 验证。
6. 训练成功自动创建 baseline/candidate Radar Run，训练服务无法写评分或通过结论。
7. 跨租户数据、Artifact、Baseline、Run 和发布投影访问全部被拒绝。
