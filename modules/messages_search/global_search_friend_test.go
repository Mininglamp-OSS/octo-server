package messages_search

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
)

// OCTO-266 consumer regression: a real-user friend whose DM lives outside the
// Token's bound Space must not be readable through the v2 global search, while
// the legacy v1 friend fallback keeps its existing behaviour.
//
// The scenario deliberately gives the caller TWO readable rooms inside the
// Space (a joined group and a same-Space member's DM) next to the friend DM
// outside it. The request specifies no channel filter, so the resolved scope
// never collapses to one channel and never re-dispatches through the
// single-channel fast path: v2 and v1 both hit the real multi-channel global
// branch whose terms(channelId) scope is the allowlist under test.
//
// The fake backend is scope-driven: it parses the channelId terms from the
// query it actually receives and returns only the candidate hits for those
// channels. It is NOT an unconditional echo of the friend DM, so a v2
// allowlist that wrongly admits the out-of-Space friend proves itself by
// putting that channel into the query and receiving its hit back.
//
// Every candidate doc carries the caller's Space (the friend's DM has
// historical docs indexed while the peer still shared the Space), so the
// query's DM spaceId double-guard matches them all and cannot mask an
// over-broad allowlist — the allowlist composition is the decisive gate.

const (
	globalFriendLoginUID  = "token-user"
	globalFriendPeerUID   = "peer-user"      // friend, but NOT a member of the Space
	globalColleagueUID    = "colleague-user" // NOT a friend, but IS a Space member
	globalFriendSpaceID   = "token-space"
	globalInSpaceGroupNo  = "in-space-group"
	globalFriendDMNeedle  = "out-of-space-friend-needle"
	globalGroupNeedle     = "in-space-group-needle"
	globalColleagueNeedle = "in-space-colleague-needle"
)

// globalFriendUserSvc composes the two existing stubs: stubUserSvc supplies the
// global allowlist edges (GetFriends / ExistBlacklistsBoth), stubAuthzUserSvc
// supplies the single-channel P2P gate edges (IsFriend / AreSpaceMembers /
// QueryPeerRobotInfo / ExistBlacklist). Both fixtures describe the same
// relationship: the peer is a friend, the colleague is only a Space member.
type globalFriendUserSvc struct {
	user.IService
	authz  *stubAuthzUserSvc
	global *stubUserSvc
}

func (s *globalFriendUserSvc) GetFriends(uid string) ([]*user.FriendResp, error) {
	return s.global.GetFriends(uid)
}

func (s *globalFriendUserSvc) ExistBlacklistsBoth(loginUID string, peers []string) (map[string]bool, map[string]bool, error) {
	return s.global.ExistBlacklistsBoth(loginUID, peers)
}

func (s *globalFriendUserSvc) IsFriend(uid, toUID string) (bool, error) {
	return s.authz.IsFriend(uid, toUID)
}

func (s *globalFriendUserSvc) ExistBlacklist(uid, toUID string) (bool, error) {
	return s.authz.ExistBlacklist(uid, toUID)
}

func (s *globalFriendUserSvc) QueryPeerRobotInfo(peerUID string) (bool, string, error) {
	return s.authz.QueryPeerRobotInfo(peerUID)
}

func (s *globalFriendUserSvc) AreSpaceMembers(spaceID, uid1, uid2 string) (bool, error) {
	return s.authz.AreSpaceMembers(spaceID, uid1, uid2)
}

func (s *globalFriendUserSvc) GetUsers([]string) ([]*user.Resp, error) {
	return nil, nil
}

func newGlobalFriendUserSvc() *globalFriendUserSvc {
	return &globalFriendUserSvc{
		authz: &stubAuthzUserSvc{
			// The peer is a friend, but no space_member pair exists for it.
			friends:      map[string]bool{friendKey(globalFriendLoginUID, globalFriendPeerUID): true},
			spaceMembers: map[string]bool{},
		},
		global: &stubUserSvc{
			friends: []*user.FriendResp{{UID: globalFriendPeerUID}},
		},
	}
}

// globalSearchFixtureDocs are the candidate rooms the backend knows about.
// They are filtered per request by the query's channelId scope; a channel the
// allowlist never asks for never comes back.
func globalSearchFixtureDocs() []Doc {
	textPayload := payloadTypeText
	return []Doc{
		{
			MessageID:   403,
			MessageSeq:  9,
			Timestamp:   1717000003000,
			From:        globalFriendPeerUID,
			ChannelID:   fakeChannelIDFor(globalFriendLoginUID, globalFriendPeerUID),
			ChannelType: uint32(channelTypePerson),
			SpaceID:     globalFriendSpaceID,
			Payload:     &Payload{Type: &textPayload, Text: &TextPayload{Content: globalFriendDMNeedle}},
		},
		{
			MessageID:   402,
			MessageSeq:  8,
			Timestamp:   1717000002000,
			From:        globalColleagueUID,
			ChannelID:   fakeChannelIDFor(globalFriendLoginUID, globalColleagueUID),
			ChannelType: uint32(channelTypePerson),
			SpaceID:     globalFriendSpaceID,
			Payload:     &Payload{Type: &textPayload, Text: &TextPayload{Content: globalColleagueNeedle}},
		},
		{
			MessageID:   401,
			MessageSeq:  7,
			Timestamp:   1717000001000,
			From:        globalColleagueUID,
			ChannelID:   globalInSpaceGroupNo,
			ChannelType: uint32(channelTypeGroup),
			SpaceID:     globalFriendSpaceID,
			Payload:     &Payload{Type: &textPayload, Text: &TextPayload{Content: globalGroupNeedle}},
		},
	}
}

