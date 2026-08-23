-- +migrate Up

-- 退订待办表：IMRemoveSubscriber 失败时，失败本身要活下来。
--
-- 背景（Mininglamp-OSS/octo-server#797）：删掉 group_member 行之后调用
-- IMRemoveSubscriber，这一步失败原先只打日志，工单照样标 done。人在业务库里
-- 已经不是成员，在 WuKongIM 里还是订阅者。实测（wukongim v2.2.4-20260313）：
-- 这种「泄漏态」与正常群成员**完全无差别** —— 照发照收；只有退订成功才会
-- 得到 SubscriberNotExist。而且四条自愈路径全断：
--   1. 重跑清理工单是空转（重试范围由活跃 group_member 行推导，人已删）
--   2. broker 不会重载（IMDatasource 回调是死代码）
--   3. 用户自己看不到这个群（group_member 已删），没有退出可点
--   4. 管理员在成员列表里也看不到这个人
-- 所以泄漏是永久且四方不可见的。
--
-- 为什么必须独立成表、而不是挂在 space_member_removal_cleanup 上：
-- 五个泄漏点里只有两个来自 Space 成员移除；拉黑、退群 bot 级联、删 bot
-- 都没有对应的成员移除工单，也没有 space_id / reason 可填。
--
-- 没有 done 状态：成功即删行。这行的唯一用途是「失败时活下来」，没有人会查
-- 「哪些退订成功了」——移除本身已由 space_member_removal_cleanup 与事件记录。
-- 一次 1000 人 / 50 群的解散是约 5 万次退订，全部留痕只会压垮保留期清理。
-- 于是表的大小只与「在途 + 坏掉的」成正比，而不是与历史全部流量成正比。
CREATE TABLE IF NOT EXISTS `im_pending_subscriber_removal` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `channel_id`      VARCHAR(128)    NOT NULL COMMENT '群 group_no，或子区 {groupNo}____{shortID}',
  `channel_type`    TINYINT UNSIGNED NOT NULL COMMENT 'WuKongIM channel type：群=2，子区=CommunityTopic',
  `uid`             VARCHAR(64)     NOT NULL COMMENT '要摘除订阅的用户或 bot',
  `status`          TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '0=pending 2=abandoned（成功即删行，故无 done）',
  `attempts`        INT UNSIGNED    NOT NULL DEFAULT 0,
  `next_attempt_at` DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `lease_owner`     VARCHAR(64)     NOT NULL DEFAULT '',
  `lease_until`     DATETIME(3)     NULL,
  `last_error`      VARCHAR(255)    NOT NULL DEFAULT '' COMMENT '低基数失败摘要，不含用户内容',
  `created_at`      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  PRIMARY KEY (`id`),
  -- 认领扫 (status, next_attempt_at, lease_until)，与 space_member_removal_cleanup 同款。
  KEY `idx_im_pending_sub_removal_pending` (`status`, `next_attempt_at`, `lease_until`),
  -- 幂等入队：同一个 (channel, uid) 已有待办时不再重复插入。
  UNIQUE KEY `uk_im_pending_sub_removal_target` (`channel_id`, `channel_type`, `uid`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='待重试的 IM 退订操作';

-- +migrate Down
DROP TABLE IF EXISTS `im_pending_subscriber_removal`;
