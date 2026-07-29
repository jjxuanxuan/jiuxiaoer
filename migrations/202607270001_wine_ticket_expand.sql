-- PRD-WT-20260718-01 v1.0-rc1
-- 面向 backend-go 的 MySQL 8.4 / Goose 纯结构 EXPAND 迁移。
-- ID 使用应用生成的 Snowflake BIGINT UNSIGNED 值。
-- 本迁移有意兼容旧 Go 写入方：
-- payments/refunds 的业务字段保持可空，旧关联字段保持 NOT NULL。
-- 应先部署双写和注册代码并排空旧进程，再执行 ddl_contract.sql。
-- 在此之前，酒票资金写入功能开关必须保持关闭。
-- 生产回滚应通过应用或功能开关完成；存在业务事实后不得执行 Down。
-- Down 只允许用于空的一次性数据库。
-- 重要：现有支付、退款和退回记录由可续跑的主键游标任务回填。
-- 本迁移有意不包含数据 UPDATE。
-- 时间前置条件：backend-go DSN 使用 parseTime=True&loc=Local，
-- 进程使用 TZ=Asia/Shanghai，MySQL 全局及会话 time_zone=+08:00。
-- DATETIME(3) 存储上海时区墙上时间。若配置漂移，就绪检查必须失败；
-- 当前 Go 基线下不得使用 .UTC() 写入酒票时间。

-- +goose Up

-- ---------------------------------------------------------------------------
-- 1. 扩展现有支付和退款事实。
-- ---------------------------------------------------------------------------

ALTER TABLE payments
  ADD COLUMN biz_type VARCHAR(32) NULL AFTER payment_no,
  ADD COLUMN biz_id BIGINT UNSIGNED NULL AFTER biz_type,
  ADD UNIQUE KEY uk_payment_business_channel (biz_type, biz_id, channel),
  ADD KEY idx_payment_business_status (biz_type, status, updated_at, id);

-- 现有支付记录在 CONTRACT 前通过 Goose 之外的任务回填。

ALTER TABLE refunds
  ADD COLUMN biz_type VARCHAR(48) NULL AFTER refund_no,
  ADD COLUMN biz_id BIGINT UNSIGNED NULL AFTER biz_type,
  ADD KEY idx_refund_business_status (biz_type, biz_id, status, id);

-- 现有退款记录在 CONTRACT 前通过 Goose 之外的任务回填。

ALTER TABLE orders
  ADD COLUMN order_type VARCHAR(32) NOT NULL DEFAULT 'retail' AFTER order_no,
  ADD COLUMN settlement_mode VARCHAR(32) NOT NULL DEFAULT 'cash' AFTER order_type,
  ADD COLUMN delivery_time_slot_id BIGINT UNSIGNED NULL AFTER address_snapshot,
  ADD COLUMN delivery_time_slot_snapshot JSON NULL AFTER delivery_time_slot_id,
  ADD KEY idx_order_type_status (order_type, status, updated_at, id),
  ADD KEY idx_order_delivery_time_slot (delivery_time_slot_id, id),
  ADD CONSTRAINT chk_order_business_mode CHECK (
    (order_type = 'retail' AND settlement_mode = 'cash')
    OR (order_type = 'wine_ticket_redemption' AND settlement_mode = 'wine_ticket')
  );

ALTER TABLE delivery_orders
  ADD COLUMN scheduled_start_at DATETIME(3) NULL AFTER recipient_snapshot,
  ADD COLUMN scheduled_end_at DATETIME(3) NULL AFTER scheduled_start_at,
  ADD COLUMN not_before_at DATETIME(3) NULL AFTER scheduled_end_at,
  ADD KEY idx_delivery_not_before (status, not_before_at, id),
  ADD CONSTRAINT chk_delivery_schedule CHECK (
    (
      scheduled_start_at IS NULL
      AND scheduled_end_at IS NULL
      AND not_before_at IS NULL
    )
    OR (
      scheduled_start_at IS NOT NULL
      AND scheduled_end_at IS NOT NULL
      AND not_before_at IS NOT NULL
      AND scheduled_start_at < scheduled_end_at
      AND not_before_at <= scheduled_start_at
    )
  );

