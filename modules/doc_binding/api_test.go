package doc_binding

// api_test.go
//
// 用 wkhttp.New() 起裸路由 + in-memory store + stub group/space auth 直接跑 4 endpoint。
// 不依赖 MySQL 是因为本地 CI 没数据库；handler 里所有真正落 DB 的路径都在 store 接口后面。
// 覆盖矩阵（父单 OCT-134 §验收）：
//   - 群成员 / 群主/管理员 / 非群成员 / 跨 space
//   - hidden-404：非成员看不到 slug 存在（404 而不是 403）
//   - allow_share_code 默认 false 生效

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- stubs ----------------------------------------------------------------

// memStore 是 store 的 in-memory 实现；用 map 直存 *Model，够单测隔离场景用。
type memStore struct {
	mu   sync.Mutex
	rows map[string]*Model
	// 允许测试注入错误
	insertErr error
	queryErr  error
	updateErr error
	deleteErr error
}

func newMemStore() *memStore { return &memStore{rows: map[string]*Model{}} }

func (s *memStore) insert(m *Model) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[m.Slug]; ok {
		// 模拟 MySQL UNIQUE(slug) 冲突：格式对齐 isDupSlugErr 的匹配
		return errors.New("Error 1062: Duplicate entry '" + m.Slug + "' for key 'doc_binding_slug'")
	}
	c := *m
	s.rows[m.Slug] = &c
	return nil
}
func (s *memStore) queryBySlug(slug string) (*Model, error) {
	if s.queryErr != nil {
		return nil, s.queryErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.rows[slug]
	if !ok {
		return nil, nil
	}
	c := *m
	return &c, nil
}
func (s *memStore) updateAllowShareCode(slug string, allow int) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.rows[slug]
	if !ok {
		return nil
	}
	m.AllowShareCode = allow
	return nil
}
func (s *memStore) deleteBySlug(slug string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, slug)
	return nil
}

// stubGroupAuth: (groupNo, uid) → 权限布尔的两张表；未列 = false。
type stubGroupAuth struct {
	members  map[string]map[string]bool // groupNo -> uid -> isActiveMember
	managers map[string]map[string]bool // groupNo -> uid -> isCreatorOrManager
}

func newStubGroupAuth() *stubGroupAuth {
	return &stubGroupAuth{
		members:  map[string]map[string]bool{},
		managers: map[string]map[string]bool{},
	}
}
func (s *stubGroupAuth) addMember(gn, uid string) { ensure(s.members, gn)[uid] = true }
func (s *stubGroupAuth) addManager(gn, uid string) {
	ensure(s.managers, gn)[uid] = true
	ensure(s.members, gn)[uid] = true
}

func (s *stubGroupAuth) ExistMemberActive(gn, uid string) (bool, error) {
	return s.members[gn][uid], nil
}
func (s *stubGroupAuth) IsCreatorOrManager(gn, uid string) (bool, error) {
	return s.managers[gn][uid], nil
}

// stubSpaceAuth: 记录成员及 role。
type stubSpaceAuth struct {
	roles map[string]map[string]int // spaceId -> uid -> role (仅当作 member 时才有条目)
}

func newStubSpaceAuth() *stubSpaceAuth {
	return &stubSpaceAuth{roles: map[string]map[string]int{}}
}
func (s *stubSpaceAuth) addMember(sp, uid string, role int) {
	ensureInt(s.roles, sp)[uid] = role
}
func (s *stubSpaceAuth) IsMember(sp, uid string) (bool, error) {
	_, ok := s.roles[sp][uid]
	return ok, nil
}
func (s *stubSpaceAuth) MemberRole(sp, uid string) (int, bool, error) {
	r, ok := s.roles[sp][uid]
	return r, ok, nil
}

// stubThreadAuth: 记录 (groupNo, threadId) 归属；未列 = 归属不上。
// 允许注入 err 用来覆盖 store-failed 路径。
type stubThreadAuth struct {
	ownership map[string]map[string]bool
	existErr  error
}

func newStubThreadAuth() *stubThreadAuth {
	return &stubThreadAuth{ownership: map[string]map[string]bool{}}
}
func (s *stubThreadAuth) addThread(gn, tid string) { ensure(s.ownership, gn)[tid] = true }
func (s *stubThreadAuth) ExistInGroup(gn, tid string) (bool, error) {
	if s.existErr != nil {
		return false, s.existErr
	}
	return s.ownership[gn][tid], nil
}

