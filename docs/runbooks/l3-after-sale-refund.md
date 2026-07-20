# L3 After-Sale And Refund Operations

This runbook covers after-sale review, return receipt, replacement stock reservation, WeChat refunds, and reconciliation. Never mark a refund successful manually.

## Refund Exception

1. Inspect the refund, payment, order, after-sale, callback, history, audit, and Outbox records by `refund_no` and request ID.
2. For amount, currency, payment number, or provider refund ID mismatch, stop rollout expansion. Compare immutable local reservation data with the WeChat merchant console.
3. Do not edit aggregate amounts. Resolve the provider discrepancy, then choose recovery from the provider state: `submission_unknown` queries first and may resubmit the immutable request with the same `refund_no`; `CLOSED` creates one replacement refund with a new `refund_no`; `ABNORMAL` requires merchant-platform handling followed by the authorized reconcile action.
4. Confirm `payments.refunded_amount`, `orders.refunded_amount`, successful refund sum, and after-sale item allocation are equal before closing the incident.

## Refund Backlog

1. Check `jxe_refund_tasks` and `jxe_refund_oldest_pending_seconds` by status.
2. Verify worker configuration, instance logs, MySQL lock waits, WeChat connectivity, certificates, and refund callback URL.
3. A provider timeout is unknown, not failed. Query the existing refund number; never create a replacement refund number.
4. Restart workers one instance at a time. Leases expire after 30 seconds and tasks are reclaimed with `SKIP LOCKED`.

## Stored Refund Repair

1. Run the read-only repair-candidate scan and preview each candidate before applying a change.
2. Apply requires `refund:exception`, an idempotency key, and a fresh WeChat query; never supply or write an administrator-declared success state.
3. `RESOURCE_NOT_EXISTS` may schedule original-number resubmission only for uncertain submissions or approved legacy mismatch/retryable cases. Permanent provider errors and `ABNORMAL` remain exceptions for manual investigation.
4. Retain the before/after status, operator, WeChat `Request-Id`, decision, audit record, and Outbox event as repair evidence.

## Callback Failure

1. Inspect certificate rotation, API v3 key, callback timestamp, signature, merchant configuration, and public callback reachability.
2. Never log or persist decrypted callback bodies, signatures, private keys, API v3 keys, open IDs, or customer contact data.
3. Replay callbacks only through WeChat or the signed test fixture. Duplicate event IDs must remain idempotent.

## Review Backlog

1. Split waiting requests by `submitted`, `shop_reviewing`, and `platform_reviewing`.
2. Platform queues include high-value requests, compensation, late delivery, disputes, and historical orders without a return policy snapshot.
3. Check RBAC assignments and merchant shop scope before reassigning work. Do not widen JWT scope to clear a queue.

## Rollback

- Disable `JXE_AFTERSALE_ENABLED` to stop new applications.
- Disable `JXE_REFUND_EXECUTION_ENABLED` to stop provider calls, while keeping signed callbacks and reconciliation available.
- Keep additive L3 tables and amount columns during application rollback. Production down migration is prohibited after L3 data exists.
- Replacement and compensation remain independently reviewable; do not convert them into cash refunds during rollback.
