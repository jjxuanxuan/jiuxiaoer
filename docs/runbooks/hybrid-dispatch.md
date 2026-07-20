# 混合派单运行手册

## 核心事实与安全边界

- 支付成功事务必须同步创建 `delivery_orders`、`dispatch_jobs` 和 `dispatch.job.ready` Outbox；RabbitMQ 只负责唤醒，MySQL `next_action_at` Sweeper 负责兜底。
- 骑手可在门店备货前接受邀约或抢单；`pickup_ready_status=ready` 之前取货接口必须返回 `DELIVERY_PICKUP_NOT_READY`。
- `delivery_assignments` 的生成列唯一键保证每个配送单最多一个 active assignment。任何重复值都按一致性事故处理。
- 已取货订单不得自动取消或改派，转履约异常流程。

## job overdue

1. 查看 `jxe_dispatch_oldest_overdue_seconds`、`jxe_dispatch_jobs` 和 `jxe_mq_queue_*{consumer="dispatch"}`。
2. RabbitMQ 正常但 job 仍超时：确认 dispatch consumer 和 Sweeper 至少一个运行，检查 `JXE_DISPATCH_WORKER_ENABLED`、DB 连接池和行锁等待。
3. RabbitMQ 不可用：不要补造 delivery/job；保持 Outbox pending，确认 Sweeper 正在按 `next_action_at` 推进。
4. 对 `manual_required` 且配送单仍未归属的任务，可通过管理端 retry 创建新 `dispatch_seq`。禁止直接改数据库状态。
5. 恢复后确认过期 offer 归零、每个 delivery 只有一个 active job/assignment，且 dead letter 已处置。

## manual backlog

1. 按 `shop_id`、`status_reason_code` 查询 `/api/v1/admin/dispatch/jobs`，区分无候选、抢单超时和 Worker 最大重试。
2. 先确认骑手审核、服务门店、在线心跳、定位精度与容量；不要绕过资格检查直接写 `rider_id`。
3. 使用现有人工 assign/reassign API。改派仅限 `accepted` 且未取货，验证码在门店已备妥时重生。
4. 若同一门店持续进入人工队列，切换该门店策略为 `grab` 或 `manual`，并保留审计记录后再排查运力。

## invariant violation

1. 立即将 `JXE_DISPATCH_MODE_OVERRIDE=manual`，保留 API 查询和人工处置能力；不要删除 job、offer 或 assignment 历史。
2. `jxe_dispatch_paid_orders_without_delivery>0`：按订单核对支付回调事务和迁移版本，使用受控修复程序调用支付事实补偿逻辑，禁止手写不带 job 的 delivery。
3. `jxe_dispatch_duplicate_active_assignments>0`：停止新的归属写入，导出相关 delivery、assignment、audit、outbox；确认真实骑手后将错误记录置为 `superseded/cancelled`，再恢复唯一约束。
4. 检查最近部署、MySQL deadlock/超时和幂等记录。修复后验证所有不变量为零并执行并发抢单回归。

## service scope mismatch

1. 对比告警骑手的 `riders.service_scope.shop_ids` 和 `rider_service_shops` active 记录。
2. 确认迁移、入驻或管理写入是否只更新了一个表示；派单热路径继续以标准关系表为准。
3. 通过受审计的修复操作同步两边，确认 `jxe_dispatch_service_scope_mismatches` 恢复为 0。

## RabbitMQ dead letter

dispatch 队列使用 `1s/5s/30s` 独立重试和 `jxe.dispatch.dead.v1.queue`。先确认 DB job 当前状态：若已 assigned/cancelled，消息可按幂等事件重放；若仍 actionable，确认故障原因后通过 MQ dead-letter 管理接口重放。Sweeper 会在消息未恢复时继续收敛任务。

## 回退

- 优先按门店发布 `manual` 或 `grab` 策略；紧急时设置 `JXE_DISPATCH_MODE_OVERRIDE=manual` 并滚动重启 Worker。
- 回退不删除加法 DDL、不回退已成功归属、不重新公开普通 `pending_assign` 配送单。
- pending offer 等待到期或由任务状态推进统一取消；恢复前核对通知、Outbox、active assignment 和取货门禁。
