package user

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/model"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------- WuKongIM 录制桩 ----------
//
// 断言「哪些对端被断、哪些被放过」比断言 WuKongIM 内部白名单状态更直接，也让这组
// 用例不依赖 IM 容器（仍需要 MySQL）。桩把会话列表喂给被测代码，并录下所有
// whitelist_remove / CMD 调用。

type imStub struct {
	server *httptest.Server

	mu              sync.Mutex
	conversations   []map[string]interface{}
	whitelistRemove []whitelistRemoveCall
	whitelistAdd    []whitelistRemoveCall
	whitelistSet    []whitelistRemoveCall
	cmds            []map[string]interface{}
	// failChannels 里的 channel_id 上，whitelist_remove 返回 500，用来断言
	// 摘白名单失败会被上抛而不是吞掉。
	failChannels map[string]bool
	// failSetChannels 同理，但作用在 whitelist_set 上。与 failChannels 分开：
	// 一个频道可能同时收到两种调用，共用一张表就没法只让其中一种失败。
	failSetChannels map[string]bool
}

type whitelistRemoveCall struct {
	ChannelID   string   `json:"channel_id"`
	ChannelType uint8    `json:"channel_type"`
	UIDs        []string `json:"uids"`
}

func newIMStub(t *testing.T, ctx *config.Context, conversationPeers []string) *imStub {
	t.Helper()
	stub := &imStub{}
	for _, peer := range conversationPeers {
		stub.conversations = append(stub.conversations, map[string]interface{}{
			"channel_id":   peer,
			"channel_type": common.ChannelTypePerson.Uint8(),
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/conversation/sync", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		_ = json.NewEncoder(w).Encode(stub.conversations)
	})
	mux.HandleFunc("/channel/whitelist_add", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call whitelistRemoveCall
		_ = json.Unmarshal(body, &call)
		stub.mu.Lock()
		stub.whitelistAdd = append(stub.whitelistAdd, call)
		stub.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/channel/whitelist_set", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call whitelistRemoveCall
		_ = json.Unmarshal(body, &call)
		stub.mu.Lock()
		stub.whitelistSet = append(stub.whitelistSet, call)
		shouldFail := stub.failSetChannels[call.ChannelID]
		stub.mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/channel/whitelist_remove", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call whitelistRemoveCall
		_ = json.Unmarshal(body, &call)
		stub.mu.Lock()
		stub.whitelistRemove = append(stub.whitelistRemove, call)
		shouldFail := stub.failChannels[call.ChannelID]
		stub.mu.Unlock()
		if shouldFail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/message/send", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		stub.mu.Lock()
		stub.cmds = append(stub.cmds, payload)
		stub.mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	})
	// 其余端点一律放行，避免无关调用把用例带偏。
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	stub.server = httptest.NewServer(mux)
	cfg := ctx.GetConfig()
	previous := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = stub.server.URL
	t.Cleanup(func() {
		cfg.WuKongIM.APIURL = previous
		stub.server.Close()
	})
	return stub
}

// failWhitelistRemove 让指定频道上的 whitelist_remove 返回 500。
func (s *imStub) failWhitelistRemove(channelIDs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failChannels == nil {
		s.failChannels = make(map[string]bool, len(channelIDs))
	}
	for _, id := range channelIDs {
		s.failChannels[id] = true
	}
}

// addedChannels 返回所有被补白名单的 (channel_id, uid) 组合。
func (s *imStub) addedChannels() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string, len(s.whitelistAdd))
	for _, call := range s.whitelistAdd {
		out[call.ChannelID] = append(out[call.ChannelID], call.UIDs...)
	}
	return out
}

// failWhitelistSet 让指定频道上的 whitelist_set 返回 500。
func (s *imStub) failWhitelistSet(channelIDs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSetChannels == nil {
		s.failSetChannels = make(map[string]bool, len(channelIDs))
	}
	for _, id := range channelIDs {
		s.failSetChannels[id] = true
	}
}

// setWhitelistOf 返回该频道最后一次 whitelist_set 写入的 uid 集合，
// 以及这个频道到底有没有被 set 过。覆写是「最后一次说了算」，所以只看最后一次。
func (s *imStub) setWhitelistOf(channelID string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		uids  []string
		found bool
	)
	for _, call := range s.whitelistSet {
		if call.ChannelID == channelID {
			uids, found = call.UIDs, true
		}
	}
	return uids, found
}

// setCallCount 返回 whitelist_set 的总次数。
func (s *imStub) setCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.whitelistSet)
}

// clearWhitelistRemoveFailures 恢复所有被打故障的频道。
func (s *imStub) clearWhitelistRemoveFailures() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failChannels = nil
}

// removedChannels 返回所有被摘白名单的 (channel_id, uid) 组合。
func (s *imStub) removedChannels() map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]string, len(s.whitelistRemove))
	for _, call := range s.whitelistRemove {
		out[call.ChannelID] = append(out[call.ChannelID], call.UIDs...)
	}
	return out
}

func (s *imStub) cmdCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cmds)
}

// ---------- fixture ----------

// dmTestCtx 本文件共用一个测试服务器。
//
// testutil.NewTestServer 每次约 0.42s（本机实测），而 modules/user 整个包已经建了
// 186 个，在 CI 的 5 分钟 per-package 预算里长期贴着上限跑（E2E Test (1/4) 就是
// 这么被 kill 的）。本文件十几个用例没有任何理由各建一个：隔离靠每个用例开头的
// CleanAllTables，而不是靠换一个 server。
var (
	dmTestCtxOnce sync.Once
	dmTestCtx     *config.Context
)

func dmTestSetup(t *testing.T) (*config.Context, *Friend) {
	t.Helper()
	dmTestCtxOnce.Do(func() {
		_, dmTestCtx = testutil.NewTestServer()
	})
	require.NoError(t, testutil.CleanAllTables(dmTestCtx))
	return dmTestCtx, NewFriend(dmTestCtx)
}

