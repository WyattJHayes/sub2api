# Radar 可信执行核心 G1 合同设计

- 状态：用户已于 2026-07-28 书面确认，进入实施计划阶段
- 日期：2026-07-27
- 继承规格：[sub2api 模型质量雷达设计规格](2026-07-25-sub2api-model-quality-radar-design.md)
- 适用阶段：G1 合同就绪、G2 staging 证明、G3 record-only
- 适用工作包：Radar 可信执行核心
- 权威关系：本补丁在可信执行核心范围内覆盖继承规格与总设计中的冲突条款

## 1. 文档目标

本设计补齐 Radar 可信执行核心的可执行合同。它解决五类会破坏评测结论可信度的问题。

1. 精确预算 Run 在优先级组合不同时可能无法领取任务。
2. 已完成的路由证据仍可被后写覆盖。
3. Score 修订后无法生成对应的 Aggregate revision。
4. Gate Decision 与真实发布主体、证据版本和后续修订缺少因果关系。
5. 取消、暂停、Worker 轮换和租约写入缺少统一 fencing 语义。

本设计只定义可信执行核心。企业租户治理、Benchmark 与安全 Adapter、性能与可靠性、精调训练、Agent 与插件评测分别拥有独立规格。它们统一消费本设计定义的 Case、Pair、Evidence、Score、Aggregate、Gate 和 Release Subject 合同。

## 2. 默认决策

本补丁采用以下默认决策。

1. Run 因取消或不可恢复失败进入终态时立即递增 `control_epoch`，所有已发出的 lease 立即失效。
2. 暂停 Run 时停止新 claim，已经领取的工作允许完成。
3. Route Evidence 通过显式 Finalize 操作封存，Gate 只消费 sealed evidence。
4. completed Run 允许追加 regrade，Run 终态保持不变，Score、Aggregate 和 Gate 产生新 revision。
5. 历史 Route Evidence、历史 Aggregate 和 `legacy-unbound` Run 不回填为可信记录。
6. Migration 198 只启用 Gate 追加存储兼容层。可信求值在 migration 199 后启用，enforcement 在 G2 与 G3 证据完成前保持关闭。
7. 普通 Waiver 不能覆盖 P0 安全、租户隔离、Route Identity、证据完整性和生产安全矩阵失败。
8. Release Verifier 每次发布都校验 current Policy Head、Baseline Head、Score Head、Aggregate Head、Reliability Head 和 Decision Head，任何过期 Decision 都不能继续授权发布。

## 3. 核心术语

| 术语 | 定义 |
| --- | --- |
| `PairSpec` | 冻结一对 baseline/candidate 的公共实验条件 |
| `SideSpec` | baseline 或 candidate 独有的目标配置 |
| `control_epoch` | Run 或 Revision Batch 控制版本，失败、取消或显式 fence 时递增 |
| `lease_epoch` | claim 时复制 owning Run 或 Revision Batch 的 `control_epoch` |
| sealed evidence | 身份、终态、计费和哈希已经封存的网关证据 |
| score head set | 某个分析单元当前使用的完整 eligible ScoreRef 集合 |
| cell revision | 一个能力域与一个 canonical model route 的 Aggregate 版本 |
| global revision | 覆盖当前全部 cell revision 的显式全局版本 |
| `ReleaseSubject` | Gate Decision 所绑定的可部署对象和环境身份 |
| supersession | 新证据产生新 Decision 后，旧 Decision 退出有效投影的追加式关系 |
| policy head | 某个部署环境与 scope 当前生效的不可变 Policy 版本指针 |
| decision head | 某个 Run、Policy lineage 与 Release Subject 当前有效的 Decision 指针 |
| baseline head | 某个 comparison route、环境与 scope 当前批准的 Baseline 指针 |
| `ScoreRef` | 分区 Score 的复合定位符 `(score_id, score_created_at)` |
| `SnapshotRef` | 分区 Aggregate Snapshot 的复合定位符 `(snapshot_id, window_start)` |
| revision batch | completed Run 上承载 regrade lease、独立 epoch 与审计状态的控制对象 |

## 4. 组件边界

### 4.1 管理控制面

管理控制面创建 Run、执行 pause、resume、cancel、Worker 注册、禁用与 token rotation。它只接受管理员身份和 Radar RBAC 权限，不接受调用方提交的状态结论、证据哈希或 Gate 计算结果。

### 4.2 Run Reconciler

Run Reconciler 是 Run 状态的唯一归约器。它在同一数据库事务中先锁定 Run，再读取当前有效 Assignment、Grading Job、Analysis Job、Score Head 和 Aggregate Head，执行一次单调状态迁移并追加事件。取消或不可恢复失败还会在同一事务递增 `control_epoch` 并处置全部非终态工作。

### 4.3 Gateway Evidence Finalizer

网关负责创建 open evidence、累积 transport 与 billing 字段、校验身份、计算 canonical payload hash，并通过 Finalize 操作写入终态与 `sealed_at`。Worker 无权封存 Route Evidence。

### 4.4 Grading 与 Statistics

Grader 追加 Score 并原子推进 Score Head。Statistics 只消费 Analysis Job 固化的输入集合，产出 immutable Aggregate Snapshot。Worker 提交内容不能选择 revision，也不能推进任意 Head。

### 4.5 Gate Evidence Loader

Gate Evidence Loader 在一致数据库快照中加载 current Policy/Baseline Head、Run、Pair/Side Binding、sealed Route Evidence、eligible Score Head、matching Aggregate revision、current Reliability Head 和 Release Subject。Loader 独立复算来源 hash 与 HMAC，再生成 canonical Evidence Manifest 和服务端 SHA256。

### 4.6 Release Verifier

发布控制器只接受已保存的 Decision ID。Verifier 重新加载 Decision、current Policy Head、current Baseline Head、current Score Head、current Aggregate Head、current Reliability Head、Decision Head、Waiver 和 Evidence Freshness，校验 `release_subject_hash` 与待部署对象完全一致。任一 current Head 晚于 Decision 固定的水位时立即拒绝。即使新 Decision 尚未写入，旧 Decision 仍然失效。

## 5. PairSpec 与 SideSpec

Baseline 与 Candidate 需要共享受控实验条件，同时允许目标模型或目标配置存在声明过的差异。

### 5.1 PairSpec

`PairSpec` 至少包含以下字段。

```text
dataset_version_id
case_id
sample_index
repeat_index
prompt_sha256
tool_schema_sha256
expected_request_manifest_id
expected_request_manifest_sha256
grader_id
grader_version
sampling_policy
random_seed
region
protocol
time_block
interleave_order
retry_policy
allowed_treatment_fields
```

这些字段使用 canonical JSON 编码并计算 `pair_spec_hash`。一对样本的 `pair_spec_hash` 必须相同。`allowed_treatment_fields` 只能取 Policy 允许的 SideSpec 字段子集，调用方不能自行扩大该集合。

### 5.2 SideSpec

`SideSpec` 至少包含以下字段。

```text
side
model_route
model_config_sha256
expected_model_alias
expected_resolved_model
route_profile_version
provider_parameters_sha256
```

`side` 是 baseline/candidate 判别字段，不计入 treatment 差异。`model_route` 使用 `baseline:<comparison_route_key>` 或 `candidate:<comparison_route_key>`，两侧去除一个前缀后必须得到相同 key。真实目标差异由 model config、expected alias、expected resolved model 和 provider parameters 表达。

默认 Policy 只允许 `model_config_sha256`、`expected_model_alias`、`expected_resolved_model` 与 `provider_parameters_sha256` 出现在 `allowed_treatment_fields`。region、protocol、sampling policy、retry policy 和 route profile 必须保持一致，专门研究这些变量时使用独立实验类型和 Policy。

每个 SideSpec 使用 canonical JSON 计算 `side_spec_hash`。Baseline 与 Candidate 只能在 `allowed_treatment_fields` 声明的字段上不同。任何额外差异令该 pair 进入 `invalid_pair`，不能进入质量 Aggregate。

`pair_binding_hash` 绑定 `pair_spec_hash`、baseline `side_spec_hash` 与 candidate `side_spec_hash`。Score、Aggregate 和 Gate Evidence 都引用该绑定，避免只冻结公共条件却遗漏真实处理差异。

### 5.3 配对失败规则

- 任一侧发生基础设施失败时，该 pair 不进入能力得分，失败进入可靠性分母。
- 任一侧发生上游失败时，按相同 retry policy 处理，完整 attempt 序列进入证据。
- 只有一侧重试成功时，Pair Evidence 保存非对称重试标记。
- 任一侧被取消时，该 pair 标记为 cancelled，不进入能力结论。
- Pair 排除必须保存有限 reason code，不能静默丢弃。

### 5.4 持久化合同

Migration 197 创建四类不可变记录。

```text
evaluation_request_manifests
  id, schema_version, interaction_type
  canonical_manifest_bytes, manifest_sha256, created_at
  UNIQUE (manifest_sha256)
  UNIQUE (id, manifest_sha256)

evaluation_pair_specs
  id, run_id, case_id, sample_index, repeat_index
  request_manifest_id, request_manifest_sha256
  schema_version, canonical_spec, pair_spec_hash, created_at
  UNIQUE (run_id, case_id, sample_index, repeat_index)

evaluation_side_specs
  id, pair_spec_id, sample_id, side
  schema_version, canonical_spec, side_spec_hash, created_at
  UNIQUE (pair_spec_id, side)
  UNIQUE (sample_id)

evaluation_pair_bindings
  id, pair_spec_id, baseline_side_spec_id, candidate_side_spec_id
  pair_binding_hash, created_at
  UNIQUE (pair_spec_id)
```

Request Manifest 首版 schema 为 `radar-request-manifest-v1`，canonical bytes 使用 RFC 8785。内容只保存以下字段和 hash，不保存 Prompt 或工具参数正文。

```text
interaction_type       single | multi_turn | agent
ordinal_policy         exact | contiguous_bounded
min_requests
max_requests
request_slots[]
  slot_id
  ordinal_min
  ordinal_max
  phase
  required
  semantics_mode            exact | adapter_policy
  expected_request_semantics_sha256
  request_semantics_policy_sha256
  tool_schema_sha256
  allowed_tool_set_sha256
  max_occurrences
```

