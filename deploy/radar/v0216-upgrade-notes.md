# v0.2.16 Radar 同步说明

## 源码基准

- 官方发布：<https://github.com/Wei-Shaw/sub2api/releases/tag/v0.2.16>
- 官方 annotated tag：`c3cb8588ce6a5aef4dc29fa4f71ac91f3c0bdf3d`
- tag 指向源码提交：`01842576b3d5c1e2545b5a99ad555d36e28ed306`
- fork 起点：`3214c9e6e93901f169cb3bf322b2b2cb8e06cb43`

同步官方 Gemini 显式上下文缓存、分组多语言、模型冷却与长上下文计费变更，保留 Radar、评测 cron、Worker 优雅退出、Seedance 结算、中文模型三态和移动端静态资源保护。Wire 与 Ent 生成结果经过核对。

## 发布身份与迁移清单

控制面版本与 Worker 包版本均为 `0.2.16`。候选构建入口为 `build_v0216_ghcr.py`，image-record schema 为 `radar-v0216-image-record-v1`。历史构建入口继续绑定各自版本。

`manifests/v0.2.16` 保留 v0.2.15 的完整 325 条账本校验值与历史别名，新增：

1. `242_group_i18n.sql`
2. `243_gemini_cached_contents.sql`
3. `244_gemini_cached_contents_channel_usage.sql`
4. `245_gemini_cached_contents_mutation_lease.sql`

预期 SQL 文件数为 326，运行时账本条数为 329。既有 `242_drop_platform_check_constraints.sql` 按完整文件名保留，不修改旧迁移内容或编号。校验值遵循迁移执行器的去除首尾空白契约。

## 配置与发布边界

新增 `gateway.gemini_cached_content_max_ttl_seconds` 默认 86400 秒。Gemini 缓存创建和延期会按存储价格计费；使用自定义模型时需核对 `cache_storage_cost_per_token_per_hour`，不应为所有模型盲目开启配置。

本次仅同步代码、提交 PR 和验证 CI，不合并、不构建推送候选镜像、不切换生产、不执行生产数据库迁移。生产保持现有 2 GiB 内存规格。

后续发布需以最终提交构建独立 control-plane/worker 镜像，保存不可变 digest 和源码哈希，核对新鲜私有备份、迁移账本与回滚路径，再在获得维护窗口授权后执行。

## 验证范围

发布身份测试覆盖错误版本、错误源码哈希、错误 Worker 版本、旧 Go 基础镜像、旧账本改动和缺失新迁移。历史发布测试按其声明的清单验证，当前发布测试审计完整 SQL 目录。

CI 的前端任务先构建实际资产，再运行 `go test -tags=embed ./internal/web`，验证 JavaScript MIME、缺失资源 404、指纹资产缓存和普通 logo 的缓存行为。中文模型三态测试纳入关键前端测试。

本机无运行中的 Docker daemon，容器集成验证由 Linux CI 执行。CI 通过与生产运行验收分别记录，不相互代替。
