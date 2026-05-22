-- +migrate Up

-- Follow-up to 20260522000001 (PR #123 round-5 review, Jerry-Xin + yujiawei):
-- idx_channel_auto_follow 在 (target_type, target_id, auto_follow_threads) 三列上
-- 已能定位 fanout 目标行，但 OnThreadCreated 的两步热路径都要继续读 (uid, space_id)：
--   1) 初始 SELECT uid, space_id FROM user_conversation_ext
--        WHERE target_type=? AND target_id=? AND auto_follow_threads=1
--        ORDER BY uid, space_id；
--   2) selectEligibleForFanoutTx 的 SELECT ... FOR UPDATE 走同样的列过滤再 IN 一批 (uid, space_id)。
-- 当前索引让两步都得回 PK 表取 (uid, space_id)；大群 fanout（数千 follower）下
-- 这一次额外的 random IO 会主导整体延迟。
--
-- 把 (uid, space_id) 追加到索引末端，让两步成为 covering index lookup，
-- 同时 ORDER BY uid, space_id 直接走索引顺序、不再 filesort。
-- 旧索引前缀 (target_type, target_id, auto_follow_threads) 仍保留为新索引的前缀，
-- 因此不需要单独的兼容索引。
--
-- DROP + ADD 单条 ALTER 让两步共享一次 MDL 抖动（同 20260522000001 的合并 rationale）。
ALTER TABLE user_conversation_ext
  DROP INDEX idx_channel_auto_follow,
  ADD INDEX idx_channel_auto_follow (target_type, target_id, auto_follow_threads, uid, space_id);

-- +migrate Down
ALTER TABLE user_conversation_ext
  DROP INDEX idx_channel_auto_follow,
  ADD INDEX idx_channel_auto_follow (target_type, target_id, auto_follow_threads);
