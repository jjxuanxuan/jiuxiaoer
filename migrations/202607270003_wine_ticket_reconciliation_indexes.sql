-- +goose Up
-- 三类台账任务按不可变 ID 推进。
-- 现有运营索引将 updated_at 放在 id 之前，
-- 因而无法在不触发 filesort 的情况下支持有界支付游标。
ALTER TABLE payments
  ADD KEY idx_payment_wt_reconcile (biz_type, status, id);

-- REC-WT-006 检查每个有界批次集合中的有效续期数量。
ALTER TABLE wine_ticket_renewals
  ADD KEY idx_wt_renewal_lot_status (lot_id, status, id);

-- +goose Down
ALTER TABLE wine_ticket_renewals
  DROP INDEX idx_wt_renewal_lot_status;

ALTER TABLE payments
  DROP INDEX idx_payment_wt_reconcile;
