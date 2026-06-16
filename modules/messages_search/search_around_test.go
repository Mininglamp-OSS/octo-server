package messages_search

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/Mininglamp-OSS/octo-server/pkg/searchbackend"
	"github.com/olivere/elastic"
)

func aroundHitWithID(id int64) *elastic.SearchHit {
	body, _ := json.Marshal(map[string]any{
		"messageId": id,
		"timestamp": int64(1717000000 + id),
		"from":      "u1",
	})
	src := json.RawMessage(body)
	return &elastic.SearchHit{Source: &src, Sort: []any{float64(1717000000 + id), float64(id)}}
}

// reverseHits must reverse order without aliasing the input.
func TestReverseHits(t *testing.T) {
	in := []*elastic.SearchHit{aroundHitWithID(1), aroundHitWithID(2), aroundHitWithID(3)}
	out := reverseHits(in)
	if len(out) != 3 {
		t.Fatalf("len = %d", len(out))
	}
	if lastHitMessageID(out[0]) != 3 || lastHitMessageID(out[2]) != 1 {
		t.Fatalf("reverse order wrong: %d..%d", lastHitMessageID(out[0]), lastHitMessageID(out[2]))
	}
	// input untouched
	if lastHitMessageID(in[0]) != 1 {
		t.Fatalf("reverseHits mutated input")
	}
}

// V8-a window assembly: before (oldest-first) + anchor + after (oldest-first)
// is sliced back into the AroundResult shape with correct anchor placement.
func TestSplitAroundWindow(t *testing.T) {
	h := &Handler{cfg: SearchConfig{}}
	mk := func(id int64) MessageHit { return h.singleMessageHit(Doc{MessageID: id, From: "u1"}, "C1", nil) }
	hits := []MessageHit{mk(1), mk(2), mk(3), mk(4), mk(5)} // before=[1,2], anchor=3, after=[4,5]
	res := splitAroundWindow(hits, 2, 2, true, false)
	if len(res.Before) != 2 || res.Before[0].MessageID != "1" || res.Before[1].MessageID != "2" {
		t.Fatalf("before wing wrong: %+v", res.Before)
	}
	if res.Anchor.MessageID != "3" {
		t.Fatalf("anchor wrong: %s", res.Anchor.MessageID)
	}
	if len(res.After) != 2 || res.After[0].MessageID != "4" || res.After[1].MessageID != "5" {
		t.Fatalf("after wing wrong: %+v", res.After)
	}
	if !res.HasMoreBefore || res.HasMoreAfter {
		t.Fatalf("has_more flags not propagated: %+v/%+v", res.HasMoreBefore, res.HasMoreAfter)
	}
}

// When the before wing is empty the anchor is the first element.
func TestSplitAroundWindow_NoBefore(t *testing.T) {
	h := &Handler{cfg: SearchConfig{}}
	mk := func(id int64) MessageHit { return h.singleMessageHit(Doc{MessageID: id}, "C1", nil) }
	hits := []MessageHit{mk(3), mk(4)} // anchor=3, after=[4]
	res := splitAroundWindow(hits, 0, 1, false, false)
	if len(res.Before) != 0 {
		t.Fatalf("before should be empty")
	}
	if res.Anchor.MessageID != "3" {
		t.Fatalf("anchor wrong: %s", res.Anchor.MessageID)
	}
	if len(res.After) != 1 || res.After[0].MessageID != "4" {
		t.Fatalf("after wrong: %+v", res.After)
	}
}

// buildAroundDSL must carry the spaceId term for p2p (so a cross-Space window
// can't be assembled) and exclude cmd messages; it must NOT carry a keyword.
func TestBuildAroundDSL_P2PSpaceScoped(t *testing.T) {
	req := SearchAroundReq{ChannelType: channelTypePerson, ChannelID: "peer"}
	body := asJSONString(t, buildAroundDSL(req, "fake-cid", "spaceX").(interface {
		Source() (any, error)
	}))
	if !strings.Contains(body, `"spaceId":"spaceX"`) {
		t.Fatalf("p2p around DSL must filter by spaceId, got:\n%s", body)
	}
	if !strings.Contains(body, "channelId") {
		t.Fatalf("around DSL must filter by channelId, got:\n%s", body)
	}
}

