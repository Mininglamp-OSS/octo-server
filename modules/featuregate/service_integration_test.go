package featuregate

import (
	"context"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"github.com/stretchr/testify/require"
)

// newServiceFixture 起一个真实 ctx（MySQL + Redis），返回 Service 与其 DB 句柄。
// 每个用例用独立的 feature_key，避免相互串扰；跨运行的 Redis 残留由
// newIntegrationCtx 统一清理。
func newServiceFixture(t *testing.T) (*Service, *gateDB, *config.Context) {
	t.Helper()
	ctx := newIntegrationCtx(t)
	svc := NewService(ctx)
	db := newDB(ctx)
	return svc, db, ctx
}

// seedRule 写一条规则并清掉它的缓存，保证后续读走的是刚写的值。
func seedRule(t *testing.T, svc *Service, db *gateDB, key, mode string, percent int, bucketBy string) {
	t.Helper()
	require.NoError(t, db.upsertRule(key, mode, percent, bucketBy, "test", "tester"))
	svc.Invalidate(key)
}

func seedScope(t *testing.T, svc *Service, db *gateDB, key, scopeType, scopeID string) {
	t.Helper()
	require.NoError(t, db.addScope(key, scopeType, scopeID, "tester"))
	svc.Invalidate(key)
}

// TestServiceLoadsScopesInPercentMode 是本次语义变更最重要的一条防线，且**必须**
// 走 Service 而不是 pkg/featuregate 的纯函数测试。
//
// 纯函数层照不到的那行代码是 Service 里「哪些 mode 需要加载白名单」的条件。初版
// 写作 `if rule.Mode == ModeWhitelist { loadScopes(...) }`；当 percent 也开始支持
// 白名单后，漏改这一处的症状与语义没改时**一模一样**——白名单写得进去、读路径拿到
// 空 scopes、判定静默忽略它。Evaluate 的单测全绿，线上却在放量切档时把内测人员
// 整批甩掉。
//
// percent=0 让「放行」只可能来自白名单，排除分桶巧合。
func TestServiceLoadsScopesInPercentMode(t *testing.T) {
	svc, db, _ := newServiceFixture(t)
	const key = "fgtest_percent_scopes"

	seedRule(t, svc, db, key, string(fg.ModePercent), 0, fg.ScopeTypeUser)
	seedScope(t, svc, db, key, fg.ScopeTypeUser, "u_allow")

	allow, ok := svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u_allow"})
	require.True(t, ok, "白名单命中不应被当作存储故障")
	require.True(t, allow, "percent 模式下白名单必须生效——若为 false，多半是 Service 没在 percent 模式加载 scopes")

	allow, ok = svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u_other"})
	require.True(t, ok)
	require.False(t, allow, "percent=0 且不在白名单的用户必须被拒")

	// AllowCreate 走同一条加载路径，一并钉住。
	require.True(t, svc.AllowCreate(context.Background(), key, fg.Dims{UID: "u_allow"}))
	require.False(t, svc.AllowCreate(context.Background(), key, fg.Dims{UID: "u_other"}))
}

// TestServiceWhitelistAcrossModes 覆盖白名单的作用域矩阵，其中 off 不可穿透是
// 止血语义的硬边界。
func TestServiceWhitelistAcrossModes(t *testing.T) {
	svc, db, _ := newServiceFixture(t)

	cases := []struct {
		name  string
		key   string
		mode  fg.Mode
		want  bool
		scope bool // 是否写入命中当前用户的白名单
	}{
		{name: "whitelist 命中放行", key: "fgtest_wl_hit", mode: fg.ModeWhitelist, scope: true, want: true},
		{name: "whitelist 未命中拒绝", key: "fgtest_wl_miss", mode: fg.ModeWhitelist, scope: false, want: false},
		{name: "percent 0% 白名单仍放行", key: "fgtest_pc_wl", mode: fg.ModePercent, scope: true, want: true},
		{name: "off 不可被白名单穿透", key: "fgtest_off_wl", mode: fg.ModeOff, scope: true, want: false},
		{name: "on 全放", key: "fgtest_on", mode: fg.ModeOn, scope: false, want: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedRule(t, svc, db, tc.key, string(tc.mode), 0, fg.ScopeTypeUser)
			if tc.scope {
				seedScope(t, svc, db, tc.key, fg.ScopeTypeUser, "u1")
			}
			allow, ok := svc.AllowDisplay(context.Background(), tc.key, fg.Dims{UID: "u1"})
			require.True(t, ok)
			require.Equal(t, tc.want, allow)
		})
	}
}

// TestAllowDisplayRuleMissingIsDefiniteOff 钉住「规则不存在」与「存储故障」的区分。
//
// 规则不存在必须是 ok=true + allow=false（确定性的关）。若把它做成 ok=false（省略），
// 客户端会一直保留旧值，灰度就永远关不掉。
func TestAllowDisplayRuleMissingIsDefiniteOff(t *testing.T) {
	svc, _, _ := newServiceFixture(t)

	allow, ok := svc.AllowDisplay(context.Background(), "fgtest_never_registered", fg.Dims{UID: "u1"})
	require.True(t, ok, "规则不存在是确定性结论，不能报成存储故障")
	require.False(t, allow, "未配置即关（fail-closed）")
}

