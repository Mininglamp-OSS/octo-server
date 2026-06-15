-- +migrate Up
-- 群名上限 20→50（应用层 MaxGroupNameLen）配套列加宽。VARCHAR(40)→(50) 两侧均
-- < 256 字节（utf8mb4 最坏 4 字节/字符，50×4=200B），长度前缀仍 1 字节 → MySQL 8.0
-- 走 INPLACE/LOCK=NONE 在线变更，不重建表、不截断存量数据。按项目约定不 pin ALGORITHM/LOCK。
ALTER TABLE `group` MODIFY `name` VARCHAR(50) NOT NULL DEFAULT '';

-- +migrate Down
-- 回滚收回到 VARCHAR(40)。先显式把 >40 字符群名截到 40，再收窄列宽：MySQL 严格模式（STRICT_*）
-- 下 ALTER 收窄遇到超长数据会直接报 Data too long 而非静默截断，导致回滚失败/卡住；先 UPDATE
-- 预截断可避免。注意这是有损回滚（>40 字符的群名会丢尾部）——保留宽列才是无损选择。
UPDATE `group` SET `name` = LEFT(`name`, 40) WHERE CHAR_LENGTH(`name`) > 40;
ALTER TABLE `group` MODIFY `name` VARCHAR(40) NOT NULL DEFAULT '';
