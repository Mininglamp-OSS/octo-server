//go:build integration

package message

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/go-redis/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		_ = os.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	}
	os.Exit(m.Run())
}

func resetReactionUIDRateLimit(t *testing.T, ctx *config.Context) {
	t.Helper()
	rds := redis.NewClient(&redis.Options{
		Addr:     ctx.GetConfig().DB.RedisAddr,
		Password: ctx.GetConfig().DB.RedisPass,
	})
	defer rds.Close()
	if keys, err := rds.Keys("ratelimit:uid:*").Result(); err == nil && len(keys) > 0 {
		_ = rds.Del(keys...).Err()
	}
}

func addReactionTestGroup(t *testing.T, ctx *config.Context) string {
	t.Helper()
	groupNo := newTestGroupNo()
	groupDB := group.NewDB(ctx)
	require.NoError(t, groupDB.Insert(&group.Model{
		GroupNo: groupNo,
		Name:    "reaction-test",
		Creator: testutil.UID,
		Status:  group.GroupStatusNormal,
		Version: 1,
	}))
	require.NoError(t, groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo,
		UID:     testutil.UID,
		Role:    group.MemberRoleCreator,
		Status:  int(common.GroupMemberStatusNormal),
		Version: 1,
	}))
	return groupNo
}

func postReaction(t *testing.T, s *server.Server, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/v1/reactions", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	return w
}

func countReactionRows(t *testing.T, ctx *config.Context, messageID int64) int {
	t.Helper()
	var count int
	err := ctx.DB().Select("count(*)").From("reaction_users").
		Where("message_id=?", strconv.FormatInt(messageID, 10)).
		LoadOne(&count)
	require.NoError(t, err)
	return count
}

func queryReactionRow(t *testing.T, ctx *config.Context, messageID int64, channelID string, channelType uint8, emoji string) *reactionModel {
	t.Helper()
	var model *reactionModel
	_, err := ctx.DB().Select("*").From("reaction_users").
		Where("channel_id=? and channel_type=? and message_id=? and uid=? and emoji=?",
			channelID, channelType, strconv.FormatInt(messageID, 10), testutil.UID, emoji).
		Load(&model)
	require.NoError(t, err)
	require.NotNil(t, model)
	return model
}

// decodeToggleResp 解析写接口返回的最终态 {emoji, seq, is_deleted}（c.Response 直出 data）。
func decodeToggleResp(t *testing.T, w *httptest.ResponseRecorder) reactionToggleResp {
	t.Helper()
	var resp reactionToggleResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), w.Body.String())
	return resp
}

func TestReactionAcceptsVisibleMessageAndToggles(t *testing.T) {
	s, ctx, groupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	const mid int64 = 910000
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	body := map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   groupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"emoji":        "👍",
	}
	w := postReaction(t, s, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 0, decodeToggleResp(t, w).IsDeleted, "首次点亮返回 is_deleted=0")
	model := queryReactionRow(t, ctx, mid, groupNo, common.ChannelTypeGroup.Uint8(), "👍")
	assert.Equal(t, "👍", model.Emoji)
	assert.Equal(t, 0, model.IsDeleted)
	assert.Equal(t, 1, countReactionRows(t, ctx, mid))

	w = postReaction(t, s, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, decodeToggleResp(t, w).IsDeleted, "再点同 emoji 取消，返回 is_deleted=1")
	model = queryReactionRow(t, ctx, mid, groupNo, common.ChannelTypeGroup.Uint8(), "👍")
	assert.Equal(t, 1, model.IsDeleted)
	assert.Equal(t, 1, countReactionRows(t, ctx, mid), "toggle 复用同一行，不新增")
}

