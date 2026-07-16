//go:build integration

// 构建约束（与 api_card_action_test.go 一致，PR#548 CI 修复）：本文件经
// testutil.NewTestServer 跑 sql-migrate，必须带 integration tag —— modules/message
// 默认构建混有「手建最小表」的单元测试，跑迁移的用例与之在 -race -shuffle 下并存会撞
// Error 1050 Table already exists（本包 e2e 文件皆 //go:build integration）。
package message

// card-message-interaction P2 D10 集成测试（MySQL + Redis，无需 WuKongIM）。
// spec: .octospec/tasks/card-message-interaction/brief.md；执行 brief:
// .octospec/tasks/card-message-p2-revision-history/brief.md。
//
// 覆盖：GET /v1/message/card/revisions 成员门禁(复用 authorizeCardChannelMember)、
// summary/full=1 投影、墓碑行渲染、跨频道 IDOR、cap 20 裁剪、撤回删除(store 直测)。
// bot 编辑追加 / clear 端点由 bot_api 的 IM-gated 用例承接。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardrevision"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/assert"
)

// seedRevisionFrame 经 store 追加一条非墓碑帧（同时验证 AppendFrame + cap 裁剪）。
func seedRevisionFrame(t *testing.T, store *cardrevision.Store, messageID, channelID string, channelType uint8, cardSeq int64, plain, actionID string) {
	t.Helper()
	env := cardV2EnvelopeJSON(t, actionID)
	err := store.AppendFrame(cardrevision.Revision{
		MessageID:   messageID,
		ChannelID:   channelID,
		ChannelType: channelType,
		CardSeq:     dbr.NewNullInt64(cardSeq),
		Content:     dbr.NewNullString(string(env)),
		Plain:       plain,
		EditorUID:   cardActionBotUID,
		EditedAt:    1751791200 + cardSeq,
	})
	assert.NoError(t, err)
}