// TestAllowDisplayKillSwitchIsDefiniteOff 钉住 kill switch 必须走确定性 false 而
// 不是省略。
//
// 若 kill 走省略，客户端会保留旧值——对一个已经放量的功能按下紧急开关，界面永远
// 不会收敛，止血在展示面上完全失效。
func TestAllowDisplayKillSwitchIsDefiniteOff(t *testing.T) {
	svc, db, _ := newServiceFixture(t)
	const key = "fgtest_killed"

	seedRule(t, svc, db, key, string(fg.ModeOn), 0, fg.ScopeTypeUser)

	allow, ok := svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"})
	require.True(t, ok)
	require.True(t, allow, "前置条件：kill 之前应当放行")

	t.Setenv(KillSwitchEnv(key), "1")

	allow, ok = svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"})
	require.True(t, ok, "kill switch 必须是确定性结论，绝不能表现为存储故障（省略 → 客户端保留旧值 → 关不掉）")
	require.False(t, allow)

	// kill 对另外两个调用端同样是硬拒。
	require.False(t, svc.AllowCreate(context.Background(), key, fg.Dims{UID: "u1"}))
	require.False(t, svc.AllowPush(context.Background(), key))
}

// TestAllowDisplayDimensionUnavailableFailsClosed 是读侧兜底：规则要求的分桶维度
// 在展示端点提供不了时必须 fail-closed，而不是按空串照算。
//
// 按空串照算的后果是所有用户落进同一个桶，管理台显示的 50% 会静默变成全体开或
// 全体关——一个无任何报错的错配。写侧另有拦截，但直接改库能绕过写侧，故两侧都要。
func TestAllowDisplayDimensionUnavailableFailsClosed(t *testing.T) {
	svc, db, _ := newServiceFixture(t)
	const key = "fgtest_dim_missing"

	// 模拟「绕过管理端直接改库」：客户端可见的场景下配了 group 维度。
	seedRule(t, svc, db, key, string(fg.ModePercent), 100-1, fg.ScopeTypeGroup)

	allow, ok := svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"}) // 无 GroupNo
	require.True(t, ok, "维度不可用是确定性判定结论，不是存储故障")
	require.False(t, allow, "分桶维度缺失必须 fail-closed，不得按空串照算")
}

// TestAllowPushFailOpenSemantics 钉住 push 端的非对称 fail 策略未被本次改动带偏：
// 只有显式 off（与 env kill）才拒，其余一律放行。
func TestAllowPushFailOpenSemantics(t *testing.T) {
	svc, db, _ := newServiceFixture(t)

	require.True(t, svc.AllowPush(context.Background(), "fgtest_push_unregistered"),
		"未注册规则对 push 必须 fail-open")

	seedRule(t, svc, db, "fgtest_push_off", string(fg.ModeOff), 0, fg.ScopeTypeGroup)
	require.False(t, svc.AllowPush(context.Background(), "fgtest_push_off"),
		"显式 off 是管理员主动关停，必须拒")

	seedRule(t, svc, db, "fgtest_push_wl", string(fg.ModeWhitelist), 0, fg.ScopeTypeGroup)
	require.True(t, svc.AllowPush(context.Background(), "fgtest_push_wl"),
		"push 不做维度灰度：非 off 一律放行")
}

// TestInvalidateRefreshesCachedRule 验证写后失效：改了规则再 Invalidate，下一次读
// 必须看到新值（而不是等 TTL）。这是「秒级多实例一致」赖以成立的机制。
func TestInvalidateRefreshesCachedRule(t *testing.T) {
	svc, db, _ := newServiceFixture(t)
	const key = "fgtest_invalidate"

	seedRule(t, svc, db, key, string(fg.ModeOn), 0, fg.ScopeTypeGroup)
	allow, ok := svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"})
	require.True(t, ok)
	require.True(t, allow)

	// 只改库、不失效缓存：仍应读到旧值（证明缓存确实生效，否则下一步的断言没有意义）。
	require.NoError(t, db.upsertRule(key, string(fg.ModeOff), 0, fg.ScopeTypeGroup, "test", "tester"))
	allow, _ = svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"})
	require.True(t, allow, "未失效前应仍读到缓存里的旧规则")

	svc.Invalidate(key)
	allow, ok = svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"})
	require.True(t, ok)
	require.False(t, allow, "Invalidate 后必须立刻读到新规则")
}

// TestNilRuleCachedAsSentinel 验证未注册 key 会被缓存成 nil 哨兵，避免每次请求
// 穿透到 DB —— 展示端点会对注册表里每个 key 各评估一次，穿透代价是线性放大的。
func TestNilRuleCachedAsSentinel(t *testing.T) {
	svc, db, _ := newServiceFixture(t)
	const key = "fgtest_sentinel"

	allow, ok := svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"})
	require.True(t, ok)
	require.False(t, allow)

	// 直接写库但不失效缓存：若哨兵生效，这一步不应被看到。
	require.NoError(t, db.upsertRule(key, string(fg.ModeOn), 0, fg.ScopeTypeGroup, "test", "tester"))
	allow, _ = svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"})
	require.False(t, allow, "nil 哨兵未生效：未注册 key 每次都在穿透 DB")

	svc.Invalidate(key)
	allow, _ = svc.AllowDisplay(context.Background(), key, fg.Dims{UID: "u1"})
	require.True(t, allow)
}
