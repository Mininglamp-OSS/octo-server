package message

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGroupSpaceAllows 固定 uk 树上群 / 子区单条读的归属判定（PR #713 review：
// Jerry-Xin blocker 2 / lml2468 P0「group and thread reads are not scoped to the
// key bound Space」）。
//
// 判定必须与 space_filter.go 的 decideConvKeepInSpace 群分支同口径，否则会话列表里
// 被隐藏的群能被单条直查读出来。用例按信号组合列全，重点是两条 fail-closed：
// 归属信号为空、以及默认 Space 查询失败（传 "" sentinel）时一律不放行。
func TestGroupSpaceAllows(t *testing.T) {
	cases := []struct {
		name           string
		groupSpaceID   string
		isExternal     bool
		sourceSpaceID  string
		bound          string
		defaultSpaceID string
		want           bool
	}{
		{
			name:         "群 space_id 精确匹配绑定空间",
			groupSpaceID: "sp_a", bound: "sp_a", defaultSpaceID: "sp_a",
			want: true,
		},
		{
			// 这就是 blocker 2 的形状：主人同时是 B 群成员，requireGroupMember 会放行，
			// 归属判定必须把它拦下。
			name:         "别的空间的群不得被本空间的 key 读到",
			groupSpaceID: "sp_b", bound: "sp_a", defaultSpaceID: "sp_a",
			want: false,
		},
		{
			name:         "外部成员按自己的 source_space_id 归属",
			groupSpaceID: "sp_b", isExternal: true, sourceSpaceID: "sp_a", bound: "sp_a", defaultSpaceID: "sp_a",
			want: true,
		},
		{
			name:         "外部成员的来源空间与群空间都不是绑定空间则拒绝",
			groupSpaceID: "sp_b", isExternal: true, sourceSpaceID: "sp_c", bound: "sp_a", defaultSpaceID: "sp_a",
			want: false,
		},
		{
			// 与 decideConvKeepInSpace 同序：群自身 space_id 命中优先于外部成员分支，
			// 所以外部成员身份不会把一个本就归属绑定空间的群判成不可读。
			name:         "群空间命中时外部成员身份不改变结论",
			groupSpaceID: "sp_a", isExternal: true, sourceSpaceID: "sp_c", bound: "sp_a", defaultSpaceID: "sp_a",
			want: true,
		},
		{
			name:         "未回填 space_id 的存量群归属调用者默认 Space",
			groupSpaceID: "", bound: "sp_a", defaultSpaceID: "sp_a",
			want: true,
		},
		{
			name:         "未回填 space_id 的存量群在非默认 Space 的 key 上不可读",
			groupSpaceID: "", bound: "sp_a", defaultSpaceID: "sp_default",
			want: false,
		},
		{
			name:         "外部成员来源空间为空时回落默认 Space",
			groupSpaceID: "sp_b", isExternal: true, sourceSpaceID: "", bound: "sp_a", defaultSpaceID: "sp_a",
			want: true,
		},
		{
			// 默认 Space 查询失败 → 调用方传 "" sentinel。置 bound 会让这条门恒真，
			// 把所有未归属的群向每个 Space 放开，故必须 fail-closed。
			name:         "默认 Space 查询失败时未归属的群不可读",
			groupSpaceID: "", bound: "sp_a", defaultSpaceID: "",
			want: false,
		},
		{
			name:         "绑定空间为空时一律不放行",
			groupSpaceID: "", bound: "", defaultSpaceID: "",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := groupSpaceAllows(tc.groupSpaceID, tc.isExternal, tc.sourceSpaceID, tc.bound, tc.defaultSpaceID)
			assert.Equal(t, tc.want, got,
				"groupSpaceAllows(group=%q, external=%v, source=%q, bound=%q, default=%q)",
				tc.groupSpaceID, tc.isExternal, tc.sourceSpaceID, tc.bound, tc.defaultSpaceID)
		})
	}
}

// TestPersonSpaceAllowsDefaultSpaceSentinelFailsClosed 固定 getPersonMessage 在默认
// Space 查询失败时传 "" sentinel 的语义（PR #713 review lml2468 P1）。
//
// 之前那里把 defaultSpaceID 置成当前请求 Space，于是 personSpaceAllows 的规则 2
// （无标签 DM 只在默认 Space 向前兼容）对任意请求 Space 恒真——一次默认 Space 查询
// 失败就把用户全部无标签 DM 历史放开到 key 绑定的那个 Space。sentinel 让同一分支
// 变成永假。
func TestPersonSpaceAllowsDefaultSpaceSentinelFailsClosed(t *testing.T) {
	const requested = "sp_a"

	assert.False(t, personSpaceAllows("", false, requested, ""),
		"默认 Space 查询失败时，无标签 DM 必须隐藏而不是按当前 Space 放行")
	assert.True(t, personSpaceAllows("", false, requested, requested),
		"默认 Space 解析成功且等于请求 Space 时，无标签 DM 仍按向前兼容放行（回归对照）")
	assert.True(t, personSpaceAllows(requested, false, requested, ""),
		"精确匹配的 DM 不受 sentinel 影响")
}
