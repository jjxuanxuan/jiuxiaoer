-- +goose Up
-- The raw column remains temporarily for rollback/read compatibility, but the
-- database now rejects any writer that bypasses the Go audit invariant and
-- attempts to persist a client IP in plaintext.
ALTER TABLE audit_logs
  ADD CONSTRAINT chk_audit_ip_redacted CHECK (ip IS NULL),
  ADD CONSTRAINT chk_audit_ip_hash_format CHECK (
    ip_hash IS NULL OR REGEXP_LIKE(ip_hash, '^[0-9a-f]{64}$', 'c')
  );

-- +goose Down
ALTER TABLE audit_logs
  DROP CHECK chk_audit_ip_hash_format,
  DROP CHECK chk_audit_ip_redacted;
