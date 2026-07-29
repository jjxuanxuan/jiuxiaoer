-- +goose Up
-- 职责范围受限的平台角色必须获得明确的门店授权。
-- 空集合不会被解释为全局访问权限。
CREATE TABLE IF NOT EXISTS admin_user_shops (
  id BIGINT UNSIGNED NOT NULL PRIMARY KEY,
  admin_user_id BIGINT UNSIGNED NOT NULL,
  shop_id BIGINT UNSIGNED NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  deleted_at DATETIME(3) NULL,
  created_by BIGINT UNSIGNED NULL,
  updated_by BIGINT UNSIGNED NULL,
  UNIQUE KEY uk_admin_user_shop (admin_user_id, shop_id),
  KEY idx_admin_shop (shop_id, admin_user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

-- 范围变更属于授权变更。
-- 写入本表的配置流程还必须递增相关 accounts.credential_version，
-- 从而撤销现有 JWT 快照。

-- +goose Down
DROP TABLE IF EXISTS admin_user_shops;
