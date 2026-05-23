package conversation_ext

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strconvI(i int) string { return strconv.Itoa(i) }

// ---------------------------------------------------------------------------
// Test doubles — in-process stubs that satisfy the Follow handler's needs
// without touching MySQL.
// ---------------------------------------------------------------------------

// stubService is a test double for *Service.
type stubService struct {
	followDMFn        func(uid, spaceID, peerUID string, categoryID *string) error
	unfollowDMFn      func(uid, spaceID, peerUID string) error
	unfollowChannelFn func(uid, spaceID, groupNo string) error
	followChannelFn   func(uid, spaceID, groupNo string) error
	followThreadFn    func(uid, spaceID, threadChannelID string) error
	unfollowThreadFn  func(uid, spaceID, threadChannelID string) error
	// authorizeAndMaterializeFn is the issue #151 gate.  Default (nil) → no-op
	// so existing tests that don't exercise the sort path are unaffected.
	authorizeAndMaterializeFn func(uid, spaceID string, candidateGroupNos []string) error
}

func (s *stubService) FollowDM(uid, spaceID, peerUID string, categoryID *string) error {
	if s.followDMFn != nil {
		return s.followDMFn(uid, spaceID, peerUID, categoryID)
	}
	return nil
}

func (s *stubService) UnfollowDM(uid, spaceID, peerUID string) error {
	if s.unfollowDMFn != nil {
		return s.unfollowDMFn(uid, spaceID, peerUID)
	}
	return nil
}

func (s *stubService) UnfollowChannel(uid, spaceID, groupNo string) error {
	if s.unfollowChannelFn != nil {
		return s.unfollowChannelFn(uid, spaceID, groupNo)
	}
	return nil
}

func (s *stubService) FollowChannel(uid, spaceID, groupNo string) error {
	if s.followChannelFn != nil {
		return s.followChannelFn(uid, spaceID, groupNo)
	}
	return nil
}

func (s *stubService) FollowThread(uid, spaceID, threadChannelID string) error {
	if s.followThreadFn != nil {
		return s.followThreadFn(uid, spaceID, threadChannelID)
	}
	return nil
}

func (s *stubService) UnfollowThread(uid, spaceID, threadChannelID string) error {
	if s.unfollowThreadFn != nil {
		return s.unfollowThreadFn(uid, spaceID, threadChannelID)
	}
	return nil
}

func (s *stubService) AuthorizeAndMaterializeDefaultFollowedGroups(uid, spaceID string, candidateGroupNos []string) error {
	if s.authorizeAndMaterializeFn != nil {
		return s.authorizeAndMaterializeFn(uid, spaceID, candidateGroupNos)
	}
	return nil
}

// stubDB is a test double for *DB (only UpdateSort is needed).
type stubDB struct {
	updateSortFn func(uid, spaceID string, items []SortItem, version int64) error
}

