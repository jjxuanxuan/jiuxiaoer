-- +goose Up
-- 混合派单是增量迁移。支付成功会创建配送和任务事实；
-- 门店备货仅用于打开取货门禁。

-- 旧数据存在重复项时，在第一条 DDL 语句前终止并给出可操作消息，
-- 避免留下部分应用的迁移。
DROP PROCEDURE IF EXISTS assert_one_active_delivery_assignment;
-- +goose StatementBegin
CREATE PROCEDURE assert_one_active_delivery_assignment()
BEGIN
  IF EXISTS (
    SELECT 1 FROM delivery_assignments
    WHERE status='active'
    GROUP BY delivery_order_id
    HAVING COUNT(*) > 1
  ) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='hybrid dispatch migration blocked: duplicate active delivery assignments';
  END IF;
END;
-- +goose StatementEnd
CALL assert_one_active_delivery_assignment();
DROP PROCEDURE assert_one_active_delivery_assignment;

CREATE TABLE IF NOT EXISTS dispatch_policies (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  policy_code VARCHAR(64) NOT NULL,
  scope_type VARCHAR(16) NOT NULL,
  scope_id VARCHAR(64) NOT NULL DEFAULT '0',
  version INT UNSIGNED NOT NULL,
  mode VARCHAR(16) NOT NULL,
  auto_rounds TINYINT UNSIGNED NOT NULL DEFAULT 3,
  offer_ttl_seconds INT UNSIGNED NOT NULL DEFAULT 10,
  grab_ttl_seconds INT UNSIGNED NOT NULL DEFAULT 30,
  candidate_limit INT UNSIGNED NOT NULL DEFAULT 100,
  offer_candidate_limit INT UNSIGNED NOT NULL DEFAULT 3,
  heartbeat_fresh_seconds INT UNSIGNED NOT NULL DEFAULT 60,
  location_fresh_seconds INT UNSIGNED NOT NULL DEFAULT 120,
  max_location_accuracy_m INT UNSIGNED NOT NULL DEFAULT 200,
  max_pickup_distance_m INT UNSIGNED NOT NULL DEFAULT 5000,
  max_active_orders_default TINYINT UNSIGNED NOT NULL DEFAULT 3,
  idle_full_score_seconds INT UNSIGNED NOT NULL DEFAULT 1800,
  score_weights JSON NOT NULL,
  rejection_cooldown_seconds INT UNSIGNED NOT NULL DEFAULT 120,
  status VARCHAR(16) NOT NULL DEFAULT 'draft',
  published_at DATETIME(3) NULL,
  published_by BIGINT UNSIGNED NULL,
  row_version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  created_by BIGINT UNSIGNED NOT NULL,
  updated_by BIGINT UNSIGNED NOT NULL,
  published_scope_key VARCHAR(96) GENERATED ALWAYS AS (
    CASE WHEN status = 'published' THEN CONCAT(scope_type, ':', scope_id) ELSE NULL END
  ) STORED,
  UNIQUE KEY uk_dispatch_policy_version (policy_code, scope_type, scope_id, version),
  UNIQUE KEY uk_dispatch_policy_published_scope (published_scope_key),
  KEY idx_dispatch_policy_lookup (scope_type, scope_id, status, version),
  CONSTRAINT chk_dispatch_policy_mode CHECK (mode IN ('hybrid','auto','grab','manual')),
  CONSTRAINT chk_dispatch_policy_status CHECK (status IN ('draft','validated','published','retired')),
  CONSTRAINT chk_dispatch_policy_limits CHECK (
    auto_rounds <= 10 AND offer_ttl_seconds BETWEEN 5 AND 60 AND
    grab_ttl_seconds BETWEEN 5 AND 300 AND candidate_limit BETWEEN 1 AND 500 AND
    offer_candidate_limit BETWEEN 1 AND candidate_limit AND
    heartbeat_fresh_seconds BETWEEN 15 AND 300 AND location_fresh_seconds BETWEEN 30 AND 600 AND
    max_location_accuracy_m BETWEEN 20 AND 1000 AND max_pickup_distance_m BETWEEN 500 AND 50000 AND
    max_active_orders_default BETWEEN 1 AND 20 AND idle_full_score_seconds BETWEEN 60 AND 86400
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS dispatch_jobs (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  job_no VARCHAR(64) NOT NULL,
  delivery_order_id BIGINT UNSIGNED NOT NULL,
  order_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  dispatch_seq INT UNSIGNED NOT NULL DEFAULT 1,
  policy_id BIGINT UNSIGNED NULL,
  policy_version VARCHAR(64) NOT NULL,
  policy_snapshot JSON NOT NULL,
  mode VARCHAR(16) NOT NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'pending',
  round_no TINYINT UNSIGNED NOT NULL DEFAULT 0,
  candidate_cursor INT UNSIGNED NOT NULL DEFAULT 0,
  grab_opened_at DATETIME(3) NULL,
  grab_expires_at DATETIME(3) NULL,
  next_action_at DATETIME(3) NOT NULL,
  locked_until DATETIME(3) NULL,
  locked_by VARCHAR(128) NULL,
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  last_error_code VARCHAR(64) NULL,
  last_error_safe VARCHAR(255) NULL,
  status_reason_code VARCHAR(64) NULL,
  status_reason_safe VARCHAR(255) NULL,
  first_started_at DATETIME(3) NULL,
  assigned_at DATETIME(3) NULL,
  assigned_rider_id BIGINT UNSIGNED NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  active_delivery_id BIGINT UNSIGNED GENERATED ALWAYS AS (
    CASE WHEN status IN ('pending','scoring','offering','grab_open','manual_required')
      THEN delivery_order_id ELSE NULL END
  ) STORED,
  UNIQUE KEY uk_dispatch_job_no (job_no),
  UNIQUE KEY uk_dispatch_job_sequence (delivery_order_id, dispatch_seq),
  UNIQUE KEY uk_dispatch_job_one_active (active_delivery_id),
  KEY idx_dispatch_job_claim (status, next_action_at, locked_until, id),
  KEY idx_dispatch_job_shop (shop_id, status, created_at, id),
  KEY idx_dispatch_job_assigned (assigned_rider_id, assigned_at),
  CONSTRAINT chk_dispatch_job_mode CHECK (mode IN ('hybrid','auto','grab','manual')),
  CONSTRAINT chk_dispatch_job_status CHECK (status IN ('pending','scoring','offering','grab_open','manual_required','assigned','cancelled'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS dispatch_candidates (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  job_id BIGINT UNSIGNED NOT NULL,
  rider_id BIGINT UNSIGNED NOT NULL,
  rank_no INT UNSIGNED NOT NULL,
  eligible TINYINT(1) NOT NULL DEFAULT 0,
  exclusion_codes JSON NOT NULL,
  distance_m INT UNSIGNED NULL,
  active_orders TINYINT UNSIGNED NOT NULL DEFAULT 0,
  max_active_orders TINYINT UNSIGNED NOT NULL,
  heartbeat_age_seconds INT UNSIGNED NULL,
  location_age_seconds INT UNSIGNED NULL,
  distance_score DECIMAL(7,4) NULL,
  load_score DECIMAL(7,4) NULL,
  idle_score DECIMAL(7,4) NULL,
  freshness_score DECIMAL(7,4) NULL,
  rejection_penalty DECIMAL(7,4) NULL,
  final_score DECIMAL(7,4) NULL,
  score_version VARCHAR(32) NOT NULL,
  input_snapshot JSON NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_dispatch_candidate_job_rider (job_id, rider_id),
  KEY idx_dispatch_candidate_rank (job_id, eligible, rank_no),
  KEY idx_dispatch_candidate_rider (rider_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS dispatch_offers (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  offer_no VARCHAR(64) NOT NULL,
  job_id BIGINT UNSIGNED NOT NULL,
  delivery_order_id BIGINT UNSIGNED NOT NULL,
  rider_id BIGINT UNSIGNED NOT NULL,
  round_no TINYINT UNSIGNED NOT NULL,
  candidate_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'pending',
  expires_at DATETIME(3) NOT NULL,
  responded_at DATETIME(3) NULL,
  rejection_reason_code VARCHAR(64) NULL,
  rejection_remark VARCHAR(255) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  active_job_id BIGINT UNSIGNED GENERATED ALWAYS AS (
    CASE WHEN status = 'pending' THEN job_id ELSE NULL END
  ) STORED,
  UNIQUE KEY uk_dispatch_offer_no (offer_no),
  UNIQUE KEY uk_dispatch_offer_round (job_id, round_no),
  UNIQUE KEY uk_dispatch_offer_one_pending (active_job_id),
  KEY idx_dispatch_offer_rider (rider_id, status, expires_at, id),
  KEY idx_dispatch_offer_job (job_id, status),
  CONSTRAINT chk_dispatch_offer_status CHECK (status IN ('pending','accepted','rejected','expired','cancelled'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rider_runtime_states (
  rider_id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  work_status VARCHAR(16) NOT NULL DEFAULT 'offline',
  latitude DECIMAL(10,7) NULL,
  longitude DECIMAL(10,7) NULL,
  accuracy_m DECIMAL(8,2) NULL,
  captured_at DATETIME(3) NULL,
  heartbeat_at DATETIME(3) NULL,
  device_id_hash CHAR(64) NULL,
  last_sequence BIGINT UNSIGNED NOT NULL DEFAULT 0,
  online_since DATETIME(3) NULL,
  last_assigned_at DATETIME(3) NULL,
  max_active_orders TINYINT UNSIGNED NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_rider_runtime_heartbeat (work_status, heartbeat_at),
  KEY idx_rider_runtime_last_assigned (last_assigned_at),
  CONSTRAINT chk_rider_runtime_work CHECK (work_status IN ('online','offline')),
  CONSTRAINT chk_rider_runtime_location CHECK (
    (latitude IS NULL AND longitude IS NULL) OR
    (latitude BETWEEN -90 AND 90 AND longitude BETWEEN -180 AND 180)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS rider_service_shops (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  rider_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  status VARCHAR(16) NOT NULL DEFAULT 'active',
  source VARCHAR(16) NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  created_by BIGINT UNSIGNED NULL,
  updated_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_rider_service_shop (rider_id, shop_id),
  KEY idx_rider_service_shop_lookup (shop_id, status, rider_id),
  CONSTRAINT chk_rider_service_shop_status CHECK (status IN ('active','inactive'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 兼容性回填：派单热路径不再读取 riders.service_scope，
-- 因此启用应用前，每个有效的旧版门店关系都必须存在于规范化表中。
INSERT INTO rider_service_shops (id,rider_id,shop_id,status,source,created_by,updated_by)
SELECT
  CAST(CONV(SUBSTRING(SHA2(CONCAT(r.id,':',legacy_scope.shop_id_raw),256),1,15),16,10) AS UNSIGNED),
  r.id,
  CAST(legacy_scope.shop_id_raw AS UNSIGNED),
  'active',
  'migration',
  r.updated_by,
  r.updated_by
FROM riders r
JOIN JSON_TABLE(
  COALESCE(r.service_scope, JSON_OBJECT('shop_ids',JSON_ARRAY())),
  '$.shop_ids[*]' COLUMNS (shop_id_raw VARCHAR(32) PATH '$')
) legacy_scope
JOIN shops s ON s.id=CAST(legacy_scope.shop_id_raw AS UNSIGNED) AND s.deleted_at IS NULL
WHERE r.deleted_at IS NULL
  AND legacy_scope.shop_id_raw REGEXP '^[1-9][0-9]{0,19}$'
ON DUPLICATE KEY UPDATE status='active',source='migration',updated_by=VALUES(updated_by);

-- MySQL 不支持 ADD COLUMN IF NOT EXISTS 语法。
-- 这些辅助过程使增量迁移在非破坏性 goose 回滚后可以安全重复执行。
DROP PROCEDURE IF EXISTS hybrid_dispatch_add_column;
-- +goose StatementBegin
CREATE PROCEDURE hybrid_dispatch_add_column(
  IN table_name_value VARCHAR(64),
  IN column_name_value VARCHAR(64),
  IN ddl_value TEXT
)
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.COLUMNS
    WHERE TABLE_SCHEMA=DATABASE()
      AND TABLE_NAME=table_name_value
      AND COLUMN_NAME=column_name_value
  ) THEN
    SET @hybrid_dispatch_ddl=ddl_value;
    PREPARE hybrid_dispatch_stmt FROM @hybrid_dispatch_ddl;
    EXECUTE hybrid_dispatch_stmt;
    DEALLOCATE PREPARE hybrid_dispatch_stmt;
  END IF;
END;
-- +goose StatementEnd

DROP PROCEDURE IF EXISTS hybrid_dispatch_add_index;
-- +goose StatementBegin
CREATE PROCEDURE hybrid_dispatch_add_index(
  IN table_name_value VARCHAR(64),
  IN index_name_value VARCHAR(64),
  IN ddl_value TEXT
)
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.STATISTICS
    WHERE TABLE_SCHEMA=DATABASE()
      AND TABLE_NAME=table_name_value
      AND INDEX_NAME=index_name_value
  ) THEN
    SET @hybrid_dispatch_ddl=ddl_value;
    PREPARE hybrid_dispatch_stmt FROM @hybrid_dispatch_ddl;
    EXECUTE hybrid_dispatch_stmt;
    DEALLOCATE PREPARE hybrid_dispatch_stmt;
  END IF;
END;
-- +goose StatementEnd

CALL hybrid_dispatch_add_column('delivery_orders','dispatch_status','ALTER TABLE delivery_orders ADD COLUMN dispatch_status VARCHAR(24) NOT NULL DEFAULT ''pending'' AFTER assignment_version');
CALL hybrid_dispatch_add_column('delivery_orders','current_dispatch_job_id','ALTER TABLE delivery_orders ADD COLUMN current_dispatch_job_id BIGINT UNSIGNED NULL AFTER dispatch_status');
CALL hybrid_dispatch_add_column('delivery_orders','dispatch_mode_snapshot','ALTER TABLE delivery_orders ADD COLUMN dispatch_mode_snapshot VARCHAR(16) NULL AFTER current_dispatch_job_id');
CALL hybrid_dispatch_add_column('delivery_orders','dispatch_policy_version','ALTER TABLE delivery_orders ADD COLUMN dispatch_policy_version VARCHAR(64) NULL AFTER dispatch_mode_snapshot');
CALL hybrid_dispatch_add_column('delivery_orders','pickup_ready_status','ALTER TABLE delivery_orders ADD COLUMN pickup_ready_status VARCHAR(16) NOT NULL DEFAULT ''waiting_store'' AFTER dispatch_policy_version');
CALL hybrid_dispatch_add_column('delivery_orders','pickup_ready_at','ALTER TABLE delivery_orders ADD COLUMN pickup_ready_at DATETIME(3) NULL AFTER pickup_ready_status');
CALL hybrid_dispatch_add_index('delivery_orders','idx_delivery_dispatch','ALTER TABLE delivery_orders ADD KEY idx_delivery_dispatch (dispatch_status,status,created_at,id)');
CALL hybrid_dispatch_add_index('delivery_orders','idx_delivery_dispatch_job','ALTER TABLE delivery_orders ADD KEY idx_delivery_dispatch_job (current_dispatch_job_id)');
CALL hybrid_dispatch_add_index('delivery_orders','idx_delivery_pickup_ready','ALTER TABLE delivery_orders ADD KEY idx_delivery_pickup_ready (pickup_ready_status,status,id)');

UPDATE delivery_orders
SET dispatch_status = CASE
      WHEN status IN ('accepted','delivering','completed') THEN 'assigned'
      WHEN status = 'cancelled' THEN 'cancelled'
      ELSE 'pending'
    END,
    pickup_ready_status = CASE
      -- 此迁移前，delivery_orders 只会在门店备货时创建，
      -- 因此每条未取消的旧版记录都已具备取货条件。
      WHEN status IN ('pending_assign','accepted','delivering','completed') THEN 'ready'
      WHEN status = 'cancelled' THEN 'cancelled'
      ELSE 'waiting_store'
    END,
    pickup_ready_at = CASE
      WHEN status IN ('pending_assign','accepted','delivering','completed')
        THEN COALESCE(picked_up_at,accepted_at,created_at)
      ELSE NULL
    END
WHERE dispatch_policy_version IS NULL;

-- 旧版未分配记录不能悄然公开为抢单任务。它们会进入可审计的人工队列，
-- 并可在策略审核后重试。
INSERT INTO dispatch_jobs (
  id,job_no,delivery_order_id,order_id,shop_id,dispatch_seq,policy_id,policy_version,
  policy_snapshot,mode,status,next_action_at,status_reason_code,status_reason_safe,version
)
SELECT
  d.id,CONCAT('MIG-DJ-',d.id),d.id,d.order_id,d.shop_id,1,NULL,'migration/manual-v1',
  JSON_OBJECT(
    'mode','manual','auto_rounds',0,'offer_ttl_seconds',10,'grab_ttl_seconds',30,
    'candidate_limit',100,'offer_candidate_limit',3,'heartbeat_fresh_seconds',60,
    'location_fresh_seconds',120,'max_location_accuracy_m',200,'max_pickup_distance_m',5000,
    'max_active_orders_default',3,'idle_full_score_seconds',1800,
    'score_weights',JSON_OBJECT('distance',0.45,'load',0.30,'idle',0.20,'freshness',0.05),
    'rejection_cooldown_seconds',120,'score_version','dispatch-score-v1'
  ),
  'manual','manual_required',DATE_ADD(CURRENT_TIMESTAMP(3),INTERVAL 365 DAY),
  'MIGRATION_BACKFILL','legacy delivery requires dispatcher review',1
FROM delivery_orders d
WHERE d.status='pending_assign' AND d.rider_id IS NULL AND d.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM dispatch_jobs existing_job WHERE existing_job.delivery_order_id=d.id);

UPDATE delivery_orders d
JOIN dispatch_jobs j ON j.delivery_order_id=d.id AND j.dispatch_seq=1 AND j.status='manual_required'
SET d.dispatch_status='manual_required',
    d.current_dispatch_job_id=j.id,
    d.dispatch_mode_snapshot='manual',
    d.dispatch_policy_version='migration/manual-v1';

CALL hybrid_dispatch_add_column('delivery_assignments','dispatch_job_id','ALTER TABLE delivery_assignments ADD COLUMN dispatch_job_id BIGINT UNSIGNED NULL AFTER delivery_order_id');
CALL hybrid_dispatch_add_column('delivery_assignments','offer_id','ALTER TABLE delivery_assignments ADD COLUMN offer_id BIGINT UNSIGNED NULL AFTER dispatch_job_id');
CALL hybrid_dispatch_add_column('delivery_assignments','score_snapshot','ALTER TABLE delivery_assignments ADD COLUMN score_snapshot JSON NULL AFTER assignment_type');
CALL hybrid_dispatch_add_column('delivery_assignments','active_delivery_order_id','ALTER TABLE delivery_assignments ADD COLUMN active_delivery_order_id BIGINT UNSIGNED GENERATED ALWAYS AS (CASE WHEN status = ''active'' THEN delivery_order_id ELSE NULL END) STORED');
CALL hybrid_dispatch_add_index('delivery_assignments','uk_delivery_assignment_one_active','ALTER TABLE delivery_assignments ADD UNIQUE KEY uk_delivery_assignment_one_active (active_delivery_order_id)');
CALL hybrid_dispatch_add_index('delivery_assignments','idx_delivery_assignment_job','ALTER TABLE delivery_assignments ADD KEY idx_delivery_assignment_job (dispatch_job_id)');
CALL hybrid_dispatch_add_index('delivery_assignments','idx_delivery_assignment_offer','ALTER TABLE delivery_assignments ADD KEY idx_delivery_assignment_offer (offer_id)');

CALL hybrid_dispatch_add_column('riders','work_status_version','ALTER TABLE riders ADD COLUMN work_status_version INT UNSIGNED NOT NULL DEFAULT 1 AFTER work_status');
CALL hybrid_dispatch_add_column('riders','capabilities','ALTER TABLE riders ADD COLUMN capabilities JSON NULL AFTER service_scope');

INSERT INTO rider_runtime_states (rider_id,work_status,last_sequence,version)
SELECT id,
       CASE WHEN work_status='online' THEN 'online' ELSE 'offline' END,
       0,
       1
FROM riders
WHERE deleted_at IS NULL
ON DUPLICATE KEY UPDATE work_status=VALUES(work_status);

DROP PROCEDURE hybrid_dispatch_add_index;
DROP PROCEDURE hybrid_dispatch_add_column;

-- +goose Down
-- 此回滚刻意保持非破坏性。运维回滚会关闭派单，
-- 同时保留分配唯一性和历史记录。
SELECT 1;
