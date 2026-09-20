# Seedance 异步任务持久化与自动结算设计

日期：2026-09-19
目标版本：v0.2.7 定制分支
状态：待用户审核

## 1. 背景

Seedance 创建接口返回异步任务 ID。当前实现把任务归属和创建时计费参数写入 Redis，只有客户端后续查询任务状态时，网关才会观察上游成功结果并记录用量。

因此存在两个确定的欠计费路径：

1. 客户端创建任务后从不查询状态；
2. 任务已经成功，但客户端直接删除任务，网关没有在删除前检查并结算。

Redis 中的待结算快照默认 24 小时过期，服务重启不会主动恢复任务处理。当前文档也明确说明不进行后台轮询。这意味着上游已经产生成本时，本地可能没有用量、余额或配额记录。

## 2. 目标

本设计必须满足以下结果：

- Seedance 创建请求向客户端返回成功前，任务归属与计费快照已经持久化；
- 客户端不查询状态时，服务仍能自动发现终态并结算；
- 服务重启和多实例并发不会遗失任务，也不会重复扣费；
- 删除任务前先确认任务是否已经成功，成功任务先结算再删除；
- 客户端主动查询与后台轮询复用同一套终态处理逻辑；
- 生产服务器保持 2 GB 内存，后台执行器默认单并发、小批量运行；
- 不保存提示词、图片、视频内容、API 密钥、Cookie 或上游凭证；
- 不改变现有公开 Seedance URL、请求体和成功响应格式。

## 3. 范围

### 3.1 本次包含

- 新增通用异步视频计费任务表，数据结构预留 `provider`；
- 新增数据库任务仓库、租约领取和状态转换；
- 新增可恢复的后台 reconciler；
- 抽取不依赖 Gin 响应写入的 Seedance 状态查询客户端；
- 将创建、状态查询、删除和后台轮询接入统一结算服务；
- 增加配置、结构化日志、迁移测试、仓库测试、服务测试和生命周期测试；
- 更新 `docs/seedance-api.md`。

### 3.2 本次不包含

- 不为 Grok 异步视频启用后台轮询；
- 不改变 Grok 当前的 Redis 待结算路径；
- 不新增管理页面或用户页面；
- 不引入新的消息队列、外部定时服务或第三方回调；
- 不修改模型定价规则；
- 不在本阶段部署生产、修改生产数据库或清理生产数据。

底层表和服务按 provider 隔离设计，使后续可以独立评估是否让 Grok 复用，但 v0.2.7 只接受 `provider = 'seedance'` 的任务。

## 4. 方案选择

采用 PostgreSQL 持久任务表和可恢复 reconciler。

不采用 Redis 作为任务事实来源，因为键过期、数据恢复或实例配置错误仍会造成不可恢复的欠计费。Redis 可以继续服务现有请求绑定和兼容逻辑，但不能决定任务是否需要结算。

不采用上游回调，因为当前仓库没有已验证的 Seedance 回调签名、重试和投递保证，无法以此完成可靠结算。

不使用“每个创建请求启动一个 goroutine”的方式，因为进程重启、多实例切换和部署期间会遗失任务。

## 5. 架构

新增三个清晰边界：

1. `AsyncVideoBillingTaskRepository`
   - 创建任务；
   - 按到期时间和租约领取任务；
   - 使用租约令牌和租约代次进行受保护的状态更新；
   - 读取任务归属，用于客户端查询和删除路径。

2. `SeedanceTaskSettlementService`
   - 查询上游状态；
   - 将上游终态转换为统一结算结果；
   - 加载创建时的用户、API Key、分组、账户和订阅归属；
   - 调用现有 `OpenAIGatewayService.RecordUsage`；
   - 根据结果推进持久任务状态。

3. `SeedanceReconcilerRuntime`
   - 通过项目已有的后台调度器周期触发；
   - 小批量领取到期任务；
   - 默认只执行一个上游状态请求；
   - 在停止服务时取消新领取并等待当前请求退出。

Handler 只负责认证、协议适配和返回响应。客户端状态查询、删除请求和后台 reconciler 均调用 `SeedanceTaskSettlementService`，避免出现三套不同的计费判断。

## 6. 数据模型

新增迁移 `backend/migrations/239_async_video_billing_tasks.sql`，创建 `async_video_billing_tasks` 表。

### 6.1 身份与归属字段

