-- +migrate Up
-- RETAINED after the #484 revert (PR #529). The #484 code that read/wrote this
-- table is gone, but the migration FILE must stay: #484 shipped to production,
-- so `20260627000001_dm_space_presence.sql` is recorded in `gorp_migrations` on
-- every upgraded DB. sql-migrate runs with IgnoreUnknown=false, so deleting an
-- already-applied migration file makes startup panic with
-- "unknown migration in database" (see pkg/db/migrate_compat.go and the
-- 20260614000001_searchetl_init.sql retained-no-op precedent). The CREATE below
-- is idempotent (IF NOT EXISTS) and DB-neutral; the table is now an unused
-- orphan (no code references it) — harmless. A later forward migration may DROP
-- it if desired.
--
-- Original #484 intent (no longer active): DM (Person) per-Space presence index,
-- keyed by common.GetFakeChannelIDWith(uidA, uidB), written at the WuKongIM
-- message webhook and read by the conversation Space filter.
CREATE TABLE IF NOT EXISTS `dm_space_presence` (
  `fake_channel_id` VARCHAR(100) NOT NULL COMMENT 'common.GetFakeChannelIDWith 规范化的 DM 对 ID',
  `space_id`        VARCHAR(40)  NOT NULL COMMENT '该 DM 出现过消息的 Space ID',
  `last_timestamp`  BIGINT       NOT NULL DEFAULT 0 COMMENT '该 Space 下最后一条消息的服务器时间戳(秒)',
  `created_at`      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`fake_channel_id`, `space_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='DM 频道-Space 存在性索引(issue #484，#529 revert 后保留为孤儿表)';

-- +migrate Down
DROP TABLE IF EXISTS `dm_space_presence`;
