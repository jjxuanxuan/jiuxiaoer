# CP1 launch-closure operations

This runbook covers the additive phase-one capabilities. Never repair order,
payment, stock, delivery, verification, print, or notification facts with ad-hoc
SQL. Use the authenticated retry, unlock, reassign, cancel, or force-complete
commands so the audit and Outbox facts remain complete.

## Release profile and readiness gate

`JXE_APP_ENV=production` is the general production safety baseline. It does not
mean that every process is a complete phase-one release candidate: approved
incident response may still disable printing, realtime, or MQ side effects while
the MySQL business facts continue to serve. Keep
`JXE_CP1_RELEASE_PROFILE=off` for ordinary or deliberately degraded production
operation.

For release-candidate validation, set `JXE_CP1_RELEASE_PROFILE=phase_one`. This
profile is valid only with `JXE_APP_ENV=production`, so local/preprod mock
configuration cannot acquire a release-candidate label. It is fail-closed at
both configuration validation and `/readyz`, and requires:

- a registered non-`fake` print Provider, printing, the CP1 fallback worker, and
  the print MQ consumer;
- realtime, its relay, and the realtime MQ consumer;
- the MQ publisher plus notification, print, cache, security, dispatch, and
  realtime consumers;
- RabbitMQ configured as required, topology drift blocking readiness, and the
  DB fallback path enabled;
- dispatch and its sweeper enabled;
- compliance, pickup verification, and delivery verification all in `enforce`.

The candidate environment therefore includes at least these values (alongside
the existing production secrets, URLs, and provider-specific configuration):

```dotenv
JXE_APP_ENV=production
JXE_CP1_RELEASE_PROFILE=phase_one
JXE_CP1_PRINT_ENABLED=true
JXE_CP1_PRINT_PROVIDER=<registered-real-provider>
JXE_CP1_WORKER_ENABLED=true
JXE_CP1_COMPLIANCE_MODE=enforce
JXE_CP1_PICKUP_VERIFICATION_MODE=enforce
JXE_CP1_DELIVERY_VERIFICATION_MODE=enforce
JXE_ORDER_IDEMPOTENCY_ENABLED=true
JXE_STOCK_RESERVE_ENABLED=true
JXE_REALTIME_ENABLED=true
JXE_REALTIME_RELAY_ENABLED=true
JXE_MQ_PUBLISH_ENABLED=true
JXE_MQ_CONSUMER_NOTIFICATION_ENABLED=true
JXE_MQ_CONSUMER_PRINT_ENABLED=true
JXE_MQ_CONSUMER_CACHE_ENABLED=true
JXE_MQ_CONSUMER_SECURITY_ENABLED=true
JXE_MQ_CONSUMER_DISPATCH_ENABLED=true
JXE_MQ_CONSUMER_REALTIME_ENABLED=true
JXE_MQ_DB_FALLBACK_ENABLED=true
JXE_MQ_FAIL_ON_TOPOLOGY_DRIFT=true
JXE_RABBITMQ_REQUIRED=true
JXE_DISPATCH_ENABLED=true
JXE_DISPATCH_WORKER_ENABLED=true
```

Before promotion, require `/readyz` to return HTTP 200 with
`checks.cp1_release_profile=ok`, `checks.rabbitmq=ok`, and
`checks.rabbitmq_topology=ok`. A non-`fake` provider name is only the static
gate; application startup must also resolve that exact name from the print
Provider registry. Never rename `fake` to bypass this check.

During an approved emergency degradation, switch the profile to `off` together
with the affected capability flag and record the change. This preserves the
normal degradation controls without allowing that deployment to be labelled a
phase-one release candidate.

## Migration preflight

### Resource-scoped idempotency digest rollout

Path-resource commands now bind the concrete resource ID and action into the
request digest. A retry created by the previous release can therefore have the
same actor, route-template and key but a different digest. During the 24-hour
`idempotency_keys` retention window that retry is deliberately rejected with
`409 IDEMPOTENCY_KEY_REUSED`; it must never replay a response from another
resource.

For a zero-conflict cutover, stop old-version writes and wait until their
24-hour idempotency records expire before promoting the new API version. If a
rolling cutover is required, keep the records, alert support to the bounded
compatibility window, and require clients receiving that 409 to refresh state
and submit the intended command with a new key. Do not delete active
idempotency records to make the rollout appear clean.

Before applying `202607220001_phase_one_business_closure.sql`, run this
read-only query against the target database:

```sql
SELECT provider, provider_request_id, COUNT(*) AS duplicate_count
FROM print_tasks
WHERE provider_request_id IS NOT NULL
GROUP BY provider, provider_request_id
HAVING COUNT(*) > 1;
```

