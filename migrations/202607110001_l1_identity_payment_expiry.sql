-- +goose Up
ALTER TABLE orders
  ADD COLUMN expires_at DATETIME(3) NULL AFTER idempotency_key,
  ADD COLUMN cancel_source VARCHAR(32) NULL AFTER expires_at,
  ADD COLUMN cancel_reason_code VARCHAR(64) NULL AFTER cancel_source,
  ADD COLUMN version INT NOT NULL DEFAULT 0 AFTER cancel_reason_code,
  ADD KEY idx_status_expires (status, expires_at, id);

ALTER TABLE payments
  ADD COLUMN provider VARCHAR(32) NULL AFTER channel,
  ADD COLUMN provider_trade_no VARCHAR(128) NULL AFTER provider,
  ADD COLUMN provider_status VARCHAR(64) NULL AFTER provider_trade_no,
  ADD COLUMN provider_prepay_id VARCHAR(128) NULL AFTER provider_status,
  ADD COLUMN currency CHAR(3) NOT NULL DEFAULT 'CNY' AFTER amount,
  ADD COLUMN client_payload JSON NULL AFTER currency,
  ADD COLUMN expires_at DATETIME(3) NULL AFTER client_payload,
  ADD COLUMN failed_at DATETIME(3) NULL AFTER paid_at,
  ADD COLUMN failure_code VARCHAR(64) NULL AFTER failed_at,
  ADD COLUMN refunded_amount BIGINT NOT NULL DEFAULT 0 AFTER failure_code,
  ADD COLUMN version INT NOT NULL DEFAULT 0 AFTER refunded_amount;

UPDATE payments SET provider = channel WHERE provider IS NULL;

ALTER TABLE payments
  MODIFY COLUMN provider VARCHAR(32) NOT NULL,
  ADD UNIQUE KEY uk_provider_trade (provider, provider_trade_no),
  ADD KEY idx_provider_status (provider, status, created_at);

CREATE TABLE customer_identities (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  provider VARCHAR(32) NOT NULL,
  app_id VARCHAR(64) NOT NULL,
  provider_subject VARCHAR(128) NOT NULL,
  union_subject VARCHAR(128) NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  last_login_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  UNIQUE KEY uk_identity (provider, app_id, provider_subject),
  KEY idx_customer_provider (customer_id, provider)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE payment_callbacks (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  provider_event_id VARCHAR(128) NOT NULL,
  provider_trade_no VARCHAR(128) NULL,
  payment_id BIGINT UNSIGNED NULL,
  payload_hash CHAR(64) NOT NULL,
  signature_valid TINYINT(1) NOT NULL DEFAULT 0,
  process_status VARCHAR(32) NOT NULL,
  error_code VARCHAR(64) NULL,
  received_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  processed_at DATETIME(3) NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_provider_event (provider, provider_event_id),
  KEY idx_payment_received (payment_id, received_at),
  KEY idx_process_received (process_status, received_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE payment_callbacks;
DROP TABLE customer_identities;

ALTER TABLE payments
  DROP INDEX idx_provider_status,
  DROP INDEX uk_provider_trade,
  DROP COLUMN version,
  DROP COLUMN refunded_amount,
  DROP COLUMN failure_code,
  DROP COLUMN failed_at,
  DROP COLUMN expires_at,
  DROP COLUMN client_payload,
  DROP COLUMN currency,
  DROP COLUMN provider_prepay_id,
  DROP COLUMN provider_status,
  DROP COLUMN provider_trade_no,
  DROP COLUMN provider;

ALTER TABLE orders
  DROP INDEX idx_status_expires,
  DROP COLUMN version,
  DROP COLUMN cancel_reason_code,
  DROP COLUMN cancel_source,
  DROP COLUMN expires_at;