single 模式固定 exact ordinal 0。multi_turn 可以声明 exact slots。agent 模式使用有限、连续的 ordinal 范围。slot ID 必须唯一，ordinal 范围按下界与上界排序且不能重叠，因此每个实际调用只能匹配一个 slot。`semantics_mode=exact` 时 expected hash 非空且 policy hash 为空；`semantics_mode=adapter_policy` 时规则相反。每个实际调用都必须满足总数、required slot 与 occurrence 上限。动态语义使用 Adapter 发布的 request semantics policy hash，Gate 通过对应的版本化 Adapter verifier 复算。

Run 创建事务先插入或引用 Request Manifest，再按 PairSpec、两条 SideSpec、Pair Binding 的顺序插入，最后开放 claim。PairSpec 的 manifest ID 与 manifest hash 必须通过复合 FK 指向同一行。SideSpec 的 sample FK 使用 deferred constraint 以支持同一事务批量创建。四类对象的 UPDATE 与 DELETE 都由数据库拒绝。Gate Loader 具有读取 canonical manifest bytes 的内部权限，独立复算 manifest、pair、side 与 binding hash。外部 API 只返回 ID 与 hash。历史 Run 缺少任一记录时标记为 `legacy-unbound`。

## 6. Run 创建与预算模式

### 6.1 新增字段

`evaluation_runs` 增加以下控制字段。

```text
budget_mode            normal | exact_p0_drain
paused_from_status     nullable run status
pause_reason           nullable reason code
control_epoch          bigint, default 0
state_version          bigint, default 0
cancelled_at           nullable timestamptz
cancelled_by           nullable user id
route_profile_version  varchar, legacy default legacy-unbound
```

数据库约束要求 `status='paused'` 时 `paused_from_status` 只能取 `pending`、`budget_paused` 或 `running`。其他状态下 `paused_from_status` 与 `pause_reason` 必须为空。进入任一终态时 `finished_at` 必填，cancel 还要求 `cancelled_at` 与 `cancelled_by` 完整。

### 6.2 budget_mode 导出

创建 Run 的事务从同一份 Case 集合计算总预留成本与优先级分布。

```text
exact_p0_drain =
  reserved_cost == budget_limit
  AND exists priority P0
  AND exists priority P1 or P2
```

只有 `exact_p0_drain` Run 以 `budget_paused` 创建。仅含 P0、仅含 P1/P2 或预算仍有余量的 Run 以 `pending` 创建。

### 6.3 领取规则

- `pending` 与 `running` 在预留成本覆盖范围内允许领取全部优先级。
- `budget_paused` 只允许领取 P0。
- `paused`、`completed`、`failed`、`cancelled` 不允许领取。
- claim 先锁定 Run，再锁定 Assignment，并把当前 `control_epoch` 写入 lease。
- 首次成功 claim 才设置 `started_at`。
- Reconciler 在锁定 Run 的事务中计算 P0 expected、successful、active 与 retryable 集合。只有 expected 非空且全部属于 successful 时，`budget_paused` 才能转为 `running`。
- P0 的最高 attempt 已完成 Sample、Assignment 和 required Score Head 才属于 successful。已 lease、已运行、等待评分或存在可用 replacement 的 P0 仍属于 active。
- 任一 P0 当前有效工作不可恢复失败或重试耗尽时，Run 进入 `failed`。
- pause 期间完成全部 P0 只记录 readiness，Run 保持 `paused`，resume 事务重新计算集合后转入 `running`。

## 7. Run 状态与控制语义

### 7.1 合法状态转换

| 当前状态 | 事件 | 下一状态 | 关键条件 |
| --- | --- | --- | --- |
| `pending` | first claim | `running` | budget mode 为 normal |
| `pending` | pause | `paused` | 保存原状态 |
| `pending` | cancel | `cancelled` | 立即 fence |
| `pending` | unrecoverable failure | `failed` | 立即 fence，setup 或依赖失败 |
| `budget_paused` | all P0 completed | `running` | 当前 P0 集合完整成功 |
| `budget_paused` | pause | `paused` | 保存原状态 |
| `budget_paused` | cancel | `cancelled` | 立即 fence |
| `budget_paused` | unrecoverable failure | `failed` | 立即 fence，当前有效工作失败 |
| `running` | pause | `paused` | 停止新 claim |
| `running` | all work complete | `completed` | current Aggregate 覆盖完整 |
| `running` | cancel | `cancelled` | 立即 fence |
| `running` | unrecoverable failure | `failed` | 立即 fence，failure-first 归约 |
| `paused` | resume | `pending` | 原状态 pending 且尚未开始 |
| `paused` | resume | `budget_paused` | exact P0 仍未清空 |
| `paused` | resume | `running` | 其余可恢复场景 |
| `paused` | resume with failure | `failed` | 立即 fence，期间结果含不可恢复失败 |
| `paused` | cancel | `cancelled` | 立即 fence |

`completed`、`failed`、`cancelled` 没有出边。Regrade 不改变 Run 终态。

### 7.2 pause

pause 只阻止新 claim，不递增 `control_epoch`。事务把当前 `pending`、`budget_paused` 或 `running` 写入 `paused_from_status`，并追加包含原状态的事件。已经领取的 Runner、Grader 与 Statistics 工作可以完成。Reconciler 在 paused 期间保留工作结果，不自动把 Run 转成完成或失败。

resume 不盲目恢复 `paused_from_status`。它在锁定 Run 后重新计算失败、P0 readiness 和 `started_at`，选择 `failed`、`budget_paused`、`pending` 或 `running`，随后清空 `paused_from_status` 与 `pause_reason`。重复 pause 或 resume 使用请求幂等键，不能重复追加事件。

### 7.3 cancel

cancel 在一个事务中完成以下动作。

1. 锁定 Run 并确认当前状态非终态。
2. `control_epoch` 加一。
3. Run 转为 `cancelled`，写入操作者与时间。
4. 所有非终态 Sample 与 Assignment 转为 `cancelled`，清理 lease 字段。
5. 所有非终态 Grading Job 与 Analysis Job 转为 `cancelled`，清理 lease 字段。
6. 写入网关取消 outbox，使仍为 open 的 Route Evidence 最终封存为 `client_cancelled`。outbox 延迟不影响 lease 立即失效，也不能让 open evidence 进入 Gate。
7. 清空 pause 字段并追加唯一状态转换事件。

旧 lease 后续 heartbeat、complete、fail、evidence confirm、score submit 和 aggregate submit 都返回 `lease_fenced`。

### 7.4 failure-first 归约

Reconciler 先检查不可恢复失败，再检查 pending 工作。历史 Assignment 的失败只有在它仍是 Sample 最高 attempt 且没有活跃 replacement 时才生效。Score 为零属于成功评分结果。

进入 `failed` 的事务与 cancel 使用相同锁顺序。它递增 `control_epoch`，将其余非终态工作转为 `cancelled`，清理 lease，保存根因 failure class 与 code，并只追加一个 `run_failed` 转换事件。事务同时写入网关终态化 outbox，按根因把 open Route Evidence 映射为 `upstream_failed`、`protocol_failed` 或 `gateway_failed`。sealed Evidence 保持不变，终态化超时触发完整性告警。所有 Worker 写路径都先锁定 Run 再锁定子任务，防止终态事务与完成事务形成反向锁序。

### 7.5 显式 fence

显式 fence 保持 Run 的业务状态，递增 `control_epoch` 并追加 `run_fenced` 事件。持有 Runner lease 的 Assignment 以有限 `fenced` failure code 结束；仍在 retry policy 与预算内时，同一事务创建更高 attempt 的 replacement，无法重试时 Run 进入 `failed`。

绑定被替代 Assignment 的 initial Grading Job 全部转为 `cancelled`，由这些 Score 构成且尚未完成的 Analysis Job 同样取消。绑定仍为 current Assignment 的 Grading Job 可以清理 lease 后回到 `pending`。`work_origin=regrade` 只有在其 source Assignment 仍为 current、Route Evidence 集合仍 sealed 且 set hash 匹配时才能回到 `pending`。Analysis Job 只有在 frozen ScoreRef 集合仍等于 eligible current Score Head 集合时才能回到 `pending`，其余 job 保留为 stale history。旧 epoch 的任意提交继续返回 `lease_fenced`。

### 7.6 Run Control API

pause、resume、cancel 与 fence 分别使用 `POST /api/v1/admin/radar/runs/:id/pause`、`resume`、`cancel` 和 `fence`。Migration 197 增加 `PermissionRunControl`，四个端点都要求该权限。actor 只来自认证上下文，请求必须包含 64 位 idempotency key 与有限 reason code。

响应返回 Run ID、原状态、新状态、原 epoch、新 epoch、受影响工作计数、replacement IDs 和 Event ID。terminal Run 上的 cancel 或 fence 返回 409，重复幂等请求返回原响应。审计记录保存 actor、scope、reason、epoch 和计数，不能保存 token、Prompt、Completion 或 Evidence payload。

## 8. Run Event 合同

`evaluation_run_events` 增加以下字段。

```text
transition_version  nullable bigint
from_status         nullable run status
to_status           nullable run status
control_epoch       bigint
idempotency_key     char(64)
payload             jsonb
```

约束如下。

- 状态变化时先递增 `evaluation_runs.state_version`，事件使用相同 `transition_version`。
- `(run_id, transition_version)` 在非空时唯一。
- `idempotency_key` 全局唯一。
- `run_fenced` 与 `budget_paused` 首次领取产生的 `run_started` 等非状态事件允许 `transition_version` 为空。pause、resume 与终态事件必须携带 transition version。
- 事件 payload 只保存有限元数据和 reason code，不保存 token、Prompt 或 Completion。
- 状态触发器拒绝未列入状态图的转换和终态重开。

唯一键只能证明每个 transition 至多一条事件。Migration 197 还创建 `DEFERRABLE INITIALLY DEFERRED` constraint trigger，在事务提交前验证每次 Run status 与 `state_version` 变化恰好存在一条匹配 `run_id`、`transition_version`、`from_status`、`to_status` 与 `control_epoch` 的事件。缺失、错配或重复都会让整个状态事务回滚。

