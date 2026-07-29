# Sub2API 全平台模型质量雷达设计总包

- 状态：基于已确认的模型质量雷达规格与 G1 可信执行合同整理
- 日期：2026-07-29
- 目标平台：`sub2api`
- 读者：技术负责人、平台研发、质量团队、SRE、安全、训练平台和企业客户产品团队
- 配套规格：
  - [模型质量雷达设计](2026-07-25-sub2api-model-quality-radar-design.md)
  - [可信执行核心 G1 合同](2026-07-27-radar-trusted-execution-contracts-design.md)
  - [性能、可靠性、混沌与容灾设计](2026-07-29-radar-performance-reliability-design.md)
  - [精调训练集成设计](2026-07-29-radar-finetune-integration-design.md)
  - [Agent、插件与 Coding Plan 评测设计](2026-07-29-radar-agent-plugin-evaluation-design.md)
  - [生产运行手册](../../radar-production-runbook.md)

## 1. 文档定位

这份文档把已有设计拆成可交付的阶段，并为五个下游工作包定义稳定接口。它提供统一的目标、边界、数据流、决策规则、验收证据和上线顺序。

核心判断链如下：

```text
版本与路由身份
  -> 固定题集与成对样本
  -> 受控 Worker 执行
  -> 网关 Route Evidence
  -> 独立评分与统计
  -> 质量、可靠性、成本三类结论
  -> Gate Decision
  -> 发布授权或人工处置
```

能力分数只能回答模型结果质量。可靠性、路由身份、成本和证据完整性保持独立维度，任何一项失败都必须留下可审计结论。

## 2. 设计路线与选择

### 2.1 备选路线

| 路线 | 形态 | 优点 | 代价 | 结论 |
| --- | --- | --- | --- | --- |
| A | 接入外部评测 SaaS | 上手快，Benchmark 资源丰富 | 无法天然绑定 sub2api 路由、计费和租户证据 | 仅作为 Adapter 来源 |
| B | 独立质量平台 | 运行隔离清晰，便于多网关复用 | 重复建设租约、权限、预算、审计和发布治理 | 暂不采用 |
| C | 嵌入 sub2api 控制面，独立受控 Worker | 证据链完整，复用现有身份和路由，治理集中 | 需要严格的数据库边界与 Worker 隔离 | 采用 |

### 2.2 采用路线的约束

1. 控制面是运行、证据、评分、统计和 Gate 的事实源。
2. Worker 只能通过内部协议领取租约和提交证据，不能直接写业务数据库。
3. Gateway 生成 Route Evidence，Worker 无权自报实际路由、费用或通过结论。
4. 外部工具只实现 Adapter，不拥有基线、门禁、租户和发布状态。
5. 任何证据不足都进入 `insufficient_evidence`，不能被默认解释为模型退化。

## 3. 总体阶段图

```text
G0 目标与边界确认
  -> G1 可信执行合同
  -> P0 路由证据与评测隔离
  -> P1 受控 Worker、评分、统计
  -> P1.5 真实 30-pair 验收与观察期
  -> P2 Gate、基线、告警、控制台治理
  -> P2.5 性能、混沌、容灾和恢复演练
  -> P3 企业题集、训练、Agent 与插件接入
```

### 3.1 阶段出口

| 阶段 | 目标 | 必须证明的事实 |
| --- | --- | --- |
| G0 | 范围冻结 | 目标、非目标、责任边界和指标口径已签字 |
| G1 | 可信执行 | Run、租约、Pair、Evidence、Score、Aggregate 和 Gate 的身份可复算 |
| P0 | 可观测基础 | 每个有效样本能关联脱敏 Route Evidence 和计费信息 |
| P1 | MVP 雷达 | 受控 Worker 可完成执行、评分、统计和重试 |
| P1.5 | 真实验收 | 30 个 paired samples 可重复完成，失败分类准确 |
| P2 | 生产治理 | 基线审批、门禁、告警、豁免和发布授权具备审计链 |
| P2.5 | 运营韧性 | 并发、故障注入、容灾恢复和预算保护不污染质量结论 |
| P3 | 企业扩展 | 私有题集、训练产物、Agent 工具调用可使用同一证据合同 |

