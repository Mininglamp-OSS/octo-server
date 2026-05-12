-- +migrate Up

-- 账号注销冷静期：扩展 is_destroy 取值（0=正常 1=注销申请中 2=已注销），新增申请/到期时间
-- 已存在的 is_destroy=1 记录视为「已注销」，需迁移为 2 后再启用新语义
UPDATE `user` SET `is_destroy` = 2 WHERE `is_destroy` = 1;

ALTER TABLE `user`
    ADD COLUMN `destroy_apply_at`  DATETIME DEFAULT NULL COMMENT '注销申请时间',
    ADD COLUMN `destroy_expire_at` DATETIME DEFAULT NULL COMMENT '注销到期执行时间';

CREATE INDEX `idx_user_destroy_expire` ON `user` (`is_destroy`, `destroy_expire_at`);

-- +migrate Down

DROP INDEX `idx_user_destroy_expire` ON `user`;
ALTER TABLE `user`
    DROP COLUMN `destroy_apply_at`,
    DROP COLUMN `destroy_expire_at`;
UPDATE `user` SET `is_destroy` = 1 WHERE `is_destroy` = 2;
