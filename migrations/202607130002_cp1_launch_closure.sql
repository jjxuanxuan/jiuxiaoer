-- +goose Up
ALTER TABLE products
  ADD COLUMN age_restricted TINYINT(1) NOT NULL DEFAULT 1 AFTER sealed_package_required;

ALTER TABLE categories
  ADD COLUMN age_restricted TINYINT(1) NOT NULL DEFAULT 0 AFTER status;

ALTER TABLE orders
  ADD COLUMN compliance_snapshot JSON NULL AFTER delivery_promise_snapshot;

ALTER TABLE delivery_orders
  ADD COLUMN assignment_version INT UNSIGNED NOT NULL DEFAULT 1 AFTER status,
  ADD COLUMN picked_up_verified_at DATETIME(3) NULL AFTER picked_up_at,
  ADD COLUMN completed_verified_at DATETIME(3) NULL AFTER completed_at;

ALTER TABLE accounts
  ADD COLUMN token_invalid_before DATETIME(3) NULL AFTER last_login_at,
  ADD COLUMN credential_version INT UNSIGNED NOT NULL DEFAULT 1 AFTER token_invalid_before;

ALTER TABLE riders
  ADD COLUMN review_status VARCHAR(32) NOT NULL DEFAULT 'pending' AFTER work_status,
  ADD COLUMN service_scope JSON NULL AFTER review_status;

