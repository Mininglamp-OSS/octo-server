-- +migrate Up

-- user_conversation_ext 的两个查询级索引（issue #337, PR review Round-3 follow-up）。
--
-- 拆为独立 migration 是因为：sql-migrate 按 version 时间戳追踪是否已执行，
-- 任何在 20260513000001 文件内容更新前就已运行过该 migration 的
-- dev/staging 数据库都不会重跑它。把索引放到一个新的 timestamped 文件，
-- 加上 DROP INDEX IF EXISTS（MySQL 8 原生语法）做幂等保护，
-- 既能在干净环境一次性创建，也能让脏环境再次 migrate 时收敛。

-- idx_unfollowed_group 支持 ListUnfollowedGroups:
--   WHERE uid=? AND space_id=? AND target_type=2 AND group_unfollowed=1
ALTER TABLE user_conversation_ext DROP INDEX IF EXISTS idx_unfollowed_group;
CREATE INDEX idx_unfollowed_group ON user_conversation_ext (uid, space_id, target_type, group_unfollowed);

-- idx_thread_sort 支持 ListThreadExts:
--   WHERE uid=? AND space_id=? AND target_type=5 ORDER BY follow_sort
ALTER TABLE user_conversation_ext DROP INDEX IF EXISTS idx_thread_sort;
CREATE INDEX idx_thread_sort ON user_conversation_ext (uid, space_id, target_type, follow_sort);

-- +migrate Down

ALTER TABLE user_conversation_ext DROP INDEX IF EXISTS idx_thread_sort;
ALTER TABLE user_conversation_ext DROP INDEX IF EXISTS idx_unfollowed_group;
