# 微信支付账单对账运行手册

本任务每天北京时间 10:00 后检查从 `JXE_WECHAT_BILL_RECONCILIATION_START_DATE` 到 T+1 的 `trade_all` 交易账单和 `fundflow_basic` 基本账户资金账单。worker 每轮从最早的不完整日期开始，最多补齐 `JXE_WECHAT_BILL_RECONCILIATION_BACKFILL_DAYS_PER_CYCLE` 天；相同日期、类型只完成一次。微信仅允许下载最近三个月账单，因此系统把扫描和手工补账窗口限制为最新可生成账单向前 90 个自然日。首版只记录差异，不自动修改订单、支付、退款或余额。

## failure

1. 在日志中按 `bill_date`、`bill_type` 查找错误，并保存 `provider_request_id`。
2. `STATEMENT_CREATING`、`FREQUENCY_LIMITED` 或网络失败由下一调度周期重试；不要绕过摘要校验。
3. `BILL_FORMAT_INVALID` 或 `BILL_DIGEST_MISMATCH` 立即停止人工资金操作，保留运行记录并联系微信支付支持。
4. 修复配置或外部故障后，失败运行会由 worker 以同一日期、类型幂等重跑。

## overdue

确认 worker 已启用、北京时间已超过 10:00、MySQL 可写且微信 APIv3 证书与商户私钥有效。无交易时微信可能返回 `NO_STATEMENT_EXIST`，该状态也视为当日已完成；如果本地存在成功支付，系统仍会生成 `missing_wechat` 差异。

## gap

`jxe_wechat_bill_reconciliation_missing_dates` 统计缺少交易账单或资金账单终态的日期数，`jxe_wechat_bill_reconciliation_oldest_missing_bill_unixtime` 指向最早缺口。最新日期完成不能关闭中间日期缺口。

1. 在管理端运行记录中确认缺少的日期和账单类型，并查看对应失败码、微信 `Request-Id` 及下载 `Request-Id`。
2. 检查起始日期和单轮补账天数配置；若欠账很多，保持小批量让 worker 按最老日期持续追赶，避免同时下载大量账单。
3. 需要立即补单日时，由具备 `refund:exception` 权限的管理员调用 `POST /api/v1/admin/reconciliation/runs`，请求头必须携带唯一 `Idempotency-Key`，请求体示例：`{"bill_date":"2026-07-19","bill_type":"trade_all"}`。资金账单使用 `fundflow_basic`。
4. 若日期超出 90 天窗口，停止自动尝试，转到微信商户平台与财务留档核验；不得通过 SQL 伪造成功运行记录。
5. 只有同日 `trade_all` 和 `fundflow_basic` 都进入 `succeeded` 或 `no_statement`，该日缺口才算关闭。

## amount-mismatch

金额不一致必须立即人工复核。通过 `/api/v1/admin/reconciliation/discrepancies?status=open` 获取本地值和微信值，结合支付/退款查询 API 与商户平台核验。禁止直接修改数据库金额；确认处置后调用 `POST /api/v1/admin/reconciliation/discrepancies/{id}/resolve` 记录处理结论。

## discrepancy-sla

差异处理责任人为支付运营值班人员，告警渠道为生产 Prometheus/Alertmanager，处理 SLA 为金额差异立即响应、其他差异 24 小时内关闭。处理记录必须包含核验依据和关联工单号。
