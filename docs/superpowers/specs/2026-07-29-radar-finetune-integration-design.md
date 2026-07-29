# Sub2API 精调训练与模型质量雷达集成设计规格

- 状态：P3 独立工作包设计
- 日期：2026-07-29
- 目标平台：`sub2api`
- 依赖：G1 可信执行合同、全平台模型质量雷达设计总包

## 1. 设计目标

本工作包把数据上传、数据治理、训练任务、模型产物和发布前质量验证串成一条可追溯链。训练控制面负责训练事实，Radar 控制面负责评测事实，二者通过不可变 manifest 和 `Release Subject` 连接。

训练成功只表示任务完成。模型进入候选发布状态还需要通过能力、可靠性、安全、性能和成本 Gate。

### 1.1 范围

1. 数据上传、扫描、去重、切分、泄漏检查和人工批准。
2. SFT、DPO、RLHF 及后续训练算法的统一任务接口。
3. GPU 调度、checkpoint、断点恢复、模型仓库和产物签名。
4. 自动创建 candidate Release Subject 和 baseline/candidate Radar Run。
5. 训练 lineage、失败分类、预算和租户隔离。

### 1.2 非目标

1. 训练 Worker 不能直接写 Radar Score、Aggregate 或 Gate Decision。
2. Radar 不能改变训练超参数、覆盖 checkpoint 或批准训练任务。
3. 本规格不规定某一训练框架的内部优化算法，算法通过版本化插件接入。

## 2. 分层架构

| 层 | 组件 | 事实边界 |
| --- | --- | --- |
| 数据层 | Upload API、Scanner、Dataset Registry | 数据 manifest、扫描结果、批准事件 |
| 编排层 | Training Control Plane、Scheduler、Attempt Manager | 任务状态、资源账本、attempt 和 checkpoint |
| 执行层 | SFT/DPO/RLHF Plugin、Training Worker | 指标、日志、checkpoint 和训练运行事实 |
| 产物层 | Model Registry、Signer、SBOM 服务 | artifact digest、兼容性、签名和 lineage |
| 质量层 | Radar Run、Worker、Grader、Statistics、Gate | Score、Aggregate、Reliability、Decision |

Training Control Plane 与 Radar Control Plane 使用服务身份双向认证。训练服务只能调用 Dataset、Artifact 和 Run API，Radar 服务只能读取经过授权的 manifest 和模型引用。

## 3. 数据 manifest 合同

数据 manifest 使用 RFC 8785 canonical JSON 计算 `dataset_manifest_sha256`。原始数据正文存放在隔离对象存储，控制面只保存受控引用、摘要和 hash。

```json
{
  "schema_version": "radar-training-dataset-v1",
  "dataset_id": "...",
  "tenant_id": "...",
  "object_manifest_sha256": "...",
  "format": "jsonl",
  "sample_count": 0,
  "language_distribution_hash": "...",
  "length_distribution_hash": "...",
  "safety_label_distribution_hash": "...",
  "dedup_policy_hash": "...",
  "split_manifest_sha256": "...",
  "pii_scan_report_sha256": "...",
  "poison_scan_report_sha256": "...",
  "license_manifest_sha256": "...",
  "approval_event_id": "..."
}
```

上传、扫描、切分和批准分别产生追加事件。扫描失败、批准撤销或对象 hash 变化会冻结 Dataset 版本，并拒绝进入训练任务。

### 3.1 数据质量门禁

最低检查包括格式与编码、字段 schema、重复率、空样本、PII、秘密、恶意 payload、许可证、train/validation/test 泄漏和安全标签分布。每条规则保存 verifier ID、版本、输入 hash、结果和时间。发现风险时保留失败样本的受控 artifact 引用，报告与 Dashboard 只显示脱敏摘要。

## 4. Training Task 合同

训练任务由不可变 `TrainingManifest` 标识。任何超参数、代码、镜像、硬件拓扑、随机种子或父模型变化都产生新 manifest。

