-- +migrate Up

-- thread_user_state：子区「按用户」的可见性状态（per-user thread visibility）。
--
-- 背景（bug）：子区自动归档是「全局」thread.status，一人手工归档 / worker 陈旧归档
-- 会波及所有成员，且未处理的 per-uid @（P1）无法把子区强制拉回可见。本表承载
-- 四级仲裁 MUTE > P1 > P2 > P3 中的 **P2（本人手工归档意图）**，与已 per-user 的
-- thread_setting（mute，MUTE 级）语义对齐，让归档从「全局字段」升为「per-user 正源」。
--
-- 空表 = 现状：仲裁读路径查不到行即回落全局 thread.status（P3 兜底），迁移向后兼容、
-- 无需回填（feature flag DM_THREAD_PERUSER_VISIBILITY 默认 off 时本表根本不写不读）。
--
-- 索引：
--   uk_uid_thread (uid, group_no, short_id) —— 仲裁按「一个 uid × 多 thread」批量定位
--     （P1 SQL / mute 批量 / 本表批量共用同一 refs），uid 打头。
--   idx_thread (group_no, short_id) —— DeleteThread / GC 按 thread 反查清理孤儿行。
--
-- read_intent_at 为未来把 mute/read 并入本表预留列位（本批不写，见 plan out-of-scope）。
CREATE TABLE `thread_user_state` (
  `id` BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `uid` VARCHAR(40) NOT NULL,
  `group_no` VARCHAR(40) NOT NULL,
  `short_id` VARCHAR(32) NOT NULL,
  `archive_intent` TINYINT NOT NULL DEFAULT 0,      -- 0=未归档 1=已归档(本人)
  `archive_intent_at` TIMESTAMP NULL,
  `read_intent_at` TIMESTAMP NULL,                  -- 预留(mute/read 未来并入)
  `version` BIGINT NOT NULL DEFAULT 0,
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_uid_thread` (`uid`, `group_no`, `short_id`),   -- 仲裁按 uid+thread 定位
  KEY `idx_thread` (`group_no`, `short_id`)                     -- 删除/GC 按 thread 反查
) ENGINE=InnoDB;

-- +migrate Down
DROP TABLE `thread_user_state`;
