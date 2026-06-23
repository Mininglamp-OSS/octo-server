-- +migrate Up

-- @ 提及（native 端点）引入两个 per-webhook 广播能力位：
--   - allow_mention_all  ：是否允许该 webhook 推送 @所有人(真人广播 → mention.humans)；
--   - allow_mention_bots ：是否允许该 webhook 推送 @所有 AI(bot 广播 → mention.ais)。
-- 广播会刷全群红点 / 唤起全部 bot，是高噪声能力，故默认关闭(0)、需显式打开；由 webhook
-- 的合法成员（创建者/管理员）经管理端开关。定向 @uid（指定成员）不受这两列约束（受群成员闸 + 上限）。
--
-- 目标库 MySQL 8.0：在表末尾 ADD COLUMN 走 INSTANT 算法、瞬时无锁，无需显式 pin
-- ALGORITHM/LOCK。历史行按默认值 0 回填即「广播默认关闭」，与新建行口径一致。
ALTER TABLE `incoming_webhook`
  ADD COLUMN `allow_mention_all`  SMALLINT NOT NULL DEFAULT 0 COMMENT '是否允许推送 @所有人(真人广播)：0=否(默认),1=是；由 webhook 合法成员可改',
  ADD COLUMN `allow_mention_bots` SMALLINT NOT NULL DEFAULT 0 COMMENT '是否允许推送 @所有 AI(bot广播)：0=否(默认),1=是；由 webhook 合法成员可改';

-- +migrate Down
ALTER TABLE `incoming_webhook`
  DROP COLUMN `allow_mention_all`,
  DROP COLUMN `allow_mention_bots`;