-- 当前配送退回审批和关闭流程按现金退款设计。
-- 此处增加兼容旧写入方的可空结算区分字段；
-- 只有注册和双写版本全部部署后，CONTRACT 才会收紧约束。
ALTER TABLE delivery_returns
  ADD COLUMN settlement_type VARCHAR(32) NULL AFTER after_sale_id,
  ADD COLUMN settlement_biz_id BIGINT UNSIGNED NULL AFTER settlement_type,
  ADD COLUMN settlement_status VARCHAR(24) NULL AFTER settlement_biz_id,
  ADD COLUMN settled_at DATETIME(3) NULL AFTER settlement_status,
  ADD UNIQUE KEY uk_delivery_return_settlement (settlement_type, settlement_biz_id),
  ADD KEY idx_delivery_return_settlement_status (
    settlement_type, settlement_status, updated_at, id
  );

-- 现有配送退回记录在 CONTRACT 前通过 Goose 之外的任务回填。

-- ---------------------------------------------------------------------------
-- 2. 套餐目录、购买、批次及不可变权益台账。
-- ---------------------------------------------------------------------------

CREATE TABLE wine_ticket_packages (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  package_no VARCHAR(64) NOT NULL,
  package_code VARCHAR(64) NOT NULL,
  package_version INT UNSIGNED NOT NULL,
  issuer_merchant_id BIGINT UNSIGNED NOT NULL,
  settlement_shop_id BIGINT UNSIGNED NOT NULL,
  settlement_shop_product_id BIGINT UNSIGNED NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  redeem_city_code VARCHAR(32) NOT NULL,
  package_type VARCHAR(24) NOT NULL,
  name VARCHAR(128) NOT NULL,
  subtitle VARCHAR(255) NULL,
  cover_image_url VARCHAR(512) NULL,
  bottle_quantity INT UNSIGNED NOT NULL,
  sale_price_amount BIGINT NOT NULL,
  min_purchase_quantity INT UNSIGNED NOT NULL DEFAULT 1,
  max_purchase_quantity INT UNSIGNED NOT NULL,
  validity_days INT UNSIGNED NOT NULL,
  per_customer_limit INT UNSIGNED NULL,
  refund_policy JSON NOT NULL,
  renewal_policy JSON NOT NULL,
  delivery_policy JSON NOT NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'draft',
  sale_start_at DATETIME(3) NULL,
  sale_end_at DATETIME(3) NULL,
  published_at DATETIME(3) NULL,
  published_by BIGINT UNSIGNED NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  created_by BIGINT UNSIGNED NULL,
  updated_by BIGINT UNSIGNED NULL,
  published_package_code VARCHAR(64) GENERATED ALWAYS AS (
    CASE WHEN status = 'published' THEN package_code ELSE NULL END
  ) STORED,
  UNIQUE KEY uk_wt_package_no (package_no),
  UNIQUE KEY uk_wt_package_code_version (package_code, package_version),
  UNIQUE KEY uk_wt_package_one_published (published_package_code),
  KEY idx_wt_package_catalog (status, package_type, sale_start_at, sale_end_at, id),
  KEY idx_wt_package_merchant_product (issuer_merchant_id, product_id, status, id),
  CONSTRAINT chk_wt_package_type CHECK (package_type IN ('stockpile','corporate','gift')),
  CONSTRAINT chk_wt_package_status CHECK (status IN ('draft','published','unpublished','archived')),
  CONSTRAINT chk_wt_package_values CHECK (
    package_version > 0
    AND bottle_quantity > 0
    AND bottle_quantity <= 10000
    AND sale_price_amount > 0
    AND sale_price_amount <= 100000000
    AND min_purchase_quantity > 0
    AND min_purchase_quantity <= 10000
    AND max_purchase_quantity >= min_purchase_quantity
    AND max_purchase_quantity <= 10000
    AND validity_days > 0
    AND validity_days <= 3650
    AND (
      per_customer_limit IS NULL
      OR (
        per_customer_limit >= min_purchase_quantity
        AND per_customer_limit <= 10000
      )
    )
    AND (sale_end_at IS NULL OR sale_start_at IS NULL OR sale_start_at < sale_end_at)
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_purchase_quotas (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  package_code VARCHAR(64) NOT NULL,
  reserved_quantity INT UNSIGNED NOT NULL DEFAULT 0,
  consumed_quantity INT UNSIGNED NOT NULL DEFAULT 0,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_purchase_quota_customer_code (customer_id, package_code),
  KEY idx_wt_purchase_quota_code (package_code, customer_id),
  CONSTRAINT chk_wt_purchase_quota_values CHECK (
    reserved_quantity >= 0 AND consumed_quantity >= 0
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_purchases (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  purchase_no VARCHAR(64) NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  package_id BIGINT UNSIGNED NOT NULL,
  package_version INT UNSIGNED NOT NULL,
  payment_id BIGINT UNSIGNED NOT NULL,
  issuer_merchant_id BIGINT UNSIGNED NOT NULL,
  settlement_shop_id BIGINT UNSIGNED NOT NULL,
  settlement_shop_product_id BIGINT UNSIGNED NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  redeem_city_code VARCHAR(32) NOT NULL,
  package_quantity INT UNSIGNED NOT NULL,
  bottle_quantity_per_package INT UNSIGNED NOT NULL,
  total_bottle_quantity INT UNSIGNED NOT NULL,
  unit_price_amount BIGINT NOT NULL,
  payable_amount BIGINT NOT NULL,
  paid_amount BIGINT NOT NULL DEFAULT 0,
  currency CHAR(3) NOT NULL DEFAULT 'CNY',
  package_snapshot JSON NOT NULL,
  refund_policy_snapshot JSON NOT NULL,
  renewal_policy_snapshot JSON NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'pending_payment',
  version INT UNSIGNED NOT NULL DEFAULT 1,
  paid_at DATETIME(3) NULL,
  issued_at DATETIME(3) NULL,
  refunded_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_purchase_no (purchase_no),
  UNIQUE KEY uk_wt_purchase_payment (payment_id),
  KEY idx_wt_purchase_customer_created (customer_id, created_at, id),
  KEY idx_wt_purchase_status_updated (status, updated_at, id),
  KEY idx_wt_purchase_package (package_id, created_at, id),
  KEY idx_wt_purchase_admin (issuer_merchant_id, status, created_at, id),
  CONSTRAINT chk_wt_purchase_status CHECK (
    status IN (
      'pending_payment','payment_unknown','settlement_exception','issued','closed',
      'refund_holding','refund_exception','refunded'
    )
  ),
  CONSTRAINT chk_wt_purchase_values CHECK (
    package_version > 0
    AND package_quantity > 0
    AND bottle_quantity_per_package > 0
    AND total_bottle_quantity = package_quantity * bottle_quantity_per_package
    AND unit_price_amount > 0
    AND unit_price_amount <= 100000000
    AND payable_amount = package_quantity * unit_price_amount
    AND payable_amount <= 100000000
    AND paid_amount >= 0
    AND paid_amount <= payable_amount
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_lots (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  lot_no VARCHAR(64) NOT NULL,
  owner_customer_id BIGINT UNSIGNED NOT NULL,
  purchase_id BIGINT UNSIGNED NOT NULL,
  source_type VARCHAR(24) NOT NULL,
  source_lot_id BIGINT UNSIGNED NULL,
  source_gift_id BIGINT UNSIGNED NULL,
  issuer_merchant_id BIGINT UNSIGNED NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  redeem_city_code VARCHAR(32) NOT NULL,
  total_quantity INT UNSIGNED NOT NULL,
  available_quantity INT UNSIGNED NOT NULL,
  original_expires_at DATETIME(3) NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  expiry_changed_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  renewal_count INT UNSIGNED NOT NULL DEFAULT 0,
  ever_used TINYINT(1) NOT NULL DEFAULT 0,
  status VARCHAR(24) NOT NULL DEFAULT 'active',
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_lot_no (lot_no),
  KEY idx_wt_lot_owner_product_expiry (
    owner_customer_id, product_id, status, expires_at, id
  ),
  KEY idx_wt_lot_owner_group_fefo (
    owner_customer_id, issuer_merchant_id, redeem_city_code, product_id,
    status, expires_at, id
  ),
  KEY idx_wt_lot_purchase (purchase_id, id),
  KEY idx_wt_lot_expiry_worker (status, expires_at, id),
  KEY idx_wt_lot_source (source_type, source_lot_id, source_gift_id),
  KEY idx_wt_lot_admin (issuer_merchant_id, product_id, status, expires_at, id),
  CONSTRAINT chk_wt_lot_source CHECK (
    (source_type = 'purchase' AND source_lot_id IS NULL AND source_gift_id IS NULL)
    OR (source_type = 'gift' AND source_lot_id IS NOT NULL AND source_gift_id IS NOT NULL)
  ),
  CONSTRAINT chk_wt_lot_status CHECK (status IN ('active','depleted','expired','refunded')),
  CONSTRAINT chk_wt_lot_values CHECK (
    total_quantity > 0
    AND available_quantity <= total_quantity
    AND original_expires_at <= expires_at
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_transactions (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  transaction_no VARCHAR(64) NOT NULL,
  lot_id BIGINT UNSIGNED NOT NULL,
  owner_customer_id BIGINT UNSIGNED NOT NULL,
  transaction_type VARCHAR(40) NOT NULL,
  quantity_delta INT NOT NULL,
  before_available_quantity INT UNSIGNED NOT NULL,
  after_available_quantity INT UNSIGNED NOT NULL,
  biz_type VARCHAR(48) NOT NULL,
  biz_id BIGINT UNSIGNED NOT NULL,
  action_key VARCHAR(160) NOT NULL,
  metadata_json JSON NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_transaction_no (transaction_no),
  UNIQUE KEY uk_wt_transaction_action (action_key),
  KEY idx_wt_transaction_lot_created (lot_id, created_at, id),
  KEY idx_wt_transaction_owner_created (owner_customer_id, created_at, id),
  KEY idx_wt_transaction_business (biz_type, biz_id, id),
  CONSTRAINT chk_wt_transaction_type CHECK (
    transaction_type IN (
      'purchase_issue',
      'redemption_hold','redemption_restore',
      'redemption_return_restore','redemption_return_expire',
      'gift_hold','gift_claim','gift_restore',
      'refund_hold','refund_restore','expiry'
    )
  ),
  CONSTRAINT chk_wt_transaction_values CHECK (
    quantity_delta <> 0
    AND after_available_quantity = before_available_quantity + quantity_delta
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ---------------------------------------------------------------------------
-- 3. 配送时段和核销。
-- ---------------------------------------------------------------------------

CREATE TABLE delivery_time_slots (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  shop_id BIGINT UNSIGNED NOT NULL,
  service_date DATE NOT NULL,
  start_time TIME(0) NOT NULL,
  end_time TIME(0) NOT NULL,
  cutoff_at DATETIME(3) NOT NULL,
  capacity_orders INT UNSIGNED NOT NULL,
  reserved_orders INT UNSIGNED NOT NULL DEFAULT 0,
  status VARCHAR(16) NOT NULL DEFAULT 'open',
  active_slot_key TINYINT UNSIGNED GENERATED ALWAYS AS (
    CASE WHEN status = 'open' THEN 1 ELSE NULL END
  ) STORED,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  created_by BIGINT UNSIGNED NULL,
  updated_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_delivery_time_slot_active_window (
    shop_id, service_date, start_time, end_time, active_slot_key
  ),
  KEY idx_delivery_time_slot_query (shop_id, service_date, status, start_time, id),
  KEY idx_delivery_time_slot_cutoff (status, cutoff_at, id),
  CONSTRAINT chk_delivery_time_slot_status CHECK (status IN ('open','closed')),
  CONSTRAINT chk_delivery_time_slot_values CHECK (
    start_time < end_time
    AND capacity_orders > 0
    AND reserved_orders <= capacity_orders
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_redemptions (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  redemption_no VARCHAR(64) NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  issuer_merchant_id BIGINT UNSIGNED NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  shop_product_id BIGINT UNSIGNED NOT NULL,
  delivery_time_slot_id BIGINT UNSIGNED NOT NULL,
  order_id BIGINT UNSIGNED NOT NULL,
  quantity INT UNSIGNED NOT NULL,
  address_id BIGINT UNSIGNED NOT NULL,
  address_version INT UNSIGNED NOT NULL,
  address_snapshot JSON NOT NULL,
  delivery_time_slot_snapshot JSON NOT NULL,
  product_snapshot JSON NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'scheduled',
  version INT UNSIGNED NOT NULL DEFAULT 1,
  scheduled_start_at DATETIME(3) NOT NULL,
  scheduled_end_at DATETIME(3) NOT NULL,
  not_before_at DATETIME(3) NOT NULL,
  picked_up_at DATETIME(3) NULL,
  completed_at DATETIME(3) NULL,
  cancelled_at DATETIME(3) NULL,
  restored_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_redemption_no (redemption_no),
  UNIQUE KEY uk_wt_redemption_order (order_id),
  KEY idx_wt_redemption_customer_created (customer_id, created_at, id),
  KEY idx_wt_redemption_shop_status (shop_id, status, updated_at, id),
  KEY idx_wt_redemption_slot (delivery_time_slot_id, status, id),
  CONSTRAINT chk_wt_redemption_status CHECK (
    status IN (
      'scheduled','assigned','picked_up','delivered','cancelled',
      'return_in_progress','restored','exception'
    )
  ),
  CONSTRAINT chk_wt_redemption_values CHECK (
    quantity > 0
    AND scheduled_start_at < scheduled_end_at
    AND not_before_at <= scheduled_start_at
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_redemption_allocations (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  redemption_id BIGINT UNSIGNED NOT NULL,
  lot_id BIGINT UNSIGNED NOT NULL,
  quantity INT UNSIGNED NOT NULL,
  source_expires_at DATETIME(3) NOT NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'held',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_redemption_allocation (redemption_id, lot_id),
  KEY idx_wt_redemption_allocation_lot (lot_id, status, id),
  CONSTRAINT chk_wt_redemption_allocation_status CHECK (
    status IN ('held','consumed','restored','exception')
  ),
  CONSTRAINT chk_wt_redemption_allocation_quantity CHECK (quantity > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ---------------------------------------------------------------------------
-- 4. 礼赠和再次分享令牌。
-- ---------------------------------------------------------------------------

CREATE TABLE wine_ticket_gifts (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  gift_no VARCHAR(64) NOT NULL,
  giver_customer_id BIGINT UNSIGNED NOT NULL,
  receiver_customer_id BIGINT UNSIGNED NULL,
  issuer_merchant_id BIGINT UNSIGNED NOT NULL,
  product_id BIGINT UNSIGNED NOT NULL,
  redeem_city_code VARCHAR(32) NOT NULL,
  quantity INT UNSIGNED NOT NULL,
  message VARCHAR(140) NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'pending',
  claim_deadline DATETIME(3) NOT NULL,
  earliest_expires_at DATETIME(3) NOT NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  claimed_at DATETIME(3) NULL,
  cancelled_at DATETIME(3) NULL,
  returned_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_gift_no (gift_no),
  KEY idx_wt_gift_giver_created (giver_customer_id, created_at, id),
  KEY idx_wt_gift_receiver_created (receiver_customer_id, created_at, id),
  KEY idx_wt_gift_timeout (status, claim_deadline, id),
  CONSTRAINT chk_wt_gift_status CHECK (
    status IN ('pending','claimed','cancelled','expired_returned','exception')
  ),
  CONSTRAINT chk_wt_gift_values CHECK (
    quantity > 0
    AND claim_deadline > created_at
    AND claim_deadline <= earliest_expires_at
  ),
  CONSTRAINT chk_wt_gift_receiver CHECK (
    (status = 'claimed' AND receiver_customer_id IS NOT NULL AND receiver_customer_id <> giver_customer_id)
    OR (status <> 'claimed')
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_gift_allocations (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  gift_id BIGINT UNSIGNED NOT NULL,
  source_lot_id BIGINT UNSIGNED NOT NULL,
  receiver_lot_id BIGINT UNSIGNED NULL,
  quantity INT UNSIGNED NOT NULL,
  source_expires_at DATETIME(3) NOT NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'held',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_gift_allocation (gift_id, source_lot_id),
  UNIQUE KEY uk_wt_gift_receiver_lot (receiver_lot_id),
  KEY idx_wt_gift_allocation_lot (source_lot_id, status, id),
  CONSTRAINT chk_wt_gift_allocation_status CHECK (
    status IN ('held','claimed','restored','exception')
  ),
  CONSTRAINT chk_wt_gift_allocation_values CHECK (
    quantity > 0
    AND ((status = 'claimed' AND receiver_lot_id IS NOT NULL) OR status <> 'claimed')
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_gift_claim_tokens (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  gift_id BIGINT UNSIGNED NOT NULL,
  token_digest CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  issued_by_customer_id BIGINT UNSIGNED NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  consumed_at DATETIME(3) NULL,
  revoked_at DATETIME(3) NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_gift_token_digest (token_digest),
  KEY idx_wt_gift_token_active (gift_id, consumed_at, revoked_at, expires_at, id),
  KEY idx_wt_gift_token_expiry (expires_at, id),
  CONSTRAINT chk_wt_gift_token_terminal CHECK (
    consumed_at IS NULL OR revoked_at IS NULL
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ---------------------------------------------------------------------------
-- 5. 续期和退款业务保护。
-- ---------------------------------------------------------------------------

CREATE TABLE wine_ticket_renewals (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  renewal_no VARCHAR(64) NOT NULL,
  lot_id BIGINT UNSIGNED NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  payment_id BIGINT UNSIGNED NULL,
  compensating_refund_id BIGINT UNSIGNED NULL,
  old_expires_at DATETIME(3) NOT NULL,
  new_expires_at DATETIME(3) NOT NULL,
  extension_days INT UNSIGNED NOT NULL,
  fee_amount BIGINT NOT NULL,
  currency CHAR(3) NOT NULL DEFAULT 'CNY',
  policy_snapshot JSON NOT NULL,
  expected_lot_version INT UNSIGNED NOT NULL,
  status VARCHAR(32) NOT NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  completed_at DATETIME(3) NULL,
  closed_at DATETIME(3) NULL,
  refunded_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  active_lot_id BIGINT UNSIGNED GENERATED ALWAYS AS (
    CASE
      WHEN status IN (
        'pending_payment','payment_unknown','applying',
        'compensating_refund','refund_exception'
      ) THEN lot_id
      ELSE NULL
    END
  ) STORED,
  UNIQUE KEY uk_wt_renewal_no (renewal_no),
  UNIQUE KEY uk_wt_renewal_payment (payment_id),
  UNIQUE KEY uk_wt_renewal_active_lot (active_lot_id),
  KEY idx_wt_renewal_customer_created (customer_id, created_at, id),
  KEY idx_wt_renewal_status_updated (status, updated_at, id),
  CONSTRAINT chk_wt_renewal_status CHECK (
    status IN (
      'pending_payment','payment_unknown','applying','completed','closed',
      'compensating_refund','refund_exception','refunded'
    )
  ),
  CONSTRAINT chk_wt_renewal_values CHECK (
    extension_days > 0
    AND fee_amount >= 0
    AND fee_amount <= 100000000
    AND old_expires_at < new_expires_at
    AND (
      (fee_amount = 0 AND payment_id IS NULL)
      OR (fee_amount > 0 AND payment_id IS NOT NULL)
    )
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_refunds (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  wine_ticket_refund_no VARCHAR(64) NOT NULL,
  purchase_id BIGINT UNSIGNED NOT NULL,
  customer_id BIGINT UNSIGNED NOT NULL,
  current_refund_id BIGINT UNSIGNED NOT NULL,
  refund_kind VARCHAR(32) NOT NULL,
  amount BIGINT NOT NULL,
  currency CHAR(3) NOT NULL DEFAULT 'CNY',
  reason_code VARCHAR(64) NOT NULL,
  reason_text VARCHAR(256) NULL,
  eligibility_snapshot JSON NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'holding',
  version INT UNSIGNED NOT NULL DEFAULT 1,
  requested_at DATETIME(3) NOT NULL,
  succeeded_at DATETIME(3) NULL,
  closed_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  active_purchase_id BIGINT UNSIGNED GENERATED ALWAYS AS (
    CASE
      WHEN status IN (
        'holding','submitting','processing','submission_unknown',
        'retry_pending','exception'
      ) THEN purchase_id
      ELSE NULL
    END
  ) STORED,
  UNIQUE KEY uk_wt_refund_no (wine_ticket_refund_no),
  UNIQUE KEY uk_wt_refund_active_purchase (active_purchase_id),
  KEY idx_wt_refund_common (current_refund_id, id),
  KEY idx_wt_refund_purchase (purchase_id, id),
  KEY idx_wt_refund_customer_created (customer_id, created_at, id),
  KEY idx_wt_refund_status_updated (status, updated_at, id),
  CONSTRAINT chk_wt_refund_status CHECK (
    status IN (
      'holding','submitting','processing','submission_unknown',
      'retry_pending','exception','succeeded','cancelled'
    )
  ),
  CONSTRAINT chk_wt_refund_kind CHECK (
    refund_kind IN ('user_unused','issuance_compensation')
  ),
  CONSTRAINT chk_wt_refund_amount CHECK (amount > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_refund_allocations (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  wine_ticket_refund_id BIGINT UNSIGNED NOT NULL,
  lot_id BIGINT UNSIGNED NOT NULL,
  quantity INT UNSIGNED NOT NULL,
  source_expires_at DATETIME(3) NOT NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'held',
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_refund_allocation (wine_ticket_refund_id, lot_id),
  KEY idx_wt_refund_allocation_lot (lot_id, status, id),
  CONSTRAINT chk_wt_refund_allocation_status CHECK (
    status IN ('held','consumed','restored','exception')
  ),
  CONSTRAINT chk_wt_refund_allocation_quantity CHECK (quantity > 0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- ---------------------------------------------------------------------------
-- 6. 提醒、订阅授权和异常事实。
-- ---------------------------------------------------------------------------

CREATE TABLE wine_ticket_reminders (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  lot_id BIGINT UNSIGNED NOT NULL,
  owner_customer_id BIGINT UNSIGNED NOT NULL,
  expires_at DATETIME(3) NOT NULL,
  remind_days TINYINT UNSIGNED NOT NULL,
  channel VARCHAR(24) NOT NULL,
  status VARCHAR(24) NOT NULL DEFAULT 'pending',
  attempts INT UNSIGNED NOT NULL DEFAULT 0,
  provider_message_id VARCHAR(128) NULL,
  last_error_code VARCHAR(64) NULL,
  locked_by VARCHAR(128) NULL,
  locked_until DATETIME(3) NULL,
  scheduled_at DATETIME(3) NOT NULL,
  sent_at DATETIME(3) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  UNIQUE KEY uk_wt_reminder_dedupe (lot_id, expires_at, remind_days, channel),
  KEY idx_wt_reminder_dispatch (status, scheduled_at, locked_until, id),
  KEY idx_wt_reminder_owner (owner_customer_id, created_at, id),
  CONSTRAINT chk_wt_reminder_channel CHECK (channel IN ('inbox','wechat_subscription')),
  CONSTRAINT chk_wt_reminder_status CHECK (
    status IN ('pending','sent','skipped','failed')
  ),
  CONSTRAINT chk_wt_reminder_days CHECK (
    remind_days = 7
    AND scheduled_at < expires_at
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE notification_subscription_consents (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  customer_id BIGINT UNSIGNED NOT NULL,
  scene VARCHAR(48) NOT NULL,
  template_code VARCHAR(64) NOT NULL,
  consent_result VARCHAR(16) NOT NULL,
  provider_receipt VARCHAR(128) NULL,
  status VARCHAR(24) NOT NULL,
  consented_at DATETIME(3) NOT NULL,
  expires_at DATETIME(3) NULL,
  claimed_by_reminder_id BIGINT UNSIGNED NULL,
  claimed_at DATETIME(3) NULL,
  request_id VARCHAR(128) NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  KEY idx_subscription_consent_customer (
    customer_id, scene, template_code, consented_at, id
  ),
  KEY idx_subscription_consent_status (status, expires_at, id),
  UNIQUE KEY uk_subscription_consent_reminder (claimed_by_reminder_id),
  CONSTRAINT chk_subscription_consent_result CHECK (
    consent_result IN ('accepted','rejected','unknown')
  ),
  CONSTRAINT chk_subscription_consent_status CHECK (
    status IN ('available','sending','consumed','exhausted','expired','rejected','unknown')
  ),
  CONSTRAINT chk_subscription_consent_claim CHECK (
    (status = 'available' AND claimed_by_reminder_id IS NULL AND claimed_at IS NULL)
    OR (
      status IN ('sending','consumed','exhausted')
      AND claimed_by_reminder_id IS NOT NULL
      AND claimed_at IS NOT NULL
    )
    OR (
      status = 'unknown'
      AND (
        (claimed_by_reminder_id IS NULL AND claimed_at IS NULL)
        OR (claimed_by_reminder_id IS NOT NULL AND claimed_at IS NOT NULL)
      )
    )
    OR (
      status IN ('expired','rejected')
      AND claimed_by_reminder_id IS NULL
      AND claimed_at IS NULL
    )
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE wine_ticket_exceptions (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  exception_no VARCHAR(64) NOT NULL,
  exception_type VARCHAR(64) NOT NULL,
  biz_type VARCHAR(48) NOT NULL,
  biz_id BIGINT UNSIGNED NOT NULL,
  biz_no VARCHAR(64) NULL,
  issuer_merchant_id BIGINT UNSIGNED NULL,
  source_type VARCHAR(48) NOT NULL,
  source_id BIGINT UNSIGNED NULL,
  correlation_id VARCHAR(128) NULL,
  severity VARCHAR(8) NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'investigating',
  expected_snapshot JSON NULL,
  actual_snapshot JSON NULL,
  proposed_action VARCHAR(64) NULL,
  proposed_reason VARCHAR(500) NULL,
  review_ticket_no VARCHAR(64) NULL,
  proposed_by BIGINT UNSIGNED NULL,
  proposed_at DATETIME(3) NULL,
  review_decision VARCHAR(16) NULL,
  review_note VARCHAR(500) NULL,
  reviewed_by BIGINT UNSIGNED NULL,
  reviewed_at DATETIME(3) NULL,
  resolution_result JSON NULL,
  occurrence_count INT UNSIGNED NOT NULL DEFAULT 1,
  first_detected_at DATETIME(3) NOT NULL,
  last_detected_at DATETIME(3) NOT NULL,
  resolved_at DATETIME(3) NULL,
  version INT UNSIGNED NOT NULL DEFAULT 1,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  active_exception_key VARCHAR(192) GENERATED ALWAYS AS (
    CASE
      WHEN status IN ('investigating','awaiting_external_fact','pending_review')
      THEN CONCAT(exception_type, ':', biz_type, ':', CAST(biz_id AS CHAR))
      ELSE NULL
    END
  ) STORED,
  UNIQUE KEY uk_wt_exception_no (exception_no),
  UNIQUE KEY uk_wt_exception_active (active_exception_key),
  KEY idx_wt_exception_queue (status, severity, updated_at, id),
  KEY idx_wt_exception_business (biz_type, biz_id, id),
  KEY idx_wt_exception_source (source_type, source_id, id),
  CONSTRAINT chk_wt_exception_severity CHECK (severity IN ('P0','P1','P2','P3')),
  CONSTRAINT chk_wt_exception_status CHECK (
    status IN ('investigating','awaiting_external_fact','pending_review','resolved')
  ),
  CONSTRAINT chk_wt_exception_reviewers CHECK (
    reviewed_by IS NULL OR proposed_by IS NULL OR reviewed_by <> proposed_by
  )
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- +goose Down
-- 仅允许用于空的一次性数据库。

DROP TABLE wine_ticket_exceptions;
DROP TABLE notification_subscription_consents;
DROP TABLE wine_ticket_reminders;
DROP TABLE wine_ticket_refund_allocations;
DROP TABLE wine_ticket_refunds;
DROP TABLE wine_ticket_renewals;
DROP TABLE wine_ticket_gift_claim_tokens;
DROP TABLE wine_ticket_gift_allocations;
DROP TABLE wine_ticket_gifts;
DROP TABLE wine_ticket_redemption_allocations;
DROP TABLE wine_ticket_redemptions;
DROP TABLE delivery_time_slots;
DROP TABLE wine_ticket_transactions;
DROP TABLE wine_ticket_lots;
DROP TABLE wine_ticket_purchases;
DROP TABLE wine_ticket_purchase_quotas;
DROP TABLE wine_ticket_packages;

ALTER TABLE delivery_orders
  DROP CHECK chk_delivery_schedule,
  DROP INDEX idx_delivery_not_before,
  DROP COLUMN not_before_at,
  DROP COLUMN scheduled_end_at,
  DROP COLUMN scheduled_start_at;

ALTER TABLE delivery_returns
  DROP INDEX idx_delivery_return_settlement_status,
  DROP INDEX uk_delivery_return_settlement,
  DROP COLUMN settled_at,
  DROP COLUMN settlement_status,
  DROP COLUMN settlement_biz_id,
  DROP COLUMN settlement_type;

ALTER TABLE orders
  DROP CHECK chk_order_business_mode,
  DROP INDEX idx_order_delivery_time_slot,
  DROP INDEX idx_order_type_status,
  DROP COLUMN delivery_time_slot_snapshot,
  DROP COLUMN delivery_time_slot_id,
  DROP COLUMN settlement_mode,
  DROP COLUMN order_type;

ALTER TABLE refunds
  DROP INDEX idx_refund_business_status,
  DROP COLUMN biz_id,
  DROP COLUMN biz_type;

ALTER TABLE payments
  DROP INDEX idx_payment_business_status,
  DROP INDEX uk_payment_business_channel,
  DROP COLUMN biz_id,
  DROP COLUMN biz_type;
