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

// 企业微信式解散语义（产品决策 2026-06）：群解散后频道与历史保留，活跃成员仍能
// 按 ID 直查历史消息。requireGroupMember 放行 Disband（仅 Disabled 等其它非正常
// 状态 fail closed），成员资格仍由 ExistMemberActive 把关。
func TestGetGroupMessage_DisbandedGroup_StillReadable(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, groupNo := setupGroupTestData(t)
	const mid int64 = 7201
	insertGroupMessage(t, ctx, groupNo, common.ChannelTypeGroup.Uint8(), mid)

	// 解散前可查
	w := doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusOK, w.Code, "前置条件：解散前应能查到")

	_, err := ctx.DB().UpdateBySql("UPDATE `group` SET status=? WHERE group_no=?", group.GroupStatusDisband, groupNo).Exec()
	assert.NoError(t, err)

	// 解散后历史仍可读（活跃成员）
	w = doGet(t, s, fmt.Sprintf("/v1/groups/%s/messages/%d", groupNo, mid))
	assert.Equal(t, http.StatusOK, w.Code, "企业微信式解散：活跃成员解散后仍能读历史")
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

// ---------- Person (DM) ----------

const personPeerUID = "peer_user_1"

// TestGetPersonMessage_Success：DM 一方能按 (peer_uid, message_id) 取到消息。
// 消息物理落在 fakeChannelID = GetFakeChannelIDWith(loginUID, peerUID)、type=Person。
func TestGetPersonMessage_Success(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, _ := setupGroupTestData(t)
	fakeChannelID := common.GetFakeChannelIDWith(testutil.UID, personPeerUID)
	const mid int64 = 7001
	insertGroupMessage(t, ctx, fakeChannelID, common.ChannelTypePerson.Uint8(), mid)

	w := doGet(t, s, fmt.Sprintf("/v1/messages/person/%s/%d", personPeerUID, mid))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp MsgSyncResp
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, mid, resp.MessageID)
	assert.Equal(t, fakeChannelID, resp.ChannelID)
	assert.Equal(t, common.ChannelTypePerson.Uint8(), resp.ChannelType)
}

