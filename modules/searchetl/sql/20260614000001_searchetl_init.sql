-- +migrate Up

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
