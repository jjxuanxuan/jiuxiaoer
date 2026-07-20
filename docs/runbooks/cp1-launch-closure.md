# CP1 launch-closure operations

This runbook covers the additive phase-one capabilities. Never repair order,
payment, stock, delivery, verification, print, or notification facts with ad-hoc
SQL. Use the authenticated retry, unlock, reassign, cancel, or force-complete
commands so the audit and Outbox facts remain complete.

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
