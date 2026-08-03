package common

// =============================================================================
// 子区自动归档策略配置：env → system_settings 迁移与偏序约束
// （task inactive-hiding-user-control / P1）
//
// 迁移的核心承诺是**上线零行为变化**：不写任何 DB 行，解析结果与现网 env 逐字
// 相同。偏序约束（archive_days >= recent_filter_thread_days）保证「运维调大隐藏
// 窗口却被归档静默覆盖」这条危害链无法再形成。
// =============================================================================

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 三级回落：DB → env → 代码默认
// ---------------------------------------------------------------------------

// 无 DB 行、无 env → 代码默认（关闭 / 3 天）。这是「迁移不写 DB 行」时全新部署
// 看到的值。
func TestThreadAutoArchive_CodeDefaults(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	assert.False(t, s.ThreadAutoArchiveEnabled(), "默认关闭")
	assert.Equal(t, 3, s.ThreadAutoArchiveDays(), "默认 3 天")
}

// 无 DB 行、有 env → env 生效。这是现网（自动归档已开启）在本次上线瞬间看到的
// 值，必须与改动前完全一致。
func TestThreadAutoArchive_EnvFallback(t *testing.T) {
	t.Setenv(envThreadAutoArchiveEnabled, "true")
	t.Setenv(envThreadAutoArchiveDays, "7")

	s := newTestSystemSettings(t, nil)
	assert.True(t, s.ThreadAutoArchiveEnabled(), "DB 未配置时 env 生效")
	assert.Equal(t, 7, s.ThreadAutoArchiveDays())
}

