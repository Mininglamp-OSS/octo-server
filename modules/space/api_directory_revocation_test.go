package space

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
)

// 这一组用例回答的问题只有一个：**生产代码**会不会在移除成员时清掉
// SpaceMiddleware 的成员判定缓存。
//
// 因此它们有一条铁律：移除之后，测试不许碰缓存。不调用
// spacepkg.InvalidateMembershipCache、不 sleep 等 TTL 到期、不重启 server。
// 一旦测试自己清了缓存，断言就退化成「缓存清掉后判定会翻转」——那验证的是中间件
// 的读逻辑，而不是移除路径有没有触发失效，恰恰放过了要测的缺陷。
//
// 这正是 modules/robot 那条 TestSpaceBots_RemovedMemberLosesAccess 早先的问题：
// 它的请求助手每次发请求前都会失效一遍缓存，于是无论生产代码清不清，用例都是绿的。
//
// 移除前那次成功请求不是铺垫，是前提：它把「是成员」写进 Redis（正向 60s）。
// 没有它，移除后的 403 可能只是因为缓存本来就是空的，用例会在缺陷存在时照样通过。
// assertMembershipCached 把这个前提钉死。

// membershipCacheKey 复刻 pkg/space 的私有键格式。刻意复刻而不是导出：
// 键格式漂移时 assertMembershipCached 会立刻失败，正好提示这组断言需要跟着更新——
// 若改成导出常量共用，格式一变两边同步漂移，断言会静默失效。
func membershipCacheKey(spaceID, uid string) string {
	return fmt.Sprintf("space:member:%s:%s", spaceID, uid)
}

func assertMembershipCached(t *testing.T, spaceID, uid string) {
	t.Helper()
	val, err := testCtx.GetRedisConn().GetString(membershipCacheKey(spaceID, uid))
	assert.NoError(t, err)
	assert.Equal(t, "1", val,
		"前置条件不成立：一次成功请求之后，成员判定应当已被缓存为正向。"+
			"缓存是空的话，后面的 403 证明不了撤权生效")
}

// assertMembershipCacheCleared 必须在「移除之后、下一次请求之前」调用。
//
// 顺序是这条断言的全部意义。移除后的第一次请求会 miss、查库、拿到 false，然后把
// 否定判定写回缓存（"0"，30s）——所以请求之后再看，键必然存在，只是值变了，
// 断言「键不存在」会恒假。而在请求之前，键存在与否恰好区分了两件事：生产代码
// 清掉了它（键消失），还是仅仅让它过期（键还在，值仍是 "1"）。
func assertMembershipCacheCleared(t *testing.T, spaceID, uid string) {
	t.Helper()
	val, _ := testCtx.GetRedisConn().GetString(membershipCacheKey(spaceID, uid))
	assert.Empty(t, val,
		"移除路径应当在写库之后立刻清掉成员缓存条目，实际残留 %q"+
			"（%q 说明正向判定还在，被移除的人在 TTL 到期前仍可访问）", val, val)
}

