# L1 Identity And Payment Operations

This runbook covers WeChat identity, payment callbacks, active payment queries, and unpaid order expiry. Payment and stock inconsistencies block release expansion.

## Order Expiry Lag

1. Check `jxe_order_expiry_pending`, `jxe_order_expiry_lag_seconds`, and `jxe_order_expiry_total` by result.
2. If `provider_query_failed` or `provider_close_failed` grows, verify WeChat Pay connectivity and credentials. Do not force-close the local order while provider state is unknown.
3. Check MySQL lock waits on `orders`, `payments`, and `product_stocks`. The required lock order is order, payment, stock.
4. Restart one worker instance at a time. Multi-instance scans are safe, but simultaneous restarts make diagnosis harder.
5. Reconcile every order whose provider state is `SUCCESS` before manually releasing stock.

## Payment Callback Or Provider Failure

1. Split `jxe_payment_operations_total` by `provider` and `result`.
2. For signature or merchant identity failures, inspect certificate serial rotation, callback timestamp, app ID, merchant ID, and API v3 key. Never log the signature, private key, API v3 key, openid, or raw decrypted callback.
3. For amount mismatch, compare `payments.amount/currency` with the provider transaction. Do not modify the order or stock to match an untrusted callback.
4. For `payment_exception`, the provider reports success after the order was closed. Stop release expansion and reconcile/refund the transaction before restoring normal processing.
5. Re-send callbacks only through the provider console or the signed staging fixture. Direct database status updates are prohibited.

## Credential Rotation

- Store miniapp secret, merchant private key, API v3 key, and metrics token in the deployment secret manager.
- WeChat Pay platform certificates are downloaded and rotated by the official SDK. Keep the process running through the current/previous certificate overlap.
- Validate payment creation, active query, and a signed callback in staging after rotation.

## Rollback

- Disable new payment creation first, but keep signed callbacks and active reconciliation available.
- Stop the expiry worker only when provider query is unavailable and lag is being actively monitored.
- Keep additive L1 columns and callback facts during application rollback. Do not run destructive production down migrations.
