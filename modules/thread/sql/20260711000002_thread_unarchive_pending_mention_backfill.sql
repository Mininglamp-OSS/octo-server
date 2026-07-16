-- +migrate Up

-- 一次性历史存量清理（配合 GH #566 / ArchiveStaleBatch 的 NOT EXISTS 谓词）。
--
-- 背景：在归档谓词加入「NOT EXISTS(未处理 per-uid @提及)」之前，时间归档只看
-- last_message_at，会把「有人 @我、我尚未处理」的陈旧子区误归档。谓词修复后不再产生
-- **新的**误归档，但上线前被老逻辑错误归档、且现在仍挂着未处理 @我 的历史存量需一次性
-- 捞回 active。稳态不再有此类行，故不在 worker 里常驻反向扫描（那会在只增不减的
-- 「全部已归档子区」集合上持续全表探 reminders，形成随时间恶化的数据库压力）。
--
-- 跨模块表守卫：本 backfill 同时引用 thread（thread 模块）与 reminders/reminder_done
-- （message 模块）。在按模块隔离的单元测试环境里另一模块的表可能尚未建；用存储过程 +
-- INFORMATION_SCHEMA 判断三张表是否齐备，缺任一则 no-op，避免隔离测试因表缺失 panic。
-- 生产环境所有模块 migration 一起跑，三表齐备，正常执行。（存储过程包装同时规避
-- sql-migrate MySQL 驱动 no multiStatements 的限制，与 base OSS-compat repair 同法。）
--
-- version = version + 1：迁移在 SQL 层无法调用 GenSeq，用每行自增让 thread.version 严格大于
-- 原值。注意：sidebar/conversation 的增量 sync 游标推进依据的是 WuKongIM conversation
-- version，并非 thread.version（见 modules/message/api_sidebar.go、api_conversation.go）；
-- 故本迁移只保证 DB 里 thread 状态被正确翻回 active，已推进过会话游标的在线客户端不一定
-- 被这条迁移单独唤醒（下次正常 conversation sync 或该子区新事件时收敛）。thread.version
-- 自增仍是必要的：任何直接依赖 thread.version 的读路径（如 thread 列表 by version）能感知。

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __octo_backfill_unarchive_pending_mention;
-- +migrate StatementEnd

-- +migrate StatementBegin
CREATE PROCEDURE __octo_backfill_unarchive_pending_mention()
BEGIN
  DECLARE v_tbl_cnt INT DEFAULT 0;
  SELECT COUNT(*) INTO v_tbl_cnt
  FROM INFORMATION_SCHEMA.TABLES
  WHERE TABLE_SCHEMA = DATABASE()
    AND TABLE_NAME IN ('thread', 'reminders', 'reminder_done');
  IF v_tbl_cnt = 3 THEN
    UPDATE `thread` t
    SET t.status = 1, t.version = t.version + 1, t.updated_at = NOW()
    WHERE t.status = 2
      AND EXISTS (
        SELECT 1 FROM `reminders` r
        LEFT JOIN `reminder_done` rd ON rd.reminder_id = r.id AND rd.uid = r.uid
        WHERE r.channel_id = CONCAT(t.group_no, '____', t.short_id)
          -- 跨表 collation：reminders/reminder_done 已由 message 模块迁移 20260711000001（全局排序
          -- 在本 thread backfill 20260711000002 之前）归一到 utf8mb4_general_ci，与 thread.* 对齐，
          -- 故无需 COLLATE pin（pin 反而会废掉 idx_channel_type_rtype_deleted）。
          AND r.channel_type = 5           -- common.ChannelTypeCommunityTopic
          AND r.reminder_type = 1          -- ReminderTypeMentionMe（有人@我）
          AND r.uid <> ''                  -- 排除 @所有人 广播行（uid=''）
          AND r.is_deleted = 0
          AND rd.id IS NULL                -- 该被@的人尚未处理自己的提及
      );
  END IF;
END;
-- +migrate StatementEnd

-- +migrate StatementBegin
CALL __octo_backfill_unarchive_pending_mention();
-- +migrate StatementEnd

-- +migrate StatementBegin
DROP PROCEDURE IF EXISTS __octo_backfill_unarchive_pending_mention;
-- +migrate StatementEnd

-- +migrate Down

-- 不可逆：无法区分「本迁移捞回的行」与「用户/其它路径本就置为 active 的行」，
-- 强行回滚会误归档用户主动激活的子区。存量清理是幂等业务修复，回滚为 no-op。
SELECT 1;
