-- +migrate Up

-- octo_session_rollout_marker: 一行、write-once。它**不是** rollout floor 的权威。
--
-- floor 仍然只存 Redis（{uidTokenCachePrefix}auth:rollout-control，Lua CAS，单调）。
-- 这里刻意不做 "MySQL 权威 + Redis 镜像"：那个形制（octo_bot_event_seq_state #697、
-- octo_message_extra_version_state #627）服务的是承受不起 DB 往返的热路径分配器，
-- 必须让 gate 与 INCR 同处一个 Lua。session mode 是进程内字段、按轮询刷新，
-- 不在每请求路径上，没有同样的约束；照抄只会引入一个两阶段写，而 floor 单调、
-- 没有撤销操作，Redis 侧写失败时无法回滚。
--
-- 这张表只回答一个问题：**floor 缺失时，是"从未初始化"还是"初始化过但 Redis 回滚丢了"。**
-- 两者需要完全相反的反应，而 Redis 自己答不了（它就是丢失的那一方）：
--
--   marker 缺失 + floor 缺失 → 首次采纳，从 expand 开始（keyspace 空则直达 enforce）
--   marker 缺失 + floor 存在 → 从 #725 升级，采纳现有 floor 并补写本行，行为不变
--   marker 存在 + floor 缺失 → Redis 回滚，向上取 enforce 并告警
--   marker 存在 + floor 存在 → 正常
--
-- 为什么丢失时"向上取"而不是"恢复原值"：user session token 本就可丢弃，重新登录是
-- 可接受代价，因此不需要恢复原值，只需要取一个安全值——而最安全的是最大值。三种回滚
-- 场景下向上取都严格更优：Redis 全清时 token 也没了，enforce 零额外代价；回滚到 floor
-- 建立之前时，复活的永久 legacy 正是漏洞本身，拒绝它们是对的；回滚到某个中间 floor 时，
-- "精确恢复"意味着继续接受一个来自不一致快照的 legacy 集合。
--
-- write-once 由应用层保证：代码中只有 INSERT ... ON DUPLICATE KEY UPDATE singleton_id
-- = singleton_id（冲突即 no-op），没有任何 UPDATE 语句，并有源码守卫测试。
-- initialized_floor 仅供审计，读路径不依赖它——它是采纳当时看到的值，不是当前值。
CREATE TABLE IF NOT EXISTS `octo_session_rollout_marker` (
  `singleton_id`      TINYINT UNSIGNED NOT NULL COMMENT '恒为1的单例键',
  `initialized_at`    TIMESTAMP        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '本部署首次建立/采纳 rollout floor 的时刻',
  `initialized_floor` VARCHAR(16)      NOT NULL DEFAULT '' COMMENT '采纳当时观察到的 floor，仅审计；空串表示当时尚无 floor',
  `created_at`        TIMESTAMP        NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`singleton_id`),
  CONSTRAINT `chk_session_rollout_marker_singleton` CHECK (`singleton_id` = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='session rollout 初始化标记单例，用于区分"从未初始化"与"Redis 回滚丢失"';

-- 不 seed。空表就是"从未初始化"，由首次启动按上表判定后补写。

-- +migrate Down

DROP TABLE IF EXISTS `octo_session_rollout_marker`;