It must return zero rows. Any result would make
`uk_cp1_print_provider_request` fail after earlier MySQL DDL statements have
already committed. Abort the rollout and reconcile each provider request against
the provider's authoritative query API; do not delete, merge, or rewrite print
facts merely to make the index pass. The migration itself also performs a
fail-fast bidirectional check of permission IDs `2132-2144` and their codes before
its first write. A permission-catalog conflict must be resolved by correcting the
deployment allocation, not by editing an existing permission in place.

## Controlled backfill and DQ command

`cmd/cp1-data` is the supported batch tool for the phase-one print and
verification migration. It orders batches by primary key, accepts an inclusive
ID range, writes an option-bound checkpoint after every batch, retries a failed
batch at most five times, and enforces a scan-rate ceiling. The allowed backfill
batch size is 500-2000 rows. Reports, checkpoints, and verification migration
audits are created with owner-only permissions.

The command is read-only by default. A write is refused unless all of these are
present at the same time:

1. a dedicated `JXE_CP1_DATA_DSN` (the runtime DSN is not accepted for writes);
2. `JXE_CP1_DATA_ALLOW_WRITE=true`;
3. `--execute --confirm=APPLY_CP1_DATA_BACKFILL`;
4. an explicit `--checkpoint-file`.

Do not set the dedicated DSN or write gate on API/worker deployments. First run
the same job without `--execute`, review the JSON/manual list, record DB CPU,
replication delay and core p95, then run on an approved ID range. Pause if DB
CPU exceeds 60%, replication delay exceeds five seconds, or core p95 grows more
than 20%; the tool deliberately does not guess those infrastructure signals.

Read-only DQ-001 through DQ-010 report (DQ-007 refuses to infer a cutover):

```bash
make cp1-dq CP1_DATA_ARGS='\
  --verification-cutover-at=2026-07-22T10:00:00+08:00 \
  --verification-audit-file=/secure/cp1/verification-audit.json \
  --report-file=/secure/cp1/dq-report.json'
```

Run DQ only after the phase-one migrations are applied. DQ-002 treats
`quantity_delta` as the available-quantity change and
`total_quantity_delta` as the derived-total change. It rejects NULL total
ledger fields, broken equations or continuity in either series, and a latest
ledger closing value that differs from current `available_qty` or from
`available_qty + reserved_qty + locked_qty`.

Dry-run the legacy print backlog conversion. Only `pending`/`retry_wait` tasks
with a complete order snapshot are eligible; terminal tasks remain immutable.
Use an explicit old-to-new mapping, or an approved fallback template:

```bash
make cp1-backfill CP1_DATA_ARGS='\
  --job=print-tasks --min-id=1 --max-id=9007199254740991 \
  --template-map=7001:9001,7002:9002 \
  --batch-size=500 --rows-per-second=500 \
  --checkpoint-file=/secure/cp1/print-tasks.checkpoint.json \
  --report-file=/secure/cp1/print-tasks.dry-run.json'
```

For `print-settings`, the same `--template-map` maps legacy setting template
IDs. A setting without a valid published `store_receipt`/`receipt.v1` mapping is
kept or made disabled and emitted to the manual list; device credentials are
never read into a report.

Historical verification migration requires an explicit cutover and reason. It
maps only non-active pre-cutover facts to `observe`, maps their historical
attempts, and refuses to downgrade an `active`/`locked` credential in place.
Those credentials enter the manual list and must be invalidated/rotated through
the authorized business command. An attempt without its parent verification is
also left unchanged and enters the manual list because the tool cannot invent
authoritative credential facts. The independent audit file records every
scanned verification ID, delivery ID, old/resulting mode, status, decision, and
manual-repair item:

```bash
make cp1-backfill CP1_DATA_ARGS='\
  --job=verification-history \
  --verification-cutover-at=2026-07-22T10:00:00+08:00 \
  --mapping-reason="pre-enforce history cannot be proven" \
  --verification-audit-file=/secure/cp1/verification-audit.json \
  --checkpoint-file=/secure/cp1/verification.checkpoint.json \
  --report-file=/secure/cp1/verification.dry-run.json'
```

The dry-run audit is planning evidence only and is rejected by DQ-007. Run the
approved write with a separate execution checkpoint; only its completed,
non-dry-run audit may be supplied to the release DQ report. DQ-007 also rejects
an audit whose cutover differs from the DQ cutover and reports orphan historical
attempts as release-gate violations.

After approval, repeat the identical option set with the write environment and
the two write flags. Use `--resume` only with that job's unchanged checkpoint;
the command rejects a checkpoint whose job, ID range, template mapping, cutover,
or reason differs. Never delete or hand-edit a checkpoint to bypass a conflict.

## Print backlog

1. Check `jxe_print_oldest_pending_seconds` and `jxe_print_tasks_total{status=...}`.
2. Filter `GET /api/v1/admin/print-tasks` by status and confirm whether failures
   are isolated to a shop, device, provider, or template version.