// getDirectoryAs 与 getDirectory 相同，但用指定 token。
func getDirectoryAs(t *testing.T, srv *server.Server, token, spaceID string, q url.Values) *httptest.ResponseRecorder {
	t.Helper()
	resetSpaceUIDRateLimit(t, testCtx)
	path := "/v1/space/" + spaceID + "/directory"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("token", token)
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

func doJSON(t *testing.T, srv *server.Server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	resetSpaceUIDRateLimit(t, testCtx)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("token", token)
	req.Header.Set("Content-Type", "application/json")
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

// seedRevocationSpace 建一个空间：owner 是 ownerUID，testutil.UID 是普通成员。
//
// 顺带清掉这两个 uid 在该 Space 的成员缓存。这与本文件开头「移除之后不许碰缓存」
// 的铁律不冲突——清理发生在**移除之前**的建场阶段，作用是隔离，不是替生产代码
// 完成失效。
//
// 必须清：spaceID 是固定字面量，而 CleanAllTables 只清 MySQL 不清 Redis。上一轮
// 跑完会在同一个键上留下否定判定（"0"，30s），30 秒内重跑，预热请求就会读到它并
// 拿到 403，前置断言假失败。这条 flake 是真实发生过的，不是假想。
func seedRevocationSpace(t *testing.T, spaceID, ownerUID string) {
	t.Helper()
	seedDirectorySpace(t, spaceID, ownerUID)
	seedDirectoryHuman(t, spaceID, testutil.UID, "Member", 0)
	for _, uid := range []string{testutil.UID, ownerUID} {
		assert.NoError(t, testCtx.GetRedisConn().Del(membershipCacheKey(spaceID, uid)))
	}
}

// assertRevokedImmediately 断言 testutil.UID 立刻失去访问权，且响应体不泄露名册。
func assertRevokedImmediately(t *testing.T, srv *server.Server, spaceID string) {
	t.Helper()
	w := getDirectoryAs(t, srv, testutil.Token, spaceID, nil)
	assert.Equal(t, http.StatusForbidden, w.Code,
		"移除后必须立刻被拒，不能等成员缓存过期（≤60s）: %s", w.Body.String())
	// 只断状态码不够：状态码变了但名册照给同样是越权。
	assert.NotContains(t, w.Body.String(), "\"items\"")
	assert.NotContains(t, w.Body.String(), "Owner")
}

// TestDirectory_RemoveMemberRevokesImmediately 走真实的移除成员接口。
func TestDirectory_RemoveMemberRevokesImmediately(t *testing.T) {
	srv, _ := newRenderedTestServer()
	defer func() { assert.NoError(t, testutil.CleanAllTables(testCtx)) }()

	const spaceID = "sp_revoke_remove"
	const ownerUID = "sp_revoke_owner1"
	seedRevocationSpace(t, spaceID, ownerUID)

	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, testutil.Token, spaceID, nil).Code)
	assertMembershipCached(t, spaceID, testutil.UID)

	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceID+"/members/remove",
		joinApplicantToken(t, ownerUID),
		fmt.Sprintf(`{"uids":[%q]}`, testutil.UID))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assertMembershipCacheCleared(t, spaceID, testutil.UID)
	assertRevokedImmediately(t, srv, spaceID)
}

// TestDirectory_RemoveMemberKeepsBystanderCached 断言失效是**定向**的。
//
// 这条用例挡的是相反方向的退化：为了保险，把移除写成「清掉该 Space 全部成员的
// 缓存」。那样功能上依然正确，撤权测试也全绿，但每移除一个人就让整个 Space 的
// 成员——线上最大约 6000 人——全部回落到查库，热路径（conversation sync、message、
// search、sidebar）会同时被打穿。缓存存在的理由就是避免这件事。
//
// 断言落在旁观者的缓存条目仍是 "1" 上：它同时排除「被删掉」和「被降级成否定」。
func TestDirectory_RemoveMemberKeepsBystanderCached(t *testing.T) {
	srv, _ := newRenderedTestServer()
	defer func() { assert.NoError(t, testutil.CleanAllTables(testCtx)) }()

	const spaceID = "sp_revoke_bystander"
	const ownerUID = "sp_revoke_owner6"
	const bystanderUID = "sp_revoke_bystander_uid"
	seedRevocationSpace(t, spaceID, ownerUID)
	seedDirectoryHuman(t, spaceID, bystanderUID, "Bystander", 0)
	assert.NoError(t, testCtx.GetRedisConn().Del(membershipCacheKey(spaceID, bystanderUID)))

	bystanderToken := joinApplicantToken(t, bystanderUID)
	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, testutil.Token, spaceID, nil).Code)
	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, bystanderToken, spaceID, nil).Code)
	assertMembershipCached(t, spaceID, testutil.UID)
	assertMembershipCached(t, spaceID, bystanderUID)

	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceID+"/members/remove",
		joinApplicantToken(t, ownerUID),
		fmt.Sprintf(`{"uids":[%q]}`, testutil.UID))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assertMembershipCacheCleared(t, spaceID, testutil.UID)
	val, err := testCtx.GetRedisConn().GetString(membershipCacheKey(spaceID, bystanderUID))
	assert.NoError(t, err)
	assert.Equal(t, "1", val,
		"移除一个人不该殃及其他成员的缓存：那会让每次移除都把整个 Space 的热路径打回查库")

	// 旁观者当然还能访问——但这一条只是兜底，上面的缓存断言才是本用例的主张。
	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, bystanderToken, spaceID, nil).Code)
}