// TestGetPersonMessage_NotParticipant：非该 DM 一方查不到——调用者(testutil.UID)
// 与 peer 派生的 fakeChannelID 不同于消息实际所在的 (otherA, otherB) 频道，命中不到。
func TestGetPersonMessage_NotParticipant(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, ctx, _ := setupGroupTestData(t)
	// 消息属于 otherA<->otherB 的 DM，与登录用户无关。
	otherChannelID := common.GetFakeChannelIDWith("other_a", "other_b")
	const mid int64 = 7002
	insertGroupMessage(t, ctx, otherChannelID, common.ChannelTypePerson.Uint8(), mid)

	// 登录用户尝试用 peer=other_b 取——派生的 fakeChannelID(loginUID<->other_b) 不等于
	// otherChannelID(other_a<->other_b)，查不到 → 404。
	w := doGet(t, s, fmt.Sprintf("/v1/messages/person/%s/%d", "other_b", mid))
	assert.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// TestGetPersonMessage_SelfRejected：禁止自聊(peer == self)。
func TestGetPersonMessage_SelfRejected(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _ := setupGroupTestData(t)
	w := doGet(t, s, fmt.Sprintf("/v1/messages/person/%s/1", testutil.UID))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// TestGetPersonMessage_InvalidMessageID：message_id 非正整数 → 400。
func TestGetPersonMessage_InvalidMessageID(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	s, _, _ := setupGroupTestData(t)
	w := doGet(t, s, fmt.Sprintf("/v1/messages/person/%s/notanumber", personPeerUID))
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
}

// TestPersonSpaceAllows_Rules 直接固定 personSpaceAllows 的 5 条规则，与
// filterPersonMessagesBySpace 头注释里的口径一一对应。这条 helper 是 DM
// 单条读 (getPersonMessage) / DM 同步 (filterPersonMessagesBySpace) /
// reaction (syncReaction/addOrCancelReaction) 共用的 Space 隔离核心闸门，
// 任何一处走漏都是 issue #484 / YUJ-219-A / GH#1283 回归。helper 级 unit test，
// 不依赖 DB / migration，永远可执行；抓「谁把 DM 空间过滤逻辑改破了」的
// 单元金丝雀。
func TestPersonSpaceAllows_Rules(t *testing.T) {
	const (
		curSpace     = "space-A"
		otherSpace   = "space-B"
		defaultSpace = "space-A"
	)
	tests := []struct {
		name         string
		msgSpaceID   string
		isSystemBot  bool
		spaceID      string
		defaultSpace string
		want         bool
	}{
		// 规则 1：精确匹配 → 保留
		{"exact match", curSpace, false, curSpace, defaultSpace, true},
		// 规则 2：无标签 && 非 SystemBot && 当前=默认 Space → 保留（向前兼容）
		{"unlabeled non-bot in default space", "", false, curSpace, defaultSpace, true},
		// 规则 3：无标签 && 非 SystemBot && 当前 != 默认 Space → 丢弃
		{"unlabeled non-bot in non-default space", "", false, otherSpace, defaultSpace, false},
		// 规则 4：无标签 && SystemBot → 丢弃（老 fileHelper / u_10000 消息不跨 Space 泄露）
		{"unlabeled system bot", "", true, curSpace, defaultSpace, false},
		// 规则 5：跨 Space 标签 → 丢弃
		{"cross-space labeled", otherSpace, false, curSpace, defaultSpace, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := personSpaceAllows(tc.msgSpaceID, tc.isSystemBot, tc.spaceID, tc.defaultSpace)
			assert.Equal(t, tc.want, got,
				"personSpaceAllows(msg=%q, sysbot=%v, cur=%q, default=%q)",
				tc.msgSpaceID, tc.isSystemBot, tc.spaceID, tc.defaultSpace)
		})
	}
}

// TestPersonFakeChannelIDNotDoubleFaked 固定「Person DM 频道 ID 只 fake 一次」
// 的核心不变式。lookupChannelOffsetSeq 内部对 channelType==Person 会做
// GetFakeChannelIDWith(channelID, loginUID)，期望入参是 peer uid 而非已经
// fake 好的 fakeChannelID；如果调用方（如 respondSingleMessage 之前对 Person
// 传的是 fakeChannelID）不做短路，就会二次 fake，产生错误的 lookup key、
// 静默把 channel-level offset 归 0，channel-offset-truncated 的 DM 消息又能
// 被读到——这是 PR #708 review 抓到的第二条 blocking 的技术根因。
//
// 这个 helper 级测试固定该不变式：一旦有人把 respondSingleMessage 里对
// Person 短路 channel-offset 的分支拆掉、或改变 GetFakeChannelIDWith 的对称
// 语义，assertion 立即失败。
func TestPersonFakeChannelIDNotDoubleFaked(t *testing.T) {
	const (
		loginUID = "user_login"
		peerUID  = "user_peer"
	)
	fake1 := common.GetFakeChannelIDWith(loginUID, peerUID)
	// 二次 fake（如果调用者错误地把 fake1 传给 lookupChannelOffsetSeq、
	// 让它内部再做一次 GetFakeChannelIDWith(fake1, loginUID)）产生的 key。
	fake2 := common.GetFakeChannelIDWith(fake1, loginUID)

	// 反证：二次 fake 的 key 与真实的 fakeChannelID 不同——这是 lookupChannelOffsetSeq
	// 现在必须无条件用 channelID (而非再 fake) 的原因 (api_message_get.go
	// lookupChannelOffsetSeq)。如果这个不等式被打破，helper 的 caller 契约需要
	// 重新审视。
	assert.NotEqual(t, fake1, fake2,
		"double-faked Person channelID must differ from the real fakeChannelID; "+
			"if this becomes equal, lookupChannelOffsetSeq's caller contract needs revisiting")

	// 常规（非碰撞）情况下的对称性——GetFakeChannelIDWith 用 CRC32 排序两侧 uid，
	// 无碰撞时两个 uid 生成同一 key。多数正常场景走这条分支。
	assert.Equal(t, fake1, common.GetFakeChannelIDWith(peerUID, loginUID),
		"GetFakeChannelIDWith should be symmetric across the two DM sides "+
			"when their CRC32 hashes differ")
}

// TestPersonFakeChannelIDCollisionOrderMatters 固定 CRC32 碰撞时
// GetFakeChannelIDWith 的**参数顺序敏感性**——碰撞会走 `toUID@fromUID` fallthrough，
// 因此 (A,B) 与 (B,A) 会得到不同 key。所有 Person 相关调用者必须与 sibling
// 保持参数顺序一致 (统一为 `(peer_uid, login_uid)`，参见 api.go:1439 sync 路径)，
// 否则同一对话的读 / 写路径会为同一对 uid 生成不同 key、发生"写入的消息读不出来"
// 的静默 miss——PR #708 review round 2 P2 抓到的坑。
//
// 该测试用 review 提供的真实碰撞对 u_xgnb / u_cr7xslzj（CRC32 == 3955193408）。
// 只要 CRC32 算法或 GetFakeChannelIDWith fallthrough 逻辑没变，测试就有效；
// 如果哪天上游库改了 tie-breaking 让对称性也成立，assertion 会失败，
// 提示我们可以放宽对参数顺序的约束。
func TestPersonFakeChannelIDCollisionOrderMatters(t *testing.T) {
	const (
		uidA = "u_xgnb"
		uidB = "u_cr7xslzj"
	)
	// review 里提到 CRC32(uidA) == CRC32(uidB) == 3955193408，导致 tie。
	// 直接调 GetFakeChannelIDWith 两种顺序，如果 tie 逻辑仍是 fallthrough，
	// 两个结果不同——本测试的核心断言。
	keyAB := common.GetFakeChannelIDWith(uidA, uidB)
	keyBA := common.GetFakeChannelIDWith(uidB, uidA)
	assert.NotEqual(t, keyAB, keyBA,
		"CRC32-tied uids currently produce order-sensitive fake channel IDs "+
			"(GetFakeChannelIDWith falls through to toUID@fromUID on tie); "+
			"all Person callers must therefore align parameter order with siblings "+
			"(canonical order = (peer_uid, login_uid), see api.go:1439). "+
			"If this assertion fails, the upstream library changed its tie-breaking; "+
			"the order-alignment requirement can then be relaxed.")
}

// TestPersonDMAccessDecision 覆盖 checkPersonDMAccess 的纯决策核心
// personDMAccessDecision 所有 8 条决策路径。这个测试**不依赖 DB**——
// checkPersonDMAccess 已经把 DB 查询和决策逻辑拆开，orchestration 层负责
// 收集 DB 数据 → 决策纯函数负责判定。PR #708 review round 3 明确要求：
// "add a DB-free table test for checkPersonDMAccess over the six decision
// cases, in the style of TestPersonSpaceAllows_Rules"——本测试满足该要求，
// 且比 6 case 更全（覆盖了 SystemBot 短路 + own bot 单独分支）。
//
// 决策规则源码见 personDMAccessDecision 头注释。任何决策规则修改，
// 这个表就该跟着更新——它是"允许 vs 拒绝"契约的可执行文档。
func TestPersonDMAccessDecision(t *testing.T) {
	const (
		me    = "user_login"
		peer  = "user_peer"
		other = "user_other"
	)
	cases := []struct {
		name string
		in   personDMAccessInputs
		want personDMAccessResult
	}{
		{
			name: "notes_to_self",
			in:   personDMAccessInputs{loginUID: me, peerUID: me},
			want: personDMAllowNotesToSelf,
		},
		{
			name: "system_bot_allowed",
			// fileHelper / botfather / u_10000 / notification 都走这条：
			// SystemBot=true 直接短路到 blacklist；无被拉黑则放行。
			in:   personDMAccessInputs{loginUID: me, peerUID: "fileHelper", isSystemBot: true},
			want: personDMAllowSystemBot,
		},
		{
			name: "system_bot_blocked_by_me",
			// 用户拉黑 SystemBot 仍生效——防骚扰边界，符合 checkP2PAccess Step 3。
			in:   personDMAccessInputs{loginUID: me, peerUID: "fileHelper", isSystemBot: true, blockedByMe: true},
			want: personDMDenyBlacklist,
		},
		{
			name: "own_bot_skips_all",
			// 自己创建的 bot：跳过 Space / friend / blacklist。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, isRobot: true, creatorUID: me},
			want: personDMAllowOwnBot,
		},
		{
			name: "own_bot_ignores_blacklist",
			// 即使 blockedByMe/blockedByPeer 为 true，own bot 也直接放行
			// （因为 personDMAccessDecision 在 own bot 分支就 return，不检查 blacklist）。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, isRobot: true, creatorUID: me, blockedByMe: true, blockedByPeer: true},
			want: personDMAllowOwnBot,
		},
		{
			name: "other_bot_friend_allowed",
			// 别人的 bot：需要好友关系；有 friend → 通过 blacklist → 放行。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, isRobot: true, creatorUID: other, isFriend: true},
			want: personDMAllowOtherBotFriend,
		},
		{
			name: "other_bot_no_friend_denied",
			// 别人的 bot 不是好友 → 404。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, isRobot: true, creatorUID: other, isFriend: false},
			want: personDMDenyOtherBotNotFriend,
		},
		{
			name: "other_bot_friend_but_blocked",
			// 别人的 bot 是好友但双向 blacklist 存在 → 404。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, isRobot: true, creatorUID: other, isFriend: true, blockedByPeer: true},
			want: personDMDenyBlacklist,
		},
		{
			name: "real_user_same_space",
			// 真人 + 同 Space → 放行（企业模式 friend 表可能为空）。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, spaceID: "s1", sameSpace: true},
			want: personDMAllowRealUserSameSpace,
		},
		{
			name: "real_user_friend_no_space_optin",
			// 未 opt-in Space（spaceID=""）+ 好友 → 放行。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, spaceID: "", isFriend: true},
			want: personDMAllowRealUserFriend,
		},
		{
			name: "real_user_no_space_no_friend",
			// 真人 + 无 Space + 非好友 → 404。这正是 issue #484 / YUJ-219-A 关注的边界。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, spaceID: "", isFriend: false},
			want: personDMDenyRealUserNoRelation,
		},
		{
			name: "real_user_space_not_member_but_friend",
			// 有 spaceID 但两人不在同一 Space；好友兜底放行——这是 checkP2PAccess
			// 明说的"same-space OR friend"："或"关系。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, spaceID: "s1", sameSpace: false, isFriend: true},
			want: personDMAllowRealUserFriend,
		},
		{
			name: "real_user_same_space_blocked",
			// 真人同 Space 但双向 blacklist → 404。
			in:   personDMAccessInputs{loginUID: me, peerUID: peer, spaceID: "s1", sameSpace: true, blockedByMe: true},
			want: personDMDenyBlacklist,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := personDMAccessDecision(tc.in)
			assert.Equal(t, tc.want, got, "case %q: unexpected decision", tc.name)
		})
	}
}
