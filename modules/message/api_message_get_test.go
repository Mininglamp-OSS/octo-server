package message

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"database/sql"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/stretchr/testify/assert"
)

// 32 hex chars，满足 thread.IsValidGroupNo
func newTestGroupNo() string {
	return strings.ReplaceAll(util.GenerUUID(), "-", "")
}

// setupGroupTestData 准备测试用户和测试群（当前登录 UID 是 group creator）。
//
// 在 NewTestServer 之前 t.Setenv("DM_THREAD_ON","true")，确保：
//   - thread GET 路由（受 message 模块 threadFeatureEnabled() 检查）注册
//   - thread 模块（modules/thread/1module.go init() 内同源检查）注册 + 跑迁移
//
// 否则在干净 CI 环境跑 `go test ./modules/message/...` 时 thread case 会找不到
// 路由或缺 thread 表。t.Setenv 只在本测试生效，不污染其它包。
func setupGroupTestData(t *testing.T) (*server.Server, *config.Context, string) {
	t.Setenv("DM_THREAD_ON", "true")
	s, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	userDB := user.NewDB(ctx)
	err = userDB.Insert(&user.Model{UID: testutil.UID, Name: "tester", ShortNo: "10000"})
	assert.NoError(t, err)

	groupDB := group.NewDB(ctx)
	groupNo := newTestGroupNo()
	err = groupDB.Insert(&group.Model{GroupNo: groupNo, Name: "g", Creator: testutil.UID, Status: 1, Version: 1})
	assert.NoError(t, err)
	err = groupDB.InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: testutil.UID, Role: group.MemberRoleCreator,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	})
	assert.NoError(t, err)
	return s, ctx, groupNo
}

func insertGroupMessage(t *testing.T, ctx *config.Context, channelID string, channelType uint8, messageID int64) {
	msgDB := NewDB(ctx)
	payload, _ := json.Marshal(map[string]interface{}{"type": 1, "content": "hello"})
	err := msgDB.insertMessage(&messageModel{
		MessageID:   messageID,
		MessageSeq:  uint32(messageID),
		ClientMsgNo: fmt.Sprintf("cli-%d", messageID),
		FromUID:     testutil.UID,
		ChannelID:   channelID,
		ChannelType: channelType,
		Payload:     payload,
	})
	assert.NoError(t, err)
}

func doGet(t *testing.T, s *server.Server, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, err := http.NewRequest("GET", path, nil)
	assert.NoError(t, err)
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)
	return w
}

// ---------- Group ----------

func TestGetGroupMessage_Success(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 1001
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp MsgSyncResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, mid, resp.MessageID)
	assert.Equal(t, strconv.FormatInt(mid, 10), resp.MessageIDStr)
	assert.Equal(t, groupNo, resp.ChannelID)
	assert.Equal(t, common.ChannelTypeGroup.Uint8(), resp.ChannelType)
	assert.Equal(t, testutil.UID, resp.FromUID)
}

