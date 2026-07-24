-- +goose Up
-- 配送退回采用 additive migration。即使应用回退，这些事实表和来源字段也必须保留。
CREATE TABLE delivery_returns (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  return_no VARCHAR(64) NOT NULL,
  delivery_order_id BIGINT UNSIGNED NOT NULL,
  active_delivery_order_id BIGINT UNSIGNED NULL,
  order_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  rider_id BIGINT UNSIGNED NOT NULL,
  incident_id BIGINT UNSIGNED NULL,
  after_sale_id BIGINT UNSIGNED NULL,
  reason_code VARCHAR(32) NOT NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'requested',
  initiator_type VARCHAR(16) NOT NULL,
  initiator_id BIGINT UNSIGNED NOT NULL,
  request_note VARCHAR(500) NULL,
  approved_by BIGINT UNSIGNED NULL,
  approved_at DATETIME(3) NULL,
  handoff_code_hash VARCHAR(255) NULL,
  handoff_code_expires_at DATETIME(3) NULL,
  handoff_failed_attempts INT UNSIGNED NOT NULL DEFAULT 0,
  receipt_deadline_at DATETIME(3) NULL,
  requested_at DATETIME(3) NOT NULL,
  arrived_at DATETIME(3) NULL,
  received_at DATETIME(3) NULL,
  closed_at DATETIME(3) NULL,
  cancelled_at DATETIME(3) NULL,
  version BIGINT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_delivery_return_no (return_no),
  UNIQUE KEY uk_delivery_return_active_order (active_delivery_order_id),
  UNIQUE KEY uk_delivery_return_incident (incident_id),
  UNIQUE KEY uk_delivery_return_after_sale (after_sale_id),
  KEY idx_delivery_return_order_created (order_id, created_at, id),
  KEY idx_delivery_return_shop_status_deadline (shop_id, status, receipt_deadline_at, id),
  KEY idx_delivery_return_rider_status_updated (rider_id, status, updated_at, id),
  KEY idx_delivery_return_status_updated (status, updated_at, id),
  CONSTRAINT chk_delivery_return_status CHECK (status IN ('requested','returning','arrived','received','closed','cancelled','disputed','exception')),
  CONSTRAINT chk_delivery_return_reason CHECK (reason_code IN ('customer_unreachable','customer_refused','address_wrong','damaged_in_transit','other')),
  CONSTRAINT chk_delivery_return_active_key CHECK (
    (status IN ('closed','cancelled') AND active_delivery_order_id IS NULL)
    OR (status NOT IN ('closed','cancelled') AND active_delivery_order_id IS NOT NULL AND active_delivery_order_id = delivery_order_id)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE delivery_return_history (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  delivery_return_id BIGINT UNSIGNED NOT NULL,
  from_status VARCHAR(24) NULL,
  to_status VARCHAR(24) NULL,
  action VARCHAR(64) NOT NULL,
  actor_type VARCHAR(32) NOT NULL,
  actor_id BIGINT UNSIGNED NULL,
  request_id VARCHAR(64) NULL,
  idempotency_key VARCHAR(128) NULL,
  metadata_json JSON NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_delivery_return_history_created (delivery_return_id, created_at, id),
  KEY idx_delivery_return_history_request (request_id),
  KEY idx_delivery_return_history_actor_action_created (actor_type, actor_id, action, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE after_sales
  ADD COLUMN initiator_type VARCHAR(32) NOT NULL DEFAULT 'customer' AFTER merchant_id,
  ADD COLUMN source_type VARCHAR(32) NOT NULL DEFAULT 'customer_request' AFTER initiator_type,
  ADD COLUMN source_id BIGINT UNSIGNED NULL AFTER source_type,
  ADD UNIQUE KEY uk_after_sales_source (source_type, source_id);

CREATE TABLE return_receipt_items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  return_receipt_id BIGINT UNSIGNED NOT NULL,
  after_sale_item_id BIGINT UNSIGNED NOT NULL,
  order_item_id BIGINT UNSIGNED NOT NULL,
  shop_product_id BIGINT UNSIGNED NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  expected_quantity INT NOT NULL,
  received_quantity INT NOT NULL,
  disposition VARCHAR(16) NOT NULL,
  policy_code VARCHAR(64) NOT NULL,
  policy_version VARCHAR(32) NOT NULL,
  available_before INT NULL,
  available_after INT NULL,
  note VARCHAR(500) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_return_receipt_after_sale_item (return_receipt_id, after_sale_item_id),
  KEY idx_return_receipt_item_order_item (order_item_id, return_receipt_id),
  KEY idx_return_receipt_item_shop_product (shop_product_id, created_at, id),
  CONSTRAINT chk_return_receipt_item_quantity CHECK (expected_quantity > 0 AND received_quantity >= 0),
  CONSTRAINT chk_return_receipt_item_disposition CHECK (disposition IN ('restock','damaged','discard'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE stock_records
  ADD COLUMN business_action_key VARCHAR(128) NULL AFTER idempotency_key,
  ADD UNIQUE KEY uk_stock_business_action (business_action_key);

-- +goose Down
-- 按设计仅向前迁移。回滚应用时关闭配送退回写入开关，
-- 不要删除资金、库存、收货或审计事实。
