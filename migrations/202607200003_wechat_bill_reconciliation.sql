-- +goose Up
CREATE TABLE wechat_bill_reconciliation_runs (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  bill_date DATE NOT NULL,
  bill_type VARCHAR(32) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'pending',
  started_at DATETIME(3) NULL,
  completed_at DATETIME(3) NULL,
  hash_type VARCHAR(16) NULL,
  expected_hash VARCHAR(128) NULL,
  computed_hash VARCHAR(128) NULL,
  provider_request_id VARCHAR(128) NULL,
  download_request_id VARCHAR(128) NULL,
  row_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
  discrepancy_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
  stats_json JSON NULL,
  error_code VARCHAR(64) NULL,
  error_detail VARCHAR(512) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wechat_bill_date_type (bill_date, bill_type),
  KEY idx_wechat_bill_status (status, bill_date, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Observations are deliberately narrow staging rows. They let the worker parse
-- and compare large bills without retaining the complete file in memory.
CREATE TABLE wechat_bill_observations (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
  run_id BIGINT UNSIGNED NOT NULL,
  line_no BIGINT UNSIGNED NOT NULL,
  entry_kind VARCHAR(16) NOT NULL,
  business_no VARCHAR(128) NULL,
  provider_trade_no VARCHAR(128) NULL,
  provider_refund_no VARCHAR(128) NULL,
  provider_status VARCHAR(64) NULL,
  currency CHAR(3) NULL,
  amount BIGINT NULL,
  occurred_at DATETIME(3) NULL,
  raw_hash CHAR(64) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wechat_bill_observation_line (run_id, line_no),
  KEY idx_wechat_bill_observation_business (run_id, entry_kind, business_no),
  KEY idx_wechat_bill_observation_trade (run_id, provider_trade_no),
  KEY idx_wechat_bill_observation_refund (run_id, provider_refund_no)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wechat_bill_discrepancies (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  run_id BIGINT UNSIGNED NOT NULL,
  bill_date DATE NOT NULL,
  bill_type VARCHAR(32) NOT NULL,
  discrepancy_type VARCHAR(64) NOT NULL,
  business_no VARCHAR(128) NULL,
  provider_trade_no VARCHAR(128) NULL,
  provider_refund_no VARCHAR(128) NULL,
  local_value JSON NULL,
  wechat_value JSON NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'open',
  handling_note VARCHAR(1000) NULL,
  handled_by BIGINT UNSIGNED NULL,
  handled_at DATETIME(3) NULL,
  dedupe_key CHAR(64) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wechat_bill_discrepancy (run_id, dedupe_key),
  KEY idx_wechat_bill_discrepancy_open (status, bill_date, id),
  KEY idx_wechat_bill_discrepancy_business (business_no, id),
  KEY idx_wechat_bill_discrepancy_trade (provider_trade_no, id),
  KEY idx_wechat_bill_discrepancy_refund (provider_refund_no, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE IF EXISTS wechat_bill_discrepancies;
DROP TABLE IF EXISTS wechat_bill_observations;
DROP TABLE IF EXISTS wechat_bill_reconciliation_runs;
