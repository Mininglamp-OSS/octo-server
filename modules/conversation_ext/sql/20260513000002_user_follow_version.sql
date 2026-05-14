-- +migrate Up

-- 用户关注列表版本号（issue #337, PR review Round-3 Blocking #1/#2）
--
-- 设计动机：
--   sidebar 聚合既要支持 follow 列表的 CAS 排序（/v1/follow/sort），
--   又要让客户端感知 follow/unfollow/category 变更后的列表刷新。
--
--   原方案把 IM 会话游标当作 follow_version 用，但 IM 游标只在收到消息时推进，
--   关注/取消关注/分类变更不会推 IM 游标，因此客户端会丢更新。
--   user_conversation_ext.version 是行级乐观锁，也无法表达"用户级"的状态变化。
--
--   独立的 (uid, space_id) → version 单调序列号同时解决两类需求：
--   1. UpdateSort 用它做 CAS（替代 per-row version 的脆弱方案）。
--   2. sidebar 响应携带 follow_version，客户端用来判断 follow 列表是否需要全量重建。
--
-- 写入路径：
--   - FollowDM / UnfollowDM / FollowChannel / UnfollowChannel /
--     FollowThread / UnfollowThread / UpdateSort 在同一 tx 内 +1。
--   - 级联清理（群解散、退群、踢人、删好友、删子区）也在同一 tx 内 +1。
--
-- 存量数据：
--   - 已存在的用户没有这一行，首次 BumpTx 时会创建为 version=1。
--   - 已存在用户首次访问 sidebar 会观察到 follow_version=0，触发全量拉取。
CREATE TABLE IF NOT EXISTS user_follow_version (
  uid        VARCHAR(40)  NOT NULL                  COMMENT '用户ID',
  space_id   VARCHAR(40)  NOT NULL DEFAULT ''       COMMENT '空间ID',
  version    BIGINT       NOT NULL DEFAULT 0        COMMENT 'follow 状态单调版本号',
  updated_at DATETIME     DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (uid, space_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户关注状态单调版本号（issue #337）';

-- +migrate Down

DROP TABLE IF EXISTS user_follow_version;
