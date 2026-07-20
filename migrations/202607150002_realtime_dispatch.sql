-- +goose Up
-- Durable rider realtime delivery log. RabbitMQ remains the source event bus;
-- Redis only wakes API instances and is never the replay source of truth.

CREATE TABLE IF NOT EXISTS realtime_deliveries (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  source_event_id CHAR(36) NOT NULL,
  source_event_type VARCHAR(64) NOT NULL,
  client_event_type VARCHAR(64) NOT NULL,
  recipient_type VARCHAR(16) NOT NULL DEFAULT 'rider',
  recipient_id BIGINT UNSIGNED NOT NULL,
  aggregate_type VARCHAR(32) NOT NULL,
  aggregate_id BIGINT UNSIGNED NOT NULL,
  payload_snapshot JSON NOT NULL,
  sound_key VARCHAR(64) NULL,
  occurred_at DATETIME(3) NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  relay_status VARCHAR(16) NOT NULL DEFAULT 'pending',
  relay_attempts SMALLINT UNSIGNED NOT NULL DEFAULT 0,
  next_relay_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  relayed_at DATETIME(3) NULL,
  last_error_code VARCHAR(64) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_realtime_delivery_source_recipient (
    source_event_id, recipient_type, recipient_id, client_event_type
  ),
  KEY idx_realtime_delivery_resume (recipient_type, recipient_id, id),
  KEY idx_realtime_delivery_relay (relay_status, next_relay_at, id),
  KEY idx_realtime_delivery_expiry (expires_at, id),
  KEY idx_realtime_delivery_aggregate (aggregate_type, aggregate_id, recipient_id),
  CONSTRAINT chk_realtime_delivery_recipient CHECK (recipient_type IN ('rider')),
  CONSTRAINT chk_realtime_delivery_positive_ids CHECK (recipient_id > 0 AND aggregate_id > 0),
  CONSTRAINT chk_realtime_delivery_relay CHECK (relay_status IN ('pending','relayed','expired','dead')),
  CONSTRAINT chk_realtime_delivery_expiry_order CHECK (expires_at > occurred_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS realtime_acknowledgements (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  realtime_delivery_id BIGINT UNSIGNED NOT NULL,
  rider_id BIGINT UNSIGNED NOT NULL,
  device_hash CHAR(64) NOT NULL,
  ack_type VARCHAR(32) NOT NULL,
  client_occurred_at DATETIME(3) NULL,
  received_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  error_code VARCHAR(64) NULL,
  client_version VARCHAR(32) NOT NULL,
  platform VARCHAR(16) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_realtime_ack_delivery_device_type (realtime_delivery_id, device_hash, ack_type),
  KEY idx_realtime_ack_rider (rider_id, received_at, id),
  KEY idx_realtime_ack_type_received (ack_type, received_at, id),
  CONSTRAINT chk_realtime_ack_type CHECK (
    ack_type IN ('displayed','sound_played','sound_disabled','sound_error','closed')
  ),
  CONSTRAINT chk_realtime_ack_positive_ids CHECK (realtime_delivery_id > 0 AND rider_id > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
-- Non-destructive rollback: disabling the feature flags reverts traffic. The
-- durable delivery and acknowledgement audit data is intentionally retained.
SELECT 1;
