package incomingwebhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// TestResolveWebhookDisplayName_ForPush 验证离线推送链路能解析出 incoming webhook 虚拟
// 发送者（iwh_xxx，user 表里没有行）的展示名。推送侧用 user.ResolveWebhookDisplayName
// 兜底，它经 BussDataSource.ChannelGet 注册链命中本模块的 datasource（init 已注册）。
func TestResolveWebhookDisplayName_ForPush(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	whID := mustInsertWebhook(t, newDB(ctx), groupNo) // Name 固定为 "wh"

	name, err := user.ResolveWebhookDisplayName(ctx, whID)
	assert.NoError(t, err)
	assert.Equal(t, "wh", name, "webhook 发送者应能解析出展示名供推送使用")

	// 不存在的 iwh_（已删除/伪造）→ 各模块均 ErrDatasourceNotProcess → 干净的"无人处理"
	// 语义：("", nil)，调用方据此保持空名、不报错（注意：非 iwh_ 的未知 uid 会被 user 模块
	// 的 datasource 当作"用户不存在"返回真实 error，不是这里要测的 not-found 路径）。
	name, err = user.ResolveWebhookDisplayName(ctx, "iwh_"+util.GenerUUID()[:12])
	assert.NoError(t, err)
	assert.Empty(t, name)
}
