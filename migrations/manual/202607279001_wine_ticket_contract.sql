-- PRD-WT-20260718-01 纯结构 CONTRACT。
-- 本文件有意放在常规迁移目录之外。
-- 只有滚动发布运行手册中的门禁全部通过后，才能使用 Goose 执行。
-- 本文件只包含断言和 DDL，绝不包含无界数据 UPDATE。

-- +goose Up

DROP PROCEDURE IF EXISTS assert_wine_ticket_contract_ready;
-- +goose StatementBegin
CREATE PROCEDURE assert_wine_ticket_contract_ready()
BEGIN
  IF EXISTS (
    SELECT 1 FROM payments
    WHERE biz_type IS NULL OR biz_id IS NULL
       OR (biz_type = 'retail_order' AND (order_id IS NULL OR biz_id <> order_id))
       OR (biz_type <> 'retail_order' AND order_id IS NOT NULL)
    LIMIT 1
  ) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='wine-ticket CONTRACT blocked: payment registry is not clean';
  END IF;

  IF EXISTS (
    SELECT 1 FROM refunds
    WHERE biz_type IS NULL OR biz_id IS NULL
       OR (
         biz_type = 'retail_after_sale'
         AND (after_sale_id IS NULL OR order_id IS NULL OR biz_id <> after_sale_id)
       )
       OR (
         biz_type <> 'retail_after_sale'
         AND (after_sale_id IS NOT NULL OR order_id IS NOT NULL)
       )
    LIMIT 1
  ) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='wine-ticket CONTRACT blocked: refund registry is not clean';
  END IF;

  IF EXISTS (
    SELECT 1 FROM delivery_returns
    WHERE settlement_type IS NULL OR settlement_status IS NULL
       OR settlement_type NOT IN ('retail_cash_refund','wine_ticket_restore')
       OR (
         settlement_type = 'retail_cash_refund'
         AND NOT (
           (settlement_status = 'not_started' AND settlement_biz_id IS NULL AND settled_at IS NULL)
           OR (
             settlement_status IN ('processing','exception')
             AND settlement_biz_id IS NOT NULL AND settled_at IS NULL
           )
           OR (
             settlement_status = 'succeeded'
             AND settlement_biz_id IS NOT NULL AND settled_at IS NOT NULL
           )
         )
       )
       OR (
         settlement_type = 'wine_ticket_restore'
         AND NOT (
           settlement_biz_id IS NOT NULL
           AND (
             (settlement_status IN ('not_started','processing','exception') AND settled_at IS NULL)
             OR (settlement_status = 'succeeded' AND settled_at IS NOT NULL)
           )
         )
       )
    LIMIT 1
  ) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='wine-ticket CONTRACT blocked: return settlement registry is not clean';
  END IF;
END;
-- +goose StatementEnd

CALL assert_wine_ticket_contract_ready();
DROP PROCEDURE assert_wine_ticket_contract_ready;

ALTER TABLE payments
  MODIFY COLUMN biz_type VARCHAR(32) NOT NULL,
  MODIFY COLUMN biz_id BIGINT UNSIGNED NOT NULL,
  MODIFY COLUMN order_id BIGINT UNSIGNED NULL,
  ADD CONSTRAINT chk_payment_business_link CHECK (
    (biz_type = 'retail_order' AND order_id IS NOT NULL AND biz_id = order_id)
    OR (biz_type <> 'retail_order' AND order_id IS NULL)
  );

ALTER TABLE refunds
  MODIFY COLUMN biz_type VARCHAR(48) NOT NULL,
  MODIFY COLUMN biz_id BIGINT UNSIGNED NOT NULL,
  MODIFY COLUMN after_sale_id BIGINT UNSIGNED NULL,
  MODIFY COLUMN order_id BIGINT UNSIGNED NULL,
  ADD CONSTRAINT chk_refund_business_link CHECK (
    (
      biz_type = 'retail_after_sale'
      AND after_sale_id IS NOT NULL
      AND order_id IS NOT NULL
      AND biz_id = after_sale_id
    )
    OR (
      biz_type <> 'retail_after_sale'
      AND after_sale_id IS NULL
      AND order_id IS NULL
    )
  );

ALTER TABLE delivery_returns
  MODIFY COLUMN settlement_type VARCHAR(32) NOT NULL,
  MODIFY COLUMN settlement_status VARCHAR(24) NOT NULL,
  ADD CONSTRAINT chk_delivery_return_settlement_type CHECK (
    settlement_type IN ('retail_cash_refund','wine_ticket_restore')
  ),
  ADD CONSTRAINT chk_delivery_return_settlement_state CHECK (
    (
      settlement_type = 'retail_cash_refund'
      AND (
        (
          settlement_status = 'not_started'
          AND settlement_biz_id IS NULL
          AND settled_at IS NULL
        )
        OR (
          settlement_status IN ('processing','exception')
          AND settlement_biz_id IS NOT NULL
          AND settled_at IS NULL
        )
        OR (
          settlement_status = 'succeeded'
          AND settlement_biz_id IS NOT NULL
          AND settled_at IS NOT NULL
        )
      )
    )
    OR (
      settlement_type = 'wine_ticket_restore'
      AND settlement_biz_id IS NOT NULL
      AND (
        (
          settlement_status IN ('not_started','processing','exception')
          AND settled_at IS NULL
        )
        OR (
          settlement_status = 'succeeded'
          AND settled_at IS NOT NULL
        )
      )
    )
  );

-- +goose Down
-- 仅允许用于空的一次性数据库。
-- 非零售资金事实无法满足旧 NOT NULL 关联字段时，本回滚会有意失败。

ALTER TABLE delivery_returns
  DROP CHECK chk_delivery_return_settlement_state,
  DROP CHECK chk_delivery_return_settlement_type,
  MODIFY COLUMN settlement_status VARCHAR(24) NULL,
  MODIFY COLUMN settlement_type VARCHAR(32) NULL;

ALTER TABLE refunds
  DROP CHECK chk_refund_business_link,
  MODIFY COLUMN order_id BIGINT UNSIGNED NOT NULL,
  MODIFY COLUMN after_sale_id BIGINT UNSIGNED NOT NULL,
  MODIFY COLUMN biz_type VARCHAR(48) NULL,
  MODIFY COLUMN biz_id BIGINT UNSIGNED NULL;

ALTER TABLE payments
  DROP CHECK chk_payment_business_link,
  MODIFY COLUMN order_id BIGINT UNSIGNED NOT NULL,
  MODIFY COLUMN biz_type VARCHAR(32) NULL,
  MODIFY COLUMN biz_id BIGINT UNSIGNED NULL;
