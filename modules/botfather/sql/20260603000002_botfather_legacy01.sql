-- +migrate Up
-- robot 占用/绑定字段（PM Octo-link：一个 Bot 同时只被一个 Agent 占用）。
-- bound_agent_ref 为不透明标签（如 octopush:agent_xxx），空=空闲；占用互斥由
-- bind 接口的行级 CAS 保证，无需额外索引。
ALTER TABLE `robot`
  ADD COLUMN `bound_agent_ref` varchar(128) NOT NULL DEFAULT '' COMMENT '占用方不透明标签（如 octopush:agent_xxx）；空=空闲',
  ADD COLUMN `bound_at` timestamp NULL DEFAULT NULL COMMENT '占用时间；释放时清空';

-- +migrate Down
ALTER TABLE `robot`
  DROP COLUMN `bound_at`,
  DROP COLUMN `bound_agent_ref`;
