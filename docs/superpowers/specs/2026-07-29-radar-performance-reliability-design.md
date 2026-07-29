# Sub2API 雷达性能、可靠性、混沌与容灾设计规格

- 状态：P2.5 独立工作包设计
- 日期：2026-07-29
- 目标平台：`sub2api`
- 依赖：G1 可信执行合同、全平台模型质量雷达设计总包

## 1. 设计目标

本工作包负责回答四类问题：推理性能是否回归，服务可靠性是否恶化，多租户压力是否破坏公平性，故障恢复是否保持证据与账本一致。所有结果都以受控评测租户和版本化窗口为边界。

能力 Aggregate 只表示模型结果质量。性能和可靠性结果写入独立的 `Reliability Snapshot`，由 Gate 同时读取，避免单一总分掩盖 SLO 失败。

### 1.1 范围

1. TTFT、TPOT、端到端延迟、吞吐、Goodput、错误率和单位成本。
2. 多租户并发、限流、队列、公平性、渠道熔断和计费幂等。
3. Redis、PostgreSQL、Worker、上游、GPU、对象存储和区域切换的故障演练。
4. RPO、RTO、租约回收、重复提交防护和恢复后确定性验收。

### 1.2 非目标

1. 负载发生器不修改生产路由、不改变客户预算、不读取客户原始数据。
2. 混沌控制器不拥有发布权限，也不能绕过 Gate 或直接修复业务数据。
3. 性能快照不能替代能力评分、路由证据或发布审批。

## 2. 组件边界

| 组件 | 输入 | 输出 | 权限边界 |
| --- | --- | --- | --- |
| Load Plan Registry | 负载模型、路由、租户和预算 | 不可变 Load Plan | 只能发布和停用计划 |
| Load Coordinator | Load Plan、Run 和环境 | 分层请求流、窗口事件 | 只能使用评测身份 |
| Metric Collector | Trace、SSE、GPU、账单和网关指标 | 原始直方图、计数和分位数 | 不保存 Prompt 或 Completion 正文 |
| Reliability Publisher | 冻结窗口、错误分母、直方图 | `Reliability Snapshot` 与 Head | 不能修改历史快照 |
| Chaos Controller | 已批准实验、护栏和环境 | Fault Experiment、Recovery Evidence | 只能操作声明范围 |
| Recovery Verifier | 备份、恢复实例和验收 Run | RPO/RTO 与完整性报告 | 不能覆盖恢复前数据 |
| Gate Adapter | Snapshot、Policy、Evidence Manifest | 可靠性规则结果 | 不能自行发布或回滚 |

## 3. Load Plan 合同

Load Plan 使用 RFC 8785 canonical JSON 计算 `load_plan_sha256`。发布后字段不可更新，修改任何字段都创建新版本。

```json
{
  "schema_version": "radar-load-plan-v1",
  "tenant_class": "evaluation",
  "environment": "staging",
  "route_profile_version": "route-v42",
  "model_aliases": ["deepseek-chat"],
  "regions": ["cn-east"],
  "traffic_mode": "streaming",
  "concurrency_levels": [1, 10, 50, 100],
  "input_token_buckets": [128, 2048, 8192],
  "output_token_buckets": [64, 512, 2048],
  "warmup_seconds": 120,
  "measurement_seconds": 600,
  "minimum_valid_requests": 100,
  "max_run_cost": "10.00000000",
  "max_concurrency": 100,
  "client_image_digest": "sha256:...",
  "generator_version": "loadgen-v1"
}
```

服务端重新计算 hash，并校验模型、区域、评测 Key、预算和渠道均属于该计划。负载发生器提交的统计值只用于诊断，Gate 读取由控制面和网关独立生成的快照。

## 4. 指标与快照

### 4.1 指标定义

| 指标 | 口径 | 必须切片 |
| --- | --- | --- |
| TTFT | 请求接收至首个有效 token | 模型、区域、流式模式、输入桶、并发档位 |
| TPOT | 首 token 后相邻 token 的间隔 | 模型、区域、输出桶、并发档位 |
| E2E | 请求接收至完成事件或明确失败 | 模型、区域、请求模式、租户等级 |
| P99 | 完整窗口的 99 分位 | 上述全部维度，保留原始直方图 |
| Goodput | 同时满足质量和 SLO 的成功请求数 | Run、能力域、窗口 |
| Error Rate | 错误请求数除以进入窗口的请求数 | 错误类别和上游 attempt |
| Cost per Success | 账单金额除以有效成功请求数 | 模型、渠道、区域、租户等级 |

