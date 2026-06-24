-- +migrate Up

-- 入站 webhook 支持绑定到子区（thread）：新增投递目标频道两列 + 一个复合索引。
--   - channel_type    ：投递频道类型。2=群(common.ChannelTypeGroup，默认/存量语义)，
--                        5=子区(common.ChannelTypeCommunityTopic)。
--   - thread_short_id ：绑定的子区 short_id；群 webhook 恒为空串。
-- 绑定在创建时写入、之后不可改——push 路径只读这两列经 targetChannel() 派生投递频道，
-- 推送 URL / body 完全不变（推送方零适配）。存量行按默认值回填即「投递到父群」，与历史
-- 行为逐字节一致；create 时两条路径都会显式写入 channel_type(2/5)，DEFAULT 仅作回填兜底。
--
-- 拆成两条 ALTER（MySQL 8.0）：列在表末尾 ADD COLUMN 走 INSTANT（瞬时、无 rebuild）；索引
-- 单独一条并显式 pin ALGORITHM=INPLACE, LOCK=NONE。原因：ADD INDEX 不支持 INSTANT，若与
-- ADD COLUMN 合在一条 ALTER 里，MySQL 取最严算法、【整条】退化为 INPLACE，把列的 INSTANT
-- 也一并拖慢——注释写「INSTANT」即与实际不符。拆分让列保持 INSTANT、索引在线构建
-- (LOCK=NONE)；显式 pin 还能让不支持在线 DDL 的环境 fail-loud 而非静默降级（与
-- modules/oidc/sql/20260515000001_oidc_bind_uniques.sql 同口径）。incoming_webhook 体量小，
-- 索引 INPLACE 构建成本可忽略。
ALTER TABLE `incoming_webhook`
  ADD COLUMN `channel_type`    SMALLINT    NOT NULL DEFAULT 2  COMMENT '投递频道类型：2=群(默认),5=子区(ChannelTypeCommunityTopic)',
  ADD COLUMN `thread_short_id` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '绑定的子区 short_id；群 webhook 为空串';

ALTER TABLE `incoming_webhook`
  ADD INDEX `idx_incoming_webhook_thread` (`group_no`, `thread_short_id`, `status`), ALGORITHM=INPLACE, LOCK=NONE;

-- +migrate Down
ALTER TABLE `incoming_webhook` DROP INDEX `idx_incoming_webhook_thread`;
ALTER TABLE `incoming_webhook` DROP COLUMN `channel_type`, DROP COLUMN `thread_short_id`;
