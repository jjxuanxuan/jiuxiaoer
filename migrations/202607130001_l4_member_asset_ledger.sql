-- +goose Up
CREATE TABLE member_tier_rule_sets (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  version VARCHAR(32) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'draft',
  effective_at DATETIME(3) NOT NULL,
  reason VARCHAR(500) NOT NULL,
  created_by BIGINT UNSIGNED NOT NULL,
  activated_by BIGINT UNSIGNED NULL,
  activated_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_member_rule_version (version),
  KEY idx_member_rule_effective (status, effective_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE member_tier_rules (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  rule_set_id BIGINT UNSIGNED NOT NULL,
  tier_code VARCHAR(32) NOT NULL,
  tier_name VARCHAR(64) NOT NULL,
  min_growth BIGINT NOT NULL,
  sort_order INT NOT NULL,
  benefits_snapshot JSON NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_member_rule_tier (rule_set_id, tier_code),
  UNIQUE KEY uk_member_rule_sort (rule_set_id, sort_order),
  CONSTRAINT chk_member_min_growth CHECK (min_growth >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

INSERT INTO member_tier_rule_sets (id, version, status, effective_at, reason, created_by, activated_by, activated_at)
VALUES (9100, 'candidate-20260713', 'active', '1970-01-01 00:00:00.000', 'L4 default candidate thresholds', 0, 0, CURRENT_TIMESTAMP(3));
INSERT INTO member_tier_rules (id, rule_set_id, tier_code, tier_name, min_growth, sort_order, benefits_snapshot) VALUES
  (9101, 9100, 'normal', '普通会员', 0, 1, JSON_OBJECT()),
  (9102, 9100, 'silver', '银卡会员', 1000, 2, JSON_OBJECT()),
  (9103, 9100, 'gold', '金卡会员', 5000, 3, JSON_OBJECT());

CREATE TABLE member_profiles (
  customer_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  current_growth BIGINT NOT NULL DEFAULT 0,
  tier_code VARCHAR(32) NOT NULL DEFAULT 'normal',
  rule_set_id BIGINT UNSIGNED NOT NULL,
  tier_effective_at DATETIME(3) NOT NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_member_tier_growth (tier_code, current_growth, customer_id),
  CONSTRAINT chk_member_growth CHECK (current_growth >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE member_tier_histories (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  event_id VARCHAR(128) NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  from_tier VARCHAR(32) NOT NULL,
  to_tier VARCHAR(32) NOT NULL,
  growth_value BIGINT NOT NULL,
  rule_set_id BIGINT UNSIGNED NOT NULL,
  asset_transaction_id BIGINT UNSIGNED NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_member_tier_event (event_id),
  KEY idx_member_history (customer_id, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_accounts (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  account_no VARCHAR(64) NOT NULL,
  owner_type VARCHAR(32) NOT NULL,
  owner_id BIGINT UNSIGNED NOT NULL,
  asset_type VARCHAR(32) NOT NULL,
  unit VARCHAR(32) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  allow_negative TINYINT(1) NOT NULL DEFAULT 0,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_account_no (account_no),
  UNIQUE KEY uk_asset_owner_type_unit (owner_type, owner_id, asset_type, unit),
  KEY idx_asset_owner (owner_type, owner_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_balances (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  account_id BIGINT UNSIGNED NOT NULL,
  bucket VARCHAR(32) NOT NULL,
  amount BIGINT NOT NULL DEFAULT 0,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_balance_bucket (account_id, bucket)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_transactions (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  transaction_no VARCHAR(64) NOT NULL,
  asset_type VARCHAR(32) NOT NULL,
  unit VARCHAR(32) NOT NULL,
  action VARCHAR(32) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'posted',
  source_type VARCHAR(64) NOT NULL,
  source_id VARCHAR(128) NOT NULL,
  idempotency_key_hash CHAR(64) NOT NULL,
  request_hash CHAR(64) NOT NULL,
  reversal_of_transaction_id BIGINT UNSIGNED NULL,
  amount BIGINT NOT NULL,
  actor_type VARCHAR(32) NOT NULL,
  actor_id BIGINT UNSIGNED NOT NULL,
  metadata JSON NULL,
  occurred_at DATETIME(3) NOT NULL,
  posted_at DATETIME(3) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_transaction_no (transaction_no),
  UNIQUE KEY uk_asset_source_action (source_type, source_id, action),
  UNIQUE KEY uk_asset_idempotency (actor_type, actor_id, idempotency_key_hash),
  KEY idx_asset_tx_created (created_at, id),
  KEY idx_asset_tx_reversal (reversal_of_transaction_id, id),
  CONSTRAINT chk_asset_tx_amount CHECK (amount > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_entries (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  transaction_id BIGINT UNSIGNED NOT NULL,
  entry_seq INT UNSIGNED NOT NULL,
  account_id BIGINT UNSIGNED NOT NULL,
  bucket VARCHAR(32) NOT NULL,
  delta BIGINT NOT NULL,
  balance_after BIGINT NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_entry_seq (transaction_id, entry_seq),
  KEY idx_asset_entry_account (account_id, created_at, id),
  KEY idx_asset_entry_tx (transaction_id, id),
  CONSTRAINT chk_asset_entry_delta CHECK (delta <> 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_lots (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  account_id BIGINT UNSIGNED NOT NULL,
  grant_transaction_id BIGINT UNSIGNED NOT NULL,
  granted_amount BIGINT NOT NULL,
  available_amount BIGINT NOT NULL,
  frozen_amount BIGINT NOT NULL DEFAULT 0,
  consumed_amount BIGINT NOT NULL DEFAULT 0,
  expired_amount BIGINT NOT NULL DEFAULT 0,
  expires_at DATETIME(3) NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_lot_grant (grant_transaction_id, account_id),
  KEY idx_asset_lot_expiry (account_id, status, expires_at, id),
  CONSTRAINT chk_asset_lot_amounts CHECK (granted_amount > 0 AND available_amount >= 0 AND frozen_amount >= 0 AND consumed_amount >= 0 AND expired_amount >= 0 AND granted_amount = available_amount + frozen_amount + consumed_amount + expired_amount)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_holds (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  hold_no VARCHAR(64) NOT NULL,
  reservation_key VARCHAR(128) NOT NULL,
  account_id BIGINT UNSIGNED NOT NULL,
  asset_type VARCHAR(32) NOT NULL,
  unit VARCHAR(32) NOT NULL,
  original_amount BIGINT NOT NULL,
  committed_amount BIGINT NOT NULL DEFAULT 0,
  released_amount BIGINT NOT NULL DEFAULT 0,
  status VARCHAR(32) NOT NULL DEFAULT 'active',
  source_type VARCHAR(64) NOT NULL,
  source_id VARCHAR(128) NOT NULL,
  expires_at DATETIME(3) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_hold_no (hold_no),
  UNIQUE KEY uk_asset_reservation (reservation_key),
  UNIQUE KEY uk_asset_hold_source (source_type, source_id),
  KEY idx_asset_hold_expiry (status, expires_at, id),
  KEY idx_asset_hold_account (account_id, status, id),
  CONSTRAINT chk_asset_hold_amounts CHECK (original_amount > 0 AND committed_amount >= 0 AND released_amount >= 0 AND committed_amount + released_amount <= original_amount)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_hold_lots (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  hold_id BIGINT UNSIGNED NOT NULL,
  lot_id BIGINT UNSIGNED NOT NULL,
  amount BIGINT NOT NULL,
  committed_amount BIGINT NOT NULL DEFAULT 0,
  released_amount BIGINT NOT NULL DEFAULT 0,
  UNIQUE KEY uk_asset_hold_lot (hold_id, lot_id),
  KEY idx_asset_hold_lot_lot (lot_id, hold_id),
  CONSTRAINT chk_asset_hold_lot_amounts CHECK (amount > 0 AND committed_amount >= 0 AND released_amount >= 0 AND committed_amount + released_amount <= amount)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_adjustments (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  adjustment_no VARCHAR(64) NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  asset_type VARCHAR(32) NOT NULL,
  unit VARCHAR(32) NOT NULL,
  direction VARCHAR(16) NOT NULL,
  amount BIGINT NOT NULL,
  reason_code VARCHAR(64) NOT NULL,
  reason VARCHAR(500) NOT NULL,
  evidence_refs JSON NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'pending',
  created_by BIGINT UNSIGNED NOT NULL,
  reviewed_by BIGINT UNSIGNED NULL,
  reviewed_at DATETIME(3) NULL,
  asset_transaction_id BIGINT UNSIGNED NULL,
  failure_code VARCHAR(64) NULL,
  idempotency_key_hash CHAR(64) NOT NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_adjustment_no (adjustment_no),
  UNIQUE KEY uk_asset_adjustment_idempotency (created_by, idempotency_key_hash),
  UNIQUE KEY uk_asset_adjustment_transaction (asset_transaction_id),
  KEY idx_asset_adjustment_status (status, created_at, id),
  CONSTRAINT chk_asset_adjustment_amount CHECK (amount > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_reconciliation_jobs (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  job_no VARCHAR(64) NOT NULL,
  scope VARCHAR(32) NOT NULL,
  scope_id VARCHAR(128) NULL,
  mode VARCHAR(32) NOT NULL DEFAULT 'dry_run',
  status VARCHAR(32) NOT NULL DEFAULT 'pending',
  requested_by BIGINT UNSIGNED NOT NULL,
  idempotency_key_hash CHAR(64) NOT NULL,
  scanned_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
  difference_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
  critical_count BIGINT UNSIGNED NOT NULL DEFAULT 0,
  started_at DATETIME(3) NULL,
  finished_at DATETIME(3) NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_reconcile_job_no (job_no),
  UNIQUE KEY uk_asset_reconcile_idempotency (requested_by, idempotency_key_hash),
  KEY idx_asset_reconcile_status (status, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE asset_reconciliation_items (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  job_id BIGINT UNSIGNED NOT NULL,
  object_type VARCHAR(32) NOT NULL,
  object_id VARCHAR(128) NOT NULL,
  diff_type VARCHAR(64) NOT NULL,
  expected_amount BIGINT NULL,
  actual_amount BIGINT NULL,
  severity VARCHAR(16) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'open',
  detail JSON NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_asset_reconcile_diff (job_id, object_type, object_id, diff_type),
  KEY idx_asset_reconcile_item_status (status, severity, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE compensation_ledger
  ADD COLUMN asset_transaction_id BIGINT UNSIGNED NULL AFTER issued_at,
  ADD COLUMN failure_code VARCHAR(64) NULL AFTER asset_transaction_id,
  ADD COLUMN attempts INT UNSIGNED NOT NULL DEFAULT 0 AFTER failure_code,
  ADD COLUMN next_retry_at DATETIME(3) NULL AFTER attempts,
  ADD COLUMN locked_until DATETIME(3) NULL AFTER next_retry_at,
  ADD COLUMN locked_by VARCHAR(128) NULL AFTER locked_until,
  ADD UNIQUE KEY uk_compensation_asset_transaction (asset_transaction_id),
  ADD KEY idx_compensation_claim (status, next_retry_at, locked_until, id);

UPDATE compensation_ledger SET asset_type = 'balance' WHERE asset_type = 'manual_credit_pending';

-- +goose Down
UPDATE compensation_ledger SET asset_type = 'manual_credit_pending' WHERE asset_type = 'balance';
ALTER TABLE compensation_ledger
  DROP INDEX idx_compensation_claim,
  DROP INDEX uk_compensation_asset_transaction,
  DROP COLUMN locked_by,
  DROP COLUMN locked_until,
  DROP COLUMN next_retry_at,
  DROP COLUMN attempts,
  DROP COLUMN failure_code,
  DROP COLUMN asset_transaction_id;
DROP TABLE asset_reconciliation_items;
DROP TABLE asset_reconciliation_jobs;
DROP TABLE asset_adjustments;
DROP TABLE asset_hold_lots;
DROP TABLE asset_holds;
DROP TABLE asset_lots;
DROP TABLE asset_entries;
DROP TABLE asset_transactions;
DROP TABLE asset_balances;
DROP TABLE asset_accounts;
DROP TABLE member_tier_histories;
DROP TABLE member_profiles;
DROP TABLE member_tier_rules;
DROP TABLE member_tier_rule_sets;