## 9. Worker 身份与 Lease Fencing

### 9.1 Worker 注册

Worker 明文 token 至少 32 个字符，只存在于请求解码、请求缓冲和 SHA256 计算期间。数据库、审计、响应和日志只保存 hash 与前 12 位 fingerprint。

注册身份包含 name、kind、region、image digest 和 immutable capabilities。相同身份与相同 token hash 可幂等返回。同名且 kind 或 token hash 不同返回冲突。token rotation 使用独立端点。

### 9.2 Worker 控制动作

| 动作 | 新 claim | 在途 lease | token |
| --- | --- | --- | --- |
| pause claims | 禁止 | 允许完成 | 保持有效 |
| drain | 禁止 | 等待归零 | 保持有效 |
| disable | 禁止 | 立即拒绝 Worker 鉴权 | 保持 hash，仅不可用 |
| rotate token | 新 token 可用 | 旧 bearer 立即失效 | hash 原子替换 |
| fence Run | 禁止该 Run | epoch 不匹配 | Worker 身份仍可用于其他 Run |

恢复语义固定如下。

- pause claims 与 drain 不改变任何 lease。drain 在所有 lease 归零后完成。
- token rotation 保留 Worker ID 与现有 lease。后续 heartbeat 或提交必须同时使用新 bearer 和原 lease token。
- disable 立即使该 Worker 的 bearer 认证失败。lease reaper 清理其到期 lease，Runner 工作按 retry policy 创建 replacement。绑定被替代 Assignment 的下游工作取消；Grading 与 Analysis 工作只有满足 7.5 节的 current input 校验时才回到 pending。
- fence Run 按 7.5 节执行，不依赖 lease 自然到期。
- 每个控制动作写入不可变 Worker 或 Run Event，重复请求由 idempotency key 返回原结果。

### 9.3 Lease 字段

Assignment、Grading Job 和 Analysis Job 的 lease 保存以下字段。

```text
lease_token_hash
leased_by
lease_expires_at
lease_epoch
worker_image_digest
```

每次 heartbeat、complete 和 fail 同时验证 bearer identity、Worker active 状态、lease token、expiry、worker kind、job status 和 work origin。initial work 要求 `lease_epoch == run.control_epoch` 且 Run 状态允许该阶段提交；regrade work 要求 `lease_epoch == revision_batch.control_epoch`、Batch 为 running 且 Run 为 completed。任一检查失败都返回相同的 fenced 领域错误，日志不输出 token。

## 10. Route Evidence 封存

### 10.1 数据字段

`evaluation_route_evidence` 增加以下字段。

```text
schema_version
canonicalization_version
assignment_id
request_ordinal
lease_epoch
request_manifest_id
request_manifest_sha256
request_slot_id
request_semantics_id
request_semantics_sha256
request_semantics_policy_sha256
request_tool_schema_sha256
request_allowed_tool_set_sha256
evidence_revision
terminal_at
sealed_at
payload_hash
signing_key_id
payload_hmac
billing_status
gateway_image_digest
incomplete_reason
```

`(assignment_id, request_ordinal)` 唯一，ordinal 从 0 连续递增。单轮 benchmark 只有 ordinal 0，多轮 Agent 任务由 Adapter manifest 固定预期 ordinal 与调用语义。历史 retry attempt 保留各自的 Assignment 与 Route Trace。Gate 只选择 Sample current Assignment 的完整 sealed Evidence 集合，缺号、重复或额外调用都产生 `insufficient_evidence`。

`evaluation_request_semantics` 保存 `id`、schema version、canonical semantics bytes、`request_semantics_sha256` 与创建时间，并对 `(id, request_semantics_sha256)` 提供唯一键。canonical 对象只包含 interaction type、slot ID、request ordinal、phase、消息角色序列、内容 part 类型序列、Prompt hash、tool schema hash、实际提供工具集合 hash、tool choice policy、sampling policy hash 与前序 EvidenceRef，不保存 Prompt、工具参数或 Completion 正文。对象使用 RFC 8785，UPDATE 与 DELETE 由数据库拒绝，Gate Loader 具有受审计的内部读取权限。Route Evidence 的 semantics ID 与 hash 通过复合 FK 指向同一行。

open evidence 创建时复制 Assignment 的 `lease_epoch`，绑定 PairSpec 的 manifest ID 与 hash、唯一匹配的 slot、实际 Request Semantics 记录，并设置 `evidence_revision=1`。每次 transport 或 billing patch 都携带 expected revision，在行锁内按字段 merge matrix 合并并递增一次。Finalize 再递增一次并签署最终 revision。sealed 后 revision 永久不变。

网关必须在上游分发前通过 CreateOpen 创建完整 identity。CreateOpen 还要验证受控 Gateway service identity、非空 request ID、已注册 image digest、manifest hash 和 Request Semantics hash。网关从 PairSpec 加载 manifest canonical bytes 并复算 hash，按 ordinal 选出唯一 slot，再把实际请求归一为 canonical Request Semantics。exact slot 要求实际 semantics hash 与 expected hash 相等；adapter policy slot 要求注册 verifier 对 canonical semantics bytes 与 policy hash 返回通过。tool schema 与实际提供工具集合也必须和 slot 一致。任一失败都禁止上游分发，并在同一受控事务中创建和封存 `protocol_failed` Evidence。

CreateOpen、patch 与正常 Finalize 都先锁定 Run，再锁定 Assignment 和 evidence，验证 `lease_epoch == run.control_epoch`、Assignment 仍为 Sample current attempt，并允许 Run 处于 `running`、`budget_paused` 或 `paused`。transport 与 billing API 只允许 patch 已存在的 open row，不能通过 upsert 隐式创建记录或补写 identity。

### 10.2 不可变身份

以下字段从首次插入起不可修改。

```text
schema_version
canonicalization_version
route_trace_id
evaluation_run_id
sample_id
assignment_id
request_ordinal
lease_epoch
request_manifest_id
request_manifest_sha256
request_slot_id
request_semantics_id
request_semantics_sha256
request_semantics_policy_sha256
request_tool_schema_sha256
request_allowed_tool_set_sha256
api_key_id
request_id
requested_model
route_profile_version
gateway_image_digest
region
started_at
```

### 10.3 状态转换

Route Evidence 只允许从 `started` 转到以下终态之一。

```text
succeeded
upstream_failed
protocol_failed
client_cancelled
gateway_failed
```

`finished_at` 表示网关观察到 transport 结束的时间，`terminal_at` 表示 Repository 持久化终态的时间，`sealed_at` 表示 Envelope 完成签署的时间。三个时间必须单调不减。`billing_status` 只允许 `complete`、`not_applicable` 或 `incomplete`。

`succeeded` 还要求 resolved model、provider、latency、token usage、billing status、billed amount 和 finish reason 满足 Gate Policy 定义的完整性要求。`complete` 必须有 billed amount，`not_applicable` 必须由 Policy 显式允许，`incomplete` 不能进入可信 Gate。`attempts` 必须与 fallback chain 表达的实际 attempt 序列一致。无法取得某个允许缺失的字段时，使用有限 `incomplete_reason`，不能使用空字符串表达未知。

### 10.4 RouteEvidenceEnvelope

`RouteEvidenceEnvelope` 是 HMAC 的唯一输入对象，字段全集固定如下。

```text
schema_version
canonicalization_version
route_trace_id
evaluation_run_id
sample_id
assignment_id
request_ordinal
lease_epoch
request_manifest_id
request_manifest_sha256
request_slot_id
request_semantics_id
request_semantics_sha256
request_semantics_policy_sha256
request_tool_schema_sha256
request_allowed_tool_set_sha256
evidence_revision
api_key_id
request_id
requested_model
resolved_model
route_profile_version
gateway_image_digest
provider
channel_ref
account_pool_ref
region
attempts
fallback_chain
transport_status
error_code
finish_reason
input_tokens
output_tokens
ttft_ms
latency_ms
billing_status
billed_amount
incomplete_reason
started_at
finished_at
terminal_at
sealed_at
signing_key_id
```

`fallback_chain` 元素固定为以下 schema，并按 `attempt_index` 升序编码。index 从 1 连续递增，`parent_attempt_index` 只能为空或指向更小 index。

```text
attempt_index
parent_attempt_index
dispatch_mode          primary | retry | fallback | hedge
route_rule_hash
requested_model
resolved_model
provider
channel_ref
account_pool_ref
region
outcome                succeeded | upstream_failed | protocol_failed | gateway_failed | cancelled
error_code
started_at
finished_at
```

top-level `attempts` 必须等于元素数量。top-level resolved model、provider、channel、account pool 与 region 必须等于最终成功 attempt，或在失败时等于最后一个实际执行 attempt。相同 index 的元素只能同值重试。除 Policy 明确允许 hedge 外，后一个 attempt 的开始时间不得早于 parent 的结束时间。

`schema_version` 首版为 `radar-route-evidence-v1`，`canonicalization_version` 首版为 `rfc8785-v1`。编码使用 UTF-8 与 RFC 8785 JSON Canonicalization Scheme。所有键都存在，可空值编码为 JSON null。UUID 使用小写连字符格式，枚举使用小写 ASCII，时间统一为 UTC 并固定六位小数的 RFC 3339 格式，整数使用 JSON 十进制整数，`billed_amount` 使用固定八位小数的字符串。`fallback_chain` 保留实际因果顺序，每个元素内部仍按 RFC 8785 规范化。数据库的 `created_at`、`updated_at`、`payload_hash` 和 `payload_hmac` 不进入 Envelope。

`payload_hash` 为 canonical Envelope 字节的 SHA256 小写十六进制。`payload_hmac` 为 `HMAC-SHA256(key, UTF8(schema_version) || 0x0a || canonical_bytes)` 的小写十六进制。Gate Loader 必须从数据库字段独立重建 Envelope，同时复算 hash 与 HMAC，禁止信任行内已保存 hash。schema version 变化会同步改变 HMAC domain separator。

