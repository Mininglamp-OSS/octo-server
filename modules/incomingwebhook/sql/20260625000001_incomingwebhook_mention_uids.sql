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

-- +migrate Down
ALTER TABLE `incoming_webhook`
  DROP COLUMN `mention_uids`;
