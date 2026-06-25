-- +migrate Up

-- @ 提及目标改为【创建/修改 webhook 时配置】(不再由外部调用方在 push body 里传 mention)：
-- 新增 mention_uids 列，存放该 webhook 每次推送要 @ 的成员/ bot UID 列表(JSON 数组字符串，
-- 如 '["uid_a","bot_b"]')。推送时服务端把它过一遍【当前】群成员闸后渲染成 @气泡 + 路由；
-- 定向 @uid 不受 allow_mention_* 广播开关约束(受群成员闸 + 去重 + 上限 50)。空串即不 @。
--
-- 容量：上限 50 个 UID，单个 UID ≤ 40 字符(与 creator_uid VARCHAR(40) 同口径)，JSON 化后
-- 50*(40+3)+2 ≈ 2.2KB，VARCHAR(4096) 留足余量且不挤占行内空间(DYNAMIC 行格式长串可离页)。
-- 用 VARCHAR NOT NULL DEFAULT '' 与表内其它字符串列一致，避开 TEXT 表达式默认值的版本差异。
--
-- 目标库 MySQL 8.0：表末尾 ADD COLUMN 走 INSTANT 算法、瞬时无锁。历史行回填 '' 即「不 @」，
-- 与新建行口径一致，向后兼容。
ALTER TABLE `incoming_webhook`
  ADD COLUMN `mention_uids` VARCHAR(4096) NOT NULL DEFAULT '' COMMENT '推送时自动 @ 的成员/bot UID 列表(JSON 数组，上限 50)；创建/修改时配置，push body 不再接受 mention；空串=不 @';

-- 广播开关语义变更（同次需求）：allow_mention_all / allow_mention_bots 从「允许调用方在 push
-- body 里按条请求广播」(#445/#448 旧契约) 改为「每次推送都广播」。旧契约下被设过 1 的行在新
-- 语义下会变成「每条都 @所有人 / @所有 AI」(全员刷屏)，故升级时把存量值归零，让运维在新语义
-- 下重新显式开启——避免静默的追溯性行为变更。新建/未设过的行本就是 0，不受影响。
UPDATE `incoming_webhook` SET `allow_mention_all` = 0, `allow_mention_bots` = 0
  WHERE `allow_mention_all` <> 0 OR `allow_mention_bots` <> 0;

-- 同步更新列 COMMENT：旧注释写的是「是否*允许*推送」(per-request 闸语义)，与新运行时「每次
-- 推送都广播」矛盾，会误导读 schema 的运维。仅改 COMMENT(元数据)，不动类型/默认值。
ALTER TABLE `incoming_webhook`
  MODIFY COLUMN `allow_mention_all`  SMALLINT NOT NULL DEFAULT 0 COMMENT '每次推送是否 @所有人(真人广播→mention.humans)：0=否(默认),1=是；另受 system_setting member_can_broadcast 策略闸',
  MODIFY COLUMN `allow_mention_bots` SMALLINT NOT NULL DEFAULT 0 COMMENT '每次推送是否 @所有 AI(bot广播→mention.ais)：0=否(默认),1=是；另受 member_can_broadcast 策略闸';

-- +migrate Down
-- 还原列 COMMENT 到旧语义（值无法还原——升级时归零是单向的，按需手动重配）。
ALTER TABLE `incoming_webhook`
  MODIFY COLUMN `allow_mention_all`  SMALLINT NOT NULL DEFAULT 0 COMMENT '是否允许推送 @所有人(真人广播)：0=否(默认),1=是；由 webhook 合法成员可改',
  MODIFY COLUMN `allow_mention_bots` SMALLINT NOT NULL DEFAULT 0 COMMENT '是否允许推送 @所有 AI(bot广播)：0=否(默认),1=是；由 webhook 合法成员可改';
ALTER TABLE `incoming_webhook`
  DROP COLUMN `mention_uids`;