CREATE TABLE credential_reset_tokens (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  account_id BIGINT UNSIGNED NOT NULL,
  token_hash CHAR(64) NOT NULL,
  token_ciphertext VARBINARY(512) NOT NULL,
  purpose VARCHAR(32) NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  used_at DATETIME(3) NULL,
  created_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_credential_reset_hash (token_hash),
  KEY idx_credential_reset_account (account_id, purpose, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE print_settings (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  shop_id BIGINT UNSIGNED NOT NULL,
  provider VARCHAR(32) NOT NULL,
  device_id_ciphertext VARBINARY(512) NOT NULL,
  device_id_mask VARCHAR(128) NOT NULL,
  template_id BIGINT UNSIGNED NOT NULL,
  copies TINYINT UNSIGNED NOT NULL DEFAULT 1,
  auto_print_events JSON NOT NULL,
  enabled TINYINT(1) NOT NULL DEFAULT 0,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_by BIGINT UNSIGNED NOT NULL,
  updated_by BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_print_setting_shop (shop_id),
  KEY idx_print_setting_provider_enabled (provider, enabled)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE print_tasks (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  task_no VARCHAR(64) NOT NULL,
  event_id VARCHAR(128) NOT NULL,
  order_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  event_type VARCHAR(64) NOT NULL,
  template_id BIGINT UNSIGNED NOT NULL,
  template_version VARCHAR(32) NOT NULL,
  render_payload JSON NOT NULL,
  reprint_seq INT UNSIGNED NOT NULL DEFAULT 0,
  provider VARCHAR(32) NOT NULL,
  provider_request_id VARCHAR(128) NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_retry_at DATETIME(3) NULL,
  locked_until DATETIME(3) NULL,
  locked_by VARCHAR(128) NULL,
  last_error_code VARCHAR(64) NULL,
  last_error_safe VARCHAR(255) NULL,
  succeeded_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_print_task_no (task_no),
  UNIQUE KEY uk_print_task_event (shop_id, order_id, event_type, template_version, reprint_seq),
  KEY idx_print_task_claim (status, next_retry_at, locked_until, id),
  KEY idx_print_task_order (order_id, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE print_attempts (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  print_task_id BIGINT UNSIGNED NOT NULL,
  attempt_no INT UNSIGNED NOT NULL,
  operation VARCHAR(16) NOT NULL,
  provider_request_id VARCHAR(128) NULL,
  request_hash CHAR(64) NOT NULL,
  result VARCHAR(32) NOT NULL,
  provider_status VARCHAR(32) NULL,
  error_code VARCHAR(64) NULL,
  duration_ms INT UNSIGNED NOT NULL DEFAULT 0,
  started_at DATETIME(3) NOT NULL,
  finished_at DATETIME(3) NOT NULL,
  request_id VARCHAR(128) NULL,
  UNIQUE KEY uk_print_attempt (print_task_id, attempt_no, operation),
  KEY idx_print_attempt_provider (provider_request_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE notification_templates (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  template_code VARCHAR(64) NOT NULL,
  event_type VARCHAR(64) NOT NULL,
  channel VARCHAR(32) NOT NULL,
  provider_template_id VARCHAR(128) NULL,
  version VARCHAR(32) NOT NULL,
  title_template VARCHAR(255) NOT NULL,
  body_template TEXT NOT NULL,
  allowed_fields JSON NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'draft',
  created_by BIGINT UNSIGNED NOT NULL,
  published_by BIGINT UNSIGNED NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_notification_template_version (template_code, version),
  KEY idx_notification_template_lookup (event_type, channel, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE notification_deliveries (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  delivery_no VARCHAR(64) NOT NULL,
  event_id VARCHAR(128) NOT NULL,
  event_type VARCHAR(64) NOT NULL,
  recipient_type VARCHAR(32) NOT NULL,
  recipient_id BIGINT UNSIGNED NOT NULL,
  channel VARCHAR(32) NOT NULL,
  template_id BIGINT UNSIGNED NOT NULL,
  template_version VARCHAR(32) NOT NULL,
  target_ciphertext VARBINARY(512) NULL,
  target_mask VARCHAR(128) NULL,
  payload_snapshot JSON NOT NULL,
  provider_request_id VARCHAR(128) NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  next_retry_at DATETIME(3) NULL,
  locked_until DATETIME(3) NULL,
  locked_by VARCHAR(128) NULL,
  last_error_code VARCHAR(64) NULL,
  sent_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_notification_delivery_no (delivery_no),
  UNIQUE KEY uk_notification_delivery_event (event_id, recipient_type, recipient_id, channel, template_id),
  KEY idx_notification_delivery_claim (status, next_retry_at, locked_until, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE message_inboxes (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  source_event_id VARCHAR(128) NOT NULL,
  type VARCHAR(64) NOT NULL,
  title VARCHAR(255) NOT NULL,
  summary VARCHAR(512) NOT NULL,
  target_type VARCHAR(64) NULL,
  target_id BIGINT UNSIGNED NULL,
  read_at DATETIME(3) NULL,
  archived_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_message_source (customer_id, source_event_id, type),
  KEY idx_message_customer_read (customer_id, read_at, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE delivery_verifications (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  delivery_order_id BIGINT UNSIGNED NOT NULL,
  stage VARCHAR(16) NOT NULL,
  code_hash CHAR(64) NOT NULL,
  code_ciphertext VARBINARY(512) NOT NULL,
  code_mask VARCHAR(16) NOT NULL,
  policy_version VARCHAR(32) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  failed_attempts INT UNSIGNED NOT NULL DEFAULT 0,
  max_attempts INT UNSIGNED NOT NULL DEFAULT 5,
  expires_at DATETIME(3) NOT NULL,
  locked_until DATETIME(3) NULL,
  verified_at DATETIME(3) NULL,
  verified_by_type VARCHAR(32) NULL,
  verified_by_id BIGINT UNSIGNED NULL,
  override_reason_code VARCHAR(64) NULL,
  override_reason VARCHAR(255) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_delivery_verification_stage (delivery_order_id, stage),
  KEY idx_delivery_verification_status (status, expires_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE delivery_verification_attempts (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  verification_id BIGINT UNSIGNED NOT NULL,
  delivery_order_id BIGINT UNSIGNED NOT NULL,
  stage VARCHAR(16) NOT NULL,
  actor_type VARCHAR(32) NOT NULL,
  actor_id BIGINT UNSIGNED NOT NULL,
  result VARCHAR(32) NOT NULL,
  failure_code VARCHAR(64) NULL,
  attempt_no INT UNSIGNED NOT NULL,
  request_id VARCHAR(128) NULL,
  ip_hash CHAR(64) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_verification_attempt (verification_id, created_at),
  KEY idx_verification_actor (actor_type, actor_id, created_at),
  KEY idx_verification_ip (ip_hash, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE delivery_assignments (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  delivery_order_id BIGINT UNSIGNED NOT NULL,
  from_rider_id BIGINT UNSIGNED NULL,
  to_rider_id BIGINT UNSIGNED NOT NULL,
  assignment_type VARCHAR(32) NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  reason_code VARCHAR(64) NULL,
  reason VARCHAR(255) NULL,
  actor_type VARCHAR(32) NOT NULL,
  actor_id BIGINT UNSIGNED NOT NULL,
  version_before INT UNSIGNED NOT NULL,
  version_after INT UNSIGNED NOT NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  KEY idx_assignment_delivery (delivery_order_id, created_at),
  KEY idx_assignment_active (delivery_order_id, status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE admin_override_approvals (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  action VARCHAR(64) NOT NULL,
  resource_type VARCHAR(64) NOT NULL,
  resource_id BIGINT UNSIGNED NOT NULL,
  maker_admin_id BIGINT UNSIGNED NOT NULL,
  checker_admin_id BIGINT UNSIGNED NOT NULL,
  reason_code VARCHAR(64) NOT NULL,
  reason VARCHAR(255) NOT NULL,
  expected_version INT UNSIGNED NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'pending',
  expires_at DATETIME(3) NOT NULL,
  approved_at DATETIME(3) NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_override_request (action, resource_type, resource_id, request_id),
  KEY idx_override_checker (checker_admin_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE provisioning_operations (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  operation_no VARCHAR(64) NOT NULL,
  operation_type VARCHAR(32) NOT NULL,
  idempotency_key_hash CHAR(64) NOT NULL,
  request_hash CHAR(64) NOT NULL,
  actor_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(32) NOT NULL,
  target_type VARCHAR(32) NULL,
  target_id BIGINT UNSIGNED NULL,
  step_state JSON NULL,
  failure_code VARCHAR(64) NULL,
  started_at DATETIME(3) NOT NULL,
  finished_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_provisioning_operation_no (operation_no),
  UNIQUE KEY uk_provisioning_idempotency (actor_id, operation_type, idempotency_key_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE identity_verification_requests (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  request_no VARCHAR(64) NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  provider VARCHAR(32) NOT NULL,
  provider_request_id VARCHAR(128) NULL,
  document_type VARCHAR(32) NOT NULL,
  document_hash CHAR(64) NOT NULL,
  name_hash CHAR(64) NOT NULL,
  masked_name VARCHAR(64) NOT NULL,
  masked_document_no VARCHAR(32) NOT NULL,
  status VARCHAR(32) NOT NULL,
  adult_result VARCHAR(16) NOT NULL DEFAULT 'unknown',
  birth_date DATE NULL,
  failure_code VARCHAR(64) NULL,
  attempts INT UNSIGNED NOT NULL DEFAULT 1,
  expires_at DATETIME(3) NULL,
  verified_at DATETIME(3) NULL,
  callback_payload_hash CHAR(64) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_identity_request_no (request_no),
  UNIQUE KEY uk_identity_provider_request (provider, provider_request_id),
  KEY idx_identity_customer_status (customer_id, status, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE customer_realname_verifications (
  customer_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  request_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(32) NOT NULL,
  provider VARCHAR(32) NOT NULL,
  provider_subject VARCHAR(128) NULL,
  masked_name VARCHAR(64) NOT NULL,
  masked_document_no VARCHAR(32) NOT NULL,
  adult_result VARCHAR(16) NOT NULL,
  birth_date DATE NULL,
  verified_at DATETIME(3) NULL,
  expires_at DATETIME(3) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_realname_status_expiry (status, adult_result, expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE IF EXISTS customer_realname_verifications;
DROP TABLE IF EXISTS identity_verification_requests;
DROP TABLE IF EXISTS provisioning_operations;
DROP TABLE IF EXISTS admin_override_approvals;
DROP TABLE IF EXISTS delivery_assignments;
DROP TABLE IF EXISTS delivery_verification_attempts;
DROP TABLE IF EXISTS delivery_verifications;
DROP TABLE IF EXISTS message_inboxes;
DROP TABLE IF EXISTS notification_deliveries;
DROP TABLE IF EXISTS notification_templates;
DROP TABLE IF EXISTS print_attempts;
DROP TABLE IF EXISTS print_tasks;
DROP TABLE IF EXISTS print_settings;
DROP TABLE IF EXISTS credential_reset_tokens;

ALTER TABLE riders
  DROP COLUMN service_scope,
  DROP COLUMN review_status;

ALTER TABLE accounts
  DROP COLUMN credential_version,
  DROP COLUMN token_invalid_before;

ALTER TABLE delivery_orders
  DROP COLUMN completed_verified_at,
  DROP COLUMN picked_up_verified_at,
  DROP COLUMN assignment_version;

ALTER TABLE orders DROP COLUMN compliance_snapshot;
ALTER TABLE categories DROP COLUMN age_restricted;
ALTER TABLE products DROP COLUMN age_restricted;
