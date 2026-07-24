-- +goose Up
-- 商户用户此前仅根据 accounts.account_type 继承全部商户能力。
-- 将每个用户绑定到明确的商户范围角色，使令牌权限快照可从受控目录重建。
DROP PROCEDURE IF EXISTS assert_merchant_rbac_catalog;
-- +goose StatementBegin
CREATE PROCEDURE assert_merchant_rbac_catalog()
BEGIN
  IF EXISTS (
    SELECT 1
    FROM roles r
    WHERE
      (r.id = 1006 AND NOT (BINARY r.code = BINARY 'merchant_owner'))
      OR (r.id = 1007 AND NOT (BINARY r.code = BINARY 'merchant_order_operator'))
      OR (r.id = 1008 AND NOT (BINARY r.code = BINARY 'merchant_inventory_clerk'))
      OR (LOWER(RTRIM(r.code)) = 'merchant_owner' AND r.id <> 1006)
      OR (LOWER(RTRIM(r.code)) = 'merchant_order_operator' AND r.id <> 1007)
      OR (LOWER(RTRIM(r.code)) = 'merchant_inventory_clerk' AND r.id <> 1008)
  ) OR EXISTS (
    SELECT 1
    FROM permissions p
    WHERE
      (p.id = 2145 AND NOT (BINARY p.code = BINARY 'merchant_user:update_role'))
      OR (LOWER(RTRIM(p.code)) = 'merchant_user:update_role' AND p.id <> 2145)
  ) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='merchant RBAC migration blocked: role or permission catalog conflict';
  END IF;
END;
-- +goose StatementEnd
CALL assert_merchant_rbac_catalog();

INSERT INTO roles (id, code, name, scope, status) VALUES
  (1006, 'merchant_owner', '商家负责人', 'merchant', 'active'),
  (1007, 'merchant_order_operator', '商家接单员', 'merchant', 'active'),
  (1008, 'merchant_inventory_clerk', '商家库存员', 'merchant', 'active')
ON DUPLICATE KEY UPDATE
  name = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(name), name),
  scope = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), 'merchant', scope),
  status = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), 'active', status),
  deleted_at = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), NULL, deleted_at);

INSERT INTO permissions (id, code, resource, action, description, status) VALUES
  (2145, 'merchant_user:update_role', 'merchant_user', 'update_role', '调整商家用户角色', 'active')
ON DUPLICATE KEY UPDATE
  resource = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(resource), resource),
  action = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(action), action),
  description = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(description), description),
  status = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), 'active', status),
  deleted_at = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), NULL, deleted_at);

CALL assert_merchant_rbac_catalog();
DROP PROCEDURE assert_merchant_rbac_catalog;

ALTER TABLE merchant_users
  ADD COLUMN role_id BIGINT UNSIGNED NULL AFTER merchant_id;

-- 现有用户在此迁移前均以负责人身份操作，因此负责人是唯一保持兼容的回填值。
-- 新应用写入必须始终提供已校验的 role_id；该列刻意不设置默认值。
UPDATE merchant_users SET role_id = 1006 WHERE role_id IS NULL;

ALTER TABLE merchant_users
  MODIFY COLUMN role_id BIGINT UNSIGNED NOT NULL,
  ADD KEY idx_role_status (role_id, status);

-- RBAC 上线前短期存在的代码路径可能从 Go 结构体持久化零值，
-- 而没有使用数据库默认值。零值为旧版 JWT 保留，因此在访问或刷新验证
-- 开始比较版本前，先规范化这些记录。
UPDATE accounts SET credential_version = 1 WHERE credential_version = 0;

-- 负责人保留原有商户能力集合。针对一期所需的高风险操作，
-- 两个运营角色刻意不重叠：订单操作员不能调整库存，
-- 库存员不能接单或备货。
DELETE FROM role_permissions WHERE role_id IN (1006, 1007, 1008);

INSERT INTO role_permissions (id, role_id, permission_id)
SELECT r.id * 1000 + p.id, r.id, p.id
FROM roles r
JOIN permissions p ON (
  (r.code = 'merchant_owner' AND p.code IN (
    'store_order:list', 'store_order:view', 'store_order:accept', 'store_order:prepare',
    'shop_product:list', 'shop_product:create', 'shop_product:update', 'shop:business_status',
    'inventory:view', 'inventory:adjust',
    'after_sale:list_shop', 'after_sale:view_shop', 'after_sale:review_shop',
    'after_sale:receive_return', 'after_sale:create_replacement',
    'print_setting:view_shop', 'print_setting:update_shop', 'print_setting:test_shop',
    'print_task:list_shop', 'print_task:reprint_shop',
    'delivery_verification:view_shop', 'delivery_incident:view_shop',
    'delivery_return:list_shop', 'delivery_return:view_shop', 'delivery_return:receive_shop'
  ))
  OR (r.code = 'merchant_order_operator' AND p.code IN (
    'store_order:list', 'store_order:view', 'store_order:accept', 'store_order:prepare',
    'print_setting:view_shop', 'print_setting:update_shop', 'print_setting:test_shop',
    'print_task:list_shop', 'print_task:reprint_shop', 'delivery_verification:view_shop'
  ))
  OR (r.code = 'merchant_inventory_clerk' AND p.code IN (
    'inventory:view', 'inventory:adjust'
  ))
)
WHERE r.scope = 'merchant' AND r.deleted_at IS NULL AND p.deleted_at IS NULL
ON DUPLICATE KEY UPDATE deleted_at = NULL, updated_at = CURRENT_TIMESTAMP(3);

-- 角色管理属于管理端开通能力，绝不是商户自助权限。
INSERT INTO role_permissions (id, role_id, permission_id)
SELECT r.id * 1000 + p.id, r.id, p.id
FROM roles r
JOIN permissions p ON p.code = 'merchant_user:update_role'
WHERE r.code IN ('super_admin', 'admin_manager', 'operation')
  AND r.deleted_at IS NULL AND p.deleted_at IS NULL
ON DUPLICATE KEY UPDATE deleted_at = NULL, updated_at = CURRENT_TIMESTAMP(3);

-- +goose Down
DELETE FROM role_permissions
WHERE role_id IN (1006, 1007, 1008) OR permission_id = 2145;

ALTER TABLE merchant_users
  DROP INDEX idx_role_status,
  DROP COLUMN role_id;

DELETE FROM roles
WHERE (id = 1006 AND code = 'merchant_owner')
   OR (id = 1007 AND code = 'merchant_order_operator')
   OR (id = 1008 AND code = 'merchant_inventory_clerk');

DELETE FROM permissions
WHERE id = 2145 AND code = 'merchant_user:update_role';
