package internal_membership

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
)

// testInternalToken is exactly minInternalTokenBytes long so every auth test
// runs against a value that would clear the production floor. One constant
// anchors that invariant instead of each test inventing a string.
const testInternalToken = "0123456789abcdef0123456789abcdef" // 32 bytes

const (
	epochsPath = "/v1/internal/membership/epochs"
	verifyPath = "/v1/internal/project-memberships/_verify"
)

// stubStore is an in-memory membershipStore. Errors are settable per method so
// each fail-closed path can be driven independently.
type stubStore struct {
	epochs   map[string]int64
	roles    map[string]int
	epoch    int64
	epochErr error
	memErr   error

	epochCalls  int
	memberCalls int
	lastSpaceID string
	lastProject string
	lastUIDs    []string
	lastIDs     []string
}

func (s *stubStore) Epochs(spaceID string, projectIDs []string) (map[string]int64, error) {
	s.epochCalls++
	s.lastSpaceID = spaceID
	s.lastIDs = projectIDs
	if s.epochErr != nil {
		return nil, s.epochErr
	}
	// Matched case-INSENSITIVELY and returned under the STORED spelling, because
	// that is what the real tables do: octo_project* are pinned to
	// utf8mb4_general_ci. A stub that matched exactly would agree with any handler,
	// including one that keys its answers off the database's spelling — which
	// answers a real member "not a member" whenever the caller's case differs.
	out := make(map[string]int64, len(projectIDs))
	for _, id := range projectIDs {
		for stored, v := range s.epochs {
			if strings.EqualFold(stored, id) {
				out[stored] = v
				break
			}
		}
	}
	return out, nil
}

func (s *stubStore) Memberships(spaceID, projectID string, uids []string) (int64, map[string]int, error) {
	s.memberCalls++
	s.lastSpaceID = spaceID
	s.lastProject = projectID
	s.lastUIDs = uids
	if s.memErr != nil {
		return 0, nil, s.memErr
	}
	// Case-insensitive match, stored spelling returned — see Epochs above.
	out := make(map[string]int, len(uids))
	for _, uid := range uids {
		for stored, role := range s.roles {
			if strings.EqualFold(stored, uid) {
				out[stored] = role
				break
			}
		}
	}
	return s.epoch, out, nil
}

func newTestModule(store membershipStore) *Module {
	return &Module{
		store:         store,
		internalToken: testInternalToken,
		Log:           log.NewTLog("InternalMembershipTest"),
	}
}

// newRouter mounts the read deadline + auth + handler. The production order is
// deadline → IP limiter → auth → handler on the concrete routes; only the
// limiter is left out here, because it is the one middleware that needs Redis,
// and route_order_test.go asserts the full order separately with recording
// middleware.
//
// The deadline IS mounted, because leaving it out would make every test in this
// file exercise a chain that cannot reproduce the hazard it exists for — and
// that is not hypothetical: it was called from inside the handler for a round,
// which meant it never covered a request that aborted at the limiter or at auth.
func newRouter(m *Module) *wkhttp.WKHttp {
	r := wkhttp.New()
	r.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	r.GET(epochsPath, boundBodyReadTime(), m.internalAuthMiddleware(), m.membershipEpochs)
	r.POST(verifyPath, boundBodyReadTime(), m.internalAuthMiddleware(), m.verifyProjectMemberships)
	return r
}