func TestCardRevisionsQuery(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)
	store := cardrevision.NewStore(ctx.DB())

	seedCardBot(t, ctx, cardActionBotUID)
	fake := common.GetFakeChannelIDWith(testutil.UID, cardActionBotUID)
	seedCardMessage(t, ctx, 9201, cardActionBotUID, fake, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	seedRevisionFrame(t, store, "9201", fake, common.ChannelTypePerson.Uint8(), 1, "审批单 #42:待审批", "approve_btn")
	seedRevisionFrame(t, store, "9201", fake, common.ChannelTypePerson.Uint8(), 2, "审批单 #42:已通过", "done_btn")

	get := func(q string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/message/card/revisions?"+q, nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	base := fmt.Sprintf("message_id=9201&channel_id=%s&channel_type=%d", cardActionBotUID, common.ChannelTypePerson.Uint8())

	// ① 成员 summary 查询：最新在前，字段齐全，默认不带 full card。
	w := get(base)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		Revisions []map[string]interface{} `json:"revisions"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Revisions, 2)
	assert.EqualValues(t, 2, resp.Revisions[0]["card_seq"], "最新帧在前")
	assert.Equal(t, "审批单 #42:已通过", resp.Revisions[0]["plain"])
	assert.Equal(t, cardActionBotUID, resp.Revisions[0]["editor_uid"])
	assert.Nil(t, resp.Revisions[0]["card"], "summary 模式不含完整帧")

	// ② full=1：附完整帧信封，且能过 cardmsg.Validate。
	w = get(base + "&full=1")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	rawCard, ok := resp.Revisions[0]["card"]
	assert.True(t, ok, "full=1 应带 card")
	cardBytes, _ := json.Marshal(rawCard)
	var env map[string]interface{}
	assert.NoError(t, json.Unmarshal(cardBytes, &env))
	assert.NoError(t, cardmsg.Validate(env), "full 帧应过校验器")

	// ③ 墓碑行：store.Clear 删帧 + 写墓碑，出现在列表里。
	cleared, err := store.Clear("9201", fake, common.ChannelTypePerson.Uint8(), cardActionBotUID, 1751791300)
	assert.NoError(t, err)
	assert.Equal(t, 2, cleared, "清除 2 帧")
	w = get(base)
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Len(t, resp.Revisions, 1)
	assert.Equal(t, true, resp.Revisions[0]["tombstone"])
	assert.EqualValues(t, 2, resp.Revisions[0]["cleared"])
}

func TestCardRevisionsMemberGate(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)
	store := cardrevision.NewStore(ctx.DB())

	seedCardBot(t, ctx, cardActionBotUID)
	get := func(q string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", "/v1/message/card/revisions?"+q, nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// 群卡片，testutil.UID 非成员 → denied（复用 card/action 同门禁）。群行须为
	// Normal 状态，把唯一拒因隔离到「非成员」（否则会先撞群状态门禁 → invalid）。
	_, err := ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'grt', ?, '')", "g_rev_test", group.GroupStatusNormal).Exec()
	assert.NoError(t, err)
	seedCardMessage(t, ctx, 9202, cardActionBotUID, "g_rev_test", common.ChannelTypeGroup.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	seedRevisionFrame(t, store, "9202", "g_rev_test", common.ChannelTypeGroup.Uint8(), 1, "x", "approve_btn")
	w := get(fmt.Sprintf("message_id=9202&channel_id=g_rev_test&channel_type=%d", common.ChannelTypeGroup.Uint8()))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "You are not allowed to view this card's revisions.")

	// 跨频道 IDOR：B 群消息 + B 的 channel_id，非成员 → denied（绑定源自存储行）。
	_, err = ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'grb', ?, '')", "g_rev_b", group.GroupStatusNormal).Exec()
	assert.NoError(t, err)
	seedCardMessage(t, ctx, 9203, cardActionBotUID, "g_rev_b", common.ChannelTypeGroup.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	w = get(fmt.Sprintf("message_id=9203&channel_id=g_rev_b&channel_type=%d", common.ChannelTypeGroup.Uint8()))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "You are not allowed to view this card's revisions.")

	// person fake 频道指群消息 → 绑定不匹配（查不到）→ invalid。
	w = get(fmt.Sprintf("message_id=9203&channel_id=%s&channel_type=%d", cardActionBotUID, common.ChannelTypePerson.Uint8()))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "Invalid card revision request.")
}

func TestCardRevisionsCapAndDelete(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	_, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	store := cardrevision.NewStore(ctx.DB())
	fake := common.GetFakeChannelIDWith(testutil.UID, cardActionBotUID)

	// cap 裁剪：追加 cap+5 帧，仅保留最新 cap 帧。
	for i := 1; i <= cardrevision.CapFrames+5; i++ {
		seedRevisionFrame(t, store, "9301", fake, common.ChannelTypePerson.Uint8(), int64(i), fmt.Sprintf("f%d", i), "approve_btn")
	}
	revs, err := store.Query("9301", 1000)
	assert.NoError(t, err)
	assert.Len(t, revs, cardrevision.CapFrames, "非墓碑帧应被裁剪到 cap")
	assert.EqualValues(t, cardrevision.CapFrames+5, revs[0].CardSeq.Int64, "保留的是最新帧")

	// DeleteByMessageID（撤回清理的底层）：删后查询为空。
	assert.NoError(t, store.DeleteByMessageID("9301"))
	revs, err = store.Query("9301", 1000)
	assert.NoError(t, err)
	assert.Len(t, revs, 0, "删除后应无修订")
}

// TestCardRevisionsWithdrawnHidden 验证撤回/删除可见性兜底（verify P1）：即便修订
// 行仍在，已撤回 / 全局删除 / 操作者本地删除的卡片，GET 返回空列表（不泄漏历史内容）。
func TestCardRevisionsWithdrawnHidden(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)
	store := cardrevision.NewStore(ctx.DB())
	fake := common.GetFakeChannelIDWith(testutil.UID, cardActionBotUID)

	get := func(msgID string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		q := fmt.Sprintf("message_id=%s&channel_id=%s&channel_type=%d", msgID, cardActionBotUID, common.ChannelTypePerson.Uint8())
		req, _ := http.NewRequest("GET", "/v1/message/card/revisions?"+q, nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	emptyRevisions := func(w *httptest.ResponseRecorder) bool {
		var resp struct {
			Revisions []map[string]interface{} `json:"revisions"`
		}
		return json.Unmarshal(w.Body.Bytes(), &resp) == nil && len(resp.Revisions) == 0
	}

	// 三种回收态：revoke / 全局 is_deleted / 操作者 message_is_deleted。每种都有修订
	// 行存在，但 GET 必须返回空（内容历史不可查）。
	cases := []struct {
		msgID  int64
		seedWD func(msgID string)
	}{
		{9401, func(id string) {
			_, e := ctx.DB().InsertBySql("INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,`revoke`,version) VALUES (?,?,?,?,?,?)",
				id, 1, fake, common.ChannelTypePerson.Uint8(), 1, 1).Exec()
			assert.NoError(t, e)
		}},
		{9402, func(id string) {
			_, e := ctx.DB().InsertBySql("INSERT INTO message_extra (message_id,message_seq,channel_id,channel_type,is_deleted,version) VALUES (?,?,?,?,?,?)",
				id, 1, fake, common.ChannelTypePerson.Uint8(), 1, 1).Exec()
			assert.NoError(t, e)
		}},
		{9403, func(id string) {
			_, e := ctx.DB().InsertBySql("INSERT INTO message_user_extra (uid,message_id,message_seq,channel_id,channel_type,message_is_deleted) VALUES (?,?,?,?,?,?)",
				testutil.UID, id, 1, fake, common.ChannelTypePerson.Uint8(), 1).Exec()
			assert.NoError(t, e)
		}},
	}
	for _, tc := range cases {
		idStr := fmt.Sprintf("%d", tc.msgID)
		seedCardMessage(t, ctx, tc.msgID, cardActionBotUID, fake, common.ChannelTypePerson.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
		seedRevisionFrame(t, store, idStr, fake, common.ChannelTypePerson.Uint8(), 1, "content-should-be-hidden", "approve_btn")
		tc.seedWD(idStr)
		w := get(idStr)
		assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
		assert.True(t, emptyRevisions(w), "回收态卡片(%s)应返回空修订", idStr)
		assert.NotContains(t, w.Body.String(), "content-should-be-hidden", "不得泄漏历史内容")
	}
}

// TestCardRevisionsCanonicalVisibility 验证修订查询与单条读同口径的第四类可见性
// (PR#549 review B1)：被 visibles 白名单排除 / 被用户清理偏移截断的群成员，虽过
// 成员+生命周期门禁，仍看不到卡片本身 —— 其修订历史也必须返回空（不泄漏内容/存在
// 性），与 card/action 共用 cardCanonicalVisibleToViewer。
func TestCardRevisionsCanonicalVisibility(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()
	resetCardActionState(t, ctx)
	store := cardrevision.NewStore(ctx.DB())
	seedCardBot(t, ctx, cardActionBotUID)

	// 正常群 + testutil.UID 为成员 —— 让唯一区分点落在 visibles / offset。
	_, err := ctx.DB().InsertBySql("INSERT INTO `group`(group_no, name, status, space_id) VALUES(?, 'grv', ?, '')", "g_rev_vis", group.GroupStatusNormal).Exec()
	assert.NoError(t, err)
	_, err = ctx.DB().InsertBySql("insert into group_member(group_no,uid) values(?,?)", "g_rev_vis", testutil.UID).Exec()
	assert.NoError(t, err)

	get := func(msgID string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		q := fmt.Sprintf("message_id=%s&channel_id=g_rev_vis&channel_type=%d", msgID, common.ChannelTypeGroup.Uint8())
		req, _ := http.NewRequest("GET", "/v1/message/card/revisions?"+q, nil)
		req.Header.Set("token", testutil.Token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}
	emptyRevisions := func(w *httptest.ResponseRecorder) bool {
		var resp struct {
			Revisions []map[string]interface{} `json:"revisions"`
		}
		return json.Unmarshal(w.Body.Bytes(), &resp) == nil && len(resp.Revisions) == 0
	}
	withVisibles := func(uids ...string) []byte {
		var env map[string]interface{}
		assert.NoError(t, json.Unmarshal(cardV2EnvelopeJSON(t, "approve_btn"), &env))
		vis := make([]interface{}, 0, len(uids))
		for _, u := range uids {
			vis = append(vis, u)
		}
		env["visibles"] = vis
		b, _ := json.Marshal(env)
		return b
	}

	// ① visibles 只含别人 → 成员看不到卡片，其历史返回空（修订行仍在）。
	seedCardMessage(t, ctx, 9210, cardActionBotUID, "g_rev_vis", common.ChannelTypeGroup.Uint8(), withVisibles("someone_else"))
	seedRevisionFrame(t, store, "9210", "g_rev_vis", common.ChannelTypeGroup.Uint8(), 1, "visibles-hidden-content", "approve_btn")
	w := get("9210")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, emptyRevisions(w), "visibles 排除成员应得空修订")
	assert.NotContains(t, w.Body.String(), "visibles-hidden-content", "不得泄漏被 visibles 排除卡片的历史内容")

	// ② 对照：visibles 含操作者 → 正常返回历史。
	seedCardMessage(t, ctx, 9211, cardActionBotUID, "g_rev_vis", common.ChannelTypeGroup.Uint8(), withVisibles(testutil.UID))
	seedRevisionFrame(t, store, "9211", "g_rev_vis", common.ChannelTypeGroup.Uint8(), 1, "visible-content", "approve_btn")
	w = get("9211")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.False(t, emptyRevisions(w), "visibles 含操作者应返回历史")

	// ③ 用户清理偏移截断：offset.message_seq(=5) ≥ 卡片 seq(=1) → 成员已清理该消息，
	//    历史返回空。用 newChannelOffsetDB.insertOrUpdate 走分表逻辑（勿裸 INSERT 撞错分片）。
	seedCardMessage(t, ctx, 9212, cardActionBotUID, "g_rev_vis", common.ChannelTypeGroup.Uint8(), cardV2EnvelopeJSON(t, "approve_btn"))
	seedRevisionFrame(t, store, "9212", "g_rev_vis", common.ChannelTypeGroup.Uint8(), 1, "offset-truncated-content", "approve_btn")
	err = newChannelOffsetDB(ctx).insertOrUpdate(&channelOffsetModel{
		UID: testutil.UID, ChannelID: "g_rev_vis", ChannelType: common.ChannelTypeGroup.Uint8(), MessageSeq: 5,
	})
	assert.NoError(t, err)
	w = get("9212")
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.True(t, emptyRevisions(w), "偏移截断成员应得空修订")
	assert.NotContains(t, w.Body.String(), "offset-truncated-content", "不得泄漏偏移截断卡片的历史内容")
}