Gate Loader 还要通过 PairSpec 读取 immutable Request Manifest 与 Request Semantics canonical bytes，分别复算两个 hash。它验证 manifest ID、slot ID、ordinal、semantics mode、tool schema 与工具集合绑定。exact slot 必须匹配 expected semantics hash，adapter policy slot 必须由 Policy 指定版本的 Adapter verifier 独立重放并通过。任何对象缺失、hash 不符、slot 歧义、verifier 不存在或 verdict 失败都产生 `insufficient_evidence` 与协议完整性告警。

open patch 的 merge matrix 固定如下。

| 字段组 | 首次写入 | 同值重试 | 不同值重试 |
| --- | --- | --- | --- |
| immutable identity、schema、epoch | 仅 CreateOpen | 返回当前 revision | `route_evidence_identity_conflict` |
| fallback chain | 只允许追加合法后继或补齐当前 attempt 的空值 | 幂等成功 | 修改既有非空值时 conflict |
| provider、resolved model、错误与时间 | transport 可从 null 写一次 | 幂等成功 | conflict |
| token、latency、finish reason、billing amount | billing 可从 null 写一次 | 幂等成功 | conflict |
| billing status | `incomplete` 可转 `complete` 或 `not_applicable` | 幂等成功 | 其他转换 conflict |
| terminal、seal、hash、HMAC | 仅 Finalize | 按 sealed retry 处理 | sealed conflict |

stale expected revision 返回 409 并携带当前 revision，调用方重新读取后按同一 matrix 重试。任何 patch 都不能清空非空字段。

### 10.5 Finalize

网关在 transport 与 usage accounting 都完成后调用 Finalize。正常 Finalize 使用 Run 优先锁序，验证 immutable identity、expected revision、lease epoch、current Assignment 与 Run 非终态，随后选取 active signing key 与数据库事务时间，生成 canonical Envelope，计算 SHA256 与 HMAC，原子写入终态、`terminal_at`、`sealed_at`、`payload_hash`、`signing_key_id` 和 `payload_hmac`。

cancel 或 failed 事务提交后，只有带匹配 terminalization outbox Event ID 与 current epoch 的系统 Finalizer 可以绕过旧 lease，把 open row 封存为对应失败终态。系统 Finalizer 仍使用 Run 优先锁序，并把 causation Event ID 纳入审计。若正常 Finalize 先取得 Run 锁，它可以完成，随后 cancel 或 failed 保留 sealed row；若终态事务先取得锁，旧 Gateway Finalize 返回 `lease_fenced`。

sealed 重试使用已保存的 server generated revision、key ID 与时间重建候选 Envelope。相同 payload 返回已有记录，不同 payload 返回 `route_evidence_sealed_conflict`。sealed 行的业务字段 UPDATE 与 DELETE 由数据库触发器拒绝。

### 10.6 密钥演进

Evidence 保存 `signing_key_id`。Signing key 状态为 `active`、`verify_only` 或 `revoked`，每次状态转换都递增该 key 不可回退的 `state_epoch`。常规轮换把旧 key 转为 verify_only，保留期覆盖 Evidence 生命周期。密钥疑似泄露时转为 revoked，所有引用它的 Evidence 与 Decision 立即失效并入队重新求值。Gate Evidence Loader 根据 key ID 验证 HMAC，只接受 active 或 verify_only。无法找到 key、key 已 revoked 或签名不匹配时返回 `insufficient_evidence` 并触发证据完整性告警。

## 11. Score Head 与 Regrade

### 11.1 Score 追加语义

Score 与 Aggregate Snapshot 的 UPDATE 和 DELETE 都由数据库拒绝。`evaluation_scores.is_current` 停止参与业务逻辑，也不再被更新。所有 current Score 查询都 join `evaluation_score_heads`。

Score 增加 `source_assignment_id`、`route_evidence_set_hash`、`route_evidence_refs` 与 `artifact_manifest_hash`。Evidence refs 按 request ordinal 排序，set hash 绑定 expected request manifest hash，以及每个 Route Trace ID、ordinal 与 payload hash。Score Head 只在 source Assignment 等于 Sample current attempt、全部 Route Evidence sealed 且 set hash 匹配 Adapter manifest 时属于 eligible current Head。替换 Assignment 会立即使旧 Head 退出 eligible 投影，并写 recompute outbox。

由于 Score 按 `created_at` 分区，Score idempotency、Grading Job、Manual Review、Head、Head Event、Analysis Job 和 Snapshot 一律保存完整 `ScoreRef`。Aggregate Snapshot 按 `window_start` 分区，Analysis Job、Aggregate Head、Global Job 与 Gate Manifest 一律保存完整 `SnapshotRef`。数据库 FK 使用复合 locator，禁止只凭 UUID 定位分区记录。

### 11.2 Score Head 推进

新增 Score 的事务执行以下步骤。

1. 锁定 `(sample_id, grader_id)` Head。
2. 验证 grader ID 与 version 完全匹配 PairSpec、source Assignment 仍为 current、sealed Route Evidence set hash、artifact manifest hash 和递增 version。
3. INSERT 新 Score。
4. INSERT `evaluation_score_head_events`。
5. 原子更新 `evaluation_score_heads`。
6. 计算受影响 cell 的 current score-head set。
7. 只有 expected pair 的 baseline 与 candidate current Score Head 全部存在时，才按 input hash 幂等创建 Analysis Job。单侧推进只写 recompute outbox，不能生成半对 Aggregate。

Head Event 对 `(sample_id, grader_id, version)` 唯一。它保存 previous `ScoreRef`、new `ScoreRef`、source Assignment、Evidence set hash、原因、actor 或 job identity。

### 11.3 completed Run regrade

Regrade 使用已封存的原始 Evidence 创建新 Grading Job。Grading Job 增加 `work_origin=initial|regrade`、`grading_input_hash`、`evidence_manifest_hash`、`recovery_generation` 和可空 `revision_batch_id`。数据库约束要求 regrade job 的 Batch ID 非空，initial job 的 Batch ID 为空，generation 从 0 单调递增。`grading_input_hash` 固定 assignment、sealed Route Evidence set hash、artifact manifest hash、grader ID、grader version、rubric/config hash 与原因。

initial job 使用 partial unique key `(assignment_id, grader_id, grading_input_hash)`。regrade job 使用 `(revision_batch_id, assignment_id, grader_id, grading_input_hash, recovery_generation)`。普通创建和重试只能命中 generation 0 的同一 Job。只有 blocked Batch 的受控 repair 或 fence 可以在记录 failure remediation 与审批事件后增加 generation，避免历史 Batch 阻止新的合法 regrade，也避免普通调用绕过幂等约束。

completed Run 只允许受 RBAC 保护的控制面通过 revision batch 创建并领取 `work_origin=regrade` 的 Grading 与 Analysis Job，Runner Assignment 始终禁止领取。Regrade 可以推进 Score Head，但不能修改 Sample、Run 或旧 Score，也不能改变 Run 终态。Grading 失败且 Head 未推进时保留原 Gate 投影。Head 已推进后，Analysis 或传播失败会让 current Gate 投影保持 `insufficient_evidence` 并触发 pipeline 告警，直到 matching Aggregate 与新 Decision 完成。

### 11.4 Revision Batch

`evaluation_revision_batches` 保存 `id`、completed Run ID、status、`control_epoch`、reason、actor、idempotency key 与时间戳。状态为 `pending|running|blocked|completed|failed|cancelled`。pending、running 与 blocked 视为 active，数据库 partial unique index 保证每个 Run 最多一个 active Batch。completed、failed 与 cancelled 为终态。blocked 表示已有 eligible Head 推进，但强制传播发生可恢复故障。Regrade lease 的 `lease_epoch` 对比 Batch epoch，initial work 继续对比 Run epoch。Batch 对 `(id, run_id)` 提供唯一键，所有 regrade Grading Job、cell/global Analysis Job、outbox event 和 Snapshot 都以复合 FK 禁止跨 Run 绑定。

`evaluation_revision_batch_requirements` 在创建 Batch 时冻结每个目标的 Assignment、起始 ScoreRef、grader 协议、`grading_input_hash` 与状态。Score Head 推进事务为该 Batch 追加不可变的 cell requirement，后续 Aggregate Head 推进继续追加 global 与 Gate requirement。每条 requirement 保存 source hash、cause set hash 和 `pending|completed|failed|superseded` 状态。pending 只允许转为 completed、failed 或 superseded。failed 只有在同一事务插入覆盖同一目标的 replacement requirement 后才能转为 superseded。目标身份与 hash 永久不可修改。

`POST /api/v1/admin/radar/revision-batches/:id/fence` 接受 running 或 blocked Batch。running 分支递增 Batch epoch 并清理旧 lease；blocked 分支先按原 cause set 把可恢复 dead letter 重置为 pending，为可恢复的 failed grading requirement 插入下一 recovery generation 的 replacement requirement，再递增 epoch 并转为 running。`resume` 只接受不存在活动 lease 且不存在 failed grading requirement 的 blocked Batch，重置原 dead letter 并转为 running，epoch 保持不变。旧 Batch epoch 的 Grading、Analysis 或 outbox handler 提交都返回 `lease_fenced`。同一 Run 存在 active Batch 时，创建第二个 Batch 返回冲突。

cancel 只有在该 Batch 尚未推进任何 eligible Score Head 时才能转为 cancelled，同时取消未开始 Grading Job。已经推进 Head 时返回 `revision_batch_propagation_required`，控制面只能 fence 或修复，所有 cell、global、Gate、Alert 与 Release propagation 必须继续收敛。尚无 Head 推进时，任一未被 replacement 覆盖的 failed grading requirement 使 Batch 转为 failed，旧 Gate 投影保持有效。failed 与 cancelled 事务都递增 Batch epoch，取消其余非终态 Job，清理 lease 并追加唯一状态事件。已有部分 Head 推进时，同类 failure 使 Batch 转为 blocked，current Gate 投影保持 `insufficient_evidence`。blocked Batch 必须通过受控 replacement、resume、fence 或追加式 compensating Head Event 覆盖失败 requirement 并完成原有因果链。Compensating Event 只能把 Head 指向该 Batch 开始时冻结的 prior ScoreRef，要求双人审批、有限 reason code 和完整审计，不能修改任何 Score。

