package messages_search

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codex.mlamp.cn/dmwork/octo-auth-sdk/contract"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	"github.com/Mininglamp-OSS/octo-server/pkg/searchbackend"
	"github.com/olivere/elastic"
)

const (
	v2SearchUserID        = "token-user"
	v2SearchSpaceID       = "token-space"
	v2SearchBotID         = "bound-bot"
	v2SearchServiceSecret = "messages-search-test-service-secret-at-least-32-bytes"
)

func TestV2SearchUsesTokenUserAndBoundSpaceAndFiltersInvisibleHits(t *testing.T) {
	var (
		osMu       sync.Mutex
		osQuery    string
		osRequests int
	)
	loginUID := v2SearchUserID
	peerUID := "peer-user"
	fakeChannelID := fakeChannelIDFor(loginUID, peerUID)
	osServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/_search") {
			http.NotFound(w, r)
			return
		}
		query, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "cannot read search query", http.StatusBadRequest)
			return
		}
		osMu.Lock()
		osQuery = string(query)
		osRequests++
		osMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"hits":{"total":1,"max_score":1,"hits":[{"_index":"messages","_type":"_doc","_id":"401","_score":1,"_source":{"messageId":401,"messageSeq":7,"timestamp":1717000000000,"from":"sender","channelId":"`+fakeChannelID+`","channelType":1,"spaceId":"token-space","visibles":["bound-bot"],"payload":{"type":1,"text":{"content":"private needle"}}},"sort":[1717000000000,401]}]}}`)
	}))
	defer osServer.Close()
	installV2SearchESClient(t, osServer.URL)

	userSvc := &recordingV2SearchUserService{stubAuthzUserSvc: &stubAuthzUserSvc{
		spaceMembers: map[string]bool{
			spaceKey(v2SearchSpaceID, loginUID, peerUID): true,
		},
	}}
	probe := &stubProbe{}
	h := newV2SearchHTTPHandler(userSvc, probe, true)
	target := newV2SearchRouter(t, h, []string{"im.messages:read"})

	req := httptest.NewRequest(http.MethodPost, "/v2/cli/messages/_search", strings.NewReader(
		`{"channel_type":1,"channel_id":"`+peerUID+`","keyword":"needle","page_size":10}`,
	))
	req.Header.Set("Authorization", "Bearer business-token")
	// A client-supplied Space must never override the authenticated token's Space.
	req.Header.Set("X-Space-ID", "attacker-space")
	req.URL.RawQuery = "space_id=attacker-space"
	rec := httptest.NewRecorder()
	target.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("v2 search returned HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode search response %q: %v", rec.Body.String(), err)
	}
	if len(response.Data) != 0 {
		t.Fatalf("message not visible to token user must be filtered, got %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "private needle") {
		t.Fatalf("invisible search content leaked: %s", rec.Body.String())
	}
	spaceID, userID, observedPeerID := userSvc.observedAccess()
	if spaceID != v2SearchSpaceID || userID != loginUID || observedPeerID != peerUID {
		t.Fatalf("channel access was checked for space/user/peer %q/%q/%q, want %q/%q/%q",
			spaceID, userID, observedPeerID, v2SearchSpaceID, loginUID, peerUID)
	}
	osMu.Lock()
	query, requests := osQuery, osRequests
	osMu.Unlock()
	if requests != 1 {
		t.Fatalf("OpenSearch requests = %d, want 1", requests)
	}
	if !strings.Contains(query, `"spaceId":"`+v2SearchSpaceID+`"`) {
		t.Fatalf("OpenSearch DSL is not scoped to the Token Space %q: %s", v2SearchSpaceID, query)
	}
	if strings.Contains(query, "attacker-space") {
		t.Fatalf("client-supplied Space leaked into OpenSearch DSL: %s", query)
	}
}

func TestV2SearchRejectsMissingSearchAction(t *testing.T) {
	var (
		osMu       sync.Mutex
		osRequests int
	)
	osServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		osMu.Lock()
		osRequests++
		osMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"hits":{"total":0,"hits":[]}}`)
	}))
	defer osServer.Close()
	installV2SearchESClient(t, osServer.URL)

	h := newV2SearchHTTPHandler(&recordingV2SearchUserService{stubAuthzUserSvc: &stubAuthzUserSvc{}}, &stubProbe{}, true)
	target := newV2SearchRouter(t, h, []string{"im.messages:write"})
	req := httptest.NewRequest(http.MethodPost, "/v2/cli/messages/_search", strings.NewReader(
		`{"channel_type":1,"channel_id":"peer-user","keyword":"needle"}`,
	))
	req.Header.Set("Authorization", "Bearer business-token")
	rec := httptest.NewRecorder()
	target.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("search without im.messages:read must be forbidden, got HTTP %d: %s", rec.Code, rec.Body.String())
	}
	osMu.Lock()
	requests := osRequests
	osMu.Unlock()
	if requests != 0 {
		t.Fatalf("unauthorized search reached OpenSearch %d times", requests)
	}
}

