-- +migrate Up
-- robot 占用/绑定字段（PM Octo-link：一个 Bot 同时只被一个 Agent 占用）。
-- bound_agent_ref 为不透明标签（如 octopush:agent_xxx），空=空闲；占用互斥由
-- bind 接口的行级 CAS 保证，无需额外索引。
--
-- 与 20260603000001 同样采用存在性守卫的可重入写法：避免任一 pod 部分应用/
-- 滚动发布并发执行后，重启再跑 ADD COLUMN 报 Duplicate column name 而 CrashLoop。
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_robot_occupancy;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __botfather_robot_occupancy()
BEGIN
  IF NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'robot'
         AND COLUMN_NAME = 'bound_agent_ref') THEN
    ALTER TABLE `robot`
      ADD COLUMN `bound_agent_ref` varchar(128) NOT NULL DEFAULT '' COMMENT '占用方不透明标签（如 octopush:agent_xxx）；空=空闲',
      ADD COLUMN `bound_at` timestamp NULL DEFAULT NULL COMMENT '占用时间；释放时清空';
  END IF;
END;
-- +migrate StatementEnd

CALL __botfather_robot_occupancy();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_robot_occupancy;
-- +migrate StatementEnd

-- +migrate Down
-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_robot_occupancy_down;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __botfather_robot_occupancy_down()
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.COLUMNS
       WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'robot'
         AND COLUMN_NAME = 'bound_agent_ref') THEN
    ALTER TABLE `robot`
      DROP COLUMN `bound_at`,
      DROP COLUMN `bound_agent_ref`;
  END IF;
END;
-- +migrate StatementEnd

CALL __botfather_robot_occupancy_down();

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __botfather_robot_occupancy_down;
-- +migrate StatementEnd
