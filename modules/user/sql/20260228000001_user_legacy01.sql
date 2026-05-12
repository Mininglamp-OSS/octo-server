-- +migrate Up
-- 扩展 password 字段长度以支持 bcrypt 哈希（60字符）
ALTER TABLE `user` MODIFY COLUMN `password` VARCHAR(255) NOT NULL DEFAULT '';
