# Radar Evaluation Outbox Consumer 设计

- 状态：用户已于 2026-08-01 确认
- 日期：2026-08-01
- 继承规格：[Radar 可信执行核心 G1 合同设计](2026-07-27-radar-trusted-execution-contracts-design.md)
- 适用范围：`evaluation_outbox_events` 生产消费、Aggregate 传播、Gate 重评估、Release 与 Alert 投影、Run 收敛
- 首个验收 Run：`2719e76a-f573-4c89-bc6c-2c07d1ad8d68`

## 1. 背景

Radar 已经在 Score Head、Aggregate Head 和 Route Evidence 封存事务中写入 `evaluation_outbox_events`，也已具备 claim、heartbeat、complete、dead letter、replay 和 revision batch fencing。当前缺少生产 consumer，导致事件长期停留在 `pending`，Analysis Job 无法自动创建，Run 无法完成。

当前 staging Run 已生成 60 个 Score 和 120 个 pending 事件，Analysis Job 与 Aggregate 数量仍为 0。本设计补齐以下自动传播链路。

```text
route evidence sealed
  -> score head
  -> cell analysis
  -> global analysis
  -> gate reevaluation
  -> release and alert projection
  -> run reconciliation
```

## 2. 目标与边界

### 2.1 目标

1. 自动消费四类现有生产事件。
2. 保持 at-least-once 投递下的领域幂等。
3. 对依赖未就绪、临时故障、永久故障和 fencing 使用不同处置策略。
4. 保证多父事件在所有 cause 完成后才可领取。
5. 自动完成无 Gate 配置的 Run。
6. 对有效 Gate 配置执行可信证据加载、求值、决策和投影。
7. 让 initial Run 与 regrade revision batch 对 dead letter 显式失败。
8. 支持多实例部署、优雅停止、租约接管和无迁移回滚。

### 2.2 本次不包含

1. 修改 Statistics Worker 的聚合算法。
2. 修改 Gate 质量阈值或可靠性计算口径。
3. 新增消息中间件。
4. 自动授权发布或自动部署 Release Subject。
5. 回填历史 `legacy-unbound` Run。

## 3. 方案选择

本次采用单一 `EvaluationOutboxConsumerRuntime` 加事件处理器注册表。

四个独立 runtime 会重复实现调度、租约、重试和停止流程。数据库触发器方案会将领域编排沉入 SQL，增加测试与演进成本。单 runtime 共享基础机制，各处理器仍保有清晰边界，与现有 `RouteEvidenceTerminalizationRuntime` 生命周期模式一致。

## 4. 组件设计

### 4.1 EvaluationOutboxConsumerRuntime

Runtime 只负责基础设施行为。

1. 启动或加载持久化的内部 consumer worker identity。
2. 定时领取允许的事件类型。
3. 按固定并发上限执行 dispatcher。
4. 在处理期间续租。
5. 根据分类结果调用 Complete、Retry 或 DeadLetter。
6. 记录批次与事件级指标。
7. Stop 时取消调度，等待在途处理退出。

Runtime 不直接包含 Aggregate、Gate 或 Run 领域规则。

### 4.2 EvaluationOutboxDispatcher

Dispatcher 校验事件的通用完整性，然后按 `event_type` 调用处理器。通用校验包括以下内容。

1. `run_id`、`source_type`、`source_id` 和 `source_hash` 完整。
2. payload 是有效 JSON。
3. payload 的 canonical SHA256 等于 `payload_hash`。
4. event type、source type 与 payload schema 匹配。

Dispatcher 返回结构化结果 `complete`、`retry`、`dead_letter` 或 `fenced`。Runtime 依据结果更新 outbox 状态。

### 4.3 EvaluationOutboxRepository 扩展

Repository 增加以下能力。

```go
Retry(ctx, eventID, leaseToken, leaseEpoch, errorCode, delay) error
EnsureConsumerWorker(ctx, name) (uuid.UUID, error)
```

`Retry` 在一个 worker writer transaction 中重新校验 token、lease expiry、lease owner、revision batch status 与 epoch，然后执行以下更新。

```text
status = pending
available_at = transaction_timestamp() + delay
lease_token_hash = null
lease_owner = null
lease_expires_at = null
last_error_code = errorCode
```

`EnsureConsumerWorker` 使用固定名称 `radar-control-plane-outbox` 建立内部 statistics worker。多实例共享该持久身份，每次 claim 仍生成独立 lease token。数据库中的 `lease_owner` 外键始终有效。

