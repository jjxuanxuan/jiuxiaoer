-- +goose Up
-- 将四个后台高风险动作从双人审批收敛为具备专用权限的单人直执。
-- 历史审批、提案和审计数据全部保留；仅停用旧权限与未执行的配送审批。

DROP PROCEDURE IF EXISTS assert_single_operator_permission_catalog;
-- +goose StatementBegin
CREATE PROCEDURE assert_single_operator_permission_catalog()
BEGIN
  IF EXISTS (
    SELECT 1
    FROM permissions p
    WHERE
      (p.id = 2048 AND NOT (BINARY p.code = BINARY 'asset_adjustment:create'))
      OR (p.id = 2049 AND NOT (BINARY p.code = BINARY 'asset_adjustment:approve'))
      OR (p.id = 2068 AND NOT (BINARY p.code = BINARY 'delivery:force_complete'))
      OR (p.id = 2143 AND NOT (BINARY p.code = BINARY 'delivery:force_complete_request'))
      OR (p.id = 2144 AND NOT (BINARY p.code = BINARY 'delivery:force_complete_approve'))
      OR (p.id = 2148 AND NOT (BINARY p.code = BINARY 'wine_ticket_exception:resolve'))
      OR (p.id = 2149 AND NOT (BINARY p.code = BINARY 'wine_ticket_exception:review'))
      OR (p.id = 2164 AND NOT (BINARY p.code = BINARY 'wine_ticket_package:publish'))
      OR (LOWER(RTRIM(p.code)) = 'asset_adjustment:create' AND p.id <> 2048)
      OR (LOWER(RTRIM(p.code)) = 'asset_adjustment:approve' AND p.id <> 2049)
      OR (LOWER(RTRIM(p.code)) = 'delivery:force_complete' AND p.id <> 2068)
      OR (LOWER(RTRIM(p.code)) = 'delivery:force_complete_request' AND p.id <> 2143)
      OR (LOWER(RTRIM(p.code)) = 'delivery:force_complete_approve' AND p.id <> 2144)
      OR (LOWER(RTRIM(p.code)) = 'wine_ticket_exception:resolve' AND p.id <> 2148)
      OR (LOWER(RTRIM(p.code)) = 'wine_ticket_exception:review' AND p.id <> 2149)
      OR (LOWER(RTRIM(p.code)) = 'wine_ticket_package:publish' AND p.id <> 2164)
  ) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='single-operator migration blocked: permission id/code catalog conflict';
  END IF;
END;
-- +goose StatementEnd
CALL assert_single_operator_permission_catalog();

INSERT INTO permissions (id, code, resource, action, description, status) VALUES
  (2048, 'asset_adjustment:create', 'asset_adjustment', 'create', '执行人工资产调账', 'active'),
  (2068, 'delivery:force_complete', 'delivery', 'force_complete', '强制完成配送', 'active'),
  (2148, 'wine_ticket_exception:resolve', 'wine_ticket_exception', 'resolve', '处置酒票异常', 'active'),
  (2164, 'wine_ticket_package:publish', 'wine_ticket_package', 'publish', '发布酒票套餐', 'active')
ON DUPLICATE KEY UPDATE
  resource = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(resource), resource),
  action = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(action), action),
  description = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(description), description),
  status = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), 'active', status),
  deleted_at = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), NULL, deleted_at);

UPDATE permissions
SET status = 'inactive', updated_at = CURRENT_TIMESTAMP(3)
WHERE
  (id = 2049 AND BINARY code = BINARY 'asset_adjustment:approve')
  OR (id = 2143 AND BINARY code = BINARY 'delivery:force_complete_request')
  OR (id = 2144 AND BINARY code = BINARY 'delivery:force_complete_approve')
  OR (id = 2149 AND BINARY code = BINARY 'wine_ticket_exception:review');

UPDATE role_permissions rp
JOIN permissions p ON p.id = rp.permission_id
SET rp.deleted_at = COALESCE(rp.deleted_at, CURRENT_TIMESTAMP(3)),
    rp.updated_at = CURRENT_TIMESTAMP(3)
WHERE p.code IN (
  'asset_adjustment:approve',
  'delivery:force_complete_request',
  'delivery:force_complete_approve',
  'wine_ticket_exception:review'
);

