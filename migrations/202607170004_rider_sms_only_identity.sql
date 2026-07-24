-- +goose Up
-- 骑手只使用手机号和短信认证。从骑手资料回填旧版骑手账户，
-- 移除停用的密码凭据，并吊销旧凭据模型下签发的会话。
UPDATE accounts a
JOIN riders r ON r.account_id = a.id AND r.deleted_at IS NULL
SET a.phone = r.phone,
    a.updated_at = CURRENT_TIMESTAMP(3)
WHERE a.account_type = 'rider'
  AND a.deleted_at IS NULL
  AND r.phone IS NOT NULL
  AND r.phone <> '';

UPDATE accounts
SET username = NULL,
    password_hash = NULL,
    token_invalid_before = CURRENT_TIMESTAMP(3),
    credential_version = credential_version + 1,
    updated_at = CURRENT_TIMESTAMP(3)
WHERE account_type = 'rider'
  AND deleted_at IS NULL;

-- +goose Down
-- 旧版用户名和密码哈希刻意不可恢复。
-- 回滚应用代码不会重新创建密码凭据。
SELECT 1;
