package botfather

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/messages_search"
	"github.com/gin-gonic/gin"
)

// YUJ-49 (#B) — uk search entry: resolveUKPrincipal turns the authUserAPIKey()
// context (api_key_uid / api_key_space_id) into a uk principal (直接真人身份).
//
// YUJ-58 (RC #608) — resolveUKPrincipal additionally enforces live Space
// membership of the key owner against api_key_space_id, mirroring the human
// route's SpaceMiddleware. An expired key whose owner has been removed from /
// disabled in the frozen Space must fail closed (403), otherwise global DM
// search would enumerate that Space's current members' DM history. Membership
// is checked via an injectable spacepkg.MembershipChecker so these unit tests
// need no DB; a DB-backed regression lives alongside the SpaceMiddleware tests.

func newUKSearchCtx(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/user/messages/_search", nil)
	return &wkhttp.Context{Context: gc}, rec
}

// memberChecker returns a stub MembershipChecker that always reports isMember,
// recording whether it was invoked so tests can assert the space-scoped gate
// only runs when api_key_space_id is present.
func memberChecker(isMember bool, called *bool) func(spaceID, uid string) (bool, error) {
	return func(spaceID, uid string) (bool, error) {
		if called != nil {
			*called = true
		}
		return isMember, nil
	}
}

func TestResolveUKPrincipal_DirectRealUser(t *testing.T) {
	bf := &BotFather{Log: log.NewTLog("uk-search-test")}
	c, rec := newUKSearchCtx(t)
	c.Set("api_key_uid", "u_9")
	c.Set("api_key_space_id", "sp_1")

	called := false
	bf.resolveUKPrincipalWithChecker(c, memberChecker(true, &called))

	if c.IsAborted() {
		t.Fatalf("uk with valid key + active membership must not abort, body=%q", rec.Body.String())
	}
	if !called {
		t.Fatalf("membership must be checked when api_key_space_id is present")
	}
	if got := messages_search.PrincipalKind(c); got != "uk" {
		t.Fatalf("principal kind = %q, want uk", got)
	}
}

func TestResolveUKPrincipal_MissingKeyFailsClosed(t *testing.T) {
	bf := &BotFather{Log: log.NewTLog("uk-search-test")}
	c, rec := newUKSearchCtx(t)

	bf.resolveUKPrincipal(c)

	if !c.IsAborted() {
		t.Fatalf("missing api_key_uid must fail closed / abort")
	}
	if got := messages_search.PrincipalKind(c); got != "" {
		t.Fatalf("missing key must not set a principal, got %q", got)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("missing key must not return 200")
	}
}

// TestResolveUKPrincipal_NonMemberFailsClosed is the core YUJ-58 regression: an
// active key whose owner is no longer an active member of the frozen
// api_key_space_id must be denied (403) and never get a uk principal — matching
// the human route's SpaceMiddleware, closing the global-DM-enumeration bypass.
func TestResolveUKPrincipal_NonMemberFailsClosed(t *testing.T) {
	bf := &BotFather{Log: log.NewTLog("uk-search-test")}
	c, rec := newUKSearchCtx(t)
	c.Set("api_key_uid", "u_9")
	c.Set("api_key_space_id", "sp_1")

	bf.resolveUKPrincipalWithChecker(c, memberChecker(false, nil))

	if !c.IsAborted() {
		t.Fatalf("non-member key must fail closed / abort")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-member must return 403 (align SpaceMiddleware), got %d body=%q", rec.Code, rec.Body.String())
	}
	if got := messages_search.PrincipalKind(c); got != "" {
		t.Fatalf("non-member must not set a principal, got %q", got)
	}
}

// TestResolveUKPrincipal_MembershipErrorFailsClosed: a checker error must
// fail closed (500), not fall through to a resolved principal.
func TestResolveUKPrincipal_MembershipErrorFailsClosed(t *testing.T) {
	bf := &BotFather{Log: log.NewTLog("uk-search-test")}
	c, rec := newUKSearchCtx(t)
	c.Set("api_key_uid", "u_9")
	c.Set("api_key_space_id", "sp_1")

	bf.resolveUKPrincipalWithChecker(c, func(spaceID, uid string) (bool, error) {
		return false, errors.New("db down")
	})

	if !c.IsAborted() {
		t.Fatalf("membership check error must fail closed / abort")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("membership error must return 500, got %d body=%q", rec.Code, rec.Body.String())
	}
	if got := messages_search.PrincipalKind(c); got != "" {
		t.Fatalf("membership error must not set a principal, got %q", got)
	}
}

// TestResolveUKPrincipal_NoSpaceSkipsMembership: a spaceless uk key
// (api_key_space_id empty) has no membership to check — the gate is skipped and
// the uk principal is resolved; the empty-Space fail-close is enforced
// downstream by RequiresSpaceScope, not here.
func TestResolveUKPrincipal_NoSpaceSkipsMembership(t *testing.T) {
	bf := &BotFather{Log: log.NewTLog("uk-search-test")}
	c, rec := newUKSearchCtx(t)
	c.Set("api_key_uid", "u_9")
	// no api_key_space_id

	called := false
	bf.resolveUKPrincipalWithChecker(c, memberChecker(true, &called))

	if c.IsAborted() {
		t.Fatalf("spaceless uk must not abort here, body=%q", rec.Body.String())
	}
	if called {
		t.Fatalf("membership must NOT be checked when api_key_space_id is empty")
	}
	if got := messages_search.PrincipalKind(c); got != "uk" {
		t.Fatalf("principal kind = %q, want uk", got)
	}
}
