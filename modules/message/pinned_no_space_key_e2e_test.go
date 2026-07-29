//go:build integration

package message

// DB 依赖：无 Space key 时的置顶集合（PR #679 review, yujiawei）。
//
// 打 integration tag 与本模块其余 DB 测试一致 —— `testutil.NewTestServer` 在默认
// 构建里被同包多次调用时，会让 module.Setup 对已存在的表重跑迁移并 panic，拖垮
// 整个包的测试二进制（见 api_sidebar_recent_filter_e2e_test.go 的文件头说明）。
//
// 注意：本模块的 `-tags integration` 目前因既有 import cycle
// （api_card_action_test.go → app_bot → bot_api → messages_search → message）
// 无法编译，`origin/main` 同样如此。本用例因此暂时跑不起来，随该 cycle 修复后生效。

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 无 Space key 时的置顶集合（PR #679 review, yujiawei）
//
// 按 space_id='' 查会恒空 —— 写入侧拒绝空 Space，表里不存在这样的行 —— 于是置顶
// 豁免在这条路径上曾是死代码，而 keepDespiteRecentWindow 的文档把置顶描述为绝对
// 豁免。无 Space key 的响应本就跨 Space，置顶集合也应取该 uid 的全部。
// ---------------------------------------------------------------------------

func TestLoadPinnedChannelSet_NoSpaceKeyReturnsAllPinsForUID(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const uid = "pin-user"
	insert := func(spaceID, channelID string, channelType uint8) {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO user_pinned_channel (uid, space_id, channel_id, channel_type, sort_order, created_at) VALUES (?,?,?,?,?,?)",
			uid, spaceID, channelID, channelType, 0, time.Now(),
		).Exec()
		require.NoError(t, err)
	}
	insert("space-a", "g-a", common.ChannelTypeGroup.Uint8())
	insert("space-b", "g-b", common.ChannelTypeGroup.Uint8())
	// 另一个用户的置顶不得混入。
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO user_pinned_channel (uid, space_id, channel_id, channel_type, sort_order, created_at) VALUES (?,?,?,?,?,?)",
		"other-user", "space-a", "g-other", common.ChannelTypeGroup.Uint8(), 0, time.Now(),
	).Exec()
	require.NoError(t, err)

	// 带 Space key：只拿该 Space 的。
	scoped, err := loadPinnedChannelSet(ctx, uid, "space-a")
	require.NoError(t, err)
	assert.Len(t, scoped, 1)
	assert.Contains(t, scoped, channelKey("g-a", common.ChannelTypeGroup.Uint8()))

	// 不带 Space key：拿该 uid 的全部，且不跨用户。
	all, err := loadPinnedChannelSet(ctx, uid, "")
	require.NoError(t, err)
	assert.Len(t, all, 2, "无 Space key 时必须返回该 uid 的全部置顶，而非恒空")
	assert.Contains(t, all, channelKey("g-a", common.ChannelTypeGroup.Uint8()))
	assert.Contains(t, all, channelKey("g-b", common.ChannelTypeGroup.Uint8()))
	assert.NotContains(t, all, channelKey("g-other", common.ChannelTypeGroup.Uint8()),
		"绝不得跨用户泄漏置顶")
}