func TestV2SearchAroundWrapsTheWindowInData(t *testing.T) {
	var (
		osMu       sync.Mutex
		osRequests int
	)
	loginUID := v2SearchUserID
	peerUID := "peer-user"
	fakeChannelID := fakeChannelIDFor(loginUID, peerUID)
	osServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		osMu.Lock()
		osRequests++
		requestNo := osRequests
		osMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if requestNo != 1 {
			_, _ = io.WriteString(w, `{"hits":{"total":0,"hits":[]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"hits":{"total":1,"max_score":1,"hits":[{"_index":"messages","_type":"_doc","_id":"401","_score":1,"_source":{"messageId":401,"messageSeq":7,"timestamp":1717000000000,"from":"sender","channelId":"`+fakeChannelID+`","channelType":1,"spaceId":"token-space","visibles":["token-user"],"payload":{"type":1,"text":{"content":"anchor text"}}},"sort":[1717000000000,401]}]}}`)
	}))
	defer osServer.Close()
	installV2SearchESClient(t, osServer.URL)

	userSvc := &recordingV2SearchUserService{stubAuthzUserSvc: &stubAuthzUserSvc{
		spaceMembers: map[string]bool{spaceKey(v2SearchSpaceID, loginUID, peerUID): true},
	}}
	h := newV2SearchHTTPHandler(userSvc, &stubProbe{}, true)
	target := newV2SearchRouter(t, h, []string{"im.messages:read"})
	req := httptest.NewRequest(http.MethodPost, "/v2/cli/messages/_search_around", strings.NewReader(
		`{"channel_type":1,"channel_id":"`+peerUID+`","anchor_message_id":"401","page_size":1}`,
	))
	req.Header.Set("Authorization", "Bearer business-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	target.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("v2 _search_around returned HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("decode around response %q: %v", rec.Body.String(), err)
	}
	data, ok := wire["data"]
	if !ok {
		t.Fatalf("v2 _search_around must wrap its window in data: %s", rec.Body.String())
	}
	if _, ok := wire["anchor"]; ok {
		t.Fatalf("v2 _search_around must not expose a bare legacy window: %s", rec.Body.String())
	}
	var result AroundResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode around data %q: %v", string(data), err)
	}
	if result.Anchor.MessageID != "401" || len(result.Before) != 0 || len(result.After) != 0 {
		t.Fatalf("unexpected around window in data: %+v", result)
	}
	if _, ok := wire["pagination"]; ok {
		t.Fatalf("around window has directional paging fields and must not add cursor pagination: %s", rec.Body.String())
	}
}

func TestV2SearchRejectsGroupAndThreadOutsideTokenSpace(t *testing.T) {
	const groupNo = "outside-space-group"
	groupSvc := &stubAuthzGroupSvc{
		activeMembers: map[string]bool{groupNo: true},
		groupModels: map[string]*group.InfoResp{
			groupNo: {GroupNo: groupNo, SpaceID: "another-space"},
		},
	}
	h := newV2SearchHTTPHandler(
		&recordingV2SearchUserService{stubAuthzUserSvc: &stubAuthzUserSvc{}},
		&stubProbe{},
		true,
	)
	h.groupService = groupSvc
	h.threadService = &stubAuthzThreadSvc{threadOK: map[string]bool{groupNo + "|thread-1": true}}
	h.externalGroupFn = func(string) (map[string]string, error) { return map[string]string{}, nil }
	target := newV2SearchRouter(t, h, []string{"im.messages:read"})

	requests := []struct {
		name string
		body string
	}{
		{name: "group", body: `{"channel_type":2,"channel_id":"` + groupNo + `","keyword":"needle"}`},
		{name: "thread", body: `{"channel_type":5,"channel_id":"` + groupNo + `____thread-1","keyword":"needle"}`},
	}
	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v2/cli/messages/_search", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer business-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			target.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
				t.Fatalf("cross-Space %s search returned HTTP %d; want a non-enumerating denial: %s",
					tc.name, rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "needle") {
				t.Fatalf("cross-Space %s search returned message content: %s", tc.name, rec.Body.String())
			}
		})
	}
}

