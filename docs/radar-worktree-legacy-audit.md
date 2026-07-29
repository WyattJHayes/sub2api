# Radar 工作树遗留项审计

- 审计日期：2026-07-29
- 审计方式：只读文件比较、Git 状态、接口签名检查和专项测试
- 处置状态：未删除、未覆盖、未提交来源不明的现有改动

## 1. 结论

当前工作树有一个 P0 协议不一致和三类可清理构建或页面副本。P0 问题必须在继续 Worker 生命周期实现前完成，两个 Vue 副本和一个二进制副本可以在确认无外部留存需求后删除。

## 2. P0 Worker lease epoch 合同不一致

### 2.1 证据

`radar-worker/src/sub2api_radar/runner.py` 的未提交改动已经对以下调用增加 `lease.lease_epoch`：

1. `submit_evidence`
2. `fail_assignment`
3. `heartbeat`

当前 `radar-worker/src/sub2api_radar/models.py` 中的 `AssignmentLease` 未定义：

```text
lease_epoch
worker_image_digest
work_origin
```

当前 `radar-worker/src/sub2api_radar/control_plane.py` 中的真实客户端签名仍为：

```python
async def heartbeat(self, assignment_id: UUID, token: str) -> str
async def submit_evidence(self, assignment_id: UUID, token: str, evidence: ExecutionEvidence) -> EvidenceReceipt
async def complete_assignment(self, assignment_id: UUID, token: str) -> None
async def fail_assignment(self, assignment_id: UUID, token: str, failure_code: str) -> None
```

`tests/test_control_plane.py` 已经期望 Assignment lease 和 mutating calls 携带 epoch，说明测试合同与实现处于半切换状态。

### 2.2 可复现测试

```bash
cd radar-worker
PYTHONPATH=src UV_CACHE_DIR=/tmp/sub2api-radar-uv-cache \
  uv run pytest tests/test_control_plane.py tests/test_worker_loops.py -q
```

结果：`7 failed, 4 passed`。

主要失败：

1. `AssignmentLease` 拒绝 `lease_epoch`、`worker_image_digest` 和 `work_origin`。
2. Runner 访问不存在的 `lease.lease_epoch`，Evidence 无法提交。
3. Runner 的 error reporting 再次访问缺失 epoch，根因被二次异常覆盖。
4. `ControlPlaneClient.complete_assignment` 测试传入 epoch，真实签名只接受两个业务参数。

### 2.3 修复边界

该问题属于 migration 197 Worker 生命周期计划的一部分，修复必须作为一个原子提交完成：

1. 为 Assignment、Grading 和 Analysis lease DTO 增加 `lease_epoch`、worker digest 与 work origin。
2. `heartbeat`、`submit_evidence`、`complete_assignment` 和 `fail_assignment` 接受 epoch，并写入 JSON body。
3. Go handler 对所有 mutating call 强制验证 epoch，缺失字段返回有限的 fenced 错误。
4. Runner、Grader、Statistics 与测试 stub 统一签名。
5. 执行全部 Worker 测试、mypy、ruff 和 Go Worker route tests。

当前 `runner.py` 改动不能独立提交，也不能回退覆盖，因为它可能属于正在进行的 Worker 协议工作。

## 3. 两个 Vue 页面副本

| 文件 | 大小 | 与受版本控制页面的差异 | 建议 |
| --- | ---: | --- | --- |
| `frontend/src/views/admin/radar/RadarDatasetsView 2.vue` | 893 bytes | 早期单行只读列表，缺少创建、发布、校验和反馈 | 确认无外部留存需求后删除 |
| `frontend/src/views/admin/radar/RadarRunsView 2.vue` | 236 bytes | 早期空状态页，缺少 Plan 与 Run 操作 | 确认无外部留存需求后删除 |

`git ls-files` 只包含无 ` 2` 后缀的正式页面。仓库中没有引用这两个副本。正式页面更新时间更晚、功能更完整。

## 4. 二进制副本

| 文件 | 类型 | 大小 | SHA256 |
| --- | --- | ---: | --- |
| `radar-control-plane` | Linux x86-64 静态 ELF | 109371554 bytes | `bc039a2a4cb048f42f0da0ff8baa68ad625ae850e8b3228f98fae06191f17b61` |
| `radar-control-plane 2` | Linux x86-64 静态 ELF | 109101218 bytes | `0bddd341a2397aff36d7cdd32b70316a8681b068d971701a88eda8d5650a4fa0` |

`.gitignore` 只忽略根目录精确路径 `/radar-control-plane`，带 ` 2` 后缀的副本因此出现在工作树状态中。两个 hash 不同，时间戳显示 `radar-control-plane 2` 更早。

建议处置：

1. 确认无需保留旧 staging 二进制后删除 `radar-control-plane 2`。
2. 构建输出统一放入 `dist/` 或临时目录。
3. 后续修改 `.gitignore`，覆盖受控构建输出目录，避免使用宽泛通配符隐藏未知文件。

## 5. 推荐处置顺序

1. 先完成 Worker lease epoch 原子修复并恢复专项测试。
2. 再执行 migration 197 至 199 的可信执行计划。
3. 删除两个无引用 Vue 副本和旧二进制副本。
4. 提交清理记录和 `.gitignore` 的受控构建目录规则。
5. 开始 migration 200、201、202 三个新增工作包。
