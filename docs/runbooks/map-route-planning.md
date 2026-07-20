# 配送路线规划运行手册

## 安全边界

- 路线功能默认关闭；本手册只覆盖非生产启用和故障处置，不授权生产发布。
- 路线查询是只读旁路能力。高德、Redis 或路线模块故障不得阻断接单、取货、送达和异常上报。
- 禁止在日志、指标、工单或聊天中粘贴高德 Key、请求 URL、客户地址、电话、起终点坐标或 polyline。
- 先关闭 `JXE_MAP_ROUTE_ENABLED`，再进行故障排查；不要通过绕过 Redis、限流、对象归属或配送状态检查来恢复服务。

## provider success low

1. 查看 `jxe_route_provider_calls_total`，按 `result` 区分 timeout、quota、invalid 和 failure；同时核对 `jxe_route_cache_total` 与 `jxe_route_degraded_total`。
2. 确认高德开发 Key 的配额、白名单和服务状态，只记录“已配置/未配置”及受控指纹，不输出明文 Key。
3. 若连续 5 分钟低于 95%，关闭路线开关；确认 `jxe_route_provider_calls_total` 停止增长，并验证履约接口仍正常。
4. 修复后先使用 Fake Provider 和单个骑手白名单回归，再恢复非生产高德白名单。

## provider latency high

1. 查看 `jxe_route_provider_duration_seconds` P95、`jxe_route_inflight` 和 HTTP 路线耗时，区分高德延迟与本地 DB/Redis 延迟。
2. 确认 timeout 仍在 200ms~5s 范围、默认 1500ms，且代码未增加自动重试。
3. P95 连续 5 分钟超过 2 秒时关闭路线开关。不要提高全局并发来掩盖供应商延迟。

## provider quota unavailable

1. 立即关闭路线开关，确认没有新的高德调用；旧缓存只在既定 stale TTL 内降级返回。
2. 核对骑手、账号、IP 限流和全局并发指标，排查异常流量及同 Key 重复调用。
3. 由有权限的人员在高德控制台处理开发 Key 配额。禁止切换未知供应商或静默改变出行模式。

## 回退与恢复验证

1. 设置 `JXE_MAP_ROUTE_ENABLED=false` 并重启或刷新配置，目标 10 分钟内完成。
2. 保留新增坐标系字段和迁移，不执行破坏性 Down DDL；路线缓存等待 TTL 回收即可。
3. 回归心跳、接单、取货、完成和异常上报，确认路线故障没有业务写入或 Outbox 副作用。
4. 恢复顺序为：迁移/权限 → Fake 自动化 → Fake 骑手白名单 → 非生产高德固定坐标 → 非生产高德骑手白名单。
