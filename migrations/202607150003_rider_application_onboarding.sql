-- +goose Up
-- Rider applications have a separate lifecycle from formal riders. A rider
-- row is created only when an administrator approves an application.

-- The approved-flow source value is `rider_application` (17 characters).
ALTER TABLE rider_service_shops MODIFY COLUMN source VARCHAR(32) NOT NULL;

CREATE TABLE IF NOT EXISTS rider_applications (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  application_no VARCHAR(64) NOT NULL,
  account_id BIGINT UNSIGNED NOT NULL,
  rider_id BIGINT UNSIGNED NULL,
  name VARCHAR(64) NOT NULL,
  service_scope JSON NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'submitted',
  submission_count INT UNSIGNED NOT NULL DEFAULT 1,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  create_idempotency_key_hash CHAR(64) NOT NULL,
  create_request_hash CHAR(64) NOT NULL,
  last_submitted_at DATETIME(3) NOT NULL,
  last_reviewed_at DATETIME(3) NULL,
  approved_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  created_by BIGINT UNSIGNED NULL,
  updated_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_rider_application_no (application_no),
  UNIQUE KEY uk_rider_application_account (account_id),
  UNIQUE KEY uk_rider_application_rider (rider_id),
  UNIQUE KEY uk_rider_application_create_idem (create_idempotency_key_hash),
  KEY idx_rider_application_queue (status, last_submitted_at, id),
  KEY idx_rider_application_updated (updated_at, id),
  CONSTRAINT chk_rider_application_status CHECK (status IN ('submitted','rejected','approved','cancelled')),
  CONSTRAINT chk_rider_application_counters CHECK (submission_count > 0 AND version > 0),
  CONSTRAINT chk_rider_application_scope CHECK (
    JSON_TYPE(service_scope) = 'OBJECT'
    AND JSON_CONTAINS_PATH(service_scope, 'one', '$.shop_ids') = 1
    AND JSON_TYPE(JSON_EXTRACT(service_scope, '$.shop_ids')) = 'ARRAY'
    AND JSON_LENGTH(JSON_EXTRACT(service_scope, '$.shop_ids')) > 0
  ),
  CONSTRAINT chk_rider_application_approval CHECK (
    (status = 'approved' AND rider_id IS NOT NULL AND approved_at IS NOT NULL)
    OR
    (status <> 'approved' AND rider_id IS NULL AND approved_at IS NULL)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rider_application_reviews (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  application_id BIGINT UNSIGNED NOT NULL,
  submission_no INT UNSIGNED NOT NULL,
  decision VARCHAR(16) NOT NULL,
  reason VARCHAR(255) NOT NULL,
  reviewer_admin_id BIGINT UNSIGNED NOT NULL,
  application_snapshot JSON NOT NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_rider_application_review_submission (application_id, submission_no),
  KEY idx_rider_application_review_admin (reviewer_admin_id, created_at),
  KEY idx_rider_application_review_created (created_at, id),
  CONSTRAINT chk_rider_application_review_submission CHECK (submission_no > 0),
  CONSTRAINT chk_rider_application_review_decision CHECK (decision IN ('approved','rejected')),
  CONSTRAINT chk_rider_application_review_reason CHECK (CHAR_LENGTH(reason) BETWEEN 2 AND 255)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- Local development must run `make provision-runtime-user` after migration so
-- the runtime account receives CRUD grants for these new tables.

-- +goose Down
-- Non-destructive rollback: disable JXE_RIDER_APPLICATION_ENABLED and retain
-- application and review audit data.
SELECT 1;
