-- +goose Up
-- All persisted coordinates used by delivery routing are normalized to GCJ-02.
ALTER TABLE customer_addresses
  ADD COLUMN coordinate_system VARCHAR(16) NOT NULL DEFAULT 'gcj02' AFTER longitude,
  ADD CONSTRAINT chk_customer_address_coordinate_system CHECK (coordinate_system = 'gcj02'),
  ADD CONSTRAINT chk_customer_address_coordinate_pair CHECK ((latitude IS NULL) = (longitude IS NULL));

ALTER TABLE shops
  ADD COLUMN coordinate_system VARCHAR(16) NOT NULL DEFAULT 'gcj02' AFTER longitude,
  ADD CONSTRAINT chk_shop_coordinate_system CHECK (coordinate_system = 'gcj02'),
  ADD CONSTRAINT chk_shop_coordinate_pair CHECK ((latitude IS NULL) = (longitude IS NULL));

ALTER TABLE rider_runtime_states
  ADD COLUMN coordinate_system VARCHAR(16) NOT NULL DEFAULT 'gcj02' AFTER longitude,
  ADD CONSTRAINT chk_rider_runtime_coordinate_system CHECK (coordinate_system = 'gcj02'),
  ADD CONSTRAINT chk_rider_runtime_coordinate_pair CHECK ((latitude IS NULL) = (longitude IS NULL));

-- +goose Down
-- Non-destructive rollback: disable JXE_MAP_ROUTE_ENABLED and retain the
-- coordinate contract so historical rows cannot silently become ambiguous.
SELECT 1;
