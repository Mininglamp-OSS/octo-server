package botfather

import (
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

func newUKSearchCtx(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/user/messages/_search", nil)
	return &wkhttp.Context{Context: gc}, rec
}

func TestResolveUKPrincipal_DirectRealUser(t *testing.T) {
	bf := &BotFather{Log: log.NewTLog("uk-search-test")}
	c, rec := newUKSearchCtx(t)
	c.Set("api_key_uid", "u_9")
	c.Set("api_key_space_id", "sp_1")

	bf.resolveUKPrincipal(c)

	if c.IsAborted() {
		t.Fatalf("uk with valid key must not abort, body=%q", rec.Body.String())
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
