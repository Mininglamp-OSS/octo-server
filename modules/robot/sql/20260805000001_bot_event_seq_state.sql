-- +migrate Up

-- octo_bot_event_seq_state (#697): bot 事件 id 分配器的**权威**状态，恒 1 行。
--
-- 为什么状态必须在 MySQL 而不是只在 Redis：分配器本身是 Redis INCR（热路径，
-- saveRobotMessage 跑在 msgSem 槽内，承受不起每次一次 DB 往返），但激活状态若也只在
-- Redis，就会随 RDB 回滚一起丢——生产 appendonly no，只有 RDB 快照。状态丢失后分配器
-- 降级回 legacy GenSeq，而 GenSeq 的号低于计数器已发出的号，于是新事件落在消费者游标
-- 下方、永久不可见，正是 #697 本身的镜像。
--
-- 所以：本表是权威 + 恢复源，Redis 键 botEventSeq:mode 只是镜像。镜像丢失时分配器读
-- 本表自愈（重建镜像 + 重新 seed），不降级。
--
-- 与 octo_message_extra_version_state (#627) 刻意同形（mode/epoch/cutover_floor +
-- FOR UPDATE 的 CAS flip + floor 校验），但**不复用它的 FOR SHARE drain barrier**：
-- 那个 barrier 要求每个写入方在业务事务内持锁到 commit，而 robotEvent 的写入是
-- INCR + ZADD 纯 Redis，没有 commit 可持。本方案的 flip 前提是运维先确认无旧副本，
-- 详见 tools/botevent-seq。
CREATE TABLE IF NOT EXISTS `octo_bot_event_seq_state` (
  `singleton_id`  TINYINT UNSIGNED NOT NULL COMMENT '恒为1的单例键',
  `mode`          TINYINT          NOT NULL DEFAULT 0 COMMENT '0=legacy(GenSeq) 1=incr(Redis计数器)',
  `epoch`         BIGINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '换代计数,operator CAS 递增;镜像据此判过期',
  `cutover_floor` BIGINT           NOT NULL DEFAULT 0 COMMENT '激活时校验过的号段下界',
  `updated_at`    TIMESTAMP        NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`singleton_id`),
  CONSTRAINT `chk_bot_event_seq_singleton` CHECK (`singleton_id` = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='bot 事件 id 分配器状态单例(#697)';

-- seed 为 legacy：部署本身行为中立,切换是独立的运维动作。
INSERT INTO `octo_bot_event_seq_state`
  (`singleton_id`, `mode`, `epoch`, `cutover_floor`)
VALUES (1, 0, 0, 0)
ON DUPLICATE KEY UPDATE `singleton_id` = `singleton_id`;

-- +migrate Down

DROP TABLE IF EXISTS `octo_bot_event_seq_state`;
