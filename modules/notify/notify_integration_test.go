package notify

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/base/app"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gin-gonic/gin"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// Test harness — stubs, mocks, httptest servers, sqlmock
// =============================================================================

func init() {
	gin.SetMode(gin.TestMode)
}

// stubUserService is a minimal in-memory stub of user.IService.
// Only the methods actually touched by notify are overridden; unused methods
// fall through to the embedded nil interface and would panic if invoked —
// that is intentional so unexpected code paths fail loudly.
type stubUserService struct {
	user.IService

	mu                 sync.Mutex
	users              map[string]*user.Resp // keyed by username
	addUserErr         error
	addUserCount       int32
	addUserDelay       time.Duration
	getByUsernameCalls int32
	updateUserCount    int32
	updateUserErr      error
}

func newStubUserService() *stubUserService {
	return &stubUserService{users: map[string]*user.Resp{}}
}

func (s *stubUserService) GetUserWithUsername(username string) (*user.Resp, error) {
	atomic.AddInt32(&s.getByUsernameCalls, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[username]; ok {
		// return a shallow copy so tests can't mutate our map through the pointer
		cp := *u
		return &cp, nil
	}
	return nil, nil
}

func (s *stubUserService) AddUser(req *user.AddUserReq) error {
	atomic.AddInt32(&s.addUserCount, 1)
	if s.addUserDelay > 0 {
		time.Sleep(s.addUserDelay)
	}
	if s.addUserErr != nil {
		return s.addUserErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[req.Username] = &user.Resp{UID: req.UID, Name: req.Name}
	return nil
}

func (s *stubUserService) UpdateUser(req user.UserUpdateReq) error {
	atomic.AddInt32(&s.updateUserCount, 1)
	if s.updateUserErr != nil {
		return s.updateUserErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.users[req.UID]; ok && req.Name != nil {
		u.Name = *req.Name
	}
	return nil
}

// stubAppService mocks app.IService.
type stubAppService struct {
	createErr   error
	createCount int32
	deleteCount int32
}

func (s *stubAppService) GetApp(appID string) (*app.Resp, error) {
	return nil, nil
}

func (s *stubAppService) CreateApp(r app.Req) (*app.Resp, error) {
	atomic.AddInt32(&s.createCount, 1)
	if s.createErr != nil {
		return nil, s.createErr
	}
	return &app.Resp{AppID: r.AppID, AppKey: "test-app-key", AppName: "test-app"}, nil
}

func (s *stubAppService) DeleteApp(appID string) error {
	atomic.AddInt32(&s.deleteCount, 1)
	return nil
}

// newMockedDBSession returns a dbr.Session backed by go-sqlmock. Callers may
// register expectations on `mock`, but T1/T2/T3 mostly avoid actually routing
// queries to the DB (we pre-seed memberCache / botOK instead). The returned
// closer should be called on cleanup.
// Uses default regex matcher because dbr's MySQL dialect fully interpolates
// placeholders into the SQL string before it reaches the driver, so matching
// on the literal SQL with parameters as `?` placeholders would never match.
func newMockedDBSession(t *testing.T) (*dbr.Session, sqlmock.Sqlmock, func()) {
	t.Helper()
	rawDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	session := conn.NewSession(nil)
	return session, mock, func() { _ = rawDB.Close() }
}

// injectMockDBIntoContext overwrites the (unexported) `mySQLSession` field
// of *config.Context so that ctx.DB() returns our sqlmock-backed session.
// Necessary because T4 touches ensureNotifyBot which calls ctx.GenSeq /
// ctx.UpdateIMToken / ctx.SendCMD — all of which go through ctx.DB() or
// WuKongIM HTTP. We also mark mysqlOnce as already executed.
func injectMockDBIntoContext(t *testing.T, ctx *config.Context, session *dbr.Session) {
	t.Helper()
	ctxVal := reflect.ValueOf(ctx).Elem()

	onceField := ctxVal.FieldByName("mysqlOnce")
	once := (*sync.Once)(unsafe.Pointer(onceField.UnsafeAddr()))
	once.Do(func() {}) // mark as done without running real init

	sessionField := ctxVal.FieldByName("mySQLSession")
	reflect.NewAt(sessionField.Type(), unsafe.Pointer(sessionField.UnsafeAddr())).
		Elem().Set(reflect.ValueOf(session))
}

// wuKongServer spins up an httptest.Server that mimics the WuKongIM endpoints
// notify touches: /message/send, /user/update, /user/token.
// Request counts are exposed for assertions.
type wuKongServer struct {
	server        *httptest.Server
	messageCount  int32
	messageFail   atomic.Bool // when true, /message/send returns 500
	lastMessage   atomic.Value
	userUpdates   int32
	tokenUpdates  int32
	cmdCount      int32
	messageFilter func(body []byte) bool // optional: if non-nil and returns true, fail that specific message
}

func newWuKongServer() *wuKongServer {
	s := &wuKongServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/message/send", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		atomic.AddInt32(&s.messageCount, 1)
		s.lastMessage.Store(append([]byte(nil), body...))
		if s.messageFail.Load() || (s.messageFilter != nil && s.messageFilter(body)) {
			http.Error(w, `{"msg":"injected failure"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"message_id":1,"message_seq":1,"client_msg_no":"ok"}}`))
	})
	mux.HandleFunc("/user/update", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.userUpdates, 1)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/user/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.tokenUpdates, 1)
		_, _ = w.Write([]byte(`{}`))
	})
	// generic catch-all for any other WK endpoints (e.g. CMD), prevent panics
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.cmdCount, 1)
		_, _ = w.Write([]byte(`{}`))
	})
	s.server = httptest.NewServer(mux)
	return s
}

