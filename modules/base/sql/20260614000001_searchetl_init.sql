-- +migrate Up

-- NOTE: The built-in producer module (formerly modules/searchetl) has been removed.
-- This migration is retained ONLY to keep the gorp_migrations ID sequence continuous:
-- the migration ID is the bare filename and is already recorded in gorp_migrations, so
-- deleting the file would make sql-migrate panic on boot (unknown migration in database,
-- IgnoreUnknown=false). The file was moved here (DB-neutral; the ID is unchanged) when
-- modules/searchetl was deleted, and base is always loaded (blank-imported + embeds sql/).
-- Because the ID is already applied, sql-migrate skips this migration on every boot — the
-- CREATE TABLE below never re-runs. The octo_etl_es_cursor table is now owned by the
-- standalone searchetl-producer.
--
-- searchetl 消息检索 ETL 抽取水位（YUJ-4530 ETL→Kafka→ES indexer）。
-- 每个 message 分片表一行，记录已投递到 Kafka 的最大主键 id 水位。
--
-- 与 opanalytics 的 octo_etl_message_cursor 物理隔离（两条独立 ETL，各自游标，互不影响）。
-- 增量抽取按 PK `WHERE id>last_id ORDER BY id LIMIT batch` keyset 分页；水位只推进到
-- 「落库已超过 lag（稳定性滞后窗口）」的稳定前缀末尾，杜绝低 id 晚提交被游标越过的并发漏扫。
-- 撤回/删除态不走该游标（路线甲：读时回 MySQL join 过滤），本游标只跑正文一条流。
CREATE TABLE `octo_etl_es_cursor` (
  `shard_table` VARCHAR(64) NOT NULL          COMMENT 'message 分片表名 (message / message1 / ...)',
  `last_id`     BIGINT      NOT NULL DEFAULT 0 COMMENT '已投递到 Kafka 的最大 message.id 水位',
  `updated_at`  TIMESTAMP   NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '行更新时间',
  PRIMARY KEY (`shard_table`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='消息检索ETL抽取水位(searchetl)';

-- +migrate Down
DROP TABLE IF EXISTS `octo_etl_es_cursor`;
