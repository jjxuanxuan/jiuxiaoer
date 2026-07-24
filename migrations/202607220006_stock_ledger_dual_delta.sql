-- +goose Up
-- quantity_delta 仍表示可售数量变动，使每条账本记录都满足
-- before_available_qty + quantity_delta = after_available_qty。
-- 实物库存变动独立记录：预留和释放只在桶之间移动，
-- 支付会扣减总库存，但不改变已经预留的可售数量。
ALTER TABLE stock_records
  ADD COLUMN total_quantity_delta INT NULL AFTER after_available_qty,
  ADD COLUMN before_total_qty INT NULL AFTER total_quantity_delta,
  ADD COLUMN after_total_qty INT NULL AFTER before_total_qty;

-- 在将 quantity_delta 规范为明确的可售数量含义前，
-- 先保留旧版支付扣减值。
UPDATE stock_records
SET total_quantity_delta = CASE
  WHEN change_type IN ('reserve', 'release') THEN 0
  ELSE quantity_delta
END
WHERE deleted_at IS NULL;

UPDATE stock_records
SET quantity_delta = 0
WHERE deleted_at IS NULL
  AND change_type = 'deduct'
  AND source_type = 'payment'
  AND before_available_qty = after_available_qty;

-- 从当前库存事实向后重建历史总库存账本。最新记录终止于当前总量；
-- 每条更早记录都减去其后的所有实物变动。没有现存库存事实的记录刻意保留 NULL，
-- 使迁移失败，而不是悄然虚构库存。
UPDATE stock_records sr
JOIN (
  SELECT
    history.id,
    current_total - COALESCE(
      SUM(history.total_quantity_delta) OVER (
        PARTITION BY history.shop_product_id
        ORDER BY history.created_at DESC, history.id DESC
        ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING
      ),
      0
    ) AS reconstructed_after_total
  FROM stock_records history
  JOIN (
    SELECT shop_product_id, available_qty + reserved_qty + locked_qty AS current_total
    FROM product_stocks
    WHERE deleted_at IS NULL
  ) current_stock ON current_stock.shop_product_id = history.shop_product_id
  WHERE history.deleted_at IS NULL
) reconstructed ON reconstructed.id = sr.id
SET
  sr.after_total_qty = reconstructed.reconstructed_after_total,
  sr.before_total_qty = reconstructed.reconstructed_after_total - sr.total_quantity_delta
WHERE sr.deleted_at IS NULL;

ALTER TABLE stock_records
  MODIFY COLUMN total_quantity_delta INT NOT NULL DEFAULT 0,
  MODIFY COLUMN before_total_qty INT NOT NULL DEFAULT 0,
  MODIFY COLUMN after_total_qty INT NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE stock_records
  DROP COLUMN after_total_qty,
  DROP COLUMN before_total_qty,
  DROP COLUMN total_quantity_delta;
