-- +goose Up
-- Permission IDs 2132-2144 are allocated after the repository's current
-- controlled catalog maximum (2131). Abort before the first write when either
-- an ID or code belongs to a different permission; the upsert below is safe
-- only after this bidirectional mapping has been proven.
DROP PROCEDURE IF EXISTS assert_phase_one_permission_catalog;
-- +goose StatementBegin
CREATE PROCEDURE assert_phase_one_permission_catalog()
BEGIN
  IF EXISTS (
    SELECT 1
    FROM permissions p
    WHERE
      (p.id = 2132 AND NOT (BINARY p.code = BINARY 'cart:view'))
      OR (p.id = 2133 AND NOT (BINARY p.code = BINARY 'cart:update'))
      OR (p.id = 2134 AND NOT (BINARY p.code = BINARY 'order:create'))
      OR (p.id = 2135 AND NOT (BINARY p.code = BINARY 'payment:create'))
      OR (p.id = 2136 AND NOT (BINARY p.code = BINARY 'payment:view'))
      OR (p.id = 2137 AND NOT (BINARY p.code = BINARY 'delivery_verification:view_customer'))
      OR (p.id = 2138 AND NOT (BINARY p.code = BINARY 'store_order:view'))
      OR (p.id = 2139 AND NOT (BINARY p.code = BINARY 'print_setting:test_shop'))
      OR (p.id = 2140 AND NOT (BINARY p.code = BINARY 'delivery:view_own'))
      OR (p.id = 2141 AND NOT (BINARY p.code = BINARY 'delivery:pickup'))
      OR (p.id = 2142 AND NOT (BINARY p.code = BINARY 'delivery:complete'))
      OR (p.id = 2143 AND NOT (BINARY p.code = BINARY 'delivery:force_complete_request'))
      OR (p.id = 2144 AND NOT (BINARY p.code = BINARY 'delivery:force_complete_approve'))
      OR (LOWER(RTRIM(p.code)) = 'cart:view' AND p.id <> 2132)
      OR (LOWER(RTRIM(p.code)) = 'cart:update' AND p.id <> 2133)
      OR (LOWER(RTRIM(p.code)) = 'order:create' AND p.id <> 2134)
      OR (LOWER(RTRIM(p.code)) = 'payment:create' AND p.id <> 2135)
      OR (LOWER(RTRIM(p.code)) = 'payment:view' AND p.id <> 2136)
      OR (LOWER(RTRIM(p.code)) = 'delivery_verification:view_customer' AND p.id <> 2137)
      OR (LOWER(RTRIM(p.code)) = 'store_order:view' AND p.id <> 2138)
      OR (LOWER(RTRIM(p.code)) = 'print_setting:test_shop' AND p.id <> 2139)
      OR (LOWER(RTRIM(p.code)) = 'delivery:view_own' AND p.id <> 2140)
      OR (LOWER(RTRIM(p.code)) = 'delivery:pickup' AND p.id <> 2141)
      OR (LOWER(RTRIM(p.code)) = 'delivery:complete' AND p.id <> 2142)
      OR (LOWER(RTRIM(p.code)) = 'delivery:force_complete_request' AND p.id <> 2143)
      OR (LOWER(RTRIM(p.code)) = 'delivery:force_complete_approve' AND p.id <> 2144)
  ) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='phase-one migration blocked: permission id/code catalog conflict';
  END IF;
END;
-- +goose StatementEnd
CALL assert_phase_one_permission_catalog();

-- Before production rollout, also run the read-only print preflight documented
-- in docs/runbooks/cp1-launch-closure.md. The query must return zero rows before
-- adding uk_cp1_print_provider_request; do not delete or rewrite print facts.
INSERT INTO permissions (id, code, resource, action, description, status) VALUES
  (2132, 'cart:view', 'cart', 'view', '查看本人购物车', 'active'),
  (2133, 'cart:update', 'cart', 'update', '维护本人购物车', 'active'),
  (2134, 'order:create', 'order', 'create', '创建本人订单', 'active'),
  (2135, 'payment:create', 'payment', 'create', '创建本人订单支付', 'active'),
  (2136, 'payment:view', 'payment', 'view', '查看本人订单支付', 'active'),
  (2137, 'delivery_verification:view_customer', 'delivery_verification', 'view_customer', '查看本人订单送达码', 'active'),
  (2138, 'store_order:view', 'store_order', 'view', '查看授权门店订单详情', 'active'),
  (2139, 'print_setting:test_shop', 'print_setting', 'test_shop', '测试授权门店打印设备', 'active'),
  (2140, 'delivery:view_own', 'delivery', 'view_own', '查看本人配送详情', 'active'),
  (2141, 'delivery:pickup', 'delivery', 'pickup', '核销本人配送取货', 'active'),
  (2142, 'delivery:complete', 'delivery', 'complete', '核销本人配送送达', 'active'),
  (2143, 'delivery:force_complete_request', 'delivery', 'force_complete_request', '发起强制完成配送申请', 'active'),
  (2144, 'delivery:force_complete_approve', 'delivery', 'force_complete_approve', '复核强制完成配送申请', 'active')