## 4. 统一组件边界

| 组件 | 输入 | 输出 | 禁止事项 |
| --- | --- | --- | --- |
| Dataset Registry | Case、PairSpec、评分器和执行镜像 | 不可变 Dataset Manifest | 修改已发布题集 |
| Run Orchestrator | Plan、Release Subject、预算和矩阵 | Run、Sample、Assignment、Grading Job | 绕过预算和权限创建任务 |
| Controlled Runner | Assignment、Request Manifest | 最终响应、工具事件、执行 Evidence | 自报 score、路由和费用 |
| Gateway Evidence | Evaluation Context、请求和 transport 事件 | Route Evidence Envelope | 记录客户 Prompt、凭据和原始账号 |
| Grader | Sealed Evidence、Case、Verifier | Score、Failure Classification | 读取生产数据库或隐藏推理 |
| Statistics | Frozen ScoreRef、Pair Binding | Aggregate Snapshot、Confidence Interval | 消费未封存或过期 Score |
| Reliability Publisher | 网关指标、压测窗口、错误分母 | Reliability Snapshot | 用能力分数覆盖 SLO 失败 |
| Gate Evaluator | Evidence Manifest、Policy、Baseline、Reliability Head | Decision、Release Authorization | 直接修改生产路由 |
| Console | Read API、RBAC、审计事件 | 查询、审批、复跑、处置命令 | 暴露上游凭据和原始敏感证据 |

## 5. 全链路数据合同

### 5.1 Release Subject

一次可发布对象由以下字段构成，并通过 canonical bytes 与 SHA256 固定身份：

```json
{
  "candidate_model_config_sha256": "...",
  "baseline_id": "...",
  "dataset_manifest_sha256": "...",
  "route_profile_version": "...",
  "gateway_image_digest": "...",
  "control_plane_image_digest": "...",
  "runner_image_digests": ["..."],
  "grader_image_digests": ["..."],
  "statistics_image_digests": ["..."],
  "analysis_version": "...",
  "region_set": ["..."],
  "deployment_environment": "staging",
  "scope_type": "global",
  "scope_id": "global"
}
```

集合字段去重排序。服务端重新计算 hash，客户端提交的 hash 只用于校验。

### 5.2 Pair 与 Side

每个 Pair 固定一个能力题和 comparison key，包含 baseline side 与 candidate side。Side 冻结模型别名、路由配置、采样参数、区域和 Request Manifest。

规则：

- 一次 Run 只能绑定一个版本化 Pair Binding。
- baseline 与 candidate 的题目、顺序、环境和评分器相同。
- treatment 数量必须恰为两个，额外 treatment 进入 `invalid_pair`。
- 缺少 side、重复 side、Manifest hash 不一致都不能进入统计。

### 5.3 Route Evidence Envelope

Gateway 在实际分发前创建 open evidence，在 transport、billing 和 fallback 事件完成后封存。Envelope 至少包含：

```json
{
  "run_id": "...",
  "sample_id": "...",
  "assignment_id": "...",
  "request_ordinal": 0,
  "route_trace_id": "...",
  "requested_model": "...",
  "resolved_model": "...",
  "route_profile_version": "...",
  "provider": "...",
  "channel_ref": "hmac:...",
  "account_pool_ref": "hmac:...",
  "region": "...",
  "attempts": 1,
  "finish_reason": "stop",
  "input_tokens": 0,
  "output_tokens": 0,
  "ttft_ms": 0,
  "latency_ms": 0,
  "billed_amount": "0",
  "payload_hash": "...",
  "evidence_revision": 1,
  "sealed_at": "...",
  "payload_hmac": "..."
}
```

