# 酒票 Expand / Backfill / Contract 上线手册

适用 PRD：`PRD-WT-20260718-01`。本手册的核心约束是：普通零售支付、退款和配送退回不能因为酒票发布而中断；任何不确定资金事实都保持未结算并进入人工处置，不能伪造成成功。

## 1. 发布前门禁

- 所有 `JXE_WINE_TICKET_*_ENABLED` 保持 `false`。
- API/worker 进程使用 `TZ=Asia/Shanghai`；Go `time.Local` 必须为 `Asia/Shanghai`。
- DSN 必须含 `parseTime=true&loc=Local`，MySQL global/session `time_zone` 必须精确为 `+08:00`。
- 对 Expand、回填代码、普通订单支付、售后退款和配送退回回归已完成。
- 已备份数据库，并记录发布负责人、DBA、开始时间和回滚负责人。

## 2. Expand

正常执行：

```bash
make migrate-up
```

`202607270001_wine_ticket_expand.sql` 只有增量 Schema，没有数据 `UPDATE`。迁移后重建运行账号授权：

```bash
bash ./deploy/mysql/provision-local-runtime-user.sh
```

验证 `wine_ticket_transactions` 的运行账号只有 `SELECT, INSERT`，`UPDATE/DELETE` 必须被 MySQL 拒绝。

## 3. 部署双写版本

部署包含以下行为的 API/worker，但继续关闭酒票业务写开关：

- 新普通支付写 `biz_type=retail_order,biz_id=order_id`。
- 新售后退款及 replacement 写 `biz_type=retail_after_sale,biz_id=after_sale_id`。
- 配送退回写 `retail_cash_refund` settlement 状态。
- 已注册普通零售 payment/refund/return handler；原订单、库存、派单和 legacy event 回归完全等价。

若生产把数据库后台任务从 API 拆出，使用镜像内
`JXE_WINE_TICKET_MAINTENANCE_OWNER=worker /app/jiuxiaoer-worker -role wine-ticket-maintenance`。
该角色不要求 RabbitMQ；每个进程仍必须配置独立 `JXE_INSTANCE_ID` 和
`JXE_SNOWFLAKE_NODE_ID`。owner 只允许 `api|worker`，默认 `api`；独立
角色在 owner 不是 `worker` 时会拒绝启动。

| owner | API 酒票专属任务 | API 共享支付过期/退款任务 | `wine-ticket-maintenance` |
| --- | --- | --- | --- |
| `api` | 启动 | 启动 | 拒绝启动 |
| `worker` | 不启动 | 不启动 | 独占启动酒票任务及完整共享任务 |

支付过期与退款执行共用普通零售和酒票队列，不能让两个进程各自扫描。
因此 owner 切到 `worker` 时，共享任务也整体迁移到该 Worker，普通零售
仍由它处理，不会漏扫或双跑。稳态下所有 API 与 Worker 必须配置相同
owner。`api → worker` 时先发布 owner=`worker` 的新 API 并摘除旧 API，
确认旧 API 全部退出后再启动 dedicated Worker；反向切换先停 Worker，
再发布 owner=`api` 的 API。切换间的短暂停扫由数据库任务续跑，不可用
新旧 owner 同时运行来填补。

开放后台配送时段管理前，必须为所有 `scope=scoped` 的平台管理员写入
`admin_user_shops`。授权关系的增删与对应 `accounts.credential_version + 1`
必须在同一事务完成，使旧 JWT 立即失效；空授权集合会按 403 拒绝，不能把
它解释成全门店。只有数据库 `scope=all` 且角色为 `super_admin` 或
`admin_manager` 时允许全局门店范围。

酒票对账从上海时间每日 `00:05` 开始，`wine_ticket_reconciliation_checkpoints`
按业务日持久化 phase、主键 cursor、各 phase high-watermark、累计扫描量和完成
时间。每批最多 2000 行；重启从最后成功批次续跑，新增 ID 超过本周期
high-watermark 的事实进入下一日周期，因此持续写入不会阻止本周期完成。
checkpoint 租约绑定 `JXE_INSTANCE_ID` 和进程唯一后缀，过期接管使用 version
CAS，旧 owner 不能推进新 owner 的 cursor。maintenance owner 仍按上表只运行
一处；若滚动发布短时重叠，数据库租约会阻止重复扫描。

