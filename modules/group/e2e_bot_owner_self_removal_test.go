//go:build wukong_e2e

// 自助移除 bot 的端到端验证（task bot-owner-self-removal / octo-web#1511），
// 打**真实** WuKongIM，不使用 newGroupIMStub。
//
// 与 api_bot_owner_self_removal_test.go 的区别很关键：那里的 Tip 断言打在录制桩上，
// 只能证明「服务端发起了一次 /message/send」；证明不了 WuKongIM 收下了它、更证明
// 不了群里的人真的看得到。本文件从**留在群里的成员视角**把会话同步回来，断言那条
// 系统消息确实出现在群频道的最近消息里。
//
// 同样从真实 IM 侧验证退订：被移除的 bot 不该再看到这个群频道。
//
// 与 e2e_issue27_wukong_test.go 同一约定：wukong_e2e 构建标签，opt-in，不进 CI
// （WuKongIM 的投递语义与版本耦合）。
//
// 运行（需要 CI 同款栈：MySQL + Redis + WuKongIM 默认端口，test 库为
// utf8mb4_general_ci）：
//
//	go test -tags wukong_e2e ./modules/group/ -run TestE2E_BotOwnerSelfRemoval -v -count=1
package group

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2E_BotOwnerSelfRemoval_GroupSeesTipAndBotIsUnsubscribed(t *testing.T) {
	s, ctx := newTestServer(t)
	require.NoError(t, testutil.CleanAllTables(ctx))
	f := New(ctx)
	wireI18nRendererForGroupTest(s)
	resetGroupUIDRateLimit(t, ctx)

	// WuKongIM 的数据落在 ~/.wukong 且跨进程持久，固定频道 ID 会让相邻两次运行
	// 互相污染（上一轮的消息/会话还在）。每次跑用一个唯一 groupNo。
	runID := time.Now().UnixNano()
	groupNo := fmt.Sprintf("e2e_botrm_%d", runID)
	ownerUID := fmt.Sprintf("e2e_owner_%d", runID) // 群主，留在群里 —— 用他的视角验「群里看得到」
	botUID := fmt.Sprintf("e2e_bot_%d", runID)
	const botName = "报表助手"
	ct := common.ChannelTypeGroup.Uint8()

	require.NoError(t, f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "bot-owner", ShortNo: "e2e_op"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: ownerUID, Name: "group-owner", ShortNo: "e2e_ow"}))
	require.NoError(t, f.userDB.Insert(&user.Model{UID: botUID, Name: botName, ShortNo: "e2e_bt", Robot: 1}))
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)", botUID, testutil.UID,
	).Exec()
	require.NoError(t, err)

	require.NoError(t, f.db.Insert(&Model{
		GroupNo: groupNo, Name: "e2e bot removal", Creator: ownerUID,
		Status: GroupStatusNormal, Version: 1,
	}))
	seedE2EMember(t, f, groupNo, ownerUID, MemberRoleCreator, 0)
	seedE2EMember(t, f, groupNo, testutil.UID, MemberRoleCommon, 0) // 发起人：普通成员
	seedE2EMember(t, f, groupNo, botUID, MemberRoleCommon, 1)

	// 三方都订阅群频道（与真实入群一致）。
	require.NoError(t, ctx.IMAddSubscriber(&config.SubscriberAddReq{
		ChannelID: groupNo, ChannelType: ct,
		Subscribers: []string{ownerUID, testutil.UID, botUID},
	}))

	hasGroupChannel := func(uid string) bool {
		convs, serr := ctx.IMSyncUserConversation(uid, 0, 50, "", nil)
		require.NoError(t, serr)
		for _, c := range convs {
			if c.ChannelID == groupNo && c.ChannelType == ct {
				return true
			}
		}
		return false
	}
	// recentsOf 返回某成员在群频道里能看到的最近消息文本。
	recentsOf := func(uid string) []string {
		convs, serr := ctx.IMSyncUserConversation(uid, 0, 50, "", nil)
		require.NoError(t, serr)
		var out []string
		for _, c := range convs {
			if c.ChannelID != groupNo || c.ChannelType != ct {
				continue
			}
			for _, m := range c.Recents {
				out = append(out, string(m.Payload))
			}
		}
		return out
	}

	// 会话只在**有消息投递**后才产生，光订阅不算 —— 所以先发一条预热消息，
	// 才能用 conversation/sync 观察「谁收得到这个群频道」。
	sendToGroup := func(content string) {
		require.NoError(t, ctx.SendMessage(&config.MsgSendReq{
			Header:      config.MsgHeader{RedDot: 1},
			FromUID:     ownerUID,
			ChannelID:   groupNo,
			ChannelType: ct,
			Payload:     []byte(`{"type":1,"content":"` + content + `"}`),
		}))
	}

	// --- 1. 移除前：bot 确实在收这个群频道的消息 ---
	sendToGroup("before-remove")
	time.Sleep(800 * time.Millisecond)
	require.True(t, hasGroupChannel(botUID), "前置：未移除时 bot 应通过 WuKongIM 收到群消息")
	require.True(t, hasGroupChannel(ownerUID), "前置：群主应收到")

	// --- 2. 普通成员走真实 HTTP 路径自助移除自己的 bot ---
	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"members": []string{botUID}})
	req, rerr := http.NewRequest("DELETE", "/v1/groups/"+groupNo+"/members", bytes.NewReader([]byte(body)))
	require.NoError(t, rerr)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "自助移除应成功: %s", w.Body.String())

	exist, eerr := f.db.ExistMember(botUID, groupNo)
	require.NoError(t, eerr)
	require.False(t, exist, "bot 应已不在成员表里")

	// WuKongIM 的投递/落库是异步的，给一个有界的重试窗口，别用固定 sleep。
	var seen []string
	for i := 0; i < 30; i++ {
		seen = recentsOf(ownerUID)
		if containsFragment(seen, "将机器人") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	// --- 3. 核心断言：留在群里的人真的看得到这条系统消息 ---
	//
	// 句式是「{操作者} 将机器人 {bot 名} 移出了群聊」。注意操作者名取自
	// c.GetLoginName()（登录态），不是 user 表的 Name —— 测试里登录名是 testutil
	// 约定的值，所以这里只断言「有一个非空前缀」，不写死具体名字。
	tailWithBot := fmt.Sprintf("将机器人 %s 移出了群聊", botName)
	assert.True(t, containsFragment(seen, tailWithBot),
		"群主应在群频道里看到「X %s」，实际最近消息=%v", tailWithBot, seen)
	assert.True(t, containsFragment(seen, `"type":2000`),
		"这条应是系统 Tip（common.Tip=2000），实际=%v", seen)
	assert.True(t, hasNonEmptyOperatorPrefix(seen, tailWithBot),
		"系统消息里应带上操作者名字作为前缀，实际=%v", seen)
	// 反向：不该出现被移除者视角的旧文案。
	assert.False(t, containsFragment(seen, "移除群聊"),
		"不应出现「你被 X 移除群聊」，实际=%v", seen)

	// --- 4. 退订从真实 IM 侧验证：移除后再发一条，bot 不该再收到 ---
	// 与 e2e_issue27 同口径：判据是「新消息还投不投得到他」，而不是历史会话是否残留。
	sendToGroup("after-remove")
	time.Sleep(800 * time.Millisecond)
	assert.False(t, hasGroupChannel(botUID),
		"被移除的 bot 必须不再收到群消息，否则「移除」只是名单上的假象")
	assert.True(t, hasGroupChannel(ownerUID),
		"对照：仍在群里的成员必须继续收到，证明摘订阅是精确按 uid 的")
}

func seedE2EMember(t *testing.T, f *Group, groupNo, uid string, role, robot int) {
	t.Helper()
	require.NoError(t, f.db.InsertMember(&MemberModel{
		GroupNo: groupNo, UID: uid, Role: role, Robot: robot,
		Status: 1, Version: 1, Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	}))
}

func containsFragment(payloads []string, fragment string) bool {
	for _, p := range payloads {
		if strings.Contains(p, fragment) {
			return true
		}
	}
	return false
}

// hasNonEmptyOperatorPrefix 确认 Tip 里 tail 之前确实插值了一个非空的操作者名，
// 而不是「 将机器人 X 移出了群聊」这种前缀丢失的形态。
func hasNonEmptyOperatorPrefix(payloads []string, tail string) bool {
	for _, p := range payloads {
		idx := strings.Index(p, tail)
		if idx <= 0 {
			continue
		}
		// 取 content 字段起始到 tail 之间的部分，去掉 JSON 前缀与空白后必须非空。
		prefix := p[:idx]
		if ci := strings.Index(prefix, `"content":"`); ci >= 0 {
			prefix = prefix[ci+len(`"content":"`):]
		}
		if strings.TrimSpace(prefix) != "" {
			return true
		}
	}
	return false
}