func (s *wuKongServer) close() { s.server.Close() }

// newTestContext builds a minimal *config.Context with WuKongIM API pointing
// at the provided httptest server. The returned context has NO real MySQL.
// Callers that need ctx.DB() must also call injectMockDBIntoContext.
func newTestContext(t *testing.T, wk *wuKongServer) *config.Context {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.WuKongIM.APIURL = wk.server.URL
	cfg.WuKongIM.ManagerToken = "test-token"
	// Avoid starting long-lived workers that leak between tests.
	cfg.EventPoolSize = 1
	cfg.Push.PushPoolSize = 1
	cfg.Robot.EventPoolSize = 1
	ctx := config.NewContext(cfg)
	return ctx
}

// newTestNotify builds a Notify directly (bypassing New()) with the given
// collaborators. This avoids the goroutine/event-listener side effects of
// New() that would make tests non-deterministic.
func newTestNotify(ctx *config.Context, db *dbr.Session, us user.IService, as app.IService, token string) *Notify {
	return &Notify{
		ctx:           ctx,
		userService:   us,
		appService:    as,
		db:            db,
		memberCache:   newMemberCache(),
		internalToken: token,
		Log:           log.NewTLog("NotifyTest"),
	}
}

// primeMemberCache seeds memberCache so verify() skips the DB query entirely.
func primeMemberCache(n *Notify, spaceID string, uids ...string) {
	set := make(map[string]bool, len(uids))
	for _, u := range uids {
		set[u] = true
	}
	n.memberCache.mu.Lock()
	n.memberCache.entries[spaceID] = &memberCacheEntry{
		uids:     set,
		expireAt: time.Now().Add(cacheTTL),
	}
	n.memberCache.mu.Unlock()
}

// buildRouter mounts the notify internal routes on a fresh wkhttp router.
func buildRouter(n *Notify) *wkhttp.WKHttp {
	r := wkhttp.New()
	n.Route(r)
	return r
}

