package incomingwebhook

import (
	"context"
	"testing"
	"time"

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

// TestSoftDelete 验证软删除（#254）的 DB 层语义：deleteByWebhookID 把行标记为
// statusDeleted 而非物理删除——记录仍可被 queryByWebhookID 解析（供历史消息渲染），
// 但从 queryByGroupNo 列表隐藏。
func TestSoftDelete(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	d := newDB(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	whID := mustInsertWebhook(t, d, groupNo)

	assert.NoError(t, d.deleteByWebhookID(whID))

	// 行仍在，status=statusDeleted（display datasource 据此仍能渲染发送者名/头像）。
	m, err := d.queryByWebhookID(whID)
	assert.NoError(t, err)
	assert.NotNil(t, m, "soft-deleted row must remain for historical message rendering")
	if m != nil {
		assert.Equal(t, statusDeleted, m.Status)
	}

	// 管理列表隐藏已删除项。
	list, err := d.queryByGroupNo(groupNo)
	assert.NoError(t, err)
	assert.Empty(t, list, "soft-deleted webhook must not appear in management list")
}

// TestSoftDelete_FreesQuota 验证软删除释放每群配额：填满配额后软删一个应能再建一个。
func TestSoftDelete_FreesQuota(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	d := newDB(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	const max = 2

	id1 := mustInsertWebhookWithMax(t, d, groupNo, max)
	mustInsertWebhookWithMax(t, d, groupNo, max)

	// 配额已满：第三个被拒。
	over := &incomingWebhookModel{WebhookID: generateWebhookID(), TokenHash: "h", GroupNo: groupNo, Name: "wh", Status: statusEnabled}
	assert.ErrorIs(t, d.insertWithQuota(over, max), ErrQuotaExceeded)

	// 软删一个释放配额后可再建一个。
	assert.NoError(t, d.deleteByWebhookID(id1))
	again := &incomingWebhookModel{WebhookID: generateWebhookID(), TokenHash: "h", GroupNo: groupNo, Name: "wh", Status: statusEnabled}
	assert.NoError(t, d.insertWithQuota(again, max), "soft-delete must free per-group quota")
}

// TestDisableByGroupNo_SkipsDeleted 验证群解散级联禁用不会"复活"已软删除的 webhook：
// disableByGroupNo 把启用项翻为 statusDisabled，但必须跳过 statusDeleted 行（否则
// status 2→0 会让它重新出现在列表、重新占配额）。
func TestDisableByGroupNo_SkipsDeleted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	d := newDB(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	enabledID := mustInsertWebhook(t, d, groupNo)
	deletedID := mustInsertWebhook(t, d, groupNo)
	assert.NoError(t, d.deleteByWebhookID(deletedID))

	assert.NoError(t, d.disableByGroupNo(groupNo))

	enabled, err := d.queryByWebhookID(enabledID)
	assert.NoError(t, err)
	assert.NotNil(t, enabled)
	if enabled != nil {
		assert.Equal(t, statusDisabled, enabled.Status, "enabled webhook must be disabled on disband")
	}

	deleted, err := d.queryByWebhookID(deletedID)
	assert.NoError(t, err)
	assert.NotNil(t, deleted)
	if deleted != nil {
		assert.Equal(t, statusDeleted, deleted.Status, "soft-deleted webhook must NOT be revived to disabled on disband")
	}
}

// TestUpdateFields_SkipsDeleted 锁定并发复活漏洞的根因防线（#254 follow-up）：
// queryManageable 是非事务读，与 updateFields 之间有 TOCTOU 窗口——PUT 先通过校验，
// 随后并发 DELETE 把行软删，最后 PUT 的 updateFields 把 status 写回 1 即"复活"已删除
// webhook（重回列表 + 旧 token 复活）。updateFields 必须带 status != statusDeleted
// 守卫，对已删除行的写入一律落空。这里直接对一个已软删除的行调 updateFields，断言
// status / 业务字段都【不】被写入。
func TestUpdateFields_SkipsDeleted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	d := newDB(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	whID := mustInsertWebhook(t, d, groupNo)
	assert.NoError(t, d.deleteByWebhookID(whID)) // status -> statusDeleted

	// 模拟竞态尾部：对已删除行尝试 PUT status=1 + 改名。
	assert.NoError(t, d.updateFields(whID, map[string]interface{}{
		"status": statusEnabled,
		"name":   "revived",
	}))

	m, err := d.queryByWebhookID(whID)
	assert.NoError(t, err)
	assert.NotNil(t, m)
	if m != nil {
		assert.Equal(t, statusDeleted, m.Status, "soft-deleted webhook must NOT be revived by a racing updateFields")
		assert.NotEqual(t, "revived", m.Name, "soft-deleted webhook business fields must not be writable")
	}
}

// TestUpdateFields_TokenHash_SkipsDeleted 覆盖 regenerate 路径：不得给已删除 webhook
// 轮换 token_hash（否则会向调用方返回一个"看似有效"实则指向已删除行的新 token）。
func TestUpdateFields_TokenHash_SkipsDeleted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	d := newDB(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	whID := mustInsertWebhook(t, d, groupNo)
	before, err := d.queryByWebhookID(whID)
	assert.NoError(t, err)
	assert.NotNil(t, before)
	assert.NoError(t, d.deleteByWebhookID(whID))

	assert.NoError(t, d.updateFields(whID, map[string]interface{}{"token_hash": "newhash"}))

	after, err := d.queryByWebhookID(whID)
	assert.NoError(t, err)
	assert.NotNil(t, after)
	if after != nil && before != nil {
		assert.Equal(t, before.TokenHash, after.TokenHash, "token_hash of a soft-deleted webhook must not be rotated")
	}
}

// TestMarkUsed_SkipsDeleted 锁定纵深防御一致性：push 成功后的异步审计若在行被并发软
// 删除后才执行，markUsed 不得再给已删除 webhook 记账（call_count 不增）。
func TestMarkUsed_SkipsDeleted(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	d := newDB(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	whID := mustInsertWebhook(t, d, groupNo)
	assert.NoError(t, d.deleteByWebhookID(whID))

	assert.NoError(t, d.markUsed(context.Background(), whID, time.Now()))

	m, err := d.queryByWebhookID(whID)
	assert.NoError(t, err)
	assert.NotNil(t, m)
	if m != nil {
		assert.Equal(t, int64(0), m.CallCount, "markUsed must not bump a soft-deleted webhook")
	}
}

func mustInsertWebhook(t *testing.T, d *incomingWebhookDB, groupNo string) string {
	t.Helper()
	return mustInsertWebhookWithMax(t, d, groupNo, 100)
}

func mustInsertWebhookWithMax(t *testing.T, d *incomingWebhookDB, groupNo string, max int) string {
	t.Helper()
	m := &incomingWebhookModel{
		WebhookID: generateWebhookID(),
		TokenHash: "h",
		GroupNo:   groupNo,
		Name:      "wh",
		Status:    statusEnabled,
	}
	assert.NoError(t, d.insertWithQuota(m, max))
	return m.WebhookID
}