扫描事务只读业务表。唯一写边界是幂等 upsert
`wine_ticket_exceptions` 和推进 checkpoint；不得更新 lot、allocation、订单、
库存或资金事实。上线前仍需用生产分布脱敏样本执行 `EXPLAIN ANALYZE`，并证明
1000 万 transactions 在 `T+1 06:00` 前完成。

观察至少一个完整峰值窗口。若出现普通订单资金或履约回归，关闭新版本流量并回滚应用；Expand 字段保留。

## 4. 三类可续跑回填

先 dry-run，每类使用独立私有 checkpoint/report：

```bash
make wine-ticket-backfill JOB=wine-ticket-payments CHECKPOINT=/secure/wt-payments.json REPORT=/secure/wt-payments-report.json
make wine-ticket-backfill JOB=wine-ticket-refunds CHECKPOINT=/secure/wt-refunds.json REPORT=/secure/wt-refunds-report.json
make wine-ticket-backfill JOB=wine-ticket-returns CHECKPOINT=/secure/wt-returns.json REPORT=/secure/wt-returns-report.json
```

写入必须同时提供环境门禁、显式执行参数和确认短语：

```bash
JXE_WINE_TICKET_BACKFILL_ALLOW_WRITE=true \
make wine-ticket-backfill \
  JOB=wine-ticket-payments \
  EXECUTE=--execute \
  CONFIRM=APPLY_WINE_TICKET_REGISTRY_BACKFILL \
  CHECKPOINT=/secure/wt-payments.json \
  REPORT=/secure/wt-payments-report.json
```

退款和退回任务同样执行。每批 500～2000 行；根据锁等待、复制延迟和 binlog 增长降低 `rows-per-second`。中断后只可用相同参数和 checkpoint 继续。

## 5. 追平与 Contract

1. 从负载均衡摘除并清空全部旧 Go 进程。
2. 删除上一轮已完成 checkpoint，三类任务从零再跑第二遍。
3. 确认三份报告 `completed=true`，且以下查询均为 0：

```sql
SELECT COUNT(*) FROM payments WHERE biz_type IS NULL OR biz_id IS NULL;
SELECT COUNT(*) FROM refunds WHERE biz_type IS NULL OR biz_id IS NULL;
SELECT COUNT(*) FROM delivery_returns WHERE settlement_type IS NULL OR settlement_status IS NULL;
```

4. 备份并在变更窗口执行手工 Contract：

```bash
go run github.com/pressly/goose/v3/cmd/goose@v3.26.0 \
  -allow-missing \
  -dir ./migrations/manual \
  mysql "$JXE_MYSQL_MIGRATION_DSN" up
```

`-allow-missing` 只用于这个受控的手工 Contract 目录：长期停留在 Expand
的环境可能已经应用版本号更高的普通迁移。Contract 自带业务链接断言；
任一断言失败必须停止，不能绕过或手工改写为成功。普通迁移命令严禁使用
`-allow-missing`。

## 6. 灰度与止血

按顺序开启：套餐只读/管理 → 购买 → 酒柜 → 提酒 → 赠礼 → 提醒 → 续期 → 用户退款。每次只开一个分支并观察对应 SLA。

业务止血只关闭新增动作的分支开关。已有酒票事实后不得关闭 master，也不得停止 callback、confirm、query、cancel、timeout、补偿退款、到期和 reconcile worker。

## 7. 回滚

- Contract 前：回滚应用并关闭分支开关；保留 Expand 字段和新表。
- Contract 后且尚无非零售资金事实：可在演练/空库按手工迁移 `down`。
- Contract 后已有酒票资金事实：禁止执行 `down`；回滚应用必须仍理解 registry，关闭新增入口并继续处理在途闭环。
- 任何 payment success 超过 60 秒未结算、退款 SUCCESS 超过 60 秒未核销或 lot invariant 异常：关闭对应新增入口，保留事实，创建 P1 异常并由具备 `wine_ticket_exception:resolve` 的单个授权管理员受控处置。