- `id BIGSERIAL PRIMARY KEY`
- `provider VARCHAR(32) NOT NULL`
- `upstream_task_id VARCHAR(255) NOT NULL`
- `task_key VARCHAR(320) NOT NULL`
- `user_id BIGINT NOT NULL`
- `api_key_id BIGINT NOT NULL`
- `group_id BIGINT`
- `account_id BIGINT NOT NULL`
- `subscription_id BIGINT`

`task_key` 使用现有 `seedance:<upstream-id>` 形式。唯一约束为 `(provider, upstream_task_id, user_id, api_key_id)`，既防止重复创建本地任务，又保持租户和 API Key 归属隔离。

本表不为这些归属 ID 建立外键，也不设置级联删除，避免异步任务记录改变现有账户删除语义。仓库为结算提供只读的“包含软删除记录”加载方法。若关联主体已经被物理删除，任务进入死信并产生运维错误，不构造虚假主体或静默跳过计费。

### 6.2 计费快照字段

- `model VARCHAR(255) NOT NULL`
- `billing_model VARCHAR(255) NOT NULL`
- `upstream_model VARCHAR(255)`
- `original_model VARCHAR(255)`
- `quota_platform VARCHAR(32)`
- `subscription_billing BOOLEAN NOT NULL`
- `pricing_at TIMESTAMPTZ NOT NULL`
- `request_payload_hash CHAR(64)`
- `inbound_endpoint VARCHAR(255)`
- `upstream_endpoint VARCHAR(255)`
- `created_at TIMESTAMPTZ NOT NULL`

`pricing_at` 固定为创建请求开始时刻，使异步结算跨越峰谷时段时不会改变请求级定价语义。`request_payload_hash` 只保存已有不可逆哈希，不保存原始请求。

后台结算不复制客户端 IP、User-Agent、Session ID、提示词或媒体 URL。后台产生的用量记录允许这些非计费元数据为空。

### 6.3 生命周期与租约字段

- `status VARCHAR(24) NOT NULL`
- `next_poll_at TIMESTAMPTZ NOT NULL`
- `poll_deadline_at TIMESTAMPTZ NOT NULL`
- `attempt_count INT NOT NULL DEFAULT 0`
- `lease_token UUID`
- `lease_epoch BIGINT NOT NULL DEFAULT 0`
- `lease_expires_at TIMESTAMPTZ`
- `last_error_code VARCHAR(80)`
- `last_error_at TIMESTAMPTZ`
- `terminal_at TIMESTAMPTZ`
- `settled_at TIMESTAMPTZ`
- `updated_at TIMESTAMPTZ NOT NULL`

状态限定为：

- `pending`：等待首次或下一次查询；
- `settled`：成功任务已经通过幂等计费链路处理；
- `failed`：上游明确返回失败；
- `cancelled`：上游明确取消或本地删除成功；
- `dead_letter`：无法安全自动处理，需要运维介入。

租约不是单独状态。领取条件为 `status = 'pending'`、`next_poll_at <= NOW()`，且租约为空或已经过期。领取使用 `FOR UPDATE SKIP LOCKED`，写入随机 `lease_token`、递增 `lease_epoch` 和租约到期时间。完成、重试和终态更新必须同时匹配任务 ID、令牌和代次，防止旧执行器覆盖新执行器结果。

领取索引覆盖 `(status, next_poll_at, lease_expires_at)` 的待处理行。

## 7. 创建流程

1. Handler 完成现有认证、配额检查和账户选择；
2. 网关向 Seedance 上游提交创建请求；
3. 上游返回任务 ID 后，保留现有 Redis 绑定以兼容当前查询路径；
4. 同步写入 `async_video_billing_tasks`，包括账户归属和创建时计费快照；
5. 数据库写入成功后，才把上游成功响应返回客户端。

持久化使用唯一约束保证同一个任务重复写入为幂等成功。临时数据库错误进行有限、短间隔重试。

若上游已经接受任务，但持久化最终失败，网关返回服务错误并记录不包含凭证或请求正文的高严重度结构化日志，其中包含本地请求 ID、上游任务 ID、账户 ID 和错误码。上游网络调用与本地数据库无法组成原子事务，因此进程恰好在收到上游任务 ID 后、写入数据库前崩溃仍是不可完全消除的外部副作用窗口。实现不得把这一窗口描述为“严格零遗失”。

## 8. 自动轮询与退避

`SeedanceReconcilerRuntime` 默认配置：

- 调度周期：5 秒；
- 每批领取：8 个任务；
- 最大并发：1；
- 单次上游请求超时：20 秒；
- 租约时长：60 秒；
- 任务轮询期限：创建后 24 小时；
- 退避序列：5 秒、10 秒、20 秒、30 秒、60 秒，之后最大 5 分钟并加入小幅随机抖动。

