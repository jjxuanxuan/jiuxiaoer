-- +goose Up
ALTER TABLE refunds
  ADD COLUMN provider_accepted_at DATETIME(3) NULL AFTER requested_at,
  ADD KEY idx_refund_provider_accepted (provider, provider_accepted_at, id);

-- +goose Down
ALTER TABLE refunds
  DROP KEY idx_refund_provider_accepted,
  DROP COLUMN provider_accepted_at;