func (d *stubDB) UpdateSort(uid, spaceID string, items []SortItem, version int64) error {
	if d.updateSortFn != nil {
		return d.updateSortFn(uid, spaceID, items, version)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Test router helpers
// ---------------------------------------------------------------------------

// injectAuth is a gin middleware that sets uid and space_id on the context,
// simulating a successfully authenticated + space-resolved request.
func injectAuth(uid, spaceID string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("uid", uid)
		c.Set("space_id", spaceID)
		c.Next()
	}
}

// newTestRouter builds a WKHttp router that registers all 7 Follow handlers
// behind an auth-injection middleware (no real Redis / token logic).
func newTestRouter(svc followService, db sortDB) *wkhttp.WKHttp {
	gin.SetMode(gin.TestMode)
	r := wkhttp.New()
	f := NewFollow(svc, db)

	// auth + space_id injection middleware
	inject := func(c *wkhttp.Context) {
		c.Set("uid", "test-uid")
		c.Set("space_id", "test-space")
		c.Next()
	}

	grp := r.Group("/v1/follow", inject)
	grp.POST("/dm", f.FollowDM)
	grp.DELETE("/dm", f.UnfollowDM)
	grp.POST("/channel/unfollow", f.UnfollowChannel)
	grp.POST("/channel/refollow", f.FollowChannel)
	grp.POST("/thread", f.FollowThread)
	grp.DELETE("/thread", f.UnfollowThread)
	grp.PUT("/sort", f.UpdateSort)

	return r
}

// do is a convenience wrapper that performs a JSON request and returns the
// response recorder.
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

// assertOK checks that the response status is 200.
func assertOK(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
}

// assertBadRequest checks that the response status is 400.
func assertBadRequest(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	assert.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
}

// ---------------------------------------------------------------------------
// FollowDM
// ---------------------------------------------------------------------------

func TestFollow_FollowDM_HappyPath(t *testing.T) {
	var gotUID, gotSpaceID, gotPeerUID string
	var gotCatID *string

	svc := &stubService{
		followDMFn: func(uid, spaceID, peerUID string, categoryID *string) error {
			gotUID, gotSpaceID, gotPeerUID, gotCatID = uid, spaceID, peerUID, categoryID
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/dm", map[string]interface{}{"peer_uid": "peer1"})

	assertOK(t, w)
	assert.Equal(t, "test-uid", gotUID)
	assert.Equal(t, "test-space", gotSpaceID)
	assert.Equal(t, "peer1", gotPeerUID)
	assert.Nil(t, gotCatID)
}

func TestFollow_FollowDM_WithCategoryID(t *testing.T) {
	var gotCatID *string
	svc := &stubService{
		followDMFn: func(uid, spaceID, peerUID string, categoryID *string) error {
			gotCatID = categoryID
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	catID := "cat-uuid-abc"
	w := do(r, "POST", "/v1/follow/dm", map[string]interface{}{
		"peer_uid":    "peer2",
		"category_id": catID,
	})

	assertOK(t, w)
	require.NotNil(t, gotCatID)
	assert.Equal(t, catID, *gotCatID)
}

func TestFollow_FollowDM_MissingPeerUID(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v1/follow/dm", map[string]interface{}{})
	assertBadRequest(t, w)
}

func TestFollow_FollowDM_ServiceError(t *testing.T) {
	svc := &stubService{
		followDMFn: func(uid, spaceID, peerUID string, categoryID *string) error {
			return errors.New("db gone away")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/dm", map[string]interface{}{"peer_uid": "peer1"})
	assertBadRequest(t, w)
}

// PR #21 Round-6 (Jerry-Xin)：DMCategoryChecker 拒绝时 handler 应该把
// ErrDMCategoryForbidden 暴露给客户端。
func TestFollow_FollowDM_CategoryForbidden(t *testing.T) {
	svc := &stubService{
		followDMFn: func(uid, spaceID, peerUID string, categoryID *string) error {
			return ErrDMCategoryForbidden
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/dm", map[string]interface{}{
		"peer_uid":    "peer3",
		"category_id": "not-mine-uuid",
	})
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "dm category forbidden")
}

// ---------------------------------------------------------------------------
// UnfollowDM
// ---------------------------------------------------------------------------

func TestFollow_UnfollowDM_HappyPath(t *testing.T) {
	var gotPeerUID string
	svc := &stubService{
		unfollowDMFn: func(uid, spaceID, peerUID string) error {
			gotPeerUID = peerUID
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "DELETE", "/v1/follow/dm?peer_uid=peerX", nil)

	assertOK(t, w)
	assert.Equal(t, "peerX", gotPeerUID)
}

func TestFollow_UnfollowDM_MissingPeerUID(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "DELETE", "/v1/follow/dm", nil)
	assertBadRequest(t, w)
}

func TestFollow_UnfollowDM_ServiceError(t *testing.T) {
	svc := &stubService{
		unfollowDMFn: func(uid, spaceID, peerUID string) error {
			return errors.New("gone")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "DELETE", "/v1/follow/dm?peer_uid=p", nil)
	assertBadRequest(t, w)
}

// ---------------------------------------------------------------------------
// UnfollowChannel
// ---------------------------------------------------------------------------

func TestFollow_UnfollowChannel_HappyPath(t *testing.T) {
	var gotGroupNo string
	svc := &stubService{
		unfollowChannelFn: func(uid, spaceID, groupNo string) error {
			gotGroupNo = groupNo
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/unfollow", map[string]interface{}{"group_no": "grp1"})

	assertOK(t, w)
	assert.Equal(t, "grp1", gotGroupNo)
}

func TestFollow_UnfollowChannel_MissingGroupNo(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/unfollow", map[string]interface{}{})
	assertBadRequest(t, w)
}

func TestFollow_UnfollowChannel_ServiceError(t *testing.T) {
	svc := &stubService{
		unfollowChannelFn: func(uid, spaceID, groupNo string) error {
			return errors.New("oops")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/unfollow", map[string]interface{}{"group_no": "grp1"})
	assertBadRequest(t, w)
}

// ---------------------------------------------------------------------------
// FollowChannel (refollow)
// ---------------------------------------------------------------------------

func TestFollow_FollowChannel_HappyPath(t *testing.T) {
	var gotGroupNo string
	svc := &stubService{
		followChannelFn: func(uid, spaceID, groupNo string) error {
			gotGroupNo = groupNo
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/refollow", map[string]interface{}{"group_no": "grp2"})

	assertOK(t, w)
	assert.Equal(t, "grp2", gotGroupNo)
}

func TestFollow_FollowChannel_MissingGroupNo(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/refollow", map[string]interface{}{})
	assertBadRequest(t, w)
}

func TestFollow_FollowChannel_ServiceError(t *testing.T) {
	svc := &stubService{
		followChannelFn: func(uid, spaceID, groupNo string) error {
			return errors.New("db error")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/refollow", map[string]interface{}{"group_no": "grp2"})
	assertBadRequest(t, w)
}

// PR #123 round-1 (Jerry-Xin / yujiawei P1) + round-5 (Jerry-Xin follow-up)：
// ErrChannelForbidden 必须作为 403 返回，与 FollowThread 的 ErrThreadForbidden→403
// 契约对齐，让客户端走"无权访问"路径而不是通用 400 重试。
func TestFollow_FollowChannel_Forbidden_Returns403(t *testing.T) {
	svc := &stubService{
		followChannelFn: func(uid, spaceID, groupNo string) error {
			return ErrChannelForbidden
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/refollow", map[string]interface{}{"group_no": "grp2"})
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// ---------------------------------------------------------------------------
// FollowThread
// ---------------------------------------------------------------------------

func TestFollow_FollowThread_HappyPath(t *testing.T) {
	var gotThreadID string
	svc := &stubService{
		followThreadFn: func(uid, spaceID, threadChannelID string) error {
			gotThreadID = threadChannelID
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/thread", map[string]interface{}{"thread_channel_id": "grp1____thr1"})

	assertOK(t, w)
	assert.Equal(t, "grp1____thr1", gotThreadID)
}

func TestFollow_FollowThread_MissingThreadChannelID(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v1/follow/thread", map[string]interface{}{})
	assertBadRequest(t, w)
}

func TestFollow_FollowThread_ServiceError(t *testing.T) {
	svc := &stubService{
		followThreadFn: func(uid, spaceID, threadChannelID string) error {
			return errors.New("tx failed")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/thread", map[string]interface{}{"thread_channel_id": "grp1____thr1"})
	assertBadRequest(t, w)
}

// PR review (Round 3) Blocking #3: SetThreadAuthChecker is the injection seam
// for the auth pipeline.  This test verifies the contract:
//   - When a checker is registered AND it returns ErrThreadForbidden, FollowThread
//     must short-circuit and propagate that error without touching the DB.
//   - When no checker is registered, FollowThread proceeds (backward compatibility
//     for tests that pre-date the injection).
//
// We use a dedicated package-level helper test that exercises Service directly
// with a no-DB session — only the auth short-circuit path is asserted, which
// avoids requiring MySQL for this regression.
func TestService_FollowThread_RejectsWhenCheckerDenies(t *testing.T) {
	called := false
	checker := stubAuthChecker(func(uid, spaceID, groupNo, shortID string) error {
		called = true
		assert.Equal(t, "u1", uid)
		assert.Equal(t, "s1", spaceID)
		assert.Equal(t, "grp1", groupNo)
		assert.Equal(t, "thr1", shortID)
		return ErrThreadForbidden
	})
	// Service constructed without a session: the auth check fires before any
	// DB call so session-nil is safe in this code path.
	s := &Service{}
	s.SetThreadAuthChecker(checker)

	err := s.FollowThread("u1", "s1", "grp1____thr1")
	assert.True(t, called, "checker must be invoked before any DB write")
	assert.ErrorIs(t, err, ErrThreadForbidden)
}

// stubAuthChecker adapts a closure to the ThreadAuthChecker interface.
type stubAuthChecker func(uid, spaceID, groupNo, shortID string) error

func (f stubAuthChecker) AuthorizeThreadFollow(uid, spaceID, groupNo, shortID string) error {
	return f(uid, spaceID, groupNo, shortID)
}

// PR review (Round 3) Blocking #3: ErrThreadForbidden bubbles up as 403,
// distinguishable from generic 400 service errors so the client can show a
// dedicated "no access" message.
func TestFollow_FollowThread_Forbidden_Returns403(t *testing.T) {
	svc := &stubService{
		followThreadFn: func(uid, spaceID, threadChannelID string) error {
			return ErrThreadForbidden
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v1/follow/thread", map[string]interface{}{"thread_channel_id": "grp1____thr1"})
	assert.Equal(t, http.StatusForbidden, w.Code, "body: %s", w.Body.String())
}

// ---------------------------------------------------------------------------
// UnfollowThread
// ---------------------------------------------------------------------------

func TestFollow_UnfollowThread_HappyPath(t *testing.T) {
	var gotThreadID string
	svc := &stubService{
		unfollowThreadFn: func(uid, spaceID, threadChannelID string) error {
			gotThreadID = threadChannelID
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "DELETE", "/v1/follow/thread?thread_channel_id=grp1____thr2", nil)

	assertOK(t, w)
	assert.Equal(t, "grp1____thr2", gotThreadID)
}

func TestFollow_UnfollowThread_MissingThreadChannelID(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "DELETE", "/v1/follow/thread", nil)
	assertBadRequest(t, w)
}

func TestFollow_UnfollowThread_ServiceError(t *testing.T) {
	svc := &stubService{
		unfollowThreadFn: func(uid, spaceID, threadChannelID string) error {
			return errors.New("delete failed")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "DELETE", "/v1/follow/thread?thread_channel_id=grp1____thr2", nil)
	assertBadRequest(t, w)
}

// ---------------------------------------------------------------------------
// UpdateSort (CAS)
// ---------------------------------------------------------------------------

func TestFollow_UpdateSort_HappyPath(t *testing.T) {
	var gotItems []SortItem
	var gotVersion int64
	db := &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			gotItems = items
			gotVersion = version
			return nil
		},
	}
	r := newTestRouter(&stubService{}, db)
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
			{"target_type": 2, "target_id": "grp-1", "sort": 2},
		},
		"version": 3,
	})

	assertOK(t, w)
	require.Len(t, gotItems, 2)
	assert.Equal(t, uint8(1), gotItems[0].TargetType)
	assert.Equal(t, "dm-1", gotItems[0].TargetID)
	assert.Equal(t, int64(3), gotVersion)
}

func TestFollow_UpdateSort_MissingItems(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items":   []interface{}{},
		"version": 0,
	})
	assertBadRequest(t, w)
}

func TestFollow_UpdateSort_InvalidTargetType(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 99, "target_id": "x", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
}

func TestFollow_UpdateSort_CASConflict(t *testing.T) {
	db := &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			return ErrVersionConflict
		},
	}
	r := newTestRouter(&stubService{}, db)
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "version conflict")
}

func TestFollow_UpdateSort_DBError(t *testing.T) {
	db := &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			return errors.New("connection reset")
		},
	}
	r := newTestRouter(&stubService{}, db)
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
}

// PR #21 Round-4 review I3 (yujiawei) regressions：UpdateSort 必须在 parse 阶段
// 拒绝以下输入，避免不必要的 DB 往返 + 不准确的客户端错误提示。

func TestFollow_UpdateSort_TooManyItems_Rejected(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			t.Fatal("DB.UpdateSort must NOT be called for over-cap payloads")
			return nil
		},
	})
	items := make([]map[string]interface{}, 0, maxUpdateSortItems+1)
	for i := 0; i < maxUpdateSortItems+1; i++ {
		items = append(items, map[string]interface{}{
			"target_type": 1,
			"target_id":   "dm-" + strconvI(i),
			"sort":        i,
		})
	}
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items":   items,
		"version": 0,
	})
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "items 太多")
}

func TestFollow_UpdateSort_EmptyTargetID_Rejected(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			t.Fatal("DB.UpdateSort must NOT be called when target_id is empty")
			return nil
		},
	})
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "target_id")
}

func TestFollow_UpdateSort_DuplicateItems_Rejected(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			t.Fatal("DB.UpdateSort must NOT be called when payload has duplicate items")
			return nil
		},
	})
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
			{"target_type": 1, "target_id": "dm-1", "sort": 2},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "重复")
}

