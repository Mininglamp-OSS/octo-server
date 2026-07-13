package messages_search

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/gin-gonic/gin"
	"github.com/olivere/elastic"
)

// buildGlobalReq returns a minimal keyword-mode global-messages request so
// each shape test only has to override the fields it cares about.
func buildGlobalReq(keyword string, filters GlobalSearchFilters) SearchGlobalMessagesReq {
	return SearchGlobalMessagesReq{Keyword: keyword, Filters: filters}
}

func TestBuildGlobalMessagesDSL_KeywordWhitelist(t *testing.T) {
	req := buildGlobalReq("hello", GlobalSearchFilters{})
	q, _ := buildGlobalMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, []string{"gA", "gB"}, "S1")
	body := marshalDSL(t, q)
	// keyword path: hard whitelist [1,8,11,14] (image/video excluded).
	for _, want := range []string{
		`"channelId":["gA","gB"]`,
		`"revoked":true`,
		`"payload.type":[1,8,11,14]`,
		`"multi_match"`,
		`"payload.file.name^2"`,
		`"payload.text.content^3"`,
		// DM double-guard leaves a should([mustNot(channelType=1),
		// filter(channelType=1,spaceId=S1)]) MSM=1 scope.
		`"spaceId":"S1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("DSL missing %q in:\n%s", want, body)
		}
	}
	// Image / video only appear on the browse path.
	if strings.Contains(body, `"payload.type":[1,8,11,14,2,5]`) {
		t.Errorf("keyword path must not include image/video: %s", body)
	}
}

func TestBuildGlobalMessagesDSL_BrowseAddsMedia(t *testing.T) {
	req := buildGlobalReq("", GlobalSearchFilters{SenderIDs: []string{"u1"}})
	q, _ := buildGlobalMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, []string{"gA"}, "")
	body := marshalDSL(t, q)
	// Empty keyword browse: whitelist [1,8,11,14,2,5]. Order matters — we
	// append image then video, so pin the exact terms shape.
	if !strings.Contains(body, `"payload.type":[1,8,11,14,2,5]`) {
		t.Errorf("browse path must include image+video whitelist; got: %s", body)
	}
}

func TestBuildGlobalMessagesDSL_ContentTypesIntersection(t *testing.T) {
	// Keyword path: content_types=[2] intersects to empty (image not in
	// keyword whitelist). DSL should synthesise a match-none term.
	reqKeyword := buildGlobalReq("hi", GlobalSearchFilters{ContentTypes: []int{payloadTypeImage}})
	q, _ := buildGlobalMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, reqKeyword, []string{"gA"}, "")
	bodyK := marshalDSL(t, q)
	if !strings.Contains(bodyK, `"match_none"`) {
		t.Errorf("empty content_types intersection must match no docs; got: %s", bodyK)
	}
	// Browse path: content_types=[8] narrows to just files.
	reqBrowse := buildGlobalReq("", GlobalSearchFilters{ContentTypes: []int{payloadTypeFile}, SenderIDs: []string{"u"}})
	q2, _ := buildGlobalMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, reqBrowse, []string{"gA"}, "")
	bodyB := marshalDSL(t, q2)
	if !strings.Contains(bodyB, `"payload.type":[8]`) {
		t.Errorf("content_types=[8] must narrow to just files; got: %s", bodyB)
	}
}

func TestBuildGlobalMessagesDSL_ChannelTypesFilter(t *testing.T) {
	req := buildGlobalReq("hi", GlobalSearchFilters{ChannelTypes: []uint8{1, 2}})
	q, _ := buildGlobalMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, []string{"gA"}, "")
	body := marshalDSL(t, q)
	if !strings.Contains(body, `"channelType":[1,2]`) {
		t.Errorf("channel_types filter missing: %s", body)
	}
}

func TestBuildGlobalMessagesDSL_DMSpaceScopeOmitWhenEmpty(t *testing.T) {
	req := buildGlobalReq("hi", GlobalSearchFilters{})
	q, _ := buildGlobalMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, []string{"gA"}, "")
	body := marshalDSL(t, q)
	if strings.Contains(body, `"spaceId"`) {
		t.Errorf("empty spaceID must not emit spaceId term (double-guard is DM-only): %s", body)
	}
}

func TestBuildGlobalFilesDSL_Shape(t *testing.T) {
	req := SearchGlobalFilesReq{
		Keyword: "合同",
		Filters: GlobalFileFilters{
			FileExts:     []string{"PDF", "docx", "PDF"}, // exercise lowercase + dedup
			FileSizeMin:  1024,
			FileSizeMax:  10240,
			ChannelTypes: []uint8{2},
			SenderIDs:    []string{"u1"},
		},
	}
	q, _ := buildGlobalFilesDSL(context.Background(), fallbackTestAnalyzer(), true, req, []string{"gA", "gB"}, "S1")
	body := marshalDSL(t, q)
	for _, want := range []string{
		`"channelId":["gA","gB"]`,
		`"revoked":true`,
		`"payload.type":8`,
		`"payload.file.extension":["pdf","docx"]`,
		`"payload.file.size"`,
		`"from":1024`,
		`"to":10240`,
		`"channelType":[2]`,
		`"payload.file.name^2"`,
		`"payload.file.caption"`,
		`"合同"`,
		`"spaceId":"S1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("files DSL missing %q in:\n%s", want, body)
		}
	}
}