ON DUPLICATE KEY UPDATE
  resource = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(resource), resource),
  action = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(action), action),
  description = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(description), description),
  status = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), 'active', status),
  deleted_at = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), NULL, deleted_at);

-- Recheck after the write as well: this closes the gap if another catalog
-- writer races the initial preflight. Conflicting rows are never mutated above.
CALL assert_phase_one_permission_catalog();
DROP PROCEDURE assert_phase_one_permission_catalog;

-- Split force-complete maker/checker authority. The runtime also rejects the
-- same admin on both sides, while these mappings keep ordinary operations
-- unable to approve their own request.
INSERT INTO role_permissions (id, role_id, permission_id)
SELECT r.id * 1000 + p.id, r.id, p.id
FROM roles r
JOIN permissions p ON (
  (r.code = 'super_admin' AND p.code IN ('delivery:force_complete_request', 'delivery:force_complete_approve'))
  OR (r.code = 'admin_manager' AND p.code = 'delivery:force_complete_approve')
  OR (r.code = 'operation' AND p.code = 'delivery:force_complete_request')
)
WHERE r.deleted_at IS NULL AND p.deleted_at IS NULL
ON DUPLICATE KEY UPDATE deleted_at = NULL, updated_at = CURRENT_TIMESTAMP(3);

ALTER TABLE delivery_verifications
  ADD COLUMN mode_snapshot VARCHAR(16) NULL DEFAULT NULL AFTER stage,
  ADD COLUMN secret_key_version VARCHAR(32) NOT NULL DEFAULT 'v1' AFTER policy_version,
  ADD COLUMN activated_at DATETIME(3) NULL AFTER expires_at,
  ADD COLUMN invalidated_at DATETIME(3) NULL AFTER activated_at,
  ADD COLUMN invalidation_reason_code VARCHAR(64) NULL AFTER invalidated_at;

ALTER TABLE delivery_verification_attempts
  ADD COLUMN account_id BIGINT UNSIGNED NULL AFTER actor_id,
  ADD COLUMN device_id_hash CHAR(64) NULL AFTER ip_hash,
  ADD COLUMN mode_snapshot VARCHAR(16) NULL DEFAULT NULL AFTER stage;

ALTER TABLE print_settings
  ADD COLUMN provider_config_ref VARCHAR(128) NULL AFTER provider,
  ADD COLUMN device_status VARCHAR(32) NOT NULL DEFAULT 'unknown' AFTER device_id_mask,
  ADD COLUMN last_health_at DATETIME(3) NULL AFTER device_status,
  ADD COLUMN last_health_error_code VARCHAR(64) NULL AFTER last_health_at;

ALTER TABLE print_tasks
  ADD COLUMN payload_schema_version VARCHAR(32) NOT NULL DEFAULT 'legacy-v1' AFTER render_payload,
  ADD COLUMN source_task_id BIGINT UNSIGNED NULL AFTER reprint_seq,
  ADD COLUMN provider_status VARCHAR(32) NULL AFTER provider_request_id,
  ADD COLUMN submitted_at DATETIME(3) NULL AFTER provider_status,
  ADD COLUMN confirmed_at DATETIME(3) NULL AFTER submitted_at,
  ADD COLUMN callback_deadline_at DATETIME(3) NULL AFTER confirmed_at,
  ADD KEY idx_cp1_print_shop_status (shop_id, status, created_at, id),
  ADD KEY idx_cp1_print_provider_status (provider, provider_status, callback_deadline_at, id),
  ADD KEY idx_cp1_print_source_task (source_task_id),
  ADD UNIQUE KEY uk_cp1_print_provider_request (provider, provider_request_id);

