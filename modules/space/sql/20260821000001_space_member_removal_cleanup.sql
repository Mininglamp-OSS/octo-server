-- +migrate Up
-- Transactional outbox for Space member-removal conversation cleanup.
--
-- A row is inserted in the SAME transaction that flips space_member.status to 0,
-- so a crash between the membership commit and the cascade cannot lose the work.
-- The worker (modules/space/member_removal.go) claims rows with
-- FOR UPDATE SKIP LOCKED under a lease, runs every registered cleanup step, and
-- retries with exponential backoff until the steps succeed or attempts are
-- exhausted.
--
-- Why an outbox rather than the shared event table: modules/base/event is a
-- fire-once dispatcher. A listener that reports an error marks the event Fail
-- (event/api.go updateEventStatus) and QueryAllWait only selects Wait, so the
-- row is never re-dispatched; multiple listeners also share one status row whose
-- version_lock is never incremented, making last-writer-wins. Neither property
-- can carry an at-least-once cascade.
--
-- Deliberately NOT unique on (space_id, uid): remove → rejoin → remove must
-- enqueue a second job. The worker re-checks live membership before acting, so a
-- stale job for a member who has since rejoined resolves to a no-op.
CREATE TABLE IF NOT EXISTS `space_member_removal_cleanup` (
  `id`              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  `space_id`        VARCHAR(40)     NOT NULL COMMENT '成员被移出的 Space',
  `uid`             VARCHAR(64)     NOT NULL COMMENT '被移除的成员',
  `operator_uid`    VARCHAR(64)     NOT NULL DEFAULT '' COMMENT '操作者；自助退出时等于 uid',
  `reason`          VARCHAR(32)     NOT NULL COMMENT 'kicked / left / force_removed / space_disbanded',
  `status`          TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '0=pending 1=done 2=abandoned',
  `attempts`        INT UNSIGNED    NOT NULL DEFAULT 0,
  `next_attempt_at` DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `lease_owner`     VARCHAR(64)     NOT NULL DEFAULT '',
  `lease_until`     DATETIME(3)     NULL,
  `last_error`      VARCHAR(255)    NOT NULL DEFAULT '' COMMENT '低基数失败摘要，不含用户内容',
  `created_at`      DATETIME(3)     NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  `finished_at`     DATETIME(3)     NULL,
  PRIMARY KEY (`id`),
  KEY `idx_space_member_removal_cleanup_pending` (`status`, `next_attempt_at`, `lease_until`),
  KEY `idx_space_member_removal_cleanup_target` (`space_id`, `uid`),
  -- 保留期清理按 (status, finished_at) 扫。pending 索引的首列虽然也是 status，
  -- 但第二列是 next_attempt_at，帮不上 finished_at 的范围条件；没有这条索引，
  -- 每小时一次的 purge 会全表扫一张随每次移除增长的表。
  KEY `idx_space_member_removal_cleanup_finished` (`status`, `finished_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='Space 成员移除后的会话面清理工单';

-- +migrate Down
DROP TABLE IF EXISTS `space_member_removal_cleanup`;
