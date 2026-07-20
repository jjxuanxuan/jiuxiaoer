-- +goose Up
ALTER TABLE outbox_events
  ADD COLUMN locked_by VARCHAR(128) NULL AFTER next_retry_at,
  ADD COLUMN locked_until DATETIME(3) NULL AFTER locked_by,
  ADD COLUMN last_error_code VARCHAR(64) NULL AFTER locked_until,
  ADD COLUMN last_error_detail VARCHAR(512) NULL AFTER last_error_code,
  ADD KEY idx_claim (status, next_retry_at, locked_until, id);

-- +goose Down
ALTER TABLE outbox_events
  DROP INDEX idx_claim,
  DROP COLUMN last_error_detail,
  DROP COLUMN last_error_code,
  DROP COLUMN locked_until,
  DROP COLUMN locked_by;