func TestReactionMultipleEmojisIndependent(t *testing.T) {
	s, ctx, groupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	const mid int64 = 910004
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)
	ct := common.ChannelTypeGroup.Uint8()
	base := map[string]interface{}{"message_id": strconv.FormatInt(mid, 10), "channel_id": groupNo, "channel_type": ct}

	post := func(emoji string) *httptest.ResponseRecorder {
		b := map[string]interface{}{"emoji": emoji}
		for k, v := range base {
			b[k] = v
		}
		return postReaction(t, s, b)
	}

	// 点 👍 → 一行
	require.Equal(t, http.StatusOK, post("👍").Code)
	assert.Equal(t, 1, countReactionRows(t, ctx, mid))

	// 点 ❤️（不同 emoji）→ 追加独立第二行，两行均 is_deleted=0
	require.Equal(t, http.StatusOK, post("❤️").Code)
	assert.Equal(t, 2, countReactionRows(t, ctx, mid))
	assert.Equal(t, 0, queryReactionRow(t, ctx, mid, groupNo, ct, "👍").IsDeleted)
	assert.Equal(t, 0, queryReactionRow(t, ctx, mid, groupNo, ct, "❤️").IsDeleted)

	// 再点 👍 → 仅 👍 行 toggle 取消，❤️ 不受影响
	require.Equal(t, http.StatusOK, post("👍").Code)
	assert.Equal(t, 1, queryReactionRow(t, ctx, mid, groupNo, ct, "👍").IsDeleted, "👍 独立取消")
	assert.Equal(t, 0, queryReactionRow(t, ctx, mid, groupNo, ct, "❤️").IsDeleted, "❤️ 不受影响")
	assert.Equal(t, 2, countReactionRows(t, ctx, mid), "仍是两行，独立 toggle 不新增")
}

func TestReactionSameEmojiUpsertNoDuplicate(t *testing.T) {
	s, ctx, groupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	const mid int64 = 910005
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)
	body := map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   groupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"emoji":        "👍",
	}

	// 同 (uid, message, channel, emoji) 连续多次 toggle：唯一约束 + 原子 upsert 保证始终 ≤1 行。
	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusOK, postReaction(t, s, body).Code)
	}
	assert.Equal(t, 1, countReactionRows(t, ctx, mid), "同 emoji 反复 toggle 不产生重复行")
}

// ---------- 鉴权 / 可见性闭环（外部 review F1/F2/F3）----------

func postReactionSync(t *testing.T, s *server.Server, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/v1/reaction/sync", bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// F2：已删除子区的历史消息不可被 reaction（与 getThreadMessage 的 deleted fail-closed 对齐）。
func TestReactionRejectsDeletedThreadWrite(t *testing.T) {
	s, ctx, groupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)
	shortID := newTestShortID()
	insertTestThread(t, ctx, groupNo, shortID)
	channelID := thread.BuildChannelID(groupNo, shortID)
	const mid int64 = 910100
	insertGroupMessage(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), mid)

	_, err := ctx.DB().UpdateBySql("UPDATE thread SET status=? WHERE group_no=? AND short_id=?",
		thread.ThreadStatusDeleted, groupNo, shortID).Exec()
	require.NoError(t, err)

	w := postReaction(t, s, map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   channelID,
		"channel_type": common.ChannelTypeCommunityTopic.Uint8(),
		"emoji":        "👍",
	})
	assert.NotEqual(t, http.StatusOK, w.Code, "deleted thread 不可 reaction")
	assert.Equal(t, 0, countReactionRows(t, ctx, mid), "deleted-thread reaction 不落库")
}

// F1：syncReaction 对非父群成员的子区拒绝（旧代码完全绕过子区鉴权）。
func TestSyncReactionRejectsNonMemberTopic(t *testing.T) {
	s, ctx, _ := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	// 另建一个 testutil.UID 不是成员的父群 + 活跃子区。
	otherGroup := newTestGroupNo()
	require.NoError(t, group.NewDB(ctx).Insert(&group.Model{
		GroupNo: otherGroup, Name: "no-member", Creator: "someone-else",
		Status: group.GroupStatusNormal, Version: 1,
	}))
	shortID := newTestShortID()
	insertTestThread(t, ctx, otherGroup, shortID)
	channelID := thread.BuildChannelID(otherGroup, shortID)

	w := postReactionSync(t, s, map[string]interface{}{
		"channel_id":   channelID,
		"channel_type": common.ChannelTypeCommunityTopic.Uint8(),
		"seq":          0,
	})
	// 明确断言 4xx（400 request_invalid / 403 channel_access_denied，均由 i18n 门面固定 400）：
	// 若鉴权因 DB 抖动 500，NotEqual(200) 会静默通过，掩盖真实 bug。
	assert.GreaterOrEqual(t, w.Code, 400, "非父群成员必须被拒绝: %d %s", w.Code, w.Body.String())
	assert.Less(t, w.Code, 500, "拒绝不能是 5xx（否则可能是 DB 错误伪通过）: %d %s", w.Code, w.Body.String())
}

