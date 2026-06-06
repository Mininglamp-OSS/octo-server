//go:build integration

package message

// End-to-end coverage for the opt-in recent activity-window filter on
// POST /v1/conversation/sync (issue #294). Unlike the pure-logic unit tests in
// conversation_recent_filter_test.go, these drive the REAL HTTP handler
// (syncUserConversation) through the registered route, with WuKongIM mocked by
// an httptest server (the IM result is the only external seam the handler can't
// reach in CI). MySQL + Redis must be up (same as the sidebar recent-filter
// e2e).
//
// IM mock: IMSyncUserConversation is a concrete *config.Context method that
// POSTs to {WuKongIM.APIURL}/conversation/sync. We point that URL at a local
// httptest server returning a canned conversation slice — no interface seam
// needed. DM-only conversations keep the seeding minimal: groups would require
// membership/detail rows, and DMs skip the group-membership / thread / space
// branches entirely (space_id omitted ⇒ no Space filter).
//
// Build-tagged `integration` (run with `go test -tags=integration`), matching
// api_sidebar_recent_filter_e2e_test.go — these spin up testutil.NewTestServer
// against the shared `test` DB and are order-fragile in the default `go test`
// job. The filter logic itself is covered by the default-job unit tests; this
// file is the full-stack glue check the reviewer asked for.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const masterKeyEnvConvE2E = "OCTO_MASTER_KEY"

// One long-lived fake WuKongIM server for the whole file. Per-test httptest
// servers proved fragile: testutil.NewTestServer binds the registered handler's
// ctx to the FIRST server's config, so a per-test APIURL (whose server is then
// closed) leaves later tests dialling a dead port. A single never-closed server
// with a mutable response slice keeps the URL stable and alive across all tests.
// Tests run sequentially (no t.Parallel), so the shared slice needs no lock.
var (
	fakeIMOnce  sync.Once
	fakeIMSrv   *httptest.Server
	fakeIMConvs []*config.SyncUserConversationResp
)

func sharedFakeIM() *httptest.Server {
	fakeIMOnce.Do(func() {
		fakeIMSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/conversation/sync") {
				_, _ = w.Write([]byte(util.ToJson(fakeIMConvs)))
				return
			}
			_, _ = w.Write([]byte("{}"))
		}))
	})
	return fakeIMSrv
}

// dmIMConv builds a DM conversation as IMSyncUserConversation would return it,
// with a single non-deleted recent message so the handler keeps it (the build
// loop drops conversations whose Recents end up empty).
func dmIMConv(channelID string, ts, version int64, seq uint32) *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID:   channelID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Unread:      0,
		Timestamp:   ts,
		LastMsgSeq:  int64(seq),
		Version:     version,
		Recents: []*config.MessageResp{
			{
				MessageID:   version*1000 + int64(seq),
				MessageSeq:  seq,
				ClientMsgNo: channelID + "-" + strconv.FormatUint(uint64(seq), 10),
				FromUID:     channelID,
				ChannelID:   channelID,
				ChannelType: common.ChannelTypePerson.Uint8(),
				Timestamp:   int32(ts),
				IsDeleted:   0,
				Payload:     []byte(`{"type":1,"content":"hi"}`),
			},
		},
	}
}

