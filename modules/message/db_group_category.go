package message

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/gocraft/dbr/v2"
)

// groupCategoryDB 群组分类数据库操作
type groupCategoryDB struct {
	ctx     *config.Context
	session *dbr.Session
}

func newGroupCategoryDB(ctx *config.Context) *groupCategoryDB {
	return &groupCategoryDB{
		ctx:     ctx,
		session: ctx.DB(),
	}
}

// GroupCategorySetting 群组分类设置（来自 group_setting JOIN group_category）。
//
// PR #21 review (lml2468 blocker #3)：swagger 承诺 v2 SidebarItem.category_sort
// 来自 group_category.sort，且 /category/sort 接口也只更新 group_category.sort
// 并 bump follow_version。如果 sidebar 只读 group_setting.category_sort（类别
// 内排序），用户重排序类别后 sidebar 完全不变，与 contract 不符。本结构两个
// sort 字段一起读出：
//   - CategoryGroupSort  →  group_category.sort，category 之间的相对顺序
//     （也就是 SidebarItem.CategorySort 暴露给客户端的值）；
//   - IntraCategorySort  →  group_setting.category_sort，同类别内组之间的顺序
//     （sidebar 排序时作为二级 key，不暴露给客户端，避免破坏现有 schema）。
type GroupCategorySetting struct {
	GroupNo string
	// CategoryID 是 group_setting.category_id —— 用户给该群指定的类别 ID。
	CategoryID *string
	// CategorySort 是 group_setting.category_sort —— 类别内排序（v1 兼容字段，
	// v1 API_conversation 直接回显该值，故保留语义不变）。
	CategorySort int `db:"category_sort"`
	// CategoryGroupSort 是 group_category.sort —— 类别之间的排序权重，
	// 对应 swagger v2 sidebar 的 SidebarItem.category_sort 字段。
	CategoryGroupSort int `db:"category_group_sort"`
}

// QueryCategorySettingsByGroupNos 批量查询群组的分类设置。
//
// 用 LEFT JOIN group_category：当用户改了 group_setting.category_id 但目标分类
// 已删除时（gc.category_id IS NULL），CategoryGroupSort 退回到 0，
// 与 LEFT JOIN 的 NULL 一致；客户端把该群当作"未分类"展示。
//
// JOIN 谓词同时绑定 `gc.uid = gs.uid`（PR #21 Round-4 review I4 by yujiawei）：
// 虽然 group_category.category_id 当前是全局唯一、且应只被 owner 的 group_setting
// 引用，但 LEFT JOIN 只匹配 category_id 时这条不变量是 *结构上未强制* 的隐式约束。
// 显式加 uid 谓词把约束变成 JOIN 自身的属性，避免未来 schema 演进（例如允许
// 跨用户共享 category）时静默走 stale category_group_sort=0 分支。
func (d *groupCategoryDB) QueryCategorySettingsByGroupNos(groupNos []string, uid string) ([]*GroupCategorySetting, error) {
	if len(groupNos) == 0 {
		return nil, nil
	}
	var results []*GroupCategorySetting
	// JOIN 谓词除了 (gs.category_id, gs.uid) 双绑定外，还加 gc.status != 2
	// （PR #21 Round-6 P1 by yujiawei）：defense-in-depth，避免任何意外指向软删
	// 分类的 group_setting 行从 gc.sort 拿到 stale 值而不是走 LEFT JOIN miss 退到 0。
	_, err := d.session.Select(
		"gs.group_no",
		"gs.category_id",
		"IFNULL(gs.category_sort, 0) AS category_sort",
		"IFNULL(gc.sort, 0) AS category_group_sort",
	).
		From(dbr.I("group_setting").As("gs")).
		LeftJoin(dbr.I("group_category").As("gc"), "gs.category_id = gc.category_id AND gs.uid = gc.uid AND gc.status != 2").
		Where("gs.group_no IN ? AND gs.uid = ?", groupNos, uid).
		Load(&results)
	return results, err
}

// QueryCategorySortsByIDs 批量返回 group_category.sort（map[categoryID]sort）。
//
// Issue #41：DM 在 user_conversation_ext.dm_category_id 上引用 group_category.category_id；
// sidebar follow tab 排序需要把对应 category 的 sort 值写到 SidebarItem.CategorySort，
// 让带 category 的 DM 与同 category 的群同桶。
//
// 与 QueryCategorySettingsByGroupNos 共同遵守 uid 维度的隔离 + 软删过滤：
//   - uid 谓词阻止跨 user 读到他人的 category sort；
//   - status != 2 过滤掉软删除 category，让 DM 的 dm_category_id 退到默认 0 桶。
//
// 入参为空时直接返回空 map，避免触发 "IN ()" 错误。
func (d *groupCategoryDB) QueryCategorySortsByIDs(categoryIDs []string, uid string) (map[string]int, error) {
	if len(categoryIDs) == 0 {
		return map[string]int{}, nil
	}
	type row struct {
		CategoryID string `db:"category_id"`
		Sort       int    `db:"sort"`
	}
	var rows []*row
	_, err := d.session.Select("category_id", "IFNULL(sort, 0) AS sort").
		From("group_category").
		Where("category_id IN ? AND uid = ? AND status != 2", categoryIDs, uid).
		Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("query category sorts by ids: %w", err)
	}
	result := make(map[string]int, len(rows))
	for _, r := range rows {
		result[r.CategoryID] = r.Sort
	}
	return result, nil
}