// F1：syncReaction 拒绝未知 channel_type（旧代码无 else，直接落到查询）。
func TestSyncReactionRejectsUnknownChannelType(t *testing.T) {
	s, ctx, _ := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	w := postReactionSync(t, s, map[string]interface{}{
		"channel_id":   "whatever",
		"channel_type": 99,
		"seq":          0,
	})
	assert.GreaterOrEqual(t, w.Code, 400, "未知 channel_type 必须被拒绝: %d %s", w.Code, w.Body.String())
	assert.Less(t, w.Code, 500, "拒绝不能是 5xx: %d %s", w.Code, w.Body.String())
}

// ---------- 跨 Space DM 隔离（reviewer blocking: DM reaction 绕过 Space 隔离）----------

const reactionPeerUID = "20001"

func seedReactionFriend(t *testing.T, ctx *config.Context, uid, toUID string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO friend (uid, to_uid, is_deleted) VALUES (?,?,0)", uid, toUID).Exec()
	require.NoError(t, err)
}

// insertPersonMessageWithSpace 在 DM 物理频道(fakeChannelID)插入一条带 space_id 的纯文本消息。
func insertPersonMessageWithSpace(t *testing.T, ctx *config.Context, fakeChannelID string, mid int64, spaceID string) {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{"type": 1, "content": "dm", "space_id": spaceID})
	require.NoError(t, err)
	require.NoError(t, NewDB(ctx).insertMessage(&messageModel{
		MessageID:   mid,
		MessageSeq:  uint32(mid),
		ClientMsgNo: "cli-dm-" + strconv.FormatInt(mid, 10),
		FromUID:     testutil.UID,
		ChannelID:   fakeChannelID,
		ChannelType: common.ChannelTypePerson.Uint8(),
		Payload:     payload,
	}))
}

func postReactionWithSpace(t *testing.T, s *server.Server, path, spaceID string, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, path+"?space_id="+spaceID, bytes.NewReader(raw))
	require.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	req.Header.Set("Content-Type", "application/json")
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// setupCrossSpaceDM: UID 是 spaceA(默认,较早)与 spaceB 的成员，与 peer 是好友，
// DM 频道里有一条 space_id=spaceB 的文本消息。返回 (fakeChannelID, mid, spaceA, spaceB)。
func setupCrossSpaceDM(t *testing.T) (*server.Server, *config.Context, string, int64, string, string) {
	s, ctx := testutil.NewTestServer()
	resetReactionUIDRateLimit(t, ctx)
	spaceA, spaceB := "spcA"+strconv.FormatInt(nextShortID.Add(1), 10), "spcB"+strconv.FormatInt(nextShortID.Add(1), 10)
	seedSpaceMemberRepro(t, ctx, spaceA, testutil.UID, "2020-01-01 00:00:00")
	seedSpaceMemberRepro(t, ctx, spaceB, testutil.UID, "2020-06-01 00:00:00")
	seedReactionFriend(t, ctx, testutil.UID, reactionPeerUID)
	fakeChannelID := common.GetFakeChannelIDWith(testutil.UID, reactionPeerUID)
	const mid int64 = 920001
	insertPersonMessageWithSpace(t, ctx, fakeChannelID, mid, spaceB)
	return s, ctx, fakeChannelID, mid, spaceA, spaceB
}

