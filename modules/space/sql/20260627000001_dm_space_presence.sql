-- +migrate Up
-- DM (Person) per-Space presence index (issue #484).
--
-- Records "this DM pair has at least one message tagged with this Space",
-- keyed by the symmetric canonical pair id produced by
-- common.GetFakeChannelIDWith(uidA, uidB). Written authoritatively at WuKongIM
-- message-webhook ingest (modules/webhook handleMessageNotify) for Person
-- messages carrying payload.space_id; read by the conversation Space filter
-- (modules/message/space_filter.go) so DM visibility no longer depends on the
-- single shared Recents window (fixes symptom 2: DMs mutually hiding between
-- Spaces). Population is incremental; readers OR this signal with the existing
-- Recents-window scan, so a missing row never hides a currently-visible DM.
CREATE TABLE IF NOT EXISTS `dm_space_presence` (
  `fake_channel_id` VARCHAR(100) NOT NULL COMMENT 'common.GetFakeChannelIDWith 规范化的 DM 对 ID',
  `space_id`        VARCHAR(40)  NOT NULL COMMENT '该 DM 出现过消息的 Space ID',
  `last_timestamp`  BIGINT       NOT NULL DEFAULT 0 COMMENT '该 Space 下最后一条消息的服务器时间戳(秒)',
  `created_at`      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updated_at`      TIMESTAMP    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`fake_channel_id`, `space_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='DM 频道-Space 存在性索引(issue #484)';

-- +migrate Down
DROP TABLE IF EXISTS `dm_space_presence`;
