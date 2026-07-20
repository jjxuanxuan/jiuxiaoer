# RabbitMQ Event Backbone Runbook

This runbook covers the Phase 1.5 topology declared by the Go backend. MySQL
remains the business source of truth. Never purge a queue, delete Outbox rows,
or bulk replay messages while investigating an incident.

## Broker unavailable or reconnect storm

1. Confirm core API transactions still commit and new `outbox_events` remain `pending`.
2. Check credentials, vhost permissions, TLS/network policy, connection and channel limits.
3. Restore RabbitMQ; watch `jxe_outbox_oldest_pending_seconds` and queue depth converge.
4. Reconcile Outbox, receipts, notification records, and print tasks before closing the incident.

## Unrouted event

1. Treat any increase in `jxe_mq_unrouted_total` as P1.
2. Query `/api/v1/admin/mq/dead-letters?consumer_name=unrouted&status=open`.
3. Compare the event registry and `DefaultTopology` binding. Do not add a wildcard binding as a shortcut.
4. Add or repair the approved consumer contract, deploy it, then replay one event with an idempotency key.

## Dead letter

1. Query the dead-letter API and correlate `event_id`, `request_id`, Outbox, receipt, and business task.
2. Repair the downstream prerequisite and verify the event schema is still supported.
3. Replay a single event using `POST /api/v1/admin/mq/dead-letters/{id}/replay` with a reason and expected version.
4. Confirm the replay Outbox event is published and the business idempotency constraint prevents duplicate side effects.

## Consumer backlog

1. Identify the queue and consumer from metrics; check retry growth and DB/provider latency.
2. Pause only the affected consumer. Independent queues must keep processing.
3. Scale the consumer within DB/provider limits or restore the dependency.
4. Keep the DB fallback enabled until the queue and reconciliation differences reach zero.

Identity verification events are security observations, not an authorization RPC.
The callback transaction first commits the current adult fact and an Outbox row;
the security consumer then records its idempotent receipt. Order creation always
reads MySQL directly, so a RabbitMQ outage may delay observation and alerting but
must neither allow an unknown customer nor block an already verified adult.

## Topology drift

1. Call `POST /api/v1/admin/mq/topology/verify`; this endpoint is read-only.
2. Compare exchange type/durability/alternate exchange, queue durability/DLX/TTL, and bindings.
3. Parameter conflicts require an explicit queue/exchange migration. The application must not delete or recreate resources automatically.
4. Do not mutate the legacy `/` vhost in place. Provision a dedicated vhost with `make mq-vhost-provision`, apply the v1 topology with `make mq-topology-apply`, verify it with `make mq-topology-verify`, then switch publishers and consumers through configuration.

## Independent worker processes

Run `go run ./cmd/worker -role <role>` (or `make run-worker` with
`JXE_WORKER_ROLE`) for `outbox-publisher`, `mq-consumer-notification`,
`mq-consumer-print`, `mq-consumer-cache`, `mq-consumer-security`, or
`mq-dead-sink`. Give every process a distinct `JXE_SNOWFLAKE_NODE_ID` and
instance ID. Startup declares the exact topology and exits on a parameter
conflict; SIGTERM cancels consumption, waits for handlers, and closes shared
connections.

## DB-only rollback

1. Set all `JXE_MQ_CONSUMER_*_ENABLED=false` and keep `JXE_MQ_DB_FALLBACK_ENABLED=true`.
2. Stop new MQ consumption, allow in-flight handlers to finish, and retain all queues.
3. Keep additive receipt/dead/replay tables and Outbox contract columns.
4. Run notification/print/cache reconciliation and provider Query for unknown results.
5. Re-enable a consumer only after duplicate and missing side effects are both zero.

## Phase 1.5 acceptance gate

Run `make acceptance-phase15` from `backend-go`. It performs, in order:

1. The full original business gate (`make verify-cp1`): formatting, OpenAPI coverage, registry checks, vet, race tests, migrations, seed, database-backed integration tests, and Prometheus rule validation.
2. Read-only verification of the managed v1 topology in the configured dedicated vhost.
3. All `ACC-RMQ-001` through `ACC-RMQ-024` checks in a disposable `jxe-rmq-integration` vhost, including duplicate delivery, consumer stop/recovery, notification lifecycle, DB fallback, print wake-up, provider unknown reconciliation, dead letter, replay, authorization, and schema safety.

The gate is fail-closed: any failed or skipped required command exits non-zero. Provider checks use deterministic fake adapters; production release still requires the separately approved real printer and WeChat smoke records.
