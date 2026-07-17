package bot_api

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/messages_search"
	"github.com/gin-gonic/gin"
)

// YUJ-49 (#B) / YUJ-53 (#F) — bot search entry: resolveSearchPrincipal
// distinguishes as-bot (no on_behalf_of) from as-user(OBO) (on_behalf_of
// present). App Bot is explicitly denied on both branches. The OBO entry does
// a channel-agnostic grant-existence fail-fast (validateSearchOBO); the
// per-channel scope + grantorCanReadChannel (TOCTOU) check is reused by the
// messages_search obo gate via the injected ba.SearchOBOAllowed.

func newBotSearchCtx(t *testing.T, body string) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/bot/messages/_search", strings.NewReader(body))
	return &wkhttp.Context{Context: gc}, rec
}

func newSearchTestBotAPI() *BotAPI { return &BotAPI{Log: log.NewTLog("bot-search-test")} }

func TestResolveSearchPrincipal_AsBot(t *testing.T) {
	ba := newSearchTestBotAPI()
	c, rec := newBotSearchCtx(t, `{"keyword":"hi"}`)
	c.Set(CtxKeyRobotID, "bot_1")
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.resolveSearchPrincipal(c)

	if c.IsAborted() {
		t.Fatalf("as-bot must not abort, body=%q", rec.Body.String())
	}
	if got := messages_search.PrincipalKind(c); got != "user_bot" {
		t.Fatalf("principal kind = %q, want user_bot", got)
	}
	// Body must still be readable by the downstream _search* handler (the probe
	// consumed it via GetRawData and must have restored it).
	b, _ := io.ReadAll(c.Request.Body)
	if !strings.Contains(string(b), "keyword") {
		t.Fatalf("request body not restored for handler BindJSON, got %q", string(b))
	}
}

func TestResolveSearchPrincipal_AppBotDenied(t *testing.T) {
	ba := newSearchTestBotAPI()
	c, rec := newBotSearchCtx(t, `{}`)
	c.Set(CtxKeyRobotID, "app_1")
	c.Set(CtxKeyBotKind, BotKindApp)

	ba.resolveSearchPrincipal(c)

	if !c.IsAborted() {
		t.Fatalf("App Bot must be denied (决策五), chain must abort")
	}
	if got := messages_search.PrincipalKind(c); got != "" {
		t.Fatalf("App Bot must not set a search principal, got %q", got)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("App Bot denial must not return 200")
	}
}

func TestResolveSearchPrincipal_OBOAuthorizedSetsPrincipal(t *testing.T) {
	// A live grant (active=1, global_enabled=1) exists for (grantor, bot):
	// the entry fail-fast passes and an obo principal is set. Per-channel
	// scope/TOCTOU is left to the messages_search obo gate.
	s := newFakeOBOStore()
	gid, err := s.insertGrant("grantor_2", "bot_1", "auto", "")
	if err != nil {
		t.Fatalf("insertGrant: %v", err)
	}
	enable := 1
	if err := s.updateGrant(gid, "", &enable, nil); err != nil {
		t.Fatalf("updateGrant: %v", err)
	}
	ba := newBotAPIWithFakeStore(s)

	c, rec := newBotSearchCtx(t, `{"on_behalf_of":"grantor_2","keyword":"x"}`)
	c.Set(CtxKeyRobotID, "bot_1")
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.resolveSearchPrincipal(c)

	if c.IsAborted() {
		t.Fatalf("authorized OBO must not abort, body=%q", rec.Body.String())
	}
	if got := messages_search.PrincipalKind(c); got != "obo" {
		t.Fatalf("principal kind = %q, want obo", got)
	}
	// Body must still be readable by the downstream _search* handler.
	b, _ := io.ReadAll(c.Request.Body)
	if !strings.Contains(string(b), "keyword") {
		t.Fatalf("request body not restored for handler BindJSON, got %q", string(b))
	}
}

