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

// TestPersonSpaceAllowsUnlabelledDMIsDefaultSpaceOnly 固定 personSpaceAllows 规则 2
// 与 getPersonMessage 默认 Space 错误路径的关系。
//
// 该错误路径按这一族调用点的统一约定 fail-open（defaultSpaceID = 请求 Space），四个
// 调用点逐字同款；space_filter.go 的 "" sentinel 服务的是会话列表谓词，不是这一族。
// 本用例把两种取值各自的后果都锁住：将来若整族改成 fail-closed，改的人能立刻看到差别
// 落在哪一条规则上。
func TestPersonSpaceAllowsUnlabelledDMIsDefaultSpaceOnly(t *testing.T) {
	const requested = "sp_a"

	assert.True(t, personSpaceAllows("", false, requested, requested),
		"无标签 DM 在默认 Space 等于请求 Space 时按向前兼容保留（现状 fail-open 走的就是这条）")
	assert.False(t, personSpaceAllows("", false, requested, "sp_other"),
		"无标签 DM 在非默认 Space 不保留")
	assert.False(t, personSpaceAllows("", false, requested, ""),
		"若整族改用 \"\" sentinel，同一条无标签 DM 变为隐藏——这是约定变更的落点")
	assert.True(t, personSpaceAllows(requested, false, requested, ""),
		"精确匹配的 DM 与 defaultSpaceID 取值无关")
}
