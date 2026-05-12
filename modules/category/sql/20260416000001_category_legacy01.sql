-- +migrate Up

-- 1. 迁移群聊关联：把挂在重复默认分组下的 group_setting 指向保留的分组（MIN(id)）
UPDATE group_setting gs
INNER JOIN group_category gc_remove ON gs.category_id = gc_remove.category_id
    AND gs.uid = gc_remove.uid
    AND gc_remove.is_default = 1 AND gc_remove.status = 1
INNER JOIN group_category gc_keep ON gc_keep.uid = gc_remove.uid
    AND gc_keep.space_id = gc_remove.space_id
    AND gc_keep.is_default = 1 AND gc_keep.status = 1
    AND gc_keep.id = (
        SELECT MIN(gc_min.id) FROM group_category gc_min
        WHERE gc_min.uid = gc_remove.uid
          AND gc_min.space_id = gc_remove.space_id
          AND gc_min.is_default = 1 AND gc_min.status = 1
    )
SET gs.category_id = gc_keep.category_id;

-- 2. 软删除重复默认分组（保留每组 MIN(id)），同时清除 is_default 避免唯一索引冲突
UPDATE group_category gc1
INNER JOIN group_category gc2
    ON gc1.uid = gc2.uid AND gc1.space_id = gc2.space_id
    AND gc1.is_default = 1 AND gc2.is_default = 1
    AND gc1.status = 1 AND gc2.status = 1
    AND gc1.id > gc2.id
SET gc1.status = 2, gc1.is_default = 0;

-- 3. is_default 改为 nullable：非默认分组存 NULL（MySQL 唯一索引不约束 NULL）
ALTER TABLE `group_category` MODIFY COLUMN `is_default` TINYINT DEFAULT NULL COMMENT '1=默认未分类分组（不可删除/重命名），NULL=普通分组';

UPDATE `group_category` SET `is_default` = NULL WHERE `is_default` = 0;

-- 4. 添加唯一索引：同一用户同一 Space 最多一个 is_default=1
ALTER TABLE `group_category` ADD UNIQUE KEY `uk_uid_space_is_default` (`uid`, `space_id`, `is_default`);

-- 5. 统一 group_category 表 collation，与数据库默认及其他表保持一致
ALTER TABLE `group_category` CONVERT TO CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;