3. For offline hardware, keep the order accepted and restore the device. Printing
   must never roll the order transaction back.
4. Retry a `dead` or `retry_wait` task with
   `POST /api/v1/admin/print-tasks/{id}/retry`, an `Idempotency-Key`, and a reason.
5. If provider submit timed out, query the provider request ID before replaying;
   do not create an untracked duplicate receipt.

## Notification backlog

1. Check `jxe_notification_oldest_pending_seconds` and delivery status counts.
2. Confirm the published template, provider authorization, and recipient consent.
3. `suppressed` is terminal for unavailable consent; do not retry it.
4. Retry only `dead`/`retry_wait` deliveries through the admin API. Inbox messages
   remain the safe fallback and are independent of the external channel.

## Verification failures or suspected code exposure

1. Identify the delivery and stage without copying a plaintext code into a ticket,
   chat, log, or metric.
2. Check verification attempts by actor, result, and request ID. A non-current
   rider must be denied before a business-code attempt is counted.
3. If locked or exposed, call
   `POST /api/v1/admin/deliveries/{id}/verification/unlock` with a structured
   reason. The command generates a new code and permanently invalidates the old one.
4. If assignment ownership is wrong, reassign first; reassigning regenerates the
   active credentials and removes the previous rider's write scope atomically.

## Assignment recovery

1. Read `GET /api/v1/admin/deliveries/{id}/assignments` and the delivery's current
   `assignment_version`.
2. Submit assign/reassign with that exact version. A 409 means another operator
   won; refresh rather than overwriting the winner.
3. Verify there is one current rider and one active assignment. Notify both old and
   new riders through the generated Outbox events.

## Force completion

Force completion is disabled unless `JXE_CP1_FORCE_ACTION_ENABLED=true`. The maker
and checker must be distinct active administrators with the force-complete
permission. The maker first calls
`POST /api/v1/admin/deliveries/{id}/force-complete-requests`; that request only
creates a 30-minute pending approval and cannot change delivery state. The named
checker must authenticate separately and call
`POST /api/v1/admin/deliveries/{id}/force-complete` with the approval ID and exact
delivery version. Record a customer-confirmed reason and retain the approval,
before/after audit snapshot, and notification event. Never alter a normal rider
verification to look like a rider success; it must remain `overridden`.

## Identity provider or sensitive-data incident

1. Set `JXE_CP1_COMPLIANCE_MODE=enforce` and stop new restricted orders if provider
   results cannot be trusted. Unknown is never adult.
2. Treat a callback only as a change notification. Verify its signature, timestamp,
   provider request ID, and state binding, then query the provider API for the
   authoritative result. Do not accept the callback body as the adult conclusion.
3. The session flow stores only lifecycle status, adult result, verification level,
   policy/result references, provider subject reference, and audit hashes. It never
   accepts or stores a name, document number, document image, face image, or birth
   date. Legacy mask/hash columns remain empty for additive schema compatibility.
4. A verified adult fact has no fixed one-year expiry. It becomes unusable only
   when a provider/policy supplies `valid_until`, the provider revokes it, or an
   authorized manual review rejects it. Revoked, expired, minor, and unknown all
   fail the restricted-order gate.
5. `identity.verification.updated`, `.failed`, and `.revoked` are written to the
   Outbox in the same database transaction and consumed asynchronously by the
   security queue. RabbitMQ failure must not roll back the identity fact or become
   the order authorization source of truth.
6. Rotate the verification pepper/callback secret through secret management and
   follow the approved key-version migration procedure. Do not commit secrets.

## Rollback

1. Stop expansion and capture the application/config version.
2. Disable print and external notification workers; set verification/compliance to
   `observe` only when the incident commander and compliance owner approve it.
3. Roll the application back on the additive schema. Keep all CP1 task, audit,
   verification, assignment, provisioning, and identity facts.
4. Verify login, order query, payment callback, refund, inbox, and admin read smoke.
5. Reconcile affected order IDs before resuming workers or traffic.

Target functional rollback time is 30 minutes. Production rollout still requires
approved providers, real-device/sandbox smoke, alert routing, on-call ownership, and
the PRD decision gates.

## PRD runbook index

| ID | Procedure |
|---|---|
| RB-CP1-001 | `l1-payment-operations.md` payment callback/provider recovery |
| RB-CP1-002 | `l0-operations.md` consistency recovery plus stock reconciliation; never edit stock facts directly |
| RB-CP1-003 | Print backlog above |
| RB-CP1-004 | Notification backlog above |
| RB-CP1-005 | Verification failures or suspected code exposure above |
| RB-CP1-006 | Assignment recovery above |
| RB-CP1-007 | Identity provider or sensitive-data incident above |
| RB-CP1-008 | `l0-operations.md` outbox backlog/dead-event recovery |