Envelope identity字段只能在 CreateOpen 时写入。后续更新使用 revision CAS，只允许把空字段补为确定值，不能清空或改写身份。

### 5.4 Score 与 Aggregate

Score 通过 `(score_id, score_created_at)` 引用。每个 sample 和 grader 只有一个 current Score Head，历史 Score 追加保留。Aggregate 只消费冻结的 ScoreRef 集合，保存输入 hash、score refs、source watermark 和 aggregate hash。

### 5.5 Gate Evidence Manifest

Gate 在 repeatable-read 事务中读取：

- Release Subject
- current Policy、Baseline、Reliability Head
- current Score Head 和 Aggregate Head
- Route Evidence Envelope hash
- Dataset、Verifier、镜像和规则 hash
- Outbox source watermark 与 cause set hash

Manifest 经过 canonicalization、SHA256 和 HMAC 后写入 Decision。任何 Head 变化都会使旧 Decision 失效。

## 6. 降智与降级检测方法

### 6.1 结论类型

雷达输出四种互斥结论：

| 结论 | 含义 | 是否可阻断 |
| --- | --- | --- |
| `regression` | 能力或协议相对基线显著下降 | 按 Policy |
| `reliability_degradation` | 延迟、错误、截断或计费 SLO 下降 | 按 Reliability Policy |
| `insufficient_evidence` | 样本、证据、稳定性或置信度不足 | 不能当作能力退化 |
| `healthy` | 证据完整且未达到任何阈值 | 允许继续 |

### 6.2 能力域统计

对每个 capability domain，使用相同 Pair 的成对结果：

```text
delta = candidate_weighted_score - baseline_weighted_score
CI95  = paired_bootstrap(delta, 10000 resamples, seed=fixed_by_manifest)
```

判定条件同时满足：

1. P0 用例失败或新增协议错误达到规则条件。
2. 有效 paired sample 数达到 domain minimum sample count。
3. CI95 上界低于零，或连续窗口 EWMA/CUSUM 超出批准阈值。
4. Route Evidence、评分器、题集和镜像 hash 完整。

单次错误只产生样本级结论。连续退化需要在相邻窗口中使用相同 Policy 复验。

### 6.3 切片归因

按以下顺序进行归因：

1. 先排除 `invalid_evidence`、Worker 和 verifier 失败。
2. 检查 expected route 与 resolved route 的一致性。
3. 按 model route、provider、channel ref、account pool ref、region、route profile 和 image digest 切片。
4. 对比官方直连或受控 mock 结果，区分上游能力与 Gateway 协议问题。
5. 只有单一切片异常时，标记为局部路由或资源问题；多切片同步下降才进入模型或题集诊断。

归因结果必须保留证据集合和规则版本，不能只保存一段自然语言摘要。

### 6.4 反作弊与稳定性

- 评分器盲评，不把模型名、渠道和 baseline/candidate 顺序传给 Judge。
- Judge 交换答案顺序，分歧超过阈值进入复评。
- 高频重复题使用隐藏变体和低频 rotation set，避免固定答案记忆污染。
- 任何 `retry until pass` 都禁止进入有效分数。
- Seed、采样参数、时区和随机性策略写入 Manifest。

## 7. 五个下游工作包

### 7.1 企业治理控制台

提供 Overview、Models、Runs、Alerts、Gates、Workers、Datasets 七个视图。

关键流程：

```text
创建题集 -> 发布题集 -> 创建 Plan -> 启动 Run
  -> 查看证据 -> 查看统计 -> Gate 求值
  -> 双人审批 -> 发布授权或人工豁免
  -> 诊断复测 -> 关闭告警
```

权限边界：

- Viewer 只能读取脱敏数据。
- Test Operator 可以运行和复跑，不能晋升 Baseline。
- Quality Admin 管理 Dataset、Verifier、Policy 和 Baseline 提案。
- Release Manager 批准 Gate、Waiver 和 Release Authorization。
- Platform Admin 执行渠道隔离、路由回滚和恢复。