```json
{
  "schema_version": "radar-training-manifest-v1",
  "training_task_id": "...",
  "tenant_id": "...",
  "algorithm": "sft",
  "algorithm_plugin_id": "sft-transformers",
  "algorithm_plugin_version": "...",
  "base_model_artifact_digest": "sha256:...",
  "dataset_manifest_sha256": "...",
  "code_revision": "...",
  "runtime_image_digest": "sha256:...",
  "hardware_topology": "8xGPU",
  "hyperparameters_hash": "...",
  "random_seed": 0,
  "evaluation_policy_id": "...",
  "budget_amount": "100.00000000",
  "training_manifest_sha256": "..."
}
```

控制面服务端重新计算 `training_manifest_sha256`，客户端提交的值只用于一致性校验。任务执行期间不能修改 manifest 内容，调整配置必须创建新的 attempt 或新的任务。

## 5. 任务状态与 attempt

### 5.1 Training Task 状态

```text
draft -> queued -> running -> validating -> succeeded
                    |          |
                    v          v
                  paused      failed
                    |
                    +--> queued
```

`cancelled` 可以从 `draft`、`queued` 或 `paused` 进入。`succeeded`、`failed` 和 `cancelled` 为终态。终态任务不能被覆盖，补跑创建新的任务或受控 revision。

### 5.2 Attempt 合同

每次启动和恢复都创建单调递增的 `attempt_no`。Attempt 保存 Worker identity、lease、GPU 分配、开始结束时间、退出原因、资源消耗和 checkpoint refs。旧 attempt 只读，恢复 attempt 必须引用某个已验证的 checkpoint digest。

Worker 心跳、日志上传、checkpoint 提交和状态变更都携带 `task_id + attempt_no + lease_epoch`。旧 epoch 的写入返回 `lease_fenced`，不能改变任务状态或资源账本。

## 6. Checkpoint 与资源账本

Checkpoint manifest 至少包括：

```text
checkpoint_id
training_task_id
attempt_no
step
parent_checkpoint_digest
weights_digest
optimizer_digest
tokenizer_digest
config_digest
runtime_image_digest
created_at
```

保存流程先上传临时对象，再执行 HEAD、SHA256、大小、签名和元数据校验，最后写入不可变 manifest。任务恢复只允许使用状态为 `verified` 的 checkpoint。对象缺失或 checksum 不符会使 Attempt 失败并触发训练基础设施告警。

GPU 资源账本按 `tenant_id`、任务、attempt、GPU 类型、数量、开始结束时间和金额记录。抢占、失败和恢复都必须生成配对的释放事件。达到预算硬上限后停止低优先级任务，保留经批准的 P0 训练验收。

## 7. Model Artifact 合同

模型产物经过加载、tokenizer、配置、量化和推理 Smoke 后才进入 `validated`。

```json
{
  "schema_version": "radar-model-artifact-v1",
  "artifact_id": "...",
  "tenant_id": "...",
  "parent_model_artifact_digest": "sha256:...",
  "training_manifest_sha256": "...",
  "checkpoint_lineage": ["..."],
  "weights_digest": "sha256:...",
  "tokenizer_digest": "sha256:...",
  "config_digest": "sha256:...",
  "runtime_image_digest": "sha256:...",
  "sbom_digest": "sha256:...",
  "signature_ref": "...",
  "validation_report_sha256": "...",
  "artifact_digest": "sha256:..."
}
```

`artifact_digest` 绑定全部前置 digest 和校验结果。任何权重、Tokenizer、配置、运行时或 SBOM 变化都要求新的 Artifact。模型仓库的下载权限按租户和 Release Subject scope 校验。

## 8. Radar 自动验收接入

训练任务进入 `succeeded` 后，训练控制面调用 Radar 的受控 Run API：

1. 创建 candidate `Release Subject`，绑定 `model_artifact_digest`、`training_manifest_sha256`、数据 manifest、运行时镜像和评测策略。
2. 从已批准的 baseline registry 读取 baseline artifact，不接受训练服务自报基线。
3. 创建固定 Dataset、Pair、Side 和 Request Manifest，候选 Side 只引用新 Artifact。
4. 由受控 Worker 通过 Gateway 执行 baseline/candidate，生成 Route Evidence、Score 和 Aggregate。
5. 由 Gate 同时读取能力、可靠性、安全和成本结果。
6. 将 Gate Decision ID 回写训练任务的发布投影，保持 Score 与 Decision 的只读引用。

