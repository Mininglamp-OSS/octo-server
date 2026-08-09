package channel

import (
	"net/http"
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"

	// thread 的 ChannelGet datasource 必须注册，COMMUNITY_TOPIC 分支才可达；
	// channel 与 thread 互不依赖（已确认无 import cycle），故可在测试里 blank-import。
	_ "github.com/Mininglamp-OSS/octo-server/modules/thread"
)

// channel_get_topic_authz_test.go —— COMMUNITY_TOPIC(5) 分支的路由级授权测试。
//
// 单列一个文件的原因：thread 模块的 datasource 只在 DM_THREAD_ON 为真时注册，而该
// 开关在模块 factory 内读取、factory 由 register.GetModules 的 once.Do 只跑一次，
// 所以必须在**首次** GetModules 之前设好 env —— 用包级 TestMain 统一设置，避免与
// channel_get_authz_test.go 里的用例产生顺序耦合。
//
// 这条分支还依赖一个跨模块数据契约：channelGet 从 Extra["group_no"] 取父群号，该值由
// modules/thread 的 newChannelRespWithThread 生成。这里的测试同时钉住那个契约——若
// 将来 thread 改了 key 名或类型，父群成员校验会静默失效（正是本次要堵的洞）。
func TestMain(m *testing.M) {
	_ = os.Setenv("DM_THREAD_ON", "true")
	code := m.Run()
	_ = os.Unsetenv("DM_THREAD_ON")
	os.Exit(code)
}

// seedThread 建子区行。表名/列名与 modules/thread 的 schema 对齐。
func seedThread(t *testing.T, ctx *config.Context, groupNo, shortID, name, creator string) {
	t.Helper()
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO thread (group_no, short_id, name, creator_uid, status, version) VALUES (?,?,?,?,1,1)",
		groupNo, shortID, name, creator,
	).Exec()
	assert.NoError(t, err)
}

// 父群成员：可读子区详情。同时验证 Extra["group_no"] 契约成立（否则会被判 forbidden）。
func TestChannelGet_Topic_ParentGroupMember_OK(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	seedGroup(t, ctx, "g_topic_ok", "topic-parent", "", "creator_x")
	seedMember(t, ctx, "g_topic_ok", "creator_x", 1)
	seedMember(t, ctx, "g_topic_ok", testutil.UID, 0) // 调用方是父群成员
	seedThread(t, ctx, "g_topic_ok", "t1", "TOPIC_VISIBLE_NAME", "creator_x")

	w := getChannel(s, "g_topic_ok____t1", common.ChannelTypeCommunityTopic.Uint8())

	if w.Code != http.StatusOK {
		t.Fatalf("父群成员应能读子区详情, code=%d body=%s", w.Code, w.Body.String())
	}
	assert.Contains(t, w.Body.String(), "TOPIC_VISIBLE_NAME")
}

// 非父群成员：不得读子区详情，且不泄露子区名 / 父群号 / 创建者。
func TestChannelGet_Topic_NonParentMember_Forbidden(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	seedGroup(t, ctx, "g_topic_nm", "topic-parent", "", "creator_x")
	seedMember(t, ctx, "g_topic_nm", "creator_x", 1) // 调用方不是成员
	seedThread(t, ctx, "g_topic_nm", "t1", "TOPIC_SECRET_NAME", "creator_x")

	w := getChannel(s, "g_topic_nm____t1", common.ChannelTypeCommunityTopic.Uint8())

	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.view_forbidden", "body=%s", w.Body.String())
	assert.NotContains(t, w.Body.String(), "TOPIC_SECRET_NAME", "非父群成员不得读到子区名")
	assert.NotContains(t, w.Body.String(), "creator_x", "非父群成员不得读到创建者")
}

// 已退出父群：与非成员同样被拒（成员行保留 is_deleted=1）。
func TestChannelGet_Topic_LeftParentGroup_Forbidden(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	seedGroup(t, ctx, "g_topic_left", "topic-parent", "", "creator_x")
	seedMember(t, ctx, "g_topic_left", "creator_x", 1)
	seedMemberDeleted(t, ctx, "g_topic_left", testutil.UID, 0, 1) // 已退群
	seedThread(t, ctx, "g_topic_left", "t1", "TOPIC_LEFT_NAME", "creator_x")

	w := getChannel(s, "g_topic_left____t1", common.ChannelTypeCommunityTopic.Uint8())

	assert.Equal(t, http.StatusBadRequest, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.view_forbidden")
	assert.NotContains(t, w.Body.String(), "TOPIC_LEFT_NAME")
}

// 子区不存在：与"非父群成员"返回同一 forbidden，不泄露子区是否存在。
func TestChannelGet_Topic_NotFound_UnifiedForbidden(t *testing.T) {
	s, ctx := newChannelAuthzServer(t)
	defer testutil.CleanAllTables(ctx)

	seedGroup(t, ctx, "g_topic_nf", "topic-parent", "", "creator_x")
	seedMember(t, ctx, "g_topic_nf", testutil.UID, 0) // 即便是父群成员，子区不存在也不给存在性信号

	w := getChannel(s, "g_topic_nf____nosuch", common.ChannelTypeCommunityTopic.Uint8())

	assert.NotEqual(t, http.StatusInternalServerError, w.Code, "body=%s", w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.group.view_forbidden",
		"子区不存在应与非成员同码, body=%s", w.Body.String())
}
