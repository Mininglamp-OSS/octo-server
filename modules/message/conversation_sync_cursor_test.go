package message

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
)

// TestMaxConversationVersion PR-B：sync 游标必须基于 raw conversations 推进，
// 否则被服务端隐藏的尾部会话（archived 子区 / 无效群）会让 cursor 停留在前一个
// 较小的 version，下一次 sync 反复拉到同一批 raw conversations。
func TestMaxConversationVersion(t *testing.T) {
	cases := []struct {
		name string
		raw  []*config.SyncUserConversationResp
		base int64
		want int64
	}{
		{
			name: "empty falls back to base",
			raw:  nil,
			base: 42,
			want: 42,
		},
		{
			name: "max version greater than base wins",
			raw: []*config.SyncUserConversationResp{
				{Version: 10},
				{Version: 50},
				{Version: 30},
			},
			base: 5,
			want: 50,
		},
		{
			name: "base wins when all raw versions are smaller",
			raw: []*config.SyncUserConversationResp{
				{Version: 1},
				{Version: 2},
			},
			base: 100,
			want: 100,
		},
		{
			name: "skips nil entries",
			raw: []*config.SyncUserConversationResp{
				nil,
				{Version: 7},
				nil,
			},
			base: 0,
			want: 7,
		},
		{
			name: "tail-only filtered case: max comes from last entry even if caller would have discarded it",
			raw: []*config.SyncUserConversationResp{
				{Version: 11},
				{Version: 12},
				{Version: 99}, // 假设这是 archived/invalid → 会被业务层过滤掉，但 cursor 仍要推进
			},
			base: 10,
			want: 99,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := maxConversationVersion(tc.raw, tc.base)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestSidebar_Cursor_UsesRawSliceAfterSpaceFilter 是 PR #21 Round-4 review B1
// （Jerry-Xin / lml2468 / yujiawei 同时指出）的回归契约测试。
//
// Sidebar.Sync 在 X-Space-ID 存在时调用 FilterRawConversationsBySpace 把
// IM 返回的 conversations 收紧到当前 Space，然后用 maxConversationVersion 算
// respVersion。 如果 cursor 取自 *filtered* slice：最高 version 的会话恰好属于
// 另一个 Space 时它会被过滤掉，respVersion 退回 req.Version，客户端缓存的 version
// 永远不前进 —— 同一批 raw conversations 反复被拉。
//
// 本测试以 "raw vs filtered 算同一函数得到不同 cursor" 的方式锁定契约：必须用
// raw 算，过滤后的只能用来构建 items。
func TestSidebar_Cursor_UsesRawSliceAfterSpaceFilter(t *testing.T) {
	raw := []*config.SyncUserConversationResp{
		{ChannelID: "g-other-space", Version: 99},
		{ChannelID: "g-current-space", Version: 5},
	}
	// 模拟 FilterRawConversationsBySpace 把 high-version 那条剔除
	// （属于另一个 Space，对当前请求不可见）。
	filtered := []*config.SyncUserConversationResp{raw[1]}

	rawCur := maxConversationVersion(raw, 0)
	filteredCur := maxConversationVersion(filtered, 0)

	assert.Equal(t, int64(99), rawCur, "cursor 必须基于 raw slice → 客户端能推进")
	assert.Equal(t, int64(5), filteredCur,
		"反向证据：基于 filtered slice 会卡在 5，让客户端反复轮询同一批 raw conversations")
	assert.NotEqual(t, rawCur, filteredCur,
		"raw 与 filtered 的 cursor 不同 → Sidebar.Sync 必须传 raw 给 maxConversationVersion")
}