// seedSpaceMember 直接写 space / space_member，不经 modules/space 的 handler。
func seedSpaceMember(t *testing.T, ctx *config.Context, spaceID string, uids ...string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"INSERT IGNORE INTO space (space_id, name, creator, status, max_users, created_at, updated_at) VALUES (?, ?, ?, 1, 1000, NOW(), NOW())",
		spaceID, spaceID, uids[0])
	require.NoError(t, err)
	for _, uid := range uids {
		_, err := ctx.DB().Exec(
			"INSERT INTO space_member (space_id, uid, role, status, created_at, updated_at) VALUES (?, ?, 0, 1, NOW(), NOW())",
			spaceID, uid)
		require.NoError(t, err)
	}
}

// removeSpaceMember 模拟成员行已被置 0（清理步骤总是在移除提交之后才跑）。
func removeSpaceMember(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	_, err := ctx.DB().Exec(
		"UPDATE space_member SET status=0 WHERE space_id=? AND uid=?", spaceID, uid)
	require.NoError(t, err)
}

// seedDMPresence 标记这一对在该 Space 下聊过。
func seedDMPresence(t *testing.T, ctx *config.Context, a, b, spaceID string) {
	t.Helper()
	require.NoError(t, spacepkg.UpsertDMSpacePresence(
		ctx.DB(), common.GetFakeChannelIDWith(a, b), spaceID, 1))
}

func seedMutualFriend(t *testing.T, ctx *config.Context, a, b string) {
	t.Helper()
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		_, err := ctx.DB().Exec(
			"INSERT INTO friend (uid, to_uid, flag, version, is_deleted, is_alone, created_at, updated_at) VALUES (?, ?, 0, 1, 0, 0, NOW(), NOW())",
			pair[0], pair[1])
		require.NoError(t, err)
	}
}

// ---------- 用例 ----------

// TestDMCutoffDropsPeerWithNoRemainingRelationship 基线：既不同处任何 Space、也不是好友
// → 双向摘白名单，两端各收一条 channelUpdate。
func TestDMCutoffDropsPeerWithNoRemainingRelationship(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, peer = "dm-basic", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	seedDMPresence(t, ctx, removed, peer, spaceID)
	removeSpaceMember(t, ctx, spaceID, removed)

	stub := newIMStub(t, ctx, []string{peer})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	removedCalls := stub.removedChannels()
	// 挡住 A→B 必须把 A 从 B 的频道白名单摘掉，反之亦然；只摘一边只断一个方向。
	assert.Equal(t, []string{removed}, removedCalls[peer], "对端频道要摘掉被移除者")
	assert.Equal(t, []string{peer}, removedCalls[removed], "被移除者频道要摘掉对端")
	assert.Equal(t, 2, stub.cmdCount(), "两端各推一条 channelUpdate")
}

// TestDMCutoffKeepsPeerSharingAnotherSpace 首要回归点：
// 两人还同处另一个活跃 Space 时，私聊必须原封不动。
func TestDMCutoffKeepsPeerSharingAnotherSpace(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceA, spaceB, removed, peer = "dm-multi-a", "dm-multi-b", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceA, removed, peer)
	seedSpaceMember(t, ctx, spaceB, removed, peer) // 仍共处 B
	seedDMPresence(t, ctx, removed, peer, spaceA)
	removeSpaceMember(t, ctx, spaceA, removed)

	stub := newIMStub(t, ctx, []string{peer})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceA, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	assert.Empty(t, stub.removedChannels(), "还有别的共同 Space，一个白名单都不该摘")
	assert.Zero(t, stub.cmdCount(), "私聊没变化就不该推 channelUpdate")
}

// TestDMCutoffKeepsFriendPeer 好友关系独立于 Space 授权私聊，不能被 Space 移除带走。
func TestDMCutoffKeepsFriendPeer(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, peer = "dm-friend", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	seedDMPresence(t, ctx, removed, peer, spaceID)
	seedMutualFriend(t, ctx, removed, peer)
	removeSpaceMember(t, ctx, spaceID, removed)

	stub := newIMStub(t, ctx, []string{peer})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonLeft,
	}))

	assert.Empty(t, stub.removedChannels(), "双向好友的私聊不受 Space 移除影响")
}

// TestDMCutoffIgnoresOneSidedFriend 单向好友（对方已把你删掉，is_alone=1）不进白名单，
// 因此不能拿它当"仍有授权"的理由——否则会把已经无权的私聊留着。
func TestDMCutoffIgnoresOneSidedFriend(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, peer = "dm-alone", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	seedDMPresence(t, ctx, removed, peer, spaceID)
	_, err := ctx.DB().Exec(
		"INSERT INTO friend (uid, to_uid, flag, version, is_deleted, is_alone, created_at, updated_at) VALUES (?, ?, 0, 1, 0, 1, NOW(), NOW())",
		removed, peer)
	require.NoError(t, err)
	removeSpaceMember(t, ctx, spaceID, removed)

	stub := newIMStub(t, ctx, []string{peer})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	assert.Len(t, stub.removedChannels(), 2, "单向好友不构成授权，私聊仍要断")
}

// TestDMCutoffDoesNotRequireDMPresenceRow 回归（code review 第二轮）：
// dm_space_presence 不再是硬门槛。
//
// 那张表是 message webhook 上尽力而为写的增量索引：不回填、只覆盖带 space_id 的
// 非加密 Person 消息、写失败只记日志。把它当唯一门槛，等于让任何在该表上线前聊过、
// 或用加密私聊、或那次 upsert 恰好失败的一对，静默跳过整个隔离清理。
// 现在的范围由「他自己的会话列表 ∩ 在本 Space 有过成员行」圈定，
// 切不切仍由逐对端的授权判定决定。
func TestDMCutoffDoesNotRequireDMPresenceRow(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, peer = "dm-nopresence", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	// 刻意不写 dm_space_presence —— 会话列表本身已经是"有过私聊"的证据
	removeSpaceMember(t, ctx, spaceID, removed)

	stub := newIMStub(t, ctx, []string{peer})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	removedCalls := stub.removedChannels()
	assert.Equal(t, []string{removed}, removedCalls[peer])
	assert.Equal(t, []string{peer}, removedCalls[removed])
}

// TestDMCutoffSkipsNonSpaceMember 会话列表里的外部对端（不是本 Space 成员）不该被牵连。
func TestDMCutoffSkipsNonSpaceMember(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, outsider = "dm-outsider", "u-removed", "u-outsider"

	seedSpaceMember(t, ctx, spaceID, removed)
	seedDMPresence(t, ctx, removed, outsider, spaceID)
	removeSpaceMember(t, ctx, spaceID, removed)

	stub := newIMStub(t, ctx, []string{outsider})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	assert.Empty(t, stub.removedChannels(), "非本 Space 成员不在清理范围内")
}