// DB 行存在 → 覆盖 env。管理台写入后即为单一真源。
func TestThreadAutoArchive_DBOverridesEnv(t *testing.T) {
	t.Setenv(envThreadAutoArchiveEnabled, "true")
	t.Setenv(envThreadAutoArchiveDays, "7")

	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("thread", "auto_archive_enabled", "0", settingTypeBool, ""))
	require.NoError(t, s.db.upsert("thread", "auto_archive_days", "30", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	assert.False(t, s.ThreadAutoArchiveEnabled(), "DB 关闭覆盖 env 的开启")
	assert.Equal(t, 30, s.ThreadAutoArchiveDays())
}

// days=0 保留 env 语义：禁用时间阈值，但开关仍可为 on（RunOnce 内部短路）。
func TestThreadAutoArchive_ZeroDaysDisablesThresholdNotWorker(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("thread", "auto_archive_enabled", "1", settingTypeBool, ""))
	require.NoError(t, s.db.upsert("thread", "auto_archive_days", "0", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	assert.True(t, s.ThreadAutoArchiveEnabled())
	assert.Equal(t, 0, s.ThreadAutoArchiveDays(), "0 是合法的『禁用阈值』值，不得回退默认")
}

// DB 越界值（绕过管理 API 直接改表）回退代码默认 —— getIntClamped 的纵深防御。
func TestThreadAutoArchive_DBOutOfRangeClampsToDefault(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("thread", "auto_archive_days", "999999", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, 3, s.ThreadAutoArchiveDays())
}

// env 层必须逐字保留 legacy parseDays 语义：负值/非法回退默认，0 合法，其余
// 非负整数**原样生效**（含超过 settingIntMax 的值）。
//
// 上界只作用于 DB/管理台来源的值。对 env 加上界等于把现网既有配置**静默改写**：
// DM_THREAD_AUTO_ARCHIVE_DAYS=9999 的语义是「实质不归档」，上线时折成 3 天会
// 批量归档长期存活的子区 —— 与本次迁移「上线零行为变化」的承诺正相反
// （PR #679 review, Jerry-Xin）。
func TestThreadAutoArchive_EnvPreservesLegacySemantics(t *testing.T) {
	t.Setenv(envThreadAutoArchiveDays, "-5")
	assert.Equal(t, 3, newTestSystemSettings(t, nil).ThreadAutoArchiveDays(),
		"负值 env 回退默认（同 legacy parseDays）")

	t.Setenv(envThreadAutoArchiveDays, "abc")
	assert.Equal(t, 3, newTestSystemSettings(t, nil).ThreadAutoArchiveDays(),
		"非法 env 回退默认（同 legacy parseDays）")

	t.Setenv(envThreadAutoArchiveDays, "0")
	assert.Equal(t, 0, newTestSystemSettings(t, nil).ThreadAutoArchiveDays(),
		"0 是合法的『禁用阈值』，不得回退")

	t.Setenv(envThreadAutoArchiveDays, "9999")
	assert.Equal(t, 9999, newTestSystemSettings(t, nil).ThreadAutoArchiveDays(),
		"超 settingIntMax 的 env 值必须原样生效——上线不得改写现网既有配置")
}

// 对照面：上界仍然作用于 DB 值。绕过管理 API 直接改表写入的越界值不被原样服务，
// 而是回落到 env 层（此处 env 未设 → 代码默认）。
func TestThreadAutoArchive_DBValueStillBounded(t *testing.T) {
	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("thread", "auto_archive_days", "99999", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, 3, s.ThreadAutoArchiveDays(), "越界 DB 值回落，不被原样服务")
}

// env 越界 + DB 越界：DB 不可服务 → 回落 env 层，env 的 legacy 语义仍然成立。
func TestThreadAutoArchive_OutOfRangeDBFallsBackToEnvLayer(t *testing.T) {
	t.Setenv(envThreadAutoArchiveDays, "9999")
	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("thread", "auto_archive_days", "88888", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	assert.Equal(t, 9999, s.ThreadAutoArchiveDays(),
		"越界 DB 值回落到 env 层，而非代码默认——三级链条的既定语义")
}

func TestThreadAutoArchive_EnvGarbageStaysDisabled(t *testing.T) {
	t.Setenv(envThreadAutoArchiveEnabled, "yes-please")
	s := newTestSystemSettings(t, nil)
	assert.False(t, s.ThreadAutoArchiveEnabled(), "只有 true/1 开启，其余一律关闭")
}

// ---------------------------------------------------------------------------
// 偏序约束（两级衰减的定义式）
// ---------------------------------------------------------------------------

func TestViolatesThreadArchiveOrdering_ArchiveShorterThanRecent(t *testing.T) {
	assert.True(t, ViolatesThreadArchiveOrdering(ThreadArchiveOrdering{
		ArchiveEnabled: true, ArchiveDays: 3, RecentDays: 30,
	}), "归档窗口短于隐藏窗口 → 隐藏窗口不可观测，必须拒绝")
}

func TestViolatesThreadArchiveOrdering_EqualIsAllowed(t *testing.T) {
	assert.False(t, ViolatesThreadArchiveOrdering(ThreadArchiveOrdering{
		ArchiveEnabled: true, ArchiveDays: 3, RecentDays: 3,
	}), "相等允许——这正是现网默认状态，不能让约束一上线就拒掉存量")
}

func TestViolatesThreadArchiveOrdering_ArchiveLongerIsAllowed(t *testing.T) {
	assert.False(t, ViolatesThreadArchiveOrdering(ThreadArchiveOrdering{
		ArchiveEnabled: true, ArchiveDays: 14, RecentDays: 7,
	}), "归档晚于隐藏 → 两级衰减，各司其职")
}

// 三条「窗口不咬合」的短路，任何一条成立都不构成违规。
func TestViolatesThreadArchiveOrdering_InertConfigurations(t *testing.T) {
	cases := []struct {
		name string
		o    ThreadArchiveOrdering
	}{
		{"归档关闭", ThreadArchiveOrdering{ArchiveEnabled: false, ArchiveDays: 3, RecentDays: 30}},
		{"归档阈值为0", ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 0, RecentDays: 30}},
		{"隐藏窗口为0", ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 3, RecentDays: 0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.False(t, ViolatesThreadArchiveOrdering(c.o), "无窗口可被遮蔽，不应判违规")
		})
	}
}

// ThreadArchiveOrdering() 必须取当前生效值（含 env 回落），供写路径做
// merge(current, incoming) —— 只看 DB 会把 env 配好的现网误判成默认值。
func TestThreadArchiveOrdering_SnapshotUsesEffectiveValues(t *testing.T) {
	t.Setenv(envThreadAutoArchiveEnabled, "true")
	t.Setenv(envThreadAutoArchiveDays, "10")

	s := newTestSystemSettings(t, nil)
	require.NoError(t, s.db.upsert("sidebar", "recent_filter_thread_days", "5", settingTypeInt, ""))
	require.NoError(t, s.Reload())

	o := s.ThreadArchiveOrdering()
	assert.True(t, o.ArchiveEnabled, "env 开启必须体现在快照里")
	assert.Equal(t, 10, o.ArchiveDays, "env 天数必须体现在快照里")
	assert.Equal(t, 5, o.RecentDays)
	assert.False(t, ViolatesThreadArchiveOrdering(o))
}

// ---------------------------------------------------------------------------
// 管理写路径：偏序约束必须在**两个** key 的入口都强制
//
// 只在归档侧校验会留下一条绕行路径 —— 运维从 sidebar 侧把隐藏窗口调大，就能达到
// 约束本要阻止的状态。这几条用例锁死双向覆盖。
//
// 依赖 MySQL + Redis + WuKongIM（testutil.NewTestServer）。
// ---------------------------------------------------------------------------

func newSuperAdminServer(t *testing.T) (*wkhttp.WKHttp, *config.Context) {
	t.Helper()
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))
	// 出口清理：管理 handler 写的是进程级 SystemSettings 单例，本用例留下的
	// thread.* / sidebar.* 行会泄漏给后续测试。同 sidebar e2e 的处理。
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		_ = EnsureSystemSettings(ctx).Reload()
	})
	route := s.GetRoute()
	// testutil.NewTestServer 不注入 i18n renderer，兜底 renderer 只输出
	// {msg,status}。生产在 main.go 注入的是 i18n ErrorRenderer，输出带
	// error.code / error.details 的完整封套。要断言封套形状就必须在这里注入同一个
	// renderer，否则断的是兜底实现、与线上行为无关。
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer("zh-CN")))
	return route, ctx
}

