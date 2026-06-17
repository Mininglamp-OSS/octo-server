//go:build wukong_e2e

// End-to-end verification for Issue #27 against a live WuKongIM (channel_type=5,
// 子区/CommunityTopic). Opt-in only — excluded from normal `go test` by the
// `wukong_e2e` build tag so it never burdens CI (WuKongIM message-delivery
// semantics are version-coupled; see PR #300 review discussion).
//
// Run (requires the CI-equivalent stack: MySQL+Redis+WuKongIM on default ports,
// OCTO_MASTER_KEY set, `test` DB as utf8mb4_general_ci):
//
//	go test -tags wukong_e2e ./modules/group/ -run TestE2E_Issue27 -v -count=1
//
// What it proves, through the REAL fixed code path:
//  1. A bot subscribed to a thread channel (as addUsersToGroupThreads does on
//     join) actually RECEIVES messages pushed to that channel — the #27 leak.
//  2. After removeUserFromGroupThreadsCleanup (the fix) runs for that bot —
//     even though the bot has NO thread_member row (the exact #27 scenario the
//     old JOIN query skipped) — the bot STOPS receiving thread messages at the
//     WuKongIM layer, while a still-subscribed user keeps receiving.
package group

import (
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/require"
)

func TestE2E_Issue27_RemovedBotStopsReceivingThreadMessages(t *testing.T) {
	_, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	f := New(ctx)
	ensureThreadTables(t, f)

	const (
		groupNo = "e2e_g27"
		shortID = "e2eth01"
		spaceID = "e2e_space"
		botUID  = "e2e_bot"   // 入群但从不 JoinThread —— #27 的典型 bot
		usrUID  = "e2e_user"  // 普通成员，对照组
		sender  = "e2e_sender"
	)
	channelID := groupNo + "____" + shortID
	ct := common.ChannelTypeCommunityTopic.Uint8() // == 5

	// 入群时挂订阅（与 addUsersToGroupThreads 一致：bot 和 user 都订阅，无 thread_member）
	require.NoError(t, ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID: channelID, ChannelType: ct, Subscribers: []string{botUID, usrUID},
	}))

	// seed 一个 active 子区，但故意不给 bot 建 thread_member 行（#27 场景）
	_, err := ctx.DB().InsertInto("thread").
		Columns("short_id", "group_no", "name", "creator_uid", "status", "version").
		Values(shortID, groupNo, "e2e-sub", "owner", 1, 1).Exec()
	require.NoError(t, err)

	sendToThread := func(content string) {
		require.NoError(t, ctx.SendMessage(&config.MsgSendReq{
			Header:      config.MsgHeader{RedDot: 1},
			FromUID:     sender,
			ChannelID:   channelID,
			ChannelType: ct,
			Payload:     []byte(`{"type":1,"content":"` + content + `"}`),
		}))
	}
	hasChannel := func(uid string) bool {
		convs, serr := ctx.IMSyncUserConversation(uid, 0, 50, "", nil)
		require.NoError(t, serr)
		for _, c := range convs {
			if c.ChannelID == channelID && c.ChannelType == ct {
				return true
			}
		}
		return false
	}

	// --- 1. 移除前：bot 是订阅者，应收到子区消息 ---
	sendToThread("before-remove")
	time.Sleep(800 * time.Millisecond)
	require.True(t, hasChannel(botUID), "前置：未移除时 bot 应通过 WuKongIM 收到子区消息（#27 泄漏面）")
	require.True(t, hasChannel(usrUID), "前置：普通成员应收到")

	// --- 2. 跑修复后的真实 helper（bot 无 thread_member 行，旧 JOIN 查询会直接跳过）---
	removeUserFromGroupThreadsCleanup(ctx, f.Log, groupNo, botUID, spaceID)

	// --- 3. 移除后：再发一条，bot 不应再收到；user 仍订阅、继续收到 ---
	sendToThread("after-remove")
	time.Sleep(800 * time.Millisecond)
	require.False(t, hasChannel(botUID),
		"修复生效：removeUserFromGroupThreadsCleanup 后被移除的 bot 必须不再收到子区消息（Issue #27）")
	require.True(t, hasChannel(usrUID),
		"对照：仍在订阅的成员必须继续收到，证明摘订阅是精确按 uid 的")
}
