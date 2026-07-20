-- +goose Up
-- Rider authentication is phone/SMS-only. Backfill legacy rider accounts from
-- the rider profile, remove dormant password credentials, and revoke sessions
-- issued under the legacy credential model.
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
-- Legacy usernames and password hashes are intentionally not recoverable.
-- Rolling application code back does not recreate password credentials.
SELECT 1;
