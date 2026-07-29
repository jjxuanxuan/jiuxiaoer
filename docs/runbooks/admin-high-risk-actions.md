# 后台高风险单人操作告警处置

本手册用于取消双人复核后的高风险后台动作监测。告警用于发现异常和事后追查，
不形成同步审批，也不应阻塞权限、幂等、版本和业务不变量均校验通过的合法请求。

## High risk admin action spike

`JiuxiaoerAdminHighRiskActionSpike` 监测以下三类非配送动作：

- 资产人工调账：`asset_adjustment.execute`
- 酒票异常处置：`wine_ticket_exception.resolution_executed`
- 酒票套餐发布：`wine_ticket.package.publish`

规则按固定的 `action` 和 `result` 标签聚合，不把管理员 ID 放入 Prometheus 标签。
每个 API 副本暴露的是同一审计库的全局计数，PromQL 使用跨副本 `max` 去重，禁止
按副本求和放大实际操作量。
15 分钟内任一上述动作新增超过 10 次成功记录，或资产人工调账新增超过 3 次已落库
的终态失败记录，并持续 2 分钟，触发 warning。酒票异常处置和套餐发布的失败事务
不会生成审计事实，因此本规则不虚构对应的 `failed` 时序；这两类失败应通过 HTTP
错误指标和请求日志排查。配送强制完成继续由既有的
`JiuxiaoerAdminOverrideSpike` 独立监测，避免同一事件重复告警。

收到告警后：

1. 确认指标是否持续增长，排除 Prometheus 重启、目标重复抓取或测试环境误接入。
2. 在审计库按告警的 `action`、`result` 和时间窗口聚合 `actor_id`，定位集中操作的
   管理员及受影响资源：

   ```sql
   SELECT action, result, actor_id, COUNT(*) AS operation_count,
          MIN(created_at) AS first_seen_at, MAX(created_at) AS last_seen_at
   FROM audit_logs
   WHERE actor_type = 'admin'
     AND action = '<alert action>'
     AND result = '<alert result>'
     AND created_at >= NOW(3) - INTERVAL 30 MINUTE
   GROUP BY action, result, actor_id
   ORDER BY operation_count DESC;
   ```

3. 逐条核对工单/原因、目标资源、请求 ID、幂等记录和业务事实；资产失败告警优先
   检查余额/状态不变量。酒票处置或发布失败应另查 HTTP 错误指标和同 request ID
   的请求日志。
4. 若无法确认操作合法，立即暂停对应管理员的高风险权限并保全审计、登录和请求
   证据；涉及资产时同步核对账本与余额，涉及酒票时核对异常单或套餐发布版本。
5. 处置完成后记录影响范围、根因和恢复依据。不得通过删除或改写审计记录来消除
   告警。