func TestV2SearchAllowsExternalGroupMappedToTokenSpace(t *testing.T) {
	const groupNo = "external-space-group"
	osServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"hits":{"total":0,"hits":[]}}`)
	}))
	defer osServer.Close()
	installV2SearchESClient(t, osServer.URL)

	h := newV2SearchHTTPHandler(
		&recordingV2SearchUserService{stubAuthzUserSvc: &stubAuthzUserSvc{}},
		&stubProbe{},
		true,
	)
	h.groupService = &stubAuthzGroupSvc{
		activeMembers: map[string]bool{groupNo: true},
		groupModels: map[string]*group.InfoResp{
			groupNo: {GroupNo: groupNo, SpaceID: "source-space"},
		},
	}
	h.externalGroupFn = func(uid string) (map[string]string, error) {
		if uid != v2SearchUserID {
			t.Errorf("external-group lookup used uid %q, want Token user %q", uid, v2SearchUserID)
		}
		return map[string]string{groupNo: v2SearchSpaceID}, nil
	}
	target := newV2SearchRouter(t, h, []string{"im.messages:read"})
	req := httptest.NewRequest(http.MethodPost, "/v2/cli/messages/_search", strings.NewReader(
		`{"channel_type":2,"channel_id":"`+groupNo+`","keyword":"needle"}`,
	))
	req.Header.Set("Authorization", "Bearer business-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	target.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("external group mapped into the Token Space should remain searchable, got HTTP %d: %s",
			rec.Code, rec.Body.String())
	}
	var response struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode search response %q: %v", rec.Body.String(), err)
	}
	if len(response.Data) != 0 {
		t.Fatalf("empty OpenSearch fixture should return no hits, got %s", rec.Body.String())
	}
}

func newV2SearchRouter(t *testing.T, h *Handler, actions []string) *wkhttp.WKHttp {
	t.Helper()
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/internal/tokens/_resolve" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+v2SearchServiceSecret {
			t.Errorf("Auth Server Authorization = %q", got)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		access := contract.AccessContext{
			Principal: contract.Principal{
				TokenID:     "token-1",
				SpaceID:     v2SearchSpaceID,
				OwnerID:     v2SearchUserID,
				OwnerName:   "Token User",
				SubjectType: "user",
				SubjectID:   v2SearchUserID,
			},
			Permissions: []contract.PermissionGroup{{
				Domain:  "im.messages",
				Actions: actions,
			}},
			BotBindings: []contract.BotBinding{{BotID: v2SearchBotID, BotName: "Bound Bot"}},
		}
		if err := json.NewEncoder(w).Encode(contract.Data[contract.AccessContext]{Data: access}); err != nil {
			t.Errorf("encode auth response: %v", err)
		}
	}))
	t.Cleanup(authServer.Close)

	cfg := config.New()
	cfg.Test = true
	h.ctx = config.NewContext(cfg)
	source := wkhttp.New()
	h.Route(source)
	target, err := cliv2.NewRouter(authServer.URL, v2SearchServiceSecret)
	if err != nil {
		t.Fatalf("create v2 router: %v", err)
	}
	if err := cliv2.SetUIDRateLimiter(target, func(c *wkhttp.Context) {
		if c.GetLoginUID() == "" {
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error.")
			c.Abort()
			return
		}
		c.Next()
	}); err != nil {
		t.Fatalf("configure v2 UID rate limiter: %v", err)
	}
	if err := cliv2.Mount(source, target); err != nil {
		t.Fatalf("mount v2 routes: %v", err)
	}
	return target
}

func newV2SearchHTTPHandler(userSvc user.IService, probe visibilityProbe, enabled bool) *Handler {
	return &Handler{
		Log:           log.NewTLog("messages_search-cliv2-test"),
		cfg:           SearchConfig{OSReadAlias: "messages", Timeout: 2 * time.Second, CursorHMAC: "cliv2-test-key", RequireSpaceID: true},
		userService:   userSvc,
		groupService:  &stubAuthzGroupSvc{},
		threadService: &stubAuthzThreadSvc{},
		visibility:    probe,
		limiter:       newUIDLimiter(20, 20),
		cache:         newSenderCache(4, 0),
		mode:          searchbackend.Mode{ESServe: enabled, Declared: searchbackend.DeclaredES},
	}
}

func installV2SearchESClient(t *testing.T, address string) {
	t.Helper()
	resetESClientForTest()
	client, err := elastic.NewClient(
		elastic.SetURL(address),
		elastic.SetSniff(false),
		elastic.SetHealthcheck(false),
	)
	if err != nil {
		t.Fatalf("create OpenSearch fixture client: %v", err)
	}
	osMu.Lock()
	osClient = client
	osMu.Unlock()
	t.Cleanup(func() {
		resetESClientForTest()
		client.Stop()
	})
}

type recordingV2SearchUserService struct {
	mu sync.Mutex
	*stubAuthzUserSvc
	gotSpaceID string
	gotUserID  string
	gotPeerID  string
}

func (s *recordingV2SearchUserService) AreSpaceMembers(spaceID, uid1, uid2 string) (bool, error) {
	s.mu.Lock()
	s.gotSpaceID, s.gotUserID, s.gotPeerID = spaceID, uid1, uid2
	s.mu.Unlock()
	return s.stubAuthzUserSvc.AreSpaceMembers(spaceID, uid1, uid2)
}

func (s *recordingV2SearchUserService) observedAccess() (string, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gotSpaceID, s.gotUserID, s.gotPeerID
}

func (s *recordingV2SearchUserService) GetUsers([]string) ([]*user.Resp, error) {
	return nil, nil
}

var _ user.IService = (*recordingV2SearchUserService)(nil)
