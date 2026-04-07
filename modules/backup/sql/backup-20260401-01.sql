-- +migrate Up

-- 修复时区问题：TIMESTAMP 改为 DATETIME
-- 固定 session 时区为北京时间，确保 TIMESTAMP→DATETIME 转换时数据正确保留
SET @saved_tz = @@session.time_zone;
SET time_zone = '+08:00';

ALTER TABLE `backup_history`
    MODIFY `started_at` DATETIME NULL COMMENT '开始时间',
    MODIFY `finished_at` DATETIME NULL COMMENT '完成时间';

SET time_zone = @saved_tz;

-- +migrate Down

SET @saved_tz = @@session.time_zone;
SET time_zone = '+08:00';

ALTER TABLE `backup_history`
    MODIFY `started_at` TIMESTAMP NULL COMMENT '开始时间',
    MODIFY `finished_at` TIMESTAMP NULL COMMENT '完成时间';

SET time_zone = @saved_tz;