延迟以网关和推理引擎分别计算。全局平均值不能替代分层 P99。流式请求必须记录首 token、每个有效 token 的累计计数和终止事件，断流进入明确的协议或上游错误分类。

### 4.2 Reliability Snapshot

Snapshot 至少包含以下字段：

```json
{
  "snapshot_id": "...",
  "snapshot_created_at": "...",
  "run_id": "...",
  "load_plan_id": "...",
  "load_plan_sha256": "...",
  "window_start": "...",
  "window_end": "...",
  "source_watermark": "...",
  "query_version": "reliability-query-v1",
  "slice_key": "model=deepseek-chat|region=cn-east|concurrency=50",
  "request_count": 0,
  "success_count": 0,
  "error_count": 0,
  "timeout_count": 0,
  "retry_count": 0,
  "protocol_error_count": 0,
  "billing_idempotency_failures": 0,
  "ttft_histogram_hash": "...",
  "latency_histogram_hash": "...",
  "p99_latency_ms": 0,
  "error_rate": "0.000000",
  "cost_amount": "0.00000000",
  "fresh_until": "...",
  "snapshot_hash": "..."
}
```

`request_count` 是错误率分母，不能只统计成功请求。每个直方图以固定 bucket、计数、总和和最高值 canonicalize 后计算 hash。Snapshot 发布事务同时写 Head 和 outbox，旧 Head 保留为历史记录。

### 4.3 新鲜度与缺失语义

窗口未结束、source watermark 不完整、直方图缺失、计费事件未对账或超过 `fresh_until` 时，Snapshot 不能进入 Gate。Gate 结果为 `insufficient_evidence`，并保留缺失字段和 source watermark。

## 5. 性能回归规则

每个矩阵格点先执行 warmup，再执行固定时长或最小有效请求数。负载发生器丢包、客户端超时和控制面故障单独计数，不能从分母中静默删除。

默认规则由 Policy 配置：

```text
p99_latency <= baseline_p99 * (1 + allowed_regression_ratio)
ttft_p99 <= route_specific_slo
error_rate <= baseline_error_rate + allowed_error_delta
throughput >= baseline_throughput * minimum_retention_ratio
cost_per_success <= approved_cost_limit
```

Baseline 与 candidate 必须使用相同 Load Plan、题集、路由身份、区域和 warmup 规则。若质量下降、延迟改善，两个维度都保留原始结果，由 Gate Policy 表达业务取舍。

## 6. 多租户压测与预算隔离

负载模型至少包含小租户长尾、头部租户突发、不同 API Key、不同模型、流式与非流式混合。每个请求绑定 `tenant_id`、`evaluation_key_id`、`route_trace_id` 和 `load_cell_id`。

必须验证：

1. 限流拒绝准确，拒绝请求不产生模型用量。
2. 重试有界，已收到的 token 只按账本规则结算一次。
3. 单租户突发不能挤占其他评测租户的并发槽和预算。
4. 达到 80% 预算触发告警，达到 100% 后只保留 P0 哨兵。
5. 负载发生器、专用渠道和评测 API Key 的账本完全可分离。

公平性使用每个租户的排队时间、成功率和配额利用率报告。全局吞吐上升不能掩盖单租户饥饿。

## 7. 混沌实验合同

每次实验先写入不可变 `FaultExperiment`，由安全负责人和值班负责人审批。实验至少包含：

```json
{
  "experiment_id": "...",
  "environment": "staging",
  "fault_type": "worker_crash",
  "target_selector": {"worker_id": "..."},
  "blast_radius": "one_worker",
  "start_deadline": "...",
  "stop_conditions": ["customer_error_rate_delta_gt_0.005"],
  "rollback_action": "revoke_fault",
  "approvers": ["...", "..."],
  "experiment_policy_hash": "..."
}
```

场景集合：

| 层级 | 场景 | 关键验证 |
| --- | --- | --- |
| 依赖 | Redis 不可用、数据库切换、对象存储失败 | fail-close、PITR、artifact 引用一致 |
| 调度 | Worker 崩溃、租约过期、网络隔离、时钟偏移 | fencing、回收、重复提交防护 |
| 上游 | 429、5xx、慢响应、断流 | 重试上限、错误分类、计费幂等 |
| 资源 | GPU OOM、队列拥塞、区域容量下降 | 路由降级、预算保护、客户隔离 |

