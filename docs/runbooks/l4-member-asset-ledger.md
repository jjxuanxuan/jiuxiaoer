# L4 Member And Asset Ledger Runbook

## First Response

1. Set `JXE_ASSET_WRITE_ENABLED=false`, `JXE_COMPENSATION_ISSUE_ENABLED=false`, `JXE_ASSET_EXPIRY_ENABLED=false`, and `JXE_ASSET_REPAIR_ENABLED=false` for the next deployment/config rollout.
2. Keep asset reads and the database available. Do not update or delete `asset_entries`.
3. Record the alert time, instance, request IDs, transaction IDs, customer IDs, compensation IDs, and reconciliation job IDs.
4. Run a dry-run reconciliation before any repair or reversal.

## Ledger Invariant Failure

- Trigger: `jxe_asset_unbalanced_transactions > 0` or `jxe_asset_negative_customer_balances > 0`.
- Stop all asset writes immediately.
- Query the affected transaction with all entries and verify `SUM(delta)` by asset type/unit.
- Do not use projection repair for unbalanced entries. Preserve database and application logs, identify the producing code path, and prepare an approved business reversal or data incident procedure.
- Resume only after the invariant query returns zero and the concurrency acceptance suite passes.

## Projection Mismatch

- Trigger: `jxe_asset_projection_mismatches > 0` for two minutes.
- Run `POST /api/v1/admin/asset-reconciliations` with `dry_run=true` and the narrowest customer/account scope.
- Confirm every difference is `projection_mismatch`; any `unbalanced_entries` item blocks automatic repair.
- Enable `JXE_ASSET_REPAIR_ENABLED` only for the repair window, call the repair endpoint, disable it again, and rerun dry-run reconciliation.
- Repair changes `asset_balances` or `member_profiles`; it never changes `asset_entries`.

## Compensation Mismatch

- Trigger: `jxe_asset_compensation_mismatches > 0`.
- Stop compensation claims, then compare `compensation_ledger.amount/customer_id/status` with the linked `asset_transactions` source `compensation/{id}`.
- If the asset transaction is posted but compensation remains `issuing`, rerun the worker with the same source key so it only backfills `issued + asset_transaction_id`.
- If compensation is `issued` without a valid transaction, do not create an unreferenced replacement transaction. Escalate as a critical data incident.
- Never modify `payments.refunded_amount` or `orders.refunded_amount` during compensation recovery.

## Compensation Backlog

- Check `locked_by`, `locked_until`, `attempts`, `next_retry_at`, and `failure_code` for approved/issuing rows.
- Verify asset write and compensation issue gates, MySQL health, and the worker instance ID.
- Expired leases are safe to reclaim because `source_type + source_id + action` is unique.

## Hold Or Expiry Backlog

- Verify the asset worker is enabled and inspect oldest active holds and expired lots.
- A frozen lot is not expired until commit/release. Releasing an already expired frozen lot must transfer it directly to the platform expiry account.
- Do not bulk set hold/lot statuses. Resume the worker in small batches and verify lot and hold conservation after each batch.

## Rollback

1. Disable new writes and worker claims.
2. Let current short transactions finish; reclaim expired compensation leases.
3. Reconcile transaction balance, customer non-negativity, projections, holds, lots, members, and compensation.
4. Reverse approved business errors through the ledger; rebuild only derived projections.
5. Roll back the application to the L3-compatible image while retaining L4 tables once any posted L4 transaction exists.