Batch 只有在全部 frozen grading requirement 与新增 requirement 都已 completed，或被带完整因果证据的 replacement 或后续 Head 合法 supersede 时才能 completed。任何未覆盖的 failed requirement 都禁止完成。收敛检查锁定 Batch 和 requirement 集合，验证 current cell/global Aggregate 与 current Decision 已覆盖该 Batch 的全部 Head Event。Batch 终态不改变 Run 终态。

同一 Run 的 regrade 只有使用 PairSpec 固定的 grader ID、version 与 rubric/config hash 才能推进 eligible Score Head。研究新 Grader 或新 rubric 时创建新协议 Run。控制面可以保存不同 `grading_input_hash` 的实验 Job，但它们不能进入该 Run 的 Aggregate 或 Gate。

## 12. Cell Aggregate Revision

### 12.1 canonical cell

Cell identity 为以下三元组。

```text
run_id
capability_domain
canonical_model_route
```

`canonical_model_route` 只去除一个 `baseline:` 或 `candidate:` 前缀。其余文本必须保持稳定。

### 12.2 input_set_hash

Repository 在锁定 Score Head 后，按以下字段稳定排序并生成 canonical JSON。

```text
case_id
sample_index
pair_spec_hash
baseline_side_spec_hash
candidate_side_spec_hash
pair_binding_hash
grader_id
grader_version
baseline_head_version
baseline_score_ref
baseline_source_assignment_id
baseline_route_evidence_set_hash
candidate_head_version
candidate_score_ref
candidate_source_assignment_id
candidate_route_evidence_set_hash
case_weight
```

SHA256 结果为 `input_set_hash`。完整 pair 数必须等于期望 pair 数且大于零。

### 12.3 Analysis Job

Analysis Job 保存以下不可变输入。

```text
scope                 cell | global
work_origin           initial | regrade
revision_batch_id
input_set_hash
input_score_refs
input_snapshot_refs
aggregate_revision
analysis_version
cause_set_hash
```

数据库约束要求 `work_origin=regrade` 的 cell 与 global job 都具有非空 `revision_batch_id`，initial job 必须为空。regrade job 的 claim、heartbeat、complete 和 fail 都先锁定 owning Batch，要求 Batch 为 running 且 lease epoch 匹配。blocked、completed、failed 或 cancelled Batch 禁止 Worker 提交。fence 后的新 claim 使用新 epoch，resume 重放继续使用未变化的 current epoch。

cell job 的自然幂等键如下。

```text
run_id + capability_domain + canonical_model_route + analysis_version + input_set_hash
```

`aggregate_revision` 在同一 cell 与 analysis version 内单调递增。`evaluation_aggregate_heads` 的主键为 `(run_id, capability_domain, canonical_model_route, analysis_version)`。Statistics Worker 不能替换输入 ScoreRef，也不能提交 job 未固定的 Score。

### 12.4 Snapshot 与 Aggregate Head

Snapshot 增加以下字段。

```text
analysis_job_id
revision_batch_id
input_set_hash
aggregate_revision
aggregate_hash
score_refs
source_head_event_ids
origin_revision_batch_ids
cause_set_hash
```

完成 job 时，Repository 验证提交 ScoreRef 集合与 job 固化集合完全相等，随后 INSERT immutable snapshot。source Head Event ID 与 origin Batch ID 都去重并稳定排序，`cause_set_hash` 绑定完整 cause relation。只有每个 Score source 仍匹配 current Assignment 与 sealed Evidence，且 `input_set_hash` 仍等于 eligible current Score Head 集合时，Repository 才推进 `evaluation_aggregate_heads`。Aggregate Head 保存完整 SnapshotRef。较旧 job 可以完成和保留 Snapshot，但不能成为 current Head。

## 13. Global Aggregate Revision

Run 只有一个 cell 时不要求 global snapshot。Run 包含多个能力域或多个 canonical model route 时，必须有显式 `global/global`。

预期 cell 集合由 Run 冻结的 PairSpec、SideSpec 与 capability domain 导出，不能从已生成 Snapshot 反推。只有每个预期 cell 都存在与 current score-head set 匹配的 current Snapshot 时，Repository 才构造 global input。输入按以下字段排序。

```text
capability_domain
model_route
snapshot_ref
aggregate_revision
input_set_hash
aggregate_hash
```

canonical JSON 的 SHA256 为 `global_input_hash`。Global Job 以该 hash 幂等，保存 regrade 来源的非空 `revision_batch_id`、全部直接 cause Event ID 与 `cause_set_hash`。Snapshot 保存 `global_revision`、全部 source SnapshotRef、稳定排序的 source Head Event ID、origin Revision Batch ID 集合与 cause set hash。任一 cell Head 变化都会产生新的 global revision。

## 14. Gate Evidence 与 Decision

### 14.1 只读一致快照

Gate Evidence Loader 使用只读、可重复读事务。它固定 current Policy Head、current Baseline Head、Run、PairSpec、SideSpec、eligible current Score Heads、matching cell/global Snapshot、sealed Route Evidence、Reliability Snapshot 和 Release Subject 的相关 source watermark。

缺少任一 current cell、需要 global 时缺少 global、Route Evidence 未 sealed、hash 或 HMAC 无法复算、`legacy-unbound`、样本量不足或类型错误都会产生 `insufficient_evidence`，同时按有限 reason code 创建完整性告警。

### 14.2 ReleaseSubject

`ReleaseSubject` 至少包含以下字段。

```text
candidate_model_config_sha256
baseline_id
dataset_manifest_sha256
route_profile_version
gateway_image_digest
control_plane_image_digest
runner_image_digests
grader_image_digests
statistics_image_digests
analysis_version
region_set
deployment_environment
scope_type
scope_id
```

Worker digest 字段是去重排序后的集合，Evidence Manifest 还保存每项工作的实际 Worker digest。Policy 可以要求每类 Worker 只有一个 digest。每份 Route Evidence 的 `gateway_image_digest` 必须等于 Release Subject 声明的 gateway digest。canonical JSON 的 SHA256 为 `release_subject_hash`。发布对象、环境或执行组件发生变化都需要新 Decision。

### 14.3 Evidence Manifest

Evidence Manifest 只包含 ID、hash、revision、计数、有限指标和计算版本。它不得包含 Prompt、Completion、token、真实账号、真实渠道或上游正文。

```text
policy_id + policy_hash + policy_activation_event_id
baseline_id + baseline_evidence_hash + baseline_activation_event_id
run_id + route_profile_version
request_manifest_ids + manifest_hashes
pair_spec_hashes + side_spec_hashes + pair_binding_hashes
current_assignment_refs
score_refs + head_versions + eligible_score_head_set_hashes
cell_snapshot_refs + revisions + hashes + cause_set_hashes
global_snapshot_ref + revision + hash + cause_set_hash
route_evidence_ids + request_ordinals + request_semantics_hashes + payload_hashes
signing_key_ids + signing_key_states + signing_key_state_epochs
reliability_snapshot_ref + hash
worker_job_ids + worker_image_digests
release_subject_hash
loader_version
source_watermark
```

所有集合先按领域主键稳定排序。Pair 按 case ID、sample index、repeat index 排序，AssignmentRef 按 sample ID 排序，ScoreRef 按 cell、case、side、grader 排序，SnapshotRef 按 cell 排序，Route Evidence 按 Assignment ID 与 request ordinal 排序，Worker job 按 kind 与 Job ID 排序。空的可选 global Snapshot 使用 JSON null，不能省略字段。

`source_watermark` 是相关 source tuple 的稳定 hash。输入固定为 Run ID 与 state version、release subject hash、Policy Head identity 与 activation event、Baseline Head identity 与 activation event、每个 Sample 的 current AssignmentRef 与 control epoch、每个 cell 的 eligible score head set hash、ScoreRef、head version、source Assignment ID、Route Evidence set hash、Request Manifest 与 Request Semantics hash、cell/global Aggregate Head 的完整 SnapshotRef、revision、aggregate hash 与 cause set hash、sealed Evidence 的 revision 与 payload hash、每个 signing key 的 state 与 state epoch、Reliability SnapshotRef 与 hash、Loader version。

Signing key 的 active、verify_only 与 revoked 转换都递增不可回退的 `state_epoch`，状态转换事务同时写 Gate reevaluation outbox。Assignment replacement、key 状态变化或 Release Subject 变化即使未改变 Score Head version，也会改变 watermark 并使旧 Decision 立即失效。watermark 不使用全局 transaction ID，因此无关数据库写入不会改变 Evidence。canonical Manifest 的 SHA256 为 `evidence_hash`。

### 14.4 Gate 状态

持久化 Decision 只使用以下状态。

```text
recorded
passed
blocked
review_required
insufficient_evidence
```

`waived` 是查询投影，不写回原始 Decision。控制台颜色只作为辅助映射，Green 对应 passed，Yellow 对应 recorded、review_required 或 insufficient_evidence，Red 对应 blocked。

### 14.5 求值顺序

求值短路顺序固定如下。

1. Evidence sufficiency
2. P0 safety、protocol、tool permission 和 billing correctness
3. Route identity
4. Reliability SLO
5. record-only observation
6. Critical domain quality
7. Global quality
8. Judge disagreement
9. Negative trend
10. Pass

record-only 只影响统计质量规则。P0、Route Identity、Reliability SLO 和 Evidence Sufficiency 始终执行。

### 14.6 幂等与 supersession

Decision 自然键为以下三元组。

```text
run_id + policy_id + evidence_hash
```

新 Evidence 产生新 Decision。Decision Head 的 lineage key 固定为 `(run_id, policy_id, release_subject_hash)`，三元组是 `evaluation_gate_decision_heads` 的主键。新记录保存 `supersedes_decision_id`，旧 Decision 不更新。`supersedes_decision_id` 在非空时唯一，防止 lineage 分叉。Repository 对完整 lineage key 获取事务级 advisory lock，重新读取 current Decision Head，追加新 Decision 与 supersession event，再原子推进 Head。有效投影只读取该 Head。

已部署对象出现新 evidence 时，系统创建新 Decision 与 Alert。若新 Decision 为 `blocked` 或 `insufficient_evidence`，Release 状态进入 `degraded`，执行人工回滚或补证流程。

