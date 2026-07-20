-- +goose Up
ALTER TABLE identity_verification_requests
  ADD COLUMN purpose VARCHAR(64) NOT NULL DEFAULT 'alcohol_purchase',
  ADD COLUMN state_hash CHAR(64) NULL,
  ADD COLUMN session_expires_at DATETIME(3) NULL,
  ADD COLUMN verification_level VARCHAR(32) NOT NULL DEFAULT '',
  ADD COLUMN policy_version VARCHAR(32) NOT NULL DEFAULT 'cp1-v2',
  ADD COLUMN consent_version VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN result_hash CHAR(64) NULL,
  ADD COLUMN callback_event_id VARCHAR(128) NULL,
  ADD COLUMN callback_received_at DATETIME(3) NULL,
  ADD COLUMN revoked_at DATETIME(3) NULL,
  ADD COLUMN revoked_reason VARCHAR(255) NULL,
  ADD KEY idx_identity_pending_session (status, session_expires_at, id),
  ADD KEY idx_identity_customer_current (customer_id, status, id);

ALTER TABLE customer_realname_verifications
  ADD COLUMN verification_level VARCHAR(32) NOT NULL DEFAULT '',
  ADD COLUMN policy_version VARCHAR(32) NOT NULL DEFAULT 'cp1-v2',
  ADD COLUMN result_hash CHAR(64) NULL,
  ADD COLUMN revoked_at DATETIME(3) NULL,
  ADD COLUMN revoked_reason VARCHAR(255) NULL;

CREATE TABLE identity_verification_callbacks (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  provider_event_id VARCHAR(128) NOT NULL,
  provider_request_id VARCHAR(128) NOT NULL,
  payload_hash CHAR(64) NOT NULL,
  signature_valid TINYINT(1) NOT NULL DEFAULT 0,
  process_status VARCHAR(16) NOT NULL,
  error_code VARCHAR(64) NULL,
  received_at DATETIME(3) NOT NULL,
  processed_at DATETIME(3) NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_identity_callback_event (provider, provider_event_id),
  KEY idx_identity_callback_request (provider, provider_request_id, received_at),
  KEY idx_identity_callback_status (process_status, received_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Existing fake/synchronous verification records are development evidence and
-- must not be promoted to production. A production cutover requires users to
-- create a provider-hosted session and receive a new cp1-v2 result.

-- +goose Down
-- Identity callback rows and result hashes are security/audit evidence. Keep
-- the additive schema and roll back the application flow instead of deleting
-- verification facts.
SIGNAL SQLSTATE '45000'
  SET MESSAGE_TEXT = '202607140002 is additive and irreversible; roll back the application, not identity verification evidence';
