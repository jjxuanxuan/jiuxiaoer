-- +goose Up
ALTER TABLE payments
  ADD COLUMN reconcile_attempts INT UNSIGNED NOT NULL DEFAULT 0 AFTER failure_code,
  ADD COLUMN next_reconcile_at DATETIME(3) NULL AFTER reconcile_attempts,
  ADD KEY idx_payment_reconcile (provider, status, next_reconcile_at, id);

-- +goose Down
ALTER TABLE payments
  DROP KEY idx_payment_reconcile,
  DROP COLUMN next_reconcile_at,
  DROP COLUMN reconcile_attempts;