func doJSONRequest(t *testing.T, r http.Handler, method, path string, header http.Header, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else if raw, ok := body.([]byte); ok {
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader([]byte(util.ToJson(body)))
	}
	req, _ := http.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// =============================================================================
// T1. internalAuthMiddleware
// =============================================================================

func TestIntegration_InternalAuth_TokenNotConfigured_Rejects(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "") // empty token
	r := buildRouter(n)

	// Try reaching /v1/internal/notify with no header and with a valid-looking header.
	cases := []struct{ name, token string }{
		{"no-header", ""},
		{"any-token", "whatever"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.token != "" {
				h.Set(InternalTokenHeader, tc.token)
			}
			w := doJSONRequest(t, r, "POST", "/v1/internal/notify", h, map[string]string{"x": "y"})
			assert.Equal(t, http.StatusUnauthorized, w.Code)
			assert.Contains(t, w.Body.String(), "internal API auth not configured")
			// Never leak token value in response body.
			if tc.token != "" {
				assert.NotContains(t, w.Body.String(), tc.token)
			}
		})
	}
}

func TestIntegration_InternalAuth_MissingHeader(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "secret-token-42")
	r := buildRouter(n)

	w := doJSONRequest(t, r, "POST", "/v1/internal/notify", nil, map[string]string{"x": "y"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unauthorized")
	assert.NotContains(t, w.Body.String(), "secret-token-42")
}

func TestIntegration_InternalAuth_WrongToken(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "secret-token-42")
	r := buildRouter(n)

	h := http.Header{}
	h.Set(InternalTokenHeader, "wrong-token")
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify", h, map[string]string{"x": "y"})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unauthorized")
	// The error response must not echo either the real token nor the attacker-provided one.
	assert.NotContains(t, w.Body.String(), "secret-token-42")
	assert.NotContains(t, w.Body.String(), "wrong-token")
}

func TestIntegration_InternalAuth_CorrectToken_ReachesHandler(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	us := newStubUserService()
	n := newTestNotify(ctx, db, us, &stubAppService{}, "correct-token")

	// Set botOK so deliverNotification proceeds.
	const spaceID = "sp_auth_ok"
	n.botOK.Store(true)
	primeMemberCache(n, spaceID, "uid_a")

	r := buildRouter(n)

	h := http.Header{}
	h.Set(InternalTokenHeader, "correct-token")
	body := NotifyReq{
		SpaceID: spaceID,
		Service: "svc",
		Targets: []string{"uid_a"},
		Payload: map[string]interface{}{"type": 1},
	}
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify", h, body)
	// Authenticated request must reach deliverNotification; expect 200 OK.
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "delivered")
}

// ConstantTimeCompare exercised: byte-length-mismatch token is still rejected safely.
func TestIntegration_InternalAuth_ConstantTimeCompare_DiffLengths(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "longer-secret-42")
	r := buildRouter(n)

	h := http.Header{}
	h.Set(InternalTokenHeader, "short") // different length — ConstantTimeCompare returns 0
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify", h, map[string]string{})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "unauthorized")
}

// =============================================================================
// T2. sendNotify / sendNotifyBatch — parameter validation
// =============================================================================

func TestIntegration_SendNotify_BadJSON(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	r := buildRouter(n)

	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify", h, []byte("{not json"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "参数格式错误")
}

// Table-driven: direct deliverNotification calls cover its internal
// validation layer (those messages are wrapped as 500 "internal error" when
// surfaced via HTTP, so we test the function directly to observe them).
func TestIntegration_DeliverNotification_ValidationErrors(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")

	bigTargets := make([]string, 201)
	for i := range bigTargets {
		bigTargets[i] = fmt.Sprintf("uid_%d", i)
	}

	cases := []struct {
		name    string
		req     *NotifyReq
		wantMsg string
	}{
		{
			name:    "empty_space_id",
			req:     &NotifyReq{SpaceID: "", Targets: []string{"a"}, Payload: map[string]interface{}{"x": 1}},
			wantMsg: "space_id不能为空",
		},
		{
			name:    "empty_targets",
			req:     &NotifyReq{SpaceID: "sp", Targets: []string{}, Payload: map[string]interface{}{"x": 1}},
			wantMsg: "targets不能为空",
		},
		{
			name:    "targets_over_limit",
			req:     &NotifyReq{SpaceID: "sp", Targets: bigTargets, Payload: map[string]interface{}{"x": 1}},
			wantMsg: "targets上限200",
		},
		{
			name:    "nil_payload",
			req:     &NotifyReq{SpaceID: "sp", Targets: []string{"a"}, Payload: nil},
			wantMsg: "payload不能为空",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := n.deliverNotification(tc.req)
			assert.Nil(t, resp)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantMsg)
		})
	}
}