// TestDMCutoffHandlesBotSpacePrefixedChannel bot 私聊用 s{spaceID}_{uid} 频道，
// 只摘裸 uid 频道会把 bot 私聊漏在开着的状态。
func TestDMCutoffHandlesBotSpacePrefixedChannel(t *testing.T) {
	ctx, f := dmTestSetup(t)
	// Space ID 必须是真实形状：生产用 util.GenerUUID()（去掉横线的 UUIDv4，32 位 hex），
	// 而 spacepkg.ParseChannelID 只在「已注册的 spaceId」或正则 ^s[0-9a-f]{32}_ 命中时
	// 才剥前缀。用 "dm-bot" 这类假 ID 会让前缀永远剥不掉，测试就测不到真实路径。
	const spaceID, removed, bot = "0123456789abcdef0123456789abcdef", "u-removed", "bot-1"

	seedSpaceMember(t, ctx, spaceID, removed, bot)
	seedDMPresence(t, ctx, removed, bot, spaceID)
	_, err := ctx.DB().Exec(
		"INSERT INTO robot (robot_id, status, created_at, updated_at) VALUES (?, 1, NOW(), NOW())", bot)
	require.NoError(t, err)
	removeSpaceMember(t, ctx, spaceID, removed)

	// 会话 id 用 Space 前缀形式，验证对端解析也能剥掉前缀
	stub := newIMStub(t, ctx, []string{spacepkg.BuildChannelID(spaceID, bot)})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	removedCalls := stub.removedChannels()
	assert.Contains(t, removedCalls, bot, "裸频道要摘")
	assert.Contains(t, removedCalls, removed, "被移除者裸频道要摘")
	assert.Contains(t, removedCalls, spacepkg.BuildChannelID(spaceID, bot), "bot 的 Space 前缀频道要摘")
	assert.Contains(t, removedCalls, spacepkg.BuildChannelID(spaceID, removed), "用户侧 Space 前缀频道要摘")
}

// TestDMPeerCandidatesFiltering 对端抽取的边界：群频道、自己、重复项、Space 前缀。
func TestDMPeerCandidatesFiltering(t *testing.T) {
	self := "me"
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "peer-a", ChannelType: common.ChannelTypePerson.Uint8()},
		{ChannelID: "group-1", ChannelType: common.ChannelTypeGroup.Uint8()}, // 群不算
		{ChannelID: self, ChannelType: common.ChannelTypePerson.Uint8()},     // 自己不算
		{ChannelID: "peer-a", ChannelType: common.ChannelTypePerson.Uint8()}, // 去重
		{ChannelID: "s" + "0123456789abcdef0123456789abcdef" + "_peer-b",
			ChannelType: common.ChannelTypePerson.Uint8()}, // Space 前缀要剥掉
		nil,
	}
	assert.Equal(t, []string{"peer-a", "peer-b"},
		dmPeerCandidates(convs, self, "0123456789abcdef0123456789abcdef"))
}

// TestDMPeerCandidatesStripsNonHexSpacePrefix Space id 不是 32 位 hex 时，前缀
// 同样必须剥掉——**而且不能依赖全局 knownSpaceIDs 缓存**。
//
// ParseChannelID 先查那份缓存，不中才回落到 `^s[0-9a-f]{32}_` 正则；而
// loadKnownSpaceIDs 只装 status=1 的 Space，解散/封禁路径又恰好在跑清理之前刷新
// 缓存。于是对 minglue_default 这类老 id，解散之后 "sminglue_default_botfather"
// 既不中缓存也不中正则，整条 channel_id 被当成对端返回，永远匹配不上
// space_member.uid，那条 bot 私聊被静默跳过而工单仍标 done。
//
// 这里刻意**不**预热缓存：用例要钉住的正是「缓存里没有也照样剥得掉」。
// 而且必须**主动清空**——knownSpaceIDs 是包级全局，整包跑的时候 dmTestSetup 起的
// server 会在 Route() 里 loadKnownSpaceIDs()。只靠「本包没 seed 这个 Space」是
// 靠运气：别处 seed 一个同名 Space，这条用例就会在快路径被删掉之后依然通过。
func TestDMPeerCandidatesStripsNonHexSpacePrefix(t *testing.T) {
	const spaceID, self = "minglue_default", "me"
	spacepkg.RegisterSpaceIDs(nil)
	t.Cleanup(func() { spacepkg.RegisterSpaceIDs(nil) })
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "s" + spaceID + "_botfather", ChannelType: common.ChannelTypePerson.Uint8()},
		{ChannelID: "peer-plain", ChannelType: common.ChannelTypePerson.Uint8()},
	}
	assert.Equal(t, []string{"botfather", "peer-plain"},
		dmPeerCandidates(convs, self, spaceID),
		"非 32-hex 的 Space 前缀必须靠工单自带的 spaceID 剥掉，不能指望全局缓存")
}

// TestDMPeerCandidatesFallsBackWithoutSpaceID spaceID 缺失时仍走原来的
// ParseChannelID 回落，不能因为新增的快路径把老行为弄丢。
func TestDMPeerCandidatesFallsBackWithoutSpaceID(t *testing.T) {
	convs := []*config.SyncUserConversationResp{
		{ChannelID: "s0123456789abcdef0123456789abcdef_peer-b",
			ChannelType: common.ChannelTypePerson.Uint8()},
	}
	assert.Equal(t, []string{"peer-b"}, dmPeerCandidates(convs, "me", ""))
}

