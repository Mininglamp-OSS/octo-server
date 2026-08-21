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
	cmds            []map[string]interface{}
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
	mux.HandleFunc("/channel/whitelist_remove", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call whitelistRemoveCall
		_ = json.Unmarshal(body, &call)
		stub.mu.Lock()
		stub.whitelistRemove = append(stub.whitelistRemove, call)
		stub.mu.Unlock()
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

func dmTestSetup(t *testing.T) (*config.Context, *Friend) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	return ctx, NewFriend(ctx)
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
