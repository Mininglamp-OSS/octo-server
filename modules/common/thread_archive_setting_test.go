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

// env 越界同样回退默认：env 层不得偷渡一个管理写路径本会拒绝的值。
func TestThreadAutoArchive_EnvOutOfRangeFallsBackToDefault(t *testing.T) {
	t.Setenv(envThreadAutoArchiveDays, "-5")
	s := newTestSystemSettings(t, nil)
	assert.Equal(t, 3, s.ThreadAutoArchiveDays(), "负值 env 回退默认")

	t.Setenv(envThreadAutoArchiveDays, "99999")
	s2 := newTestSystemSettings(t, nil)
	assert.Equal(t, 3, s2.ThreadAutoArchiveDays(), "超上限 env 回退默认")
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
// 依赖 MySQL + Redis + WuKongIM（testutil.NewTestServer），本地无依赖时跳过，
// 由 CI 执行。
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
	return s.GetRoute(), ctx
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