func TestReactionRejectsCrossSpaceDMWrite(t *testing.T) {
	s, ctx, _, mid, spaceA, spaceB := setupCrossSpaceDM(t)

	body := map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   reactionPeerUID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"emoji":        "👍",
	}
	// 用非归属 Space(默认 spaceA)写 spaceB 的 DM 消息 → 拒绝，不落库。
	w := postReactionWithSpace(t, s, "/v1/reactions", spaceA, body)
	assert.NotEqual(t, http.StatusOK, w.Code, "cross-Space DM reaction write must be rejected")
	assert.Equal(t, 0, countReactionRows(t, ctx, mid), "rejected cross-Space reaction must not persist")

	// 用归属 Space(spaceB)写同一消息 → 允许。
	w = postReactionWithSpace(t, s, "/v1/reactions", spaceB, body)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, 1, countReactionRows(t, ctx, mid), "in-Space reaction must persist")
}

func TestSyncReactionHidesCrossSpaceDMReaction(t *testing.T) {
	s, ctx, _, mid, spaceA, spaceB := setupCrossSpaceDM(t)

	// 先在归属 Space(spaceB)成功点一个 reaction。
	body := map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   reactionPeerUID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"emoji":        "👍",
	}
	require.Equal(t, http.StatusOK, postReactionWithSpace(t, s, "/v1/reactions", spaceB, body).Code)
	require.Equal(t, 1, countReactionRows(t, ctx, mid))

	syncBody := map[string]interface{}{"channel_id": reactionPeerUID, "channel_type": common.ChannelTypePerson.Uint8(), "seq": 0}

	// 归属 Space(spaceB)sync → 能看到该 reaction。
	w := postReactionWithSpace(t, s, "/v1/reaction/sync", spaceB, syncBody)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var inSpace []reactionResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &inSpace), w.Body.String())
	found := false
	for _, r := range inSpace {
		if r.MessageID == strconv.FormatInt(mid, 10) {
			found = true
		}
	}
	assert.True(t, found, "in-Space sync should include the reaction")

	// 非归属 Space(默认 spaceA)sync → 该 reaction 被 Space 过滤掉。
	w = postReactionWithSpace(t, s, "/v1/reaction/sync", spaceA, syncBody)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var crossSpace []reactionResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &crossSpace), w.Body.String())
	for _, r := range crossSpace {
		assert.NotEqual(t, strconv.FormatInt(mid, 10), r.MessageID, "cross-Space DM reaction must not leak in sync")
	}
}

// F3+P2#5：读侧也按 payload.type 过滤——手动落一条非文本消息对应的 reaction 行
// （模拟历史存量或其它写路径绕过），syncReaction 必须剔除，不下发。
func TestSyncReactionHidesNonTextMessageReaction(t *testing.T) {
	s, ctx, groupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	// 插入一条图片消息（type=2）。
	const mid int64 = 910300
	msgDB := NewDB(ctx)
	payload, err := json.Marshal(map[string]interface{}{"type": int(common.Image), "url": "http://example.com/x.png"})
	require.NoError(t, err)
	require.NoError(t, msgDB.insertMessage(&messageModel{
		MessageID:   mid,
		MessageSeq:  uint32(mid),
		ClientMsgNo: "cli-img-910300",
		FromUID:     testutil.UID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     payload,
	}))
	// 直接 DB 写入 reaction 行，绕过写路径的类型门（模拟历史脏数据）。
	seq, err := ctx.GenSeq("messageReactionSeq:" + groupNo)
	require.NoError(t, err)
	_, err = New(ctx).messageReactionDB.toggleReaction(&reactionModel{
		ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(), UID: testutil.UID,
		Name: "u", MessageID: strconv.FormatInt(mid, 10), Emoji: "👍", Seq: seq,
	})
	require.NoError(t, err)
	require.Equal(t, 1, countReactionRows(t, ctx, mid), "sanity: reaction 已落库")

	// sync：读侧类型门应过滤掉该 reaction。
	w := postReactionSync(t, s, map[string]interface{}{
		"channel_id":   groupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"seq":          0,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got []reactionResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), w.Body.String())
	for _, r := range got {
		assert.NotEqual(t, strconv.FormatInt(mid, 10), r.MessageID, "非文本消息的 reaction 不应下发")
	}
}

