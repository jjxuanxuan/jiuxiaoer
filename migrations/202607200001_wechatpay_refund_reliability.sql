-- +goose Up
ALTER TABLE refunds
  ADD COLUMN replaces_refund_id BIGINT UNSIGNED NULL AFTER payment_id,
  ADD COLUMN reason VARCHAR(80) NOT NULL DEFAULT 'after-sale refund' AFTER currency,
  ADD COLUMN notify_url VARCHAR(256) NULL AFTER reason,
  ADD UNIQUE KEY uk_refund_replaces (replaces_refund_id);

-- +goose Down
ALTER TABLE refunds
  DROP KEY uk_refund_replaces,
  DROP COLUMN notify_url,
  DROP COLUMN reason,
  DROP COLUMN replaces_refund_id;