### 14.7 Revision 强制传播

Score Head 推进后，以下链路不能跳过。

```text
score_head_event
-> cell recompute outbox keyed by run + cell + input_set_hash
-> matching cell Aggregate Head
-> global recompute outbox keyed by run + analysis version + global_input_hash
-> matching global Aggregate Head
-> gate reevaluation outbox keyed by run + policy + subject + source_watermark
-> Decision supersession
-> Alert 与 Release projection
```

单 cell Run 跳过 global 两步。每个 outbox consumer 使用自然幂等键并可重复执行。任何中间步骤待处理或失败时，Release Verifier 通过 current Head 比对拒绝旧 Decision，current Gate 投影为 `insufficient_evidence`。新 Decision 写入后，Alert 与 Release projection 通过同一 Decision ID 幂等更新。已部署对象的新 Decision 为 `blocked` 或 `insufficient_evidence` 时，Release 立即进入 `degraded`。

`evaluation_outbox_events` 保存 `id`、`event_type`、全局唯一 `dedup_key`、`causation_id`、`cause_set_hash`、可空 `work_origin`、可空 Revision Batch ID、Run ID、source type、source ID、source hash、payload hash、有限 payload、status、attempt、available time、lease token hash、lease owner、lease expiry、`lease_epoch` 与时间戳。status 只允许 `pending|leased|completed|dead_letter`。claim 使用 `FOR UPDATE SKIP LOCKED` 与租约 fencing。regrade event 的 Batch ID 必须非空，initial 与无 work origin 的控制面 event 必须为空，复合 FK 保证 Batch 与 Run 相同。合流 event 的任一 cause 来自 regrade 时继承同一 Batch ID，单 Run 单 active Batch 保证该值唯一。

regrade outbox claim 在锁定 Batch 后复制 current Batch epoch。heartbeat、handler commit 与产物写事务再次锁定 Batch，要求 status 为 running、event lease epoch 等于 Batch control epoch、lease token 与 expiry 有效。Batch fence 后，旧 handler 不能写 Job、Snapshot、Decision、Alert 或 Release projection；reaper 把旧 epoch event 恢复为 pending，新 claim 复制新 epoch。initial 与控制面 event 使用对应 Run 或 Head 的 current source 校验，不引用 Batch epoch。

`evaluation_outbox_event_causes` 保存 `(event_id, cause_event_id)`、可空 Revision Batch ID 与 source Head Event ID，主键为前两个 ID。cause Event 必须早于子 Event，且属于同一 Run。根事件使用自身不可变 source tuple 计算 cause set。合流事件按 cause Event ID 排序后计算 `cause_set_hash` 与 `causation_id`，因此 global Aggregate、Gate Decision 和 Release projection 可以证明多 cell 与多 source 收敛。Snapshot、Decision 与 Batch requirement 保存相同 cause set hash。

event type、dedup key、causation ID、cause set hash、Revision Batch ID、Run ID、source type、source ID、source hash、payload hash、payload、created time 与 cause relation 都是 insert-only 字段。业务 writer 只能更新 status、attempt、available time、lease owner、lease token hash、lease expiry、lease epoch、有限 last error code 与更新时间，其他 UPDATE 和 DELETE 由数据库拒绝。完成事件的受控归档只能在所有引用它的 Snapshot、Decision、Batch requirement 与审计记录超过共同保留期后执行。

每次 Head 推进与对应 outbox INSERT 必须位于同一事务。Route Evidence seal、Reliability Snapshot publish、Policy Head、Baseline Head、signing key state 或 Release Subject 推进也必须在同一事务写 Gate reevaluation outbox。consumer 创建的 Job、Snapshot、Decision、Alert 或 Release projection 保存完整 cause set，随后事件通过 cause relation 引用所有直接来源。全局 `dedup_key` 统一计算为 `SHA256(domain_prefix, event_type, run_id, scope_key, analysis_version, source_hash)`。Gate key 额外包含 Policy ID、release subject hash 与 source watermark，projection key 包含 Decision ID 与 projection type。Gate consumer 加载 Evidence 后继续使用 Policy ID 加 evidence hash 作为 Decision 幂等键。重试耗尽进入 dead letter 并触发 pipeline 告警，人工重放继续使用原 event ID、dedup key 与 cause relation。

### 14.8 Policy 生命周期

Policy 内容采用不可变版本。`evaluation_gate_policy_heads` 的主键为 `(tenant_id, deployment_environment, scope_type, scope_id)`，scope ID 使用非空 canonical value，全局 scope 使用保留值 `global`。激活与替代通过追加 `evaluation_gate_policy_events` 审计。激活事务锁定包含租户的完整 scope key，校验审批与生效时间，追加 activation event 并推进 Head。Release Gate API 只接受与 current Policy Head 相同的 Policy ID，历史回放使用独立只读接口且不能生成发布授权。

Release Verifier 要求 Decision 的 Policy ID、hash 和 activation event 与 current Policy Head 完全一致。Policy 被替代、撤销、过期或适用 scope 改变时，旧 Decision 立即失效，并为相关 active Release 入队重新求值。Policy Head 没有 active 版本时，发布结果为 `insufficient_evidence`。

### 14.9 Baseline 生命周期

Baseline 版本不可变，`evaluation_baseline_heads` 的主键为 `(comparison_route_key, deployment_environment, scope_type, scope_id)`。激活、替代、撤销和到期通过追加 `evaluation_baseline_events` 与 Head 推进表达。Baseline 保存来源 Run、dataset manifest、Evidence hash、route profile、model config hash、批准主体和最大有效期。

Release Verifier 要求 Release Subject 的 baseline ID 与 current Baseline Head 一致，且 Baseline 未过期、route profile 和 scope 匹配。Baseline Head 变化会立即使旧 Decision 失效并入队新 Radar Run 或 Gate 求值。enforcement 必须绑定 active Baseline，record-only 可使用明确标记的 synthetic Baseline。

### 14.10 Release Authorization

Verifier 在同一事务锁定 Release 记录和全部相关 Head，完成复核后写入短时效、单次使用的 Release Authorization 与 deployment outbox。Authorization 固定 Decision ID、Release Subject hash、source watermark、有效 Waiver IDs、签发时间、过期时间和随机 nonce。

Deployment executor 在执行外部变更前重新校验 Authorization 未消费、未过期且 current Head 仍匹配，并用 compare-and-set 标记 consumed。Head 在外部变更后发生变化时，supersession 链立即把 Release 转为 `degraded`。部署过程不能直接消费 Decision row 或绕过 Authorization。

## 15. Waiver 与紧急发布

### 15.1 普通 Waiver

普通 Waiver 是追加记录，必须包含 Decision ID、允许豁免的 rule IDs、业务原因、风险负责人、缓解措施、复测计划、适用 Release Subject、到期时间和批准人。

以下规则不能使用普通 Waiver。

- P0 safety
- tenant boundary
- Route Identity
- Evidence signature 或完整性
- 缺失 current Aggregate
- 生产网络、凭据、PITR 或回滚矩阵失败

Waiver 到期、Decision 被 supersede、Release Subject 改变或复测截止时间到达时，投影自动失效。

### 15.2 四眼原则

Waiver 由 Release Manager 批准，风险负责人必须是不同主体。创建者不能批准自己的 Waiver。企业租户范围的 Waiver 还要满足租户授权与平台策略交集。

### 15.3 紧急发布

紧急发布使用独立 Break Glass 流程，不伪装成普通 Waiver。它需要 Platform Admin、Release Manager 和安全负责人三个不同主体，必须有短有效期、回滚目标、事件编号和自动告警。Break Glass 不改变原始 Gate Decision。

Break Glass 只能处理统计质量或已量化的可靠性风险。tenant boundary、Evidence signature 或完整性、Route Identity、未知 Release Subject、生产网络与凭据安全、PITR 和回滚能力属于 hard stop。P0 safety 只允许回滚到仍有有效可信 Decision 的既有 Release，不能授权一个新的未通过候选版本。

## 16. Reliability Snapshot

Gate 使用显式 Reliability Snapshot ID。Snapshot 固定窗口、切片、查询版本、水位、分子、分母、样本量、直方图或 sketch hash、P99、error rate 和数据新鲜度。

Migration 199 创建 immutable `evaluation_reliability_snapshots` 与 `evaluation_reliability_heads`。Snapshot 保存 Run ID、reliability profile ID、slice key、UTC window、query version、source hash、有限 metrics、snapshot hash、fresh until 与 created at。自然键为 `(run_id, reliability_profile_id, slice_key, window_start, window_end, source_hash)`。

Reliability Head 的主键为 `(run_id, reliability_profile_id, slice_key)`。Publisher 在同一事务插入 Snapshot、推进 Head 并写 Gate reevaluation outbox。Gate Policy 固定 required slice 集合与 profile ID。Release Verifier 要求 Decision 引用的每个 Snapshot 都等于 current Head 且仍在 freshness 窗口内。

发布 Gate 的默认规则如下。

- 窗口使用 UTC 半开区间。
- 成功延迟切片至少 200 个请求。
- 质量切片至少 30 个有效 pair。
- required slice 为空或过期时返回 `insufficient_evidence`。存在已确认且仍进行中的 P0 incident 时返回 `blocked`。
- 客户取消独立计数，上游失败进入 error rate，不能隐藏在成功延迟分布中。

## 17. 错误合同

