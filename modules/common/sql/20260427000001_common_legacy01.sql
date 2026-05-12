-- +migrate Up

-- 注销冷静期天数（默认 7 天），由后台或环境配置统一管理
ALTER TABLE `app_config`
    ADD COLUMN `destroy_cooling_off_days` INT NOT NULL DEFAULT 7 COMMENT '注销冷静期天数';

-- +migrate Down

ALTER TABLE `app_config` DROP COLUMN `destroy_cooling_off_days`;