所有写操作返回 correlation ID，并产生 append-only audit event。移动端保留 Run 状态、P0 告警、审批和回滚四条处置路径。

### 7.2 Benchmark 与安全 Adapter

Adapter 统一输出：

```text
Case -> PairSpec -> Request Manifest -> Request Semantics Verifier
     -> Evidence Manifest -> Grader identity -> Score
```

Benchmark Adapter 负责数据导入和 verifier 执行。安全 Adapter 额外输出 threat category、attack family、expected safety behavior、over-refusal policy 和人工复核标记。

执行环境默认拒绝网络，使用固定镜像、CPU、内存、进程数和超时上限。任何外部依赖必须进入 lockfile 和镜像 digest。Adapter 不能直接调用生产凭据。

### 7.3 性能、可靠性、混沌与容灾

Reliability Snapshot 与能力 Aggregate 分开生成，至少包含：

- request、success、error、timeout 和 retry 分母
- TTFT、latency、tokens/s 的完整直方图
- SSE 截断率、协议错误率和计费幂等率
- 多租户并发模型、区域、route profile 和 worker image
- 窗口起止、样本新鲜度和输入 watermark

混沌场景分三层：

1. 依赖层：Redis、PostgreSQL、上游超时、429、断流。
2. 调度层：Worker 崩溃、租约过期、重复通知、网络隔离。
3. 区域层：路由切换、单区域不可用、恢复和 failback。

每次演练都要验证：客户流量隔离、已完成 Score 不重复、未完成 Lease 可回收、告警可达、证据 hash 不变、预算账本一致。

### 7.4 精调训练接入

训练平台在训练任务提交和产出时生成：

```text
dataset_manifest_sha256
training_manifest_sha256
model_artifact_digest
checkpoint_lineage
runtime_image_digest
evaluation_policy_id
```

训练任务完成后自动创建 candidate Release Subject 和 Radar Run。训练平台不直接写 Score 或 Gate，只调用 Run API。训练失败属于 infra/training failure，不能伪装成模型能力失败。SFT、RLHF 和后训练策略保留为 Subject 字段，便于按训练 lineage 归因。

### 7.5 Agent、插件与 Coding Plan

Agent Adapter 扩展 Request Manifest：

- 预期工具集合和 schema hash
- 调用顺序与最大步骤数
- 每个工具的权限 scope
- 沙箱 image、文件系统和网络策略
- token、时间、进程和费用账本

越权调用、未注册工具、参数 schema 破坏和沙箱逃逸属于不可豁免 P0。多轮执行继续使用 `assignment_id + request_ordinal + lease_epoch`，每个工具事件都可关联到 Route Evidence。

## 8. Gate、豁免与发布治理

### 8.1 固定短路顺序

Gate Evaluator 按以下顺序求值：

1. Tenant、scope 和 Release Subject 身份
2. Route Identity、Evidence Integrity 和签名状态
3. 数据集、评分器、镜像和 Policy hash
4. P0 安全、协议、Tool Call、计费和路由规则
5. Reliability Snapshot 新鲜度与 SLO
6. 样本量、稳定性和统计置信区间
7. 能力域差值、趋势和综合规则
8. 已批准 Waiver 与 Release Authorization

上游 hard stop 失败时，后续统计只用于诊断，不能把结论改成通过。

### 8.2 Waiver 边界

普通 Waiver 必须包含原因、风险接受人、缓解措施、失效时间和补测 Run。它不能覆盖 Evidence Integrity、Tenant Boundary、生产凭据、PITR、回滚能力和 P0 安全越权。

Break Glass 只允许在已有可信 Release 上执行回滚或缩小流量，不得授权未知候选版本上线。所有 Waiver 追加写，查询投影可以显示 `waived`，事实 Decision 仍保留原始阻断原因。

### 8.3 发布授权

Release Authorization 是短时效单次令牌，绑定：

```text
release_subject_hash + decision_id + source_watermark + actor_id + expires_at
```

