-- +goose Up
ALTER TABLE customer_addresses
  ADD COLUMN city_code CHAR(6) NULL AFTER city,
  ADD COLUMN district_code CHAR(6) NULL AFTER district,
  ADD COLUMN doorplate VARCHAR(128) NULL AFTER address_detail,
  ADD COLUMN version INT UNSIGNED NOT NULL DEFAULT 1 AFTER is_default,
  ADD COLUMN active_default_customer_id BIGINT UNSIGNED
    GENERATED ALWAYS AS (CASE WHEN is_default = 1 AND deleted_at IS NULL THEN customer_id ELSE NULL END) STORED,
  ADD KEY idx_customer_active (customer_id, deleted_at, updated_at, id),
  ADD UNIQUE KEY uk_customer_active_default (active_default_customer_id);

ALTER TABLE shops
  ADD COLUMN city_code CHAR(6) NULL AFTER city,
  ADD COLUMN service_mode VARCHAR(16) NOT NULL DEFAULT 'disabled' AFTER business_status,
  ADD COLUMN service_radius_m INT UNSIGNED NOT NULL DEFAULT 0 AFTER service_mode,
  ADD COLUMN service_area_version INT UNSIGNED NOT NULL DEFAULT 1 AFTER service_radius_m,
  ADD COLUMN priority INT NOT NULL DEFAULT 0 AFTER service_area_version,
  ADD COLUMN delivery_fee_amount BIGINT UNSIGNED NOT NULL DEFAULT 0 AFTER priority,
  ADD COLUMN free_delivery_threshold_amount BIGINT UNSIGNED NULL AFTER delivery_fee_amount,
  ADD COLUMN delivery_eta_min SMALLINT UNSIGNED NOT NULL DEFAULT 15 AFTER free_delivery_threshold_amount,
  ADD COLUMN delivery_eta_max SMALLINT UNSIGNED NOT NULL DEFAULT 25 AFTER delivery_eta_min,
  ADD COLUMN overtime_policy_code VARCHAR(64) NULL AFTER delivery_eta_max,
  ADD KEY idx_shop_service_city (city_code, status, business_status, service_mode, priority, id);

CREATE TABLE shop_business_hours (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  shop_id BIGINT UNSIGNED NOT NULL,
  day_of_week TINYINT UNSIGNED NOT NULL,
  open_time TIME NOT NULL,
  close_time TIME NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  created_by BIGINT UNSIGNED NULL,
  updated_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_shop_day_open (shop_id, day_of_week, open_time, deleted_at),
  KEY idx_shop_day_status (shop_id, day_of_week, status, open_time, close_time),
  CONSTRAINT chk_shop_hours_day CHECK (day_of_week BETWEEN 1 AND 7),
  CONSTRAINT chk_shop_hours_range CHECK (close_time > open_time)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE home_slots (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  city_code CHAR(6) NULL,
  slot_type VARCHAR(32) NOT NULL,
  slot_key VARCHAR(64) NOT NULL,
  title VARCHAR(128) NOT NULL,
  payload_json JSON NOT NULL,
  start_at DATETIME(3) NULL,
  end_at DATETIME(3) NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'draft',
  sort_order INT NOT NULL DEFAULT 0,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_by BIGINT UNSIGNED NOT NULL,
  updated_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  KEY idx_home_effective (city_code, status, start_at, end_at, sort_order, id),
  KEY idx_home_admin (status, updated_at, id),
  KEY idx_home_slot_key (city_code, slot_type, slot_key, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE orders
  ADD COLUMN delivery_promise_snapshot JSON NULL AFTER address_snapshot;

-- +goose Down
ALTER TABLE orders DROP COLUMN delivery_promise_snapshot;
DROP TABLE home_slots;
DROP TABLE shop_business_hours;
ALTER TABLE shops
  DROP INDEX idx_shop_service_city,
  DROP COLUMN overtime_policy_code,
  DROP COLUMN delivery_eta_max,
  DROP COLUMN delivery_eta_min,
  DROP COLUMN free_delivery_threshold_amount,
  DROP COLUMN delivery_fee_amount,
  DROP COLUMN priority,
  DROP COLUMN service_area_version,
  DROP COLUMN service_radius_m,
  DROP COLUMN service_mode,
  DROP COLUMN city_code;
ALTER TABLE customer_addresses
  DROP INDEX uk_customer_active_default,
  DROP INDEX idx_customer_active,
  DROP COLUMN active_default_customer_id,
  DROP COLUMN version,
  DROP COLUMN doorplate,
  DROP COLUMN district_code,
  DROP COLUMN city_code;
