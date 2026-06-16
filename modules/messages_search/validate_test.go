package messages_search

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// newValidateCtx builds a minimal wkhttp.Context whose writes land in a
// recorder so validateBase's error envelope can be inspected.
func newValidateCtx(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search", nil)
	return &wkhttp.Context{Context: gc}, rec
}

// V7 (page-size half) — page_size above the 100 ceiling must be rejected with
// VALIDATION_ERROR rather than silently clamped (a silent clamp would let a
// caller think they paged 500 and miss results). Mirrors the plan's
// "page-size 上限 100" gate.
func TestValidateBase_PageSizeOverMaxRejected(t *testing.T) {
	c, rec := newValidateCtx(t)
	_, ok := validateBase(c, SearchConfig{}, channelTypeGroup, "G1", "", "",
		SearchFilters{}, maxPageSize+1, true)
	if ok {
		t.Fatalf("page_size %d (> %d) must be rejected", maxPageSize+1, maxPageSize)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("rejected page_size must render a VALIDATION_ERROR envelope")
	}
}

func TestValidateBase_PageSizeZeroDefaults(t *testing.T) {
	c, _ := newValidateCtx(t)
	page, ok := validateBase(c, SearchConfig{}, channelTypeGroup, "G1", "", "",
		SearchFilters{}, 0, true)
	if !ok {
		t.Fatalf("page_size 0 must default, not reject")
	}
	if page != defaultPage {
		t.Fatalf("page_size 0 must default to %d, got %d", defaultPage, page)
	}
}

func TestValidateBase_PageSizeAtMaxAccepted(t *testing.T) {
	c, _ := newValidateCtx(t)
	page, ok := validateBase(c, SearchConfig{}, channelTypeGroup, "G1", "", "",
		SearchFilters{}, maxPageSize, true)
	if !ok {
		t.Fatalf("page_size at the %d ceiling must be accepted", maxPageSize)
	}
	if page != maxPageSize {
		t.Fatalf("page_size %d must pass through unchanged, got %d", maxPageSize, page)
	}
}

func TestValidateBase_PageSizeNegativeRejected(t *testing.T) {
	c, rec := newValidateCtx(t)
	_, ok := validateBase(c, SearchConfig{}, channelTypeGroup, "G1", "", "",
		SearchFilters{}, -1, true)
	if ok {
		t.Fatalf("negative page_size must be rejected")
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("rejected page_size must render an error envelope")
	}
}

// V2-adjacent — channel_type outside the {1,2,5} whitelist is rejected before
// any access check runs (None/CS/Community/Info channels are not openable this
// phase; the plan says None must be explicitly refused).
func TestValidateBase_UnknownChannelTypeRejected(t *testing.T) {
	for _, ct := range []uint8{0, 3, 4, 99} {
		c, rec := newValidateCtx(t)
		_, ok := validateBase(c, SearchConfig{}, ct, "X", "", "",
			SearchFilters{}, 20, true)
		if ok {
			t.Fatalf("channel_type %d must be rejected", ct)
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("channel_type %d rejection must render an error envelope", ct)
		}
	}
}

// A malformed cursor must surface VALIDATION_ERROR(field=cursor), never be
// silently treated as a first-page request (which would reset pagination and
// leak the first page on a tampered cursor).
func TestValidateBase_MalformedCursorRejected(t *testing.T) {
	c, rec := newValidateCtx(t)
	_, ok := validateBase(c, SearchConfig{}, channelTypeGroup, "G1", "", "not-a-valid-cursor!!",
		SearchFilters{}, 20, true)
	if ok {
		t.Fatalf("malformed cursor must be rejected")
	}
	if !strings.Contains(rec.Body.String(), "cursor") &&
		!strings.Contains(strings.ToLower(rec.Body.String()), "invalid") {
		t.Fatalf("cursor rejection must render a validation envelope, got %q", rec.Body.String())
	}
}