同一实例用原子运行标记防止调度重入；多实例依赖数据库租约互斥。运行时接入现有应用启动和 `Cleanup` 停机顺序，不创建无管理的 goroutine。

状态处理规则：

- 上游仍在排队或处理中：释放租约并设置下一次轮询时间；
- 上游成功且 `completion_tokens > 0`：进入统一结算；
- 上游成功但缺少可计费 token：短间隔复查三次，仍缺失则进入 `dead_letter`，不得标记为已结算；
- 上游明确失败：标记 `failed`，不计费；
- 上游明确取消：标记 `cancelled`，不计费；
- 网络错误、429 和 5xx：退避重试；
- 401、403、持续 404、无法加载计费归属或超过 24 小时：进入 `dead_letter` 并记录可操作错误码。

404 在创建后的短暂宽限期内视为上游最终一致性延迟，宽限期后才计入持续 404。

## 9. 结算与幂等

统一结算按以下顺序执行：

1. 使用持久任务中的用户、API Key、分组、账户和订阅 ID 加载归属；
2. API Key 和订阅加载支持软删除记录，确保创建后禁用或删除 API Key 不会免除已经产生的上游成本；
3. 用上游 `completion_tokens` 和创建时模型快照构造 `OpenAIForwardResult`；
4. 使用 `StableGrokVideoBillingRequestID(task_key)` 作为稳定用量请求 ID；
5. 同步调用现有 `OpenAIGatewayService.RecordUsage`；
6. 成功后，用当前租约令牌和代次把任务标记为 `settled`。

`RecordUsage` 的 `usage_billing_dedup` 是资金副作用的最终幂等边界。若进程在计费成功后、任务状态写回前崩溃，下一执行器会再次调用 `RecordUsage`；去重表阻止第二次扣费，随后任务补写为 `settled`。

Redis 的 `ClaimGrokVideoBilling` 可以继续保护旧 Grok 路径，但 Seedance 自动结算不得依赖 Redis claim 判断是否已经扣费。客户端查询和后台 reconciler 都以数据库任务状态加 `usage_billing_dedup` 为准。

若 `RecordUsage` 返回错误，任务保持 `pending` 并退避重试；不得提前写 `settled`。

## 10. 客户端状态查询

客户端 GET 仍返回原始 Seedance 状态响应。Handler 在收到上游响应后把相同结果交给统一结算服务：

- 若后台已经结算，Handler 不产生第二次扣费；
- 若 Handler 首先发现成功，Handler 同步完成结算并把任务标记为 `settled`；
- 若持久任务不存在但 Redis 兼容快照存在，保留当前结算行为，同时记录迁移兼容日志；
- 若任务不属于当前用户和 API Key，继续返回现有的不可见结果，不泄露任务是否存在。

## 11. 删除流程

DELETE 不再直接转发。处理顺序为：

1. 根据当前用户、API Key 和任务 ID 加载持久任务并验证归属；
2. 领取该任务的短租约，避免与后台 reconciler 同时推进状态；
3. 删除前先查询一次上游状态；
4. 若任务已经成功且有可计费 token，先完成幂等结算，再调用上游删除；
5. 若任务仍在处理，调用上游删除，成功后标记 `cancelled`；
6. 若任务已经失败或取消，允许转发删除并保持对应终态；
7. 若删除前状态查询出现临时错误，不执行删除，返回可重试的上游错误，防止擦除可能已经成功但尚未结算的任务；
8. 上游删除失败时释放租约并保留任务，后台继续处理。

删除与上游完成之间仍可能存在极短竞争窗口。只要上游状态查询已经返回成功，必须先结算。上游在“处理中”状态查询之后才完成、同时删除成功的成本语义由上游取消协议决定，本地不伪造 token 或估算费用。

## 12. 上游客户端重构

当前 `ForwardSeedance` 同时包含 HTTP 调用、结果解析和 Gin 响应写入。实现时拆出纯服务接口：

- `CreateSeedanceTask(ctx, account, body)`；
- `GetSeedanceTask(ctx, account, taskID)`；
- `DeleteSeedanceTask(ctx, account, taskID)`。

这些方法返回规范化响应、原始安全响应体和明确错误类型，不直接写 `gin.Context`。Handler 适配层继续保持现有响应格式；后台 reconciler 只使用纯服务接口。

上游错误类型至少区分：认证失败、限流、未找到、临时 5xx、协议解析失败和明确终态失败。日志不得包含账户凭证、请求正文或视频 URL。

