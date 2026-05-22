-- +migrate Up

-- 关注频道时自动连带关注其下子区（issue: sidebar channel→threads 级联关注）。
-- - auto_follow_threads=1 表示用户已对该群按下"关注频道"，新建子区时同步给该用户落 thread ext 行。
-- - FollowChannel 把字段置 1 并物化当前 active 子区；UnfollowChannel 置 0 并级联清空 thread 行。
-- - 默认 0，对未上线 follow tab 的老用户无影响（无回填需求）。
--
-- idx_channel_auto_follow 服务于 OnThreadCreated 的 fanout 查询：
--   WHERE target_type=2 AND target_id=<groupNo> AND auto_follow_threads=1
-- 走该索引能在 N 个 follower 中快速定位目标用户集合。
--
-- ADD COLUMN + ADD INDEX 合在同一 ALTER 内（yujiawei review round-2）。
-- 精确表述：MySQL 8.0 上 ADD COLUMN 是 INSTANT（仅改元数据，秒级返回），
-- ADD INDEX 是 INPLACE（在线重建，持 SHARED_UPGRADABLE MDL；耗时与表大小成正比）。
-- 合并成单条 ALTER 让两步共享一次 MDL 抖动，避免顺序两次 ALTER 各占一次窗口；
-- 但整体仍按 INPLACE 计时 —— 大表上线前请评估 user_conversation_ext 行数与
-- 索引构建窗口（建议先确认行数 < 500 万，否则用 pt-osc / gh-ost 单独跑 ADD INDEX）。
ALTER TABLE user_conversation_ext
  ADD COLUMN auto_follow_threads TINYINT(1) NOT NULL DEFAULT 0
    COMMENT '关注频道时自动连带关注其下子区 (cascade follow flag)',
  ADD INDEX idx_channel_auto_follow (target_type, target_id, auto_follow_threads);

-- +migrate Down
ALTER TABLE user_conversation_ext
  DROP INDEX idx_channel_auto_follow,
  DROP COLUMN auto_follow_threads;