消费时重新锁定 current Head。任何 Policy、Baseline、Reliability、Aggregate 或 Route Evidence 变化都会拒绝消费并要求重新求值。

## 9. SLO、容量与预算

### 9.1 雷达平台 SLO

| 指标 | 初始目标 |
| --- | --- |
| 15 分钟哨兵按时完成率 | >= 95% |
| Route Evidence 关联成功率 | >= 99.9% |
| Score 可见延迟 | 99% <= 10 分钟 |
| P0 告警通知延迟 | <= 5 分钟 |
| 重复计分率 | 0 |
| Outbox dead letter | 0 个未处置 |

### 9.2 质量与成本指标

每个模型和能力域同时展示覆盖率、有效样本率、基础设施失败率、用例不稳定率、误报率、漏报演练结果、单次运行成本和预算消耗。能力通过不能消除可靠性告警，可靠性通过也不能消除质量回归。

### 9.3 容量保护

- 评测租户、API Key、Group、RPM、并发和预算全部独立。
- 80% 预算触发告警，100% 停止低优先级任务，保留 P0 哨兵。
- 生产只允许低速 Canary。压测和混沌在专用环境或专用渠道执行。
- Worker 扩容必须受 evaluation quota 控制，不能抢占客户保留容量。

## 10. 生产安全与数据治理

生产启用前必须具备：

1. 非 root runtime、只读根文件系统和显式 writable paths。
2. Redis 鉴权，数据库、缓存、上游和 Worker trust network 分离。
3. mTLS 或等价 workload identity 与 bearer token 双重校验。
4. 外部 secret manager，不在镜像层、参数、日志和普通环境检查中出现密钥。
5. S3/MinIO quarantine、HEAD、SHA256、恶意扫描、保留和删除事件。
6. PostgreSQL PITR、对象版本化、跨故障域复制和季度恢复演练。
7. 原始响应、公共题 artifact、机密复放数据采用分层保留策略。
8. Prompt、Completion、隐藏推理、原始账号和渠道标识不进入 Dashboard、label 或告警。

## 11. 测试与验收矩阵

| 层级 | 覆盖内容 | 证据 |
| --- | --- | --- |
| 单元 | canonical hash、统计、阈值、状态机、RBAC | 测试结果与 fixture hash |
| 契约 | OpenAI、Anthropic、Gemini、SSE、Worker API | 协议回放报告 |
| 集成 | PostgreSQL、Redis、上游 mock、账单、路由 fallback | 集成日志和 SQL 断言 |
| 竞态 | 重复 submit、租约过期、rows 并发、outbox 重试 | race test 和 idempotency report |
| E2E | 30 pair、baseline/candidate、Gate、Alert、Recovery | Run ID、Score、Aggregate、Decision |
| 压测 | 多租户、P99、限流、成本和队列积压 | k6/直方图和预算报告 |
| 混沌 | Worker、DB、Redis、上游、区域故障 | 演练记录和恢复时间 |
| 安全 | 凭据泄漏、越权工具、跨租户、旧 token | red-team negative matrix |
| 生产 | 灰度、回滚、PITR、failover、审批 | 签名验收报告 |

### 11.1 最小 deterministic acceptance

固定 3 个 reasoning case，每个 `sample_count=10`，形成 30 个 baseline/candidate pairs。Baseline 返回 `Paris`，candidate 返回 `Lyon`，exact grader 期望 `Paris`。

必须观测：

- 60 个 grading jobs 完成
- 60 个 Route Evidence sealed
- 60 个 Score
- 2 个 analysis jobs 完成
- 两个 Aggregate 都覆盖 30 个 paired samples
- baseline mean 为 1，candidate mean 为 0，paired delta 为 -100 个百分点
- 无失败任务、无活跃 lease、无重复 current Score

### 11.2 生产 Gate acceptance

除 deterministic acceptance 外，生产 Gate 还要证明：