Claim 查询增加 cause closure 条件。存在任何未完成 cause 的事件不进入候选集合。

```sql
NOT EXISTS (
  SELECT 1
  FROM evaluation_outbox_event_causes c
  JOIN evaluation_outbox_events parent ON parent.id = c.cause_event_id
  WHERE c.event_id = event.id AND parent.status <> 'completed'
)
```

### 4.4 EvaluationGateProcessor

Gate Processor 使用小接口组合现有 Governance Repository。

1. 解析 Run 当前有效的 Release Subject 与 Policy Head。
2. 无有效目标时返回 `not_applicable`。
3. 有目标时调用 `LoadRadarGateReliability`。
4. 使用服务端加载的 authoritative input 调用 `EvaluateRadarGate`。
5. 构建 canonical evidence envelope 与 evidence hash。
6. 在一个 writer transaction 中记录 decision、推进 decision head、更新 release projection，并观察或关闭 alert projection。

请求 payload 无权提交 Gate 状态、阈值、证据 hash 或 Release Subject identity。

### 4.5 Run Reconciler 扩展

Run Reconciler 的 pending work 增加两类事实。

1. 当前 Run 中状态为 `pending`、`leased` 或 `running` 的 Analysis Job。
2. 当前 Run 中状态为 `pending` 或 `leased` 的受支持 outbox event。

Initial 事件进入 dead letter 时记录 pipeline failure。该失败属于不可恢复状态，Run 进入 `failed` 并递增 `control_epoch`。Regrade 事件继续通过 revision requirement 令对应 batch 失败，completed Run 的历史终态保持不变。

## 5. 事件合同

| Event Type | 有效 Source | 处理动作 | 完成条件 |
| --- | --- | --- | --- |
| `route_evidence_sealed` | `route_evidence` | 校验 sealed evidence 身份与 payload | durable sealed row 与事件一致 |
| `cell_recompute` | `score_head_event` | `EnsureCellAnalysisJob` | Analysis Job 已存在或已创建 |
| `global_recompute` | `aggregate_head` | `EnsureGlobalAnalysisJob` | Global Job 已存在、已创建，或历史单 cell 事件已完成兼容 Gate 处理 |
| `gate_reevaluation` | `aggregate_head` | 解析目标并执行 Gate | 无有效配置，或 decision 与投影已提交 |

### 5.1 route_evidence_sealed

Route Evidence 已在生产事件的同一事务中封存。处理器读取 durable row，核对 trace ID、schema version、evidence revision 和 payload hash。核对成功后完成事件，不重复执行封存。

### 5.2 cell_recompute

处理器从 payload 读取 `capability_domain` 和 `model_route`，并对 route 执行 canonicalization。当前生产事件没有独立保存 Aggregate analysis version。为了消费 staging 已存在的事件，首版使用兼容版本 `v1`。后续 producer 在 payload 中显式写入 `analysis_version`，consumer 优先使用 payload 值，并保留 `v1` fallback。

同一个 cell 的多个 Score Head 事件会调用相同自然幂等入口。完整 baseline/candidate 输入集合只生成一个 Analysis Job。

### 5.3 global_recompute

处理器从 payload 读取 analysis version，并调用 `EnsureGlobalAnalysisJob`。多 cell Run 创建 Global Job。

多 cell Run 在 Global Job 完成并推进 Global Aggregate Head 后生成 `gate_reevaluation`。该事件成为 Run 收敛前的最后一个 outbox barrier。

新产生的单 cell Run 在 Cell Aggregate Head 推进时直接生成 `gate_reevaluation`，跳过 `global_recompute` 与 Global Job。当前 staging 可能已经保存旧版 `global_recompute`。Consumer 发现该事件无需 Global Job 时，使用同一个事件的 cause set 执行兼容 Gate 处理。无 Gate 配置时完成事件并收敛 Run，有有效配置时必须完成 Gate decision 与投影后才完成事件。

### 5.4 gate_reevaluation

无 active Release Subject 或无匹配 Policy Head 表示该 Run 没有 Gate 配置。处理器完成事件并触发 Run Reconciler。

有效 Gate 配置必须 fail closed。Policy approval、Release Subject、Policy Head 或 authority 发生冲突时进入重试，耗尽后进入 dead letter。可信 Loader 成功返回的 `insufficient_evidence` 是有效 Gate 决策，需要持久化并形成 blocked release projection。

