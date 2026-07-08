-- +migrate Up

-- card-message-interaction P2 D9：卡片消息(ContentType=17)乱序帧防护。
-- 存最新已存帧的单调 card_seq，与 content_edit 同表；bot 编辑携带 card_seq 时
-- 走条件 CAS(仅当新值 > 已存或已存为 NULL 才覆盖)。NULL = 无序号 → last-write-wins
-- (单写者 bot 零迁移，行为不变)。
ALTER TABLE `message_extra` ADD COLUMN card_seq bigint DEFAULT NULL COMMENT '卡片消息(type=17)最新已存帧的 card_seq(D9 CAS)；NULL=无序号/last-write-wins';