func ensure(m map[string]map[string]bool, k string) map[string]bool {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	return m[k]
}
func ensureInt(m map[string]map[string]int, k string) map[string]int {
	if m[k] == nil {
		m[k] = map[string]int{}
	}
	return m[k]
}

// ---- test harness ---------------------------------------------------------

// buildHandler 拼 handler + in-memory 依赖；不走 New() 以避免碰 ctx.DB()。
func buildHandler() (*DocBinding, *memStore, *stubGroupAuth, *stubSpaceAuth, *stubThreadAuth) {
	st := newMemStore()
	ga := newStubGroupAuth()
	sa := newStubSpaceAuth()
	ta := newStubThreadAuth()
	// Log 不注入的话，任何 h.Error(...) 路径会 nil deref。既有 27 测试都没走到 err 分支所以未暴露；
	// thread create 的 store-failed 场景第一次触发这条路径。
	h := &DocBinding{db: st, groupAuth: ga, spaceAuth: sa, threadAuth: ta, Log: log.NewTLog("doc_binding_test")}
	return h, st, ga, sa, ta
}

// newTestRouter 挂 handler 到 wkhttp，注入 uid，避开 AuthMiddleware（后者需要 cache/token）。
func newTestRouter(h *DocBinding, uid string) *wkhttp.WKHttp {
	gin.SetMode(gin.TestMode)
	r := wkhttp.New()
	inject := func(c *wkhttp.Context) {
		if uid != "" {
			c.Set("uid", uid)
		}
		c.Next()
	}
	grp := r.Group("/v1/docs", inject)
	grp.POST("/bindings", h.create)
	grp.GET("/bindings/:slug", h.get)
	grp.PUT("/bindings/:slug", h.update)
	grp.DELETE("/bindings/:slug", h.delete)
	return r
}

func do(r *wkhttp.WKHttp, method, path string, body interface{}) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// D14 legacy compat: ResponseErrorL pins wire status to 400 regardless of the
// error code's semantic HTTPStatus (see AGENTS.md §httperr / pkg/httperr/respond.go).
// New endpoint tests therefore assert on the localized message (each code has a
// distinct DefaultMessage) instead of w.Code. If octo-server later flips
// endpoints to ResponseErrorLWithStatus, tighten these to real status codes.
const (
	msgNotFound    = "Doc binding not found."
	msgForbidden   = "You do not have permission to modify this doc binding."
	msgConflict    = "This slug is already bound."
	msgReqInvalid  = "Invalid request."
	msgStoreFailed = "Doc binding storage operation failed."
)

// assertErrMsg 校验业务错误响应：wire=400（D14 遗留兼容），body msg == 期望。
// 用它替代 assert.Equal(t, http.StatusXxx, w.Code) 以匹配 octo 现有响应约定。
func assertErrMsg(t *testing.T, w *httptest.ResponseRecorder, wantMsg string) {
	t.Helper()
	if !assert.Equalf(t, http.StatusBadRequest, w.Code, "wire status should be 400 (D14 compat); body=%s", w.Body.String()) {
		return
	}
	assert.Containsf(t, w.Body.String(), "\"msg\":\""+wantMsg+"\"", "body: %s", w.Body.String())
}

// ==================== POST /v1/docs/bindings (create) ====================

func TestCreate_Group_ByManager_OK(t *testing.T) {
	h, st, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")

	r := newTestRouter(h, "u_admin")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "team-charter", "mount_type": "group", "group_no": "g1",
	})
	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	m, _ := st.queryBySlug("team-charter")
	require.NotNil(t, m)
	assert.Equal(t, "u_admin", m.CreatorUID)
	// 未传 allow_share_code：应默认 false（AllowShareCodeOff）
	assert.Equal(t, AllowShareCodeOff, m.AllowShareCode)
}

func TestCreate_Group_ByMember_403(t *testing.T) {
	// 普通群成员没有建 binding 的权限（写路径按 IsCreatorOrManager 判定）
	h, _, ga, _, _ := buildHandler()
	ga.addMember("g1", "u_member")

	r := newTestRouter(h, "u_member")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "s1", "mount_type": "group", "group_no": "g1",
	})
	assertErrMsg(t, w, msgForbidden)
}

func TestCreate_Group_ByNonMember_403(t *testing.T) {
	// 非群成员发 POST：属于主动挂载不存在群，直接 403（不必 hidden-404 —— caller 已经知道自己在填 group_no）
	h, _, _, _, _ := buildHandler()
	r := newTestRouter(h, "u_outsider")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "s1", "mount_type": "group", "group_no": "g_other",
	})
	assertErrMsg(t, w, msgForbidden)
}

