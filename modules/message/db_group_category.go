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
	// CategoryID 是 "live" category id —— 已经过 group_category.status != 2
	// 过滤。NULL 含义统一为"该群当前未分类"，同时覆盖三种数据情况：
	//   (a) group_setting 行不存在 category_id（用户从未分配）；
	//   (b) group_setting.category_id 指向已被软删的 category
	//       （TOCTOU race 残留 / category_cleanup 未跑）；
	//   (c) group_setting.category_id 指向根本不存在的 category（异常残留）。
	// 三种情况下游一律按"未分类"处理 —— sidebar 不展示在 follow tab、
	// 物化路径不写 ext 行、API 不向客户端暴露 dangling id。这是 follow-tab
	// 整套机制（issue #151）依赖的 schema-app 不变量：sidebar 显示 / 物化 /
	// /v1/follow/sort 鉴权三处都按这个语义统一判定，不再有 stale-id 暴露。
	//
	// 实现上 SELECT 必须取 gc.category_id（JOIN 出来的字段），不是 gs.category_id
	// （持久化的 stale 字段）—— 二者只在 JOIN 命中时相等，JOIN miss（status=2 或
	// gc 不存在）时 gc.category_id 自然为 NULL，恰好对应 (b)(c) 两种 dangling 情况。
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
// 用 LEFT JOIN group_category：保留所有 gs 行（包括未分类的、指向已删 category
// 的），让 sidebar 能在一次查询里同时拿到分类内排序（gs.category_sort）和分类
// 之间排序（gc.sort）；JOIN miss 走 IFNULL 退回 0，无需第二次查询。
//
// JOIN 谓词同时绑定 `gc.uid = gs.uid`（PR #21 Round-4 review I4 by yujiawei）：
// 虽然 group_category.category_id 当前是全局唯一、且应只被 owner 的 group_setting
// 引用，但 LEFT JOIN 只匹配 category_id 时这条不变量是 *结构上未强制* 的隐式约束。
// 显式加 uid 谓词把约束变成 JOIN 自身的属性，避免未来 schema 演进（例如允许
// 跨用户共享 category）时静默走 stale category_group_sort=0 分支。
//
// SELECT gc.category_id 而非 gs.category_id（issue #151 review fix）：
// gs.category_id 是持久化字段，category 被软删后这里仍保留 stale 值（cleanup
// 路径会清，但 TOCTOU race 可能产生 dangling — modules/category 的
// MoveGroupToCategory_TOCTOU_DanglingReference 测试明确承认这种状态合法存在）。
// 取 JOIN 后的 gc.category_id：JOIN miss（gc.status=2 或 gc 不存在）时为 NULL，
// 恰好让 GroupCategorySetting.CategoryID 反映 "live" 语义。下游 buildFollowItems
// 和 sidebar 物化路径的 cs.CategoryID == nil → 跳过 都自动获得正确语义，
// 不再让 dangling category 的群进入 follow tab 或被物化（issue #151 review
// blocker — 之前 SELECT gs.category_id 让 sidebar 物化路径绕过软删校验）。
func (d *groupCategoryDB) QueryCategorySettingsByGroupNos(groupNos []string, uid string) ([]*GroupCategorySetting, error) {
	if len(groupNos) == 0 {
		return nil, nil
	}
	var results []*GroupCategorySetting
	_, err := d.session.Select(
		"gs.group_no",
		"gc.category_id",
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

// FilterDefaultFollowedGroups returns the subset of candidateGroupNos that the
// given uid has actually placed into a non-deleted category.  These are the
// "default-followed" groups per the follow-tab definition — see
// modules/conversation_ext/service.go DefaultFollowedGroupGuard for why this
// gate exists (issue #151 code review #1).
//
// Same "live category" predicate as QueryCategorySettingsByGroupNos above,
// expressed as INNER JOIN rather than LEFT JOIN:
//
//   - QueryCategorySettingsByGroupNos: LEFT JOIN, preserves uncategorized
//     rows so sidebar can render them with CategoryID=nil + CategoryGroupSort=0.
//   - FilterDefaultFollowedGroups (here): INNER JOIN, drops uncategorized
//     rows entirely because the guard only needs to know "is this group
//     live-categorized for this user" (boolean), not surface "no" rows.
//
// Both queries are the same schema-app invariant ("CategoryID is live IFF
// gc.status != 2"), expressed for two consumer shapes.  Keep the JOIN
// predicates in lockstep — diverging means one entry point would silently
// admit dangling refs the other rejects.  The `gc.uid = gs.uid` binding
// matches QueryCategorySettingsByGroupNos: protects against future schema
// evolution that allows category sharing across users.
//
// Returns an empty slice if no candidate matches.  Callers must NOT assume the
// result preserves input order.
func (d *groupCategoryDB) FilterDefaultFollowedGroups(uid string, candidateGroupNos []string) ([]string, error) {
	if len(candidateGroupNos) == 0 {
		return nil, nil
	}
	type row struct {
		GroupNo string `db:"group_no"`
	}
	var rows []*row
	_, err := d.session.Select("gs.group_no").
		From(dbr.I("group_setting").As("gs")).
		Join(
			dbr.I("group_category").As("gc"),
			"gs.category_id = gc.category_id AND gs.uid = gc.uid AND gc.status != 2",
		).
		Where("gs.uid = ? AND gs.group_no IN ?", uid, candidateGroupNos).
		Load(&rows)
	if err != nil {
		return nil, fmt.Errorf("filter default-followed groups: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GroupNo)
	}
	return out, nil
}