func postSystemSetting(t *testing.T, route *wkhttp.WKHttp, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	return w
}

// 情形 A：同一批里同时写两个 key 且违反 → 拒绝。顺序无关（按最终状态校验）。
func TestManagerSystemSetting_OrderingRejectsSameBatchViolation(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_enabled","value":"1"},
		{"category":"thread","key":"auto_archive_days","value":"3"},
		{"category":"sidebar","key":"recent_filter_thread_days","value":"30"}
	]}`)
	assert.NotEqual(t, http.StatusOK, w.Code,
		"归档 3 天 < 隐藏 30 天，必须拒绝")

	// 同一批、相反顺序 —— 结果必须一致，运维不该背负「先调哪个」的隐式知识。
	w = postSystemSetting(t, route, `{"items":[
		{"category":"sidebar","key":"recent_filter_thread_days","value":"30"},
		{"category":"thread","key":"auto_archive_days","value":"3"},
		{"category":"thread","key":"auto_archive_enabled","value":"1"}
	]}`)
	assert.NotEqual(t, http.StatusOK, w.Code, "校验必须与 items 顺序无关")
}

// 情形 A':同批把两个值一起调成合法 → 通过。这正是被拒后运维的修复动作。
func TestManagerSystemSetting_OrderingAcceptsSameBatchRepair(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_enabled","value":"1"},
		{"category":"thread","key":"auto_archive_days","value":"30"},
		{"category":"sidebar","key":"recent_filter_thread_days","value":"7"}
	]}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// 情形 B：只写 sidebar 一侧，与 DB 存量比较后违反 → 同样必须拒绝。
// 这条是「双入口校验」的关键回归：只在 thread 侧校验的话这里会放行。
func TestManagerSystemSetting_OrderingRejectsFromSidebarSide(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	// 先建立合法存量：归档开启、7 天。
	w := postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_enabled","value":"1"},
		{"category":"thread","key":"auto_archive_days","value":"7"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// 再单独把隐藏窗口调到超过归档窗口 → 必须被拒。
	w = postSystemSetting(t, route, `{"items":[
		{"category":"sidebar","key":"recent_filter_thread_days","value":"30"}
	]}`)
	assert.NotEqual(t, http.StatusOK, w.Code,
		"从 sidebar 侧也必须拦住，否则约束可被绕过")
}

// 情形 B':只写 thread 一侧、与存量比较后违反 → 拒绝。
func TestManagerSystemSetting_OrderingRejectsFromThreadSide(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_enabled","value":"1"},
		{"category":"thread","key":"auto_archive_days","value":"30"},
		{"category":"sidebar","key":"recent_filter_thread_days","value":"14"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	w = postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_days","value":"3"}
	]}`)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

// 归档关闭时约束不咬合：任何隐藏窗口都合法，不得误拦。
func TestManagerSystemSetting_OrderingInertWhenArchiveDisabled(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_enabled","value":"0"},
		{"category":"thread","key":"auto_archive_days","value":"3"},
		{"category":"sidebar","key":"recent_filter_thread_days","value":"30"}
	]}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// 与本约束无关的 key 不得因这段逻辑付出任何代价，也不得被误拦。
func TestManagerSystemSetting_OrderingIgnoresUnrelatedKeys(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"sidebar","key":"recent_filter_group_days","value":"30"}
	]}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// 拒绝响应必须走 i18n 错误封套（含结构化 details），而非裸 c.JSON。
func TestManagerSystemSetting_OrderingRejectionUsesI18nEnvelope(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	w := postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_enabled","value":"1"},
		{"category":"thread","key":"auto_archive_days","value":"3"},
		{"category":"sidebar","key":"recent_filter_thread_days","value":"30"}
	]}`)
	require.NotEqual(t, http.StatusOK, w.Code)

	var body struct {
		Error struct {
			Code    string            `json:"code"`
			Details map[string]string `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	assert.Equal(t, "err.server.common.thread_archive_window_ordering", body.Error.Code)
	assert.Equal(t, "3", body.Error.Details["archive_days"])
	assert.Equal(t, "30", body.Error.Details["recent_days"])
}