func doGet(t *testing.T, r *wkhttp.WKHttp, token, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, epochsPath+query, nil)
	if token != "" {
		req.Header.Set(internalTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func doPost(t *testing.T, r *wkhttp.WKHttp, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case []byte:
		raw = v
	case nil:
		raw = nil
	default:
		var err error
		raw, err = json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, verifyPath, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(internalTokenHeader, token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

type errorEnvelope struct {
	Error struct {
		Code       string `json:"code"`
		HTTPStatus int    `json:"http_status"`
	} `json:"error"`
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error %s: %v", w.Body.String(), err)
	}
	return env
}

// ============================================================================
// Auth — both endpoints, one token, no partial exposure
// ============================================================================

func TestBothEndpointsRejectMissingToken(t *testing.T) {
	s := &stubStore{}
	r := newRouter(newTestModule(s))

	if w := doGet(t, r, "", "?space_id=sp1&project_ids=p1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("epochs: want 401, got %d", w.Code)
	}
	if w := doPost(t, r, "", verifyRequest{SpaceID: "sp1", ProjectID: "p1", UIDs: []string{"u1"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("verify: want 401, got %d", w.Code)
	}
	if s.epochCalls != 0 || s.memberCalls != 0 {
		t.Fatalf("unauthenticated request reached the store: epochs=%d members=%d", s.epochCalls, s.memberCalls)
	}
}

func TestBothEndpointsRejectWrongToken(t *testing.T) {
	s := &stubStore{}
	r := newRouter(newTestModule(s))

	if w := doGet(t, r, "wrong-but-32-bytes-long-abcdefgh", "?space_id=sp1&project_ids=p1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("epochs: want 401, got %d", w.Code)
	}
	if w := doPost(t, r, "wrong-but-32-bytes-long-abcdefgh", verifyRequest{SpaceID: "sp1", ProjectID: "p1", UIDs: []string{"u1"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("verify: want 401, got %d", w.Code)
	}
	if s.epochCalls != 0 || s.memberCalls != 0 {
		t.Fatalf("wrong-token request reached the store")
	}
}

// TestUnsetServerTokenRejectsEvenAnEmptyClientToken pins the fail-closed
// posture: an unconfigured deployment must not accept a caller who also sends
// nothing, which is what a naive equality check would do.
func TestUnsetServerTokenRejectsEvenAnEmptyClientToken(t *testing.T) {
	m := newTestModule(&stubStore{})
	m.internalToken = ""
	r := newRouter(m)

	if w := doGet(t, r, "", "?space_id=sp1&project_ids=p1"); w.Code != http.StatusUnauthorized {
		t.Fatalf("epochs: want 401, got %d", w.Code)
	}
	if w := doPost(t, r, "", verifyRequest{SpaceID: "sp1", ProjectID: "p1", UIDs: []string{"u1"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("verify: want 401, got %d", w.Code)
	}
}

// TestUnauthorizedIsIndistinguishable pins the anti-enumeration property: an
// unset server token and a wrong token must produce byte-identical answers, so
// a caller cannot probe deployment state.
func TestUnauthorizedIsIndistinguishable(t *testing.T) {
	wrong := doGet(t, newRouter(newTestModule(&stubStore{})), "nope-nope-nope-nope-nope-nope-32", "?space_id=sp1&project_ids=p1")

	unset := newTestModule(&stubStore{})
	unset.internalToken = ""
	missing := doGet(t, newRouter(unset), "", "?space_id=sp1&project_ids=p1")

	if wrong.Code != missing.Code || wrong.Body.String() != missing.Body.String() {
		t.Fatalf("unauthorized answers differ:\n wrong token: %d %s\n unset server token: %d %s",
			wrong.Code, wrong.Body.String(), missing.Code, missing.Body.String())
	}
}

// ============================================================================
// epochs
// ============================================================================

func TestEpochsRequiresSpaceID(t *testing.T) {
	s := &stubStore{}
	w := doGet(t, newRouter(newTestModule(s)), testInternalToken, "?project_ids=p1")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
	}
	if s.epochCalls != 0 {
		t.Fatal("invalid request reached the store")
	}
}

func TestEpochsRequiresAtLeastOneProjectID(t *testing.T) {
	for _, q := range []string{"?space_id=sp1", "?space_id=sp1&project_ids=", "?space_id=sp1&project_ids=,,,"} {
		w := doGet(t, newRouter(newTestModule(&stubStore{})), testInternalToken, q)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d", q, w.Code)
		}
	}
}

func TestEpochsRejectsOversizedBatch(t *testing.T) {
	ids := make([]string, maxBatchProjectIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("p%d", i)
	}
	s := &stubStore{}
	w := doGet(t, newRouter(newTestModule(s)), testInternalToken,
		"?space_id=sp1&project_ids="+strings.Join(ids, ","))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if s.epochCalls != 0 {
		t.Fatal("oversized batch reached the store")
	}
}

func TestEpochsAcceptsExactlyTheBatchLimit(t *testing.T) {
	ids := make([]string, maxBatchProjectIDs)
	for i := range ids {
		ids[i] = fmt.Sprintf("p%d", i)
	}
	w := doGet(t, newRouter(newTestModule(&stubStore{epochs: map[string]int64{}})), testInternalToken,
		"?space_id=sp1&project_ids="+strings.Join(ids, ","))
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 at the limit, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestEpochsAnswersEveryRequestedID is the contract's core promise: an unknown
// project is 0, never a missing key. A consumer cannot distinguish an absent key
// from a dropped one, and the two demand opposite reactions.
func TestEpochsAnswersEveryRequestedID(t *testing.T) {
	s := &stubStore{epochs: map[string]int64{"known": 43}}
	w := doGet(t, newRouter(newTestModule(s)), testInternalToken,
		"?space_id=sp1&project_ids=known,unknown")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp epochsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Projects) != 2 {
		t.Fatalf("want 2 answers, got %d: %v", len(resp.Projects), resp.Projects)
	}
	if resp.Projects["known"] != 43 {
		t.Errorf("known: want 43, got %d", resp.Projects["known"])
	}
	got, present := resp.Projects["unknown"]
	if !present {
		t.Error("unknown project must be present with epoch 0, not omitted")
	}
	if got != 0 {
		t.Errorf("unknown: want 0, got %d", got)
	}
}

func TestEpochsAcceptsRepeatedAndCommaSeparatedParams(t *testing.T) {
	s := &stubStore{epochs: map[string]int64{"a": 1, "b": 2, "c": 3}}
	w := doGet(t, newRouter(newTestModule(s)), testInternalToken,
		"?space_id=sp1&project_ids=a,b&project_ids=c")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp epochsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Projects) != 3 {
		t.Fatalf("want 3 answers, got %v", resp.Projects)
	}
}

func TestEpochsDeduplicatesBeforeQuerying(t *testing.T) {
	s := &stubStore{epochs: map[string]int64{"a": 7}}
	doGet(t, newRouter(newTestModule(s)), testInternalToken, "?space_id=sp1&project_ids=a,a,a")
	if len(s.lastIDs) != 1 {
		t.Fatalf("want the store to see 1 deduplicated id, got %v", s.lastIDs)
	}
}

// TestEpochsFailsClosedOnStoreError pins that a lookup failure is a 500, never
// a 200 with an empty map — an empty map reads as "every project is gone".
func TestEpochsFailsClosedOnStoreError(t *testing.T) {
	s := &stubStore{epochErr: errors.New("db down")}
	w := doGet(t, newRouter(newTestModule(s)), testInternalToken, "?space_id=sp1&project_ids=p1")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "db down") {
		t.Error("internal error text leaked to the caller")
	}
}

func TestEpochsPassesSpaceIDToTheStore(t *testing.T) {
	s := &stubStore{epochs: map[string]int64{}}
	doGet(t, newRouter(newTestModule(s)), testInternalToken, "?space_id=sp-42&project_ids=p1")
	if s.lastSpaceID != "sp-42" {
		t.Fatalf("space_id not forwarded: got %q", s.lastSpaceID)
	}
}

// ============================================================================
// verify
// ============================================================================

func TestVerifyRejectsMalformedBodies(t *testing.T) {
	cases := []struct {
		name string
		body interface{}
	}{
		{"not json", []byte("{")},
		{"empty body", []byte("")},
		{"unknown field", []byte(`{"space_id":"s","project_id":"p","uid":["u1"]}`)},
		{"trailing garbage", []byte(`{"space_id":"s","project_id":"p","uids":["u1"]}{}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{}
			w := doPost(t, newRouter(newTestModule(s)), testInternalToken, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
			}
			if s.memberCalls != 0 {
				t.Fatal("malformed body reached the store")
			}
		})
	}
}

func TestVerifyRejectsInvalidFields(t *testing.T) {
	many := make([]string, maxBatchUIDs+1)
	for i := range many {
		many[i] = fmt.Sprintf("u%d", i)
	}
	cases := []struct {
		name string
		req  verifyRequest
	}{
		{"no space_id", verifyRequest{ProjectID: "p", UIDs: []string{"u1"}}},
		{"blank space_id", verifyRequest{SpaceID: "   ", ProjectID: "p", UIDs: []string{"u1"}}},
		{"no project_id", verifyRequest{SpaceID: "s", UIDs: []string{"u1"}}},
		{"no uids", verifyRequest{SpaceID: "s", ProjectID: "p"}},
		{"empty uids", verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: []string{}}},
		{"blank uid", verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: []string{"u1", "  "}}},
		{"duplicate uid", verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: []string{"u1", "u1"}}},
		{"too many uids", verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: many}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubStore{}
			w := doPost(t, newRouter(newTestModule(s)), testInternalToken, tc.req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
			}
			if s.memberCalls != 0 {
				t.Fatal("invalid request reached the store")
			}
		})
	}
}

// TestVerifyRejectsDuplicateUIDsRatherThanCollapsing is called out separately
// because the tempting alternative — silently deduplicating — returns fewer
// answers than the caller asked for and trips their "exactly one answer per
// requested uid" check as if the server had dropped one.
func TestVerifyRejectsDuplicateUIDsRatherThanCollapsing(t *testing.T) {
	s := &stubStore{epoch: 5, roles: map[string]int{"u1": 0}}
	w := doPost(t, newRouter(newTestModule(s)), testInternalToken,
		verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: []string{"u1", "u2", "u1"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for duplicates, got %d (%s)", w.Code, w.Body.String())
	}
}

func TestVerifyAcceptsExactlyTheBatchLimit(t *testing.T) {
	uids := make([]string, maxBatchUIDs)
	for i := range uids {
		uids[i] = fmt.Sprintf("u%d", i)
	}
	w := doPost(t, newRouter(newTestModule(&stubStore{epoch: 1})), testInternalToken,
		verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: uids})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 at the limit, got %d (%s)", w.Code, w.Body.String())
	}
	var resp verifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Members) != maxBatchUIDs {
		t.Fatalf("want %d answers, got %d", maxBatchUIDs, len(resp.Members))
	}
}

// TestVerifyAnswersEveryUIDInRequestOrder is the contract the consumer checks
// before trusting the batch at all.
func TestVerifyAnswersEveryUIDInRequestOrder(t *testing.T) {
	s := &stubStore{epoch: 43, roles: map[string]int{"owner": 2, "member": 0}}
	w := doPost(t, newRouter(newTestModule(s)), testInternalToken,
		verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: []string{"member", "stranger", "owner"}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var resp verifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ProjectID != "p" || resp.MemberEpoch != 43 {
		t.Fatalf("envelope mismatch: %+v", resp)
	}
	want := []string{"member", "stranger", "owner"}
	if len(resp.Members) != len(want) {
		t.Fatalf("want %d answers, got %d", len(want), len(resp.Members))
	}
	for i, uid := range want {
		if resp.Members[i].UID != uid {
			t.Fatalf("answer %d: want uid %q, got %q", i, uid, resp.Members[i].UID)
		}
	}
	if !resp.Members[0].Member || resp.Members[0].Role == nil || *resp.Members[0].Role != 0 {
		t.Errorf("ordinary member answer wrong: %+v", resp.Members[0])
	}
	if resp.Members[1].Member || resp.Members[1].Role != nil {
		t.Errorf("non-member must carry no role: %+v", resp.Members[1])
	}
	if !resp.Members[2].Member || resp.Members[2].Role == nil || *resp.Members[2].Role != 2 {
		t.Errorf("owner answer wrong: %+v", resp.Members[2])
	}
}

// TestVerifyNonMemberOmitsRoleOnTheWire checks the BYTES, not the decoded
// struct. Role 0 is a real role (ordinary member), so a non-member answer that
// serialized `"role":0` would read as a membership grant to any consumer that
// looks at role without first checking member.
func TestVerifyNonMemberOmitsRoleOnTheWire(t *testing.T) {
	s := &stubStore{epoch: 9, roles: map[string]int{}}
	w := doPost(t, newRouter(newTestModule(s)), testInternalToken,
		verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: []string{"stranger"}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "\"role\"") {
		t.Fatalf("non-member answer must not carry a role key: %s", w.Body.String())
	}
}

// TestVerifyUnknownProjectIsEpochZeroAndAllNonMembers pins that an unknown
// project, a disbanded one and one in another Space are a single answer.
//
// That answer is fail-closed only because 0 is unreachable for a real project —
// projects are created at member_epoch 1. The property lives in modules/project,
// not here, so it is asserted there against a real row
// (TestFreshProjectEpochIsNeverTheAbsentSentinel); this stub-backed test can
// only pin the handler's half.
func TestVerifyUnknownProjectIsEpochZeroAndAllNonMembers(t *testing.T) {
	s := &stubStore{epoch: 0, roles: map[string]int{}}
	w := doPost(t, newRouter(newTestModule(s)), testInternalToken,
		verifyRequest{SpaceID: "s", ProjectID: "nope", UIDs: []string{"u1", "u2"}})
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", w.Code)
	}
	var resp verifyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MemberEpoch != 0 {
		t.Errorf("want epoch 0, got %d", resp.MemberEpoch)
	}
	for _, a := range resp.Members {
		if a.Member || a.Role != nil {
			t.Errorf("unknown project must answer non-member: %+v", a)
		}
	}
}

func TestVerifyFailsClosedOnStoreError(t *testing.T) {
	s := &stubStore{memErr: errors.New("db down")}
	w := doPost(t, newRouter(newTestModule(s)), testInternalToken,
		verifyRequest{SpaceID: "s", ProjectID: "p", UIDs: []string{"u1"}})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d (%s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "db down") {
		t.Error("internal error text leaked to the caller")
	}
}

func TestVerifyForwardsIdentifiersVerbatim(t *testing.T) {
	s := &stubStore{epoch: 1, roles: map[string]int{}}
	doPost(t, newRouter(newTestModule(s)), testInternalToken,
		verifyRequest{SpaceID: "sp1", ProjectID: "p1", UIDs: []string{"u1"}})
	if s.lastSpaceID != "sp1" || s.lastProject != "p1" {
		t.Fatalf("identifiers not forwarded: space=%q project=%q", s.lastSpaceID, s.lastProject)
	}
	if len(s.lastUIDs) != 1 || s.lastUIDs[0] != "u1" {
		t.Fatalf("uids not forwarded: %v", s.lastUIDs)
	}
}

// TestVerifyRejectsPaddedIdentifiers pins that the body's values are taken
// verbatim, not silently rewritten.
//
// A JSON field carries the value itself: there is no separator syntax for the
// whitespace to belong to, unlike the epochs endpoint's comma-separated query
// parameter. So a padded id is malformed, and trimming it would ANSWER UNDER A
// DIFFERENT KEY than the caller asked about — the response echoes project_id and
// every uid, and a consumer keying the answers by its own strings would find a
// missing entry and read it as "no verdict returned". 400 says what happened.
func TestVerifyRejectsPaddedIdentifiers(t *testing.T) {
	cases := map[string]verifyRequest{
		"space_id":   {SpaceID: " sp1 ", ProjectID: "p1", UIDs: []string{"u1"}},
		"project_id": {SpaceID: "sp1", ProjectID: " p1 ", UIDs: []string{"u1"}},
		"uid":        {SpaceID: "sp1", ProjectID: "p1", UIDs: []string{" u1 "}},
		"tab in uid": {SpaceID: "sp1", ProjectID: "p1", UIDs: []string{"u1\t"}},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			s := &stubStore{epoch: 1, roles: map[string]int{}}
			w := doPost(t, newRouter(newTestModule(s)), testInternalToken, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
			}
			if s.memberCalls != 0 {
				t.Fatal("a padded identifier reached the store")
			}
		})
	}
}

// TestVerifyBodyCapIsEnforced pins that the byte cap exists alongside the
// structural uid limit — trust-boundary.md asks for both, because a
// well-formed-but-enormous payload passes any count check.
func TestVerifyBodyCapIsEnforced(t *testing.T) {
	huge := `{"space_id":"s","project_id":"p","uids":["` +
		strings.Repeat("u", maxRequestBodyBytes+1) + `"]}`
	s := &stubStore{}
	w := doPost(t, newRouter(newTestModule(s)), testInternalToken, []byte(huge))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
	if s.memberCalls != 0 {
		t.Fatal("oversized body reached the store")
	}
}

// TestParseIDListStopsAtTheLimit pins that an oversized query is refused WITHOUT
// the parser first materializing it. A GET carries no body, so the verify
// endpoint's byte cap does not bound this input — only the request line does,
// which still admits thousands of ids.
func TestParseIDListStopsAtTheLimit(t *testing.T) {
	huge := make([]string, 5000)
	for i := range huge {
		huge[i] = fmt.Sprintf("p%d", i)
	}
	ids, over := parseIDList([]string{strings.Join(huge, ",")}, maxBatchProjectIDs)
	if !over {
		t.Fatal("want overLimit for an oversized list")
	}
	if len(ids) != maxBatchProjectIDs {
		t.Fatalf("parser must stop at the limit, got %d entries", len(ids))
	}
}

func TestParseIDListExactLimitIsNotOverLimit(t *testing.T) {
	exact := make([]string, maxBatchProjectIDs)
	for i := range exact {
		exact[i] = fmt.Sprintf("p%d", i)
	}
	ids, over := parseIDList([]string{strings.Join(exact, ",")}, maxBatchProjectIDs)
	if over {
		t.Fatal("exactly the limit must not be over")
	}
	if len(ids) != maxBatchProjectIDs {
		t.Fatalf("want %d, got %d", maxBatchProjectIDs, len(ids))
	}
}

// TestParseIDListDeduplicatesBeforeCountingTheLimit pins that duplicates do not
// consume batch budget: 60 copies of two ids is a two-id request, not an
// oversized one.
func TestParseIDListDeduplicatesBeforeCountingTheLimit(t *testing.T) {
	raw := strings.TrimSuffix(strings.Repeat("a,b,", 30), ",")
	ids, over := parseIDList([]string{raw}, maxBatchProjectIDs)
	if over {
		t.Fatal("duplicates must not count toward the batch limit")
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 unique ids, got %v", ids)
	}
}

// TestSentinelAnomalyFailsClosedAndIsIndistinguishable pins how the module
// handles the one store failure that is a DATA anomaly rather than an outage.
//
// pkg/project refuses to serve an ACTIVE project that holds the contract's
// absent-epoch sentinel, because serving it lets a consumer cache a grant keyed
// on 0 that a later disband can never invalidate — 0 is also the answer for a
// project that is gone, so the staleness check agrees forever.
//
// Two properties, and they pull in opposite directions on purpose:
//
//   - on the WIRE it must be the ordinary 500, byte-identical to a database
//     outage. A caller that could tell them apart would learn that a specific
//     project exists and is mid-rollout;
//   - in the LOG it must be distinguishable, because it is the one case an
//     operator can act on (the reconcile loop repairs it; if the loop is off,
//     nothing does).
func TestSentinelAnomalyFailsClosedAndIsIndistinguishable(t *testing.T) {
	anomaly := fmt.Errorf("%w: p-42", projectpkg.ErrLiveProjectOnAbsentSentinel)
	outage := errors.New("dial tcp: connection refused")

	t.Run("epochs", func(t *testing.T) {
		anomalyW := doGet(t, newRouter(newTestModule(&stubStore{epochErr: anomaly})),
			testInternalToken, "?space_id=s&project_ids=p-42")
		outageW := doGet(t, newRouter(newTestModule(&stubStore{epochErr: outage})),
			testInternalToken, "?space_id=s&project_ids=p-42")

		if anomalyW.Code != http.StatusInternalServerError {
			t.Fatalf("want 500 for the sentinel anomaly, got %d (%s)", anomalyW.Code, anomalyW.Body.String())
		}
		if anomalyW.Body.String() != outageW.Body.String() {
			t.Errorf("a data anomaly must answer byte-identically to an outage, or the caller "+
				"learns that a specific project exists:\n anomaly: %s\n outage:  %s",
				anomalyW.Body.String(), outageW.Body.String())
		}
		if strings.Contains(anomalyW.Body.String(), "p-42") {
			t.Error("the response leaked the project id from the wrapped error")
		}
	})

	t.Run("verify", func(t *testing.T) {
		s := &stubStore{memErr: anomaly}
		w := doPost(t, newRouter(newTestModule(s)), testInternalToken,
			verifyRequest{SpaceID: "s", ProjectID: "p-42", UIDs: []string{"u1"}})
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("want 500, got %d (%s)", w.Code, w.Body.String())
		}
		// Never a 200 that reports the users as members at epoch 0 — that answer
		// is the whole defect.
		if strings.Contains(w.Body.String(), "member_epoch") {
			t.Error("the refusal must not carry an epoch of any kind")
		}
	})
}

// TestLookupFailureLoggingSeparatesTheAnomaly is the log half of the property
// above: the wire is uniform, the log is not.
func TestLookupFailureLoggingSeparatesTheAnomaly(t *testing.T) {
	src, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	body := string(src)
	start := strings.Index(body, "func (m *Module) logLookupFailure(")
	if start < 0 {
		t.Fatal("logLookupFailure not found — if it moved, point this guard at it rather than deleting it")
	}
	if end := strings.Index(body[start:], "\n}\n"); end > 0 {
		body = body[start : start+end]
	}
	if !strings.Contains(body, "ErrLiveProjectOnAbsentSentinel") {
		t.Error("the sentinel anomaly must get its own log line: it is the only store failure an " +
			"operator can act on, and with the reconcile loop disabled it is the only signal that " +
			"the endpoint is refusing rather than the database being down")
	}
	// The anomaly branch must extract WHICH project tripped it, or the out-of-band
	// repair has nothing to act on and the operator waits for the reconcile cursor
	// to come around — hours on a large table, during which every request naming
	// that id is a 500.
	if !strings.Contains(body, "SentinelAnomalyError") {
		t.Error("the anomaly branch must match the typed error that carries the project id, " +
			"not just the sentinel value: naming the row is what makes the fast-path repair " +
			"possible")
	}
	if !strings.Contains(body, "repairSentinelOutOfBand(") {
		t.Error("a named anomalous project must be repaired out of band; leaving it to the " +
			"reconcile scan means a bounded page budget behind a persisted cursor, and the " +
			"anomalous row has the highest id so it is reached last")
	}

	// EVERY branch must reach a logger. A silent return would make the endpoint
	// answer 500 with nothing anywhere saying why.
	//
	// Counted against the number of returns rather than pinned to a literal, because
	// pinning it to a literal is what made this assertion fail when a third branch
	// was added correctly — a guard that has to be edited to accept a correct change
	// teaches people to edit guards. There are three branches now (typed anomaly,
	// sentinel without an id, generic failure) and the invariant is one log call per
	// branch, whatever the count becomes.
	branches := strings.Count(body, "\n\t\treturn\n") + 1 // early returns + the fall-through
	if got := strings.Count(body, "m.Error("); got != branches {
		t.Errorf("every failure branch must log: found %d m.Error call(s) for %d branch(es). "+
			"A branch that returns without logging makes a 500 with no explanation anywhere",
			got, branches)
	}
}

// TestBothEndpointsAgreeAboutSpaceID pins the two endpoints treating the same
// field name the same way.
//
// They disagreed for a round: verify rejected a padded space_id while epochs
// trimmed it. The separator-syntax rationale that justifies trimming project_ids
// (a comma-separated list, where the whitespace is punctuation) does not extend
// to a single-valued parameter, where the whitespace can only be part of the
// value. A field name meaning two things across two endpoints of one contract is
// the kind of divergence a consumer discovers in production.
func TestBothEndpointsAgreeAboutSpaceID(t *testing.T) {
	padded := []string{" sp1", "sp1 ", " sp1 ", "\tsp1"}
	for _, value := range padded {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			epochsStore := &stubStore{epochs: map[string]int64{"p1": 1}}
			w := doGet(t, newRouter(newTestModule(epochsStore)), testInternalToken,
				"?space_id="+url.QueryEscape(value)+"&project_ids=p1")
			if w.Code != http.StatusBadRequest {
				t.Errorf("epochs: want 400 for a padded space_id, got %d (%s)", w.Code, w.Body.String())
			}
			if epochsStore.epochCalls != 0 {
				t.Error("epochs: a padded space_id reached the store")
			}

			verifyStore := &stubStore{epoch: 1, roles: map[string]int{}}
			w = doPost(t, newRouter(newTestModule(verifyStore)), testInternalToken,
				verifyRequest{SpaceID: value, ProjectID: "p1", UIDs: []string{"u1"}})
			if w.Code != http.StatusBadRequest {
				t.Errorf("verify: want 400 for a padded space_id, got %d (%s)", w.Code, w.Body.String())
			}
			if verifyStore.memberCalls != 0 {
				t.Error("verify: a padded space_id reached the store")
			}
		})
	}

	// The clean value still works on both, so the check is a rejection of padding
	// and not a rejection of the field.
	epochsStore := &stubStore{epochs: map[string]int64{"p1": 1}}
	if w := doGet(t, newRouter(newTestModule(epochsStore)), testInternalToken,
		"?space_id=sp1&project_ids=p1"); w.Code != http.StatusOK {
		t.Fatalf("epochs: a clean space_id must be accepted, got %d (%s)", w.Code, w.Body.String())
	}
	verifyStore := &stubStore{epoch: 1, roles: map[string]int{}}
	if w := doPost(t, newRouter(newTestModule(verifyStore)), testInternalToken,
		verifyRequest{SpaceID: "sp1", ProjectID: "p1", UIDs: []string{"u1"}}); w.Code != http.StatusOK {
		t.Fatalf("verify: a clean space_id must be accepted, got %d (%s)", w.Code, w.Body.String())
	}
}

// TestAnswersAreKeyedByTheCallerNotTheDatabase pins the fix for a defect that
// looks like nothing and denies real members.
//
// Both tables are pinned to utf8mb4_general_ci, which is case-INSENSITIVE, so the
// SQL matches a stored `abc` for a caller's `ABC` and returns the row spelled
// `abc`. Keying the response off the returned spelling means the caller's lookup
// of its own `ABC` misses — and a missing key is the contract's "does not exist" /
// member:false sentinel. Fail-closed, but wrong: a live project reads as absent
// and a real member as denied, while the database matched perfectly.
//
// The stub returns lowercase, the request uses uppercase — exactly the shape a
// peer produces after round-tripping the id through its own store.
func TestAnswersAreKeyedByTheCallerNotTheDatabase(t *testing.T) {
	t.Run("epochs", func(t *testing.T) {
		s := &stubStore{epochs: map[string]int64{"p-abc": 7}}
		w := doGet(t, newRouter(newTestModule(s)), testInternalToken,
			"?space_id=sp1&project_ids=P-ABC")
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
		}
		var resp epochsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		got, ok := resp.Projects["P-ABC"]
		if !ok {
			t.Fatalf("the answer must be keyed by the id the CALLER sent; got %v", resp.Projects)
		}
		if got != 7 {
			t.Errorf("want the matched project's epoch 7 under the caller's key, got %d — a 0 here "+
				"is the contract's \"does not exist\" sentinel for a project that plainly exists", got)
		}
	})

	t.Run("verify", func(t *testing.T) {
		s := &stubStore{epoch: 3, roles: map[string]int{"u-abc": 1}}
		w := doPost(t, newRouter(newTestModule(s)), testInternalToken,
			verifyRequest{SpaceID: "sp1", ProjectID: "p1", UIDs: []string{"U-ABC"}})
		if w.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
		}
		var resp verifyResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, w.Body.String())
		}
		if len(resp.Members) != 1 {
			t.Fatalf("want one answer, got %+v", resp.Members)
		}
		a := resp.Members[0]
		if a.UID != "U-ABC" {
			t.Errorf("the answer must echo the caller's uid verbatim, got %q", a.UID)
		}
		if !a.Member {
			t.Fatal("a real member must not be answered member:false because the database " +
				"returned their uid in a different case than the caller sent")
		}
		if a.Role == nil || *a.Role != 1 {
			t.Errorf("want role 1, got %v", a.Role)
		}
	})
}
