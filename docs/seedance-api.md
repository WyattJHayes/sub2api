# Seedance 原生 API

支持火山方舟 Ark 的异步视频任务协议，无需把 `content[]` 转换成 OpenAI `messages` 或 Grok `prompt`。

## 配置

1. 创建 OpenAI 平台的 **API Key** 账号，填写 Ark API Key，Base URL 使用 `https://ark.cn-beijing.volces.com/api/v3`。兼容服务可填写自己的 `/api/v3` 或 `/v3` Base URL。
2. 在账号的端点能力中勾选 **Seedance (Ark)**。默认不启用，避免请求误调度到其他 OpenAI 账号。支持创建、编辑与批量编辑。
3. 将账号加入 OpenAI 分组并启用分组的「允许图片生成」媒体权限；合成分组也可路由到这些账号。
4. 配置模型映射，例如将公开模型名 `seedance-video` 映射到实际 `doubao-seedance-*` 模型或 `ep-*` 推理接入点。配置对应模型的输出 token 价格；本接口不使用 Grok 的按秒视频价格。

## 调用

```bash
curl "$SUB2API_BASE_URL/api/v3/contents/generations/tasks" \
  -H "Authorization: Bearer $SUB2API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "seedance-video",
    "content": [{"type": "text", "text": "海浪轻轻拍打沙滩"}],
    "duration": 5,
    "resolution": "720p",
    "ratio": "16:9",
    "generate_audio": true
  }'

# 使用创建响应中的原生 id 查询，直至 succeeded / failed / cancelled 等终态。
curl "$SUB2API_BASE_URL/api/v3/contents/generations/tasks/$TASK_ID" \
  -H "Authorization: Bearer $SUB2API_KEY"

curl -X DELETE "$SUB2API_BASE_URL/api/v3/contents/generations/tasks/$TASK_ID" \
  -H "Authorization: Bearer $SUB2API_KEY"
```

亦支持 `/v3`、`/v1` 和无版本前缀别名。Ark SDK 的 Base URL 可改为 `$SUB2API_BASE_URL/api/v3`。文本、图片、视频、音频内容、角色及扩展参数原样传递，仅按账号配置改写模型名；响应保持上游原生格式。

## 任务与计费

- 查询和删除只能访问同一用户、同一 API Key 创建的任务，并始终使用原提交账号；创建时的分组仅作为计费快照，不会转到其他账号查询。
- 创建时不扣 token 用量。首次查询到 `succeeded` 后，根据上游 `usage.completion_tokens` 计费；重复查询由共享缓存声明和持久化用量去重共同保护。失败、排队及运行中的任务不计费。
- 创建接口只有在已受理任务写入本地持久化存储后才返回成功。
- 服务通过有界协调器轮询待处理的 Seedance 任务，进程重启后会继续处理。
- GET 查询与后台协调器共用同一条幂等结算路径。
- DELETE 会先查询任务状态；若任务已经完成，则先结算再执行删除。
- 关闭 `gateway.seedance_reconciler.enabled` 只会停止后台领取任务，不会删除持久化记录，也不会禁用 GET 触发的结算。
- 进入 dead-letter 的记录需要人工诊断，系统不会静默计费或丢弃。
- 上游已受理任务后、任务写入本地存储前若进程崩溃，仍属于无法由本地事务消除的外部系统限制；此类上游任务可能需要按上游记录人工核查。
- 不开放上游的任务列表接口，防止共享账号的任务泄露给其他用户。删除遵循上游语义，不自动退款。
- 异步创建的上游错误不自动重试，以免重复创建付费任务。

协议依据：[火山官方 Go SDK](https://github.com/volcengine/volcengine-go-sdk/blob/master/service/arkruntime/model/content_generation.go)、[创建任务文档](https://www.volcengine.com/docs/82379/1520757)、[查询任务文档](https://www.volcengine.com/docs/82379/1521309)。
