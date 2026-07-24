-- +goose Up
-- 配送路线使用的所有持久化坐标都规范为 GCJ-02。
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
-- 非破坏性回滚：关闭 JXE_MAP_ROUTE_ENABLED 并保留坐标契约，
-- 避免历史记录悄然变得含义不明。
SELECT 1;