生产实验的初始范围只允许单 Worker 进程终止或单 Worker 网络隔离。数据库切换、对象存储故障、时钟偏移和区域切换必须先在同构预生产完成两次成功演练。

### 7.1 停止护栏

实验自动停止条件包括：客户错误率上升 0.5 个百分点、客户 P99 上升 20%、控制面可用性低于 99.9%、数据 hash 不一致、告警链路失效或预算账本不一致。停止后先撤销故障，再验证 lease fencing、Score 不重复、账本幂等和对象引用一致。

## 8. 容灾与恢复验证

| 数据 | RPO | RTO | 恢复要求 |
| --- | --- | --- | --- |
| 控制面元数据 | 5 分钟 | 30 分钟 | PITR、迁移校验、幂等恢复 |
| immutable Score 与 Aggregate | 5 分钟 | 30 分钟 | 哈希校验、禁止覆盖、Head 重建 |
| Evidence Manifest | 5 分钟 | 30 分钟 | 数据库与对象存储引用一致 |
| 临时 artifact | 24 小时 | 4 小时 | 可丢弃，不影响已确认评分 |
| Policy 与配置 | 5 分钟 | 30 分钟 | 版本、审批和激活记录完整 |

RPO 从最后可用的持久化事务和对象版本计算。RTO 从宣布切换开始，直到控制面恢复、Worker 重新注册、确定性验收和必要审批完成。缺失对象版本、checksum 不符、重复 immutable 记录、无法追踪的 Policy 版本或过期备份证据都会使演练失败。

## 9. Gate 集成

Gate 按以下顺序消费可靠性结果：

1. 校验 Snapshot 的 Run、Load Plan、Release Subject 和 route profile 身份。
2. 校验窗口、source watermark、query version 和 `fresh_until`。
3. 校验错误率分母、直方图 hash、计费幂等和切片覆盖率。
4. 按 Policy 比较 P99、TTFT、吞吐、Goodput、错误率和成本。
5. 任何 hard stop 进入 `blocked` 或 `insufficient_evidence`，能力分数只能作为诊断信息。

Reliability Snapshot 变化会通过 outbox 触发 Gate 重新求值。旧 Decision 不因新快照自动延长有效期。

## 10. 安全与数据治理

负载发生器使用短时效工作负载身份和专用评测 Key。日志只保存 trace、切片、计数和 hash。Prompt、Completion、隐藏推理、客户账号和原始渠道标识不进入指标标签、告警和导出报告。混沌控制器必须有独立 RBAC、审批、变更单和实时停止通道。

## 11. 验收矩阵

| 层级 | 最小证据 |
| --- | --- |
| 契约 | Load Plan、Snapshot、Fault Experiment canonical bytes 与 hash 可复算 |
| 性能 | 并发 1、10、50、100、峰值和峰值两倍的分层直方图与 P99 |
| 多租户 | 至少三个租户的限流、公平性、预算和计费报告 |
| 混沌 | 每类至少一个场景，含注入、停止、恢复、用户影响和未解决项 |
| 容灾 | PITR、Worker 重注册、lease 回收、Score 不重复和 RPO/RTO 实测 |
| Gate | 过期 Snapshot、缺失分母、P99 超标、成本超限分别触发预期规则 |

### 11.1 完成定义

1. 所有指标可从网关、引擎和账本的独立来源复算。
2. 任何可靠性结论都能关联 Snapshot、slice、source watermark 和 Policy 版本。
3. 故障演练不会改变已封存 Evidence、Score、Aggregate 或账本事实。
4. 恢复后完成一次 30-pair deterministic acceptance，结果与灾难前 hash 一致。
5. 生产实验具备审批、护栏、停止、回滚和复盘证据。

## 12. 默认决策

| 项目 | 默认值 | 变更要求 |
| --- | --- | --- |
| 直方图 | 固定 bucket、计数、总和、最高值 | 需要 query version 与双跑校准 |
| 统计窗口 | 10 分钟测量，5 分钟滚动新鲜度 | 需要 SRE 与质量负责人批准 |
| 生产压测 | 低速 Canary，专用渠道 | 需要业务负责人、安全负责人和值班负责人批准 |
| 自动熔断 | P3 前关闭 | 需要两次成功演练和 Release Manager 批准 |
| ClickHouse | 样本超过 1000 万或事务查询受影响后评估 | 需要架构评审 |