// TestAnnotateDMSendability 下发给客户端的「能否发消息」标记。
//
// 判定必须与 Person 频道白名单同源（friends(peer, is_alone=0) ∪ coMembers(peer)），
// 口径不一致就会出现「前端显示可发但被 WuKongIM 拒收」或反过来无谓置灰。
func TestAnnotateDMSendability(t *testing.T) {
	ctx, _ := dmTestSetup(t)

	t.Run("同处活跃 Space 时不下发标记", func(t *testing.T) {
		seedSpaceMember(t, ctx, "ann-shared", "viewer", "peer")
		resp := &model.ChannelResp{}
		annotateDMSendability(ctx, newFriendDB(ctx), resp, "peer", "viewer")
		assert.NotContains(t, resp.Extra, dmForbiddenExtraKey, "可发送时整个 key 都不该出现")
	})

	t.Run("无共同 Space 且非好友时标记不可发送", func(t *testing.T) {
		require.NoError(t, testutil.CleanAllTables(ctx))
		resp := &model.ChannelResp{}
		annotateDMSendability(ctx, newFriendDB(ctx), resp, "stranger", "viewer")
		require.Contains(t, resp.Extra, dmForbiddenExtraKey)
		assert.Equal(t, 1, resp.Extra[dmForbiddenExtraKey])
		assert.Equal(t, dmForbiddenReasonNoSharedSpace, resp.Extra[dmForbiddenReasonExtraKey])
	})

	t.Run("好友即使无共同 Space 也可发送", func(t *testing.T) {
		require.NoError(t, testutil.CleanAllTables(ctx))
		seedMutualFriend(t, ctx, "viewer", "buddy")
		resp := &model.ChannelResp{}
		annotateDMSendability(ctx, newFriendDB(ctx), resp, "buddy", "viewer")
		assert.NotContains(t, resp.Extra, dmForbiddenExtraKey)
	})

	t.Run("自己的频道永远可写", func(t *testing.T) {
		require.NoError(t, testutil.CleanAllTables(ctx))
		resp := &model.ChannelResp{}
		annotateDMSendability(ctx, newFriendDB(ctx), resp, "viewer", "viewer")
		assert.NotContains(t, resp.Extra, dmForbiddenExtraKey)
	})

	t.Run("不覆盖既有 extra 字段", func(t *testing.T) {
		require.NoError(t, testutil.CleanAllTables(ctx))
		resp := &model.ChannelResp{Extra: map[string]interface{}{"sex": 1}}
		annotateDMSendability(ctx, newFriendDB(ctx), resp, "stranger", "viewer")
		assert.Equal(t, 1, resp.Extra["sex"], "既有 extra 必须保留")
		assert.Contains(t, resp.Extra, dmForbiddenExtraKey)
	})

	t.Run("已解散 Space 不算共处", func(t *testing.T) {
		require.NoError(t, testutil.CleanAllTables(ctx))
		seedSpaceMember(t, ctx, "ann-dead", "viewer", "peer")
		_, err := ctx.DB().Exec("UPDATE space SET status=0 WHERE space_id=?", "ann-dead")
		require.NoError(t, err)
		resp := &model.ChannelResp{}
		annotateDMSendability(ctx, newFriendDB(ctx), resp, "peer", "viewer")
		assert.Contains(t, resp.Extra, dmForbiddenExtraKey,
			"已解散 Space 不构成授权（GetCommonSpaceID 不校验 status，故不能用它判定）")
	})
}

// TestDMCutoffAfterSpaceDisbanded 回归（code review P0）：解散会先把 space.status 置 0，
// 若对端筛选按「活跃成员」口径走，整个私聊切断会静默空转——一条白名单都不摘。
func TestDMCutoffAfterSpaceDisbanded(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, peer = "dm-disbanded", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	seedDMPresence(t, ctx, removed, peer, spaceID)
	// 复刻 forceDisbandSpace 的终态：空间置 0 + 全员置 0
	_, err := ctx.DB().Exec("UPDATE space SET status=0 WHERE space_id=?", spaceID)
	require.NoError(t, err)
	_, err = ctx.DB().Exec("UPDATE space_member SET status=0 WHERE space_id=?", spaceID)
	require.NoError(t, err)

	stub := newIMStub(t, ctx, []string{peer})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "su", Reason: spacemod.MemberRemoveReasonSpaceDisbanded,
	}))

	removedCalls := stub.removedChannels()
	assert.Equal(t, []string{removed}, removedCalls[peer])
	assert.Equal(t, []string{peer}, removedCalls[removed])
}

// TestDMCutoffWhenBothPeersRemovedTogether 回归（code review P0）：A、B 同批被移除时
// 各自工单里对方的 sm.status 都已是 0，按活跃口径筛会互相过滤掉，
// 两人之间的私聊永远切不断。
func TestDMCutoffWhenBothPeersRemovedTogether(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, a, b = "dm-both-removed", "u-a", "u-b"

	seedSpaceMember(t, ctx, spaceID, a, b)
	seedDMPresence(t, ctx, a, b, spaceID)
	removeSpaceMember(t, ctx, spaceID, a)
	removeSpaceMember(t, ctx, spaceID, b)

	stub := newIMStub(t, ctx, []string{b})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: a, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	removedCalls := stub.removedChannels()
	assert.Equal(t, []string{a}, removedCalls[b])
	assert.Equal(t, []string{b}, removedCalls[a])
}

// TestDMCutoffSingleDirectionFriend 单向好友：A 留着 A→B 的好友行，B 早把 A 删了。
//
// 白名单是按频道推导的（X 的频道白名单 = 谁能发给 X）：A 的频道仍因这条好友行授权 B，
// 但 B 的频道并不授权 A。若任一方向有好友行就整个跳过，B 频道上那条早已失效的
// 白名单就没人摘 —— A 还能继续发给 B。两个方向必须独立判定。
func TestDMCutoffSingleDirectionFriend(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, a, b = "dm-one-direction", "u-a", "u-b"

	seedSpaceMember(t, ctx, spaceID, a, b)
	seedDMPresence(t, ctx, a, b, spaceID)
	// 只有 a -> b 这一条有效好友行
	_, err := ctx.DB().Exec(
		"INSERT INTO friend (uid, to_uid, flag, version, is_deleted, is_alone, created_at, updated_at) VALUES (?, ?, 0, 1, 0, 0, NOW(), NOW())",
		a, b)
	require.NoError(t, err)
	removeSpaceMember(t, ctx, spaceID, a)

	stub := newIMStub(t, ctx, []string{b})
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: a, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	removedCalls := stub.removedChannels()
	// a 的频道白名单仍因 a->b 的好友行包含 b：这个方向不该动
	assert.NotContains(t, removedCalls, a, "a 仍授权 b 发给自己，该方向必须保留")
	// b 的频道白名单已不含 a：这条陈旧条目必须摘掉
	assert.Equal(t, []string{a}, removedCalls[b], "b 已不授权 a，该方向必须切断")
}

