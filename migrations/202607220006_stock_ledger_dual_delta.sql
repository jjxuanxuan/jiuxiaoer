-- +goose Up
-- quantity_delta remains the available-to-sell movement so every ledger row
-- satisfies before_available_qty + quantity_delta = after_available_qty.
-- Physical inventory movement is recorded independently: reservation/release
-- only moves buckets, while payment deducts total inventory without changing
-- the already-reserved available quantity.
ALTER TABLE stock_records
  ADD COLUMN total_quantity_delta INT NULL AFTER after_available_qty,
  ADD COLUMN before_total_qty INT NULL AFTER total_quantity_delta,
  ADD COLUMN after_total_qty INT NULL AFTER before_total_qty;

-- Preserve the legacy payment deduction before normalising quantity_delta to
-- its now-explicit available-quantity meaning.
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

-- Reconstruct the historical total ledger backwards from the current stock
-- fact. The latest row ends at current total; each earlier row subtracts all
-- later physical deltas. Rows without a live stock fact intentionally remain
-- NULL so the migration fails instead of silently inventing inventory.
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