// queryChannelScope walks the elastic DSL and collects every channelId value.
// It handles both the terms query used by the multi-channel global branch and
// the term query used by the single-channel fast path.
func queryChannelScope(body []byte) map[string]struct{} {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	ids := map[string]struct{}{}
	var walk func(any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if raw, ok := node["channelId"]; ok {
				switch value := raw.(type) {
				case string:
					ids[value] = struct{}{}
				case []any:
					for _, item := range value {
						if id, ok := item.(string); ok {
							ids[id] = struct{}{}
						}
					}
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, item := range node {
				walk(item)
			}
		}
	}
	walk(root)
	return ids
}

// globalSearchScopeFixture serves _analyze tokens and, for every _search,
// returns only the candidate docs whose channelId is inside the query's
// terms(channelId) scope.
type globalSearchScopeFixture struct {
	t        *testing.T
	mu       sync.Mutex
	requests int
	scope    map[string]struct{}
}

func installGlobalSearchScopeFixture(t *testing.T) *globalSearchScopeFixture {
	t.Helper()
	f := &globalSearchScopeFixture{t: t}
	osServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/_analyze"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"tokens":[{"token":"needle","start_offset":0,"end_offset":6,"type":"word","position":0}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_search"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "cannot read search query", http.StatusBadRequest)
				return
			}
			scope := queryChannelScope(body)
			hits := make([]map[string]any, 0, 3)
			for _, doc := range globalSearchFixtureDocs() {
				if _, ok := scope[doc.ChannelID]; !ok {
					continue
				}
				src, err := json.Marshal(doc)
				if err != nil {
					f.t.Errorf("marshal fixture doc: %v", err)
					continue
				}
				hits = append(hits, map[string]any{
					"_index":  "messages",
					"_type":   "_doc",
					"_id":     strconv.FormatInt(doc.MessageID, 10),
					"_score":  1,
					"_source": json.RawMessage(src),
					"sort":    []any{doc.Timestamp, doc.MessageID},
				})
			}
			f.mu.Lock()
			f.requests++
			f.scope = scope
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"hits": map[string]any{"total": len(hits), "max_score": 1, "hits": hits},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(osServer.Close)
	installV2SearchESClient(t, osServer.URL)
	return f
}

func (f *globalSearchScopeFixture) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

func (f *globalSearchScopeFixture) lastScope() map[string]struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]struct{}, len(f.scope))
	for id := range f.scope {
		out[id] = struct{}{}
	}
	return out
}

// newGlobalSearchInSpaceGroupSvc wires the caller's joined group and the
// Space-member DM source. Thread enumeration is stubbed out: these tests only
// exercise the group + DM portion of the allowlist.
func newGlobalSearchInSpaceGroupSvc() *stubGroupSvc {
	return &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{
			globalFriendLoginUID: {{GroupNo: globalInSpaceGroupNo, SpaceID: globalFriendSpaceID}},
		},
	}
}

func newGlobalSearchInSpaceMembers(spaceID, loginUID string) ([]string, error) {
	if spaceID != globalFriendSpaceID || loginUID != globalFriendLoginUID {
		return nil, nil
	}
	return []string{globalColleagueUID}, nil
}

