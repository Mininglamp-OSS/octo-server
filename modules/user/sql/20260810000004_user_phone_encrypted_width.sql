-- +migrate Up
-- user.zone VARCHAR(20) + user.phone VARCHAR(100) 在 utf8mb4 下最多占 480 字节；
-- 再加上长度前缀、AES-GCM nonce/tag 和密文版本头，极限密文会超过
-- 512 字节。扩到 1024 为存量非数字脏数据留足余量，避免某一行
-- Error 1406 让回填 remaining 永远无法归零。变长列不会为短手机号
-- 预先分配 1024 字节。
ALTER TABLE `user`
  MODIFY COLUMN `phone_encrypted` VARBINARY(1024) NULL DEFAULT NULL COMMENT '手机号密文(AES-256-GCM,phone 的加密影子列),无手机号时为 NULL';

-- +migrate Down
-- 字段缩回 255 可能截断已合法入库的密文，所以应用回滚时刻意保留
-- 该加法 schema。如果继续回退早期迁移，00001 的 Down 会整体删除影子列。
SELECT 1;