func TestGetGroupMessage_NotFound(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, groupNo := setupGroupTestData(t)
	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/9999", groupNo))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetGroupMessage_Revoked(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 2001
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	// 写一条 message_extra 标记 revoke=1
	msg := New(ctx)
	tx, err := ctx.DB().Begin()
	assert.NoError(t, err)
	err = msg.messageExtraDB.insertTx(&messageExtraModel{
		MessageID:   strconv.FormatInt(mid, 10),
		MessageSeq:  uint32(mid),
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Revoke:      1,
		Revoker:     testutil.UID,
		Version:     1,
	}, tx)
	assert.NoError(t, err)
	assert.NoError(t, tx.Commit())

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetGroupMessage_UserDeleted(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 3001
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	msg := New(ctx)
	tx, err := ctx.DB().Begin()
	assert.NoError(t, err)
	err = msg.messageUserExtraDB.insertOrUpdateDeletedTx(&messageUserExtraModel{
		UID:              testutil.UID,
		MessageID:        strconv.FormatInt(mid, 10),
		MessageSeq:       uint32(mid),
		ChannelID:        groupNo,
		ChannelType:      common.ChannelTypeGroup.Uint8(),
		MessageIsDeleted: 1,
	}, tx)
	assert.NoError(t, err)
	assert.NoError(t, tx.Commit())

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetGroupMessage_NotMember(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, _ := setupGroupTestData(t)
	otherGroup := newTestGroupNo()
	groupDB := group.NewDB(ctx)
	err := groupDB.Insert(&group.Model{GroupNo: otherGroup, Name: "g2", Creator: "user_other", Status: 1, Version: 1})
	assert.NoError(t, err)

	const mid int64 = 4001
	insertGroupMessage(t, ctx, otherGroup, common.ChannelTypeGroup.Uint8(), mid)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", otherGroup, mid))
	// 非成员一律 404，避免泄露"群存在"信号。
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetGroupMessage_InvalidGroupNo(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _ := setupGroupTestData(t)
	w := doGet(t, s, "/v1/groups/badformat/messages/1")
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

func TestGetGroupMessage_InvalidMessageID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, groupNo := setupGroupTestData(t)
	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/notanumber", groupNo))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// payload > hardParsePayloadLimit (1MB) 时 from() 会把 payload 整个换成
// placeholder，丢掉 visibles 字段，导致原先依赖"from() 后查 IsDeleted"的
// 兜底失效——visibles=[other_uid] 的大消息会被 200 返回（仅元数据不含正文，
// 但 message_id/from_uid/timestamp 等仍属隐私泄露）。
//
// 修复后必须在 from() 前直接基于原始 msgModel.Payload 做白名单判定。
func TestGetGroupMessage_VisiblesBypass_LargePayload_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7501
	msgDB := NewDB(ctx)
	bigContent := strings.Repeat("a", 1024*1024+1024) // > 1MB
	payload, _ := json.Marshal(map[string]interface{}{
		"type":     1,
		"content":  bigContent,
		"visibles": []string{"someone_else"},
	})
	assert.Greater(t, len(payload), hardParsePayloadLimit, "payload 必须超过 1MB 触发 placeholder 路径")
	err := msgDB.insertMessage(&messageModel{
		MessageID:   mid,
		MessageSeq:  uint32(mid),
		ClientMsgNo: "vis-large",
		FromUID:     testutil.UID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     payload,
	})
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// 防御 visibles 白名单绕过：批量同步路径靠客户端过滤，单条直查不能依赖客户端，
// 必须服务端 404。
func TestGetGroupMessage_VisiblesBypass_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7001
	msgDB := NewDB(ctx)
	payload, _ := json.Marshal(map[string]interface{}{
		"type":     1,
		"content":  "secret",
		"visibles": []string{"someone_else"},
	})
	err := msgDB.insertMessage(&messageModel{
		MessageID:   mid,
		MessageSeq:  uint32(mid),
		ClientMsgNo: "vis-bypass",
		FromUID:     testutil.UID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Payload:     payload,
	})
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// 黑名单成员（group_member.status=GroupMemberStatusBlacklist）的 is_deleted 仍为 0，
// 单查 ExistMember 不够。本接口绕过 WuKongIM 直接读本地分表，
// 必须 fail closed，不能让被拉黑的用户按 ID 把消息读出来。
func TestGetGroupMessage_BlacklistMember_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7601
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	// 把当前登录用户的成员状态改成 Blacklist (=2)。is_deleted 保持 0。
	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		common.GroupMemberStatusBlacklist, groupNo, testutil.UID).Exec()
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetThreadMessage_BlacklistMember_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	shortID := newTestShortID()
	insertTestThread(t, ctx, groupNo, shortID)
	channelID := thread.BuildChannelID(groupNo, shortID)
	const mid int64 = 6201
	insertGroupMessage(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), mid)

	_, err := ctx.DB().UpdateBySql(
		"UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		common.GroupMemberStatusBlacklist, groupNo, testutil.UID).Exec()
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// group.status 除 Normal/Disband 外还存在 Disabled (=0)。
// requireGroupMember 必须按 status=Normal 白名单拦截，否则未来新状态默认放行。
func TestGetGroupMessage_DisabledGroup_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7701
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	_, err := ctx.DB().UpdateBySql("UPDATE `group` SET status=? WHERE group_no=?",
		group.GroupStatusDisabled, groupNo).Exec()
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// queryMessageByID 必须用字符串绑定 message_id（VARCHAR(20) 列）才能命中
// UNIQUE 索引；如果用 int64 会触发 MySQL 隐式类型转换 → EXPLAIN type=ALL 全表扫。
// 用 sql.Conn 直接跑 EXPLAIN 而非 dbr.Load —— EXPLAIN 输出列含 SQL 关键字 `key`，
// dbr 反射映射不便。命中索引时 type 取值集合：const / ref / eq_ref / NULL（const
// 优化短路）；type='ALL' 即回归。
func TestQueryMessageByID_UsesIndex(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	t.Setenv("DM_THREAD_ON", "true")
	_, ctx := testutil.NewTestServer()
	err := testutil.CleanAllTables(ctx)
	assert.NoError(t, err)

	// 插一条消息保证表非空，否则优化器永远 const 短路看不出区别
	const mid int64 = 8001
	channelID := newTestGroupNo()
	payload, _ := json.Marshal(map[string]interface{}{"type": 1, "content": "x"})
	err = NewDB(ctx).insertMessage(&messageModel{
		MessageID: mid, MessageSeq: uint32(mid), ClientMsgNo: "idx-test",
		FromUID: testutil.UID, ChannelID: channelID,
		ChannelType: common.ChannelTypeGroup.Uint8(), Payload: payload,
	})
	assert.NoError(t, err)

	rows, err := ctx.DB().Query(
		"EXPLAIN SELECT * FROM message WHERE channel_id=? AND channel_type=? AND message_id=? AND is_deleted=0",
		channelID, common.ChannelTypeGroup.Uint8(), strconv.FormatInt(mid, 10))
	assert.NoError(t, err)
	defer rows.Close()
	cols, err := rows.Columns()
	assert.NoError(t, err)
	typeIdx := -1
	for i, c := range cols {
		if c == "type" {
			typeIdx = i
		}
	}
	assert.GreaterOrEqual(t, typeIdx, 0, "EXPLAIN 输出应包含 type 列")
	assert.True(t, rows.Next(), "EXPLAIN 应返回至少一行")
	vals := make([]sql.NullString, len(cols))
	scanArgs := make([]interface{}, len(cols))
	for i := range vals {
		scanArgs[i] = &vals[i]
	}
	assert.NoError(t, rows.Scan(scanArgs...))
	planType := vals[typeIdx].String
	assert.NotEqual(t, "ALL", planType, "string-bound query 必须命中索引，不能 type=ALL，否则上线会全表扫")
}

// 群解散后 group_member 记录不会清理，仅 group.status=GroupStatusDisband。
// 单查 ExistMember 不够，必须再查群状态，否则解散群历史能被本地直查接口读出。
func TestGetGroupMessage_DisbandedGroup_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7201
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	// 在拿到合法 200 之后再解散群，模拟"消息发出后群被解散"的场景。
	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusOK, w.Code, "前置条件：解散前应能查到")

	_, err := ctx.DB().UpdateBySql("UPDATE `group` SET status=? WHERE group_no=?", group.GroupStatusDisband, groupNo).Exec()
	assert.NoError(t, err)

	w = doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// message 表自身 is_deleted=1（双向删除写到这里）也要返回 404。
func TestGetGroupMessage_DBLevelDeleted_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7101
	msgDB := NewDB(ctx)
	payload, _ := json.Marshal(map[string]interface{}{"type": 1, "content": "x"})
	err := msgDB.insertMessage(&messageModel{
		MessageID:   mid,
		MessageSeq:  uint32(mid),
		ClientMsgNo: "db-deleted",
		FromUID:     testutil.UID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		IsDeleted:   1,
		Payload:     payload,
	})
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// 用户级历史清理偏移：/v1/message/offset 写入 channel_offset 后，
// message_seq <= offset 的消息单条直查也要 404，否则等于绕过用户清理。
func TestGetGroupMessage_UserChannelOffset_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7301
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	// 偏移设置成 mid+1 之类的更大值即可让该消息被截断。
	msg := New(ctx)
	err := msg.channelOffsetDB.insertOrUpdate(&channelOffsetModel{
		UID:         testutil.UID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		MessageSeq:  uint32(mid) + 100,
	})
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// header 字段在响应里要还原（与 syncChannelMessage 一致）。
func TestGetGroupMessage_HeaderRoundtrip(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7401
	msgDB := NewDB(ctx)
	payload, _ := json.Marshal(map[string]interface{}{"type": 1, "content": "h"})
	err := msgDB.insertMessage(&messageModel{
		MessageID:   mid,
		MessageSeq:  uint32(mid),
		ClientMsgNo: "hdr",
		FromUID:     testutil.UID,
		ChannelID:   groupNo,
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Header:      `{"no_persist":1,"red_dot":1,"sync_once":1}`,
		Payload:     payload,
	})
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp MsgSyncResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Header.NoPersist)
	assert.Equal(t, 1, resp.Header.RedDot)
	assert.Equal(t, 1, resp.Header.SyncOnce)
}

// ---------- Thread ----------

func insertTestThread(t *testing.T, ctx *config.Context, groupNo, shortID string) {
	tdb := thread.NewDB(ctx)
	err := tdb.Insert(&thread.Model{
		ShortID: shortID, GroupNo: groupNo, Name: "t1", CreatorUID: testutil.UID,
		Status: thread.ThreadStatusActive, Version: 1,
	})
	assert.NoError(t, err)
}

// 每个 thread 测试用独立 short_id，避免因为 testutil.CleanAllTables 的 TRUNCATE
// 时机或 t.Parallel 时撞主键。
var nextShortID atomic.Int64

func newTestShortID() string {
	return strconv.FormatInt(100000000000000+nextShortID.Add(1), 10)
}

func TestGetThreadMessage_Success(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	shortID := newTestShortID()
	insertTestThread(t, ctx, groupNo, shortID)
	channelID := thread.BuildChannelID(groupNo, shortID)
	const mid int64 = 5001
	insertGroupMessage(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), mid)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp MsgSyncResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, mid, resp.MessageID)
	assert.Equal(t, channelID, resp.ChannelID)
	assert.Equal(t, common.ChannelTypeCommunityTopic.Uint8(), resp.ChannelType)
}

