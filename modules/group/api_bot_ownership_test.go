package group

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// insertBotUser creates a user with robot=1 and an associated active robot row
// (status=1) owned by creatorUID. If creatorUID is empty, no robot row is
// created so the bot behaves as a "third-party / orphaned bot".
func insertBotUser(t *testing.T, f *Group, uid, name, shortNo, creatorUID string) {
	t.Helper()
	err := f.userDB.Insert(&user.Model{
		UID:     uid,
		Name:    name,
		ShortNo: shortNo,
		Robot:   1,
	})
	assert.NoError(t, err)
	if creatorUID == "" {
		return
	}
	_, err = f.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		uid, creatorUID,
	).Exec()
	assert.NoError(t, err)
}

// setupBotOwnershipGroup creates a normal group where the test user
// (testutil.UID, a.k.a. "user-c" in issue #1181) is already a member.
// No external bots are owned by user-c by default; individual tests insert
// the bots they need.
func setupBotOwnershipGroup(t *testing.T) (*Group, http.Handler) {
	s, ctx := newTestServer(t)
	// NB: module.Setup(ctx) inside NewTestServer already constructs a Group
	// instance and registers its routes on s.GetRoute(). Calling
	// f.Route(s.GetRoute()) again would double-register the same paths and
	// panic. We reuse the routes already registered by module.Setup and
	// only construct a local Group for DB helpers (sharing the same ctx).
	f := New(ctx)

	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// memberAdd invokes commonService.GetAppConfig which panics on a nil
	// row. Seed a minimal app_config record so the success path can reach
	// the Service layer.
	_, _ = ctx.DB().InsertBySql(
		"INSERT INTO app_config (version, invite_system_account_join_group_on) VALUES (1, 1)",
	).Exec()

	// testutil.UID plays "user-c": a regular, logged-in user who is a member
	// of the target group.
	err = f.userDB.Insert(&user.Model{UID: testutil.UID, Name: "user-c", ShortNo: "uc_bot_own"})
	assert.NoError(t, err)

	err = f.db.Insert(&Model{
		GroupNo: "g_bot_own",
		Name:    "bot ownership test",
		Creator: testutil.UID,
		Status:  GroupStatusNormal,
		Version: 1,
	})
	assert.NoError(t, err)
	err = f.db.InsertMember(&MemberModel{
		GroupNo: "g_bot_own",
		UID:     testutil.UID,
		Role:    MemberRoleCreator,
		Status:  1,
		Version: 1,
		Vercode: fmt.Sprintf("%s@1", util.GenerUUID()),
	})
	assert.NoError(t, err)
	wireI18nRendererForGroupTest(s)
	return f, s.GetRoute()
}

func postAddMembers(t *testing.T, handler http.Handler, groupNo string, members []string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	body := util.ToJson(map[string]interface{}{"members": members})
	req, err := http.NewRequest("POST", "/v1/groups/"+groupNo+"/members", bytes.NewReader([]byte(body)))
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	handler.ServeHTTP(w, req)
	return w
}

// TestGroupMemberAdd_BotOwnedBySelf — acceptance case 1 (YUJ-46):
// user-c adds their own bot → 200.
func TestGroupMemberAdd_BotOwnedBySelf(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	f, h := setupBotOwnershipGroup(t)

	insertBotUser(t, f, "thomas_fu_bot", "thomas", "thomas_bot_sn", testutil.UID)

	w := postAddMembers(t, h, "g_bot_own", []string{"thomas_fu_bot"})
	if !assert.Equal(t, http.StatusOK, w.Code, "user-c 邀请自己的 bot 应返回 200, got body=%s", w.Body.String()) {
		return
	}

	exist, err := f.db.ExistMember("thomas_fu_bot", "g_bot_own")
	assert.NoError(t, err)
	assert.True(t, exist, "自己的 bot 应被加入群")
}