// TestDMCutoffPropagatesSpacePrefixedWhitelistFailure bot 私聊的 Space 前缀频道
// 摘白名单失败必须上抛，让工单重试。
//
// 这条曾经是 best-effort，理由是「频道未必存在」。实测（WuKongIM v2.2.4-20260313）
// 对不存在的频道 whitelist_remove 返回 HTTP 200，所以走到错误分支的一定是真故障：
// 吞掉的话工单会被标成 done，bot 私聊的白名单就永远留在那儿——被移出 Space 的人
// 还能继续给 bot 发消息，而这正是本任务要堵的洞。
func TestDMCutoffPropagatesSpacePrefixedWhitelistFailure(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, bot = "0123456789abcdef0123456789abcdef", "u-removed", "bot-1"

	seedSpaceMember(t, ctx, spaceID, removed, bot)
	_, err := ctx.DB().Exec(
		"INSERT INTO robot (robot_id, status, created_at, updated_at) VALUES (?, 1, NOW(), NOW())", bot)
	require.NoError(t, err)
	removeSpaceMember(t, ctx, spaceID, removed)

	stub := newIMStub(t, ctx, []string{bot})
	// 只让前缀频道失败，裸频道照常成功：断言失败确实来自这一步，
	// 而不是把两条路径混在一起。
	stub.failWhitelistRemove(spacepkg.BuildChannelID(spaceID, bot))

	err = f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	})
	require.Error(t, err, "前缀频道摘白名单失败必须上抛，否则工单会被误标 done")
	assert.Contains(t, err.Error(), spacepkg.BuildChannelID(spaceID, bot),
		"错误信息要带上失败的频道，last_error 才有诊断价值")

	// 重跑必须能收敛：范围（MembersEverInSpace）不看成员状态，IM 恢复之后
	// 同一批对端会被重新枚举到，重试才不是空转。
	stub.clearWhitelistRemoveFailures()
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}), "IM 恢复后重跑应当成功——重试才有意义")
	assert.Contains(t, stub.removedChannels(), spacepkg.BuildChannelID(spaceID, bot))
}

// ---------- 加入侧：白名单回补 ----------

// TestDMRestoreAfterRejoin 摘和补必须成对：踢出后重新加入，两侧白名单都要回来。
//
// 这条是本轮的核心回归。之前只实现了摘那一半，于是「踢出 → 重新加入」在开启
// Person 白名单校验的部署里会让两个人永久发不了私聊，而服务端推导侧
// （SharesActiveSpace）已经恢复成 true、annotateDMSendability 还告诉客户端「可以发」。
func TestDMRestoreAfterRejoin(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, peer = "sp-restore", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	stub := newIMStub(t, ctx, []string{peer})

	// 1. 移除 + 切断
	removeSpaceMember(t, ctx, spaceID, removed)
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))
	cut := stub.removedChannels()
	require.Contains(t, cut, peer, "前提：对端频道上的白名单被摘掉了")
	require.Contains(t, cut, removed, "前提：被移除者频道上的白名单被摘掉了")

	// 2. 重新加入
	_, err := ctx.DB().Exec(
		"UPDATE space_member SET status=1 WHERE space_id=? AND uid=?", spaceID, removed)
	require.NoError(t, err)

	require.NoError(t, f.restoreSpaceMemberDMs(ctx, spacemod.MemberRejoin{
		SpaceID: spaceID, UID: removed,
	}))

	added := stub.addedChannels()
	assert.Contains(t, added[peer], removed, "对端频道上必须把被移除者加回来")
	assert.Contains(t, added[removed], peer, "被移除者频道上必须把对端加回来")
}

// TestDMRestoreSkipsUnauthorizedPeer 回补不能凭空授权：对端自己已经不在这个
// Space 了（也不是好友），加入者回来也不该恢复这一对。
//
// 没有这条约束，回补就成了一个「谁加入谁就能给所有聊过的人发消息」的提权口子。
func TestDMRestoreSkipsUnauthorizedPeer(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, rejoiner, goneP = "sp-restore-skip", "u-rejoin", "u-gone"

	seedSpaceMember(t, ctx, spaceID, rejoiner, goneP)
	// 对端也被移除了，且两人不是好友 —— 现在谁都不该被授权
	removeSpaceMember(t, ctx, spaceID, goneP)

	stub := newIMStub(t, ctx, []string{goneP})
	require.NoError(t, f.restoreSpaceMemberDMs(ctx, spacemod.MemberRejoin{
		SpaceID: spaceID, UID: rejoiner,
	}))

	assert.Empty(t, stub.addedChannels(), "对端已不在本 Space 且非好友，一条白名单都不该补")
}

// TestDMRestoreHandlesBotSpacePrefixedChannel bot 私聊的 Space 前缀频道也要回补，
// 与 cutOffDM 对称——只补裸频道会把 bot 私聊永久留在断开状态。
func TestDMRestoreHandlesBotSpacePrefixedChannel(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, uid, bot = "0123456789abcdef0123456789abcdef", "u-rejoin", "bot-1"

	seedSpaceMember(t, ctx, spaceID, uid, bot)
	_, err := ctx.DB().Exec(
		"INSERT INTO robot (robot_id, status, created_at, updated_at) VALUES (?, 1, NOW(), NOW())", bot)
	require.NoError(t, err)

	stub := newIMStub(t, ctx, []string{spacepkg.BuildChannelID(spaceID, bot)})
	require.NoError(t, f.restoreSpaceMemberDMs(ctx, spacemod.MemberRejoin{
		SpaceID: spaceID, UID: uid,
	}))

	added := stub.addedChannels()
	assert.Contains(t, added, bot, "bot 裸频道要补")
	assert.Contains(t, added, uid, "用户裸频道要补")
	assert.Contains(t, added, spacepkg.BuildChannelID(spaceID, bot), "bot 的 Space 前缀频道要补")
	assert.Contains(t, added, spacepkg.BuildChannelID(spaceID, uid), "用户侧 Space 前缀频道要补")
}

