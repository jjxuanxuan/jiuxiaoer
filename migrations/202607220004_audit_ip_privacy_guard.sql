-- +goose Up
-- 为兼容回滚和读取，原始列暂时保留；但数据库现在会拒绝任何绕过
-- Go 审计不变量并试图以明文持久化客户端 IP 的写入器。
ALTER TABLE audit_logs
  ADD CONSTRAINT chk_audit_ip_redacted CHECK (ip IS NULL),
  ADD CONSTRAINT chk_audit_ip_hash_format CHECK (
    ip_hash IS NULL OR REGEXP_LIKE(ip_hash, '^[0-9a-f]{64}$', 'c')
  );

-- +goose Down
ALTER TABLE audit_logs
  DROP CHECK chk_audit_ip_hash_format,
  DROP CHECK chk_audit_ip_redacted;