func TestResolveSearchPrincipal_OBONoGrantDenied(t *testing.T) {
	// No grant row at all → entry fail-fast rejects the whole request
	// (existence-hidden), never degrades to a 200 empty result.
	ba := newBotAPIWithFakeStore(newFakeOBOStore())

	c, rec := newBotSearchCtx(t, `{"on_behalf_of":"grantor_2","keyword":"x"}`)
	c.Set(CtxKeyRobotID, "bot_1")
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.resolveSearchPrincipal(c)

	if !c.IsAborted() {
		t.Fatalf("OBO with no grant must abort")
	}
	if got := messages_search.PrincipalKind(c); got != "" {
		t.Fatalf("unauthorized OBO must not set a principal, got %q", got)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("unauthorized OBO must not return 200")
	}
}

func TestResolveSearchPrincipal_OBOAppBotDenied(t *testing.T) {
	// App Bot cannot hold an OBO grant; it must be denied at principal
	// construction, not silently returned as "no results".
	s := newFakeOBOStore()
	gid, _ := s.insertGrant("grantor_2", "app_1", "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	ba := newBotAPIWithFakeStore(s)

	c, rec := newBotSearchCtx(t, `{"on_behalf_of":"grantor_2","keyword":"x"}`)
	c.Set(CtxKeyRobotID, "app_1")
	c.Set(CtxKeyBotKind, BotKindApp)

	ba.resolveSearchPrincipal(c)

	if !c.IsAborted() {
		t.Fatalf("App Bot OBO search must be denied (决策五), chain must abort")
	}
	if got := messages_search.PrincipalKind(c); got != "" {
		t.Fatalf("App Bot must not set a search principal, got %q", got)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("App Bot denial must not return 200")
	}
}

func TestResolveSearchPrincipal_OBOInfraErrorFailsClosed(t *testing.T) {
	// A DB error on the grant lookup must fail closed (abort, non-200), never
	// fall through to an unauthenticated / unscoped search.
	s := newFakeOBOStore()
	s.failFindActiveGrant = errors.New("db down")
	ba := newBotAPIWithFakeStore(s)

	c, rec := newBotSearchCtx(t, `{"on_behalf_of":"grantor_2","keyword":"x"}`)
	c.Set(CtxKeyRobotID, "bot_1")
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.resolveSearchPrincipal(c)

	if !c.IsAborted() {
		t.Fatalf("OBO infra error must fail closed (abort)")
	}
	if got := messages_search.PrincipalKind(c); got != "" {
		t.Fatalf("infra-error OBO must not set a principal, got %q", got)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("infra-error OBO must not return 200")
	}
}

func TestParseSearchOnBehalfOf(t *testing.T) {
	// present (trimmed) + body restored for the handler.
	c, _ := newBotSearchCtx(t, `{"on_behalf_of":"  g1  ","keyword":"k"}`)
	if got := parseSearchOnBehalfOf(c); got != "g1" {
		t.Fatalf("trimmed on_behalf_of = %q, want g1", got)
	}
	b, _ := io.ReadAll(c.Request.Body)
	if !strings.Contains(string(b), "keyword") {
		t.Fatalf("body must be restored after probe, got %q", string(b))
	}

	// absent → "".
	c2, _ := newBotSearchCtx(t, `{"keyword":"k"}`)
	if got := parseSearchOnBehalfOf(c2); got != "" {
		t.Fatalf("no on_behalf_of field = %q, want empty", got)
	}

	// invalid JSON → treated as no obo (handler re-binds and reports the error).
	c3, _ := newBotSearchCtx(t, `not json`)
	if got := parseSearchOnBehalfOf(c3); got != "" {
		t.Fatalf("invalid json on_behalf_of = %q, want empty", got)
	}

	// empty body → "".
	c4, _ := newBotSearchCtx(t, ``)
	if got := parseSearchOnBehalfOf(c4); got != "" {
		t.Fatalf("empty body on_behalf_of = %q, want empty", got)
	}
}
