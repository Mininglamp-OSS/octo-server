package group

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bot_created_by_me 的下发口径（「移出成员」分类页 / octo-web#1511 后续）。
//
// 这组测试钉住一条**语义分离**：
//
//	bot_owned_by_me   = 我**能撤**它（归属 AND 目标是普通角色）
//	bot_created_by_me = 我**建**了它（只看 robot.creator_uid）
//
// 为什么要分开：前端「移出成员」页按「我的 BOT / 其他成员」分类，分类依据是
// **归属**，而行的可见性依据是**权限**。用一个字段兼职两件事的后果是——
// 群主拥有、但被提为管理员的 bot：owned=false（自助分支不放行），但群主本人
// 确实能移除它，于是它会带着可点的移除按钮出现在「其他成员」组里。
//
// 关键的回归风险是**两个字段被写成同一个判据**（比如有人图省事把 role 门提到
// 归属赋值之前，或者反过来把 role 门整个删掉）。前者让分类退化、后者是提权。
// 所以下面每条用例都成对断言两个字段，而不是只测新字段。

// 核心分离用例：同一个 bot，两个字段给出不同答案。
//
// 这是整组里唯一真正会因为「把 role 门提前到 BotCreatedByMe 赋值之前」而变红的
// 用例 —— 那个改动不影响任何既有断言，却会让分类功能静默退化。
func TestBotCreatedByMe_RoleBotIsCreatedButNotOwned(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)

	// 我建的 bot，但它在本群被提为管理员。
	require.NoError(t, f.userDB.Insert(&user.Model{
		UID: "bot_mgr_split", Name: "mgr-bot", ShortNo: "bmgrsplit", Robot: 1,
	}))
	_, err := f.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		"bot_mgr_split", testutil.UID,
	).Exec()
	require.NoError(t, err)
	seedSelfRemovalMember(t, f, "bot_mgr_split", MemberRoleManager, 1)

	// 对照组：同样是我建的，但角色普通。
	seedSelfRemovalBot(t, f, "bot_plain_split", "plain-bot", testutil.UID, 1)

	owned, created := membersGetBotFlags(t, handler, selfRemovalGroupNo)

	// 管理员角色的 bot：建了 ≠ 能撤。
	assert.True(t, created["bot_mgr_split"],
		"bot_created_by_me 是纯归属，不该被 group_member.role 影响 —— "+
			"否则「移出成员」页会把群主自己建的 bot 归到「其他成员」组")
	assert.False(t, owned["bot_mgr_split"],
		"bot_owned_by_me 的权限语义必须保持不变：角色 bot 不走自助分支")

	// 普通角色的 bot：两个字段应一致为 true。
	assert.True(t, created["bot_plain_split"], "对照：普通角色的自有 bot 归属为 true")
	assert.True(t, owned["bot_plain_split"], "对照：普通角色的自有 bot 可移除")
}

// 他人的 bot 与人类成员：两个字段都恒为 false。
//
// 防的是「归属字段忘了查 owned 白名单、直接按 robot=1 赋值」——那会把全群所有
// bot 都塞进「我的 BOT」组，并且泄露了别人 bot 的存在感。
func TestBotCreatedByMe_OthersBotAndHumansStayFalse(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_created_mine", "mine", testutil.UID, 1)
	seedSelfRemovalBot(t, f, "bot_created_theirs", "theirs", "owner_other", 1)

	owned, created := membersGetBotFlags(t, handler, selfRemovalGroupNo)

	assert.True(t, created["bot_created_mine"], "自己建的 bot 应为 true")
	assert.False(t, created["bot_created_theirs"],
		"他人建的 bot 必须为 false —— 否则分类会把别人的 bot 放进「我的 BOT」")
	assert.False(t, created[testutil.UID], "人类成员恒为 false")
	assert.False(t, created["owner_other"], "人类成员恒为 false")

	// 同步确认既有字段没被这次改动带偏。
	assert.True(t, owned["bot_created_mine"])
	assert.False(t, owned["bot_created_theirs"])
}

// 被拉黑的所有者拿不到 bot_created_by_me=true。
//
// 新字段仍在 fillBotOwnedByMe 的活跃成员门（ExistMemberActive）之后回填，所以它
// 严格说是「活跃成员视角的归属」。这是有意的：被拉黑的人本来就进不了移出页，
// 多一道门零风险。这条用例把该决策钉住，免得后人为了「纯归属」把赋值挪到门前面
// —— 那会让被拉黑的人从成员列表里读出自己名下 bot 的分布。
func TestBotCreatedByMe_BlacklistedOwnerGetsNothing(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_bl_created", "mine", testutil.UID, 1)

	_, err := f.ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(common.GroupMemberStatusBlacklist), selfRemovalGroupNo, testutil.UID,
	).Exec()
	require.NoError(t, err)

	owned, created := membersGetBotFlags(t, handler, selfRemovalGroupNo)
	assert.False(t, created["bot_bl_created"],
		"被拉黑的所有者不该拿到 bot_created_by_me —— 活跃成员门对两个字段一致生效")
	assert.False(t, owned["bot_bl_created"], "既有语义：同样为 false")
}

