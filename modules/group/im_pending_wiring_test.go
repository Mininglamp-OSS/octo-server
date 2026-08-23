package group

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingUnsubStub 让 /channel/subscriber_remove 返回 500，其余 IM 接口照常成功。
// 这是本次修复的唯一危险分支，用真 broker 造不出来。
type failingUnsubStub struct {
	mu       sync.Mutex
	attempts []string
	server   *httptest.Server
}

func newFailingUnsubStub(t *testing.T, ctx *config.Context) *failingUnsubStub {
	t.Helper()
	stub := &failingUnsubStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/channel/subscriber_remove", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var call struct {
			Subscribers []string `json:"subscribers"`
		}
		_ = json.Unmarshal(body, &call)
		stub.mu.Lock()
		stub.attempts = append(stub.attempts, call.Subscribers...)
		stub.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"msg":"broker down"}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
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

func pendingUnsubUIDs(t *testing.T, ctx *config.Context, channelID string) []string {
	t.Helper()
	var uids []string
	_, err := ctx.DB().SelectBySql(
		"SELECT uid FROM im_pending_subscriber_removal WHERE channel_id=? ORDER BY uid", channelID).Load(&uids)
	require.NoError(t, err)
	return uids
}

// TestRemoveGroupMembersQueuesUnsubscribeOnIMFailure 是 #797 P0 在**真实调用点**上的回归。
//
// 修复前：IMRemoveSubscriber 失败只打日志，成员行已删、订阅还在，而实测这种泄漏态
// 与正常群成员完全无差别（照发照收），且四条自愈路径全断 —— 重跑清理工单是空转
// （重试范围由活跃 group_member 行推导，人已删）、broker 不重载、用户看不到这个群、
// 管理员看不到这个人。所以泄漏是永久的。
//
// 修复后：待办与成员行删除同事务落库，worker 重试到收敛。
func TestRemoveGroupMembersQueuesUnsubscribeOnIMFailure(t *testing.T) {
	ctx, g := cascadeSetup(t)
	stub := newFailingUnsubStub(t, ctx)

	const groupNo, victim = "g-im-queue", "u-im-victim"
	seedGroupInSpace(t, ctx, groupNo, "sp-im-queue", "u-owner")
	seedGroupMember(t, ctx, groupNo, "u-owner", MemberRoleCreator)
	seedGroupMember(t, ctx, groupNo, victim, MemberRoleCommon)

	resp, err := g.groupService.RemoveGroupMembers(&RemoveGroupMembersServiceReq{
		GroupNo:     groupNo,
		Members:     []string{victim},
		OperatorUID: "u-owner",
	})
	require.NoError(t, err, "IM 抖动不该让整个移除失败——成员行删除本身是成功的")
	require.Equal(t, 1, resp.Removed)

	// 成员行确实没了
	_, stillIn := liveMemberRole(t, ctx, groupNo, victim)
	assert.False(t, stillIn)

	// 退订确实尝试过、并且失败了
	stub.mu.Lock()
	attempted := len(stub.attempts)
	stub.mu.Unlock()
	assert.NotZero(t, attempted, "必须真的尝试过退订")

	// 关键断言：失败留下了待办，而不是蒸发
	assert.Equal(t, []string{victim}, pendingUnsubUIDs(t, ctx, groupNo),
		"退订失败必须留下待办，否则订阅永久泄漏且四方不可见")
}
