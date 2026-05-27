-- +migrate Up
-- 为 user 表新增 language 列，承载多端语言偏好的真相源（i18n 主方案 D10 / TODOS 0.5）。
-- 与 phone/zone 等历史教训一致，使用 NOT NULL DEFAULT '' 以避免扫描 NULL 到 Go string 失败；
-- 空串语义为"未显式设置，沿用 OCTO_DEFAULT_LANGUAGE"。长度参考 BCP 47 子标签上限取 16。
ALTER TABLE `user` ADD COLUMN language VARCHAR(16) NOT NULL DEFAULT '' COMMENT '用户语言偏好（BCP 47，空表示沿用 OCTO_DEFAULT_LANGUAGE）';

-- +migrate Down
ALTER TABLE `user` DROP COLUMN language;
