package incomingwebhook

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"

	// 迁移依赖链：与 api_test.go 一致，缺任一模块 module.Setup 会在跨模块 ALTER 失败。
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	_ "github.com/Mininglamp-OSS/octo-server/modules/common"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
)

// TestDisableByGroupNo 验证群解散级联禁用的 DB 层语义：disableByGroupNo 把指定群下
// 所有 webhook 的 status 翻 0，且不影响其他群。这是 #246 列出的便宜回归——只验 DB
// 层（disband fail-closed 的核心），不依赖完整 HTTP / 鉴权 harness（那部分仍 gated
// 在 #17 上）。
func TestDisableByGroupNo(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	d := newDB(ctx)

	groupA := "g_" + util.GenerUUID()[:12]
	groupB := "g_" + util.GenerUUID()[:12]

	// groupA：两个启用 webhook；groupB：一个启用 webhook（对照，验证不被误伤）。
	mustInsertWebhook(t, d, groupA)
	mustInsertWebhook(t, d, groupA)
	bID := mustInsertWebhook(t, d, groupB)

	assert.NoError(t, d.disableByGroupNo(groupA))

	listA, err := d.queryByGroupNo(groupA)
	assert.NoError(t, err)
	assert.Len(t, listA, 2)
	for _, m := range listA {
		assert.Equalf(t, 0, m.Status, "groupA webhook %s must be disabled", m.WebhookID)
	}

	mb, err := d.queryByWebhookID(bID)
	assert.NoError(t, err)
	assert.NotNil(t, mb)
	assert.Equal(t, 1, mb.Status, "groupB webhook must stay enabled (no cross-group impact)")
}

func mustInsertWebhook(t *testing.T, d *incomingWebhookDB, groupNo string) string {
	t.Helper()
	m := &incomingWebhookModel{
		WebhookID: generateWebhookID(),
		TokenHash: "h",
		GroupNo:   groupNo,
		Name:      "wh",
		Status:    1,
	}
	assert.NoError(t, d.insertWithQuota(m, 100))
	return m.WebhookID
}