// 归档子区的历史消息仍应可读：与 IM datasource、GetThread、ExistByGroupNoAndShortID
// 行为对齐——它们都只拒 ThreadStatusDeleted，归档（Archived）允许访问。
func TestGetThreadMessage_ArchivedThread_200(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	shortID := newTestShortID()
	insertTestThread(t, ctx, groupNo, shortID)
	channelID := thread.BuildChannelID(groupNo, shortID)
	const mid int64 = 6301
	insertGroupMessage(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), mid)

	_, err := ctx.DB().UpdateBySql(
		"UPDATE thread SET status=? WHERE group_no=? AND short_id=?",
		thread.ThreadStatusArchived, groupNo, shortID).Exec()
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// 已删除的子区必须 404。
func TestGetThreadMessage_DeletedThread_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	shortID := newTestShortID()
	insertTestThread(t, ctx, groupNo, shortID)
	channelID := thread.BuildChannelID(groupNo, shortID)
	const mid int64 = 6401
	insertGroupMessage(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), mid)

	_, err := ctx.DB().UpdateBySql(
		"UPDATE thread SET status=? WHERE group_no=? AND short_id=?",
		thread.ThreadStatusDeleted, groupNo, shortID).Exec()
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetThreadMessage_ThreadNotExist(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, groupNo := setupGroupTestData(t)
	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/%s/messages/1", groupNo, newTestShortID()))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetThreadMessage_MessageNotFound(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	shortID := newTestShortID()
	insertTestThread(t, ctx, groupNo, shortID)
	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/%s/messages/9999", groupNo, shortID))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// 子区也要按用户级 channel_offset 截断。
func TestGetThreadMessage_UserChannelOffset_404(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	shortID := newTestShortID()
	insertTestThread(t, ctx, groupNo, shortID)
	channelID := thread.BuildChannelID(groupNo, shortID)
	const mid int64 = 6101
	insertGroupMessage(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), mid)

	msg := New(ctx)
	err := msg.channelOffsetDB.insertOrUpdate(&channelOffsetModel{
		UID:         testutil.UID,
		ChannelID:   channelID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		MessageSeq:  uint32(mid) + 100,
	})
	assert.NoError(t, err)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/%s/messages/%d", groupNo, shortID, mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetThreadMessage_NotMember(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, _ := setupGroupTestData(t)
	otherGroup := newTestGroupNo()
	groupDB := group.NewDB(ctx)
	err := groupDB.Insert(&group.Model{GroupNo: otherGroup, Name: "g2", Creator: "user_other", Status: 1, Version: 1})
	assert.NoError(t, err)
	shortID := newTestShortID()
	insertTestThread(t, ctx, otherGroup, shortID)
	channelID := thread.BuildChannelID(otherGroup, shortID)
	const mid int64 = 6001
	insertGroupMessage(t, ctx, channelID, common.ChannelTypeCommunityTopic.Uint8(), mid)

	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/%s/messages/%d", otherGroup, shortID, mid))
	// 非父群成员一律 404。
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestGetThreadMessage_InvalidShortID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, groupNo := setupGroupTestData(t)
	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/threads/abc/messages/1", groupNo))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}
