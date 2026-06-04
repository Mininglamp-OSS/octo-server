-- +migrate Up

-- #254 软删除复用 status 列引入 2=已删除 取值；列定义不变，仅刷新 COMMENT 让 schema
-- 自文档化。原 COMMENT 只写 0/1，运维直接读表结构可能把 status=2 当脏数据，手动
-- `UPDATE ... SET status=0 WHERE status=2` 会静默复活已删除 webhook（连同旧 token），
-- 与软删除"删除即撤销"的语义相悖。目标库 MySQL 8.0：仅改 COMMENT 的 MODIFY COLUMN
-- 走 INSTANT 算法、瞬时无锁，无需显式 pin ALGORITHM/LOCK。
ALTER TABLE `incoming_webhook`
  MODIFY COLUMN `status` SMALLINT NOT NULL DEFAULT 1 COMMENT '0=禁用,1=启用,2=已删除(软删除)';

-- +migrate Down
ALTER TABLE `incoming_webhook`
  MODIFY COLUMN `status` SMALLINT NOT NULL DEFAULT 1 COMMENT '0=禁用,1=启用';