// TestDMRestoreKeepsFriendPeerWhenNotInSpace 好友关系单独就足以授权：
// 对端不在本 Space，但双方是双向好友，回补必须照做。
func TestDMRestoreKeepsFriendPeerWhenNotInSpace(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, rejoiner, friend = "sp-restore-friend", "u-rejoin", "u-friend"

	seedSpaceMember(t, ctx, spaceID, rejoiner, friend)
	removeSpaceMember(t, ctx, spaceID, friend) // 对端不在本 Space
	seedMutualFriend(t, ctx, rejoiner, friend) // 但是好友

	stub := newIMStub(t, ctx, []string{friend})
	require.NoError(t, f.restoreSpaceMemberDMs(ctx, spacemod.MemberRejoin{
		SpaceID: spaceID, UID: rejoiner,
	}))

	added := stub.addedChannels()
	assert.Contains(t, added[friend], rejoiner, "好友关系授权的方向必须补回来")
	assert.Contains(t, added[rejoiner], friend, "好友关系授权的方向必须补回来")
}

// TestDMRestoreSkipsWhenMemberAlreadyRemovedAgain 陈旧的回补绝不能把授权复活。
//
// 回补是在加入路径上异步发出的，读授权和写 IM 之间隔着整个前置查询。这中间他
// 若被重新移除，移除工单的 dm_cutoff 会正确摘掉白名单，而这条陈旧的回补随后
// 把它加回去——工单已标 done，再没有任何东西会来摘第二次，留下一对永久越权的
// 私聊。用例直接构造那个终局：判定完成之后成员行已经置 0，此时不许再写 IM。
func TestDMRestoreSkipsWhenMemberAlreadyRemovedAgain(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, rejoiner, peer = "sp-stale-restore", "u-stale", "u-peer"

	seedSpaceMember(t, ctx, spaceID, rejoiner, peer)
	stub := newIMStub(t, ctx, []string{peer})

	// 模拟「判定时是活跃成员，写之前已被再次移除」：restoreDM 自己会重查。
	removeSpaceMember(t, ctx, spaceID, rejoiner)

	granted, err := f.restoreDM(ctx, spaceID, rejoiner, peer, false)
	require.NoError(t, err)
	assert.False(t, granted, "已失效的加入不该被判成「补过了」")

	assert.Empty(t, stub.addedChannels(),
		"成员已不再活跃时，陈旧的回补一条白名单都不许写——否则会永久复活越权私聊")
}

// TestDMRestoreStillGrantsWhileMemberActive 上面那条守卫不能把正常回补也挡掉。
func TestDMRestoreStillGrantsWhileMemberActive(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, uid, peer = "sp-active-restore", "u-active", "u-peer"

	seedSpaceMember(t, ctx, spaceID, uid, peer)
	stub := newIMStub(t, ctx, []string{peer})

	granted, err := f.restoreDM(ctx, spaceID, uid, peer, false)
	require.NoError(t, err)
	assert.True(t, granted)

	added := stub.addedChannels()
	assert.Contains(t, added[peer], uid)
	assert.Contains(t, added[uid], peer)
}

// TestDMRestoreSkipsWhenPeerLostAuthorization 守卫必须覆盖**对端**方向。
//
// 上一版把 shared / grantInbound / grantOutbound 全在调用方循环里算好再传给
// restoreDM，于是只有加入者那一侧被重查。对端被移除、它自己的 dm_cutoff 跑完标
// done、加入者仍是活跃成员——陈旧的 add 照写不误，两个方向的白名单被永久复活。
// 判定移进 restoreDM 之后，这里读到的就是对端已经不在了。
func TestDMRestoreSkipsWhenPeerLostAuthorization(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, joiner, peer = "sp-peer-lost", "u-joiner", "u-peer"

	seedSpaceMember(t, ctx, spaceID, joiner, peer)
	stub := newIMStub(t, ctx, []string{peer})

	// 加入者仍活跃，**对端**已被移除，且两人不是好友 —— 谁都不再被授权。
	removeSpaceMember(t, ctx, spaceID, peer)

	granted, err := f.restoreDM(ctx, spaceID, joiner, peer, false)
	require.NoError(t, err)
	assert.False(t, granted, "对端已失去授权时不该判成补过了")
	assert.Empty(t, stub.addedChannels(),
		"对端已不在本 Space 且非好友，一条白名单都不许写——否则凭空造出越权私聊")
}

// ---------- C1：会话被本地删掉造成的逃逸 ----------

// TestDMCutoffClosesDeletedConversationEscape 是 C1 的回归用例。
//
// 逐对端枚举的范围来自 IMSyncUserConversation —— 被移除者**自己的**会话列表。
// 他本地删过这个会话（/conversation/sync 在 DeletedAtMsgSeq 之后无新消息时不返回），
// 或者会话数超出 Conversation.UserMaxCount 的截断，这个对端就枚举不到，
// 两个方向的白名单都没人摘：他离开 Space 之后对端仍能发给他。
//
// 覆写他自己的频道按构造关掉这一半，不需要先枚举出对端。
// 删掉 cleanupSpaceMemberDMs 里的 reconcileRemovedMemberChannel 调用，本用例变红。
func TestDMCutoffClosesDeletedConversationEscape(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, peer = "dm-escape", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	seedDMPresence(t, ctx, removed, peer, spaceID)
	removeSpaceMember(t, ctx, spaceID, removed)

	// 关键：会话列表是**空的**，模拟他把这个会话本地删了。
	stub := newIMStub(t, ctx, nil)
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	assert.Empty(t, stub.removedChannels(),
		"枚举不到对端，逐对端那条路本来就摘不掉任何东西——这正是洞的成因，不是用例写错了")

	uids, ok := stub.setWhitelistOf(removed)
	require.True(t, ok, "被移除者自己的 Person 频道必须被覆写一次")
	assert.NotContains(t, uids, peer, "覆写之后，前同事不再能发消息给他")
}

