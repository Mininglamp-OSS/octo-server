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
  `epoch`         BIGINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '换代计数,operator CAS 递增;Redis 镜像值为 incr:{epoch},分配器据此拒绝陈旧/伪造镜像',
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

-- 拒绝在**已激活**状态下 drop 权威行(review P2-2)。
--
-- 一旦 mode=1，这张表就是唯一不随 Redis RDB 回滚的激活凭据：drop 掉它之后，
-- ReadState 返回 ErrStateMissing、按设计被读方当作 legacy，于是任何一次镜像丢失都会让
-- 分配器发出低于计数器已发号的 GenSeq id，落在活跃游标下方 —— #697 的镜像，由回滚本身
-- 造成。这个 flip 在别处（tools/botevent-seq、pkg/botevent/seq.go）都写明不可逆，Down
-- 也必须一致。
--
-- 手法：向单例表插入 singleton_id=2，只在 mode=1 时选出行。CHECK (singleton_id = 1)
-- 于是报 3819 并中止整个 Down；mode=0 时 SELECT 无行，INSERT 是 no-op。用已有约束报错，
-- 免得为一条条件判断建存储过程。
-- 前提：MySQL >= 8.0.16 才真正强制 CHECK（更早的版本解析后忽略，这条 Down 会静默放过）。
-- 本项目锁定 MySQL 8.0，所以这个前提成立。
-- 若确实要在激活后拆除，先人工把 mode 改回 0 并明白这意味着什么。
INSERT INTO `octo_bot_event_seq_state` (`singleton_id`, `mode`, `epoch`, `cutover_floor`)
SELECT 2, 0, 0, 0 FROM `octo_bot_event_seq_state` WHERE `singleton_id` = 1 AND `mode` = 1;

DROP TABLE IF EXISTS `octo_bot_event_seq_state`;
