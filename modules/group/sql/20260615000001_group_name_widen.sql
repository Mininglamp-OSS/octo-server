-- +migrate Up
-- 群名上限 20→50（应用层 MaxGroupNameLen）配套列加宽。VARCHAR(40)→(50) 两侧均
-- < 256 字节（utf8mb4 最坏 4 字节/字符，50×4=200B），长度前缀仍 1 字节 → MySQL 8.0
-- 走 INPLACE/LOCK=NONE 在线变更，不重建表、不截断存量数据。按项目约定不 pin ALGORITHM/LOCK。
ALTER TABLE `group` MODIFY `name` VARCHAR(50) NOT NULL DEFAULT '';

-- +migrate Down
-- 回滚收回到 VARCHAR(40)。若已写入 >40 字符群名，回滚会截断——回滚前需确认无超 40 字符数据。
ALTER TABLE `group` MODIFY `name` VARCHAR(40) NOT NULL DEFAULT '';