训练服务只能提交 `artifact_id`、manifest hash 和 Radar Run 请求幂等键。它不能提交 score、aggregate、route evidence 或通过结论。

### 8.1 幂等键

自动验收使用：

```text
SHA256(training_manifest_sha256 || artifact_digest || evaluation_policy_id || baseline_id)
```

相同键返回原 Run；任一输入变化创建新 Run。训练任务重试不能重复扣费或生成重复 current Score。

## 9. 训练退化检测

Radar 质量报告至少分离以下维度：

| 维度 | 示例信号 | 结论 |
| --- | --- | --- |
| 能力遗忘 | 通用推理、数学、代码域下降 | `regression` |
| 任务过拟合 | 训练域上升，隐藏变体下降 | `regression` |
| 对齐变化 | 越狱成功率、拒答边界、工具越权 | `regression` 或 P0 |
| 产物兼容 | Tokenizer、配置、量化或加载失败 | `insufficient_evidence` 或 blocked |
| 资源退化 | TTFT、吞吐、显存和 checkpoint 时间 | `reliability_degradation` |

数据分布变化时，报告必须同时展示训练集、验证集、隐藏变体和历史 baseline。单一 loss 曲线不能作为发布结论。

## 10. 租户、安全与合规

1. 数据对象、训练任务、GPU 账本、模型 Artifact 和 Radar Run 全部带 `tenant_id`，服务端执行行级 scope 校验。
2. 训练 Worker 使用 workload identity 和短时效 token，不能读取生产 API Key 或 Radar signing key。
3. 日志、指标和训练报告禁止输出原始 Prompt、Completion、隐藏推理、凭据和原始对象路径。
4. 数据和模型对象使用独立加密密钥。删除操作从 manifest 引用计算，写入不可变 deletion event。
5. 跨租户基线引用必须经过显式批准，默认拒绝。

## 11. 验收矩阵

| 层级 | 最小证据 |
| --- | --- |
| 数据链路 | 上传、扫描、切分、泄漏检查、批准和删除事件可重放 |
| 训练状态 | 队列、运行、暂停、失败、取消和恢复状态机符合合同 |
| 资源恢复 | Worker 中断后 checkpoint 恢复，GPU 账本无悬挂分配 |
| 产物 | 权重、Tokenizer、配置、SBOM、签名和 Smoke 报告 hash 可复算 |
| Radar 接入 | 自动创建 candidate Release Subject 与 30-pair Run，结果可追溯 |
| Gate | 质量、可靠性、安全或成本失败分别阻断并保留原因 |
| 租户安全 | 跨租户数据、Artifact、基线和 Run 查询全部拒绝 |

### 11.1 最小 SFT 端到端验收

1. 上传含重复、PII、恶意字段和格式错误的受控数据。
2. 扫描拒绝风险样本，修订后生成新的 Dataset Manifest。
3. 启动小规模 SFT，注入一次 Worker 中断并从 verified checkpoint 恢复。
4. 产出签名模型，完成 tokenizer、配置和推理 Smoke。
5. 自动创建 baseline/candidate Radar Run，完成 30 个 paired samples。
6. 注入候选能力下降，Gate 阻断发布，训练任务保留完整 lineage。

## 12. 默认决策

| 项目 | 默认值 | 变更要求 |
| --- | --- | --- |
| 训练算法接入 | SFT 首发，DPO/RLHF 使用同一任务合同 | 需要算法插件评审 |
| Checkpoint 保留 | 每个成功任务至少保留首个、最佳和最终 checkpoint | 需要合规与成本评审 |
| 训练失败重试 | 新 attempt，最多三次，超过后人工处置 | 需要训练负责人批准 |
| 自动 Radar Run | 训练成功后自动创建，失败任务不创建 | 需要质量负责人批准 |
| 发布策略 | P3 前人工批准，Gate hard stop 不可豁免 | 需要 Release Manager 与安全负责人批准 |
| 数据保留 | manifest 400 天，原始训练对象按租户策略保留 | 需要合规评审 |
