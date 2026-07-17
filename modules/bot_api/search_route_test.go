package bot_api

import (
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

// YUJ-49 (#B) — bot search entry: resolveSearchPrincipal distinguishes as-bot
// (no on_behalf_of) from as-user(OBO) (on_behalf_of present), and App Bot is
// explicitly denied. The OBO real-permission check (grant/scope/TOCTOU) is #F;
// until it wires validateSearchOBO, on_behalf_of搜索 fails closed.

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

func TestResolveSearchPrincipal_OBOFailClosedUntilF(t *testing.T) {
	ba := newSearchTestBotAPI()
	c, rec := newBotSearchCtx(t, `{"on_behalf_of":"grantor_2","keyword":"x"}`)
	c.Set(CtxKeyRobotID, "bot_1")
	c.Set(CtxKeyBotKind, BotKindUser)

	ba.resolveSearchPrincipal(c)

	if !c.IsAborted() {
		t.Fatalf("on_behalf_of search must fail closed until #F wires validateSearchOBO")
	}
	if got := messages_search.PrincipalKind(c); got != "" {
		t.Fatalf("fail-closed OBO must not set a principal, got %q", got)
	}
	if rec.Code == http.StatusOK {
		t.Fatalf("fail-closed OBO must not return 200")
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