func TestBuildGlobalFilesDSL_NoKeywordDropsMustClause(t *testing.T) {
	req := SearchGlobalFilesReq{Filters: GlobalFileFilters{SenderIDs: []string{"u"}}}
	q, _ := buildGlobalFilesDSL(context.Background(), fallbackTestAnalyzer(), true, req, []string{"gA"}, "")
	body := marshalDSL(t, q)
	if strings.Contains(body, `"multi_match"`) {
		t.Errorf("empty keyword must not emit multi_match: %s", body)
	}
}

// TestBuildGlobalFilesDSL_KeywordIncludesContentField pins M3: the
// cross-channel file endpoint's keyword multi_match must reach into
// payload.file.content so a body-only hit surfaces alongside name / caption
// matches.
func TestBuildGlobalFilesDSL_KeywordIncludesContentField(t *testing.T) {
	req := SearchGlobalFilesReq{Keyword: "report"}
	q, _ := buildGlobalFilesDSL(context.Background(), fallbackTestAnalyzer(), true, req, []string{"gA"}, "")
	body := marshalDSL(t, q)
	if !strings.Contains(body, `"payload.file.content"`) {
		t.Errorf("_search_global_files keyword DSL must include payload.file.content:\n%s", body)
	}
	if strings.Contains(body, `"payload.file.content^`) {
		t.Errorf("_search_global_files content field must carry default weight (no ^N boost):\n%s", body)
	}
}

// TestBuildGlobalMessagesDSL_FileClauseIncludesContentField pins M4: the
// unified feed's keyword-path fileClause reaches into payload.file.content so
// body-only hits participate in the merged relevance ranking.
func TestBuildGlobalMessagesDSL_FileClauseIncludesContentField(t *testing.T) {
	req := buildGlobalReq("report", GlobalSearchFilters{})
	q, _ := buildGlobalMessagesDSL(context.Background(), fallbackTestAnalyzer(), true, req, []string{"gA"}, "")
	body := marshalDSL(t, q)
	if !strings.Contains(body, `"payload.file.content"`) {
		t.Errorf("_search_global_messages fileClause must include payload.file.content:\n%s", body)
	}
	if strings.Contains(body, `"payload.file.content^`) {
		t.Errorf("_search_global_messages content field must carry default weight:\n%s", body)
	}
}

