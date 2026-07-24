-- +goose Up
-- 一期要求审计事实可独立搜索。保留旧版 JSON 快照作为取证上下文，
-- 但不再要求操作人员解析快照才能获得常用的订单、配送、状态和错误维度。
ALTER TABLE audit_logs
  ADD COLUMN event_id VARCHAR(64) NULL AFTER id,
  ADD COLUMN account_id BIGINT UNSIGNED NULL AFTER actor_id,
  ADD COLUMN shop_id BIGINT UNSIGNED NULL AFTER resource_id,
  ADD COLUMN order_id BIGINT UNSIGNED NULL AFTER shop_id,
  ADD COLUMN delivery_id BIGINT UNSIGNED NULL AFTER order_id,
  ADD COLUMN error_code VARCHAR(64) NULL AFTER result,
  ADD COLUMN reason_code VARCHAR(64) NULL AFTER error_code,
  ADD COLUMN before_status VARCHAR(64) NULL AFTER reason_code,
  ADD COLUMN after_status VARCHAR(64) NULL AFTER before_status,
  ADD COLUMN version BIGINT UNSIGNED NULL AFTER after_status,
  ADD COLUMN ip_hash CHAR(64) NULL AFTER ip;

-- 现有审计 ID 是全局唯一的雪花值，因此可提供确定性的旧版事件标识符。
-- 新的 API 和工作进程写入使用 UUID。
UPDATE audit_logs
SET event_id = CONCAT('legacy-audit-', id)
WHERE event_id IS NULL OR event_id = '';

UPDATE audit_logs
SET
  shop_id = CASE WHEN resource_type = 'shop' THEN resource_id ELSE shop_id END,
  order_id = CASE WHEN resource_type = 'order' THEN resource_id ELSE order_id END,
  delivery_id = CASE
    WHEN resource_type IN ('delivery_order', 'delivery_verification') THEN resource_id
    ELSE delivery_id
  END,
  error_code = COALESCE(error_code, NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.error_code')), 'null')),
  reason_code = COALESCE(reason_code, NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.reason_code')), 'null')),
  before_status = COALESCE(before_status, NULLIF(JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.status')), 'null')),
  after_status = COALESCE(after_status, NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.status')), 'null'));

-- 隐私迁移刻意为单向：只保留适合滥用关联分析的确定性 SHA-256 摘要，
-- 随后擦除原始地址。
UPDATE audit_logs
SET ip_hash = SHA2(TRIM(ip), 256)
WHERE ip_hash IS NULL AND ip IS NOT NULL AND TRIM(ip) <> '';

UPDATE audit_logs SET ip = NULL WHERE ip IS NOT NULL;

ALTER TABLE audit_logs
  ADD UNIQUE KEY uk_audit_event_id (event_id),
  ADD KEY idx_audit_account_created (account_id, created_at),
  ADD KEY idx_audit_shop_created (shop_id, created_at),
  ADD KEY idx_audit_order_created (order_id, created_at),
  ADD KEY idx_audit_delivery_created (delivery_id, created_at),
  ADD KEY idx_audit_error_created (error_code, created_at);

-- +goose Down
ALTER TABLE audit_logs
  DROP INDEX idx_audit_error_created,
  DROP INDEX idx_audit_delivery_created,
  DROP INDEX idx_audit_order_created,
  DROP INDEX idx_audit_shop_created,
  DROP INDEX idx_audit_account_created,
  DROP INDEX uk_audit_event_id,
  DROP COLUMN ip_hash,
  DROP COLUMN version,
  DROP COLUMN after_status,
  DROP COLUMN before_status,
  DROP COLUMN reason_code,
  DROP COLUMN error_code,
  DROP COLUMN delivery_id,
  DROP COLUMN order_id,
  DROP COLUMN shop_id,
  DROP COLUMN account_id,
  DROP COLUMN event_id;
