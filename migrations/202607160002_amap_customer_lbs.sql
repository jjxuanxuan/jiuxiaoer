-- +goose Up
CREATE TABLE service_cities (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  city_code CHAR(6) NOT NULL,
  province_code CHAR(6) NOT NULL,
  name VARCHAR(64) NOT NULL,
  pinyin VARCHAR(128) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'draft',
  sort_order INT NOT NULL DEFAULT 0,
  default_browse_shop_id BIGINT UNSIGNED NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  published_at DATETIME(3) NULL,
  created_by BIGINT UNSIGNED NOT NULL,
  updated_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_service_city_code (city_code),
  KEY idx_service_city_public (status, sort_order, city_code),
  CONSTRAINT chk_service_city_status CHECK (status IN ('draft','published','disabled'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE service_city_adcodes (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  service_city_id BIGINT UNSIGNED NOT NULL,
  adcode CHAR(6) NOT NULL,
  standard_name VARCHAR(64) NOT NULL,
  level VARCHAR(16) NOT NULL,
  created_by BIGINT UNSIGNED NOT NULL,
  updated_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_service_city_adcode (adcode),
  KEY idx_service_city_adcodes_city (service_city_id, adcode),
  CONSTRAINT fk_service_city_adcodes_city FOREIGN KEY (service_city_id) REFERENCES service_cities(id),
  CONSTRAINT chk_service_city_adcodes_level CHECK (level IN ('city','district','county'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE delivery_promise_policies (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  policy_code VARCHAR(64) NOT NULL,
  version INT UNSIGNED NOT NULL,
  title VARCHAR(128) NOT NULL,
  summary VARCHAR(255) NOT NULL,
  terms_url VARCHAR(512) NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'draft',
  effective_from DATETIME(3) NULL,
  effective_to DATETIME(3) NULL,
  published_at DATETIME(3) NULL,
  published_by BIGINT UNSIGNED NULL,
  created_by BIGINT UNSIGNED NOT NULL,
  updated_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  published_policy_code VARCHAR(64) GENERATED ALWAYS AS (
    CASE WHEN status = 'published' THEN policy_code ELSE NULL END
  ) STORED,
  UNIQUE KEY uk_promise_policy_version (policy_code, version),
  UNIQUE KEY uk_promise_policy_published (published_policy_code),
  KEY idx_promise_policy_status (status, effective_from, effective_to),
  CONSTRAINT chk_promise_policy_status CHECK (status IN ('draft','published','retired')),
  CONSTRAINT chk_promise_policy_window CHECK (
    effective_to IS NULL OR effective_from IS NULL OR effective_to > effective_from
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE customer_addresses
  ADD COLUMN poi_id VARCHAR(64) NULL AFTER doorplate,
  ADD COLUMN formatted_address VARCHAR(255) NULL AFTER poi_id,
  ADD COLUMN location_source VARCHAR(24) NOT NULL DEFAULT 'legacy' AFTER coordinate_system,
  ADD COLUMN geocode_provider VARCHAR(16) NULL AFTER location_source,
  ADD COLUMN geocode_status VARCHAR(24) NOT NULL DEFAULT 'unverified' AFTER geocode_provider,
  ADD COLUMN geocoded_at DATETIME(3) NULL AFTER geocode_status,
  ADD KEY idx_customer_address_geocode (customer_id, geocode_status, deleted_at, id),
  ADD KEY idx_customer_address_poi (poi_id),
  ADD CONSTRAINT chk_customer_address_location_source CHECK (location_source IN ('amap_poi','map_pin','manual_import','legacy')),
  ADD CONSTRAINT chk_customer_address_geocode_status CHECK (geocode_status IN ('verified','unverified','missing','conflict'));

UPDATE customer_addresses
SET geocode_status = CASE
  WHEN latitude IS NOT NULL AND longitude IS NOT NULL THEN 'unverified'
  WHEN latitude IS NULL AND longitude IS NULL THEN 'missing'
  ELSE 'conflict'
END;

ALTER TABLE shops
  ADD COLUMN overtime_policy_version INT UNSIGNED NULL AFTER overtime_policy_code;

-- +goose Down
ALTER TABLE shops
  DROP COLUMN overtime_policy_version;

ALTER TABLE customer_addresses
  DROP CHECK chk_customer_address_geocode_status,
  DROP CHECK chk_customer_address_location_source,
  DROP INDEX idx_customer_address_poi,
  DROP INDEX idx_customer_address_geocode,
  DROP COLUMN geocoded_at,
  DROP COLUMN geocode_status,
  DROP COLUMN geocode_provider,
  DROP COLUMN location_source,
  DROP COLUMN formatted_address,
  DROP COLUMN poi_id;

DROP TABLE delivery_promise_policies;
DROP TABLE service_city_adcodes;
DROP TABLE service_cities;
