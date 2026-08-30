-- +migrate Up
-- YUJ-375：开放群下子区(channel_type=5)AI浓度。子区消息此前在 ETL resolveChannelMeta
-- 被当作私聊反解失败而整段丢弃，事实表本就无子区行(非"没算浓度"，是"无数据源")。
-- 现放开子区：channel_id="<父群no>____<短ID>"，劈父群段继承父群 space/conv，子区自有
-- channel_id 独立成事实行。为让下游(octo-dap)按父群归属上卷子区，两张事实表补
-- parent_channel_id 冗余列(NOT NULL DEFAULT ''，非子区行为 '')。
--
-- ③ octo_fact_member_channel_daily：写入侧(accumulateFact3)带列；重算 ④ 时按会话取 MAX。
-- ④ octo_fact_channel_daily：recomputeChannelDay 由 ③ MAX(parent_channel_id) 上卷得到；
--    加 idx_parent_date 支撑下游按父群×日聚合子区。
--
-- 在线性：列位置对事实表纯装饰(读写全按列名)，故不用 AFTER，让 ADD COLUMN 在 8.0.12+
-- 走真正的 INSTANT(AFTER <非末列> 需 8.0.29+ 才 instant，否则静默退化为表重建)。索引单独
-- 一条 ALTER(ADD INDEX 至多 INPLACE，与 INSTANT 加列合并会令整条语句无法 INSTANT)。两条均
-- 显式钉 ALGORITHM/LOCK：一旦某 MySQL 版本无法满足，直接报错而非静默退化为 COPY 锁表。
-- 注意：历史子区消息此前被丢弃，本迁移只补 schema——要有历史子区数据须在部署新 ETL 后走
-- Rebuild()/truncateForRebuild 全量重扫 message 分片重建。
ALTER TABLE `octo_fact_member_channel_daily`
  ADD COLUMN `parent_channel_id` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '子区(channel_type=5)所属父群channel_id; 非子区为空',
  ALGORITHM=INSTANT;

ALTER TABLE `octo_fact_channel_daily`
  ADD COLUMN `parent_channel_id` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '子区(channel_type=5)所属父群channel_id; 非子区为空',
  ALGORITHM=INSTANT;

ALTER TABLE `octo_fact_channel_daily`
  ADD KEY `idx_parent_date` (`parent_channel_id`,`stat_date`),
  ALGORITHM=INPLACE, LOCK=NONE;

-- +migrate Down
ALTER TABLE `octo_fact_channel_daily`
  DROP KEY `idx_parent_date`;

ALTER TABLE `octo_fact_channel_daily`
  DROP COLUMN `parent_channel_id`;

ALTER TABLE `octo_fact_member_channel_daily`
  DROP COLUMN `parent_channel_id`;