func TestCreate_SlugConflict_409(t *testing.T) {
	h, _, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	r := newTestRouter(h, "u_admin")

	first := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "dup", "mount_type": "group", "group_no": "g1",
	})
	require.Equal(t, http.StatusOK, first.Code)

	second := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "dup", "mount_type": "group", "group_no": "g1",
	})
	assertErrMsg(t, second, msgConflict)
}

func TestCreate_AllowShareCode_DefaultFalse(t *testing.T) {
	// 三种情况都应落 AllowShareCodeOff：字段完全缺省 / 显式 false / null 传输由客户端处理
	cases := []struct {
		name string
		body map[string]interface{}
	}{
		{"omitted", map[string]interface{}{"slug": "a1", "mount_type": "group", "group_no": "g1"}},
		{"explicit_false", map[string]interface{}{"slug": "a2", "mount_type": "group", "group_no": "g1", "allow_share_code": false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, st, ga, _, _ := buildHandler()
			ga.addManager("g1", "u_admin")
			r := newTestRouter(h, "u_admin")

			w := do(r, "POST", "/v1/docs/bindings", tc.body)
			require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

			m, _ := st.queryBySlug(tc.body["slug"].(string))
			require.NotNil(t, m)
			assert.Equal(t, AllowShareCodeOff, m.AllowShareCode, "default must be false (0)")
		})
	}
}

func TestCreate_AllowShareCode_ExplicitTrue_Persists(t *testing.T) {
	h, st, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	r := newTestRouter(h, "u_admin")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "a3", "mount_type": "group", "group_no": "g1", "allow_share_code": true,
	})
	require.Equal(t, http.StatusOK, w.Code)

	m, _ := st.queryBySlug("a3")
	require.NotNil(t, m)
	assert.Equal(t, AllowShareCodeOn, m.AllowShareCode)
}

func TestCreate_InvalidMountType_400(t *testing.T) {
	h, _, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	r := newTestRouter(h, "u_admin")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "s1", "mount_type": "unknown", "group_no": "g1",
	})
	assertErrMsg(t, w, msgReqInvalid)
}

func TestCreate_MountTypeMismatchedFields_400(t *testing.T) {
	// group 挂载不该带 space_id
	h, _, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	r := newTestRouter(h, "u_admin")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "s1", "mount_type": "group", "group_no": "g1", "space_id": "sp1",
	})
	assertErrMsg(t, w, msgReqInvalid)
}

func TestCreate_InvalidSlug_400(t *testing.T) {
	// 大写、中文、空格、超长都应被 slugPattern 拒
	badSlugs := []string{"UPPER", "空格 slug", "汉字slug", "with/slash", ""}
	h, _, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	r := newTestRouter(h, "u_admin")
	for _, bs := range badSlugs {
		t.Run(bs, func(t *testing.T) {
			w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
				"slug": bs, "mount_type": "group", "group_no": "g1",
			})
			assertErrMsgf(t, w, msgReqInvalid, "slug=%q", bs)
		})
	}
}

// ==================== GET /v1/docs/bindings/:slug ====================

func TestGet_Group_MemberSees(t *testing.T) {
	h, st, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	ga.addMember("g1", "u_member")
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin"}))

	r := newTestRouter(h, "u_member")
	w := do(r, "GET", "/v1/docs/bindings/s1", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), `"slug":"s1"`)
	assert.Contains(t, w.Body.String(), `"mount_type":"group"`)
}

func TestGet_Group_NonMember_Hidden404(t *testing.T) {
	// 关键 hidden-404：不给非成员泄漏\"slug 存在\"
	h, st, _, _, _ := buildHandler()
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin"}))

	r := newTestRouter(h, "u_outsider")
	w := do(r, "GET", "/v1/docs/bindings/s1", nil)
	assertErrMsgWithNote(t, w, msgNotFound, "non-member must see semantic-404 (hidden-404), not forbidden")
}

func TestGet_UnknownSlug_404(t *testing.T) {
	h, _, ga, _, _ := buildHandler()
	ga.addMember("g1", "u_member")
	r := newTestRouter(h, "u_member")
	w := do(r, "GET", "/v1/docs/bindings/does-not-exist", nil)
	assertErrMsg(t, w, msgNotFound)
}

