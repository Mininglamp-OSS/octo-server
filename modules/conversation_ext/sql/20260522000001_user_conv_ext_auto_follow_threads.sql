-- +migrate Up

-- 关注频道时自动连带关注其下子区（issue: sidebar channel→threads 级联关注）。
-- - auto_follow_threads=1 表示用户已对该群按下"关注频道"，新建子区时同步给该用户落 thread ext 行。
-- - FollowChannel 把字段置 1 并物化当前 active 子区；UnfollowChannel 置 0 并级联清空 thread 行。
-- - 默认 0，对未上线 follow tab 的老用户无影响（无回填需求）。

ALTER TABLE user_conversation_ext
  ADD COLUMN auto_follow_threads TINYINT(1) NOT NULL DEFAULT 0
    COMMENT '关注频道时自动连带关注其下子区 (cascade follow flag)';

-- idx_channel_auto_follow 服务于 OnThreadCreated 的 fanout 查询：
--   WHERE target_type=2 AND target_id=<groupNo> AND auto_follow_threads=1
-- 走该索引能在 N 个 follower 中快速定位目标用户集合。
ALTER TABLE user_conversation_ext
  ADD INDEX idx_channel_auto_follow (target_type, target_id, auto_follow_threads);

-- +migrate Down
ALTER TABLE user_conversation_ext DROP INDEX idx_channel_auto_follow;
ALTER TABLE user_conversation_ext DROP COLUMN auto_follow_threads;
