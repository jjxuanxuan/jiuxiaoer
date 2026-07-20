# Delivery return operations

## Receipt SLA or branch divergence

1. Open the admin delivery-return detail and verify the refund, physical receipt, and per-item inventory branches independently.
2. For an unreceived return, contact the assigned rider and authorized shop; never create a receipt or stock record from elapsed time alone.
3. Recover according to the provider state: an uncertain submission keeps the same `refund_no` and queries before any resubmission; `CLOSED` creates one replacement refund with a new `refund_no`; `ABNORMAL` requires merchant-platform handling and then reconciliation. Never use the generic retry action to clear a data-mismatch exception.
4. Confirm recovery produces one `delivery.return_closed` event only after all three branches are complete.

## Closed invariant violation

Treat as a P0 accounting incident. Disable new delivery-return approval and receipt writes, preserve callback processing, and compare the return, system after-sale, refund, receipt items, stock records, audit logs, and Outbox facts. Do not delete or rewrite financial or inventory facts manually.

## Premature customer notification

Disable `JXE_DELIVERY_RETURN_NOTIFICATION_ENABLED`, retain business processing, and inspect the offending event and notification consumer receipt. `delivery.return_requested` may target operations and the merchant only; customer communication starts at `delivery.return_approved`.

## Handoff rejection spike

Check whether failures are isolated to a rider, shop, expired-code window, or client version. Do not expose stored hashes or issue a new code until the previous code expired or reached its failure limit. Review rejection audits before reissuing.
