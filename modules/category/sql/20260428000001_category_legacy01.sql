-- +migrate Up

-- 背景（GH octo-server#1228）：Phase 1 遗漏 — 创建 space / 加入 space 时未自动创建
-- 默认分类，导致已有 (uid, space_id) 对缺失 is_default=1 记录，GET
-- /v1/spaces/{id}/categories 返回 []，前端无法渲染会话列表。
--
-- 本迁移为所有现存 space_member（status=1）中尚无默认分类的 (uid, space_id)
-- 组合各补齐一条 is_default=1 的记录。
--
-- 幂等性：
--   * INSERT IGNORE 配合唯一索引 uk_uid_space_is_default (uid, space_id, is_default)
--     保证同一 (uid, space_id) 只会存在一条 is_default=1 记录；
--   * NOT EXISTS 子查询进一步排除已有默认分类的用户，避免不必要的写入尝试；
--   * 多次执行结果一致，可安全重跑。
--
-- 命名：使用与 category-20260418-01 一致的占位符 '__default__'，list 端在响应
-- 时会翻译为当前配置的展示名（DM_DEFAULT_CATEGORY_NAME / '默认分组' fallback）。

INSERT IGNORE INTO `group_category`
  (`category_id`, `space_id`, `uid`, `name`, `sort`, `status`, `is_default`, `created_at`, `updated_at`)
SELECT
  REPLACE(UUID(), '-', ''),
  sm.space_id,
  sm.uid,
  '__default__',
  0,
  1,
  1,
  NOW(),
  NOW()
FROM `space_member` sm
WHERE sm.status = 1
  AND NOT EXISTS (
    SELECT 1
    FROM `group_category` gc
    WHERE gc.uid = sm.uid
      AND gc.space_id = sm.space_id
      AND gc.is_default = 1
      AND gc.status = 1
  );

-- +migrate Down

-- 仅回滚由本迁移批量补齐且仍保持占位符名称、未被用户使用的默认分类行。
-- 已关联 group_setting 的分类保持原状，避免破坏用户数据。
DELETE gc FROM `group_category` gc
LEFT JOIN `group_setting` gs ON gs.category_id = gc.category_id
WHERE gc.is_default = 1
  AND gc.status = 1
  AND gc.name = '__default__'
  AND gs.category_id IS NULL;
