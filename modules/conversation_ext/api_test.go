package conversation_ext

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Test doubles — in-process stubs that satisfy the Follow handler's needs
// without touching MySQL.
// ---------------------------------------------------------------------------

// stubService is a test double for *Service.
type stubService struct {
	followDMFn        func(uid, spaceID, peerUID string, categoryID *int64) error
	unfollowDMFn      func(uid, spaceID, peerUID string) error
	unfollowChannelFn func(uid, spaceID, groupNo string) error
	followChannelFn   func(uid, spaceID, groupNo string) error
	followThreadFn    func(uid, spaceID, threadChannelID string) error
	unfollowThreadFn  func(uid, spaceID, threadChannelID string) error
}

func (s *stubService) FollowDM(uid, spaceID, peerUID string, categoryID *int64) error {
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

// stubDB is a test double for *DB (only UpdateSort is needed).
type stubDB struct {
	updateSortFn func(uid, spaceID string, items []SortItem, version int) error
}

func (d *stubDB) UpdateSort(uid, spaceID string, items []SortItem, version int) error {
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

	grp := r.Group("/v2/follow", inject)
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
	var gotCatID *int64

	svc := &stubService{
		followDMFn: func(uid, spaceID, peerUID string, categoryID *int64) error {
			gotUID, gotSpaceID, gotPeerUID, gotCatID = uid, spaceID, peerUID, categoryID
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v2/follow/dm", map[string]interface{}{"peer_uid": "peer1"})

	assertOK(t, w)
	assert.Equal(t, "test-uid", gotUID)
	assert.Equal(t, "test-space", gotSpaceID)
	assert.Equal(t, "peer1", gotPeerUID)
	assert.Nil(t, gotCatID)
}

func TestFollow_FollowDM_WithCategoryID(t *testing.T) {
	var gotCatID *int64
	svc := &stubService{
		followDMFn: func(uid, spaceID, peerUID string, categoryID *int64) error {
			gotCatID = categoryID
			return nil
		},
	}
	r := newTestRouter(svc, &stubDB{})
	catID := int64(99)
	w := do(r, "POST", "/v2/follow/dm", map[string]interface{}{
		"peer_uid":    "peer2",
		"category_id": catID,
	})

	assertOK(t, w)
	require.NotNil(t, gotCatID)
	assert.Equal(t, catID, *gotCatID)
}

func TestFollow_FollowDM_MissingPeerUID(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v2/follow/dm", map[string]interface{}{})
	assertBadRequest(t, w)
}

func TestFollow_FollowDM_ServiceError(t *testing.T) {
	svc := &stubService{
		followDMFn: func(uid, spaceID, peerUID string, categoryID *int64) error {
			return errors.New("db gone away")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v2/follow/dm", map[string]interface{}{"peer_uid": "peer1"})
	assertBadRequest(t, w)
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
	w := do(r, "DELETE", "/v2/follow/dm?peer_uid=peerX", nil)

	assertOK(t, w)
	assert.Equal(t, "peerX", gotPeerUID)
}

func TestFollow_UnfollowDM_MissingPeerUID(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "DELETE", "/v2/follow/dm", nil)
	assertBadRequest(t, w)
}

func TestFollow_UnfollowDM_ServiceError(t *testing.T) {
	svc := &stubService{
		unfollowDMFn: func(uid, spaceID, peerUID string) error {
			return errors.New("gone")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "DELETE", "/v2/follow/dm?peer_uid=p", nil)
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
	w := do(r, "POST", "/v2/follow/channel/unfollow", map[string]interface{}{"group_no": "grp1"})

	assertOK(t, w)
	assert.Equal(t, "grp1", gotGroupNo)
}

func TestFollow_UnfollowChannel_MissingGroupNo(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v2/follow/channel/unfollow", map[string]interface{}{})
	assertBadRequest(t, w)
}

func TestFollow_UnfollowChannel_ServiceError(t *testing.T) {
	svc := &stubService{
		unfollowChannelFn: func(uid, spaceID, groupNo string) error {
			return errors.New("oops")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v2/follow/channel/unfollow", map[string]interface{}{"group_no": "grp1"})
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
	w := do(r, "POST", "/v2/follow/channel/refollow", map[string]interface{}{"group_no": "grp2"})

	assertOK(t, w)
	assert.Equal(t, "grp2", gotGroupNo)
}

func TestFollow_FollowChannel_MissingGroupNo(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v2/follow/channel/refollow", map[string]interface{}{})
	assertBadRequest(t, w)
}

func TestFollow_FollowChannel_ServiceError(t *testing.T) {
	svc := &stubService{
		followChannelFn: func(uid, spaceID, groupNo string) error {
			return errors.New("db error")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v2/follow/channel/refollow", map[string]interface{}{"group_no": "grp2"})
	assertBadRequest(t, w)
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
	w := do(r, "POST", "/v2/follow/thread", map[string]interface{}{"thread_channel_id": "grp1____thr1"})

	assertOK(t, w)
	assert.Equal(t, "grp1____thr1", gotThreadID)
}

func TestFollow_FollowThread_MissingThreadChannelID(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v2/follow/thread", map[string]interface{}{})
	assertBadRequest(t, w)
}

func TestFollow_FollowThread_ServiceError(t *testing.T) {
	svc := &stubService{
		followThreadFn: func(uid, spaceID, threadChannelID string) error {
			return errors.New("tx failed")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "POST", "/v2/follow/thread", map[string]interface{}{"thread_channel_id": "grp1____thr1"})
	assertBadRequest(t, w)
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
	w := do(r, "DELETE", "/v2/follow/thread?thread_channel_id=grp1____thr2", nil)

	assertOK(t, w)
	assert.Equal(t, "grp1____thr2", gotThreadID)
}

func TestFollow_UnfollowThread_MissingThreadChannelID(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "DELETE", "/v2/follow/thread", nil)
	assertBadRequest(t, w)
}

func TestFollow_UnfollowThread_ServiceError(t *testing.T) {
	svc := &stubService{
		unfollowThreadFn: func(uid, spaceID, threadChannelID string) error {
			return errors.New("delete failed")
		},
	}
	r := newTestRouter(svc, &stubDB{})
	w := do(r, "DELETE", "/v2/follow/thread?thread_channel_id=grp1____thr2", nil)
	assertBadRequest(t, w)
}

// ---------------------------------------------------------------------------
// UpdateSort (CAS)
// ---------------------------------------------------------------------------

func TestFollow_UpdateSort_HappyPath(t *testing.T) {
	var gotItems []SortItem
	var gotVersion int
	db := &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int) error {
			gotItems = items
			gotVersion = version
			return nil
		},
	}
	r := newTestRouter(&stubService{}, db)
	w := do(r, "PUT", "/v2/follow/sort", map[string]interface{}{
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
	assert.Equal(t, 3, gotVersion)
}

func TestFollow_UpdateSort_MissingItems(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "PUT", "/v2/follow/sort", map[string]interface{}{
		"items":   []interface{}{},
		"version": 0,
	})
	assertBadRequest(t, w)
}

func TestFollow_UpdateSort_InvalidTargetType(t *testing.T) {
	r := newTestRouter(&stubService{}, &stubDB{})
	w := do(r, "PUT", "/v2/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 99, "target_id": "x", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
}

func TestFollow_UpdateSort_CASConflict(t *testing.T) {
	db := &stubDB{
		updateSortFn: func(uid, spaceID string, items []SortItem, version int) error {
			return ErrVersionConflict
		},
	}
	r := newTestRouter(&stubService{}, db)
	w := do(r, "PUT", "/v2/follow/sort", map[string]interface{}{
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
		updateSortFn: func(uid, spaceID string, items []SortItem, version int) error {
			return errors.New("connection reset")
		},
	}
	r := newTestRouter(&stubService{}, db)
	w := do(r, "PUT", "/v2/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
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

	grp := r.Group("/v2/follow", inject)
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
	w := do(r, "POST", "/v2/follow/dm", map[string]interface{}{"peer_uid": "peer1"})
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "space_id")
}

func TestFollow_UnfollowDM_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "DELETE", "/v2/follow/dm?peer_uid=p", nil)
	assertBadRequest(t, w)
	assert.Contains(t, w.Body.String(), "space_id")
}

func TestFollow_UnfollowChannel_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v2/follow/channel/unfollow", map[string]interface{}{"group_no": "g"})
	assertBadRequest(t, w)
}

func TestFollow_FollowChannel_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v2/follow/channel/refollow", map[string]interface{}{"group_no": "g"})
	assertBadRequest(t, w)
}

func TestFollow_FollowThread_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "POST", "/v2/follow/thread", map[string]interface{}{"thread_channel_id": "g____t"})
	assertBadRequest(t, w)
}

func TestFollow_UnfollowThread_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "DELETE", "/v2/follow/thread?thread_channel_id=g____t", nil)
	assertBadRequest(t, w)
}

func TestFollow_UpdateSort_MissingSpaceID(t *testing.T) {
	r := newTestRouterNoSpace(&stubService{}, &stubDB{})
	w := do(r, "PUT", "/v2/follow/sort", map[string]interface{}{
		"items": []map[string]interface{}{
			{"target_type": 1, "target_id": "dm-1", "sort": 1},
		},
		"version": 0,
	})
	assertBadRequest(t, w)
}