func TestBuildAroundDSL_GroupNoSpaceTerm(t *testing.T) {
	req := SearchAroundReq{ChannelType: channelTypeGroup, ChannelID: "G1"}
	body := asJSONString(t, buildAroundDSL(req, "G1", "spaceX").(interface {
		Source() (any, error)
	}))
	if strings.Contains(body, "spaceId") {
		t.Fatalf("group around DSL must NOT carry spaceId term, got:\n%s", body)
	}
}

func newAroundCtx(t *testing.T, bodyJSON string) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search_around", strings.NewReader(bodyJSON))
	gc.Request.Header.Set("Content-Type", "application/json")
	gc.Set("uid", "me")
	return &wkhttp.Context{Context: gc}, rec
}

// V8-b adjacent — a non-numeric / non-positive anchor_message_id is rejected
// with VALIDATION_ERROR before any access check or OS round-trip.
func TestSearchAround_InvalidAnchorRejected(t *testing.T) {
	h := &Handler{Log: log.NewTLog("around-test"), cfg: SearchConfig{}, mode: searchbackend.Mode{ESServe: true}}
	for _, anchor := range []string{"", "abc", "0", "-5"} {
		c, rec := newAroundCtx(t, `{"channel_type":2,"channel_id":"G1","anchor_message_id":"`+anchor+`"}`)
		h.searchAround(c)
		if rec.Code == 200 && !strings.Contains(rec.Body.String(), "error") {
			t.Fatalf("anchor %q must be rejected, got %d %q", anchor, rec.Code, rec.Body.String())
		}
		if rec.Body.Len() == 0 {
			t.Fatalf("anchor %q rejection must render an envelope", anchor)
		}
	}
}

// channel_type outside the whitelist is rejected (None/CS/etc not openable).
func TestSearchAround_BadChannelTypeRejected(t *testing.T) {
	h := &Handler{Log: log.NewTLog("around-test"), cfg: SearchConfig{}, mode: searchbackend.Mode{ESServe: true}}
	c, rec := newAroundCtx(t, `{"channel_type":3,"channel_id":"X","anchor_message_id":"42"}`)
	h.searchAround(c)
	if rec.Body.Len() == 0 {
		t.Fatalf("bad channel_type must render an error envelope")
	}
}

// V8-b — the anchor lookup must exclude command messages and revoked docs,
// exactly like the window DSL, so a caller cannot anchor on a payload.type=99
// command (or a revoked doc) the surrounding stream would never show.
func TestBuildAnchorDSL_ExcludesCmdAndRevoked(t *testing.T) {
	req := SearchAroundReq{ChannelType: channelTypeGroup, ChannelID: "G1"}
	body := asJSONString(t, buildAnchorDSL(req, "G1", "", 42))
	if !strings.Contains(body, `"messageId":42`) {
		t.Fatalf("anchor DSL must pin the messageId, got:\n%s", body)
	}
	if !strings.Contains(body, "must_not") {
		t.Fatalf("anchor DSL must carry must_not exclusions, got:\n%s", body)
	}
	if !strings.Contains(body, `"payload.type":99`) {
		t.Fatalf("anchor DSL must exclude command (payload.type=99), got:\n%s", body)
	}
	if !strings.Contains(body, `"revoked":true`) {
		t.Fatalf("anchor DSL must exclude revoked docs, got:\n%s", body)
	}
}

// p2p anchor lookup carries the spaceId term so a cross-Space anchor cannot be
// located (returns 0 hits → NOT_FOUND).
func TestBuildAnchorDSL_P2PSpaceScoped(t *testing.T) {
	req := SearchAroundReq{ChannelType: channelTypePerson, ChannelID: "peer"}
	body := asJSONString(t, buildAnchorDSL(req, "fake-cid", "spaceX", 7))
	if !strings.Contains(body, `"spaceId":"spaceX"`) {
		t.Fatalf("p2p anchor DSL must be Space-scoped, got:\n%s", body)
	}
}
