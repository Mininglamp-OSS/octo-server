-- +migrate Up

-- message-extra-transactional-version (#627) PR-1：为 message_extra 增量同步游标
-- 引入「每存储频道事务序列」。version 分配改为在业务事务内经序列行 FOR UPDATE 保留、
-- 持锁到 commit，使提交序=版本序、跨副本唯一（根因见 brief D1/D2）。本迁移仅建表 + 索引，
-- 不回写历史行；生产在 mode=legacy 下 inert，真正启用是运维 cutover 动作（brief D9）。

-- 每存储频道一行的事务序列。channel_id/charset/collation 对齐 message_extra.channel_id
-- （实测 utf8mb4/utf8mb4_general_ci；生产上线前须再核一次）；channel_type 对齐其持久域(smallint)。
CREATE TABLE `octo_message_extra_channel_seq` (
  `channel_id`   VARCHAR(100) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL DEFAULT '' COMMENT '存储行频道ID(person=fakeChannelID)',
  `channel_type` SMALLINT     NOT NULL DEFAULT 0 COMMENT '频道类型(对齐 message_extra.channel_type)',
  `last_version` BIGINT       NOT NULL DEFAULT 0 COMMENT '该频道已分配的最大 message_extra 版本',
  PRIMARY KEY (`channel_id`,`channel_type`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='message_extra 每频道事务序列(#627)';

-- allocator 全局状态单例(brief D2/D9)：mode/epoch/floor 的唯一权威源。恒 1 行(singleton_id=1)。
-- 每个升级后的写入方在业务事务内 FOR SHARE 读本行选择 allocator；cutover 由 operator 工具
-- 对本行取 X 锁做 compare-and-set。seed 为 legacy，保证 Prepare 阶段生产行为不变。
CREATE TABLE `octo_message_extra_version_state` (
  `singleton_id`  TINYINT UNSIGNED NOT NULL COMMENT '恒为1的单例键',
  `mode`          TINYINT          NOT NULL DEFAULT 0 COMMENT '0=legacy,1=transactional',
  `epoch`         BIGINT UNSIGNED  NOT NULL DEFAULT 0 COMMENT '换代计数(operator CAS 递增,仅观测)',
  `cutover_floor` BIGINT           NOT NULL DEFAULT 0 COMMENT 'cutover 校验后的版本下界',
  PRIMARY KEY (`singleton_id`),
  CONSTRAINT `chk_message_extra_version_singleton` CHECK (`singleton_id` = 1)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='message_extra allocator 全局状态单例(#627)';

INSERT INTO `octo_message_extra_version_state`
  (`singleton_id`, `mode`, `epoch`, `cutover_floor`)
VALUES (1, 0, 0, 0);

-- 增量同步读路径 `WHERE channel_id=? AND channel_type=? AND version>? ORDER BY version ASC`
-- 现仅有 channel_idx(channel_id,channel_type)，version 的 range/order 无法走索引。补复合索引。
-- MySQL 8.0 下朴素 CREATE INDEX 即 INPLACE/LOCK=NONE 在线(不 pin 算法，遵仓库约定)；
-- message_extra 为大表，运维应安排低峰窗口 + 监控(时长问题，非阻塞)。
CREATE INDEX `channel_version_idx` ON `message_extra` (`channel_id`,`channel_type`,`version`);

-- +migrate Down

-- 逆向:删索引 + 两张新表(纯增量,回滚无数据迁移)。message_extra 现有行不受影响。
DROP INDEX `channel_version_idx` ON `message_extra`;
DROP TABLE IF EXISTS `octo_message_extra_version_state`;
DROP TABLE IF EXISTS `octo_message_extra_channel_seq`;