| 领域错误 | HTTP | 可重试 | 含义 |
| --- | --- | --- | --- |
| `lease_fenced` | 409 | 否 | epoch、token、worker 或 job 状态失效 |
| `worker_identity_conflict` | 409 | 否 | 同名 Worker 身份不同 |
| `worker_token_conflict` | 409 | 否 | token hash 与其他 Worker 冲突 |
| `route_evidence_identity_conflict` | 409 | 否 | Trace 的 immutable identity 不同 |
| `route_evidence_revision_conflict` | 409 | 是 | expected revision 落后，重新读取后重试 |
| `route_evidence_sealed_conflict` | 409 | 否 | sealed payload 与重试 payload 不同 |
| `request_semantics_mismatch` | 409 | 否 | 请求语义、slot、tool schema 或工具集合不匹配 |
| `score_submission_conflict` | 409 | 否 | 相同 idempotency key 对应不同内容 |
| `stale_evaluation_source` | 409 | 否 | Assignment、Evidence set 或 ScoreRef 已非 current |
| `analysis_input_conflict` | 409 | 否 | 提交输入与 job frozen set 不同 |
| `revision_batch_active` | 409 | 是 | 同一 Run 已存在 active Revision Batch |
| `revision_batch_propagation_required` | 409 | 否 | 已推进 Head 的 Batch 必须完成传播，不能取消 |
| `policy_version_conflict` | 409 | 否 | 同一 version 内容不同 |
| `policy_head_mismatch` | 409 | 否 | Release Gate 请求未使用 current Policy |
| `baseline_head_mismatch` | 409 | 否 | Release Subject 未使用 current Baseline |
| `release_authorization_stale` | 409 | 否 | Authorization 过期、已消费或 Head 已变化 |
| `radar_cutover_active` | 503 | 是 | Writer cutover 正在 draining 或 closed |
| `insufficient_evidence` | 200 | 由 Gate 补证 | 可信结论条件不完整 |

未知内部错误返回 500，并记录脱敏 correlation ID。响应、日志和审计不能包含 token、HMAC key、Prompt 或 Completion。

## 18. 迁移与发布顺序

### 18.1 Migration 197

Migration 197 增加 Request Manifest、PairSpec、SideSpec、Pair Binding、Score Head 的 `score_created_at` locator、Worker lifecycle、Run `budget_mode`、pause/cancel 字段、`control_epoch`、`state_version`、Run Event transition 唯一键、deferred event constraint trigger、lease epoch、job cancelled 状态和 Radar writer protocol gate。历史 Run 的 `budget_mode` 默认为 normal，不推断历史优先级语义。应用写路径在此阶段统一采用 Run 优先的锁顺序，并停止更新 immutable Score 的 `is_current`。

197 回填通过 `(score_id, created_at)` 唯一定位每个现有 Head，检测到重复 UUID 或缺行时迁移失败。所有 Score 读取在 197 应用发布时切换到 `evaluation_score_heads` 与完整 ScoreRef。197 到 199 期间，旧 Aggregate 通过明确标记 `legacy-untrusted` 的兼容只读视图展示，Gate 存储层不能消费它。控制台不得继续读取停滞的 `is_current`。

### 18.2 Migration 198

Migration 198 增加 Run `route_profile_version`、Release Subject、Policy Head/Event、Baseline Head/Event、Decision/Waiver append-only 约束、Decision 三元幂等键、Decision Head 与 supersession 字段。旧 Run 使用 `legacy-unbound`。此阶段只启用 Gate 存储兼容层，所有缺少 199 可信输入的求值都保持 `insufficient_evidence`，不能执行可信 Gate 验收或 enforcement。

Migration 198 不提前增加与旧 Score 写路径冲突的新约束。Migration 193 已有的 Score UPDATE trigger 保持有效，Migration 197 随应用发布的兼容补丁已经停止更新 `is_current`。

### 18.3 Migration 199

Migration 199 增加 Request Semantics、Route Evidence assignment 与 lease epoch binding、versioned Envelope、Finalize sealing、Revision Batch、Batch Requirement、regrade job identity、Score Head Event 的复合 ScoreRef、SnapshotRef、统一 outbox、outbox cause relation、Analysis input hash、Aggregate Head、Reliability Snapshot/Head、cell/global revision、完整 UPDATE/DELETE immutable trigger 和新的 job identity。

Migration 199 采用 expand-compatible 字段。历史行和滚动窗口内旧应用写入的行允许缺少 trusted metadata，并始终保持 unsealed 或 legacy 状态。只有新 Finalize 路径可以创建 sealed row，immutable trigger 只保护 sealed row 与新 revision 对象。current-head 查询、Finalize 和 revision-aware Repository 与 migration 199 同次发布，历史未 sealed Evidence 与缺少 revision metadata 的 Aggregate 保持不可信。

### 18.4 Writer quiescence

Migration 197 建立单行 `evaluation_schema_cutovers`、`evaluation_writer_sessions` 与 writer protocol。Cutover 同时保存 `write_mode=open|draining|closed`、`guard_mode=audit|enforce` 和最低 protocol version。Writer session 保存 instance ID、kind、protocol version、active lease count、last transaction time 和 heartbeat expiry。每个连接在 Radar 写事务中设置 transaction-local protocol 与 instance ID，受保护表的数据库 trigger 调用 `assert_evaluation_writer_protocol()` 校验 mode、最低版本与 session。audit 阶段允许缺失或旧 protocol 的 writer 完成写入，同时写审计事件与指标；enforce 阶段拒绝它们。

所有 Radar control、Gateway evaluation writer、Worker、Gate writer、scheduler、reaper 和 outbox consumer 都受该协议约束。write mode 为 draining 时拒绝新业务写入，已有事务完成；write mode 为 closed 时只有 migration owner 数据库角色可以写。过期 session 只有在数据库确认没有对应 transaction 或 advisory lock 后才可从 drain 计数排除。

197 使用自身的分阶段 cutover。

1. 以 `guard_mode=audit` 执行 expand schema，现有 writer 可以继续写入。
2. 部署 protocol-aware writers，所有新 Radar 事务注册 session 并设置 transaction-local identity。
3. 保持 audit，直到至少一个最大 lease 窗口内不再出现缺失标识、旧 protocol 或未知 instance。
4. 关闭 Run start、evaluation traffic 与全部 Radar control writer，暂停新 claim，并把 write mode 切到 draining。
5. 等待所有 Writer session、lease、数据库事务与相关 advisory lock 归零，随后由 migration owner 切到 closed。
6. 验证所有存活应用镜像支持目标 protocol，在同一受保护事务中设置最低版本与 `guard_mode=enforce`。
7. 把 write mode 恢复为 open，只启动 protocol-compatible writers。
8. 执行 Run lifecycle、Score Head 读取、旧 writer 拒绝、幂等与 fencing acceptance。

第 6 步之前可以回滚到旧 writer 并把 guard 保持 audit。第 6 步之后只能回滚到声明兼容目标 protocol 的应用镜像。任一步验证失败都保持 closed，禁止带部分协议保护恢复写入。

198 cutover 先关闭 Run start、Gate evaluate、Policy、Baseline、Waiver 与 Release writer，暂停新 claim 和 evaluation traffic，再等待所有 Radar writer heartbeat、lease 与数据库事务归零。migration owner 取得全局 advisory lock 后，原子替换 Decision 唯一约束、相关 trigger 与 Repository protocol version。199 使用相同步骤，并额外等待 Gateway evidence writer、replacement、Grader、Statistics、reaper、scheduler 和 outbox consumer 归零，再替换 assignment grading trigger、regrade 约束、cause relation 和 revision 写路径。

仅暂停新 claim 不满足 cutover 条件。每次恢复前必须证明所有存活 writer 的 protocol version 达到目标值。客户生产流量继续运行，evaluation context 在 closed 窗口返回受控 503。schema 已提升后只能回滚到声明兼容该 protocol 的应用镜像，sealed 与 revision 数据始终保留。

### 18.5 启用顺序

```text
197 expand schema in audit mode
-> protocol-aware writers
-> audit clean window
-> drain and close Radar writers
-> enable protocol guard
-> reopen compatible writers
-> lifecycle acceptance
-> close Radar writers for 198 cutover
-> 198 schema 与 Gate 存储兼容应用
-> append-only storage acceptance
-> close all Radar writers for 199 cutover
-> 199 schema 与 sealing/revision 应用
-> deterministic Gate acceptance
-> 30-pair G2 acceptance
-> 14 个完整日 G3 observation
-> enforcement approval
```

任一步失败都停止向后推进。回滚应用镜像时保留追加记录和 named volume，禁止回退或覆盖可信历史。

enforcement approval 绑定 current Policy hash、部署环境与 observation 窗口，至少包含以下不可省略证据。

1. 30-pair 结果与 hash 可独立复算。
2. 14 个完整 UTC 日的 cell 覆盖率、样本量和数据新鲜度满足策略。
3. 误报、漏报和人工复核队列已经复盘。
4. P0 告警路由、投递回执和升级链已经演练。
5. 回滚、PITR、凭据轮换和 Worker fence 演练通过。
6. Quality Admin 与 Release Manager 两个不同主体完成批准，涉及安全 enforcement 时还需要独立安全负责人。

批准通过追加 Policy activation event 生效，禁止修改旧 Policy 的 enforcement 字段。

## 19. 测试合同

### 19.1 Run 与预算

- exact budget 仅 P0、仅 P1/P2、混合优先级三种矩阵。
- pause 与最后一个 P0 completion 并发时保持 paused，resume 只产生一个正确转换。
- 并发 claim 只能成功领取一个 Assignment。
- 最后一个 P0 完成只产生一次 resume。
- P0 不可恢复失败优先于 pending 工作。
- pause 后无新 claim，在途工作可以完成。
- cancel 后旧 Runner、Grader、Statistics lease 全部 fenced。
- failed 后其他在途 Runner、Grader、Statistics lease 全部 fenced。
- cancel 能处置 evidence_uploaded Assignment 与全部非终态 Grading、Analysis Job。
- 显式 fence 创建合法 replacement 或令 Run 失败，旧 epoch 永远不能提交。
- replacement 创建后，旧 Grader 提交被拒绝，旧 Assignment 的 Score 不能成为 eligible Head。
- pending、budget_paused、running 三种来源的 pause 与 resume 都恢复到重新计算的正确状态。
- 每个状态版本恰有一个 transition event。
- 缺失 transition event 的状态事务被 deferred constraint trigger 回滚。

### 19.2 Route Evidence

