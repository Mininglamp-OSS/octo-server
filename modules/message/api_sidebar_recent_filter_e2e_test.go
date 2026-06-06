//go:build integration

package message

// End-to-end coverage for the configurable recent-tab activity filter
// (issue #289). Exercises the full seam that the unit tests stub out:
//
//	admin HTTP write  →  system_setting DB row  →  shared SystemSettings
//	snapshot reload   →  Sidebar.loadRecentCutoffs  →  buildRecentItems filter
//
// All test ctxs share one physical MySQL `test` database (same DSN), and the
// sidebar.* settings have no yaml fallback, so the singleton SystemSettings
// instance reads the rows we POST regardless of which ctx it was first bound
// to — the documented binding footgun does not apply here.
//
// Build-tagged `integration` (run with `go test -tags=integration`), matching
// api_sidebar_integration_test.go. These tests spin up testutil.NewTestServer
// against the shared `test` DB; running them in the default `go test ./...`
// Test job is order-fragile (a prior test in the package can leave the schema
// in a state where NewTestServer's module.Setup re-applies a migration whose
// table already exists and panics, crashing the whole package binary). The
// per-type filter logic is covered by the unit tests in api_sidebar_test.go,
// and the system_settings DB read/write path by the common package tests, both
// of which run in the default Test job; this file is the extra full-stack glue
// check.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const masterKeyEnvE2E = "OCTO_MASTER_KEY"

// TestE2E_RecentFilter_DefaultsReproduceLegacyBehaviour verifies that with no
// admin override, loadRecentCutoffs yields the historical windows: groups and
// threads = 3 days, DMs unfiltered.
func TestE2E_RecentFilter_DefaultsReproduceLegacyBehaviour(t *testing.T) {
	t.Setenv(masterKeyEnvE2E, "0123456789abcdef0123456789abcdef")
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	// Start from a known-good snapshot: the shared SystemSettings singleton may
	// hold sidebar.* rows written by a prior test (within the ~60s auto-reload
	// TTL). Reload after wiping the table so "no override" actually means
	// defaults here (PR #291 review, lml2468).
	require.NoError(t, commonapi.EnsureSystemSettings(ctx).Reload())

	sb := &Sidebar{ctx: ctx}
	now := time.Now()
	cutoffs := sb.loadRecentCutoffs(now)

	assert.Equal(t, daysCutoff(now, 3), cutoffs.group, "群默认 3 天窗口")
	assert.Equal(t, daysCutoff(now, 3), cutoffs.thread, "话题默认 3 天窗口")
	assert.Equal(t, int64(0), cutoffs.person, "DM 默认 0 = 不过滤")

	// And the filter behaves like today's hard-coded path.
	convs := []*config.SyncUserConversationResp{
		makeIMConv("g-new", common.ChannelTypeGroup.Uint8(), now.Add(-1*time.Hour).Unix()),
		makeIMConv("g-old", common.ChannelTypeGroup.Uint8(), now.Add(-73*time.Hour).Unix()),
		makeIMConv("dm-old", common.ChannelTypePerson.Uint8(), now.Add(-1000*time.Hour).Unix()),
	}
	items := buildRecentItems(convs, cutoffs, nil, nil, nil, "")
	ids := idSet(items)
	assert.True(t, ids["g-new"])
	assert.False(t, ids["g-old"], "默认 3 天窗口剔除超期群")
	assert.True(t, ids["dm-old"], "DM 不受窗口影响")
}

// TestE2E_RecentFilter_AdminOverrideTakesEffect writes per-type windows through
// the admin HTTP endpoint and asserts the running cutoffs + filter reflect them
// without any redeploy.
func TestE2E_RecentFilter_AdminOverrideTakesEffect(t *testing.T) {
	t.Setenv(masterKeyEnvE2E, "0123456789abcdef0123456789abcdef")
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	// Operator: return ALL groups (window off), keep a 3-day thread window, and
	// start filtering DMs at 7 days.
	body := `{"items":[` +
		`{"category":"sidebar","key":"recent_filter_group_days","value":"0"},` +
		`{"category":"sidebar","key":"recent_filter_thread_days","value":"3"},` +
		`{"category":"sidebar","key":"recent_filter_person_days","value":"7"}` +
		`]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	sb := &Sidebar{ctx: ctx}
	now := time.Now()
	cutoffs := sb.loadRecentCutoffs(now)
	assert.Equal(t, int64(0), cutoffs.group, "群窗口关闭 → cutoff 0")
	assert.Equal(t, daysCutoff(now, 3), cutoffs.thread)
	assert.Equal(t, daysCutoff(now, 7), cutoffs.person)

	convs := []*config.SyncUserConversationResp{
		makeIMConv("g-ancient", common.ChannelTypeGroup.Uint8(), now.Add(-5000*time.Hour).Unix()),
		makeIMConv("g1____t-old", common.ChannelTypeCommunityTopic.Uint8(), now.Add(-73*time.Hour).Unix()),
		makeIMConv("g1____t-new", common.ChannelTypeCommunityTopic.Uint8(), now.Add(-1*time.Hour).Unix()),
		makeIMConv("dm-6d", common.ChannelTypePerson.Uint8(), now.Add(-6*24*time.Hour).Unix()),
		makeIMConv("dm-8d", common.ChannelTypePerson.Uint8(), now.Add(-8*24*time.Hour).Unix()),
	}
	items := buildRecentItems(convs, cutoffs, nil, nil, nil, "")
	ids := idSet(items)
	assert.True(t, ids["g-ancient"], "群窗口关闭 → 远古群也返回")
	assert.False(t, ids["g1____t-old"], "话题 3 天窗口 → 剔除超期话题")
	assert.True(t, ids["g1____t-new"])
	assert.True(t, ids["dm-6d"], "DM 7 天窗口内保留")
	assert.False(t, ids["dm-8d"], "DM 超 7 天 → 剔除（数据驱动的 DM 过滤）")
}

func idSet(items []*SidebarItem) map[string]bool {
	ids := map[string]bool{}
	for _, it := range items {
		ids[it.TargetID] = true
	}
	return ids
}
