package messages_search

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/gin-gonic/gin"
)

// YUJ-57 — the default RequireSpaceID=true Space gate must apply ONLY to
// space-scoped principals (user / uk). Space-less principals (as-bot / OBO)
// carry no Space and must NOT be blocked before ever reaching the allowlist
// predicate. These tests deliberately set RequireSpaceID=true (the production
// default) — a bare SearchConfig{} zero-values it to false and would silently
// stop exercising the gate at all.

// recordingGroupSvc wraps stubGroupSvc to capture which subject uid the
// allowlist enumeration queried, so a test can prove searchGlobalGroups builds
// its scope for the principal SUBJECT (grantorUID under OBO) rather than the
// context login uid (botUID).
type recordingGroupSvc struct {
	*stubGroupSvc
	queried []string
}

func (s *recordingGroupSvc) GetGroupsWithMemberUID(uid string) ([]*group.InfoResp, error) {
	s.queried = append(s.queried, uid)
	return s.stubGroupSvc.GetGroupsWithMemberUID(uid)
}

// TestResolveGlobalScope_OBOEmptySpaceBypassesGate_RequireTrue — an OBO
// principal (subject = grantorUID) with no Space must pass the RequireSpaceID
// gate and go on to enumerate the grantor's allowlist, rather than 404 at the
// gate. The allowlist collapsing to the grantor's group also proves the
// enumeration subject is the grantor, not the botUID.
func TestResolveGlobalScope_OBOEmptySpaceBypassesGate_RequireTrue(t *testing.T) {
	grantor := "grantor"
	gSvc := &stubGroupSvc{
		groupsByUID: map[string][]*group.InfoResp{grantor: {{GroupNo: "grpG"}}},
	}
	h := newAllowlistHandler(t, gSvc, &stubUserSvc{})
	h.cfg.RequireSpaceID = true
	h.threadEnumFn = func([]string) (map[string][]string, error) { return nil, nil }
	// OBO enumeration ∩'s the grantor allowlist with the OBO scope gate; wire a
	// checker that grants the grantor's group so the request resolves rather than
	// fail-closing on a missing checker (that path is covered by obo_test.go).
	h.oboCheck = &fakeOBOChecker{allow: map[string]bool{oboKey("grpG", channelTypeGroup): true}}

	c, rec := newValidatorCtx(t)
	c.Set("uid", "bot9") // OBO route's context login uid is the botUID — must be ignored
	setPrincipal(c, oboPrincipal{botUID: "bot9", grantorUID: grantor, spaceID: ""})

	osIDs, _, _, _, ok := h.resolveGlobalScope(c, grantor, nil, nil, "")
	if !ok {
		t.Fatalf("OBO with empty Space must bypass the fail-close gate under RequireSpaceID=true")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("bypass must not write a 404 response: %q", rec.Body.String())
	}
	if len(osIDs) != 1 || osIDs[0] != "grpG" {
		t.Fatalf("allowlist must be enumerated for the grantor subject; got %v", osIDs)
	}
}

// TestResolveGlobalScope_UserEmptySpaceFailsClosed_RequireTrue — the exemption
// is scoped to space-less principals. A real web user with no resolved Space
// must STILL fail-close under RequireSpaceID (regression guard so the YUJ-57
// bypass never leaks to the real-user path).
func TestResolveGlobalScope_UserEmptySpaceFailsClosed_RequireTrue(t *testing.T) {
	h := newAllowlistHandler(t, &stubGroupSvc{}, &stubUserSvc{})
	h.cfg.RequireSpaceID = true

	c, rec := newValidatorCtx(t)
	c.Set("uid", "alice") // lazy user principal, no space_id ⇒ empty Space

	osIDs, _, _, _, ok := h.resolveGlobalScope(c, "alice", nil, nil, "")
	if ok {
		t.Fatalf("real user with empty Space must fail-close under RequireSpaceID=true")
	}
	if len(osIDs) != 0 {
		t.Fatalf("fail-close must return no channels, got %v", osIDs)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("user fail-close must render NOT_FOUND, got %q", rec.Body.String())
	}
}

// TestSearchGlobalGroups_SubjectFromPrincipal_OBO — the groups endpoint must
// resolve its search subject via principal.SubjectUID() (grantorUID under OBO),
// matching _search_global_messages / _search_global_files. Before the fix it
// read c.GetLoginUID() (the botUID under an OBO route), which would enumerate
// the wrong allowlist and drift from the other two global paths on the DM fake
// channel, applyVisiblesWhitelist and audit subject.
//
// The grantor has no readable channels here, so the handler returns the empty
// groups envelope before ever touching OpenSearch; the load-bearing assertion
// is that the allowlist enumeration was queried for the grantor, never the
// botUID.
func TestSearchGlobalGroups_SubjectFromPrincipal_OBO(t *testing.T) {
	grantor := "grantor"
	rec0 := &recordingGroupSvc{stubGroupSvc: &stubGroupSvc{
		// Neither subject has groups, so the allowlist is empty regardless — the
		// point is WHICH uid gets queried, not the result.
		groupsByUID: map[string][]*group.InfoResp{},
	}}
	h := newAllowlistHandler(t, rec0, &stubUserSvc{})
	h.cfg.RequireSpaceID = true
	h.threadEnumFn = func([]string) (map[string][]string, error) { return nil, nil }

	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search_global_groups",
		strings.NewReader(`{"keyword":"hello"}`))
	gc.Set("uid", "bot9") // OBO context login uid = botUID — must NOT be the subject
	c := &wkhttp.Context{Context: gc}
	setPrincipal(c, oboPrincipal{botUID: "bot9", grantorUID: grantor, spaceID: ""})

	h.searchGlobalGroups(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("empty-scope groups request must return 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var env CursorList
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response must be a success envelope, got %q (err %v)", rec.Body.String(), err)
	}
	if len(rec0.queried) == 0 {
		t.Fatalf("allowlist enumeration was never invoked")
	}
	for _, uid := range rec0.queried {
		if uid == "bot9" {
			t.Fatalf("groups subject must be the grantor, never the botUID; queried %v", rec0.queried)
		}
	}
	sawGrantor := false
	for _, uid := range rec0.queried {
		if uid == grantor {
			sawGrantor = true
		}
	}
	if !sawGrantor {
		t.Fatalf("allowlist must be enumerated for the grantor subject; queried %v", rec0.queried)
	}
}