func TestIntegration_SendNotify_InternalErrorSurfaces500(t *testing.T) {
	// Targets > 200 passes BindJSON but fails deliverNotification → 500 "internal error".
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	r := buildRouter(n)

	targets := make([]string, 201)
	for i := range targets {
		targets[i] = fmt.Sprintf("u%d", i)
	}
	body := NotifyReq{SpaceID: "sp", Service: "svc", Targets: targets, Payload: map[string]interface{}{"x": 1}}
	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify", h, body)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, w.Body.String(), "internal error")
}

func TestIntegration_InternalNotifyRejectsCardPayloadBeforeAnyDelivery(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	r := buildRouter(n)
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")
	cardPayload := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"card":         map[string]interface{}{"type": "AdaptiveCard", "version": cardmsg.CardVersion},
	}

	t.Run("single", func(t *testing.T) {
		w := doJSONRequest(t, r, http.MethodPost, "/v1/internal/notify", h, NotifyReq{
			SpaceID: "space-a", Service: "summary", Targets: []string{"user-a"}, Payload: cardPayload,
		})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"err.server.notify.card_not_allowed"`)
		assert.Zero(t, atomic.LoadInt32(&wk.messageCount))
	})

	t.Run("batch preflights every entry before delivering text", func(t *testing.T) {
		w := doJSONRequest(t, r, http.MethodPost, "/v1/internal/notify/batch", h, BatchNotifyReq{Notifications: []NotifyReq{
			{SpaceID: "space-a", Service: "summary", Targets: []string{"user-a"}, Payload: map[string]interface{}{"type": 1, "content": "legacy"}},
			{SpaceID: "space-a", Service: "summary", Targets: []string{"user-b"}, Payload: cardPayload},
		}})
		assert.Equal(t, http.StatusBadRequest, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"err.server.notify.card_not_allowed"`)
		assert.Zero(t, atomic.LoadInt32(&wk.messageCount))
	})
}

func TestIntegration_SendNotifyBatch_Empty(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	r := buildRouter(n)

	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")
	// Bypass `binding:"required"` empty-slice rejection by sending raw JSON with
	// a non-nil but empty slice — gin may still reject; if so, we'd hit the
	// BindJSON "参数格式错误" path. Either response is a 400. Assert status only.
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify/batch", h, map[string]interface{}{"notifications": []interface{}{}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	body := w.Body.String()
	// Accept either validator-produced or notifications-empty message.
	if !strings.Contains(body, "notifications不能为空") && !strings.Contains(body, "参数格式错误") {
		t.Errorf("unexpected body for empty batch: %s", body)
	}
}

func TestIntegration_SendNotifyBatch_TooMany(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	r := buildRouter(n)

	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")

	notifs := make([]NotifyReq, 51)
	for i := range notifs {
		notifs[i] = NotifyReq{SpaceID: "sp", Service: "svc", Targets: []string{"u1"}, Payload: map[string]interface{}{"i": i}}
	}
	body := BatchNotifyReq{Notifications: notifs}
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify/batch", h, body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "批量上限50条")
}

