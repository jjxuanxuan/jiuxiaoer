-- +goose Up
-- 完成对历史写入器的结构化回填；这些写入器序列化的是 Go 结构体
-- （CamelCase），而不是映射（snake_case）。
UPDATE audit_logs
SET
  before_status = COALESCE(
    before_status,
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.status')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.Status')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.order_status')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.OrderStatus')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.delivery_status')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.DeliveryStatus')), 'null')
  ),
  after_status = COALESCE(
    after_status,
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.status')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.Status')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.order_status')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.OrderStatus')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.delivery_status')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.DeliveryStatus')), 'null')
  ),
  error_code = COALESCE(
    error_code,
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.error_code')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.ErrorCode')), 'null')
  ),
  reason_code = COALESCE(
    reason_code,
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.reason_code')), 'null'),
    NULLIF(JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.ReasonCode')), 'null')
  ),
  shop_id = COALESCE(
    shop_id,
    CASE WHEN resource_type = 'shop' THEN resource_id END,
    CASE
      WHEN COALESCE(
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.shop_id')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.ShopID')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.shop_id')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.ShopID'))
      ) REGEXP '^[0-9]+$'
      THEN CAST(COALESCE(
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.shop_id')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.ShopID')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.shop_id')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.ShopID'))
      ) AS UNSIGNED)
    END
  ),
  order_id = COALESCE(
    order_id,
    CASE WHEN resource_type = 'order' THEN resource_id END,
    CASE
      WHEN COALESCE(
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.order_id')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.OrderID')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.order_id')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.OrderID'))
      ) REGEXP '^[0-9]+$'
      THEN CAST(COALESCE(
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.order_id')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.OrderID')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.order_id')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.OrderID'))
      ) AS UNSIGNED)
    END
  ),
  delivery_id = COALESCE(
    delivery_id,
    CASE WHEN resource_type IN ('delivery_order', 'delivery_verification') THEN resource_id END,
    CASE
      WHEN COALESCE(
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.delivery_id')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.DeliveryID')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.delivery_order_id')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.DeliveryOrderID')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.delivery_id')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.DeliveryID')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.delivery_order_id')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.DeliveryOrderID'))
      ) REGEXP '^[0-9]+$'
      THEN CAST(COALESCE(
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.delivery_id')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.DeliveryID')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.delivery_order_id')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.DeliveryOrderID')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.delivery_id')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.DeliveryID')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.delivery_order_id')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.DeliveryOrderID'))
      ) AS UNSIGNED)
    END
  ),
  version = COALESCE(
    version,
    CASE
      WHEN COALESCE(
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.version')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.Version')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.assignment_version')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.AssignmentVersion')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.version')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.Version'))
      ) REGEXP '^[0-9]+$'
      THEN CAST(COALESCE(
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.version')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.Version')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.assignment_version')),
        JSON_UNQUOTE(JSON_EXTRACT(after_data, '$.AssignmentVersion')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.version')),
        JSON_UNQUOTE(JSON_EXTRACT(before_data, '$.Version'))
      ) AS UNSIGNED)
    END
  );

-- 从权威业务表填充关系维度。这些关联刻意允许为空，
-- 使已删除的旧版主体仍可报告，而不是导致迁移失败。
UPDATE audit_logs a
LEFT JOIN delivery_orders d ON d.id = a.delivery_id
LEFT JOIN orders o ON o.id = COALESCE(a.order_id, d.order_id)
LEFT JOIN shop_products sp ON a.resource_type = 'shop_product' AND sp.id = a.resource_id
LEFT JOIN customers c ON a.actor_type = 'customer' AND c.id = a.actor_id
LEFT JOIN admin_users au ON a.actor_type = 'admin' AND au.id = a.actor_id
LEFT JOIN merchant_users mu ON a.actor_type = 'merchant' AND mu.id = a.actor_id
LEFT JOIN riders r ON a.actor_type = 'rider' AND r.id = a.actor_id
SET
  a.order_id = COALESCE(a.order_id, d.order_id),
  a.shop_id = COALESCE(a.shop_id, d.shop_id, o.shop_id, sp.shop_id),
  a.account_id = COALESCE(
    a.account_id,
    CASE
      WHEN a.resource_type = 'account' THEN a.resource_id
      WHEN a.actor_type = 'rider_application_applicant' THEN a.actor_id
      WHEN a.actor_type = 'customer' THEN c.account_id
      WHEN a.actor_type = 'admin' THEN au.account_id
      WHEN a.actor_type = 'merchant' THEN mu.account_id
      WHEN a.actor_type = 'rider' THEN r.account_id
    END
  );