INSERT INTO role_permissions (id, role_id, permission_id)
SELECT r.id * 1000 + p.id, r.id, p.id
FROM roles r
JOIN permissions p ON (
  (p.id = 2048 AND p.code = 'asset_adjustment:create' AND r.code IN ('super_admin', 'admin_manager', 'finance'))
  OR (p.id = 2068 AND p.code = 'delivery:force_complete' AND r.code IN ('super_admin', 'admin_manager', 'operation'))
  OR (p.id = 2148 AND p.code = 'wine_ticket_exception:resolve' AND r.code IN ('super_admin', 'admin_manager', 'operation'))
  OR (p.id = 2164 AND p.code = 'wine_ticket_package:publish' AND r.code IN ('super_admin', 'admin_manager', 'operation'))
)
WHERE r.status = 'active'
  AND r.deleted_at IS NULL
ON DUPLICATE KEY UPDATE
  deleted_at = NULL,
  updated_at = CURRENT_TIMESTAMP(3);

UPDATE admin_override_approvals
SET status = 'expired'
WHERE action = 'delivery.force_complete' AND status = 'pending';

-- 现有 permission code 的执行语义已升级，不能让部署前签发的管理员 token
-- 在代码切换后静默获得更强的直执能力。
UPDATE accounts
SET credential_version = credential_version + 1,
    token_invalid_before = CURRENT_TIMESTAMP(3),
    updated_at = CURRENT_TIMESTAMP(3)
WHERE account_type = 'admin'
  AND status = 'active'
  AND deleted_at IS NULL;

CALL assert_single_operator_permission_catalog();
DROP PROCEDURE assert_single_operator_permission_catalog;

-- +goose Down
-- 只恢复权限目录与默认角色映射；Up 中已过期的历史审批不会重新激活。

UPDATE permissions
SET status = 'active', updated_at = CURRENT_TIMESTAMP(3)
WHERE
  (id = 2049 AND BINARY code = BINARY 'asset_adjustment:approve')
  OR (id = 2143 AND BINARY code = BINARY 'delivery:force_complete_request')
  OR (id = 2144 AND BINARY code = BINARY 'delivery:force_complete_approve')
  OR (id = 2149 AND BINARY code = BINARY 'wine_ticket_exception:review');

UPDATE permissions
SET description = '创建资产调账', updated_at = CURRENT_TIMESTAMP(3)
WHERE id = 2048 AND BINARY code = BINARY 'asset_adjustment:create';

UPDATE permissions
SET description = '提交酒票异常处置提案', updated_at = CURRENT_TIMESTAMP(3)
WHERE id = 2148 AND BINARY code = BINARY 'wine_ticket_exception:resolve';

UPDATE role_permissions rp
JOIN roles r ON r.id = rp.role_id
SET rp.deleted_at = COALESCE(rp.deleted_at, CURRENT_TIMESTAMP(3)),
    rp.updated_at = CURRENT_TIMESTAMP(3)
WHERE rp.permission_id = 2068 AND r.code IN ('admin_manager', 'operation');

INSERT INTO role_permissions (id, role_id, permission_id)
SELECT r.id * 1000 + p.id, r.id, p.id
FROM roles r
JOIN permissions p ON (
  (p.id = 2143 AND p.code = 'delivery:force_complete_request' AND r.code IN ('super_admin', 'operation'))
  OR (p.id = 2144 AND p.code = 'delivery:force_complete_approve' AND r.code IN ('super_admin', 'admin_manager'))
  OR (p.id = 2049 AND p.code = 'asset_adjustment:approve' AND r.code IN ('super_admin', 'admin_manager', 'finance'))
  OR (p.id = 2149 AND p.code = 'wine_ticket_exception:review' AND r.code IN ('super_admin', 'admin_manager', 'operation'))
)
WHERE r.status = 'active' AND r.deleted_at IS NULL
ON DUPLICATE KEY UPDATE
  deleted_at = NULL,
  updated_at = CURRENT_TIMESTAMP(3);

-- 回切会恢复旧审批语义；再次吊销管理员 token，防止单人模式下签发的
-- permission 快照跨越语义边界继续使用。
UPDATE accounts
SET credential_version = credential_version + 1,
    token_invalid_before = CURRENT_TIMESTAMP(3),
    updated_at = CURRENT_TIMESTAMP(3)
WHERE account_type = 'admin'
  AND status = 'active'
  AND deleted_at IS NULL;