func TestSyncReactionHidesRevokedMessageReaction(t *testing.T) {
	s, ctx, groupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	const mid int64 = 910200
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	// 消息可见时先点 reaction（写路径通过）。
	require.Equal(t, http.StatusOK, postReaction(t, s, map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   groupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"emoji":        "👍",
	}).Code)
	require.Equal(t, 1, countReactionRows(t, ctx, mid))

	// 撤回该消息。reaction 行不会被清理，但 sync 必须按可见性剔除。
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	require.NoError(t, New(ctx).messageExtraDB.insertTx(&messageExtraModel{
		MessageID: strconv.FormatInt(mid, 10), MessageSeq: uint32(mid),
		ChannelID: groupNo, ChannelType: common.ChannelTypeGroup.Uint8(),
		Revoke: 1, Revoker: testutil.UID, Version: 1,
	}, tx))
	require.NoError(t, tx.Commit())

	w := postReactionSync(t, s, map[string]interface{}{
		"channel_id":   groupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"seq":          0,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got []reactionResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), w.Body.String())
	for _, r := range got {
		assert.NotEqual(t, strconv.FormatInt(mid, 10), r.MessageID, "撤回消息的 reaction 不应下发")
	}
}

func TestReactionRejectsMismatchedChannelWrite(t *testing.T) {
	s, ctx, realGroupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)
	wrongGroupNo := addReactionTestGroup(t, ctx)

	const mid int64 = 910001
	insertGroupMessage(t, ctx, realGroupNo, common.ChannelTypeGroup.Uint8(), mid)

	w := postReaction(t, s, map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   wrongGroupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"emoji":        "👍",
	})

	assert.NotEqual(t, http.StatusOK, w.Code, "wrong-channel reaction write must be rejected")
	assert.Equal(t, 0, countReactionRows(t, ctx, mid), "rejected reaction must not leave a reaction_users row")
}

func TestReactionRejectsNonTextMessage(t *testing.T) {
	s, ctx, groupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	// 插入一条图片消息（type=common.Image=2）。消息存在且对成员可见，仅类型不支持。
	const mid int64 = 910003
	msgDB := NewDB(ctx)
	payload, err := json.Marshal(map[string]interface{}{"type": int(common.Image), "url": "http://example.com/a.png"})
	require.NoError(t, err)
	require.NoError(t, msgDB.insertMessage(&messageModel{
		MessageID:   mid,
		MessageSeq:  uint32(mid),
		ClientMsgNo: "cli-img-910003",
		FromUID:     testutil.UID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     payload,
	}))

	w := postReaction(t, s, map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   groupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"emoji":        "👍",
	})

	assert.NotEqual(t, http.StatusOK, w.Code, "non-text messages must not accept reactions")
	assert.Equal(t, 0, countReactionRows(t, ctx, mid), "rejected non-text reaction must not be persisted")
}

func TestReactionRejectsRevokedMessageWrite(t *testing.T) {
	s, ctx, groupNo := setupGroupTestData(t)
	resetReactionUIDRateLimit(t, ctx)

	const mid int64 = 910002
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)
	tx, err := ctx.DB().Begin()
	require.NoError(t, err)
	require.NoError(t, New(ctx).messageExtraDB.insertTx(&messageExtraModel{
		MessageID:   strconv.FormatInt(mid, 10),
		MessageSeq:  uint32(mid),
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Revoke:      1,
		Revoker:     testutil.UID,
		Version:     1,
	}, tx))
	require.NoError(t, tx.Commit())

	w := postReaction(t, s, map[string]interface{}{
		"message_id":   strconv.FormatInt(mid, 10),
		"channel_id":   groupNo,
		"channel_type": common.ChannelTypeGroup.Uint8(),
		"emoji":        "👍",
	})

	assert.NotEqual(t, http.StatusOK, w.Code, "revoked messages must not accept reactions")
	assert.Equal(t, 0, countReactionRows(t, ctx, mid), "revoked-message reaction must not be persisted")
}
