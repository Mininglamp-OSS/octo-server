-- +migrate Up
-- YUJ-375：开放群下子区(channel_type=5)AI浓度。子区消息此前在 ETL resolveChannelMeta
-- 被当作私聊反解失败而整段丢弃，事实表本就无子区行(非"没算浓度"，是"无数据源")。
-- 现放开子区：channel_id="<父群no>____<短ID>"，劈父群段继承父群 space/conv，子区自有
-- channel_id 独立成事实行。为让下游(octo-dap)按父群归属上卷子区，两张事实表补
-- parent_channel_id 冗余列(非子区行为 '')。
--
-- ③ octo_fact_member_channel_daily：写入侧(accumulateFact3)带列；重算 ④ 时按会话取 MAX。
-- ④ octo_fact_channel_daily：recomputeChannelDay 由 ③ MAX(parent_channel_id) 上卷得到；
--    加 idx_parent_date 支撑下游按父群×日聚合子区。
--
-- 列可空默认 ''，历史行(全部为非子区)天然回填空串，语义正确。加列走 INSTANT/INPLACE，
-- 在线安全。注意：历史子区消息此前被丢弃，本迁移只补 schema——要有历史子区数据须在
-- 部署新 ETL 后走 truncateForRebuild 全量重扫 message 分片重建。
ALTER TABLE `octo_fact_member_channel_daily`
  ADD COLUMN `parent_channel_id` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '子区(channel_type=5)所属父群channel_id; 非子区为空' AFTER `conv_type`;

ALTER TABLE `octo_fact_channel_daily`
  ADD COLUMN `parent_channel_id` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '子区(channel_type=5)所属父群channel_id; 非子区为空' AFTER `conv_type`,
  ADD KEY `idx_parent_date` (`parent_channel_id`,`stat_date`);

-- +migrate Down
ALTER TABLE `octo_fact_channel_daily`
  DROP KEY `idx_parent_date`,
  DROP COLUMN `parent_channel_id`;

ALTER TABLE `octo_fact_member_channel_daily`
  DROP COLUMN `parent_channel_id`;
