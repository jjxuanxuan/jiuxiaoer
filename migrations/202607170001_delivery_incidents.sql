-- +goose Up
CREATE TABLE IF NOT EXISTS delivery_incidents (
  id BIGINT UNSIGNED NOT NULL,
  incident_no VARCHAR(64) NOT NULL,
  delivery_order_id BIGINT UNSIGNED NOT NULL,
  order_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  rider_id BIGINT UNSIGNED NOT NULL,
  type VARCHAR(32) NOT NULL,
  stage VARCHAR(16) NOT NULL,
  status VARCHAR(32) NOT NULL,
  priority VARCHAR(16) NOT NULL,
  reason_code VARCHAR(64) NULL,
  description VARCHAR(1000) NOT NULL,
  delivery_status_snapshot VARCHAR(32) NOT NULL,
  assignment_version_snapshot INT UNSIGNED NOT NULL,
  contact_attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
  first_contact_at DATETIME(3) NULL,
  last_contact_at DATETIME(3) NULL,
  distance_to_destination_m INT UNSIGNED NULL,
  location_accuracy_m DECIMAL(10,2) NULL,
  location_captured_at DATETIME(3) NULL,
  acknowledged_by BIGINT UNSIGNED NULL,
  acknowledged_at DATETIME(3) NULL,
  resolved_by BIGINT UNSIGNED NULL,
  resolved_at DATETIME(3) NULL,
  resolution_code VARCHAR(64) NULL,
  resolution_note VARCHAR(1000) NULL,
  rejected_by BIGINT UNSIGNED NULL,
  rejected_at DATETIME(3) NULL,
  rejection_code VARCHAR(64) NULL,
  rejection_reason VARCHAR(1000) NULL,
  reported_at DATETIME(3) NOT NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL,
  updated_at DATETIME(3) NOT NULL,
  active_delivery_order_id BIGINT UNSIGNED GENERATED ALWAYS AS (
    CASE WHEN status IN ('evidence_required','open','acknowledged') THEN delivery_order_id ELSE NULL END
  ) STORED,
  PRIMARY KEY (id),
  UNIQUE KEY uk_delivery_incidents_no (incident_no),
  UNIQUE KEY uk_delivery_incidents_active_type (active_delivery_order_id,type),
  KEY idx_delivery_incidents_status_reported (status,reported_at,id),
  KEY idx_delivery_incidents_shop_status (shop_id,status,reported_at,id),
  KEY idx_delivery_incidents_rider_reported (rider_id,reported_at,id),
  KEY idx_delivery_incidents_delivery_reported (delivery_order_id,reported_at,id),
  KEY idx_delivery_incidents_order (order_id,id),
  CONSTRAINT chk_delivery_incidents_type CHECK (type IN ('out_of_stock','alcohol_damaged','customer_refused','customer_unreachable')),
  CONSTRAINT chk_delivery_incidents_stage CHECK (stage IN ('pickup','delivery')),
  CONSTRAINT chk_delivery_incidents_status CHECK (status IN ('evidence_required','open','acknowledged','resolved','rejected')),
  CONSTRAINT chk_delivery_incidents_priority CHECK (priority IN ('high','urgent')),
  CONSTRAINT chk_delivery_incidents_version CHECK (version >= 1),
  CONSTRAINT chk_delivery_incidents_contact_pair CHECK (
    (first_contact_at IS NULL AND last_contact_at IS NULL) OR
    (first_contact_at IS NOT NULL AND last_contact_at IS NOT NULL AND last_contact_at >= first_contact_at)
  ),
  CONSTRAINT chk_delivery_incidents_location_accuracy CHECK (location_accuracy_m IS NULL OR location_accuracy_m >= 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS delivery_incident_items (
  id BIGINT UNSIGNED NOT NULL,
  incident_id BIGINT UNSIGNED NOT NULL,
  order_item_id BIGINT UNSIGNED NOT NULL,
  shop_product_id BIGINT UNSIGNED NULL,
  product_id BIGINT UNSIGNED NULL,
  quantity INT UNSIGNED NOT NULL,
  item_snapshot JSON NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_delivery_incident_items_line (incident_id,order_item_id),
  KEY idx_delivery_incident_items_order_item (order_item_id),
  CONSTRAINT chk_delivery_incident_items_quantity CHECK (quantity > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS delivery_incident_evidence (
  id BIGINT UNSIGNED NOT NULL,
  incident_id BIGINT UNSIGNED NOT NULL,
  token_id VARCHAR(128) NOT NULL,
  object_key VARCHAR(512) NOT NULL,
  mime_type VARCHAR(64) NOT NULL,
  size_bytes BIGINT UNSIGNED NOT NULL,
  sha256 CHAR(64) NOT NULL,
  scan_status VARCHAR(16) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_delivery_incident_evidence_token (token_id),
  UNIQUE KEY uk_delivery_incident_evidence_object (incident_id,object_key),
  KEY idx_delivery_incident_evidence_created (incident_id,created_at,id),
  CONSTRAINT chk_delivery_incident_evidence_mime CHECK (mime_type IN ('image/jpeg','image/png','image/heic')),
  CONSTRAINT chk_delivery_incident_evidence_size CHECK (size_bytes > 0 AND size_bytes <= 20971520),
  CONSTRAINT chk_delivery_incident_evidence_scan CHECK (scan_status = 'clean')
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS delivery_incident_history (
  id BIGINT UNSIGNED NOT NULL,
  incident_id BIGINT UNSIGNED NOT NULL,
  actor_type VARCHAR(16) NOT NULL,
  actor_id BIGINT UNSIGNED NULL,
  action VARCHAR(32) NOT NULL,
  from_status VARCHAR(32) NULL,
  to_status VARCHAR(32) NOT NULL,
  reason_code VARCHAR(64) NULL,
  remark VARCHAR(1000) NULL,
  request_id VARCHAR(128) NOT NULL,
  created_at DATETIME(3) NOT NULL,
  PRIMARY KEY (id),
  KEY idx_delivery_incident_history_timeline (incident_id,created_at,id),
  KEY idx_delivery_incident_history_request (request_id),
  CONSTRAINT chk_delivery_incident_history_actor CHECK (actor_type IN ('rider','admin','system')),
  CONSTRAINT chk_delivery_incident_history_action CHECK (action IN ('reported','evidence_added','acknowledged','resolved','rejected')),
  CONSTRAINT chk_delivery_incident_history_to_status CHECK (to_status IN ('evidence_required','open','acknowledged','resolved','rejected'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE IF EXISTS delivery_incident_history;
DROP TABLE IF EXISTS delivery_incident_evidence;
DROP TABLE IF EXISTS delivery_incident_items;
DROP TABLE IF EXISTS delivery_incidents;