// PR #21 Round-4 review I5 (lml2468)：ErrSortTargetNotFound 必须作为独立业务错误
// 暴露给客户端（swagger 已承诺），不能再吞成通用 "更新排序失败"。
func TestFollow_UpdateSort_TargetNotFound_DistinctError(t *testing.T) {
	db := &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			return ErrSortTargetNotFound
		},
	}
	r := newTestRouter(&stubService{}, db)
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "sort target not found")
}

// ---------------------------------------------------------------------------
// Issue #151 code review #1 — UpdateSort handler must run the default-followed
// group materialization gate BEFORE db.UpdateSort, with target_type=2 group
// IDs extracted from the payload.
// ---------------------------------------------------------------------------

func TestFollow_UpdateSort_CallsAuthorizeBeforeDB(t *testing.T) {
	var sawAuthorize bool
	var gotCandidates []string
	var sawDB bool
	svc := &stubService{
		authorizeAndMaterializeFn: func(uid, spaceID string, candidates []string) error {
			sawAuthorize = true
			gotCandidates = candidates
			assert.False(t, sawDB,
				"AuthorizeAndMaterializeDefaultFollowedGroups must run BEFORE "+
					"db.UpdateSort so any materialization is committed before the "+
					"sort tx tries to lock the rows")
			return nil
		},
	}
	db := &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			sawDB = true
			return nil
		},
	}
	r := newTestRouter(svc, db)
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
			{"target_type": 2, "target_id": "grp-a", "sort": 2},
			{"target_type": 5, "target_id": "grp-a____thr-1", "sort": 3},
			{"target_type": 2, "target_id": "grp-b", "sort": 4},
		},
		"version": 0,
	})
	assertOK(t, w)
	assert.True(t, sawAuthorize, "handler must invoke the authorize gate")
	assert.True(t, sawDB, "handler must invoke db.UpdateSort after authorize")
	assert.ElementsMatch(t, []string{"grp-a", "grp-b"}, gotCandidates,
		"only target_type=2 items must be forwarded to the gate — DM and thread "+
			"types follow strict ext-row semantics enforced by db.UpdateSort")
}

