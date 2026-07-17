package messages_search

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// asJSONString marshals a query Source for substring assertions.
func asJSONString(t *testing.T, q interface {
	Source() (any, error)
}) string {
	t.Helper()
	src, err := q.Source()
	if err != nil {
		t.Fatalf("Source(): %v", err)
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestApplySpaceIDScope_P2POnly is the regression guard for the P0 finding
// (PR #361 cross-Space DM disclosure): the spaceId term filter MUST be
// emitted for p2p (channel_type=1) so two Spaces sharing the same DM
// channel_id (fakeChannelID is loginUID-derived, not space-derived) are
// kept apart at the query layer.
func TestApplySpaceIDScope_P2PEmitsTermFilter(t *testing.T) {
	req := SearchMessagesReq{
		ChannelType: channelTypePerson,
		ChannelID:   "peer",
		Keyword:     "hi",
	}
	q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "fake-cid", "spaceX")
	body := asJSONString(t, q.(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"spaceId":"spaceX"`) {
		t.Fatalf("p2p DSL must filter by spaceId, got:\n%s", body)
	}
}

// TestApplySpaceIDScope_GroupBypassed — group/thread channels already encode
// the parent Space in their channel_id (the membership gate enforces active
// membership); adding a redundant spaceId filter would only mask
// indexer-mapping mismatches. Keep the DSL clean for those cases.
func TestApplySpaceIDScope_GroupBypassed(t *testing.T) {
	req := SearchMessagesReq{
		ChannelType: channelTypeGroup,
		ChannelID:   "G1",
		Keyword:     "hi",
	}
	q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "G1", "spaceX")
	body := asJSONString(t, q.(interface {
		Source() (any, error)
	}))
	if strings.Contains(body, `spaceId`) {
		t.Fatalf("group DSL must NOT include spaceId filter, got:\n%s", body)
	}
}

func TestApplySpaceIDScope_ThreadBypassed(t *testing.T) {
	req := SearchMessagesReq{
		ChannelType: channelTypeThread,
		ChannelID:   "G1____abcd",
		Keyword:     "hi",
	}
	q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "G1____abcd", "spaceX")
	body := asJSONString(t, q.(interface {
		Source() (any, error)
	}))
	if strings.Contains(body, `spaceId`) {
		t.Fatalf("thread DSL must NOT include spaceId filter, got:\n%s", body)
	}
}

// TestApplySpaceIDScope_P2PEmptySpaceIDIsNoOp — when the handler decides to
// proceed with an empty spaceID (escape hatch path), the helper itself must
// not emit a degenerate `spaceId:""` term that would match docs missing the
// field; it must skip the filter entirely.
func TestApplySpaceIDScope_P2PEmptySpaceIDIsNoOp(t *testing.T) {
	req := SearchMessagesReq{
		ChannelType: channelTypePerson,
		ChannelID:   "peer",
		Keyword:     "hi",
	}
	q, _ := buildSearchMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, "fake-cid", "")
	body := asJSONString(t, q.(interface {
		Source() (any, error)
	}))
	if strings.Contains(body, `spaceId`) {
		t.Fatalf("empty-spaceID p2p DSL must NOT include spaceId filter, got:\n%s", body)
	}
}

// TestApplySpaceIDScope_AcrossEndpoints — every search endpoint's DSL must
// route p2p through applySpaceIDScope. We pin all four explicitly because
// future copies of the handler skeleton are easy to drift.
func TestApplySpaceIDScope_AcrossEndpoints(t *testing.T) {
	ctx := context.Background()
	a := fallbackTestAnalyzer()

	mediaReq := SearchMediaReq{ChannelType: channelTypePerson, ChannelID: "peer"}
	if body := asJSONString(t, buildSearchMediaDSL(mediaReq, "fake", "S").(interface {
		Source() (any, error)
	})); !strings.Contains(body, `"spaceId":"S"`) {
		t.Errorf("search_media p2p DSL missing spaceId filter:\n%s", body)
	}
	filesReq := SearchFilesReq{ChannelType: channelTypePerson, ChannelID: "peer"}
	filesQ, _ := buildSearchFilesDSL(ctx, a, true, filesReq, "fake", "S")
	if body := asJSONString(t, filesQ.(interface {
		Source() (any, error)
	})); !strings.Contains(body, `"spaceId":"S"`) {
		t.Errorf("search_files p2p DSL missing spaceId filter:\n%s", body)
	}
	allReq := SearchAllReq{ChannelType: channelTypePerson, ChannelID: "peer", Keyword: "k"}
	allQ, _ := buildSearchAllDSL(ctx, a, true, allReq, "fake", "S")
	if body := asJSONString(t, allQ.(interface {
		Source() (any, error)
	})); !strings.Contains(body, `"spaceId":"S"`) {
		t.Errorf("search_all p2p DSL missing spaceId filter:\n%s", body)
	}
}

// newSpaceCtx builds a minimal wkhttp.Context with the given space_id (as if
// SpaceMiddleware had set it) for testing resolveP2PSpaceScope.
func newSpaceCtx(t *testing.T, spaceID string) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search", nil)
	if spaceID != "" {
		gc.Set("space_id", spaceID)
	}
	return &wkhttp.Context{Context: gc}, rec
}

func newSpaceHandler(requireSpaceID bool) *Handler {
	return &Handler{
		Log: log.NewTLog("messages_search-space-test"),
		cfg: SearchConfig{RequireSpaceID: requireSpaceID},
	}
}

// TestResolveP2PSpaceScope_GroupChannelsBypass — group/thread don't need a
// SpaceMiddleware-resolved spaceID; resolveP2PSpaceScope must short-circuit
// and let the handler proceed.
func TestResolveP2PSpaceScope_GroupChannelsBypass(t *testing.T) {
	h := newSpaceHandler(true)
	c, rec := newSpaceCtx(t, "")
	got, ok := h.resolveP2PSpaceScope(c, channelTypeGroup, "me")
	if !ok || got != "" {
		t.Fatalf("group must short-circuit to ('', true), got (%q, %v)", got, ok)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("group must not write a response: %q", rec.Body.String())
	}
}

// TestResolveP2PSpaceScope_P2PWithSpaceIDPasses — the happy path: p2p search
// with a SpaceMiddleware-resolved spaceID returns the spaceID to be plumbed
// into the DSL.
func TestResolveP2PSpaceScope_P2PWithSpaceIDPasses(t *testing.T) {
	h := newSpaceHandler(true)
	c, rec := newSpaceCtx(t, "spaceX")
	got, ok := h.resolveP2PSpaceScope(c, channelTypePerson, "me")
	if !ok || got != "spaceX" {
		t.Fatalf("expected ('spaceX', true), got (%q, %v)", got, ok)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("happy path must not write a response: %q", rec.Body.String())
	}
}

// TestResolveP2PSpaceScope_P2PMissingSpaceIDFailsClosed — fail-closed: when
// the env switch RequireSpaceID is on (default) and the request did not
// resolve a spaceID via SpaceMiddleware, the handler aborts with NOT_FOUND
// (resource=channel) — anti-enumeration, never a separate "missing space"
// signal that would let an attacker probe whether the peer exists.
func TestResolveP2PSpaceScope_P2PMissingSpaceIDFailsClosed(t *testing.T) {
	h := newSpaceHandler(true)
	c, rec := newSpaceCtx(t, "")
	got, ok := h.resolveP2PSpaceScope(c, channelTypePerson, "me")
	if ok {
		t.Fatalf("missing spaceID with fail-closed must abort, got ok=true")
	}
	if got != "" {
		t.Fatalf("missing spaceID must return empty string, got %q", got)
	}
	if rec.Body.Len() == 0 {
		t.Fatalf("fail-closed must write an error response")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "not found") {
		t.Fatalf("fail-closed must render NOT_FOUND envelope, got %q", body)
	}
}

// TestResolveP2PSpaceScope_EscapeHatch — when RequireSpaceID is explicitly
// disabled (rollout window before indexer writes payload.space_id and the
// existing corpus is backfilled), p2p search proceeds without a spaceID and
// the DSL skips the term filter. This is the only path that reproduces the
// pre-fix behaviour and must stay opt-in.
func TestResolveP2PSpaceScope_EscapeHatch(t *testing.T) {
	h := newSpaceHandler(false)
	c, rec := newSpaceCtx(t, "")
	got, ok := h.resolveP2PSpaceScope(c, channelTypePerson, "me")
	if !ok {
		t.Fatalf("escape hatch must allow p2p without spaceID")
	}
	if got != "" {
		t.Fatalf("escape hatch must return empty spaceID so DSL skips the filter, got %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("escape hatch must not write an error response: %q", rec.Body.String())
	}
}

// TestResolveP2PSpaceScope_P2PWhitespaceSpaceIDTreatedAsMissing_RequireTrue —
// defense-in-depth against an attacker bypassing SpaceMiddleware with a
// whitespace-only X-Space-ID / ?space_id (e.g. "   " or "\t"). Without
// trimming, an empty-but-non-"" spaceID would slip past the != "" check and
// reach OpenSearch as `term spaceId=" "`, returning 200 + 0 hits instead of
// the intended fail-closed NOT_FOUND. Must behave identically to the missing
// case.
func TestResolveP2PSpaceScope_P2PWhitespaceSpaceIDTreatedAsMissing_RequireTrue(t *testing.T) {
	for _, ws := range []string{"   ", "\t", " \t "} {
		h := newSpaceHandler(true)
		c, rec := newSpaceCtx(t, ws)
		got, ok := h.resolveP2PSpaceScope(c, channelTypePerson, "me")
		if ok {
			t.Fatalf("whitespace spaceID %q must abort with fail-closed, got ok=true", ws)
		}
		if got != "" {
			t.Fatalf("whitespace spaceID %q must return empty string, got %q", ws, got)
		}
		if !strings.Contains(rec.Body.String(), "not found") {
			t.Fatalf("whitespace spaceID %q must render NOT_FOUND envelope, got %q", ws, rec.Body.String())
		}
	}
}

// TestResolveP2PSpaceScope_P2PWhitespaceSpaceIDTreatedAsMissing_RequireFalse —
// in escape-hatch mode, a whitespace-only spaceID must take the same
// WARN-and-skip path as a missing one (not emit a degenerate spaceId=" "
// term filter downstream).
func TestResolveP2PSpaceScope_P2PWhitespaceSpaceIDTreatedAsMissing_RequireFalse(t *testing.T) {
	h := newSpaceHandler(false)
	c, rec := newSpaceCtx(t, "  ")
	got, ok := h.resolveP2PSpaceScope(c, channelTypePerson, "me")
	if !ok {
		t.Fatalf("escape hatch must allow whitespace spaceID through")
	}
	if got != "" {
		t.Fatalf("whitespace spaceID must be normalized to empty so DSL skips the filter, got %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("escape hatch must not write an error response: %q", rec.Body.String())
	}
}

// TestResolveP2PSpaceScope_BotBypassesGate_RequireTrue — YUJ-57: an as-bot
// principal carries no Space (bot routes have no SpaceMiddleware). Even with
// RequireSpaceID=true the p2p gate must NOT fail-close — the bot's DM
// reachability is bounded by the per-principal readable predicate, not a
// spaceId term. This is the single-DM fast path a global bot search collapses
// to; without the bypass it would 404 despite resolveGlobalScope letting it in.
func TestResolveP2PSpaceScope_BotBypassesGate_RequireTrue(t *testing.T) {
	h := newSpaceHandler(true)
	c, rec := newSpaceCtx(t, "")
	setPrincipal(c, userBotPrincipal{botUID: "bot9"})
	got, ok := h.resolveP2PSpaceScope(c, channelTypePerson, "bot9")
	if !ok {
		t.Fatalf("as-bot p2p must bypass the space fail-close, got ok=false")
	}
	if got != "" {
		t.Fatalf("as-bot has no Space; expected empty spaceID, got %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("bot bypass must not write a response: %q", rec.Body.String())
	}
}

// TestResolveP2PSpaceScope_UKStillGated_RequireTrue — the exemption is scoped
// to space-less principals. A uk principal is real-user-semantic and space
// scoped; if its Space is empty it must STILL fail-close under RequireSpaceID,
// exactly like a plain web user (regression guard against over-broadening the
// YUJ-57 bypass).
func TestResolveP2PSpaceScope_UKStillGated_RequireTrue(t *testing.T) {
	h := newSpaceHandler(true)
	c, rec := newSpaceCtx(t, "")
	setPrincipal(c, ukPrincipal{keyUID: "kate"}) // empty api_key_space_id
	got, ok := h.resolveP2PSpaceScope(c, channelTypePerson, "kate")
	if ok {
		t.Fatalf("uk with empty Space must still fail-close, got ok=true")
	}
	if got != "" {
		t.Fatalf("fail-close must return empty spaceID, got %q", got)
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("uk fail-close must render NOT_FOUND, got %q", rec.Body.String())
	}
}
