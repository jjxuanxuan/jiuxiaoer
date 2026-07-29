-- 酒票权限目录。ID 2146-2186 紧接当前受控权限目录。
-- +goose Up

DROP PROCEDURE IF EXISTS assert_wine_ticket_permission_catalog;
-- +goose StatementBegin
CREATE PROCEDURE assert_wine_ticket_permission_catalog()
BEGIN
  IF EXISTS (
    SELECT 1
    FROM permissions p
    WHERE
      (p.id BETWEEN 2146 AND 2186 AND p.code NOT LIKE 'wine_ticket_%')
      OR (p.code LIKE 'wine_ticket_%' AND p.id NOT BETWEEN 2146 AND 2186)
  ) THEN
    SIGNAL SQLSTATE '45000'
      SET MESSAGE_TEXT='wine-ticket RBAC migration blocked: permission catalog conflict';
  END IF;
END;
-- +goose StatementEnd
CALL assert_wine_ticket_permission_catalog();

INSERT INTO permissions (id, code, resource, action, description, status) VALUES
  (2146, 'wine_ticket_cabinet:view', 'wine_ticket_cabinet', 'view', '查看本人私人酒柜', 'active'),
  (2147, 'wine_ticket_exception:list', 'wine_ticket_exception', 'list', '查询酒票异常队列', 'active'),
  (2148, 'wine_ticket_exception:resolve', 'wine_ticket_exception', 'resolve', '提交酒票异常处置提案', 'active'),
  (2149, 'wine_ticket_exception:review', 'wine_ticket_exception', 'review', '复核酒票异常处置提案', 'active'),
  (2150, 'wine_ticket_exception:view', 'wine_ticket_exception', 'view', '查看酒票异常详情', 'active'),
  (2151, 'wine_ticket_gift:cancel', 'wine_ticket_gift', 'cancel', '取消本人待领取赠礼', 'active'),
  (2152, 'wine_ticket_gift:claim', 'wine_ticket_gift', 'claim', '领取酒票赠礼', 'active'),
  (2153, 'wine_ticket_gift:create', 'wine_ticket_gift', 'create', '创建酒票赠礼', 'active'),
  (2154, 'wine_ticket_gift:list', 'wine_ticket_gift', 'list', '查询本人赠礼记录', 'active'),
  (2155, 'wine_ticket_gift:share', 'wine_ticket_gift', 'share', '签发赠礼分享令牌', 'active'),
  (2156, 'wine_ticket_gift:view', 'wine_ticket_gift', 'view', '查看本人赠礼详情', 'active'),
  (2157, 'wine_ticket_lot:list', 'wine_ticket_lot', 'list', '查询本人酒票批次', 'active'),
  (2158, 'wine_ticket_lot:list_all', 'wine_ticket_lot', 'list_all', '查询全局酒票批次脱敏投影', 'active'),
  (2159, 'wine_ticket_lot:view', 'wine_ticket_lot', 'view', '查看本人酒票批次详情', 'active'),
  (2160, 'wine_ticket_notification_consent:create', 'wine_ticket_notification_consent', 'create', '记录本人订阅授权', 'active'),
  (2161, 'wine_ticket_notification_consent:view', 'wine_ticket_notification_consent', 'view', '查看本人订阅授权', 'active'),
  (2162, 'wine_ticket_package:create', 'wine_ticket_package', 'create', '创建酒票套餐草稿', 'active'),
  (2163, 'wine_ticket_package:list', 'wine_ticket_package', 'list', '查询酒票套餐管理列表', 'active'),
  (2164, 'wine_ticket_package:publish', 'wine_ticket_package', 'publish', '发布酒票套餐', 'active'),
  (2165, 'wine_ticket_package:update', 'wine_ticket_package', 'update', '更新酒票套餐草稿', 'active'),
  (2166, 'wine_ticket_package:view', 'wine_ticket_package', 'view', '查看酒票套餐完整配置', 'active'),
  (2167, 'wine_ticket_payment:confirm', 'wine_ticket_payment', 'confirm', '确认本人酒票支付结果', 'active'),
  (2168, 'wine_ticket_purchase:create', 'wine_ticket_purchase', 'create', '创建本人酒票购买', 'active'),
  (2169, 'wine_ticket_purchase:list', 'wine_ticket_purchase', 'list', '查询本人酒票购买', 'active'),
  (2170, 'wine_ticket_purchase:list_all', 'wine_ticket_purchase', 'list_all', '查询全局酒票购买脱敏投影', 'active'),
  (2171, 'wine_ticket_purchase:view', 'wine_ticket_purchase', 'view', '查看本人酒票购买详情', 'active'),
  (2172, 'wine_ticket_redemption:cancel', 'wine_ticket_redemption', 'cancel', '取消本人可取消提酒', 'active'),
  (2173, 'wine_ticket_redemption:create', 'wine_ticket_redemption', 'create', '创建本人提酒', 'active'),
  (2174, 'wine_ticket_redemption:list', 'wine_ticket_redemption', 'list', '查询本人提酒记录', 'active'),
  (2175, 'wine_ticket_redemption:view', 'wine_ticket_redemption', 'view', '查看本人提酒详情', 'active'),
  (2176, 'wine_ticket_refund:create', 'wine_ticket_refund', 'create', '申请本人未使用酒票退款', 'active'),
  (2177, 'wine_ticket_refund:quote', 'wine_ticket_refund', 'quote', '查看本人酒票退款报价', 'active'),
  (2178, 'wine_ticket_refund:view', 'wine_ticket_refund', 'view', '查看本人酒票退款进度', 'active'),
  (2179, 'wine_ticket_renewal:create', 'wine_ticket_renewal', 'create', '创建本人酒票续期', 'active'),
  (2180, 'wine_ticket_renewal:quote', 'wine_ticket_renewal', 'quote', '查看本人酒票续期报价', 'active'),
  (2181, 'wine_ticket_renewal:view', 'wine_ticket_renewal', 'view', '查看本人酒票续期结果', 'active'),
  (2182, 'wine_ticket_slot:create', 'wine_ticket_slot', 'create', '创建酒票配送时段', 'active'),
  (2183, 'wine_ticket_slot:list', 'wine_ticket_slot', 'list', '查询酒票配送时段', 'active'),
  (2184, 'wine_ticket_slot:update', 'wine_ticket_slot', 'update', '更新酒票配送时段', 'active'),
  (2185, 'wine_ticket_transaction:list', 'wine_ticket_transaction', 'list', '查询本人酒票权益流水', 'active'),
  (2186, 'wine_ticket_package:unpublish', 'wine_ticket_package', 'unpublish', '下架酒票套餐', 'active')