## 6. Gate 投影

Gate 写事务使用 outbox event ID 和现有自然键保证幂等。

| Decision Status | Release Projection | Alert 行为 |
| --- | --- | --- |
| `passed` | `pending` | 关闭同 lineage 的自动 Gate 告警 |
| `recorded` | `pending` | 保持 record-only，不开放授权 |
| `blocked` | `blocked` | observe P0 或 P1 告警 |
| `review_required` | `blocked` | observe P1 告警 |
| `insufficient_evidence` | `blocked` | observe P0 `insufficient_evidence` 告警 |

自动求值不产生 `waived`。Waiver 继续由治理 API 与多角色审批流程管理。

告警 cause 通过现有 `AttributeRadarCause` 从 rule ID 和结构化 signal 计算。告警 payload 至少包含 run ID、decision ID、policy ID、rule ID、evidence hash、outbox event ID 和 cause set hash。Release projection 保存 decision ID、release subject hash、source watermark hash、cause set hash 与 `last_outbox_event_id`。

## 7. 租约、并发与停止

默认参数如下。

| 参数 | 值 |
| --- | --- |
| poll interval | 2 秒 |
| claim batch | 16 |
| max concurrency | 4 |
| lease duration | 60 秒 |
| heartbeat interval | 20 秒 |
| handler timeout | 45 秒 |

每个事件拥有独立 heartbeat。Heartbeat 返回 fencing 或 context cancellation 时，处理器取消领域调用并停止后续状态更新。

同一进程通过 atomic guard 防止重叠 poll。多个 API 实例可以同时运行，数据库 row lock、SKIP LOCKED、lease token 与 epoch 负责互斥。Stop 先取消 scheduler，再取消 worker context，最后等待在途 goroutine。未能在停止窗口内完成的 lease 由其他实例在 expiry 后接管。

## 8. 错误分类与重试

### 8.1 依赖未就绪

`ErrAggregatePairsIncomplete` 表示 sibling Score Head 或 Cell Aggregate 尚未齐备。该错误使用 2 秒起步、60 秒封顶的指数退避。事件创建满 24 小时仍未就绪时进入 dead letter，错误码为 `aggregate_dependency_timeout`。

Cause event 未完成由 Claim 查询过滤，不增加 attempt。

### 8.2 临时故障

数据库连接、序列化冲突、临时对象存储或短暂 Governance Head 竞争最多重试 8 次。退避使用 1、2、4、8、16、30、60、60 秒，并加入受控 jitter。耗尽后使用稳定错误码进入 dead letter。

### 8.3 永久故障

以下错误立即进入 dead letter。

1. payload JSON 或 schema 无效。
2. payload hash 不匹配。
3. event type 与 source type 不匹配。
4. durable source identity 与事件冲突。
5. 不受支持的 analysis version。

### 8.4 Fencing

`ErrEvaluationOutboxFenced`、revision batch 非 running、epoch 不匹配和租约失效都返回 `fenced`。Runtime 不再执行 Retry、Complete 或 DeadLetter，当前有效 owner 或后续 batch 负责推进。

### 8.5 进程取消

正常 shutdown 期间的 context cancellation 保留 leased 状态，等待 lease expiry。业务超时且 worker context 仍有效时执行 Retry。

## 9. 幂等与一致性

1. Cell Job 使用 run、domain、canonical route、analysis version 和 input set hash 唯一。
2. Global Job 使用 run、analysis version 和 input set hash 唯一。
3. Gate Decision 使用 run、policy 和 evidence hash 唯一。
4. Decision Head 使用显式 supersedes CAS。
5. Alert 使用 tenant、route、domain、cause 和 policy version 唯一。
6. Release Projection 使用 release subject ID 唯一，并记录最后处理的 outbox event ID。
7. Complete、Retry 与 DeadLetter 都重新校验 lease token 和 epoch。

领域事务成功但 Complete 失败时，事件会再次投递。自然键使重放收敛到已有领域结果。Complete 成功前不推进 initial Run 终态。

## 10. 配置与分阶段上线

新增 `RADAR_OUTBOX_CONSUMER_MODE`。

| 值 | 行为 |
| --- | --- |
| `disabled` | 不启动 consumer |
| `core` | 处理 evidence、cell、global，并完成无 Gate 配置的 gate event |
| `full` | 处理全部事件并执行 Gate 与投影 |

Radar 关闭时忽略该配置并保持停止。Radar 开启且未配置 mode 时默认 `core`，避免首次部署直接启用 release governance 写入。

