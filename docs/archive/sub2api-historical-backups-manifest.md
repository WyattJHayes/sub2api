# 历史生产备份脱敏清单

归档日期：2026-10-11（Asia/Shanghai）

本文件是公开仓库中的脱敏索引，**不是数据库备份**。原始 dump 不得上传到公开 GitHub，因为其中可能包含用户数据、凭证或业务数据。

## 汇总

- 备份数量：12
- 总字节数：4,951,540,639（约 4.61 GiB）
- 本地归档收据标记：全部文件双重哈希校验通过
- 校验实现：Python hashlib 与系统 shasum
- 原始文件模式记录：以受限模式保存；本清单不公开本地目录和生产主机信息

## 文件摘要

下表只公开大小和 SHA-256，用于将来在私有存储中核对对象完整性。文件名中的发布标签是脱敏后的时间点标识。

| 标签 | 大小（字节） | SHA-256 |
| --- | ---: | --- |
| private-archive-1 | 1,119,092,734 | `e955e7333cf3d72fe2f916ecad7c07c7d930987c8b0ba21b2c387e0ad07e2731` |
| private-archive-2 | 999,254,592 | `c33d1529c30e6b59e08e23c4eb5644e63f275d1b5c718036515ee3e7e7ef7f6a` |
| private-archive-3 | 934,995,801 | `d334a296ed8c3422a562a8abadef04cc284c9923a4931fb63e8cb5fbee981689` |
| pre-v020-remediation | 232,868,044 | `f1ad3ef43798370108fcd5b9578e127f31a42a2825d2770fc6f6d0c6b6bdda92` |
| pre-v020-release-1 | 235,647,652 | `b049365f3264c93d2404c8451b8320c0a6f495bf6fb69640a94d8707dc9caf06` |
| pre-v020-release-2 | 235,655,554 | `6c4ff76449573c251ab6e5ef53e455d98744af205517ac07c1d0a9ffc8b3105c` |
| pre-v020-release-3 | 235,932,017 | `4374655165a2c9366ac695489ff66ae218a8d7ced9bc8013ea01081b3a45e5d6` |
| radar-v021 | 257,985,060 | `9c5e8497397d824f386eaa3a2b435a3f54b4ff45b150fe21498f74dbce8b9742` |
| radar-v021-precutover | 266,197,128 | `158df5ef72e0a7f144ff62734207c66b3472eb9f0c5381acc7fbbeae1a73b9bb` |
| radar-v01176-precutover | 125,230,641 | `89bd6439ce134eea94c0c62e0290ee3f822093b48de883299f44e9eaacb696f1` |
| radar-v01176-current | 124,904,035 | `fa39b66c9abaf2c132f5a01922476169d801ef57bac862079d8e648bc193e247` |
| radar-v01183-live-precutover | 183,777,381 | `cb44654bfe54d387ee608c99cca4456978ce7362f53f0e8970cf34fda446fe84` |

## 处置状态

本轮只完成公开清单准备。原始 dump 仍需先转存到用户指定的私有存储并重新校验，之后才能删除本地归档。未指定私有存储前，不执行删除，也不把“已准备清单”表述为“备份已转存”。

