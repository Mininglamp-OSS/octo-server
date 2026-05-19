-- +migrate Up

-- system_setting: admin 可调全局配置 KV 表
-- 设计要点：
--   1. 与旧 app_config 表 keyspace 不重叠；新增 admin 可调字段一律走本表，无需 ALTER TABLE。
--   2. value 统一字符串存储；helper 层按 value_type 解析。空串视为"未配置"，调用方回落 yaml 默认值。
--   3. value_type='encrypted' 的字段在 helper 写入侧用 AES-256-GCM (encryptKey) 加密，
--      读取侧 decryptKey 解密；解密失败回落 yaml 默认值。
CREATE TABLE `system_setting` (
    `id`          BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    `category`    VARCHAR(64)     NOT NULL COMMENT '分类，例如 register / support',
    `key_name`    VARCHAR(128)    NOT NULL COMMENT 'snake_case 键名',
    `value`       TEXT            NOT NULL COMMENT '统一字符串存储；bool 用 "1"/"0"；空串=未配置',
    `value_type`  VARCHAR(16)     NOT NULL DEFAULT 'string' COMMENT 'string / bool / int / encrypted',
    `description` VARCHAR(255)    NOT NULL DEFAULT '',
    `created_at`  TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updated_at`  TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_category_key` (`category`, `key_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='系统级 admin 可调配置 KV 表';

-- +migrate Down

DROP TABLE `system_setting`;