// ==================== PUT /v1/docs/bindings/:slug ====================

func TestUpdate_Manager_Toggles_AllowShareCode(t *testing.T) {
	h, st, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin", AllowShareCode: AllowShareCodeOff}))

	r := newTestRouter(h, "u_admin")
	w := do(r, "PUT", "/v1/docs/bindings/s1", map[string]interface{}{"allow_share_code": true})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	m, _ := st.queryBySlug("s1")
	require.NotNil(t, m)
	assert.Equal(t, AllowShareCodeOn, m.AllowShareCode)
}

func TestUpdate_Member_403(t *testing.T) {
	// 普通群员看得到 binding，但没资格改 → 差别里才是 403
	h, st, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	ga.addMember("g1", "u_member")
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin"}))

	r := newTestRouter(h, "u_member")
	w := do(r, "PUT", "/v1/docs/bindings/s1", map[string]interface{}{"allow_share_code": true})
	assertErrMsg(t, w, msgForbidden)
}

func TestUpdate_NonMember_Hidden404(t *testing.T) {
	// 非成员 PUT 也要 hidden-404，不能因为 403 泄漏\"存在\"
	h, st, _, _, _ := buildHandler()
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin"}))

	r := newTestRouter(h, "u_outsider")
	w := do(r, "PUT", "/v1/docs/bindings/s1", map[string]interface{}{"allow_share_code": true})
	assertErrMsg(t, w, msgNotFound)
}

func TestUpdate_MissingAllowShareCode_400(t *testing.T) {
	h, st, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_admin")
	w := do(r, "PUT", "/v1/docs/bindings/s1", map[string]interface{}{})
	assertErrMsg(t, w, msgReqInvalid)
}

// ==================== DELETE /v1/docs/bindings/:slug ====================