func decodeGlobalSearchHits(t *testing.T, rec *httptest.ResponseRecorder) []SearchAllHit {
	t.Helper()
	var response struct {
		Data []SearchAllHit `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode global search response %q: %v", rec.Body.String(), err)
	}
	return response.Data
}

func findMessageChannel(hits []SearchAllHit, channelID string) bool {
	for _, hit := range hits {
		if hit.Message != nil && hit.Message.ChannelID == channelID {
			return true
		}
	}
	return false
}

// TestV2GlobalSearch_FriendOutsideTokenSpaceCannotReadDM is the v2 consumer
// contract on the REAL multi-channel global branch (no channel_ids filter). The
// bound-Space candidate filter keeps the two in-Space rooms and drops the
// out-of-Space friend DM, so the scope never collapses to the single-channel
// gate. The friend channel must not appear in the query scope nor in the
// returned hits; the two in-Space rooms must still come back.
func TestV2GlobalSearch_FriendOutsideTokenSpaceCannotReadDM(t *testing.T) {
	svc := newGlobalFriendUserSvc()
	es := installGlobalSearchScopeFixture(t)

	h := newV2SearchHTTPHandler(svc, &stubProbe{}, true)
	h.groupService = newGlobalSearchInSpaceGroupSvc()
	h.threadEnumFn = func([]string) (map[string][]string, error) { return nil, nil }
	h.externalGroupFn = func(string) (map[string]string, error) { return nil, nil }
	h.spaceMembersFn = newGlobalSearchInSpaceMembers
	h.dmBotFilterFn = func(_ string, peers []string) ([]string, error) { return peers, nil }
	h.dmBotInSpaceFn = func(string, []string) (map[string]bool, error) { return map[string]bool{}, nil }
	target := newV2SearchRouter(t, h, []string{"im.messages:read"})

	req := httptest.NewRequest(http.MethodPost, "/v2/cli/messages/_search_global_messages", strings.NewReader(
		`{"keyword":"needle","page_size":10}`,
	))
	req.Header.Set("Authorization", "Bearer business-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	target.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("v2 global search returned HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if got := es.requestCount(); got != 1 {
		t.Fatalf("v2 global search issued %d OpenSearch queries, want 1", got)
	}
	scope := es.lastScope()
	friendDMChannel := fakeChannelIDFor(globalFriendLoginUID, globalFriendPeerUID)
	colleagueDMChannel := fakeChannelIDFor(globalFriendLoginUID, globalColleagueUID)
	if len(scope) != 2 {
		t.Fatalf("v2 request must scope to exactly the two in-Space rooms, got %d channel(s): %v", len(scope), scope)
	}
	if _, ok := scope[friendDMChannel]; ok {
		t.Fatalf("out-of-Space friend DM entered the v2 global query scope: %v", scope)
	}
	if _, ok := scope[globalInSpaceGroupNo]; !ok {
		t.Fatalf("in-Space group missing from v2 global query scope: %v", scope)
	}
	if _, ok := scope[colleagueDMChannel]; !ok {
		t.Fatalf("same-Space colleague DM missing from v2 global query scope: %v", scope)
	}

	hits := decodeGlobalSearchHits(t, rec)
	if findMessageChannel(hits, globalFriendPeerUID) {
		t.Fatalf("out-of-Space friend DM was returned by v2 global search: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), globalFriendDMNeedle) {
		t.Fatalf("out-of-Space friend DM content leaked through v2 global search: %s", rec.Body.String())
	}
	if !findMessageChannel(hits, globalInSpaceGroupNo) {
		t.Fatalf("v2 global search must still return the in-Space group hit, got %s", rec.Body.String())
	}
	if !findMessageChannel(hits, globalColleagueUID) {
		t.Fatalf("v2 global search must still return the same-Space member DM hit, got %s", rec.Body.String())
	}
}

// TestV1GlobalSearch_FriendFallbackStillAllowsDM pins the legacy behaviour the
// v2 gate must not change. Same Space, same scope-driven backend, same
// candidate set: the v1 global branch (no CLI principal) still keeps the
// friend DM in its allowlist and therefore returns the friend's message, while
// the in-Space rooms come back as usual.
func TestV1GlobalSearch_FriendFallbackStillAllowsDM(t *testing.T) {
	svc := newGlobalFriendUserSvc()
	es := installGlobalSearchScopeFixture(t)

	h := newAllowlistHandler(t, newGlobalSearchInSpaceGroupSvc(), svc)
	h.cfg = SearchConfig{
		OSReadAlias:          "messages",
		Timeout:              2 * time.Second,
		CursorHMAC:           "cliv2-test-key",
		RequireSpaceID:       true,
		StopwordStripEnabled: true,
	}
	h.visibility = &stubProbe{}
	h.threadEnumFn = func([]string) (map[string][]string, error) { return nil, nil }
	h.spaceMembersFn = newGlobalSearchInSpaceMembers

	c, rec := newAuthzCtxWithSpace(t, globalFriendSpaceID)
	c.Set("uid", globalFriendLoginUID)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages/_search_global_messages", strings.NewReader(
		`{"keyword":"needle","page_size":10}`,
	))
	h.searchGlobalMessages(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("v1 global search returned HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if got := es.requestCount(); got != 1 {
		t.Fatalf("v1 global search issued %d OpenSearch queries, want 1", got)
	}
	scope := es.lastScope()
	friendDMChannel := fakeChannelIDFor(globalFriendLoginUID, globalFriendPeerUID)
	if len(scope) != 3 {
		t.Fatalf("v1 request must scope to the legacy friend + Space-member + group set, got %d channel(s): %v", len(scope), scope)
	}
	if _, ok := scope[friendDMChannel]; !ok {
		t.Fatalf("v1 friend fallback must keep the out-of-Space friend DM in scope: %v", scope)
	}

	hits := decodeGlobalSearchHits(t, rec)
	if !findMessageChannel(hits, globalFriendPeerUID) {
		t.Fatalf("v1 friend fallback must return the friend DM hit, got %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), globalFriendDMNeedle) {
		t.Fatalf("v1 friend DM content missing from response: %s", rec.Body.String())
	}
	if !findMessageChannel(hits, globalInSpaceGroupNo) {
		t.Fatalf("v1 global search must still return the in-Space group hit, got %s", rec.Body.String())
	}
}