ON DUPLICATE KEY UPDATE
  resource = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(resource), resource),
  action = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(action), action),
  description = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), VALUES(description), description),
  status = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), 'active', status),
  deleted_at = IF(id = VALUES(id) AND BINARY code = BINARY VALUES(code), NULL, deleted_at);

CALL assert_wine_ticket_permission_catalog();
DROP PROCEDURE assert_wine_ticket_permission_catalog;

-- 平台管理员拥有全部酒票后台权限；运营负责套餐、时段和异常；
-- 财务/客服只拿全局脱敏查询，异常复核与写操作不下放。
INSERT INTO role_permissions (id, role_id, permission_id)
SELECT r.id * 1000 + p.id, r.id, p.id
FROM roles r
JOIN permissions p ON (
  (r.code IN ('super_admin','admin_manager') AND p.code LIKE 'wine_ticket_%')
  OR (
    r.code = 'operation' AND p.code IN (
      'wine_ticket_package:list','wine_ticket_package:view','wine_ticket_package:create',
      'wine_ticket_package:update','wine_ticket_package:publish',
      'wine_ticket_package:unpublish',
      'wine_ticket_purchase:list_all','wine_ticket_lot:list_all',
      'wine_ticket_slot:list','wine_ticket_slot:create','wine_ticket_slot:update',
      'wine_ticket_exception:list','wine_ticket_exception:view',
      'wine_ticket_exception:resolve','wine_ticket_exception:review'
    )
  )
  OR (
    r.code = 'finance' AND p.code IN (
      'wine_ticket_purchase:list_all','wine_ticket_lot:list_all',
      'wine_ticket_exception:list','wine_ticket_exception:view'
    )
  )
  OR (
    r.code = 'customer_service' AND p.code IN (
      'wine_ticket_purchase:list_all','wine_ticket_lot:list_all'
    )
  )
)
WHERE r.deleted_at IS NULL AND p.id BETWEEN 2146 AND 2186
ON DUPLICATE KEY UPDATE deleted_at = NULL, updated_at = CURRENT_TIMESTAMP(3);

-- +goose Down
DELETE FROM role_permissions WHERE permission_id BETWEEN 2146 AND 2186;
DELETE FROM permissions WHERE id BETWEEN 2146 AND 2186 AND code LIKE 'wine_ticket_%';
