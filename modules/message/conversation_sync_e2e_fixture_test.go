package message

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/require"
)

const masterKeyEnvConvE2E = "OCTO_MASTER_KEY"

// One long-lived fake WuKongIM server is shared by the default-lane manual
// unread tests and the integration-tagged conversation-filter tests. The
// tests are intentionally sequential and never call t.Parallel.
var (
	fakeIMOnce              sync.Once
	fakeIMMu                sync.Mutex
	fakeIMSrv               *httptest.Server
	fakeIMConvs             []*config.SyncUserConversationResp
	fakeIMCMDs              []string
	fakeIMCMDRecords        []fakeIMCMDRecord
	fakeIMFailCMD           string
	fakeIMAfterCMD          func(string)
	fakeIMSyncCalls         int
	fakeIMConversationCalls int
)

type fakeIMCMDRecord struct {
	CMD   string
	Param map[string]interface{}
}

func sharedFakeIM() *httptest.Server {
	fakeIMOnce.Do(func() {
		fakeIMSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if strings.HasSuffix(r.URL.Path, "/conversations") {
				fakeIMMu.Lock()
				fakeIMConversationCalls++
				imConversations := append([]*config.SyncUserConversationResp(nil), fakeIMConvs...)
				fakeIMMu.Unlock()
				conversations := make([]*config.ConversationResp, 0, len(imConversations))
				for _, conversation := range imConversations {
					if conversation == nil {
						continue
					}
					conversations = append(conversations, &config.ConversationResp{
						ChannelID:   conversation.ChannelID,
						ChannelType: conversation.ChannelType,
						Unread:      int64(conversation.Unread),
						Timestamp:   conversation.Timestamp,
					})
				}
				_, _ = w.Write([]byte(util.ToJson(conversations)))
				return
			}
			if strings.HasSuffix(r.URL.Path, "/conversation/sync") {
				fakeIMMu.Lock()
				fakeIMSyncCalls++
				imConversations := append([]*config.SyncUserConversationResp(nil), fakeIMConvs...)
				fakeIMMu.Unlock()
				_, _ = w.Write([]byte(util.ToJson(imConversations)))
				return
			}
			if strings.HasSuffix(r.URL.Path, "/message/send") {
				var req struct {
					Payload []byte `json:"payload"`
				}
				cmd := ""
				if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
					var payload struct {
						CMD   string                 `json:"cmd"`
						Param map[string]interface{} `json:"param"`
					}
					if err := json.Unmarshal(req.Payload, &payload); err == nil && payload.CMD != "" {
						cmd = payload.CMD
						fakeIMMu.Lock()
						fakeIMCMDs = append(fakeIMCMDs, payload.CMD)
						fakeIMCMDRecords = append(fakeIMCMDRecords, fakeIMCMDRecord{
							CMD:   payload.CMD,
							Param: payload.Param,
						})
						fakeIMMu.Unlock()
					}
				}
				fakeIMMu.Lock()
				failCMD := fakeIMFailCMD
				afterCMD := fakeIMAfterCMD
				fakeIMMu.Unlock()
				if afterCMD != nil {
					afterCMD(cmd)
				}
				if cmd == failCMD {
					http.Error(w, "injected command failure", http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"data":{}}`))
				return
			}
			_, _ = w.Write([]byte("{}"))
		}))
	})
	return fakeIMSrv
}

// setupConvSyncE2E wires the real message routes to the shared fake IM and the
// CI-provided MySQL/Redis services. It resets every persistent per-UID key used
// by these tests so the default lane remains deterministic under -shuffle.
func setupConvSyncE2E(t *testing.T, convs []*config.SyncUserConversationResp) (*server.Server, *config.Context) {
	t.Helper()
	t.Setenv(masterKeyEnvConvE2E, "0123456789abcdef0123456789abcdef")

	fakeIMMu.Lock()
	fakeIMConvs = convs
	fakeIMCMDs = nil
	fakeIMCMDRecords = nil
	fakeIMFailCMD = ""
	fakeIMAfterCMD = nil
	fakeIMSyncCalls = 0
	fakeIMConversationCalls = 0
	fakeIMMu.Unlock()
	imURL := sharedFakeIM().URL

	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	cfg := ctx.GetConfig()
	cfg.MessageSaveAcrossDevice = false
	cfg.WuKongIM.APIURL = imURL

	require.NoError(t, ctx.Cache().Set(
		cfg.Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))

	_ = ctx.GetRedisConn().Del("ratelimit:uid:" + testutil.UID)
	_ = ctx.GetRedisConn().Del("userMaxVersion:" + testutil.UID)
	_ = ctx.GetRedisConn().Del(conversationExtraLockKey(testutil.UID))

	require.NoError(t, commonapi.EnsureSystemSettings(ctx).Reload())

	// register.GetModules is process-global and retains the Context from the
	// first test server initialized in this package. Build a dedicated route
	// with a Conversation bound to this test's Context so command assertions do
	// not depend on package test order or accidentally call the real WuKongIM.
	s := server.New(ctx)
	s.GetRoute().UseGin(ctx.Tracer().GinMiddle())
	ctx.SetHttpRoute(s.GetRoute())
	NewConversation(ctx).Route(s.GetRoute())

	return s, ctx
}