- transport 与 billing 乱序到达后 Finalize 成功。
- cancel 与正常 Finalize 两种锁顺序都线性化，cancel 先发生时旧 Gateway 得到 `lease_fenced`。
- failed 后 open Evidence 按 failure class 封存，outbox 超时产生完整性告警。
- 两个 Finalizer 并发时只封存一份 payload。
- 相同 payload 重试返回原记录。
- 不同 payload 重试返回 sealed conflict。
- immutable identity 修改被拒绝。
- sealed UPDATE 与 DELETE 被数据库拒绝。
- 历史 attempt 不会替代当前有效 Assignment 的 Evidence。
- 单轮 ordinal 0 与多轮连续 ordinal 集合都可封存，缺号、重复和 Adapter manifest 之外的调用被拒绝。
- Request Manifest canonical bytes、manifest hash、slot mapping 与 PairSpec FK 都可独立复算，缺行或篡改会拒绝 Gate。
- exact semantics hash 不符、agent policy verifier 不通过、tool schema 或工具集合不符时，上游分发被拒绝并封存协议失败。
- Route Evidence 的 manifest、slot 与 semantics identity 任一字段被修改时，Envelope hash 与 HMAC 复算失败。
- signing key rotation 后旧 Evidence 仍可验证。
- key 缺失、Envelope 字段篡改、hash 篡改和 HMAC 篡改都会得到 `insufficient_evidence` 与完整性告警。
- 不同 patch 到达顺序产生相同的 canonical Envelope，stale expected revision 返回冲突后可安全重试。
- fallback chain 的非连续 index、非法 parent、字段改写、attempt count 不符和顶层身份不符都会被拒绝。
- merge matrix 覆盖每个字段组的首次写、同值重试、冲突重试和清空拒绝。

### 19.3 Score 与 Aggregate

- 第二版 Score 不更新第一版字节。
- completed Run 的 authorized regrade 可领取 Grading 与 Analysis Job，Runner 仍不可领取。
- Revision Batch fence 后旧 batch epoch 的 Grading 与 Analysis 提交都被拒绝。
- Revision Batch fence 后旧 epoch 的 outbox handler 不能写入任何产物，新 claim 使用 current epoch 完成原事件。
- regrade Grading、cell/global Analysis、Snapshot、outbox 与 Batch requirement 的 Batch ID 全部非空且不能跨 Run。
- 同一 Run 只能存在一个 pending、running 或 blocked Batch，并发创建只成功一个。
- Batch 创建时冻结 grading requirement，Head 推进后追加的 cell、global 与 Gate requirement 必须形成完整 cause closure。
- Head 推进前 cancel 可以终止 Batch，Head 推进后 cancel 返回 propagation required；blocked Batch 通过 dead letter 重放、受控 replacement 或 compensating Head 恢复。
- 部分 Head 已推进且另一 grading requirement 失败时 Batch 进入 blocked，受控 replacement 覆盖失败目标后才能继续收敛。
- Batch 只有在全部 requirement completed 或带因果证据 superseded 后才能完成。
- 同一 Batch 与 recovery generation 内，相同 `grading_input_hash` 只生成一个 regrade job；受控 repair 使用递增 generation，历史 Batch 不会抑制新 Batch。
- 不同 grader 或 rubric 的实验 job 不能推进该 Run 的 eligible Head。
- 所有 current 查询只使用 Score Head。
- 每个 Head、Event、Job、Snapshot 和 Gate Manifest 都使用可命中分区行的 ScoreRef 或 SnapshotRef。
- 输入顺序变化不影响 `input_set_hash`。
- 一个新 input hash 只创建一个 cell job 与一个 revision。
- 旧 job 晚到时不能推进 current Aggregate Head。
- 缺少任一 cell 时不能创建 global job。
- 任一 cell revision 变化会创建一个新 global revision。
- Aggregate submission 必须精确匹配 frozen ScoreRef 集合。
- Head advance 与 outbox insert 同事务，consumer 输出保留 causation chain，dead letter 可按原 dedup key 重放。
- 多个 cell 合流到 global 与 Gate 时，cause relation 和 cause set hash 可以复算完整直接来源。
- 两个内容相同但 Run ID 不同的 input hash 会生成不同全局 dedup key，两条事件都能执行。
- outbox source、payload、Batch ID 与 cause relation 的 UPDATE 被拒绝，只有投递运行字段允许更新。

### 19.4 Gate 与发布

- HTTP 请求只能提交 Run ID、Policy ID 和可选 Baseline ID。
- 相同 Evidence 重试返回相同 Decision ID。
- Evidence 变化追加新 Decision 并建立 supersession。
- 并发新 Evidence 只能形成线性 Decision lineage，不能从同一 predecessor 分叉。
- Release Subject 任一 digest 变化都拒绝复用旧 Decision。
- current Assignment replacement 与 signing key state epoch 变化都会改变 source watermark 并拒绝旧 Decision。
- Score Head 推进后，旧 Decision 在新 Aggregate 与 Decision 写入前立即被 Release Verifier 拒绝。
- regrade 到 cell、global、Decision、Alert 和 deployed Release degraded 的端到端链路可重放且幂等。
- Policy 收紧、替代、撤销或 scope 改变后，旧 Decision 立即失效。
- Baseline 替代、撤销、过期或 scope 改变后，旧 Decision 立即失效。
- Policy Head 与 Decision Head 的复合 key 隔离不同环境、scope 和 Release Subject lineage。
- Reliability Head 变化或 freshness 到期后，旧 Decision 立即失效并入队重新求值。
- 普通 Waiver 不能覆盖不可豁免规则。
- Break Glass 不能覆盖 hard stop，P0 safety 只能回滚到已有可信 Release。
- expired、superseded 和 scope mismatch Waiver 无效。
- 30 个 synthetic pair 生成可复算 Delta、CI、Evidence Hash 和 Decision。
- legacy Run、未 sealed Evidence 和旧 Aggregate 得到 `insufficient_evidence`。
- Release Authorization 在 Head 变化、过期、重复消费和 Subject mismatch 时均被拒绝。

### 19.5 Migration

- 从 migration 196 升级到 197、198、199，并逐阶段验证旧应用与新应用的兼容窗口。
- PairSpec、SideSpec 与 Pair Binding 缺行、hash 篡改和额外 treatment 差异都会成为 `legacy-unbound` 或 `invalid_pair`。
- 验证所有 check、trigger、partial unique index 和 FK。
- 验证历史数据未被标记为 sealed 或 current trusted revision。
- 验证 migration checksum 与运行文档一致。
- 验证 rolling deployment 期间旧应用写入得到受控错误，不产生部分可信记录。
- 验证 197 在 audit 阶段记录旧 writer 但允许写入，clean window 后可以完整 drain、closed、enforce 和 reopen。
- 验证 197 guard 启用前可以回滚旧 writer，启用后旧 protocol 被拒绝且兼容镜像能够恢复写入。
- 验证 migration 198 阶段不能产生可用于发布的 trusted Decision，199 后才允许 deterministic Gate acceptance。
- 验证 198 与 199 cutover 会等待 Gateway、Worker、Gate writer、scheduler、reaper、replacement 和 outbox writer 全部归零。
- 验证低于目标 protocol 的 writer 在恢复后仍被拒绝，兼容版本可以正常写入。

## 20. 可观测性与审计

每个关键操作记录 correlation ID、Run ID、Job ID、Worker ID、state version、control epoch、input hash、revision 和有限 reason code。

最小指标如下。

```text
radar_run_transition_total
radar_run_fenced_write_total
radar_worker_token_rotation_total
radar_route_evidence_finalize_total
radar_route_evidence_finalize_conflict_total
radar_route_evidence_terminalization_lag_seconds
radar_request_semantics_rejected_total
radar_score_head_advance_total
radar_stale_score_submission_total
radar_analysis_superseded_total
radar_aggregate_revision_total
radar_revision_batch_blocked_total
radar_outbox_lag_seconds
radar_outbox_dead_letter_total
radar_gate_decision_total
radar_gate_supersession_total
radar_gate_insufficient_total
radar_release_authorization_rejected_total
radar_writer_protocol_audit_violation_total
radar_writer_cutover_blocked_total
```

token、Prompt、Completion、HMAC key、真实账号与真实渠道不能作为 label 或日志字段。

## 21. 下游工作包接口

### 21.1 企业治理控制台

企业治理规格必须定义 tenant ownership、scope authorizer、权限驱动导航、Gate Detail、Worker lifecycle、审批、Waiver、Alert timeline、Audit、页面状态矩阵、WCAG 2.2 AA 和移动端处置流程。

### 21.2 Benchmark 与安全 Adapter

Adapter 规格必须输出版本化 Case、PairSpec、Request Manifest、Request Semantics verifier、Evidence Manifest 和 Grader identity。可执行评分使用默认拒绝网络、受控依赖、资源上限、进程树清理和出站审计。

### 21.3 性能与可靠性

性能规格必须输出 Reliability Snapshot，包含多租户负载模型、直方图、错误分母、计费幂等和数据新鲜度。

### 21.4 精调训练

训练规格必须输出 model artifact digest、dataset manifest、training manifest、checkpoint lineage 和 Release Subject，并自动创建 baseline/candidate Radar Run。

### 21.5 Agent 与插件

Agent 规格必须输出 expected request manifest、工具调用序列、权限证据、沙箱 identity、资源账本和可执行评分。多轮 Route Evidence 继续使用 Assignment 加 request ordinal 合同。越权调用属于不可豁免 P0。

## 22. G1 出口条件

G1 只有在以下合同都通过评审后成立。

1. Run 状态图、预算模式、pause、cancel 和 fencing 无歧义。
2. Request Manifest、Request Semantics、PairSpec、SideSpec 与 Pair Binding 有不可变持久化和独立复算合同。
3. Route Evidence identity、lease epoch、Envelope canonicalization、Finalize、seal、hash 与 key rotation 可执行。
4. ScoreRef、SnapshotRef、Score Head、cell revision、global revision 与 stale job 规则完整。
5. Revision Batch、outbox causation 与 regrade 强制传播链可以收敛。
6. Gate Evidence、Release Subject、Policy Head、Baseline Head、Reliability Head、Decision Head、Release Authorization、supersession 与 Waiver 边界完整。
7. Migration 197、198、199 顺序、writer quiescence 与兼容窗口明确。
8. 每个行为都有失败测试、并发测试、迁移测试和 30-pair 验收证据。
9. 下游五个工作包可以通过稳定接口接入，无需改写可信证据语义。