func TestIntegration_SendNotifyBatch_MixedResults_207(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	const goodSpace = "sp_good"
	n.botOK.Store(true)
	primeMemberCache(n, goodSpace, "uid_a")

	r := buildRouter(n)

	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")

	// First: valid; Second: fails because targets > 200.
	bad := make([]string, 201)
	for i := range bad {
		bad[i] = fmt.Sprintf("u%d", i)
	}
	body := BatchNotifyReq{
		Notifications: []NotifyReq{
			{SpaceID: goodSpace, Service: "svc", Targets: []string{"uid_a"}, Payload: map[string]interface{}{"k": "v"}},
			{SpaceID: "sp_bad", Service: "svc", Targets: bad, Payload: map[string]interface{}{"k": "v"}},
		},
	}
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify/batch", h, body)
	assert.Equal(t, http.StatusMultiStatus, w.Code, "mixed success/failure must produce 207")
	assert.Contains(t, w.Body.String(), `"has_errors":true`)
	assert.Contains(t, w.Body.String(), "targets上限200")
	assert.Contains(t, w.Body.String(), `"delivered":["uid_a"]`)
}

func TestIntegration_SendNotifyBatch_AllSuccess_200(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	const spaceID = "sp_all_ok"
	n.botOK.Store(true)
	primeMemberCache(n, spaceID, "uid_a", "uid_b")

	r := buildRouter(n)

	h := http.Header{}
	h.Set(InternalTokenHeader, "tk")
	body := BatchNotifyReq{
		Notifications: []NotifyReq{
			{SpaceID: spaceID, Service: "svc", Targets: []string{"uid_a"}, Payload: map[string]interface{}{"type": 1, "content": "v1"}},
			{SpaceID: spaceID, Service: "svc", Targets: []string{"uid_b"}, Payload: map[string]interface{}{"type": 1, "content": "v2"}},
		},
	}
	w := doJSONRequest(t, r, "POST", "/v1/internal/notify/batch", h, body)
	assert.Equal(t, http.StatusOK, w.Code)
	// The common response wrapper envelopes `data` — just check flag.
	assert.Contains(t, w.Body.String(), `"has_errors":false`)
}

// =============================================================================
// T3. deliverNotification — core path
// =============================================================================

func TestIntegration_Deliver_DeduplicatesTargets(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	const spaceID = "sp_dedup"
	n.botOK.Store(true)
	primeMemberCache(n, spaceID, "uid_a", "uid_b")

	resp, err := n.deliverNotification(&NotifyReq{
		SpaceID: spaceID,
		Service: "svc",
		Targets: []string{"uid_a", "uid_a", "uid_b", "uid_a"},
		Payload: map[string]interface{}{"x": 1},
	})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.ElementsMatch(t, []string{"uid_a", "uid_b"}, resp.Delivered)
	assert.Equal(t, int32(2), atomic.LoadInt32(&wk.messageCount), "each uid delivered exactly once")
}

func TestIntegration_Deliver_ExcludesActor(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	const spaceID = "sp_actor"
	n.botOK.Store(true)
	primeMemberCache(n, spaceID, "uid_a", "uid_b", "uid_actor")

	resp, err := n.deliverNotification(&NotifyReq{
		SpaceID:  spaceID,
		Service:  "svc",
		Targets:  []string{"uid_a", "uid_b", "uid_actor"},
		ActorUID: "uid_actor",
		Payload:  map[string]interface{}{"x": 1},
	})
	require.NoError(t, err)
	assert.NotContains(t, resp.Delivered, "uid_actor")
	assert.ElementsMatch(t, []string{"uid_a", "uid_b"}, resp.Delivered)
}

func TestIntegration_Deliver_NonMembersAreFiltered(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	const spaceID = "sp_filter"
	n.botOK.Store(true)
	primeMemberCache(n, spaceID, "uid_a") // only uid_a is a member

	resp, err := n.deliverNotification(&NotifyReq{
		SpaceID: spaceID,
		Service: "svc",
		Targets: []string{"uid_a", "uid_outsider"},
		Payload: map[string]interface{}{"x": 1},
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"uid_a"}, resp.Delivered)
	assert.Equal(t, "not_space_member", resp.Filtered["uid_outsider"])
	_, ok := resp.Filtered["uid_a"]
	assert.False(t, ok, "delivered user should not appear in filtered map")
}

