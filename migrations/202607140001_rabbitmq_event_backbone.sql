-- +goose Up
ALTER TABLE outbox_events
  ADD COLUMN event_version INT UNSIGNED NOT NULL DEFAULT 1 AFTER event_type,
  ADD COLUMN spec_version VARCHAR(16) NOT NULL DEFAULT '1.0' AFTER event_version,
  ADD COLUMN producer VARCHAR(64) NULL AFTER aggregate_id,
  ADD COLUMN schema_ref VARCHAR(128) NULL AFTER producer,
  ADD COLUMN partition_key VARCHAR(160) NULL AFTER schema_ref,
  ADD COLUMN replay_of_event_id VARCHAR(128) NULL AFTER partition_key,
  ADD COLUMN exchange_name VARCHAR(128) NULL AFTER published_at,
  ADD COLUMN routing_key VARCHAR(128) NULL AFTER exchange_name,
  ADD COLUMN dispatched_at DATETIME(3) NULL AFTER routing_key,
  ADD KEY idx_outbox_event_contract (event_type, event_version, created_at);

CREATE TABLE mq_consumer_receipts (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  consumer_name VARCHAR(64) NOT NULL,
  event_id VARCHAR(128) NOT NULL,
  event_type VARCHAR(128) NOT NULL,
  event_version INT UNSIGNED NOT NULL DEFAULT 1,
  status VARCHAR(16) NOT NULL DEFAULT 'processing',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  locked_by VARCHAR(128) NULL,
  locked_until DATETIME(3) NULL,
  last_error_code VARCHAR(64) NULL,
  result_ref_type VARCHAR(64) NULL,
  result_ref_id BIGINT UNSIGNED NULL,
  first_received_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  processed_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_mq_receipt_consumer_event (consumer_name, event_id),
  KEY idx_mq_receipt_status (consumer_name, status, updated_at),
  KEY idx_mq_receipt_event (event_type, processed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE mq_dead_letters (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  dead_no VARCHAR(32) NOT NULL,
  consumer_name VARCHAR(64) NOT NULL,
  event_id VARCHAR(128) NOT NULL,
  event_type VARCHAR(128) NOT NULL,
  event_version INT UNSIGNED NOT NULL DEFAULT 1,
  aggregate_type VARCHAR(64) NULL,
  aggregate_id VARCHAR(128) NULL,
  error_code VARCHAR(64) NOT NULL,
  error_safe VARCHAR(512) NULL,
  payload_hash CHAR(64) NOT NULL,
  retry_count INT UNSIGNED NOT NULL DEFAULT 0,
  status VARCHAR(16) NOT NULL DEFAULT 'open',
  version INT UNSIGNED NOT NULL DEFAULT 1,
  first_failed_at DATETIME(3) NOT NULL,
  dead_at DATETIME(3) NOT NULL,
  last_replay_id BIGINT UNSIGNED NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_mq_dead_no (dead_no),
  UNIQUE KEY uk_mq_dead_open_identity (consumer_name, event_id, error_code, status),
  KEY idx_mq_dead_status (status, dead_at, id),
  KEY idx_mq_dead_consumer_event (consumer_name, event_type, status),
  KEY idx_mq_dead_event_id (event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE mq_dead_letter_replays (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  dead_letter_id BIGINT UNSIGNED NOT NULL,
  replay_event_id VARCHAR(128) NOT NULL,
  actor_admin_id BIGINT UNSIGNED NOT NULL,
  reason_code VARCHAR(64) NOT NULL,
  reason VARCHAR(512) NOT NULL,
  idempotency_key_hash CHAR(64) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'requested',
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_mq_replay_event (replay_event_id),
  UNIQUE KEY uk_mq_replay_idempotency (actor_admin_id, idempotency_key_hash),
  KEY idx_mq_replay_dead (dead_letter_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
-- 回执、死信和重放记录都是审计与业务恢复事实。
-- 此增量迁移刻意不可逆：应用回滚会关闭 MQ 并恢复数据库降级路径，
-- 同时保留所有新增数据。
SIGNAL SQLSTATE '45000'
  SET MESSAGE_TEXT = '202607140001 is additive and irreversible; roll back the application, not MQ facts';