// TestGroupMemberAdd_BotOwnedByOther — acceptance case 2 (YUJ-46):
// user-c tries to invite user-a's bot → 403, bot must NOT be added.
func TestGroupMemberAdd_BotOwnedByOther(t *testing.T) {
	f, h := setupBotOwnershipGroup(t)

	// user-a is the actual creator of yutestspacebot1_bot
	err := f.userDB.Insert(&user.Model{UID: "user_a", Name: "user-a", ShortNo: "ua_bot_own"})
	assert.NoError(t, err)
	insertBotUser(t, f, "yutestspacebot1_bot", "YuTestSpaceBot1", "yts1_bot_sn", "user_a")

	w := postAddMembers(t, h, "g_bot_own", []string{"yutestspacebot1_bot"})
	// D14: wire status is fixed at 400 during the compat window; the 403
	// semantics now live in error.http_status / error.code.
	assert.Equal(t, http.StatusBadRequest, w.Code, "user-c 邀请别人的 bot 应返回 400 信封, got body=%s", w.Body.String())
	assert.True(t, strings.Contains(w.Body.String(), "err.server.group.bot_ownership_denied"),
		"响应体应包含 bot 归属错误码, got=%s", w.Body.String())

	exist, err := f.db.ExistMember("yutestspacebot1_bot", "g_bot_own")
	assert.NoError(t, err)
	assert.False(t, exist, "别人的 bot 不应被加入群")
}

// TestGroupMemberAdd_BotThirdPartyNoCreator — acceptance case 3 (YUJ-46):
// user-c tries to invite a bot whose creator_uid is empty (orphaned /
// third-party bot without a registered creator) → 403.
func TestGroupMemberAdd_BotThirdPartyNoCreator(t *testing.T) {
	f, h := setupBotOwnershipGroup(t)

	// Third-party bot: user.robot=1 but no corresponding robot row
	// (the strictest "orphaned bot" shape — treated as unowned).
	insertBotUser(t, f, "ppt_bot", "PPT Bot", "ppt_bot_sn", "")

	w := postAddMembers(t, h, "g_bot_own", []string{"ppt_bot"})
	assert.Equal(t, http.StatusBadRequest, w.Code, "第三方 bot 应返回 400 信封, got body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.bot_ownership_denied", "got=%s", w.Body.String())

	exist, err := f.db.ExistMember("ppt_bot", "g_bot_own")
	assert.NoError(t, err)
	assert.False(t, exist, "第三方 bot 不应被加入群")
}

// TestGroupMemberAdd_BotOwnershipBlocksMixedBatch — defense in depth:
// if a batch contains a legal human + an illegally invited bot, the whole
// batch is rejected so we don't accidentally half-apply.
func TestGroupMemberAdd_BotOwnershipBlocksMixedBatch(t *testing.T) {
	f, h := setupBotOwnershipGroup(t)

	err := f.userDB.Insert(&user.Model{UID: "user_b", Name: "user-b", ShortNo: "ub_bot_own"})
	assert.NoError(t, err)
	err = f.userDB.Insert(&user.Model{UID: "human_friend", Name: "friend", ShortNo: "hf_bot_own"})
	assert.NoError(t, err)
	insertBotUser(t, f, "spacebottest1_bot", "SpaceBotTest1", "sbt1_bot_sn", "user_b")

	w := postAddMembers(t, h, "g_bot_own", []string{"human_friend", "spacebottest1_bot"})
	assert.Equal(t, http.StatusBadRequest, w.Code, "mixed batch with foreign bot 应返回 400 信封, got body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.bot_ownership_denied", "got=%s", w.Body.String())

	// 混合批次被拒时，所有成员都不应被写入（避免半提交）
	exist, err := f.db.ExistMember("spacebottest1_bot", "g_bot_own")
	assert.NoError(t, err)
	assert.False(t, exist, "别人的 bot 不应被加入群")
	exist, err = f.db.ExistMember("human_friend", "g_bot_own")
	assert.NoError(t, err)
	assert.False(t, exist, "混合批次被拒时，human 也不应被写入")
}

// TestCheckBotOwnership_SkipsNonBots exercises the pure helper: non-bot UIDs
// pass through untouched regardless of inviter.
func TestCheckBotOwnership_SkipsNonBots(t *testing.T) {
	_, ctx := newTestServer(t)
	assert.NoError(t, testutil.CleanAllTables(ctx))

	_, err := ctx.DB().InsertInto("user").
		Columns("uid", "name", "short_no", "robot").
		Values("human_x", "Human X", "hx_bot_own", 0).Exec()
	assert.NoError(t, err)

	err = checkBotOwnership(ctx.DB(), "some_inviter", []string{"human_x"})
	assert.NoError(t, err)

	// Empty inputs are also fine
	assert.NoError(t, checkBotOwnership(ctx.DB(), "", []string{"human_x"}))
	assert.NoError(t, checkBotOwnership(ctx.DB(), "u1", nil))
}