func TestFollow_UpdateSort_NoGroupCandidates_SkipsAuthorize(t *testing.T) {
	authorizeCalled := false
	svc := &stubService{
		authorizeAndMaterializeFn: func(uid, spaceID string, candidates []string) error {
			authorizeCalled = true
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
		},
		"version": 0,
	})
	assertOK(t, w)
	assert.False(t, authorizeCalled,
		"payloads with no target_type=2 items must skip the gate to avoid an "+
			"unnecessary DB round-trip")
}

func TestFollow_UpdateSort_AuthorizeError_FailsBeforeDB(t *testing.T) {
	dbCalled := false
	svc := &stubService{
		authorizeAndMaterializeFn: func(uid, spaceID string, candidates []string) error {
			return errors.New("guard failure")
		},
	}
	db := &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int64) error {
			dbCalled = true
			return nil
		},
	}
	r := newTestRouter(svc, db)
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 2, "target_id": "grp-x", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
	assert.False(t, dbCalled,
		"db.UpdateSort must NOT run when the authorize step failed; otherwise "+
			"a partial materialization could land before the sort error surfaces")
}

// ---------------------------------------------------------------------------
// NewFollow and space_id guard
// ---------------------------------------------------------------------------

// newTestRouterNoSpace builds a router where space_id is NOT injected into
// the context so we can verify handlers return 400 for missing space_id.
func newTestRouterNoSpace(svc followService, db sortDB) *wkhttp.WKHttp {
	gin.SetMode(gin.TestMode)
	r := wkhttp.New()
	f := NewFollow(svc, db)

	// Only inject uid, NOT space_id.
	inject := func(c *wkhttp.Context) {
		c.Set("uid", "test-uid")
		c.Next()
	}

	grp := r.Group("/v1/follow", inject)
	grp.POST("/dm", f.FollowDM)
	grp.DELETE("/dm", f.UnfollowDM)
	grp.POST("/channel/unfollow", f.UnfollowChannel)
	grp.POST("/channel/refollow", f.FollowChannel)
	grp.POST("/thread", f.FollowThread)
	grp.DELETE("/thread", f.UnfollowThread)
	grp.PUT("/sort", f.UpdateSort)

	return r
}

func TestFollow_FollowDM_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v1/follow/dm", map[string]interface{}{"peer_uid": "peer1"})
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "space_id")
}

func TestFollow_UnfollowDM_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "DELETE", "/v1/follow/dm?peer_uid=p", nil)
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "space_id")
}

func TestFollow_UnfollowChannel_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/unfollow", map[string]interface{}{"group_no": "g"})
	assertBadRequest(t, w)
}

func TestFollow_FollowChannel_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v1/follow/channel/refollow", map[string]interface{}{"group_no": "g"})
	assertBadRequest(t, w)
}

func TestFollow_FollowThread_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v1/follow/thread", map[string]interface{}{"thread_channel_id": "g____t"})
	assertBadRequest(t, w)
}

func TestFollow_UnfollowThread_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "DELETE", "/v1/follow/thread?thread_channel_id=g____t", nil)
	assertBadRequest(t, w)
}

func TestFollow_UpdateSort_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "PUT", "/v1/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
}
