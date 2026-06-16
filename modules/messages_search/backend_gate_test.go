package messages_search

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/searchbackend"
	"github.com/gin-gonic/gin"
)

func newBackendGateCtx(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search", nil)
	return &wkhttp.Context{Context: gc}, rec
}

// V6 — when the resolved mode does not serve ES (disabled or zinc), the
// _search* surface must refuse uniformly with SEARCH_DISABLED rather than reach
// OS, and must Abort so no downstream handler runs.
func TestBackendGate_RefusesWhenESOff(t *testing.T) {
	modes := []searchbackend.Mode{
		{ESServe: false, LegacyZinc: false, Declared: searchbackend.DeclaredDisabled},
		{ESServe: false, LegacyZinc: true, Declared: searchbackend.DeclaredZinc},
	}
	for _, m := range modes {
		h := &Handler{Log: log.NewTLog("backend-gate-test"), mode: m}
		c, rec := newBackendGateCtx(t)
		h.backendGate()(c)
		if !c.IsAborted() {
			t.Fatalf("mode %+v: gate must Abort the chain", m)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("mode %+v: gate must render a SEARCH_DISABLED envelope", m)
		}
		body := rec.Body.String()
		if !strings.Contains(strings.ToLower(body), "disabled") &&
			!strings.Contains(body, "未启用") &&
			!strings.Contains(body, "not enabled") {
			t.Fatalf("mode %+v: expected SEARCH_DISABLED envelope, got %q", m, body)
		}
	}
}

// V6 — under a mode that serves ES the gate is transparent.
func TestBackendGate_PassesWhenESServes(t *testing.T) {
	h := &Handler{Log: log.NewTLog("backend-gate-test"), mode: searchbackend.Mode{ESServe: true}}
	c, rec := newBackendGateCtx(t)
	h.backendGate()(c)
	if c.IsAborted() {
		t.Fatalf("ES-serving mode: gate must NOT abort")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("ES-serving mode: gate must not write a body, got %q", rec.Body.String())
	}
}
