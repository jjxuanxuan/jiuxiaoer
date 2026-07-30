-- +goose Up
-- 常购清单先按客户、完成状态和完成时间裁剪订单，再关联订单商品。
ALTER TABLE orders
  ADD INDEX idx_customer_status_completed (customer_id, status, completed_at, id);

ALTER TABLE order_items
  ADD INDEX idx_order_product (order_id, product_id);

-- +goose Down
ALTER TABLE order_items
  DROP INDEX idx_order_product;

ALTER TABLE orders
  DROP INDEX idx_customer_status_completed;
