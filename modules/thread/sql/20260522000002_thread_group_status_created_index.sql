-- +migrate Up

-- 支撑 conversation_ext.FollowChannel → ThreadEnumerator.EnumerateActiveShortIDs
-- 的查询：
--   SELECT * FROM thread
--    WHERE group_no=? AND status IN (?)
--    ORDER BY created_at DESC, id DESC
--    LIMIT ?
--
-- 既有的 group_no / status 单列索引让"找到群下所有 thread"够快，但缺少 created_at
-- 排序键会让 LIMIT 500 物化路径在子区多的群里回表 + filesort（lml2468 round-2
-- review）。复合索引覆盖排序与 limit，且保留前缀 (group_no, status) 让旧路径仍可用。
ALTER TABLE thread
  ADD INDEX idx_group_status_created_id (group_no, status, created_at, id);

-- +migrate Down
ALTER TABLE thread DROP INDEX idx_group_status_created_id;