// P0 wire shape: MessageHit surfaces channel_type when set; FileHit surfaces
// channel_id + channel_type. Empty values omit via omitempty so single-channel
// callers stay byte-identical.
func TestGlobalHitShape_ChannelTypeAndIDPresent(t *testing.T) {
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(4, 0)}
	// Message hit, group. channelType=2 must appear on the wire.
	tp := payloadTypeText
	doc := Doc{MessageID: 1, Payload: &Payload{Type: &tp, Text: &TextPayload{Content: "x"}}}
	mh := h.singleMessageHit(doc, "gA", channelTypeGroup, nil)
	if b, _ := json.Marshal(mh); !strings.Contains(string(b), `"channel_type":2`) || !strings.Contains(string(b), `"channel_id":"gA"`) {
		t.Errorf("MessageHit must carry channel_id+channel_type: %s", b)
	}
	// File hit for a group.
	fp := payloadTypeFile
	docFile := Doc{MessageID: 2, Payload: &Payload{Type: &fp, File: &FilePayload{Name: "a.pdf"}}}
	fh := h.singleFileHit(docFile, "gA", channelTypeGroup)
	if b, _ := json.Marshal(fh); !strings.Contains(string(b), `"channel_id":"gA"`) || !strings.Contains(string(b), `"channel_type":2`) {
		t.Errorf("FileHit must carry channel_id+channel_type: %s", b)
	}
	// Legacy single-channel call site (channelType=0) → both fields omitted.
	mhLegacy := h.singleMessageHit(doc, "gA", 0, nil)
	if b, _ := json.Marshal(mhLegacy); strings.Contains(string(b), `"channel_type"`) {
		t.Errorf("legacy call with channelType=0 must omit channel_type: %s", b)
	}
}

// DM projection reverses fakeChannelID → peer uid via wireChannelFromDoc so
// the frontend can jump into the DM. Uses the actual OS-side fakeChannelID
// format ("uidA@uidB", sorted) — we don't hardcode the sort so a helper does
// it for us and stays aligned with the indexer.
func TestWireChannelFromDoc_DMReversal(t *testing.T) {
	loginUID := "me"
	peerUID := "peer"
	fake := fakeChannelIDFor(loginUID, peerUID)
	doc := Doc{ChannelID: fake, ChannelType: uint32(channelTypePerson)}
	id, ct := wireChannelFromDoc(doc, loginUID)
	if id != peerUID {
		t.Errorf("DM channel_id must reverse to peer uid: got %q, want %q", id, peerUID)
	}
	if ct != channelTypePerson {
		t.Errorf("DM channel_type must be 1; got %d", ct)
	}
	// Group hits echo the OS channelId unchanged.
	docGroup := Doc{ChannelID: "gA", ChannelType: uint32(channelTypeGroup)}
	id2, ct2 := wireChannelFromDoc(docGroup, loginUID)
	if id2 != "gA" || ct2 != channelTypeGroup {
		t.Errorf("group channel echo mismatch: %s/%d", id2, ct2)
	}
}

func TestPeerFromFakeChannelID_Defensive(t *testing.T) {
	// Well-formed
	fake := fakeChannelIDFor("me", "peer")
	if got := peerFromFakeChannelID(fake, "me"); got != "peer" {
		t.Errorf("got %q, want peer", got)
	}
	// If loginUID sits on the right side of the sort, we still return the
	// other party.
	if got := peerFromFakeChannelID(fake, "peer"); got != "me" {
		t.Errorf("got %q, want me", got)
	}
	// Malformed → return original as-is (defensive).
	if got := peerFromFakeChannelID("not-a-fake-id", "me"); got != "not-a-fake-id" {
		t.Errorf("malformed must be echoed: %q", got)
	}
	// Neither side matches → return sorted first party (defensive).
	if got := peerFromFakeChannelID("a@b", "z"); got != "a" {
		t.Errorf("unknown-uid pick must be deterministic; got %q", got)
	}
}