// TestDMCutoffOverwriteKeepsStillAuthorizedSenders 覆写的另一面：不能误伤。
//
// 覆写是「整个频道换成推导值」，推导集合少算一个人就等于误摘他的授权。
// 这里同时放三种人进去：只因本 Space 才有授权的（该摘）、仍共处另一个 Space 的、
// 以及双向好友（都该留）。
func TestDMCutoffOverwriteKeepsStillAuthorizedSenders(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const (
		spaceA  = "dm-ovr-a"
		spaceB  = "dm-ovr-b"
		removed = "u-removed"
		peer    = "u-peer"  // 只在 spaceA —— 该摘
		other   = "u-other" // 也在 spaceB —— 该留
		buddy   = "u-buddy" // 双向好友 —— 该留
	)

	seedSpaceMember(t, ctx, spaceA, removed, peer)
	seedSpaceMember(t, ctx, spaceB, removed, other)
	seedMutualFriend(t, ctx, removed, buddy)
	removeSpaceMember(t, ctx, spaceA, removed)

	stub := newIMStub(t, ctx, nil)
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceA, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	uids, ok := stub.setWhitelistOf(removed)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{other, buddy}, uids,
		"仍有授权的两个人必须留下，只因 spaceA 才有授权的那个必须消失")
}

// TestDMCutoffOverwriteFailureStillCutsEnumeratedPeers 两半互不饿死。
//
// 覆写失败会上抛（工单重试），但不能因此跳过逐对端那一半——那一半是唯一能处理
// 「他还能发给谁」的路径。反过来同理，顺序在实现里是覆写在前。
func TestDMCutoffOverwriteFailureStillCutsEnumeratedPeers(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, peer = "dm-ovr-fail", "u-removed", "u-peer"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	seedDMPresence(t, ctx, removed, peer, spaceID)
	removeSpaceMember(t, ctx, spaceID, removed)

	stub := newIMStub(t, ctx, []string{peer})
	stub.failWhitelistSet(removed)

	err := f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	})
	require.Error(t, err, "覆写失败必须上抛，否则工单标 done、这一半永远不再重试")

	cut := stub.removedChannels()
	assert.Equal(t, []string{removed}, cut[peer], "覆写失败不该让逐对端那一半被跳过")
	assert.Equal(t, []string{peer}, cut[removed])
}

// TestDMRestoreReconcilesOwnChannel 恢复侧必须与移除侧同样完整。
//
// 移除侧现在整个覆写他的频道，摘掉的范围严格大于逐对端枚举能摘的范围；
// 恢复侧若只补枚举到的对端，就会补不回覆写摘掉的那些人——加入者会永久收不到
// 一部分同事的消息。这里刻意把会话列表留空：只有覆写能救回这一对。
func TestDMRestoreReconcilesOwnChannel(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, rejoiner, peer = "dm-restore-ovr", "u-rejoin", "u-peer"

	seedSpaceMember(t, ctx, spaceID, rejoiner, peer) // 他已经重新是活跃成员

	stub := newIMStub(t, ctx, nil)
	require.NoError(t, f.restoreSpaceMemberDMs(ctx, spacemod.MemberRejoin{
		SpaceID: spaceID, UID: rejoiner,
	}))

	uids, ok := stub.setWhitelistOf(rejoiner)
	require.True(t, ok, "加入者自己的频道必须被覆写一次")
	assert.Contains(t, uids, peer, "重新成为共同成员，对端要能重新发给他")
}

// TestDerivePersonWhitelistStripsSpacePrefix 推导对 Space 前缀频道也要认得出真实 uid，
// 否则 s{spaceId}_{uid} 会被当成一个不存在的用户，推导出空集。
func TestDerivePersonWhitelistStripsSpacePrefix(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, uid, peer = "0123456789abcdef0123456789abcdef", "u-self", "u-peer"

	seedSpaceMember(t, ctx, spaceID, uid, peer)

	bare, err := f.derivePersonWhitelist(ctx, uid)
	require.NoError(t, err)
	prefixed, err := f.derivePersonWhitelist(ctx, spacepkg.BuildChannelID(spaceID, uid))
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{peer}, bare)
	assert.ElementsMatch(t, bare, prefixed, "裸频道与 Space 前缀频道推导出同一个集合")
}

// TestDMCutoffOverwriteDoesNotGuessSpacePrefixOnUID 覆写路径不能对 uid 做前缀猜测。
//
// derivePersonWhitelist 的前缀剥离是启发式的（"s" 开头 + 含 "_" 就当成
// s{spaceId}_{uid}）。工单里带的是**确定的 uid**，不是 channel_id；如果这里图省事
// 复用带剥离的那个入口，一个形如 s..._... 的真实 uid 会被剥成另一个人，
// 于是把**别人的**白名单覆写到他的频道上——一次静默的误摘。
// 覆写路径必须走 derivePersonWhitelistOfUID。
func TestDMCutoffOverwriteDoesNotGuessSpacePrefixOnUID(t *testing.T) {
	ctx, f := dmTestSetup(t)
	// 这个 uid 恰好落进启发式：以 "s" 开头，且含 "_"。剥离后会变成 "me"。
	const spaceID, removed, peer, buddy = "dm-uid-guess", "s_kick_me", "u-peer", "u-buddy"

	seedSpaceMember(t, ctx, spaceID, removed, peer)
	seedMutualFriend(t, ctx, removed, buddy)
	removeSpaceMember(t, ctx, spaceID, removed)

	stub := newIMStub(t, ctx, nil)
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	uids, ok := stub.setWhitelistOf(removed)
	require.True(t, ok, "覆写要发生在这个 uid 自己的频道上")
	assert.ElementsMatch(t, []string{buddy}, uids,
		"写的必须是 s_kick_me 自己的推导集合；若被剥成 \"me\" 会得到一个空集，好友就被误摘了")

	// 顺带把两个入口的差别本身钉住，免得以后有人把它们合并回去。
	ofUID, err := f.derivePersonWhitelistOfUID(ctx, removed)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{buddy}, ofUID)

	guessed, err := f.derivePersonWhitelist(ctx, removed)
	require.NoError(t, err)
	assert.Empty(t, guessed, "带剥离的入口对这个 uid 就是会算错——这正是两个入口必须分开的理由")
}