## 13. 配置与资源边界

新增 `gateway.seedance_reconciler` 配置段：

- `enabled`，默认 `true`；
- `poll_interval_seconds`，默认 `5`；
- `claim_batch`，默认 `8`；
- `max_concurrency`，默认 `1`；
- `request_timeout_seconds`，默认 `20`；
- `lease_seconds`，默认 `60`；
- `poll_deadline_hours`，默认 `24`。

配置校验拒绝零值、负值、租约不大于请求超时以及过高并发。为 2 GB 生产机设置硬上限：`claim_batch <= 64`、`max_concurrency <= 4`，生产默认仍为 8 和 1。

关闭 reconciler 只停止新后台领取，不删除任务，也不影响客户端查询和结算。重新启用后可恢复处理。

## 14. 可观测性

增加结构化日志，字段限制为任务 ID 的安全标识、provider、账户/用户/API Key 数字 ID、尝试次数、状态、错误码和耗时：

- `seedance_reconciler_poll`；
- `seedance_task_claimed`；
- `seedance_task_retried`；
- `seedance_task_settled`；
- `seedance_task_terminal`；
- `seedance_task_dead_lettered`；
- `seedance_task_persistence_failed`。

不得记录凭证、Cookie、Authorization、提示词、原始请求体、完整上游响应或媒体 URL。

每轮日志包含 selected、settled、retried、terminal、dead_lettered 和 fenced 数量。没有任务时使用 debug 级别，避免生产日志噪声。

## 15. 测试设计

实现必须先编写失败测试，再编写生产代码。最低测试集如下。

### 15.1 仓库与迁移

- 迁移可重复应用，表、约束和领取索引存在；
- 两个执行器不能同时领取同一任务；
- 过期租约可被新执行器回收；
- 旧租约令牌和代次不能更新已重新领取的任务；
- 唯一约束使重复持久化成为幂等成功。

### 15.2 自动结算

- 创建成功后客户端从不查询，后台最终生成一条用量并扣费一次；
- 服务停止后创建新 runtime，仍能恢复未完成任务；
- 两个 runtime 并发处理仍只扣费一次；
- 计费完成后模拟状态写回失败，重试不产生第二次扣费；
- 上游失败或取消不扣费；
- 成功但缺失 token 不会被误标为已结算；
- API Key 或订阅软删除后仍按创建时归属结算；
- 物理删除的归属进入死信并产生明确错误码。

### 15.3 Handler 生命周期

- 创建任务持久化成功后才返回成功；
- 持久化失败时不伪造成功；
- GET 与后台同时发现成功只记录一次；
- DELETE 已成功任务时先结算再删除；
- DELETE 处理中任务时删除成功并标记取消；
- DELETE 前状态查询失败时不会调用上游删除；
- 其他用户或 API Key 不能读取、删除或影响任务。

### 15.4 回归

- 现有 Seedance 请求和响应契约测试通过；
- Grok 图片、Grok 视频和其他网关行为不变；
- `go test ./...` 通过；
- Worker、前端和发布工具现有测试继续通过；
- `git diff --check` 通过。

## 16. 发布与回滚

发布顺序：

1. 在测试数据库验证迁移和任务领取；
2. 运行全部自动化测试；
3. 构建不可变镜像并记录源码哈希与镜像 digest；
4. 部署包含迁移的 control-plane；
5. 验证 reconciler 启动、空队列日志、创建任务和自动结算；
6. 观察死信、重试、数据库连接和内存；
7. 再决定是否扩大流量。

应用回滚时保留新表和任务数据，关闭或回滚 reconciler 不删除待处理任务。旧版本忽略新表；恢复新版本后继续处理。迁移不提供自动 DROP 回滚，避免遗失尚未结算的任务。

## 17. 验收标准

以下条件全部满足后，评审问题才算关闭：

- 正常返回成功的 Seedance 创建任务均存在持久任务行；
- 不依赖客户端轮询也能产生正确用量和余额/配额变更；
- 重启、多实例、重复 GET 和重复后台执行均不重复扣费；
- DELETE 不会直接擦除已经成功但尚未结算的任务；
- 失败、取消和缺失 token 的任务不会被伪造计费；
- 生产默认后台并发为 1，适配 2 GB 内存；
- 文档不再声称“只使用回调而不查询的任务不会自动结算”；
- 所有目标测试、全量 Go 测试和现有跨组件验证通过；
- 独立代码评审不再报告该欠计费路径。
