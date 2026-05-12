-- +migrate Up
-- 修复历史数据：已软删除的 Bot（status=0）仍占用 username，
-- 导致相同标识符无法被 /newbot 复用。
-- 配合 PR #791 的增量修复（deleteRobot 时清空 username）。
-- 注意：使用存储过程安全处理表不存在的情况（测试环境迁移顺序问题）。

DROP PROCEDURE IF EXISTS fix_robot_username;

-- +migrate StatementBegin
CREATE PROCEDURE fix_robot_username()
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = 'robot') THEN
        UPDATE `robot` SET `username` = '' WHERE `status` = 0 AND `username` != '';
    END IF;
END;
-- +migrate StatementEnd

CALL fix_robot_username();
DROP PROCEDURE IF EXISTS fix_robot_username;

-- +migrate Down
-- 不可逆操作：已清空的 username 无法还原（原始值未保留）。
-- 如需回滚，需从备份恢复。
