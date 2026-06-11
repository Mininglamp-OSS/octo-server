package message

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
)

// TestChannelFiles_TopicBlacklist 验证 YUJ-4219：/channel/files 子区(CommunityTopic)分支
// 解析父群后必须用 ExistMemberActive 排除黑名单。被拉黑用户(is_deleted=0,status=Blacklist)
// 不应能拉取子区文件清单（名/URL），正常成员可正常通过门禁。
func TestChannelFiles_TopicBlacklist(t *testing.T) {
	// IM mock：成员通过门禁后返回空文件列表即可（本测试只验门禁，不验文件解析）。
	mockIM := mockWuKongIMServer(t, []map[string]interface{}{})
	defer mockIM.Close()

	s, ctx := newTestServer()
	ctx.GetConfig().WuKongIM.APIURL = mockIM.URL

	// 本包的 newTestServer 不跑迁移；门禁只查 group_member 表，按需手建最小列集，
	// 与本文件 TestChannelFiles_WithFiles 手建 message_extra 等表的写法一致。
	_, err := ctx.DB().UpdateBySql("CREATE TABLE IF NOT EXISTS group_member (id INTEGER PRIMARY KEY AUTO_INCREMENT, group_no VARCHAR(40), uid VARCHAR(40), status INT DEFAULT 1, is_deleted INT DEFAULT 0)").Exec()
	assert.NoError(t, err)
	// 正常成员放行后会走到 channel_offset 查询；手建最小表让 200 放行路径可达
	// （mockIM 返回空文件列表，无需 message_extra 等下游表）。
	_, err = ctx.DB().UpdateBySql("CREATE TABLE IF NOT EXISTS channel_offset (id INTEGER PRIMARY KEY AUTO_INCREMENT, uid VARCHAR(40), channel_id VARCHAR(100), channel_type SMALLINT DEFAULT 0, message_seq BIGINT DEFAULT 0)").Exec()
	assert.NoError(t, err)

	// 父群 channelID：32 hex groupNo + 合法 snowflake shortID
	parentGroupNo := strings.ReplaceAll(util.GenerUUID(), "-", "")
	shortID := "1489104291682713601"
	topicChannelID := parentGroupNo + "____" + shortID

	// uid(=登录用户) 先设为被拉黑成员（is_deleted=0, status=Blacklist）
	_, err = ctx.DB().UpdateBySql("INSERT INTO group_member (group_no, uid, status, is_deleted) VALUES (?,?,?,0)",
		parentGroupNo, uid, int(common.GroupMemberStatusBlacklist)).Exec()
	assert.NoError(t, err)

	msg := New(ctx)
	msg.Route(s.GetRoute())

	doReq := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/message/channel/files",
			bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
				"channel_id":   topicChannelID,
				"channel_type": int(common.ChannelTypeCommunityTopic.Uint8()),
				"category":     "all",
			}))))
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// 被拉黑 → 拒（not a member of this group）
	w := doReq()
	assert.NotEqual(t, http.StatusOK, w.Code, "被拉黑用户必须被门禁拦截")
	assert.Contains(t, w.Body.String(), "not a member", "应被识别为非活跃成员")

	// 升级为正常成员 → 放行：mockIM 已配置返回空文件列表，正常成员应一路到
	// http.StatusOK（Jerry-Xin 非阻塞建议：把断言从「不再返回 not a member」加强
	// 到完整放行）。
	_, err = ctx.DB().UpdateBySql("UPDATE group_member SET status=? WHERE group_no=? AND uid=?",
		int(common.GroupMemberStatusNormal), parentGroupNo, uid).Exec()
	assert.NoError(t, err)
	w = doReq()
	assert.Equal(t, http.StatusOK, w.Code, "正常成员应一路放行至 200")
	assert.NotContains(t, w.Body.String(), "not a member", "正常成员不应被成员门禁 over-block")
}
