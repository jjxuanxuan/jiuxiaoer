# L0 Operations Runbook

This runbook covers the launch-gate alerts defined in `deploy/prometheus/alerts.yml`. Before production, the deployment owner must bind the rules to an Alertmanager route and assign the backend on-call rotation.

## API Target Down

1. Check the process/container state and recent termination reason.
2. Check `/livez`; no response means restart or routing failure, while HTTP 200 points to metrics auth or scrape configuration.
3. Validate `JXE_METRICS_TOKEN`, service discovery, network policy, and the Prometheus job name `jiuxiaoer-api`.
4. Do not restart repeatedly if the process reports configuration or schema validation failure; correct the rejected input first.

## Instance Not Ready

1. Read `/readyz` and identify the unhealthy component.
2. Remove the instance from traffic while `/livez` remains available for process diagnosis.
3. Follow the dependency section below; restore traffic only after `/readyz` remains 200 for two probe intervals.

## Required Dependency Unready

- `mysql`: check connectivity, pool exhaustion, schema migration status, and server saturation. Never point the Go backend at the legacy Sequelize database.
- `redis`: check connectivity and the Snowflake node lease. A lost lease requires the instance to remain out of service until ownership is reacquired through a clean restart.
- `rabbitmq`: check broker health, credentials, connection limits, and network policy. The manager reconnects with exponential backoff; do not create a parallel publisher.
- `snowflake_lease`: find another instance using the same node ID. Assign a unique `JXE_SNOWFLAKE_NODE_ID`; do not bypass the lease key.

## Outbox Backlog Or Dead Events

1. Check RabbitMQ health and `jxe_outbox_publish_total{result="failed"}` growth.
2. Inspect `locked_by`, `locked_until`, `retry_count`, `last_error_code`, and `last_error_detail` for the oldest rows.
3. Let expired leases be reclaimed automatically. Do not clear an active lease or mark an event published manually.
4. Fix the provider/topology issue before replaying dead events. Consumers must deduplicate by `event_id`.

## High 5xx Rate

1. Split errors by `route`, `status`, and `error_code`; correlate with `request_id` in structured logs.
2. Check dependency readiness and recent config/migration changes.
3. Roll back the application only when the additive migration remains compatible. Keep callbacks and Outbox consumers running if rollback would lose committed work.

## Rollback Guardrails

- Stop traffic expansion first, then pause new worker claims if needed.
- Do not delete Outbox rows, release another worker's lease, or run destructive down migrations.
- Preserve payment callbacks and reconciliation paths once real payment is enabled.
- Record start/end times, affected instances, data checks, and the final recovery action in the incident log.
## Snowflake Clock Rollback

1. Confirm `jxe_snowflake_clock_rollbacks_total` increased and identify the affected instance.
2. Inspect host/container NTP and virtualization clock events; do not change the Snowflake node ID as a workaround.
3. The generator keeps IDs monotonic with logical time, so traffic can continue while the clock source is repaired.
4. Restart only after wall time is stable and verify IDs remain increasing.

## RabbitMQ Consumer-Scoped Exchange Migration

The application publishes to `jxe.events.topic.v2`. Only `cache.invalidate` has an in-process consumer and durable queue.

1. Verify `jxe.events.cache.queue` is consuming normally from the v2 exchange.
2. Confirm no separately deployed consumer uses the legacy `jxe.events.topic` queues.
3. Export or discard legacy queue messages according to retention policy, then delete the old order/stock/store/delivery/audit/catalog queues and exchange through RabbitMQ administration.
4. Do not bind a durable queue until its consumer and queue-depth alerts are deployed.