`core` 遇到有效 Gate target 时将事件延后 60 秒，错误码为 `gate_full_mode_required`。该 rollout wait 不计入普通临时错误的 8 次上限。切换 `full` 后事件继续处理。

## 11. 可观测性

Runtime 每轮输出结构化指标。

```text
selected
claimed
completed
retried
dead_lettered
fenced
oldest_lag_seconds
processing_latency_ms
event_type
error_code
```

以下条件触发高优先级告警。

1. dead letter 数量大于 0。
2. 最老可用事件延迟超过 5 分钟。
3. 同一 error code 连续重试异常增长。
4. consumer 最近成功时间超过 2 个 poll interval。

日志禁止输出 prompt、completion、credential、完整 evidence payload 和 lease token。

## 12. 测试策略

实现按 Red、Green、Refactor 小步推进。

### 12.1 Service 单元测试

1. 四类事件正确 dispatch。
2. payload hash 与 schema 校验。
3. `ErrAggregatePairsIncomplete` 返回 dependency retry。
4. 临时错误按次数退避并最终 dead letter。
5. fencing 不执行后续状态更新。
6. heartbeat 续租并在失败时取消 handler。
7. 并发上限与 poll 防重入。
8. Stop 等待在途任务并取消 scheduler。
9. core 与 full mode 行为。

### 12.2 Repository 集成测试

1. Retry 原子释放 lease 并设置 `available_at`。
2. Retry 拒绝过期 token、错误 owner 和旧 epoch。
3. Claim 跳过 cause 未完成的 child event。
4. parent 完成后 child 可领取。
5. 内部 worker identity 多次启动保持同一 ID。
6. Initial dead letter 形成 Run pipeline failure。
7. Regrade dead letter 令 revision batch 失败。
8. Gate decision、head、release projection 和 alert 在同一事务提交。

### 12.3 端到端测试

现有 `radar_revision_pipeline_e2e_test.go` 移除手工 `completeRadarOutbox` 推进，改为运行真实 consumer cycle。测试覆盖以下路径。

1. Score Head 到 Cell Job。
2. Cell Aggregate 到 Global Job。
3. Global Aggregate 到 Gate。
4. 单 cell Aggregate 直接到 Gate。
5. 历史单 cell `global_recompute` 走兼容 Gate 路径。
6. 无 Gate 配置的 Run 完成。
7. 有 Gate 配置的 decision 与投影。
8. 重复投递保持单一领域结果。
9. revision batch epoch fencing 与 requirement closure。

## 13. Staging 上线与验收

### 13.1 第一阶段

部署同一镜像并设置 `RADAR_OUTBOX_CONSUMER_MODE=core`。观察 Run `2719e76a-f573-4c89-bc6c-2c07d1ad8d68`。

验收条件如下。

1. 原有 120 条 pending 事件全部被处理。
2. Cell 与 Global Analysis Job 正常创建并完成。
3. Aggregate Head 完整生成。
4. 支持的 outbox event 无 pending、leased 和 dead letter。
5. Run 从 running 收敛到 completed。
6. 服务连续健康 60 秒，容器重启次数为 0。

### 13.2 第二阶段

将 mode 切换为 `full`，使用具备有效 Policy、Release Subject 与 Reliability Evidence 的测试 Run 验证以下结果。

1. 自动产生 Gate Decision。
2. Decision Head 指向最新 Decision。
3. Release Projection 状态与 Decision 一致。
4. Alert Projection 与 rule ID 一致。
5. 重复投递不生成重复记录。

## 14. 回滚

本次复用 migration 199 已有表和列，不新增 schema migration。回滚镜像为 `sub2api/radar-control-plane:rollback-pre-grading-reclaim-20260801`。

回滚步骤只替换应用镜像。已完成 outbox event、Analysis Job、Aggregate、Decision 和 Projection 保留。旧镜像不会消费新 pending 事件，事件可以在修复镜像恢复后继续处理。内部 consumer worker row 可以保留，不影响外部 Worker claim。

## 15. 完成定义

1. 设计中的所有单元、集成和端到端测试通过。
2. Backend 完整构建通过。
3. Wire 生成文件与 provider graph 一致。
4. core staging 验收全部满足。
5. full Gate staging 验收全部满足。
6. dead letter、lag、retry 和 processing latency 可观测。
7. 回滚演练确认旧镜像健康启动且数据无损。
