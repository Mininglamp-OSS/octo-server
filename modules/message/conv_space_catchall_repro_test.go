//go:build integration

package message

// End-to-end coverage for the issue #484 follow-up (task conv-space-catchall-484):
// the two deterministic cross-Space paths in the recent-conversation list, driven
// through the REAL registered handlers against MySQL/Redis with WuKongIM mocked.
//
//  1. Default-Space DM catch-all: a bare DM whose dm_space_presence rows point
//     exclusively at other Spaces (and whose Recents carry no default/untagged
//     counter-evidence) is now HIDDEN from the default Space; DMs without any
//     presence rows keep the legacy catch-all.
//  2. Spaceless group/topic: a group with empty group.space_id (and its topics)
//     is now listed ONLY in the user's default Space, no longer in every Space.
//
// Helpers are cs-prefixed to sit alongside the base #484 repro harness
// (dm_cross_space_repro_test.go) in the same package; this suite shares that
// harness's fake WuKongIM + webhook secret (see csSetup) so both run together.
//
// Build-tagged `integration` (NOT compiled in the default CI `go test` job):
//
//	go test -tags=integration ./modules/message/ -run TestConvSpaceCatchall -v

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	csSpaceDefault = "csSpaceDefault" // joined first → the user's default Space
	csSpaceB       = "csSpaceB"
	csSpaceC       = "csSpaceC"
)

// This suite shares the repro harness's single fake WuKongIM (reproFakeIM) and
// webhook secret (reproWebhookSecret) rather than standing up its own. The
// message-module handlers bind to the FIRST NewTestServer's ctx/config in the
// process (register.GetModules sync.Once), so two independent fake-IM servers /
// secrets would make whichever suite runs second lose its IM URL and signature
// validation. Sharing one server + one secret lets both suites run in a single
// `go test -tags=integration ./modules/message/` invocation. Conversations are
// published via the shared reproIMConvs var.

// csMsg builds an IM message for peer with an optional payload.space_id tag.
func csMsg(peer string, seq uint32, spaceID string) *config.MessageResp {
	payload := map[string]interface{}{"type": 1, "content": fmt.Sprintf("m%d", seq)}
	if spaceID != "" {
		payload["space_id"] = spaceID
	}
	return &config.MessageResp{
		MessageID:   int64(seq),
		MessageSeq:  seq,
		ClientMsgNo: fmt.Sprintf("cs-%s-%d", peer, seq),
		FromUID:     peer,
		ChannelID:   peer,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   1700000000 + int32(seq),
		Payload:     []byte(util.ToJson(payload)),
	}
}

// csDMConv builds a DM conversation whose Recents are the given tagged messages.
func csDMConv(peer string, recentSpaceIDs ...string) *config.SyncUserConversationResp {
	recents := make([]*config.MessageResp, 0, len(recentSpaceIDs))
	for i, sid := range recentSpaceIDs {
		recents = append(recents, csMsg(peer, uint32(i+1), sid))
	}
	return &config.SyncUserConversationResp{
		ChannelID:   peer,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Timestamp:   1700000099,
		LastMsgSeq:  1,
		Version:     100,
		Recents:     recents,
	}
}

// csChannelConv builds a GROUP or CommunityTopic conversation (bare channel id).
func csChannelConv(channelID string, channelType uint8) *config.SyncUserConversationResp {
	return &config.SyncUserConversationResp{
		ChannelID:   channelID,
		ChannelType: channelType,
		Timestamp:   1700000099,
		LastMsgSeq:  1,
		Version:     100,
		Recents: []*config.MessageResp{{
			MessageID: 1, MessageSeq: 1, ClientMsgNo: "cs-" + channelID,
			FromUID: testutil.UID, ChannelID: channelID, ChannelType: channelType,
			Timestamp: 1700000001,
			Payload:   []byte(util.ToJson(map[string]interface{}{"type": 1, "content": "g"})),
		}},
	}
}

func csSeedSpace(t *testing.T, ctx *config.Context, spaceID, createdAt string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO space (space_id, name, creator, status, created_at, updated_at) VALUES (?, ?, ?, 1, ?, ?)",
		spaceID, spaceID, testutil.UID, createdAt, createdAt,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, ?, ?)",
		spaceID, testutil.UID, createdAt, createdAt,
	).Exec()
	require.NoError(t, err)
}

func csSeedGroup(t *testing.T, ctx *config.Context, groupNo, spaceID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, status, space_id) VALUES (?, ?, 1, ?)",
		groupNo, groupNo, spaceID,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO group_member (group_no, uid, role, status, is_deleted, version) VALUES (?, ?, 0, 1, 0, 1)",
		groupNo, testutil.UID,
	).Exec()
	require.NoError(t, err)
}

