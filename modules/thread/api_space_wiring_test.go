package thread

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// TestCreateThread_RouteMountsSpaceMiddleware 是路由接线回归测试，堵住 issue #557 复发。
// 建子区路由 POST /v1/groups/:group_no/threads 必须挂 SpaceMiddleware，否则 createThread
// 里的 spacepkg.GetSpaceID(c) 恒返回 ""、req.SpaceID 恒空，FollowThreadForCreator 永不触发，
// 创建者在关注 tab 看不到自己刚建的子区。
//
// 服务层单测可直接填非空 SpaceID，掩盖这处接线缺口，所以必须走真实注册的路由来验证。
// 手法：带一个当前用户并不属于的 space_id 打真实路由。挂了 SpaceMiddleware 时，中间件读到
// space_id 后校验成员身份，非成员被拦成 403（响应体是中间件特有的“无权访问该 Space”）；
// 若该路由没挂 SpaceMiddleware，请求会穿透到 createThread（active 群不会返回该 403，也不会
// 出现该响应体），断言随即失败，从而锁死这处接线。
func TestCreateThread_RouteMountsSpaceMiddleware(t *testing.T) {
	s, ctx := setupTestData(t)
	groupNo := createTestGroup(t, ctx)

	body := util.ToJson(map[string]interface{}{"name": "接线校验子区"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/groups/"+groupNo+"/threads?space_id=sp_not_member", bytes.NewReader([]byte(body)))
	req.Header.Set("token", testutil.Token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code,
		"建子区路由必须挂 SpaceMiddleware：带 space_id 的非成员请求应被拦成 403")
	assert.Contains(t, w.Body.String(), "无权访问该 Space",
		"403 必须来自 SpaceMiddleware 的成员校验（响应体特征），而非其他路径")
}
