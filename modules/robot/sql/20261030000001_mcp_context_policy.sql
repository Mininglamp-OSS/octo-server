-- +migrate Up
CREATE TABLE IF NOT EXISTS `mcp_policy_default` (
  `singleton_id` TINYINT UNSIGNED NOT NULL,
  `context_mode` VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `generation` BIGINT UNSIGNED NOT NULL,
  `updated_by` VARCHAR(40) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
  `created_at` TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`singleton_id`),
  CONSTRAINT `chk_mcp_default_singleton` CHECK (`singleton_id` = 1),
  CONSTRAINT `chk_mcp_default_mode` CHECK (`context_mode` IN ('strict','compatible')),
  CONSTRAINT `chk_mcp_generation` CHECK (`generation` >= 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS `mcp_bot_policy` (
  `bot_uid` VARCHAR(40) COLLATE utf8mb4_bin NOT NULL,
  `context_mode` VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
  `revision` BIGINT UNSIGNED NOT NULL,
  `updated_by` VARCHAR(40) COLLATE utf8mb4_bin NOT NULL,
  `created_at` TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
  `updated_at` TIMESTAMP(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
  PRIMARY KEY (`bot_uid`),
  CONSTRAINT `chk_mcp_bot_uid` CHECK (OCTET_LENGTH(`bot_uid`) BETWEEN 1 AND 40),
  CONSTRAINT `chk_mcp_bot_mode` CHECK (`context_mode` IN ('strict','compatible')),
  CONSTRAINT `chk_mcp_bot_revision` CHECK (`revision` >= 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

INSERT INTO `mcp_policy_default` (`singleton_id`,`context_mode`,`generation`,`updated_by`)
VALUES (1,'compatible',1,'')
ON DUPLICATE KEY UPDATE `singleton_id` = `singleton_id`;

-- +migrate Down
-- Stop production-policy consumers under a separately approved rollback first.
-- Refuse destruction after any edit or while any explicit override exists.
INSERT INTO `mcp_policy_default` (`singleton_id`,`context_mode`,`generation`,`updated_by`)
SELECT 2,'compatible',1,''
WHERE EXISTS (SELECT 1 FROM `mcp_bot_policy`)
   OR EXISTS (SELECT 1 FROM `mcp_policy_default`
              WHERE `generation` <> 1 OR `context_mode` <> 'compatible');
DROP TABLE IF EXISTS `mcp_bot_policy`;
DROP TABLE IF EXISTS `mcp_policy_default`;