-- 历史完整领域快照可能包含客户联系方式、精确坐标、验证值，
-- 或不受限制的操作人员与服务商文本。原地移除这些键，
-- 同时保留调查所需的受控状态、ID 以及稳定的错误码和原因码。
UPDATE audit_logs
SET
  before_data = CASE WHEN before_data IS NULL THEN NULL ELSE JSON_REMOVE(before_data,
    '$.AddressSnapshot', '$.address_snapshot', '$.RecipientSnapshot', '$.recipient_snapshot',
    '$.PickupSnapshot', '$.pickup_snapshot', '$.ContactPhone', '$.contact_phone',
    '$.Phone', '$.phone', '$.Mobile', '$.mobile', '$.ContactName', '$.contact_name',
    '$.RealName', '$.real_name', '$.Address', '$.address', '$.AddressDetail', '$.address_detail',
    '$.FormattedAddress', '$.formatted_address', '$.Doorplate', '$.doorplate',
    '$.Latitude', '$.latitude', '$.Longitude', '$.longitude', '$.LicenseNo', '$.license_no',
    '$.ProviderSubject', '$.provider_subject', '$.SubjectReference', '$.subject_reference',
    '$.Code', '$.code', '$.PickupCode', '$.pickup_code', '$.DeliveryCode', '$.delivery_code',
    '$.CodeHash', '$.code_hash', '$.CodeCiphertext', '$.code_ciphertext',
    '$.Reason', '$.reason', '$.Remark', '$.remark', '$.ReviewRemark', '$.review_remark',
    '$.Detail', '$.detail', '$.FailureDetail', '$.failure_detail', '$.LastErrorSafe',
    '$.StatusReasonSafe', '$.Description', '$.description', '$.Message', '$.message',
    '$.Secret', '$.secret', '$.Token', '$.token', '$.APIKey', '$.api_key', '$.PrivateKey', '$.private_key'
  ) END,
  after_data = CASE WHEN after_data IS NULL THEN NULL ELSE JSON_REMOVE(after_data,
    '$.AddressSnapshot', '$.address_snapshot', '$.RecipientSnapshot', '$.recipient_snapshot',
    '$.PickupSnapshot', '$.pickup_snapshot', '$.ContactPhone', '$.contact_phone',
    '$.Phone', '$.phone', '$.Mobile', '$.mobile', '$.ContactName', '$.contact_name',
    '$.RealName', '$.real_name', '$.Address', '$.address', '$.AddressDetail', '$.address_detail',
    '$.FormattedAddress', '$.formatted_address', '$.Doorplate', '$.doorplate',
    '$.Latitude', '$.latitude', '$.Longitude', '$.longitude', '$.LicenseNo', '$.license_no',
    '$.ProviderSubject', '$.provider_subject', '$.SubjectReference', '$.subject_reference',
    '$.Code', '$.code', '$.PickupCode', '$.pickup_code', '$.DeliveryCode', '$.delivery_code',
    '$.CodeHash', '$.code_hash', '$.CodeCiphertext', '$.code_ciphertext',
    '$.Reason', '$.reason', '$.Remark', '$.remark', '$.ReviewRemark', '$.review_remark',
    '$.Detail', '$.detail', '$.FailureDetail', '$.failure_detail', '$.LastErrorSafe',
    '$.StatusReasonSafe', '$.Description', '$.description', '$.Message', '$.message',
    '$.Secret', '$.secret', '$.Token', '$.token', '$.APIKey', '$.api_key', '$.PrivateKey', '$.private_key'
  ) END;

-- +goose Down
-- 隐私清理刻意不可逆。即使回滚此迁移，结构化值仍然有效，
-- 因此不执行破坏性的向下迁移操作。
SELECT 1;