// ---------------------------------------------------------------------------
// 空值（重置为默认）在 merge 时必须按重置后的值判定
//
// settingTypeInt 的空值是明文的「重置为默认」，normaliseBool 也接受空字符串。
// 若 merge 时把空值当作「保持现值」，清空一个高于隐藏窗口的归档天数就会悄悄落到
// 违规状态而不被拦 —— 正是这条守卫存在的理由。
// ---------------------------------------------------------------------------

func TestApplyOverlay_AbsentKeysKeepCurrent(t *testing.T) {
	cur := ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 30, RecentDays: 7}
	got := ApplyThreadArchiveOrderingOverlay(cur, map[string]string{})
	assert.Equal(t, cur, got)
}

func TestApplyOverlay_ExplicitValuesWin(t *testing.T) {
	cur := ThreadArchiveOrdering{ArchiveEnabled: false, ArchiveDays: 30, RecentDays: 7}
	got := ApplyThreadArchiveOrderingOverlay(cur, map[string]string{
		"thread.auto_archive_enabled":       "1",
		"thread.auto_archive_days":          "14",
		"sidebar.recent_filter_thread_days": "3",
	})
	assert.Equal(t, ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 14, RecentDays: 3}, got)
}

// 清空归档天数 → 回落 env/代码默认（3），而不是保留现值 30。
func TestApplyOverlay_EmptyArchiveDaysResetsToDefault(t *testing.T) {
	cur := ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 30, RecentDays: 14}
	got := ApplyThreadArchiveOrderingOverlay(cur, map[string]string{
		"thread.auto_archive_days": "",
	})
	assert.Equal(t, 3, got.ArchiveDays, "空值必须解析为重置后的默认值")
	assert.True(t, ViolatesThreadArchiveOrdering(got),
		"重置后 3 < 14 构成违规，守卫必须能看见——这正是把空值当『保持现值』会漏掉的场景")
}

func TestApplyOverlay_EmptyArchiveDaysHonoursEnv(t *testing.T) {
	t.Setenv(envThreadAutoArchiveDays, "20")
	cur := ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 30, RecentDays: 14}
	got := ApplyThreadArchiveOrderingOverlay(cur, map[string]string{
		"thread.auto_archive_days": "",
	})
	assert.Equal(t, 20, got.ArchiveDays, "重置回落到 env 层而非代码默认")
	assert.False(t, ViolatesThreadArchiveOrdering(got))
}

// 清空开关 → 回落 env。env 为 true 时不得被当成「关闭」而跳过校验。
func TestApplyOverlay_EmptyEnabledResetsToEnv(t *testing.T) {
	t.Setenv(envThreadAutoArchiveEnabled, "true")
	cur := ThreadArchiveOrdering{ArchiveEnabled: false, ArchiveDays: 3, RecentDays: 30}
	got := ApplyThreadArchiveOrderingOverlay(cur, map[string]string{
		"thread.auto_archive_enabled": "",
	})
	assert.True(t, got.ArchiveEnabled, "空值必须回落 env，而非被当作 false")
	assert.True(t, ViolatesThreadArchiveOrdering(got),
		"回落后归档开启且 3 < 30，必须判违规")
}

// 清空隐藏窗口 → 回落代码默认（3）；该键无 env 层。
func TestApplyOverlay_EmptyRecentDaysResetsToDefault(t *testing.T) {
	cur := ThreadArchiveOrdering{ArchiveEnabled: true, ArchiveDays: 7, RecentDays: 30}
	got := ApplyThreadArchiveOrderingOverlay(cur, map[string]string{
		"sidebar.recent_filter_thread_days": "",
	})
	assert.Equal(t, 3, got.RecentDays)
	assert.False(t, ViolatesThreadArchiveOrdering(got), "重置后 7 >= 3，合法")
}

// 端到端：通过管理 API 清空归档天数落到违规状态，必须被拒。
func TestManagerSystemSetting_OrderingRejectsResetToDefaultViolation(t *testing.T) {
	route, _ := newSuperAdminServer(t)

	// 建立合法存量：归档 30 天、隐藏 14 天。
	w := postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_enabled","value":"1"},
		{"category":"thread","key":"auto_archive_days","value":"30"},
		{"category":"sidebar","key":"recent_filter_thread_days","value":"14"}
	]}`)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	// 清空归档天数 → 重置为默认 3 → 3 < 14 违规，必须拒绝。
	w = postSystemSetting(t, route, `{"items":[
		{"category":"thread","key":"auto_archive_days","value":""}
	]}`)
	assert.NotEqual(t, http.StatusOK, w.Code,
		"重置为默认后落到违规状态，同样必须被拦")
}