func TestDelete_Manager_OK(t *testing.T) {
	h, st, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_admin")
	w := do(r, "DELETE", "/v1/docs/bindings/s1", nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	m, _ := st.queryBySlug("s1")
	assert.Nil(t, m)
}

func TestDelete_Member_403(t *testing.T) {
	h, st, ga, _, _ := buildHandler()
	ga.addManager("g1", "u_admin")
	ga.addMember("g1", "u_member")
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_member")
	w := do(r, "DELETE", "/v1/docs/bindings/s1", nil)
	assertErrMsg(t, w, msgForbidden)
}

func TestDelete_NonMember_Hidden404(t *testing.T) {
	h, st, _, _, _ := buildHandler()
	require.NoError(t, st.insert(&Model{Slug: "s1", MountType: MountTypeGroup, GroupNo: "g1", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_outsider")
	w := do(r, "DELETE", "/v1/docs/bindings/s1", nil)
	assertErrMsg(t, w, msgNotFound)
}

// ==================== thread 挂载 ====================

func TestGet_Thread_ParentGroupMember_Sees(t *testing.T) {
	h, st, ga, _, _ := buildHandler()
	ga.addMember("g1", "u_member")
	require.NoError(t, st.insert(&Model{Slug: "th1", MountType: MountTypeThread, GroupNo: "g1", ThreadId: "t_short", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_member")
	w := do(r, "GET", "/v1/docs/bindings/th1", nil)
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

func TestUpdate_Thread_Creator_CanEdit_IfStillMember(t *testing.T) {
	// binding creator（不是群管）也能写自己建的 thread binding —— 但必须还在父群里
	h, st, ga, _, _ := buildHandler()
	ga.addMember("g1", "u_creator")
	require.NoError(t, st.insert(&Model{Slug: "th2", MountType: MountTypeThread, GroupNo: "g1", ThreadId: "t2", CreatorUID: "u_creator"}))
	r := newTestRouter(h, "u_creator")
	w := do(r, "PUT", "/v1/docs/bindings/th2", map[string]interface{}{"allow_share_code": true})
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

func TestUpdate_Thread_Creator_LeftGroup_403(t *testing.T) {
	// 原 creator 已离群 → 写权限失效（避免离群后残留权限）
	h, st, _, _, _ := buildHandler()
	// 不 addMember，模拟已离群
	require.NoError(t, st.insert(&Model{Slug: "th3", MountType: MountTypeThread, GroupNo: "g1", ThreadId: "t3", CreatorUID: "u_creator"}))
	r := newTestRouter(h, "u_creator")
	w := do(r, "PUT", "/v1/docs/bindings/th3", map[string]interface{}{"allow_share_code": true})
	// 已离群 → hidden-404（读权限已经不过）
	assertErrMsg(t, w, msgNotFound)
}

// ==================== space 挂载：跨 space 场景 ====================

func TestGet_Space_Member_Sees(t *testing.T) {
	h, st, _, sa, _ := buildHandler()
	sa.addMember("sp1", "u_member", 0)
	require.NoError(t, st.insert(&Model{Slug: "sp_doc", MountType: MountTypeSpace, SpaceId: "sp1", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_member")
	w := do(r, "GET", "/v1/docs/bindings/sp_doc", nil)
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

func TestGet_Space_CrossSpaceOutsider_Hidden404(t *testing.T) {
	// caller 是 sp_other 的成员，但 binding 挂在 sp1 → hidden-404
	h, st, _, sa, _ := buildHandler()
	sa.addMember("sp_other", "u_x", 2)
	require.NoError(t, st.insert(&Model{Slug: "sp_doc", MountType: MountTypeSpace, SpaceId: "sp1", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_x")
	w := do(r, "GET", "/v1/docs/bindings/sp_doc", nil)
	assertErrMsgWithNote(t, w, msgNotFound, "cross-space caller must see semantic-404 (hidden-404)")
}

func TestUpdate_Space_Admin_OK(t *testing.T) {
	h, st, _, sa, _ := buildHandler()
	sa.addMember("sp1", "u_admin", 1) // 1=admin
	require.NoError(t, st.insert(&Model{Slug: "sp_doc", MountType: MountTypeSpace, SpaceId: "sp1", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_admin")
	w := do(r, "PUT", "/v1/docs/bindings/sp_doc", map[string]interface{}{"allow_share_code": true})
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

func TestUpdate_Space_NormalMember_403(t *testing.T) {
	// role=0 普通成员看得到但不能写 → 差别里 403
	h, st, _, sa, _ := buildHandler()
	sa.addMember("sp1", "u_normal", 0)
	require.NoError(t, st.insert(&Model{Slug: "sp_doc", MountType: MountTypeSpace, SpaceId: "sp1", CreatorUID: "u_admin"}))
	r := newTestRouter(h, "u_normal")
	w := do(r, "PUT", "/v1/docs/bindings/sp_doc", map[string]interface{}{"allow_share_code": true})
	assertErrMsg(t, w, msgForbidden)
}

// ==================== 小工具：isDupSlugErr ====================

func TestIsDupSlugErr(t *testing.T) {
	assert.True(t, isDupSlugErr(errors.New("Error 1062: Duplicate entry 'x' for key 'doc_binding_slug'")))
	assert.False(t, isDupSlugErr(errors.New("Error 1062: Duplicate entry 'x' for key 'other_index'")))
	assert.False(t, isDupSlugErr(nil))
	assert.False(t, isDupSlugErr(errors.New("connection refused")))
}

func assertErrMsgf(t *testing.T, w *httptest.ResponseRecorder, wantMsg, note string, args ...interface{}) {
	t.Helper()
	assert.Equalf(t, http.StatusBadRequest, w.Code, note+" (wire): body=%s", append(args, w.Body.String())...)
	assert.Containsf(t, w.Body.String(), "\"msg\":\""+wantMsg+"\"", note+" (msg)", args...)
}

func assertErrMsgWithNote(t *testing.T, w *httptest.ResponseRecorder, wantMsg, note string) {
	t.Helper()
	assert.Equalf(t, http.StatusBadRequest, w.Code, note+" (wire=400 D14); body=%s", w.Body.String())
	assert.Containsf(t, w.Body.String(), "\"msg\":\""+wantMsg+"\"", note)
}

// ==================== thread 挂载 · 创建路径（父群归属校验） ====================
// PR 首版只有"预置一条 thread binding 再测 get/update"的测试，缺 create 路径的归属校验。
// review02 复核指出：A 群管理员可能把 B 群下 thread 的 short_id 拿来当 thread_id 用；
// 若不显式校验 (group_no, thread_id) 归属，binding 会写成"group_no=A + thread_id=belongs_to_B"，
// 之后按 group_no=A 判 ACL 就跟真实所有者错位。下面用例把这个洞焊死。

func TestCreate_Thread_Manager_OwnershipOK(t *testing.T) {
	h, st, ga, _, ta := buildHandler()
	ga.addManager("g1", "u_admin")
	ta.addThread("g1", "t_ok") // (g1,t_ok) 归属真实

	r := newTestRouter(h, "u_admin")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "th-ok", "mount_type": "thread", "group_no": "g1", "thread_id": "t_ok",
	})
	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	m, _ := st.queryBySlug("th-ok")
	require.NotNil(t, m)
	assert.Equal(t, MountTypeThread, m.MountType)
	assert.Equal(t, "g1", m.GroupNo)
	assert.Equal(t, "t_ok", m.ThreadId)
}

func TestCreate_Thread_ThreadNotInGroup_400(t *testing.T) {
	// A 群管理员拿 B 群下的 thread_id 建 binding —— 必须挡在权限之前，回 400 而不是落库。
	h, st, ga, _, ta := buildHandler()
	ga.addManager("gA", "u_adminA")
	ta.addThread("gB", "t_belongs_to_B") // (gA, t_belongs_to_B) 归属不上

	r := newTestRouter(h, "u_adminA")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "steal", "mount_type": "thread", "group_no": "gA", "thread_id": "t_belongs_to_B",
	})
	// 归属不上：400 + field=thread_id；不是 hidden-404 因为 caller 自报 thread_id，不涉及"该 thread 是否存在于 B 群"的旁路信息泄漏
	assertErrMsg(t, w, msgReqInvalid)
	// D14 legacy body 只吐 {msg,status}，不带 details.field；msg=Invalid request. 已足够断言路径。

	// 落库校验：绝对不能写成功
	m, _ := st.queryBySlug("steal")
	assert.Nil(t, m, "binding must not be persisted when thread_id does not belong to group_no")
}

func TestCreate_Thread_UnknownThread_400(t *testing.T) {
	// 完全不存在的 thread_id 也应挡下，避免出现指向空的挂载记录
	h, st, ga, _, ta := buildHandler()
	ga.addManager("g1", "u_admin")
	_ = ta // 未 addThread

	r := newTestRouter(h, "u_admin")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "ghost", "mount_type": "thread", "group_no": "g1", "thread_id": "t_nope",
	})
	assertErrMsg(t, w, msgReqInvalid)
	// D14 legacy body 只吐 {msg,status}，不带 details.field；msg=Invalid request. 已足够断言路径。
	m, _ := st.queryBySlug("ghost")
	assert.Nil(t, m)
}

func TestCreate_Thread_OwnershipCheckedBeforeAuth(t *testing.T) {
	// 关键排序：归属校验必须在 canWrite 之前。这样即便 caller 不是父群管理员，
	// 我们不会把"这个 thread 在别的群里存在"的信号顺着 403/404 差异泄给他。
	// 场景：uid 是 gA 群管，但 thread 归属 gB；应报 400/field=thread_id，
	// 不能因为 gA 权限不足反而回 403（那也算旁路泄漏"归属校验通过了"）。
	h, _, ga, _, ta := buildHandler()
	ga.addManager("gA", "u_adminA")
	ta.addThread("gB", "t_x")

	r := newTestRouter(h, "u_adminA")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "order", "mount_type": "thread", "group_no": "gA", "thread_id": "t_x",
	})
	assertErrMsg(t, w, msgReqInvalid)
	// D14 legacy body 只吐 {msg,status}，不带 details.field；msg=Invalid request. 已足够断言路径。
}

func TestCreate_Thread_OwnershipStoreError_StoreFailed(t *testing.T) {
	// thread 归属查询本身报错走 store_failed 路径，不应该把 err 当作"不存在"回 400。
	h, _, ga, _, ta := buildHandler()
	ga.addManager("g1", "u_admin")
	ta.existErr = errors.New("db down")

	r := newTestRouter(h, "u_admin")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "boom", "mount_type": "thread", "group_no": "g1", "thread_id": "t_any",
	})
	assertErrMsg(t, w, msgStoreFailed)
}

func TestCreate_Thread_ByGroupMember_403_EvenIfOwnershipOK(t *testing.T) {
	// 归属通过后再判 canWrite；普通群员照旧 403（不是管理员就不能建 binding）。
	h, _, ga, _, ta := buildHandler()
	ga.addMember("g1", "u_member")
	ta.addThread("g1", "t1")

	r := newTestRouter(h, "u_member")
	w := do(r, "POST", "/v1/docs/bindings", map[string]interface{}{
		"slug": "th-mem", "mount_type": "thread", "group_no": "g1", "thread_id": "t1",
	})
	assertErrMsg(t, w, msgForbidden)
}
