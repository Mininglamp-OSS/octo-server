-- +migrate Up
-- 修复 issue #392：维表列比源列窄，生产脏/前缀数据触发
--   Error 1406 (22001): Data too long for column ...
-- 致整个 ETL 事务回滚、所有 octo_* 看板表常空、/v1/manager/dashboard/* 归零。
--
-- ① octo_dim_member.phone/email：注释写的是 user.phone/user.email，但宽度按
--    "电话号语义"取小了。源 user.phone 早已是 VARCHAR(100)、user.email 是
--    VARCHAR(255)，生产含更宽的合法/历史脏行(如把 email 误存进 phone、tombstone
--    标记 @...@delete)。维表必须能装下源能产出的内容，故对齐到不窄于源。
-- ② octo_dim_channel.member_a_uid/member_b_uid：原按裸 uid(32字节+余量)取
--    VARCHAR(40)，但私聊 channel_id 反解出的两端可能带 Space/适配器前缀
--    (s{32hex_spaceId}_{uid}≈66、sminglue_default_{uid}≈49)。ETL 现已在
--    normalizePrivatePair 反解前缀取裸 uid(对齐 dim_member.uid)，放宽到 100 仅作
--    "前缀未能反解(如命名空间未注册)时不致再次 1406"的兜底。
--
-- ALTER ... MODIFY 是幂等的：目标态相同即无实质变更，可在已手工 hotfix 的生产环境
-- 安全重跑(本迁移与线上 hotfix 等价收敛)。
ALTER TABLE `octo_dim_member`
  MODIFY COLUMN `email` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '邮箱 (user.email; agent通常为空)',
  MODIFY COLUMN `phone` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '手机号 (user.phone)';

ALTER TABLE `octo_dim_channel`
  MODIFY COLUMN `member_a_uid` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '私聊成员A裸uid (ETL反解Space前缀+字典序规范化, 对齐dim_member.uid)',
  MODIFY COLUMN `member_b_uid` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '私聊成员B裸uid';

-- +migrate Down
-- 回退会重新引入 #392(宽源数据再次溢出)，仅为保留可逆性，不建议执行。
ALTER TABLE `octo_dim_member`
  MODIFY COLUMN `email` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '邮箱 (user.email; agent通常为空)',
  MODIFY COLUMN `phone` VARCHAR(20)  NOT NULL DEFAULT '' COMMENT '手机号 (user.phone)';

ALTER TABLE `octo_dim_channel`
  MODIFY COLUMN `member_a_uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '私聊成员A (ETL按uid字典序规范化)',
  MODIFY COLUMN `member_b_uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '私聊成员B';
