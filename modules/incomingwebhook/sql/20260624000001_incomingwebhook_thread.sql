-- +migrate Up

-- 入站 webhook 支持绑定到子区（thread）：新增投递目标频道两列。
--   - channel_type    ：投递频道类型。2=群(common.ChannelTypeGroup，默认/存量语义)，
--                        5=子区(common.ChannelTypeCommunityTopic)。
--   - thread_short_id ：绑定的子区 short_id；群 webhook 恒为空串。
-- 绑定在创建时写入、之后不可改——push 路径只读这两列经 targetChannel() 派生投递频道，
-- 推送 URL / body 完全不变（推送方零适配）。存量行按默认值回填即「投递到父群」，与历史
-- 行为逐字节一致；create 时两条路径都会显式写入 channel_type(2/5)，DEFAULT 仅作回填兜底。
--
-- 目标库 MySQL 8.0：表末尾 ADD COLUMN 走 INSTANT 算法、瞬时无锁。新增
-- (group_no, thread_short_id, status) 复合索引服务子区维度的管理列表查询
-- （db.queryByThreadScope）与群维度列表（group_no + thread_short_id='' 前缀命中）。
ALTER TABLE `incoming_webhook`
  ADD COLUMN `channel_type`    SMALLINT    NOT NULL DEFAULT 2  COMMENT '投递频道类型：2=群(默认),5=子区(ChannelTypeCommunityTopic)',
  ADD COLUMN `thread_short_id` VARCHAR(32) NOT NULL DEFAULT '' COMMENT '绑定的子区 short_id；群 webhook 为空串',
  ADD INDEX `idx_incoming_webhook_thread` (`group_no`, `thread_short_id`, `status`);

-- +migrate Down
ALTER TABLE `incoming_webhook`
  DROP INDEX `idx_incoming_webhook_thread`,
  DROP COLUMN `channel_type`,
  DROP COLUMN `thread_short_id`;