// csSeedThread inserts an ACTIVE thread row so the conv-sync liveness whitelist
// (QueryActiveShortIDs, status=1) keeps the topic conversation — orthogonal to
// the Space filter under test.
func csSeedThread(t *testing.T, ctx *config.Context, groupNo, shortID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO thread (short_id, group_no, name, creator_uid, status) VALUES (?, ?, ?, ?, 1)",
		shortID, groupNo, "cs-topic", testutil.UID,
	).Exec()
	require.NoError(t, err)
}

// csEnsureAppBotTable stubs an empty app_bot table: resolveBotFilter →
// CheckBotsInSpace always queries it, but this test binary doesn't load the
// app_bot module. Without the table the query errors → skipBotFilter=true →
// both the bot sub-check AND the catch-all tightening are skipped (fail-open),
// masking the behavior under test.
func csEnsureAppBotTable(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"CREATE TABLE IF NOT EXISTS app_bot (" +
			"uid VARCHAR(40) NOT NULL, status TINYINT NOT NULL DEFAULT 0, " +
			"scope VARCHAR(20) NOT NULL DEFAULT 'platform', space_id VARCHAR(40) DEFAULT NULL)",
	).Exec()
	require.NoError(t, err)
}

func csSetup(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	// Reset the shared fake-IM response vars (see reproSetup).
	reproIMConv = nil
	reproIMConvs = nil
	reproIMMsgs = nil

	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("TS_WEBHOOK_SECRET_KEY", reproWebhookSecret)

	imURL := reproFakeIM().URL
	s, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	cfg := ctx.GetConfig()
	cfg.MessageSaveAcrossDevice = false
	cfg.WuKongIM.APIURL = imURL

	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test"))

	csSeedSpace(t, ctx, csSpaceDefault, "2020-01-01 00:00:00")
	csSeedSpace(t, ctx, csSpaceB, "2021-01-01 00:00:00")
	csSeedSpace(t, ctx, csSpaceC, "2022-01-01 00:00:00")
	csEnsureAppBotTable(t, ctx)

	r := ctx.GetRedisConn()
	_ = r.Del("ratelimit:uid:" + testutil.UID)
	_ = r.Del("userMaxVersion:" + testutil.UID)
	for _, sp := range []string{csSpaceDefault, csSpaceB, csSpaceC} {
		_ = r.Del("space:member:" + sp + ":" + testutil.UID)
	}
	return s, ctx
}

// csIngestDM drives the REAL message webhook for an inbound DM (peer → login
// user) tagged with spaceID, which writes the dm_space_presence row.
func csIngestDM(t *testing.T, s *server.Server, peer, spaceID string, seq uint32) {
	t.Helper()
	payload := fmt.Sprintf(`{"type":1,"content":"m%d","space_id":%q}`, seq, spaceID)
	b64 := base64.StdEncoding.EncodeToString([]byte(payload))
	body := fmt.Sprintf(
		`[{"message_id":%d,"message_seq":%d,"from_uid":%q,"channel_id":%q,"channel_type":1,"timestamp":%d,"payload":%q}]`,
		seq, seq, peer, testutil.UID, 1700000000+int(seq), b64,
	)
	mac := hmac.New(sha256.New, []byte(reproWebhookSecret))
	mac.Write([]byte(body))
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/webhook/message/notify", strings.NewReader(body))
	req.Header.Set("X-Signature-256", sig)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// csCallConvSync drives POST /v1/conversation/sync with X-Space-ID and returns
// the channel IDs in the response list.
func csCallConvSync(t *testing.T, s *server.Server, spaceID string) []string {
	t.Helper()
	body := `{"version":0,"msg_count":50,"device_uuid":"dev-cs","recent_filter":false}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/conversation/sync", strings.NewReader(body))
	req.Header.Set("token", testutil.Token)
	req.Header.Set("X-Space-ID", spaceID)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var wrap struct {
		Conversations []struct {
			ChannelID string `json:"channel_id"`
		} `json:"conversations"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &wrap))
	ids := make([]string, 0, len(wrap.Conversations))
	for _, c := range wrap.Conversations {
		ids = append(ids, c.ChannelID)
	}
	return ids
}

func csContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestConvSpaceCatchall_DefaultSpaceHidesElsewhereOnlyDM asserts fix 1: the
// default Space no longer lists a bare DM whose presence rows point exclusively
// at other Spaces — while every legacy/counter-evidenced DM keeps the catch-all.
func TestConvSpaceCatchall_DefaultSpaceHidesElsewhereOnlyDM(t *testing.T) {
	s, _ := csSetup(t)
	const (
		peerElsewhere = "cs_peer_elsewhere" // presence: spaceB only; Recents: spaceB-tagged
		peerLegacy    = "cs_peer_legacy"    // NO presence rows; Recents: spaceB-tagged
		peerHome      = "cs_peer_home"      // presence: spaceB AND default
		peerUntagged  = "cs_peer_untagged"  // presence: spaceB only; Recents: untagged
	)
	csIngestDM(t, s, peerElsewhere, csSpaceB, 1)
	csIngestDM(t, s, peerHome, csSpaceB, 2)
	csIngestDM(t, s, peerHome, csSpaceDefault, 3)
	csIngestDM(t, s, peerUntagged, csSpaceB, 4)

	reproIMConvs = []*config.SyncUserConversationResp{
		csDMConv(peerElsewhere, csSpaceB),
		csDMConv(peerLegacy, csSpaceB),
		csDMConv(peerHome, csSpaceDefault),
		csDMConv(peerUntagged, ""),
	}

	inDefault := csCallConvSync(t, s, csSpaceDefault)
	assert.False(t, csContains(inDefault, peerElsewhere),
		"FIXED: presence 只在他空间的 DM 不再出现在默认 Space 列表")
	assert.True(t, csContains(inDefault, peerLegacy),
		"无 presence 行的存量 DM 保持 catch-all 现状")
	assert.True(t, csContains(inDefault, peerHome),
		"默认 Space 有 presence 的 DM 保留")
	assert.True(t, csContains(inDefault, peerUntagged),
		"Recents 含未打标消息 → 本地反证保留")
	assert.True(t, csContains(inDefault, "botfather"),
		"系统 bot 在默认 Space 恒可见（EnsureSystemBotsPresent）")

	// The hidden DM is still visible in the Space it belongs to (Recents scan).
	inB := csCallConvSync(t, s, csSpaceB)
	assert.True(t, csContains(inB, peerElsewhere),
		"被默认 Space 隐藏的 DM 在其归属 Space（B）仍可见")
}

// TestConvSpaceCatchall_SpacelessGroupAndTopicOnlyDefault asserts fix 2: a group
// with empty group.space_id (and its topic) is listed only in the default Space.
func TestConvSpaceCatchall_SpacelessGroupAndTopicOnlyDefault(t *testing.T) {
	s, ctx := csSetup(t)
	const (
		grpBare = "cs_grp_bare" // group.space_id = ''
		grpB    = "cs_grp_b"    // group.space_id = spaceB
	)
	csSeedGroup(t, ctx, grpBare, "")
	csSeedGroup(t, ctx, grpB, csSpaceB)
	topicBare := grpBare + "____123456789012345"
	topicB := grpB + "____123456789012346"
	csSeedThread(t, ctx, grpBare, "123456789012345")
	csSeedThread(t, ctx, grpB, "123456789012346")

	reproIMConvs = []*config.SyncUserConversationResp{
		csChannelConv(grpBare, common.ChannelTypeGroup.Uint8()),
		csChannelConv(grpB, common.ChannelTypeGroup.Uint8()),
		csChannelConv(topicBare, common.ChannelTypeCommunityTopic.Uint8()),
		csChannelConv(topicB, common.ChannelTypeCommunityTopic.Uint8()),
	}

	inDefault := csCallConvSync(t, s, csSpaceDefault)
	assert.True(t, csContains(inDefault, grpBare), "空 space_id 群归属默认 Space")
	assert.True(t, csContains(inDefault, topicBare), "空 space_id 父群的子区归属默认 Space")
	assert.False(t, csContains(inDefault, grpB), "有归属的群不进默认 Space")
	assert.False(t, csContains(inDefault, topicB), "有归属父群的子区不进默认 Space")

	inB := csCallConvSync(t, s, csSpaceB)
	assert.True(t, csContains(inB, grpB), "spaceB 群在 spaceB 可见")
	assert.True(t, csContains(inB, topicB), "spaceB 父群的子区在 spaceB 可见")
	assert.False(t, csContains(inB, grpBare),
		"FIXED: 空 space_id 群不再出现在非默认 Space")
	assert.False(t, csContains(inB, topicBare),
		"FIXED: 空 space_id 父群的子区不再出现在非默认 Space")

	inC := csCallConvSync(t, s, csSpaceC)
	assert.False(t, csContains(inC, grpBare), "空 space_id 群在 spaceC 同样隐藏")
	assert.False(t, csContains(inC, grpB), "spaceB 群在 spaceC 隐藏")
}
