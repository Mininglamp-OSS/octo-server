-- +migrate Up
-- 修复 phone/zone 自 20220222000001 起被改回 nullable 的回归（GitHub issue #54）。
-- 上次的 MODIFY 漏写 NOT NULL DEFAULT '' ，MySQL 默认把列改成允许 NULL；之后
-- 任何外部 INSERT 漏列（如手工 bootstrap admin、第三方登录的部分路径）就会
-- 让 phone/zone 出现 NULL，导致所有把这两列扫到 Go string 字段的接口
-- （/v1/manager/user/list、/v1/maillist、第三方登录扫描等）返回
-- "converting NULL to string is unsupported"。
--
-- 1) 先 backfill：把存量 NULL 收敛成 ''，避免下一步 ALTER 因列里有 NULL 失败。
UPDATE `user` SET phone = '' WHERE phone IS NULL;
UPDATE `user` SET zone  = '' WHERE zone  IS NULL;

-- 2) 重新加 NOT NULL DEFAULT '' 并补回 20191106000003 原始 COMMENT
--    （20220222 那次裸 MODIFY 把 COMMENT 一起冲掉了）。
--    ALTER MODIFY 是幂等的——目标状态相同就不会报错，可在 already-fixed
--    的环境上安全重跑。
ALTER TABLE `user` MODIFY COLUMN phone VARCHAR(100) NOT NULL DEFAULT '' COMMENT '手机号';
ALTER TABLE `user` MODIFY COLUMN zone  VARCHAR(20)  NOT NULL DEFAULT '' COMMENT '手机区号';

-- +migrate Down
-- 保留可逆性，但执行会重新引入 #54，强烈不建议。
ALTER TABLE `user` MODIFY COLUMN phone VARCHAR(100);
ALTER TABLE `user` MODIFY COLUMN zone  VARCHAR(20);