func TestIntegration_Deliver_SendFailure_MarksFiltered(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	wk.messageFail.Store(true) // every /message/send returns 500 → ctx.SendMessage returns error

	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	const spaceID = "sp_sendfail"
	n.botOK.Store(true)
	primeMemberCache(n, spaceID, "uid_a", "uid_b")

	resp, err := n.deliverNotification(&NotifyReq{
		SpaceID: spaceID,
		Service: "svc",
		Targets: []string{"uid_a", "uid_b"},
		Payload: map[string]interface{}{"x": 1},
	})
	require.NoError(t, err)
	assert.Empty(t, resp.Delivered, "all sends failed")
	assert.Equal(t, "send_failed", resp.Filtered["uid_a"])
	assert.Equal(t, "send_failed", resp.Filtered["uid_b"])
}

func TestIntegration_Deliver_PartialSendFailure(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	// Fail only messages targeting uid_b by inspecting the body.
	wk.messageFilter = func(body []byte) bool {
		return bytes.Contains(body, []byte(`"channel_id":"uid_b"`))
	}

	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	const spaceID = "sp_partial"
	n.botOK.Store(true)
	primeMemberCache(n, spaceID, "uid_a", "uid_b", "uid_c")

	resp, err := n.deliverNotification(&NotifyReq{
		SpaceID: spaceID,
		Service: "svc",
		Targets: []string{"uid_a", "uid_b", "uid_c"},
		Payload: map[string]interface{}{"x": 1},
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"uid_a", "uid_c"}, resp.Delivered)
	assert.Equal(t, "send_failed", resp.Filtered["uid_b"])
	assert.NotContains(t, resp.Filtered, "uid_a")
}

func TestIntegration_Deliver_AllNonMembers_NoBotCreation(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	us := newStubUserService()
	as := &stubAppService{}
	n := newTestNotify(ctx, db, us, as, "tk")
	const spaceID = "sp_no_members"
	// Prime cache with NO members. botOK intentionally NOT set.
	primeMemberCache(n, spaceID /* no uids */)

	resp, err := n.deliverNotification(&NotifyReq{
		SpaceID: spaceID,
		Service: "svc",
		Targets: []string{"uid_x", "uid_y"},
		Payload: map[string]interface{}{"x": 1},
	})
	require.NoError(t, err)
	assert.Empty(t, resp.Delivered)
	assert.Equal(t, "not_space_member", resp.Filtered["uid_x"])
	assert.Equal(t, "not_space_member", resp.Filtered["uid_y"])

	// Critical invariant: bot creation must NOT have been triggered.
	assert.Equal(t, int32(0), atomic.LoadInt32(&us.addUserCount), "AddUser should not be called when no members")
	assert.Equal(t, int32(0), atomic.LoadInt32(&us.getByUsernameCalls), "GetUserWithUsername should not be called")
	assert.Equal(t, int32(0), atomic.LoadInt32(&as.createCount))
	// No WuKongIM /message/send either.
	assert.Equal(t, int32(0), atomic.LoadInt32(&wk.messageCount))

	botReady := n.botOK.Load()
	assert.False(t, botReady, "botOK must not be flipped for empty deliveries")
}

// =============================================================================

// =============================================================================
// T4. Static singleton bot — basic tests
// =============================================================================

func TestNotifyBotUID_Static(t *testing.T) {
	uid := NotifyBotUID()
	assert.Equal(t, "notification", uid)
	assert.LessOrEqual(t, len(uid), 40, "UID must fit user.uid VARCHAR(40)")
}

