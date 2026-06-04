-- +migrate Up
-- Add keyed verifier hashes for integration uk_ lookup and index the hash used
-- by AuthByKey. Integration writes store only a non-bearer marker in api_key
-- plus encrypted api_key_cipher for idempotent echo. Legacy plaintext
-- integration rows are converted on first successful touch because the verifier
-- is keyed by OCTO_USER_API_KEY_SECRET and cannot be safely backfilled in SQL.

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_user_api_key_hash;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __botfather_user_api_key_hash()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key' AND COLUMN_NAME = 'api_key_hash') THEN
    ALTER TABLE `user_api_key`
      ADD COLUMN `api_key_hash` varchar(64) NOT NULL DEFAULT '' COMMENT 'HMAC-SHA256 verifier for integration uk_ auth lookup';
  ELSE
    ALTER TABLE `user_api_key`
      MODIFY COLUMN `api_key_hash` varchar(64) NOT NULL DEFAULT '' COMMENT 'HMAC-SHA256 verifier for integration uk_ auth lookup';
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key' AND COLUMN_NAME = 'api_key_cipher') THEN
    ALTER TABLE `user_api_key`
      ADD COLUMN `api_key_cipher` varchar(255) NOT NULL DEFAULT '' COMMENT 'AES-GCM encrypted integration uk_ plaintext for idempotent echo';
  ELSE
    ALTER TABLE `user_api_key`
      MODIFY COLUMN `api_key_cipher` varchar(255) NOT NULL DEFAULT '' COMMENT 'AES-GCM encrypted integration uk_ plaintext for idempotent echo';
  END IF;

  IF NOT EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND INDEX_NAME = 'idx_api_key_hash') THEN
    ALTER TABLE `user_api_key`
      ADD INDEX `idx_api_key_hash` (`api_key_hash`);
  END IF;
END;
-- +migrate StatementEnd

CALL __botfather_user_api_key_hash();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_user_api_key_hash;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_user_api_key_hash_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __botfather_user_api_key_hash_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.STATISTICS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'user_api_key'
         AND INDEX_NAME = 'idx_api_key_hash') THEN
    ALTER TABLE `user_api_key` DROP INDEX `idx_api_key_hash`;
  END IF;
END;
-- +migrate StatementEnd

CALL __botfather_user_api_key_hash_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_user_api_key_hash_down;
-- +migrate StatementEnd
