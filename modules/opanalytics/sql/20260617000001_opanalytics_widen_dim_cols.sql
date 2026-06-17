-- +migrate Up
-- 修复 issue #392：维表列比源列窄，生产脏/前缀数据触发
--   Error 1406 (22001): Data too long for column ...
-- 致整个 ETL 事务回滚、所有 octo_* 看板表常空、/v1/manager/dashboard/* 归零。
--
-- ① octo_dim_member.phone：真正的 1406 元凶。源 user.phone 早已是 VARCHAR(100)，
--    维表却按"电话号语义"取了 VARCHAR(20)，生产含更宽的合法/历史脏行(如把 email
--    误存进 phone、tombstone 标记 @...@delete)→ 溢出。对齐到不窄于源(100)。
-- ② octo_dim_member.email：源 user.email 实为 VARCHAR(100)，维表原本也是 100、本已
--    匹配；此处加宽到 VARCHAR(255) 仅为留余量并与线上 hotfix 收敛(非镜像源宽)。
--    注意连带约束：octo_dim_member 上有整列索引 idx_email(email)，utf8mb4 下
--    255*4=1020 字节 > 旧行格式(COMPACT/REDUNDANT)的 767 索引前缀上限，会在这类实例上
--    令 ALTER 直接报 1071(max key length 767) 而失败。故同步把 idx_email 改成 191 前缀
--    索引(191*4=764<767，兼容所有行格式；email 等值/前缀检索前 191 字符足够区分)。
-- ③ octo_dim_channel.member_a_uid/member_b_uid：原按裸 uid(32字节+余量)取
--    VARCHAR(40)，但私聊 channel_id 反解出的两端可能带 Space/适配器前缀
--    (s{32hex_spaceId}_{uid}≈66、sminglue_default_{uid}≈49)。ETL 现已在
--    normalizePrivatePair 反解前缀取裸 uid(对齐 dim_member.uid)，放宽到 100 仅作
--    "前缀未能反解(如命名空间未注册)时不致再次 1406"的兜底。
--
-- ALTER 幂等：目标态相同即无实质变更，可在已手工 hotfix 的生产环境安全重跑——
-- DROP+ADD idx_email 把线上 hotfix 遗留的整列 idx_email 一并收敛为 191 前缀。
ALTER TABLE `octo_dim_member`
  DROP INDEX `idx_email`,
  MODIFY COLUMN `email` VARCHAR(255) NOT NULL DEFAULT '' COMMENT '邮箱 (源user.email实为VARCHAR(100); 此处留余量; agent通常为空)',
  MODIFY COLUMN `phone` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '手机号 (user.phone)',
  ADD KEY `idx_email` (`email`(191));

ALTER TABLE `octo_dim_channel`
  MODIFY COLUMN `member_a_uid` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '私聊成员A裸uid (ETL反解Space前缀+字典序规范化, 对齐dim_member.uid)',
  MODIFY COLUMN `member_b_uid` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '私聊成员B裸uid';

-- +migrate Down
-- 回退会重新引入 #392(宽源数据再次溢出)，仅为保留可逆性，不建议执行。
-- email 收回 100 后整列 idx_email=400<767，恢复整列索引安全。
ALTER TABLE `octo_dim_member`
  DROP INDEX `idx_email`,
  MODIFY COLUMN `email` VARCHAR(100) NOT NULL DEFAULT '' COMMENT '邮箱 (user.email; agent通常为空)',
  MODIFY COLUMN `phone` VARCHAR(20)  NOT NULL DEFAULT '' COMMENT '手机号 (user.phone)',
  ADD KEY `idx_email` (`email`);

ALTER TABLE `octo_dim_channel`
  MODIFY COLUMN `member_a_uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '私聊成员A (ETL按uid字典序规范化)',
  MODIFY COLUMN `member_b_uid` VARCHAR(40) NOT NULL DEFAULT '' COMMENT '私聊成员B';
