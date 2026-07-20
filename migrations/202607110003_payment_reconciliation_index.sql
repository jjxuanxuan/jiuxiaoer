-- +goose Up
ALTER TABLE payments
  ADD KEY idx_provider_creating_reconcile (provider, status, updated_at, id);

-- +goose Down
ALTER TABLE payments
  DROP KEY idx_provider_creating_reconcile;
