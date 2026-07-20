# 配送异常运营闭环运行手册

## 安全开关

三个开关默认关闭并独立控制入口、自然收口和通知：

- `JXE_DELIVERY_INCIDENT_ENABLED`
- `JXE_DELIVERY_INCIDENT_AUTO_RESOLVE_ENABLED`
- `JXE_DELIVERY_INCIDENT_NOTIFICATION_ENABLED`

骑手和门店白名单为空时仍然不放量；G5 全量时分别设置为单个 `*`（或 `all`），不得与具体 ID 混用。预发/生产启用总开关前还必须配置 HTTPS 媒体网关 `JXE_EVIDENCE_VIEW_BASE_URL`、至少32字符的 `JXE_EVIDENCE_VIEW_SECRET`，并保持 `JXE_EVIDENCE_VIEW_TTL<=5m`。

门店、运营和骑手详情只返回证据元数据。需要查看时调用对应的 `.../evidence/{evidence_id}/view`，服务会重新校验当前对象权限并返回不可缓存的短时地址；令牌中对象键为加密内容，媒体网关用相同密钥调用 `evidenceview.Open` 校验后读取私有桶。轮换查看密钥会立即使旧地址失效。

紧急止损时先关闭对应副作用开关；保留四张异常表、历史、审计和 Outbox，不执行 down migration，不手工改订单、金额或库存。

## Create SLO or latency

1. 停止扩大骑手和门店白名单。
2. 按 `result`、`code`、`type` 查看 `jxe_delivery_incident_requests_total`，检查 MySQL 锁等待和 Outbox 写入。
3. 检查 `jxe_delivery_incident_rate_limited_total{scope}` 和 `jxe_delivery_incident_rate_limiter_degraded_total{scope}`；后者增长表示 Redis 不可用、限流已降级到进程内窗口。
4. 若错误持续，关闭总开关；已提交事实保留并通过相同幂等键安全重试。

限流键覆盖骑手与匿名化 IP 两个维度；在线写路径不得查询 `delivery_incident_history` 计数。迁移 `202607170002` 同时为历史审计查询补充 `(actor_type,actor_id,action,created_at)` 索引。

## Location quality

`jxe_delivery_incident_location_distance_suppressed_total{reason="accuracy_gt_1000m"}` 增长表示定位精度超过1000米：服务保留精度与采集时间，但不会持久化到目的地距离。突增时检查骑手端定位权限、设备质量和上报 SDK，不得放宽服务端距离可信阈值。

## Evidence access or audit

1. `view_available=false` 时先检查媒体网关三项配置和扫描状态；预发/生产缺配置应在启动校验阶段失败。
2. 查看接口必须带 `Cache-Control: no-store`，URL 中不得出现 `object_key`；业务日志、审计和 Outbox 也不得记录查看令牌。
3. 每次异常详情和证据查看，以及写入的 `denied/invalid/conflict/token_invalid` 结果，都应在 `audit_logs` 中记录 actor、route、result、request_id 和资源 ID。
4. 怀疑泄露时关闭总开关的写入口、轮换查看密钥并保留审计；不要删除异常事实或历史。

## Fact invariant violation

1. 立即关闭总开关，保存数据库和应用日志现场。
2. 检查活跃唯一索引 `uk_delivery_incidents_active_type`，并核对每条异常的 history、audit 和 `delivery.incident.*` Outbox 数量。
3. 不直接删除重复或孤立记录；形成修复脚本并经双人复核后执行。

## Natural close residual

1. 关闭 `JXE_DELIVERY_INCIDENT_AUTO_RESOLVE_ENABLED` 并停止扩量。
2. 核对配送取货、完成、强制完成或订单取消事务是否回滚，以及异常 `resolved` 历史和 Outbox 是否同事务写入。
3. 修复后用受控命令补齐状态、历史、审计和 Outbox，禁止只改主表状态。

## Customer notification

1. 立即关闭异常通知开关和 notification consumer。
2. 检查 `message_inboxes` 与 `notification_deliveries` 中 `delivery.incident.%` 事件；客户收件人为零是硬约束。
3. 保留证据并撤回尚未发送的错误投递；恢复前运行通知路由回归测试。

## Unacknowledged backlog

按门店、类型和上报时间分组确认积压，通知运营扩容。不得为消除告警自动结案，也不得触发退款、库存或订单状态变更。

## 回滚验证

关闭开关后确认新写请求返回 `DELIVERY_INCIDENT_DISABLED`，正常取货、送达、取消仍可用；核对客户通知为零、自然收口残留为零、Outbox/DLQ 无新增异常，再决定是否恢复灰度。
