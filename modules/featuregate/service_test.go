package featuregate

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// 触发基础设施迁移；featuregate 自身的迁移由本包 init() 注册触发。
	_ "github.com/Mininglamp-OSS/octo-server/modules/base"
	_ "github.com/Mininglamp-OSS/octo-server/modules/common"
)

// TestMain 为 common.Setup 注入 32 字节 master key（本地直接 go test 也能跑）。
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		os.Setenv("OCTO_MASTER_KEY", "12345678901234567890123456789012")
	}
	os.Exit(m.Run())
}

// uniqueKey 生成只含 [a-z0-9_] 的 feature key（去掉 UUID 的连字符），既避免测试
// 间 Redis 缓存串扰，又能直接映射成合法的 env kill 变量名。
func uniqueKey(prefix string) string {
	return prefix + strings.ReplaceAll(util.GenerUUID(), "-", "")[:12]
}

func newTestSvc(t *testing.T) (*Service, *gateDB, string) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	svc := NewService(ctx)
	db := newDB(ctx)
	key := uniqueKey("test_gate_")
	t.Cleanup(func() { svc.Invalidate(key) })
	return svc, db, key
}

func TestService_NoRule_FailSemantics(t *testing.T) {
	svc, _, key := newTestSvc(t)
	ctx := context.Background()
	assert.False(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g1"}), "无规则 → create fail-closed")
	assert.True(t, svc.AllowPush(ctx, key), "无规则 → push fail-open")
}

func TestService_ModeOn(t *testing.T) {
	svc, db, key := newTestSvc(t)
	require.NoError(t, db.upsertRule(key, string(fg.ModeOn), 0, ""))
	ctx := context.Background()
	assert.True(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g1"}))
	assert.True(t, svc.AllowPush(ctx, key))
}

func TestService_ModeOff(t *testing.T) {
	svc, db, key := newTestSvc(t)
	require.NoError(t, db.upsertRule(key, string(fg.ModeOff), 0, ""))
	ctx := context.Background()
	assert.False(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g1"}), "off → create 拒")
	assert.False(t, svc.AllowPush(ctx, key), "off → push 确定性关停")
}

func TestService_Whitelist(t *testing.T) {
	svc, db, key := newTestSvc(t)
	require.NoError(t, db.upsertRule(key, string(fg.ModeWhitelist), 0, ""))
	require.NoError(t, db.addScope(key, fg.ScopeTypeGroup, "g_allow"))
	svc.Invalidate(key) // 确保读到刚写入的规则与白名单
	ctx := context.Background()
	assert.True(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g_allow"}), "白名单命中放行")
	assert.False(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g_other"}), "白名单未命中拒绝")
}

func TestService_EnvKillSwitch(t *testing.T) {
	svc, db, key := newTestSvc(t)
	require.NoError(t, db.upsertRule(key, string(fg.ModeOn), 0, ""))
	t.Setenv("DM_FT_"+strings.ToUpper(key)+"_KILL", "1")
	ctx := context.Background()
	assert.False(t, svc.AllowPush(ctx, key), "kill → push 拒")
	assert.False(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g1"}), "kill → create 拒")
}

// TestService_CacheInvalidation 验证 Redis 读缓存生效，且 Invalidate 后能秒级读到
// 新值：写入 on 并读取（缓存）→ 直接改 DB 为 off 但不失效 → 仍读旧值 on →
// Invalidate 后读到新值 off。
func TestService_CacheInvalidation(t *testing.T) {
	svc, db, key := newTestSvc(t)
	ctx := context.Background()

	require.NoError(t, db.upsertRule(key, string(fg.ModeOn), 0, ""))
	assert.True(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g1"}), "首次读到 on 并缓存")

	require.NoError(t, db.upsertRule(key, string(fg.ModeOff), 0, ""))
	assert.True(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g1"}), "未失效缓存应仍读旧值 on")

	svc.Invalidate(key)
	assert.False(t, svc.AllowCreate(ctx, key, fg.Dims{GroupNo: "g1"}), "失效后读到新值 off")
}
