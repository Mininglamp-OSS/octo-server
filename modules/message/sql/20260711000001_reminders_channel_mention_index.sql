-- +migrate Up

-- 本迁移做两件事，顺序不可换：先归一 reminders/reminder_done 的 collation，再建索引。
--
-- 背景（GH #567 review, yujiawei/OctoBoooot/Jerry-Xin 一致 P1）：
-- 子区自动归档在 ArchiveStaleBatch 里新增了跨表关联
--   r.channel_id = CONCAT(t.group_no,'____',t.short_id)
-- thread.* 是 utf8mb4_general_ci，而 reminders/reminder_done 建表未指定 CHARSET/COLLATE，
-- MySQL 8+ 解析为 utf8mb4_0900_ai_ci。两者跨 collation 列比较会抛 Error 1267。
--
-- 曾尝试在查询里 pin `COLLATE utf8mb4_general_ci` 绕开 1267，但那会让 MySQL 无法使用下方
-- 以 channel_id 打头的索引（索引按列原 collation 排序，pin 成另一 collation 后 B-tree 有序性
-- 对不上，优化器放弃索引 → 退回对热表 reminders 的全表扫，正是索引要防的 O(threads×reminders)）。
-- 故改为**归一化列 collation**：把两表 CONVERT 到 utf8mb4_general_ci，与 thread 对齐——1267 与
-- 索引失效一并解决，且查询侧不再需要任何 COLLATE pin。这也是 base OSS-compat repair
-- (20260512000001) 对 thread 等表所做归一的同法，只是 reminders/reminder_done 当时未纳入
-- （彼时无跨表 JOIN，遗漏无害；本 PR 引入首个此类比较）。
--
-- 幂等 & 守卫：存储过程内先查 information_schema，已是 general_ci 或表不存在则跳过 CONVERT。
-- ⚠️ 运维提示：CONVERT TO CHARACTER SET 在 InnoDB 上是 COPY 重建并持表级元数据锁，reminders
-- 是热表，大数据量部署请择低峰执行。

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __octo_reminders_collation_repair;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __octo_reminders_collation_repair()
BEGIN
  DECLARE v_collation VARCHAR(64);
  DECLARE v_table VARCHAR(64);
  DECLARE v_done INT DEFAULT 0;
  DECLARE v_cur CURSOR FOR
    SELECT t FROM (
      SELECT 'reminders' AS t UNION ALL SELECT 'reminder_done'
    ) AS tables_to_repair;
  DECLARE CONTINUE HANDLER FOR NOT FOUND SET v_done = 1;
  OPEN v_cur;
  read_loop: LOOP
    FETCH v_cur INTO v_table;
    IF v_done = 1 THEN LEAVE read_loop; END IF;
    -- MAX() 保证表缺失时返回一行 NULL，NOT FOUND handler 只服务游标耗尽（同 base repair）。
    SELECT MAX(TABLE_COLLATION) INTO v_collation
      FROM information_schema.TABLES
      WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = v_table;
    IF v_collation IS NOT NULL AND v_collation <> 'utf8mb4_general_ci' THEN
      SET @sql = CONCAT('ALTER TABLE `', v_table, '` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci');
      PREPARE stmt FROM @sql;
      EXECUTE stmt;
      DEALLOCATE PREPARE stmt;
    END IF;
  END LOOP;
  CLOSE v_cur;
END;
-- +migrate StatementEnd

-- +migrate StatementBegin
CALL __octo_reminders_collation_repair();
-- +migrate StatementEnd

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __octo_reminders_collation_repair;
-- +migrate StatementEnd

-- 归一 collation 后，补一个以 channel_id 打头的复合索引，让 ArchiveStaleBatch 的相关子查询走
-- 点查而非全表扫。channel_id 高选择性打头；channel_type / reminder_type / is_deleted 收窄到
-- @我 且未软删的行。（现有 channel_uid_uidx(uid,...) 首列 uid 在该关联里不受约束，用不上。）
ALTER TABLE `reminders`
  ADD INDEX `idx_channel_type_rtype_deleted` (`channel_id`, `channel_type`, `reminder_type`, `is_deleted`);

-- +migrate Down

ALTER TABLE `reminders` DROP INDEX `idx_channel_type_rtype_deleted`;
-- collation 归一不回滚：与 base OSS-compat repair 同理，回滚需逐部署记录原始状态，不予支持。
