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
	cmds            []map[string]interface{}
	// failChannels 里的 channel_id 上，whitelist_remove 返回 500，用来断言
	// 摘白名单失败会被上抛而不是吞掉。
	failChannels map[string]bool
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
	assert.Equal(t, []string{"peer-a", "peer-b"}, dmPeerCandidates(convs, self))
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

	require.NoError(t, f.restoreDM(ctx, spaceID, rejoiner, peer, true, true, false))

	assert.Empty(t, stub.addedChannels(),
		"成员已不再活跃时，陈旧的回补一条白名单都不许写——否则会永久复活越权私聊")
}

// TestDMRestoreStillGrantsWhileMemberActive 上面那条守卫不能把正常回补也挡掉。
func TestDMRestoreStillGrantsWhileMemberActive(t *testing.T) {
	ctx, f := dmTestSetup(t)
	const spaceID, uid, peer = "sp-active-restore", "u-active", "u-peer"

	seedSpaceMember(t, ctx, spaceID, uid, peer)
	stub := newIMStub(t, ctx, []string{peer})

	require.NoError(t, f.restoreDM(ctx, spaceID, uid, peer, true, true, false))

	added := stub.addedChannels()
	assert.Contains(t, added[peer], uid)
	assert.Contains(t, added[uid], peer)
}