// Multi-channel filterVisible: each hit consults its own room's clear-history
// offset. Two channels with different offsets → different pass/fail decisions
// on the same messageSeq.
func TestFilterVisible_MultiChannelOffsets(t *testing.T) {
	probe := &stubProbe{
		offsetByUC: map[string]uint32{
			"me:room-a": 100,
			"me:room-b": 5,
		},
	}
	h := newVisibilityHandler(probe)
	refs := []msgRef{
		{MessageID: "1", MessageSeq: 50, ChannelID: "room-a"},  // <= offset(100) → drop
		{MessageID: "2", MessageSeq: 50, ChannelID: "room-b"},  // > offset(5)  → keep
		{MessageID: "3", MessageSeq: 200, ChannelID: "room-a"}, // > offset(100) → keep
	}
	keep, err := h.filterVisible(context.Background(), "me", "", refs)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if _, ok := keep["1"]; ok {
		t.Errorf("room-a seq 50 should be dropped (offset 100)")
	}
	if _, ok := keep["2"]; !ok {
		t.Errorf("room-b seq 50 should survive (offset 5)")
	}
	if _, ok := keep["3"]; !ok {
		t.Errorf("room-a seq 200 should survive (offset 100)")
	}
}

// ChannelOffsets fail-closed: any DB error surfaces as filterVisible error and
// no hits are released.
func TestFilterVisible_MultiChannelOffsetError_FailClosed(t *testing.T) {
	probe := &stubProbe{offsetErr: errNoDB}
	h := newVisibilityHandler(probe)
	_, err := h.filterVisible(context.Background(), "me", "",
		[]msgRef{{MessageID: "1", ChannelID: "room-a"}})
	if err == nil {
		t.Fatalf("ChannelOffsets error must propagate")
	}
}

// projectDocRef must fill msgRef.ChannelID from doc.ChannelID so multi-channel
// filterVisible has the right per-hit key. Falls back to reqChannelID only
// when doc.ChannelID is missing.
func TestProjectDocRef_ChannelIDFromDoc(t *testing.T) {
	doc := Doc{MessageID: 42, MessageSeq: 7, ChannelID: "room-a"}
	src := json.RawMessage(mustJSON(doc))
	hit := &elastic.SearchHit{Source: &src}
	proj := projectDocRef("fallback-req", "")
	ref, ok := proj(hit)
	if !ok {
		t.Fatalf("project failed")
	}
	if ref.ChannelID != "room-a" {
		t.Errorf("doc.ChannelID must win over reqChannelID: got %q", ref.ChannelID)
	}
	// Fallback when doc.ChannelID is empty.
	doc2 := Doc{MessageID: 43, MessageSeq: 8}
	src2 := json.RawMessage(mustJSON(doc2))
	hit2 := &elastic.SearchHit{Source: &src2}
	ref2, ok := proj(hit2)
	if !ok || ref2.ChannelID != "fallback-req" {
		t.Errorf("missing doc.ChannelID must fall back to req: %q, ok=%v", ref2.ChannelID, ok)
	}
}