- 缺失 Evidence、旧 Head、错误 scope、过期 Policy 都得到 `insufficient_evidence` 或 `blocked`。
- 旧 Decision 在任一 current Head 变化后立即失效。
- Waiver、Break Glass 和 Release Authorization 的权限、有效期、审计与 hard stop 边界生效。
- worker drain、token rotation、PITR restore、区域 failover 和 rollback 均完成一次演练。

## 12. 交付顺序与责任

```text
平台控制面与数据库
  -> Gateway Evidence 与 Worker protocol
  -> Grader/Statistics/Outbox
  -> 30-pair staging acceptance
  -> Console、Gate、Alert 与基线治理
  -> Reliability/Chaos/DR
  -> Training、Agent、Plugin adapters
```

| 工作流 | 责任团队 | 依赖 | 出口 |
| --- | --- | --- | --- |
| P0/P1 核心 | 平台研发、质量 | G1、PostgreSQL、Redis | 30-pair Run |
| Gate 与治理 | 质量平台、Release | Score/Aggregate Head | Trusted Decision |
| 前端控制台 | 前端、产品 | Read API、RBAC | 页面与审批 E2E |
| 性能与混沌 | SRE、网络 | Reliability Snapshot | 恢复演练报告 |
| 训练接入 | 训练平台、质量 | Release Subject、Run API | 训练产物回归 |
| Agent/插件 | 应用平台、安全 | Request Semantics、Tool Evidence | P0 越权验收 |

## 13. 已接受的实施基线

以下决策于 2026-07-29 获得项目方接受，作为实施计划和验收的当前基线。后续变更必须由对应角色批准并创建新版本 Policy 或规格修订记录。

| 项目 | 已接受决策 | 后续变更批准角色 |
| --- | --- | --- |
| 观察期长度 | 14 个完整日 | Quality Admin、SRE |
| 能力域最低样本量 | 按 domain policy 配置，P0 单独计数 | Quality Admin |
| 数据保留 | 治理 400 天，公共 raw 30 天，机密 replay 7 天 | Security、合规 |
| 生产自动熔断 | P3 才开放，默认人工确认 | Platform Admin、SRE |
| ClickHouse | 有效样本超过 1000 万或事务库趋势查询受影响后引入 | Platform Architect |
| 私有题集 | P3，以租户隔离和加密为前置 | Enterprise Product |

## 14. 完成定义

设计阶段完成需要同时满足：

1. G1 合同、P0/P1/P2/P3 阶段和下游五个工作包边界无冲突。
2. 每个工作包都有输入、输出、权限、失败语义和验收证据。
3. 降智判定能区分能力、可靠性、路由、证据和基础设施异常。
4. Gate、Waiver、Release Authorization 和 Head 变化形成完整治理闭环。
5. 生产安全、SLO、容量、混沌、容灾、回滚和数据保留均有明确出口。
6. 30-pair deterministic acceptance、生产 Gate acceptance 和 recovery drill 都可被独立复核。
7. 后续实施计划只需引用本总包和配套规格，不需要重新解释证据语义。

配套工作包规格的责任边界如下：

| 工作包 | 配套规格 | 主要出口 |
| --- | --- | --- |
| 性能、可靠性、混沌与容灾 | [规格](2026-07-29-radar-performance-reliability-design.md)、[计划](../plans/2026-07-29-radar-performance-reliability.md) | Load Plan、Reliability Snapshot、Fault Experiment、RPO/RTO 报告 |
| 精调训练集成 | [规格](2026-07-29-radar-finetune-integration-design.md)、[计划](../plans/2026-07-29-radar-finetune-integration.md) | Dataset、Training、Checkpoint、Model Artifact、自动 Radar Run |
| Agent、插件与 Coding Plan | [规格](2026-07-29-radar-agent-plugin-evaluation-design.md)、[计划](../plans/2026-07-29-radar-agent-plugin-evaluation.md) | Tool Manifest、Sandbox Attestation、Tool Evidence、可执行评分 |