CREATE TABLE print_templates (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  template_code VARCHAR(64) NOT NULL,
  version VARCHAR(32) NOT NULL,
  paper_width_mm SMALLINT UNSIGNED NOT NULL,
  payload_schema_version VARCHAR(32) NOT NULL,
  template_body TEXT NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'draft',
  created_by BIGINT UNSIGNED NOT NULL,
  published_by BIGINT UNSIGNED NULL,
  published_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_cp1_print_template_version (template_code, version),
  KEY idx_cp1_print_template_status (status, template_code, created_at, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE print_provider_callbacks (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  provider VARCHAR(32) NOT NULL,
  provider_event_id VARCHAR(128) NOT NULL,
  provider_request_id VARCHAR(128) NOT NULL,
  print_task_id BIGINT UNSIGNED NULL,
  signature_valid TINYINT(1) NOT NULL DEFAULT 0,
  body_hash CHAR(64) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'received',
  error_code VARCHAR(64) NULL,
  received_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  processed_at DATETIME(3) NULL,
  UNIQUE KEY uk_cp1_print_provider_event (provider, provider_event_id),
  KEY idx_cp1_print_callback_request (provider, provider_request_id),
  KEY idx_cp1_print_callback_status (status, received_at, id),
  KEY idx_cp1_print_callback_task (print_task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

ALTER TABLE orders
  ADD KEY idx_cp1_customer_orders (customer_id, created_at, id),
  ADD KEY idx_cp1_store_orders (merchant_id, shop_id, status, created_at, id);

-- Historical records cannot be proven to have been enforced. The application
-- treats an enforce runtime mode as authoritative and rotates active codes at
-- the next lifecycle boundary.
UPDATE delivery_verifications
SET mode_snapshot = 'observe'
WHERE mode_snapshot IS NULL OR mode_snapshot = '';

UPDATE delivery_verification_attempts
SET mode_snapshot = 'observe'
WHERE mode_snapshot IS NULL OR mode_snapshot = '';

ALTER TABLE delivery_verifications
  MODIFY COLUMN mode_snapshot VARCHAR(16) NOT NULL DEFAULT 'enforce';

ALTER TABLE delivery_verification_attempts
  MODIFY COLUMN mode_snapshot VARCHAR(16) NOT NULL DEFAULT 'enforce';

-- +goose Down
DELETE FROM role_permissions
WHERE permission_id BETWEEN 2132 AND 2144;

ALTER TABLE orders
  DROP INDEX idx_cp1_store_orders,
  DROP INDEX idx_cp1_customer_orders;

DROP TABLE IF EXISTS print_provider_callbacks;
DROP TABLE IF EXISTS print_templates;

ALTER TABLE print_tasks
  DROP INDEX uk_cp1_print_provider_request,
  DROP INDEX idx_cp1_print_source_task,
  DROP INDEX idx_cp1_print_provider_status,
  DROP INDEX idx_cp1_print_shop_status,
  DROP COLUMN callback_deadline_at,
  DROP COLUMN confirmed_at,
  DROP COLUMN submitted_at,
  DROP COLUMN provider_status,
  DROP COLUMN source_task_id,
  DROP COLUMN payload_schema_version;

ALTER TABLE print_settings
  DROP COLUMN last_health_error_code,
  DROP COLUMN last_health_at,
  DROP COLUMN device_status,
  DROP COLUMN provider_config_ref;

ALTER TABLE delivery_verification_attempts
  DROP COLUMN mode_snapshot,
  DROP COLUMN device_id_hash,
  DROP COLUMN account_id;

ALTER TABLE delivery_verifications
  DROP COLUMN invalidation_reason_code,
  DROP COLUMN invalidated_at,
  DROP COLUMN activated_at,
  DROP COLUMN secret_key_version,
  DROP COLUMN mode_snapshot;

DELETE FROM permissions WHERE id BETWEEN 2132 AND 2144 AND code IN (
  'cart:view', 'cart:update', 'order:create', 'payment:create', 'payment:view',
  'delivery_verification:view_customer', 'store_order:view', 'print_setting:test_shop',
  'delivery:view_own', 'delivery:pickup', 'delivery:complete',
  'delivery:force_complete_request', 'delivery:force_complete_approve'
);