// TestDMPeerCandidatesPrefersLongestKnownSpacePrefix 工单自带的 spaceID 只能兜底，
// 不能凌驾于最长前缀匹配之上。
//
// Space id 允许含 "_"，所以一个 id 可能是另一个的 "_" 分隔前缀——knownSpaceIDs
// 按长度倒序排正是为了这个。若无条件先剥工单自己的前缀，清理 minglue 时会把
// "sminglue_default_botfather" 剥成 "default_botfather"（正确答案是 "botfather"），
// 于是 MembersEverInSpace 匹配不上，那条 bot 私聊被静默跳过而工单标 done。
// 与非 hex 那条的区别：这次连**活跃** Space 都会中招。
func TestDMPeerCandidatesPrefersLongestKnownSpacePrefix(t *testing.T) {
	spacepkg.RegisterSpaceIDs([]string{"minglue", "minglue_default"})
	t.Cleanup(func() { spacepkg.RegisterSpaceIDs(nil) })

	convs := []*config.SyncUserConversationResp{
		{ChannelID: "sminglue_default_botfather", ChannelType: common.ChannelTypePerson.Uint8()},
	}
	assert.Equal(t, []string{"botfather"},
		dmPeerCandidates(convs, "me", "minglue"),
		"缓存认得出更长的那个 Space，就该用它，而不是先剥本次工单的短前缀")
}

// TestDMRestoreSkipsSpaceScopedWhenMembershipLostMidFlight Q4 的回归：
// 成员身份判断必须紧挨着它真正守护的那两个写。
//
// 裸 Person 频道的授权是 friends ∪ coMembers(任一活跃 Space)，跟「是不是本 Space
// 成员」无关；只有 s{spaceID}_ 前缀频道是本 Space 作用域的。所以正确的形状是
// 「裸频道照补，前缀频道在成员身份没了之后不补」。判断若留在函数开头，它离那两个
// 写隔着四次查询和两次 broker 往返，这段中途失去成员身份的情形就会补出一对
// 该 Space 已经不该有的前缀频道白名单，而该 Space 自己的清理工单早已 done。
func TestDMRestoreSkipsSpaceScopedWhenMembershipLostMidFlight(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceX, spaceY = "0123456789abcdef0123456789abcdef", "dm-q4-y"
	const uid, bot = "u-rejoin", "bot-q4"

	// 加入者与 bot 同处 X（前缀频道用得上 X），也同处 Y（裸频道仍然被授权）。
	seedSpaceMember(t, ctx, spaceX, uid, bot)
	seedSpaceMember(t, ctx, spaceY, uid, bot)
	_, err := ctx.DB().Exec(
		"INSERT INTO robot (robot_id, status, created_at, updated_at) VALUES (?, 1, NOW(), NOW())", bot)
	require.NoError(t, err)

	// 中途失去 X 的成员身份：裸频道该照补（Y 还在），X 的前缀频道不该补。
	removeSpaceMember(t, ctx, spaceX, uid)

	stub := newIMStub(t, ctx, []string{bot})
	_, err = f.restoreDM(ctx, spaceX, uid, bot, false)
	require.NoError(t, err)

	added := stub.addedChannels()
	assert.Contains(t, added, uid, "裸频道仍被 Y 的共处关系授权，必须照补")
	assert.Contains(t, added, bot)
	assert.NotContains(t, added, spacepkg.BuildChannelID(spaceX, uid),
		"已经不是 X 的成员，X 作用域的前缀频道不该被补回")
	assert.NotContains(t, added, spacepkg.BuildChannelID(spaceX, bot),
		"已经不是 X 的成员，X 作用域的前缀频道不该被补回")
}

// TestDMCutoffLeavesBotPrefixedChannelWhenConversationDeleted 钉住一个**已知缺口**，
// 不是钉住期望行为 —— 名字里的 Leaves 是故意的。
//
// 覆写只写裸 Person 频道；s{spaceID}_{uid} 前缀频道上的条目只由 cutOffDM 的 if bot
// 分支摘，而那条路要先枚举到对端。于是「bot 对端 + 会话被本地删掉」这一组合里，
// 裸频道被修好、前缀频道没人管：这正是 PR 里声明为 open 的那一半，此前没有任何用例
// 同时覆盖 bot 对端与空会话列表（既有的 bot 用例都把会话喂进去了，走的是枚举路径）。
//
// 为什么值得写：这个缺口目前只活在注释和 brief 里。有了这条用例，将来谁把覆写扩到
// 前缀频道，它会立刻变红，从而**必须**回来改掉「已关闭」的措辞，而不是让文档和代码
// 悄悄分叉——本 PR 已经在这上面栽过一次。
func TestDMCutoffLeavesBotPrefixedChannelWhenConversationDeleted(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, removed, bot = "0123456789abcdef0123456789abcdef", "u-removed", "bot-gap"

	// 共处 Space、且**不是**好友 —— 移除之后这一对本该两个方向都断掉。
	seedSpaceMember(t, ctx, spaceID, removed, bot)
	_, err := ctx.DB().Exec(
		"INSERT INTO robot (robot_id, status, created_at, updated_at) VALUES (?, 1, NOW(), NOW())", bot)
	require.NoError(t, err)
	removeSpaceMember(t, ctx, spaceID, removed)

	// 关键：会话列表空 —— 他把与 bot 的会话本地删了。
	stub := newIMStub(t, ctx, nil)
	require.NoError(t, f.cleanupSpaceMemberDMs(ctx, spacemod.MemberRemoval{
		SpaceID: spaceID, UID: removed, OperatorUID: "op", Reason: spacemod.MemberRemoveReasonKicked,
	}))

	// 修好的那一半：裸频道被覆写，bot 不在里面了。
	uids, ok := stub.setWhitelistOf(removed)
	require.True(t, ok, "裸频道必须被覆写")
	assert.NotContains(t, uids, bot, "覆写之后 bot 不能再在裸频道上发给他")

	// 仍然缺的那一半：前缀频道一个字都没动。
	prefixed := spacepkg.BuildChannelID(spaceID, removed)
	_, setOnPrefixed := stub.setWhitelistOf(prefixed)
	assert.False(t, setOnPrefixed, "当前实现不覆写前缀频道——这是记录在案的缺口，不是期望")
	assert.NotContains(t, stub.removedChannels(), prefixed,
		"枚举不到对端，前缀频道上的 whitelist_remove 也不会发生")
	assert.NotContains(t, stub.removedChannels(), spacepkg.BuildChannelID(spaceID, bot),
		"对端一侧的前缀频道同理")
}