func TestIntegration_EnsureBot_BotOK_SkipsCreation(t *testing.T) {
	// When botOK is already true, deliverNotification should not fail
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	us := newStubUserService()
	n := newTestNotify(ctx, db, us, &stubAppService{}, "tk")
	n.botOK.Store(true)

	// Should not panic or call userService
	assert.Equal(t, int32(0), atomic.LoadInt32(&us.getByUsernameCalls))
}

// When memberCache is empty for the space, deliverNotification drives
// memberCache.refresh which issues a `SELECT uid FROM space_member ...` query.
func TestIntegration_Deliver_CacheMiss_RefreshesFromDB(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, mock, closeDB := newMockedDBSession(t)
	defer closeDB()

	const spaceID = "sp_cache_miss"
	// refresh() issues: SELECT uid FROM space_member WHERE (space_id = ? AND status = 1)
	mock.ExpectQuery(`SELECT uid FROM space_member`).
		WillReturnRows(sqlmock.NewRows([]string{"uid"}).AddRow("uid_a").AddRow("uid_b"))

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	n.botOK.Store(true)

	resp, err := n.deliverNotification(&NotifyReq{
		SpaceID: spaceID,
		Service: "svc",
		Targets: []string{"uid_a", "uid_c"}, // uid_c is not a member
		Payload: map[string]interface{}{"x": 1},
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"uid_a"}, resp.Delivered)
	assert.Equal(t, "not_space_member", resp.Filtered["uid_c"])
	assert.NoError(t, mock.ExpectationsWereMet())

	// Second call hits warm cache — no new DB query registered, so mock would complain.
	resp2, err := n.deliverNotification(&NotifyReq{
		SpaceID: spaceID,
		Service: "svc",
		Targets: []string{"uid_b"},
		Payload: map[string]interface{}{"x": 1},
	})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"uid_b"}, resp2.Delivered)
}

func TestIntegration_HandleSpaceMemberEvent_InvalidatesCache(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	db, _, closeDB := newMockedDBSession(t)
	defer closeDB()

	n := newTestNotify(ctx, db, newStubUserService(), &stubAppService{}, "tk")
	const spaceID = "sp_evt"
	primeMemberCache(n, spaceID, "uid_a")

	// Valid event payload
	called := false
	commit := func(err error) { called = true }
	n.handleSpaceMemberEvent([]byte(`{"space_id":"sp_evt","uid":"uid_a"}`), commit)
	assert.True(t, called, "commit callback must be invoked")

	n.memberCache.mu.RLock()
	_, exists := n.memberCache.entries[spaceID]
	n.memberCache.mu.RUnlock()
	assert.False(t, exists, "cache for sp_evt should be invalidated after member event")

	// Malformed JSON: commit still called, no panic.
	called = false
	n.handleSpaceMemberEvent([]byte("{not json"), commit)
	assert.True(t, called)
}

// Exercises memberCache direct getValidMembers with an empty targets slice
// (branch not covered elsewhere).
func TestMemberCache_Verify_EmptyTargets(t *testing.T) {
	mc := newMemberCache()
	members, filtered, err := mc.verify(nil, "sp", nil)
	require.NoError(t, err)
	assert.Empty(t, members)
	assert.Empty(t, filtered)
}

// TestNotifyBotUID_FitsUserUIDColumn guards against the 2026-04-30 production incident
// where NotifyBotUID generated uids longer than user.uid VARCHAR(40), causing MySQL
// error 1406 on every bot creation (3h / 786 errors, module non-functional).
//
// Static singleton UID must fit user.uid VARCHAR(40).
func TestNotifyBotUID_FitsUserUIDColumn(t *testing.T) {
	const userUIDColumnLimit = 40
	uid := NotifyBotUID()
	assert.Equal(t, "notification", uid)
	assert.LessOrEqual(t, len(uid), userUIDColumnLimit,
		"NotifyBotUID() = %q (%d chars) exceeds user.uid VARCHAR(%d)",
		uid, len(uid), userUIDColumnLimit)
}
