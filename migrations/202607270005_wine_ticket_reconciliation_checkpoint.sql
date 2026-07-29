-- +goose Up
-- 每条记录负责一个上海时区业务日的全量完整性检查周期。
-- 业务表保持只读；本表仅存储持久化调度元数据。
CREATE TABLE wine_ticket_reconciliation_checkpoints (
  cycle_key CHAR(10) NOT NULL PRIMARY KEY,
  status VARCHAR(16) NOT NULL,
  phase VARCHAR(24) NOT NULL,
  last_id BIGINT UNSIGNED NOT NULL DEFAULT 0,
  high_watermarks JSON NOT NULL,
  checked_rows BIGINT UNSIGNED NOT NULL DEFAULT 0,
  detected_rows BIGINT UNSIGNED NOT NULL DEFAULT 0,
  lease_owner VARCHAR(128) NULL,
  lease_until DATETIME(3) NULL,
  started_at DATETIME(3) NOT NULL,
  last_batch_at DATETIME(3) NULL,
  completed_at DATETIME(3) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
    ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_wt_reconciliation_claim (status, lease_until, cycle_key),
  CONSTRAINT chk_wt_reconciliation_checkpoint_status
    CHECK (status IN ('running','completed')),
  CONSTRAINT chk_wt_reconciliation_checkpoint_phase
    CHECK (
      phase IN (
        'payments','purchases','lots','redemptions','gifts',
        'renewals','refunds','slots','reminders'
      )
    ),
  CONSTRAINT chk_wt_reconciliation_checkpoint_lease
    CHECK (
      (lease_owner IS NULL AND lease_until IS NULL)
      OR
      (lease_owner IS NOT NULL AND lease_until IS NOT NULL)
    )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
DROP TABLE IF EXISTS wine_ticket_reconciliation_checkpoints;
