-- +goose Up
ALTER TABLE orders
  ADD COLUMN refunded_amount BIGINT NOT NULL DEFAULT 0 AFTER paid_amount,
  ADD COLUMN after_sale_status VARCHAR(32) NOT NULL DEFAULT 'none' AFTER refunded_amount,
  ADD KEY idx_after_sale_status (after_sale_status, updated_at, id);

ALTER TABLE products
  ADD COLUMN return_eligible TINYINT(1) NOT NULL DEFAULT 0 AFTER status,
  ADD COLUMN return_policy_code VARCHAR(64) NOT NULL DEFAULT 'not_configured' AFTER return_eligible,
  ADD COLUMN return_policy_version VARCHAR(32) NOT NULL DEFAULT '1' AFTER return_policy_code,
  ADD COLUMN sealed_package_required TINYINT(1) NOT NULL DEFAULT 1 AFTER return_policy_version;

ALTER TABLE stock_records
  ADD UNIQUE KEY uk_stock_source_idempotency (source_type, source_id, shop_product_id, idempotency_key);

CREATE TABLE after_sales (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  after_sale_no VARCHAR(64) NOT NULL,
  order_id BIGINT UNSIGNED NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  merchant_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  type VARCHAR(32) NOT NULL,
  requested_resolution VARCHAR(32) NOT NULL,
  approved_resolution VARCHAR(32) NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'submitted',
  requested_amount BIGINT NOT NULL DEFAULT 0,
  approved_amount BIGINT NOT NULL DEFAULT 0,
  refunded_amount BIGINT NOT NULL DEFAULT 0,
  compensation_amount BIGINT NOT NULL DEFAULT 0,
  include_delivery_fee TINYINT(1) NOT NULL DEFAULT 0,
  reason_code VARCHAR(64) NULL,
  description VARCHAR(1000) NOT NULL,
  appealed_at DATETIME(3) NULL,
  submitted_at DATETIME(3) NOT NULL,
  approved_at DATETIME(3) NULL,
  rejected_at DATETIME(3) NULL,
  closed_at DATETIME(3) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  created_by BIGINT UNSIGNED NULL,
  updated_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_after_sale_no (after_sale_no),
  KEY idx_customer_created (customer_id, created_at, id),
  KEY idx_shop_status (shop_id, status, updated_at, id),
  KEY idx_platform_status (status, updated_at, id),
  KEY idx_order (order_id, id),
  CONSTRAINT chk_after_sale_amounts CHECK (requested_amount >= 0 AND approved_amount >= 0 AND refunded_amount >= 0 AND approved_amount <= requested_amount AND refunded_amount <= approved_amount)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE after_sale_items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  after_sale_id BIGINT UNSIGNED NOT NULL,
  order_id BIGINT UNSIGNED NOT NULL,
  order_item_id BIGINT UNSIGNED NOT NULL,
  shop_product_id BIGINT UNSIGNED NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  requested_quantity INT NOT NULL,
  approved_quantity INT NOT NULL DEFAULT 0,
  requested_amount BIGINT NOT NULL,
  approved_amount BIGINT NOT NULL DEFAULT 0,
  refunded_amount BIGINT NOT NULL DEFAULT 0,
  return_disposition VARCHAR(32) NOT NULL DEFAULT 'none',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  UNIQUE KEY uk_after_sale_order_item (after_sale_id, order_item_id),
  KEY idx_order_item (order_item_id, after_sale_id),
  CONSTRAINT chk_after_sale_item_amounts CHECK (requested_quantity > 0 AND approved_quantity >= 0 AND requested_amount >= 0 AND approved_amount >= 0 AND refunded_amount >= 0 AND approved_quantity <= requested_quantity AND approved_amount <= requested_amount AND refunded_amount <= approved_amount)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE after_sale_evidence (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  after_sale_id BIGINT UNSIGNED NOT NULL,
  token_id VARCHAR(128) NOT NULL,
  object_key VARCHAR(512) NOT NULL,
  mime_type VARCHAR(64) NOT NULL,
  size_bytes BIGINT UNSIGNED NOT NULL,
  sha256 CHAR(64) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'verified',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  UNIQUE KEY uk_after_sale_object (after_sale_id, object_key),
  UNIQUE KEY uk_evidence_token (token_id),
  KEY idx_after_sale (after_sale_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE after_sale_history (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  after_sale_id BIGINT UNSIGNED NOT NULL,
  actor_type VARCHAR(32) NOT NULL,
  actor_id BIGINT UNSIGNED NOT NULL,
  action VARCHAR(64) NOT NULL,
  from_status VARCHAR(32) NULL,
  to_status VARCHAR(32) NULL,
  reason_code VARCHAR(64) NULL,
  remark VARCHAR(1000) NULL,
  request_id VARCHAR(64) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_after_sale_created (after_sale_id, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE refunds (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  refund_no VARCHAR(64) NOT NULL,
  after_sale_id BIGINT UNSIGNED NOT NULL,
  order_id BIGINT UNSIGNED NOT NULL,
  payment_id BIGINT UNSIGNED NOT NULL,
  provider VARCHAR(32) NOT NULL,
  provider_refund_id VARCHAR(128) NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'creating',
  amount BIGINT NOT NULL,
  total_amount BIGINT NOT NULL,
  currency CHAR(3) NOT NULL,
  provider_status VARCHAR(64) NULL,
  failure_code VARCHAR(64) NULL,
  failure_detail VARCHAR(512) NULL,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_retry_at DATETIME(3) NULL,
  locked_until DATETIME(3) NULL,
  locked_by VARCHAR(128) NULL,
  requested_at DATETIME(3) NOT NULL,
  succeeded_at DATETIME(3) NULL,
  failed_at DATETIME(3) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  UNIQUE KEY uk_refund_no (refund_no),
  UNIQUE KEY uk_provider_refund (provider, provider_refund_id),
  KEY idx_refund_claim (status, next_retry_at, locked_until, id),
  KEY idx_payment_status (payment_id, status, id),
  KEY idx_after_sale (after_sale_id, id),
  CONSTRAINT chk_refund_amount CHECK (amount > 0 AND total_amount > 0 AND amount <= total_amount)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE refund_callbacks (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  provider_event_id VARCHAR(128) NOT NULL,
  refund_id BIGINT UNSIGNED NULL,
  payload_hash CHAR(64) NOT NULL,
  signature_valid TINYINT(1) NOT NULL DEFAULT 0,
  process_status VARCHAR(32) NOT NULL,
  error_code VARCHAR(64) NULL,
  received_at DATETIME(3) NOT NULL,
  processed_at DATETIME(3) NULL,
  request_id VARCHAR(64) NULL,
  UNIQUE KEY uk_provider_event (provider, provider_event_id),
  KEY idx_refund_received (refund_id, received_at),
  KEY idx_process_received (process_status, received_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE refund_items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  refund_id BIGINT UNSIGNED NOT NULL,
  after_sale_item_id BIGINT UNSIGNED NOT NULL,
  amount BIGINT NOT NULL,
  quantity INT NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_refund_after_sale_item (refund_id, after_sale_item_id),
  KEY idx_after_sale_item (after_sale_item_id, refund_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE return_receipts (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  receipt_no VARCHAR(64) NOT NULL,
  after_sale_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  disposition VARCHAR(32) NOT NULL,
  sealed_package_intact TINYINT(1) NOT NULL,
  goods_intact TINYINT(1) NOT NULL,
  remark VARCHAR(1000) NULL,
  received_by BIGINT UNSIGNED NOT NULL,
  received_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_receipt_no (receipt_no),
  UNIQUE KEY uk_after_sale_receipt (after_sale_id),
  KEY idx_after_sale (after_sale_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE replacement_fulfillments (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  replacement_no VARCHAR(64) NOT NULL,
  after_sale_id BIGINT UNSIGNED NOT NULL,
  original_order_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'created',
  address_snapshot JSON NOT NULL,
  items_json JSON NOT NULL,
  delivery_order_id BIGINT UNSIGNED NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_replacement_no (replacement_no),
  KEY idx_after_sale (after_sale_id, id),
  KEY idx_shop_status (shop_id, status, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE compensation_ledger (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  compensation_no VARCHAR(64) NOT NULL,
  after_sale_id BIGINT UNSIGNED NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  type VARCHAR(32) NOT NULL,
  amount BIGINT NOT NULL,
  asset_type VARCHAR(32) NOT NULL DEFAULT 'manual_credit_pending',
  status VARCHAR(32) NOT NULL DEFAULT 'approved',
  reason VARCHAR(1000) NULL,
  issued_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_compensation_no (compensation_no),
  KEY idx_customer_status (customer_id, status, id),
  KEY idx_after_sale (after_sale_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE compensation_ledger;
DROP TABLE replacement_fulfillments;
DROP TABLE return_receipts;
DROP TABLE refund_items;
DROP TABLE refund_callbacks;
DROP TABLE refunds;
DROP TABLE after_sale_history;
DROP TABLE after_sale_evidence;
DROP TABLE after_sale_items;
DROP TABLE after_sales;
ALTER TABLE orders
  DROP INDEX idx_after_sale_status,
  DROP COLUMN after_sale_status,
  DROP COLUMN refunded_amount;
ALTER TABLE stock_records DROP INDEX uk_stock_source_idempotency;
ALTER TABLE products
  DROP COLUMN sealed_package_required,
  DROP COLUMN return_policy_version,
  DROP COLUMN return_policy_code,
  DROP COLUMN return_eligible;