单人处置不降低资金与资产事实校验：管理员必须先保存 SQL 证据、支付机构查询结果和 request/event ID，再提交受支持的 resolution action、原因、操作工单号、当前版本与幂等键。系统立即执行已注册的可信 closure，并在同一事务写入处置结果与审计；不再等待另一名复核人。任何事实不确定、版本冲突或 closure 校验失败均保持异常未解决，严禁直接改表。

## Payment settlement lag

`JiuxiaoerWineTicketSettlementLag` 触发后立即关闭购买与付费续期的新建入口，
但保留支付回调、confirm、order expiry、补偿退款和对账任务。按
`biz_type` 查询 `payments` 与 `wine_ticket_exceptions`，以微信支付查询结果
为唯一外部资金事实；不得手工把 payment、purchase 或 renewal 改成成功。
确认异常工单已经生成且证据齐备后，由具备 `wine_ticket_exception:resolve`
的单个授权管理员提交受控处置；系统按最新支付机构事实失败关闭并记录审计。

## Refund settlement lag

`JiuxiaoerWineTicketRefundSettlementLag` 触发后关闭新的退款和续期付款入口，
继续运行退款 callback/worker。核对 common refund、业务退款/续期记录与
allocation hold；不允许只更新 common refund。若服务商结果仍不确定，保持
权益锁定并进入 P1 异常工单，由单个授权管理员基于最新服务商事实受控处置。

## Renewal guard stalled

`JiuxiaoerWineTicketRenewalGuardStalled` 触发后关闭新的续期入口，保留支付/
退款 callback、主动查单及补偿任务。先核对 renewal、lot、payment 和
compensating refund 的服务商事实，不得手工释放 active guard，也不得直接延长
lot。默认告警阈值为 15 分钟支付 TTL 加 60 秒结算 SLA；若生产支付 TTL 调整，
必须同步修改该阈值并完成演练。

## Lot or reconciliation invariant

`JiuxiaoerWineTicketLotInvariantViolation` 或
`JiuxiaoerWineTicketReconciliationDifference` 触发后，关闭购买、提酒、赠礼、
续期、退款的新建入口，保留查询、取消、领取、回调和后台收敛。根据
`correlation_id=REC-WT-*` 定位异常；对账任务只能写异常工单，严禁直接调平
lot、不可变流水或实物库存。先保存 SQL 证据与 request/event ID，再由具备
`wine_ticket_exception:resolve` 的单个授权管理员通过受控 resolution 入口处置。

## Reconciliation deadline

`JiuxiaoerWineTicketReconciliationDeadlineMissed` 表示前一上海业务日没有在
`06:00` 前完成。先确认当前周期是否仍在推进：

```sql
SELECT cycle_key, status, phase, last_id, high_watermarks,
       checked_rows, detected_rows, lease_owner, lease_until,
       started_at, last_batch_at, completed_at, version
  FROM wine_ticket_reconciliation_checkpoints
 WHERE cycle_key >= DATE_FORMAT(CURRENT_DATE - INTERVAL 2 DAY, '%Y-%m-%d')
 ORDER BY cycle_key DESC;
```

`last_batch_at` 持续推进时不得清空 checkpoint 或并行启动第二个 worker；根据
phase high-watermark、批量索引和慢查询定位瓶颈。租约已过期且不再推进时检查
maintenance owner 与实例日志，恢复正确 owner 后由数据库 cursor 自动续跑。
禁止为了消除告警手工标记 completed。指标
`jxe_wine_ticket_reconciliation_deadline_missed` 必须连续 7 天为 0 才满足发布
观察门禁。

## Reminder lag

`JiuxiaoerWineTicketReminderLag` 触发后只关闭微信订阅消息通道，保留站内信与
lot 到期任务。检查维护任务 owner 是否唯一、OpenID/一次性授权是否可用及微信
接口健康度。对结果未知的订阅消息不得自动重发；access token 获取失败可安全
重试且不得消耗用户授权。