// applyGlobalDMSpaceScope emits a should([mustNot(channelType=1),
// (channelType=1 AND spaceId=X)]) MSM=1 structure per §6.5.
func TestApplyGlobalDMSpaceScope_Shape(t *testing.T) {
	b := elastic.NewBoolQuery()
	applyGlobalDMSpaceScope(b, "S9")
	body := marshalDSL(t, b)
	for _, want := range []string{
		`"channelType":1`,
		`"spaceId":"S9"`,
		`"should"`,
		`"must_not"`,
		`"minimum_should_match":"1"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("DM scope missing %q in: %s", want, body)
		}
	}
}

func TestApplyGlobalDMSpaceScope_EmptySpaceIsNoOp(t *testing.T) {
	b := elastic.NewBoolQuery()
	applyGlobalDMSpaceScope(b, "")
	body := marshalDSL(t, b)
	if strings.Contains(body, `"spaceId"`) {
		t.Errorf("empty spaceID must produce no clause: %s", body)
	}
}

// validateGlobalFileBase rejects extensions not in the enum; passes the
// documented set from §7.4.
func TestValidateFileExtsEnum(t *testing.T) {
	// Positive cases — every enum entry must be considered known.
	for _, entry := range fileTypeEnum {
		for _, ext := range entry.Exts {
			if !isKnownFileExt(ext) {
				t.Errorf("enum ext %q must be known", ext)
			}
		}
	}
	// Negative case — a plausible ext outside the enum is rejected.
	if isKnownFileExt("exe") {
		t.Errorf("exe is not in the enum but was accepted")
	}
}

// Helpers.

var errNoDB = &osStubErr{msg: "db down"}

type osStubErr struct{ msg string }

func (e *osStubErr) Error() string { return e.msg }

func marshalDSL(t *testing.T, q interface{ Source() (any, error) }) string {
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

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// newHandlerForGlobalTests returns a handler wired only with the minimal
// fields the DSL / projection / senderJoin paths need — no ES client, no DB.
// Used by tests that assert local behaviour on the handler's private methods.
// A stubUserSvc is injected so buildGlobalSearchAllHits / buildGlobalFileHits
// can call senderJoin without hitting a nil user.IService.
func newHandlerForGlobalTests() *Handler {
	return &Handler{
		Log:         log.NewTLog("messages_search-global-test"),
		cfg:         SearchConfig{},
		cache:       newSenderCache(4, 0),
		userService: &stubUserSvc{},
	}
}

// buildGlobalSearchAllHits should populate channel_type/channel_id on both
// message and file branches, reversing DM channelId to peer uid.
func TestBuildGlobalSearchAllHits_DMChannelReversal(t *testing.T) {
	h := newHandlerForGlobalTests()
	loginUID := "me"
	peer := "peer"
	fake := fakeChannelIDFor(loginUID, peer)
	tp := payloadTypeText
	docDM := Doc{
		MessageID:   1,
		MessageSeq:  1,
		From:        peer,
		Timestamp:   1717000000,
		ChannelID:   fake,
		ChannelType: uint32(channelTypePerson),
		Payload:     &Payload{Type: &tp, Text: &TextPayload{Content: "hi"}},
	}
	src := json.RawMessage(mustJSON(docDM))
	items := h.buildGlobalSearchAllHits(context.Background(), []*elastic.SearchHit{{Source: &src}}, loginUID)
	if len(items) != 1 || items[0].Message == nil {
		t.Fatalf("expected one message hit, got %+v", items)
	}
	if items[0].Message.ChannelID != peer {
		t.Errorf("DM channel_id must be reversed to peer uid, got %q", items[0].Message.ChannelID)
	}
	if items[0].Message.ChannelType != channelTypePerson {
		t.Errorf("DM channel_type must be 1, got %d", items[0].Message.ChannelType)
	}
}

// Regression guard: file hits from a global feed also carry the reversed DM
// channel_id + channel_type (§9.1/§9.2 NEW-A).
func TestBuildGlobalFileHits_DMChannelReversal(t *testing.T) {
	h := newHandlerForGlobalTests()
	loginUID := "me"
	peer := "peer"
	fake := fakeChannelIDFor(loginUID, peer)
	fp := payloadTypeFile
	docFile := Doc{
		MessageID:   9,
		MessageSeq:  9,
		From:        peer,
		Timestamp:   1717000000,
		ChannelID:   fake,
		ChannelType: uint32(channelTypePerson),
		Payload:     &Payload{Type: &fp, File: &FilePayload{Name: "a.pdf"}},
	}
	src := json.RawMessage(mustJSON(docFile))
	items := h.buildGlobalFileHits(context.Background(), []*elastic.SearchHit{{Source: &src}}, loginUID)
	if len(items) != 1 {
		t.Fatalf("expected one file hit, got %d", len(items))
	}
	if items[0].ChannelID != peer {
		t.Errorf("DM file channel_id must be peer uid, got %q", items[0].ChannelID)
	}
	if items[0].ChannelType != channelTypePerson {
		t.Errorf("DM file channel_type must be 1, got %d", items[0].ChannelType)
	}
}

// validateSearchNotEmptyGlobal accepts every global-only filter dimension as
// "effective" so a browse-mode caller specifying just channel_ids /
// channel_types / content_types / member_uid is not falsely rejected.
func TestValidateSearchNotEmptyGlobal_GlobalOnlyFilters(t *testing.T) {
	cases := []struct {
		name    string
		filters GlobalSearchFilters
	}{
		{"channel_ids", GlobalSearchFilters{ChannelIDs: []GlobalChannelRef{{ChannelID: "g", ChannelType: 2}}}},
		{"channel_types", GlobalSearchFilters{ChannelTypes: []uint8{1}}},
		{"content_types", GlobalSearchFilters{ContentTypes: []int{payloadTypeText}}},
		{"member_uid", GlobalSearchFilters{MemberUID: "u"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newValidatorCtx(t)
			if ok := validateSearchNotEmptyGlobal(c, "", tc.filters); !ok {
				t.Errorf("%s alone must satisfy the empty-search guard", tc.name)
			}
		})
	}
}

// newValidatorCtx is a lightweight wkhttp.Context builder for validator
// tests that only care about the accept/reject bool.
func newValidatorCtx(t *testing.T) (*wkhttp.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httptest.NewRequest("POST", "/v1/messages/_search_global_messages", nil)
	return &wkhttp.Context{Context: gc}, rec
}

// P0-1 regression: DM hits must have their channel_offset lookup keyed by
// peer_uid, not the OS fakeChannelID ("uidA@uidB"). The MySQL channel_offset
// table stores DM offsets keyed by peer_uid; if we passed the fake id through,
// the lookup would always miss and the caller's clear-history watermark would
// silently stop applying to search results.
//
// Test 1: projectDocRef reverses the DM fakeChannelID → peer uid.
func TestProjectDocRef_DMChannelIDReversedToPeer(t *testing.T) {
	loginUID := "me"
	peerUID := "peer"
	fake := fakeChannelIDFor(loginUID, peerUID)
	doc := Doc{MessageID: 1, MessageSeq: 7, ChannelID: fake, ChannelType: uint32(channelTypePerson)}
	src := json.RawMessage(mustJSON(doc))
	hit := &elastic.SearchHit{Source: &src}
	ref, ok := projectDocRef("", loginUID)(hit)
	if !ok {
		t.Fatalf("project failed")
	}
	if ref.ChannelID != peerUID {
		t.Fatalf("DM channel_offset key must be peer uid; got %q, want %q (fake=%q)",
			ref.ChannelID, peerUID, fake)
	}
	// Non-DM group hit passes through unchanged.
	docGroup := Doc{MessageID: 2, MessageSeq: 1, ChannelID: "grp-1", ChannelType: uint32(channelTypeGroup)}
	src2 := json.RawMessage(mustJSON(docGroup))
	hit2 := &elastic.SearchHit{Source: &src2}
	ref2, ok := projectDocRef("", loginUID)(hit2)
	if !ok {
		t.Fatalf("project group failed")
	}
	if ref2.ChannelID != "grp-1" {
		t.Fatalf("group channel_id must pass through unchanged; got %q", ref2.ChannelID)
	}
}

// Test 2: end-to-end through filterVisible — the stubProbe records the
// channelIDs it was queried with. A single DM hit whose OS channelId is
// fakeChannelID must reach ChannelOffsets keyed by the peer uid.
func TestFilterVisible_DMChannelOffsetsQueriedByPeer(t *testing.T) {
	loginUID := "me"
	peerUID := "peer"
	fake := fakeChannelIDFor(loginUID, peerUID)
	// Populate the offset map with the CORRECT peer-uid key so the seq=50
	// hit gets dropped by the clear-history watermark (offset=100). If the
	// lookup used the fake id, the key would miss → offset defaults to 0 →
	// the hit would incorrectly survive.
	probe := &stubProbe{offsetByUC: map[string]uint32{loginUID + ":" + peerUID: 100}}
	h := newVisibilityHandler(probe)

	doc := Doc{MessageID: 1, MessageSeq: 50, ChannelID: fake, ChannelType: uint32(channelTypePerson)}
	src := json.RawMessage(mustJSON(doc))
	hit := &elastic.SearchHit{Source: &src}
	ref, ok := projectDocRef("", loginUID)(hit)
	if !ok {
		t.Fatalf("project failed")
	}
	keep, err := h.filterVisible(context.Background(), loginUID, "", []msgRef{ref})
	if err != nil {
		t.Fatalf("filterVisible: %v", err)
	}
	// stubProbe.gotOffsetChannels must contain peer_uid, not fake id.
	if len(probe.gotOffsetChannels) != 1 || probe.gotOffsetChannels[0] != peerUID {
		t.Fatalf("ChannelOffsets must be queried by peer_uid; got %v (fake=%q)",
			probe.gotOffsetChannels, fake)
	}
	// And with offset=100 the seq=50 hit must be dropped.
	if _, survived := keep["1"]; survived {
		t.Fatalf("DM hit seq=50 with peer-keyed offset=100 must be dropped")
	}
}

// Test 3: mixed DM + group multi-channel scope — each DM's offset lookup
// uses the peer uid, group offsets pass through as-is. Regression guard
// that the DM fix doesn't accidentally mangle group channel keys.
func TestFilterVisible_MixedDMAndGroupOffsetKeys(t *testing.T) {
	loginUID := "me"
	peerA := "peerA"
	peerB := "peerB"
	fakeA := fakeChannelIDFor(loginUID, peerA)
	fakeB := fakeChannelIDFor(loginUID, peerB)
	// Peer A cleared history at seq=100; group grp-1 cleared at seq=5.
	probe := &stubProbe{offsetByUC: map[string]uint32{
		loginUID + ":" + peerA: 100,
		loginUID + ":grp-1":    5,
	}}
	h := newVisibilityHandler(probe)

	proj := projectDocRef("", loginUID)
	hits := []*elastic.SearchHit{
		mkHit(t, Doc{MessageID: 1, MessageSeq: 50, ChannelID: fakeA, ChannelType: uint32(channelTypePerson)}),
		mkHit(t, Doc{MessageID: 2, MessageSeq: 50, ChannelID: fakeB, ChannelType: uint32(channelTypePerson)}),
		mkHit(t, Doc{MessageID: 3, MessageSeq: 50, ChannelID: "grp-1", ChannelType: uint32(channelTypeGroup)}),
		mkHit(t, Doc{MessageID: 4, MessageSeq: 200, ChannelID: fakeA, ChannelType: uint32(channelTypePerson)}),
	}
	refs := make([]msgRef, 0, len(hits))
	for _, h := range hits {
		r, ok := proj(h)
		if !ok {
			t.Fatalf("project failed")
		}
		refs = append(refs, r)
	}
	keep, err := h.filterVisible(context.Background(), loginUID, "", refs)
	if err != nil {
		t.Fatalf("filterVisible: %v", err)
	}
	// Assert query key shape: DM entries collapse to peer uids, group stays.
	got := map[string]bool{}
	for _, k := range probe.gotOffsetChannels {
		got[k] = true
	}
	for _, want := range []string{peerA, peerB, "grp-1"} {
		if !got[want] {
			t.Fatalf("ChannelOffsets missing key %q; got %v (fake ids leaked = fixed peer-uid reversal missing)",
				want, probe.gotOffsetChannels)
		}
	}
	// Neither fake channelID should have leaked into the probe.
	for _, leaked := range []string{fakeA, fakeB} {
		if got[leaked] {
			t.Fatalf("fake channelID %q leaked into offset lookup; got %v", leaked, probe.gotOffsetChannels)
		}
	}
	// Decisions: seq=50 under peerA (offset 100) drops; seq=50 under peerB
	// (no offset row, defaults to 0) survives; seq=50 under grp-1 (offset 5)
	// survives; seq=200 under peerA survives (>100).
	if _, ok := keep["1"]; ok {
		t.Fatalf("peerA seq=50 with peer-offset=100 should drop")
	}
	if _, ok := keep["2"]; !ok {
		t.Fatalf("peerB seq=50 with no offset should survive")
	}
	if _, ok := keep["3"]; !ok {
		t.Fatalf("grp-1 seq=50 with offset=5 should survive")
	}
	if _, ok := keep["4"]; !ok {
		t.Fatalf("peerA seq=200 with offset=100 should survive")
	}
}

// mkHit is a tiny helper to synthesise an *elastic.SearchHit from a Doc.
func mkHit(t *testing.T, d Doc) *elastic.SearchHit {
	t.Helper()
	src := json.RawMessage(mustJSON(d))
	return &elastic.SearchHit{Source: &src}
}

// P0-2 regression: enumerateDMPeers must union friends WITH same-Space members
// so a caller can search DMs with a non-friend Space colleague. Legacy
// modules/search/api.go already does this via `SELECT uid FROM space_member`;
// messages_search was regressing that surface to friend-only.
func TestEnumerateDMPeers_UnionsFriendsAndSpaceMembers(t *testing.T) {
	loginUID := "me"
	// Friend list: bob. Space members: bob (dup) + carol (non-friend colleague).
	uSvc := &stubUserSvc{friends: []*user.FriendResp{{UID: "bob"}}}
	h := newHandlerForGlobalTests()
	h.userService = uSvc
	h.spaceMembersFn = func(spaceID, loginUID string) ([]string, error) {
		if spaceID != "space-1" {
			t.Fatalf("spaceMembersFn: expected space-1, got %q", spaceID)
		}
		if loginUID != "me" {
			t.Fatalf("spaceMembersFn: expected caller me, got %q", loginUID)
		}
		return []string{"bob", "carol"}, nil
	}
	h.dmBotFilterFn = func(_ string, peers []string) ([]string, error) {
		// No bots in this fixture — pass through unchanged so the assertion
		// focuses on the friends ∪ space_member union.
		return peers, nil
	}

	peers, err := h.enumerateDMPeers(loginUID, "space-1")
	if err != nil {
		t.Fatalf("enumerateDMPeers: %v", err)
	}
	got := map[string]bool{}
	for _, p := range peers {
		got[p] = true
	}
	// carol is the load-bearing case: non-friend Space member must appear.
	if !got["carol"] {
		t.Fatalf("non-friend Space member 'carol' missing from allowlist; got %v", peers)
	}
	if !got["bob"] {
		t.Fatalf("friend 'bob' missing from allowlist; got %v", peers)
	}
	// Dedup: bob appeared in both friends and members but shows up once.
	seen := 0
	for _, p := range peers {
		if p == "bob" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("bob must be deduplicated; got %d occurrences in %v", seen, peers)
	}
	// Non-Space fallback: same fixture with spaceID="" degrades to friends only.
	h2 := newHandlerForGlobalTests()
	h2.userService = uSvc
	h2.spaceMembersFn = func(string, string) ([]string, error) {
		t.Fatalf("spaceMembersFn must NOT be called when spaceID is empty")
		return nil, nil
	}
	peersEmpty, err := h2.enumerateDMPeers(loginUID, "")
	if err != nil {
		t.Fatalf("enumerateDMPeers empty space: %v", err)
	}
	if len(peersEmpty) != 1 || peersEmpty[0] != "bob" {
		t.Fatalf("empty spaceID must yield friends-only; got %v", peersEmpty)
	}
}
