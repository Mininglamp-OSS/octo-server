-- +migrate Up

-- octo_sidebar_section is the only ordering authority for the two peer kinds
-- shown in the follow sidebar: personal categories and Projects. group_category.sort
-- remains for rollback compatibility but is no longer read by application code.
--
-- All timestamps are application-written UTC on normal writes. The UTC_TIMESTAMP
-- calls below exist only for this one-time backfill, where no application request
-- owns the rows being copied.
CREATE TABLE `octo_sidebar_section` (
  `id`           BIGINT UNSIGNED  NOT NULL AUTO_INCREMENT,
  `uid`          VARCHAR(40)      NOT NULL DEFAULT '' COMMENT '个人排序的用户',
  `space_id`     VARCHAR(40)      NOT NULL DEFAULT '' COMMENT '所属 Space',
  `section_type` TINYINT UNSIGNED NOT NULL             COMMENT '1=category 2=project',
  `ref_id`       VARCHAR(40)      NOT NULL DEFAULT '' COMMENT 'category_id 或 project_id',
  `sort`         INT              NOT NULL DEFAULT 0  COMMENT '同一用户同一 Space 的统一顺序',
  `status`       TINYINT UNSIGNED NOT NULL DEFAULT 1   COMMENT '1=显示 2=隐藏保留',
  `created_at`   DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入',
  `updated_at`   DATETIME(3)      NOT NULL             COMMENT 'UTC；应用侧写入',
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_octo_sidebar_section_ref` (`uid`, `space_id`, `section_type`, `ref_id`),
  KEY `idx_octo_sidebar_section_order` (`uid`, `space_id`, `status`, `sort`, `id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci COMMENT='用户侧边栏顶层分区顺序';

-- Existing categories retain their old relative order. INSERT IGNORE and the
-- unique key make the backfill safe to retry; NOT EXISTS avoids useless writes.
INSERT IGNORE INTO `octo_sidebar_section`
  (`uid`, `space_id`, `section_type`, `ref_id`, `sort`, `status`, `created_at`, `updated_at`)
SELECT
  gc.uid,
  gc.space_id,
  1,
  gc.category_id,
  gc.sort,
  1,
  UTC_TIMESTAMP(3),
  UTC_TIMESTAMP(3)
FROM `group_category` gc
WHERE gc.status = 1
  AND NOT EXISTS (
    SELECT 1
    FROM `octo_sidebar_section` ss
    WHERE ss.uid = gc.uid
      AND ss.space_id = gc.space_id
      AND ss.section_type = 1
      AND ss.ref_id = gc.category_id
  );

-- Active Project members receive one entry per Project. Project rows begin after
-- all existing entries in their user/Space list; p.id makes the initial sequence
-- deterministic while later drags are written directly to this table.
INSERT IGNORE INTO `octo_sidebar_section`
  (`uid`, `space_id`, `section_type`, `ref_id`, `sort`, `status`, `created_at`, `updated_at`)
SELECT
  pm.uid,
  p.space_id,
  2,
  p.project_id,
  IFNULL((
    SELECT MAX(ss2.sort)
    FROM `octo_sidebar_section` ss2
    WHERE ss2.uid = pm.uid
      AND ss2.space_id = p.space_id
      AND ss2.status = 1
  ), -1) + ROW_NUMBER() OVER (PARTITION BY pm.uid, p.space_id ORDER BY p.id),
  1,
  UTC_TIMESTAMP(3),
  UTC_TIMESTAMP(3)
FROM `octo_project_member` pm
INNER JOIN `octo_project` p
  ON p.project_id = pm.project_id
  AND p.space_id = pm.space_id
WHERE pm.status = 1
  AND pm.removing = 0
  AND p.status = 1
  AND NOT EXISTS (
    SELECT 1
    FROM `octo_sidebar_section` ss
    WHERE ss.uid = pm.uid
      AND ss.space_id = p.space_id
      AND ss.section_type = 2
      AND ss.ref_id = p.project_id
  );

-- Project groups cannot also be present in a personal manual category. This is a
-- one-way data repair: the Project entry remains visible, so the group moves rather
-- than disappearing. The join is legacy-to-legacy and deliberately carries no COLLATE.
UPDATE `group_setting` gs
INNER JOIN `group` g ON g.group_no = gs.group_no
SET gs.category_id = NULL,
    gs.category_sort = 0
WHERE g.project_id <> ''
  AND gs.category_id IS NOT NULL;

-- +migrate Down

-- The cleanup above intentionally is not reversed: restoring a stale manual
-- assignment for a Project group would reintroduce the invariant this migration fixes.
DROP TABLE IF EXISTS `octo_sidebar_section`;