// 三处下发端点同名同型（membersGet / memberGet / membersync）。
//
// fillBotOwnedByMe 是三者共用的，所以理论上不会漏；钉住它是因为本字段的前身
// 真在 memberGet 上踩过「选择列漏了 group_member.robot → 字段静默恒 false」的坑
// （见 api_bot_owner_self_removal_test.go 的同类用例）。Android/iOS 走 WKSDK
// ChannelMember.extraMap 缓存，漏下发就永远分不出组。
func TestBotCreatedByMe_ExposedByAllThreeEndpoints(t *testing.T) {
	f, handler, ctx := setupBotSelfRemovalGroup(t)
	newGroupIMStub(t, ctx)
	seedSelfRemovalBot(t, f, "bot_three_ep", "mine", testutil.UID, 1)

	// 1) membersGet
	_, created := membersGetBotFlags(t, handler, selfRemovalGroupNo)
	assert.True(t, created["bot_three_ep"], "membersGet 应下发 bot_created_by_me")

	// 2) memberGet（单成员）
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET",
		"/v1/groups/"+selfRemovalGroupNo+"/members/bot_three_ep", nil)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "memberGet 应可读: %s", w.Body.String())

	var single struct {
		Exists bool `json:"exists"`
		Member struct {
			UID            string `json:"uid"`
			BotOwnedByMe   bool   `json:"bot_owned_by_me"`
			BotCreatedByMe bool   `json:"bot_created_by_me"`
		} `json:"member"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &single))
	require.True(t, single.Exists)
	assert.True(t, single.Member.BotCreatedByMe, "memberGet 也要如实下发 bot_created_by_me")
	assert.True(t, single.Member.BotOwnedByMe, "既有字段不变")

	// 3) membersync（增量同步，Android 缓存的唯一来源）
	wSync := httptest.NewRecorder()
	reqSync, err := http.NewRequest("GET",
		"/v1/groups/"+selfRemovalGroupNo+"/membersync?version=0&limit=100", nil)
	require.NoError(t, err)
	reqSync.Header.Set("token", testutil.Token)
	handler.ServeHTTP(wSync, reqSync)
	require.Equal(t, http.StatusOK, wSync.Code, "membersync 应可读: %s", wSync.Body.String())

	var syncMembers []map[string]interface{}
	require.NoError(t, json.Unmarshal(wSync.Body.Bytes(), &syncMembers))
	syncFlags := map[string]bool{}
	for _, m := range syncMembers {
		uid, _ := m["uid"].(string)
		createdFlag, present := m["bot_created_by_me"].(bool)
		require.True(t, present,
			"membersync 的每一行都应带 bot_created_by_me（uid=%s）—— 缺字段客户端无法分组", uid)
		syncFlags[uid] = createdFlag
	}
	assert.True(t, syncFlags["bot_three_ep"], "membersync 应下发 bot_created_by_me=true")
	assert.False(t, syncFlags[testutil.UID], "人类成员恒为 false")
}

// membersGetBotFlags 拉一次成员列表，同时返回两张表：
// uid -> bot_owned_by_me 与 uid -> bot_created_by_me。
//
// 不复用既有的 membersGetOwnershipFlags：那个只返回 owned，而本组测试的全部价值
// 就在于**成对**断言两个字段。分开取会让「两字段被写成同一判据」这类回归逃逸。
func membersGetBotFlags(
	t *testing.T, handler http.Handler, groupNo string,
) (owned map[string]bool, created map[string]bool) {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "/v1/groups/"+groupNo+"/members?page=1&limit=50", nil)
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "成员列表应可读: %s", w.Body.String())

	var members []map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &members))
	owned = map[string]bool{}
	created = map[string]bool{}
	for _, m := range members {
		uid, _ := m["uid"].(string)
		ownedFlag, ownedPresent := m["bot_owned_by_me"].(bool)
		require.True(t, ownedPresent, "每行都应带 bot_owned_by_me（uid=%s）", uid)
		createdFlag, createdPresent := m["bot_created_by_me"].(bool)
		require.True(t, createdPresent, "每行都应带 bot_created_by_me（uid=%s）", uid)
		owned[uid] = ownedFlag
		created[uid] = createdFlag
	}
	return owned, created
}
