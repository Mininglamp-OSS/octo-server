package webhook

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"

	// Migration chain + incomingwebhook's BussDataSource.ChannelGet registration
	// (its init() via register.AddModule). Without these the push path can't
	// resolve an iwh_ sender's display name. Mirrors modules/incomingwebhook
	// db_test.go's import set; none of them import modules/webhook (no cycle).
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	_ "github.com/Mininglamp-OSS/octo-server/modules/common"
	_ "github.com/Mininglamp-OSS/octo-server/modules/group"
	_ "github.com/Mininglamp-OSS/octo-server/modules/incomingwebhook"
	_ "github.com/Mininglamp-OSS/octo-server/modules/robot"
	_ "github.com/Mininglamp-OSS/octo-server/modules/space"
	_ "github.com/Mininglamp-OSS/octo-server/modules/user"
)

func TestMain(m *testing.M) {
	// common.Setup (run by module.Setup inside NewTestServer) requires a 32-byte
	// master key; matches modules/incomingwebhook/api_test.go.
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		_ = os.Setenv("OCTO_MASTER_KEY", "12345678901234567890123456789012")
	}
	os.Exit(m.Run())
}

// insertWebhookRow seeds a minimal incoming_webhook row (all other columns have
// NOT NULL DEFAULT ”) and returns the synthetic sender UID (iwh_...).
func insertWebhookRow(t *testing.T, ctx *config.Context, groupNo, name string) string {
	t.Helper()
	whID := "iwh_" + strings.ReplaceAll(util.GenerUUID(), "-", "")
	_, err := ctx.DB().InsertInto("incoming_webhook").
		Columns("webhook_id", "token_hash", "group_no", "name", "status").
		Values(whID, "h", groupNo, name, 1).Exec()
	assert.NoError(t, err)
	return whID
}

// TestGetAndCacheShowNameForFromUID_Webhook_GroupMiss is the end-to-end regression
// for the original bug: an offline group push from an incoming-webhook sender
// (iwh_, absent from the `user` table) must render the webhook's display name
// instead of an empty sender. Exercises the cache-MISS path: DB miss → fallback
// resolve via the datasource registry → cache the resolved name.
func TestGetAndCacheShowNameForFromUID_Webhook_GroupMiss(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	whID := insertWebhookRow(t, ctx, groupNo, "GH Bot")
	toUID := "u_" + util.GenerUUID()[:12]

	key := fmt.Sprintf("%s%s-%s@%s", nameCachePrefix, whID, toUID, groupNo)
	assert.NoError(t, ctx.GetRedisConn().Del(key)) // ensure cache miss

	msg := msgOfflineNotify{}
	msg.FromUID = whID
	msg.ToUID = toUID
	msg.ChannelID = groupNo
	msg.ChannelType = common.ChannelTypeGroup.Uint8()

	name, err := getAndCacheShowNameForFromUID(msg, ctx)
	assert.NoError(t, err)
	assert.Equal(t, "GH Bot", name, "incoming webhook 发送者名应在推送里解析出来")

	// resolved name must be cached so subsequent pushes hit the cache
	cached, err := ctx.GetRedisConn().Hget(key, "name")
	assert.NoError(t, err)
	assert.Equal(t, "GH Bot", cached)
}

// TestGetAndCacheShowNameForFromUID_Webhook_CacheHitStaleEmptyRepaired covers the
// cache-HIT repair path (repairEmptyCachedName): an entry cached with an empty
// name before the fix must be re-resolved and repaired in place on hit, rather
// than serving the stale empty name until the TTL expires.
func TestGetAndCacheShowNameForFromUID_Webhook_CacheHitStaleEmptyRepaired(t *testing.T) {
	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	groupNo := "g_" + util.GenerUUID()[:12]
	whID := insertWebhookRow(t, ctx, groupNo, "GH Bot")
	toUID := "u_" + util.GenerUUID()[:12]

	key := fmt.Sprintf("%s%s-%s@%s", nameCachePrefix, whID, toUID, groupNo)
	// Simulate a stale empty name cached by the pre-fix code.
	assert.NoError(t, ctx.GetRedisConn().Hmset(key, "name", "", "remark", "", "name_in_group", ""))

	msg := msgOfflineNotify{}
	msg.FromUID = whID
	msg.ToUID = toUID
	msg.ChannelID = groupNo
	msg.ChannelType = common.ChannelTypeGroup.Uint8()

	name, err := getAndCacheShowNameForFromUID(msg, ctx)
	assert.NoError(t, err)
	assert.Equal(t, "GH Bot", name, "命中旧空名应触发兜底并就地修复")

	cached, err := ctx.GetRedisConn().Hget(key, "name")
	assert.NoError(t, err)
	assert.Equal(t, "GH Bot", cached, "缓存的 name 字段应被就地修复")
}
