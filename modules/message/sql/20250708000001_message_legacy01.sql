-- +migrate Up

-- card-message-interaction P2 D9：卡片消息(ContentType=17)乱序帧防护。
-- 存最新已存帧的单调 card_seq，与 content_edit 同表；bot 编辑携带 card_seq 时
-- 走条件 CAS(仅当新值 > 已存或已存为 NULL 才覆盖)。NULL = 无序号 → last-write-wins
-- (单写者 bot 零迁移，行为不变)。
--
-- 运维(PR#548 review)：ADD COLUMN nullable + 默认 NULL 在 MySQL 8.0+ 为 INSTANT DDL
-- (ALGORITHM=INSTANT,仅改元数据,不重建表、不锁写)。message_extra 是生产大表 —— 若目标
-- 实例为 MySQL 5.7,或行格式/历史 ALTER 使其不支持 INSTANT,请用 pt-online-schema-change
-- / gh-ost 在线执行,避免全表重建期间长时间锁写。
ALTER TABLE `message_extra` ADD COLUMN card_seq bigint DEFAULT NULL COMMENT '卡片消息(type=17)最新已存帧的 card_seq(D9 CAS)；NULL=无序号/last-write-wins';
