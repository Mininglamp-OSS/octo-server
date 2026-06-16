package messages_search

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

func newDepthCtx(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search", nil)
	return &wkhttp.Context{Context: gc}, rec
}

func newDepthHandler() *Handler {
	return &Handler{Log: log.NewTLog("depth-cap-test"), cfg: SearchConfig{}}
}

// V7 depth cap — a cursor whose cumulative depth has reached the cap is
// rejected with DEPTH_EXCEEDED before any OS round-trip.
func TestResolveCursorDepth_AtCapRejected(t *testing.T) {
	h := newDepthHandler()
	cur := encodeCursorWithDepth(h.cfg, 1717000000, 42, nil, maxPaginationDepth)
	c, rec := newDepthCtx(t)
	_, ok := h.resolveCursorDepth(c, cur, 1)
	if ok {
		t.Fatalf("cursor at the depth cap must be rejected")
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "depth") &&
		!strings.Contains(rec.Body.String(), "最大深度") {
		t.Fatalf("expected DEPTH_EXCEEDED envelope, got %q", rec.Body.String())
	}
}

func TestResolveCursorDepth_OverCapRejected(t *testing.T) {
	h := newDepthHandler()
	cur := encodeCursorWithDepth(h.cfg, 1717000000, 42, nil, maxPaginationDepth+500)
	c, rec := newDepthCtx(t)
	if _, ok := h.resolveCursorDepth(c, cur, 1); ok {
		t.Fatalf("cursor over the depth cap must be rejected")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("over-cap cursor must render an error envelope")
	}
}

func TestResolveCursorDepth_UnderCapAllowed(t *testing.T) {
	h := newDepthHandler()
	c, _ := newDepthCtx(t)
	if d, ok := h.resolveCursorDepth(c, "", 20); !ok || d != 0 {
		t.Fatalf("empty cursor must allow with depth 0, got (%d,%v)", d, ok)
	}
	cur := encodeCursorWithDepth(h.cfg, 1717000000, 42, nil, 100)
	c2, _ := newDepthCtx(t)
	if d, ok := h.resolveCursorDepth(c2, cur, 20); !ok || d != 100 {
		t.Fatalf("shallow cursor must allow and report depth 100, got (%d,%v)", d, ok)
	}
}

// V7 — THE anti-bypass case (codex P2). The cap is checked against
// priorDepth + pageSize, so a caller just under the cap cannot pick a larger
// page_size to read past it.
func TestDepthCap_LargerPageSizeCannotOverrun(t *testing.T) {
	h := newDepthHandler()
	nearCap := encodeCursorWithDepth(h.cfg, 1717000000, 42, nil, maxPaginationDepth-1)

	c, rec := newDepthCtx(t)
	if _, ok := h.resolveCursorDepth(c, nearCap, 100); ok {
		t.Fatalf("depth=%d + page_size=100 overruns the cap and must be rejected", maxPaginationDepth-1)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("overrun must render DEPTH_EXCEEDED")
	}

	c2, _ := newDepthCtx(t)
	if _, ok := h.resolveCursorDepth(c2, nearCap, 1); !ok {
		t.Fatalf("depth=%d + page_size=1 lands exactly at the cap and must be allowed", maxPaginationDepth-1)
	}
}

// V7 — shrinking page_size cannot bypass the cap once the cursor's cumulative
// depth is already at/over it.
func TestDepthCap_SmallPageSizeCannotBypassAtCap(t *testing.T) {
	h := newDepthHandler()
	atCap := encodeCursorWithDepth(h.cfg, 1717000000, 42, nil, maxPaginationDepth)
	c, rec := newDepthCtx(t)
	if _, ok := h.resolveCursorDepth(c, atCap, 1); ok {
		t.Fatalf("shrinking page_size must NOT bypass the cumulative depth cap")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("anti-bypass path must render DEPTH_EXCEEDED")
	}
}

// HMAC-signed depth: tampering invalidates the signature → rejected.
func TestDepthCap_TamperedDepthRejected(t *testing.T) {
	h := newDepthHandler()
	cur := encodeCursorWithDepth(h.cfg, 1717000000, 42, nil, maxPaginationDepth)
	b := []byte(cur)
	b[len(b)/2] ^= 0x01
	c, rec := newDepthCtx(t)
	if _, ok := h.resolveCursorDepth(c, string(b), 20); ok {
		t.Fatalf("tampered cursor must be rejected")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("tampered cursor must render an error envelope")
	}
}
