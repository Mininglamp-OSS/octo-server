-- +migrate Up
-- bot_setting: Bot 级配置稀疏覆盖表。维度 (robot_id, key_name) → value。
--
-- 只存「偏离服务端全局默认」的覆盖项；无行或空串 value = 未配置，读取方回退
-- system_setting 全局默认，再回退代码默认（与 bot_mention_pref 的稀疏语义同口径）。
-- 因此「删除覆盖」== 回落上一层，而不是「设为 false」。
--
-- 存在的理由：本仓此前的一维 Bot 配置一律是给 `robot` 表加一列（inline_on /
-- auto_approve / placeholder / bot_commands …），每加一个开关一次 ALTER，滚动发布
-- 还要手写并发守卫。本表把后续 Bot 级配置收敛到 KV，新增配置项不再需要 DDL。
--
-- value 统一字符串存储（bool 用 "1"/"0"），与 system_setting 同口径；类型与合法
-- 取值由服务端注册表（modules/robot/bot_setting.go 的 botSettingDefs）校验，DB 不做
-- 类型约束——注册表是单一权威，DB 加 CHECK 只会与它漂移。
CREATE TABLE IF NOT EXISTS `bot_setting` (
  `id`         BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `robot_id`   VARCHAR(40)     NOT NULL DEFAULT '' COMMENT 'Bot UID',
  `key_name`   VARCHAR(128)    NOT NULL DEFAULT '' COMMENT '注册表键名，如 bot.reasoning_enabled',
  `value`      VARCHAR(1024)   NOT NULL DEFAULT '' COMMENT '统一字符串存储；bool 用 "1"/"0"；空串=未配置',
  `created_at` TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP       NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  `updated_by` VARCHAR(40)     NOT NULL DEFAULT '' COMMENT '最后修改人 UID（审计）',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_robot_key` (`robot_id`, `key_name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Bot 级配置稀疏覆盖表';

-- +migrate Down
DROP TABLE IF EXISTS `bot_setting`;