// setupConvSyncE2E wires a test server whose IM is the shared fake (returning
// the given conversation slice), with a SuperAdmin token (needed for the
// system_setting write) that also authenticates the conversation/sync call. It
// resets the per-uid rate-limit bucket and the cursor Redis keys so each test
// starts clean, and pins MessageSaveAcrossDevice off so the syncack
// cursor-persist path is exercised deterministically.
func setupConvSyncE2E(t *testing.T, convs []*config.SyncUserConversationResp) (*server.Server, *config.Context) {
	t.Helper()
	t.Setenv(masterKeyEnvConvE2E, "0123456789abcdef0123456789abcdef")

	// Point the IM at the shared fake and load this test's conversation slice
	// before any NewTestServer so the very first ctx already sees the live URL.
	fakeIMConvs = convs
	imURL := sharedFakeIM().URL

	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	cfg := ctx.GetConfig()
	cfg.MessageSaveAcrossDevice = false
	cfg.WuKongIM.APIURL = imURL

	// SuperAdmin token: authenticates /v1/conversation/sync AND authorizes the
	// /v1/manager/common/system_setting write. Format uid@name@role (token_parser).
	require.NoError(t, ctx.Cache().Set(
		cfg.Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	// Clean per-uid rate-limit bucket + cursor keys (persist in Redis across runs,
	// not cleared by CleanAllTables).
	_ = ctx.GetRedisConn().Del("ratelimit:uid:" + testutil.UID)
	_ = ctx.GetRedisConn().Del("userMaxVersion:" + testutil.UID)

	// Start from a known settings snapshot (the singleton may hold sidebar.* rows
	// from a prior test within the ~60s auto-reload TTL).
	require.NoError(t, commonapi.EnsureSystemSettings(ctx).Reload())

	return s, ctx
}

// writePersonWindowDays sets sidebar.recent_filter_person_days via the admin HTTP
// endpoint and reloads the shared snapshot so the next handler call sees it.
func writePersonWindowDays(t *testing.T, s *server.Server, ctx *config.Context, days int) {
	t.Helper()
	body := `{"items":[{"category":"sidebar","key":"recent_filter_person_days","value":"` +
		strconv.Itoa(days) + `"}]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/manager/common/system_setting", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NoError(t, commonapi.EnsureSystemSettings(ctx).Reload())
}

// callConversationSync POSTs to the real route and returns the decoded channel
// IDs present in the response conversation list.
func callConversationSync(t *testing.T, s *server.Server, recentFilter bool) []string {
	t.Helper()
	body := `{"msg_count":1,"device_uuid":"dev-e2e","recent_filter":` +
		strconv.FormatBool(recentFilter) + `}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/conversation/sync", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var wrap struct {
		Conversations []*SyncUserConversationResp `json:"conversations"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &wrap))
	ids := make([]string, 0, len(wrap.Conversations))
	for _, c := range wrap.Conversations {
		ids = append(ids, c.ChannelID)
	}
	return ids
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestE2E_ConvSync_FilterIsOptIn: with a person window configured but no
// recent_filter flag, the stale DM is still returned — the filter must NOT apply
// to default (mobile/desktop) callers. This is the core regression guard.
func TestE2E_ConvSync_FilterIsOptIn(t *testing.T) {
	now := time.Now()
	convs := []*config.SyncUserConversationResp{
		dmIMConv("dm-fresh", now.Add(-1*time.Hour).Unix(), 100, 11),
		dmIMConv("dm-stale", now.Add(-8*24*time.Hour).Unix(), 200, 12),
	}
	s, ctx := setupConvSyncE2E(t, convs)
	writePersonWindowDays(t, s, ctx, 7) // 7-day DM window configured

	ids := callConversationSync(t, s, false) // flag omitted/false
	assert.True(t, contains(ids, "dm-fresh"), "fresh DM returned")
	assert.True(t, contains(ids, "dm-stale"),
		"stale DM still returned when recent_filter is off — filter is opt-in")
}

// TestE2E_ConvSync_OptInFiltersStaleDM: same window, recent_filter=true ⇒ the
// stale DM (older than the 7-day window) is dropped, fresh DM kept. Proves the
// admin-configured window now reaches the conversation/sync path.
func TestE2E_ConvSync_OptInFiltersStaleDM(t *testing.T) {
	now := time.Now()
	convs := []*config.SyncUserConversationResp{
		dmIMConv("dm-fresh", now.Add(-1*time.Hour).Unix(), 100, 11),
		dmIMConv("dm-stale", now.Add(-8*24*time.Hour).Unix(), 200, 12),
	}
	s, ctx := setupConvSyncE2E(t, convs)
	writePersonWindowDays(t, s, ctx, 7)

	ids := callConversationSync(t, s, true)
	assert.True(t, contains(ids, "dm-fresh"), "fresh DM kept")
	assert.False(t, contains(ids, "dm-stale"), "stale DM dropped by the 7-day window")
}

// TestE2E_ConvSync_CursorNotStalledByFilter: the dropped (stale) DM carries the
// HIGHEST version. The response list omits it, but the cursor must still advance
// past it — otherwise the client re-syncs the same batch forever. We observe the
// cursor through the syncack → userMaxVersion Redis write.
func TestE2E_ConvSync_CursorNotStalledByFilter(t *testing.T) {
	now := time.Now()
	const staleMaxVersion int64 = 200
	convs := []*config.SyncUserConversationResp{
		dmIMConv("dm-fresh", now.Add(-1*time.Hour).Unix(), 100, 11),
		dmIMConv("dm-stale", now.Add(-8*24*time.Hour).Unix(), staleMaxVersion, 12),
	}
	s, ctx := setupConvSyncE2E(t, convs)
	writePersonWindowDays(t, s, ctx, 7)

	ids := callConversationSync(t, s, true)
	require.False(t, contains(ids, "dm-stale"), "precondition: stale DM filtered out of the list")

	// Ack persists the in-memory cursor to Redis (userMaxVersion:{uid}).
	wAck := httptest.NewRecorder()
	ackReq, _ := http.NewRequest("POST", "/v1/conversation/syncack",
		strings.NewReader(`{"device_uuid":"dev-e2e"}`))
	ackReq.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(wAck, ackReq)
	require.Equal(t, http.StatusOK, wAck.Code, wAck.Body.String())

	got, err := ctx.GetRedisConn().GetString("userMaxVersion:" + testutil.UID)
	require.NoError(t, err)
	require.NotEmpty(t, got, "cursor must be persisted")
	gotVer, err := strconv.ParseInt(got, 10, 64)
	require.NoError(t, err)
	assert.Equal(t, staleMaxVersion, gotVer,
		"cursor advanced to the filtered-out conversation's version (computed from raw)")
}