// TestDirectory_LeaveSpaceRevokesImmediately 走真实的退出空间接口。
//
// 自主退出同样要清缓存：多设备场景下另一台设备的会话仍在，缓存不清就还能读。
func TestDirectory_LeaveSpaceRevokesImmediately(t *testing.T) {
	srv, _ := newRenderedTestServer()
	defer func() { assert.NoError(t, testutil.CleanAllTables(testCtx)) }()

	const spaceID = "sp_revoke_leave"
	seedRevocationSpace(t, spaceID, "sp_revoke_owner2")

	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, testutil.Token, spaceID, nil).Code)
	assertMembershipCached(t, spaceID, testutil.UID)

	w := doJSON(t, srv, http.MethodPost, "/v1/space/"+spaceID+"/leave", testutil.Token, `{}`)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assertMembershipCacheCleared(t, spaceID, testutil.UID)
	assertRevokedImmediately(t, srv, spaceID)
}

// TestDirectory_DisbandRevokesImmediately 走真实的解散空间接口。
//
// 解散只翻 space.status，一行 space_member 都不动，所以很容易被漏掉——但
// CheckMembership 的 SQL 带 INNER JOIN space ON s.status=1，解散后全体成员立刻
// 不再是成员。它和踢人是同一类撤权，只是一次作用于所有人。
func TestDirectory_DisbandRevokesImmediately(t *testing.T) {
	srv, _ := newRenderedTestServer()
	defer func() { assert.NoError(t, testutil.CleanAllTables(testCtx)) }()

	const spaceID = "sp_revoke_disband"
	const ownerUID = "sp_revoke_owner3"
	seedRevocationSpace(t, spaceID, ownerUID)

	ownerToken := joinApplicantToken(t, ownerUID)
	// 两个人都先请求一次，各自把正向判定写进缓存。owner 这次不是多余的：
	// 若不预热，下面对 owner 的断言会在「键本来就不存在」时恒真，测不出任何东西。
	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, testutil.Token, spaceID, nil).Code)
	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, ownerToken, spaceID, nil).Code)
	assertMembershipCached(t, spaceID, testutil.UID)
	assertMembershipCached(t, spaceID, ownerUID)

	w := doJSON(t, srv, http.MethodDelete, "/v1/space/"+spaceID, ownerToken, ``)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assertMembershipCacheCleared(t, spaceID, testutil.UID)
	// 解散是全空间撤权：owner 自己的条目也必须清掉，不能只清「被点名的人」。
	// 这是解散路径独有的失败模式——按 uid 逐个失效的写法在这里会一个都清不掉。
	assertMembershipCacheCleared(t, spaceID, ownerUID)
	assertRevokedImmediately(t, srv, spaceID)
}

// TestDirectory_ManagerForceRemoveRevokesImmediately 走管理端强制移除。
func TestDirectory_ManagerForceRemoveRevokesImmediately(t *testing.T) {
	srv, _ := newRenderedTestServer()
	defer func() { assert.NoError(t, testutil.CleanAllTables(testCtx)) }()

	const spaceID = "sp_revoke_mgr_rm"
	seedRevocationSpace(t, spaceID, "sp_revoke_owner4")

	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, testutil.Token, spaceID, nil).Code)
	assertMembershipCached(t, spaceID, testutil.UID)

	w := doJSON(t, srv, http.MethodDelete, "/v1/manager/spaces/"+spaceID+"/members",
		superAdminToken(t), fmt.Sprintf(`{"uids":[%q]}`, testutil.UID))
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assertMembershipCacheCleared(t, spaceID, testutil.UID)
	assertRevokedImmediately(t, srv, spaceID)
}

// TestDirectory_ManagerForceDisbandRevokesImmediately 走管理端强制解散。
func TestDirectory_ManagerForceDisbandRevokesImmediately(t *testing.T) {
	srv, _ := newRenderedTestServer()
	defer func() { assert.NoError(t, testutil.CleanAllTables(testCtx)) }()

	const spaceID = "sp_revoke_mgr_dis"
	const ownerUID = "sp_revoke_owner5"
	seedRevocationSpace(t, spaceID, ownerUID)

	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, testutil.Token, spaceID, nil).Code)
	assert.Equal(t, http.StatusOK, getDirectoryAs(t, srv, joinApplicantToken(t, ownerUID), spaceID, nil).Code)
	assertMembershipCached(t, spaceID, testutil.UID)
	assertMembershipCached(t, spaceID, ownerUID)

	w := doJSON(t, srv, http.MethodDelete, "/v1/manager/spaces/"+spaceID, superAdminToken(t), ``)
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assertMembershipCacheCleared(t, spaceID, testutil.UID)
	assertMembershipCacheCleared(t, spaceID, ownerUID)
	assertRevokedImmediately(t, srv, spaceID)
}
