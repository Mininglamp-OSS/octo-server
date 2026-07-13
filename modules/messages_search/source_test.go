package messages_search

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
)

// TestSource_FullDoc covers the v4.2 mapping shape. Keep this fixture in sync
// with indexer-os-changes.md §3.2 — when the indexer adds a new structured
// field, extend this fixture before touching the handlers so we catch
// unmarshal regressions early.
const fullDocFixture = `{
  "messageId": 1234567890123,
  "messageSeq": 42,
  "from": "userA",
  "to": "userB",
  "channelId": "chan@channelB",
  "channelType": 1,
  "timestamp": 1717000000,
  "revoked": false,
  "payload": {
    "type": 11,
    "mergeForward": {
      "childCount": 5,
      "msgs": [
        {"messageId": 1, "type": 1, "searchText": "hello"},
        {"messageId": 2, "type": 8, "searchText": "doc.pdf"}
      ]
    }
  }
}`

func TestUnmarshalDoc_MergeForward(t *testing.T) {
	var doc Doc
	if err := json.Unmarshal([]byte(fullDocFixture), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.MessageID != 1234567890123 {
		t.Fatalf("messageId: got %d", doc.MessageID)
	}
	if doc.MessageSeq != 42 {
		t.Fatalf("messageSeq: got %d", doc.MessageSeq)
	}
	if doc.Payload == nil || doc.Payload.MergeForward == nil {
		t.Fatalf("expected mergeForward")
	}
	if doc.Payload.MergeForward.ChildCount != 5 {
		t.Fatalf("childCount: got %d", doc.Payload.MergeForward.ChildCount)
	}
	if len(doc.Payload.MergeForward.Msgs) != 2 {
		t.Fatalf("msgs len: got %d", len(doc.Payload.MergeForward.Msgs))
	}
	// Legacy docs without msgs[].from / msgs[].timestamp must deserialise
	// to zero values; the partial-shape contract relies on omitempty hiding
	// these fields on the wire (see buildInnerMessages).
	for _, m := range doc.Payload.MergeForward.Msgs {
		if m.From != "" {
			t.Errorf("legacy doc must not surface from, got %q", m.From)
		}
		if m.Timestamp != 0 {
			t.Errorf("legacy doc must not surface timestamp, got %d", m.Timestamp)
		}
	}
}

// TestUnmarshalDoc_MergeForward_FutureFields covers the post-indexer-bump
// shape where msgs[].from and msgs[].timestamp are populated. Both fields
// flow through to buildInnerMessages → InnerMessage.SenderID / SentAt.
func TestUnmarshalDoc_MergeForward_FutureFields(t *testing.T) {
	src := `{
	  "messageId": 7,
	  "channelId": "g",
	  "timestamp": 1717000000,
	  "payload": {
	    "type": 11,
	    "mergeForward": {
	      "childCount": 1,
	      "msgs": [
	        {"messageId": 99, "type": 1, "searchText": "hi",
	         "from": "u_alice", "timestamp": 1717000099}
	      ]
	    }
	  }
	}`
	var doc Doc
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m := doc.Payload.MergeForward.Msgs[0]
	if m.From != "u_alice" {
		t.Errorf("from: got %q", m.From)
	}
	if m.Timestamp != 1717000099 {
		t.Errorf("timestamp: got %d", m.Timestamp)
	}
}

// TestBuildInnerMessages_FullShape exercises the full forward-card mapping
// once msgs[].from / msgs[].timestamp are populated. Names are NOT filled
// here — that's senderJoin's job; this test asserts the projection only.
func TestBuildInnerMessages_FullShape(t *testing.T) {
	tp := payloadTypeMergeForward
	p := &Payload{
		Type: &tp,
		MergeForward: &MergeForwardPayload{
			ChildCount: 2,
			Msgs: []MergeForwardMsg{
				{MessageID: 100, Type: 1, SearchText: "hello", From: "u1", Timestamp: 1717000000},
				{MessageID: 101, Type: 8, SearchText: "doc.pdf", From: "u2", Timestamp: 1717000060},
			},
		},
	}
	inner := buildInnerMessages(p)
	if len(inner) != 2 {
		t.Fatalf("len: got %d", len(inner))
	}
	if inner[0].MessageID != "100" {
		t.Errorf("messageId[0]: got %q", inner[0].MessageID)
	}
	if inner[0].Type != 1 {
		t.Errorf("type[0]: got %d", inner[0].Type)
	}
	if inner[0].SearchText != "hello" {
		t.Errorf("searchText[0]: got %q", inner[0].SearchText)
	}
	if inner[0].SenderID != "u1" {
		t.Errorf("senderId[0]: got %q", inner[0].SenderID)
	}
	if inner[0].SenderName != "" {
		t.Errorf("senderName must be unset until senderJoin: got %q", inner[0].SenderName)
	}
	if inner[0].SentAt == "" {
		t.Errorf("sentAt[0] must be populated when timestamp>0")
	}
	if inner[1].MessageID != "101" || inner[1].SenderID != "u2" {
		t.Errorf("msg[1]: %+v", inner[1])
	}
}

// TestBuildInnerMessages_PartialFields covers the contract-v0 shape where
// msgs[].from / msgs[].timestamp are absent — the API must omit
// sender_id / sent_at entirely (omitempty + empty zero values).
func TestBuildInnerMessages_PartialFields(t *testing.T) {
	p := &Payload{
		MergeForward: &MergeForwardPayload{
			Msgs: []MergeForwardMsg{
				{MessageID: 42, Type: 1, SearchText: "hi"},
			},
		},
	}
	inner := buildInnerMessages(p)
	if len(inner) != 1 {
		t.Fatalf("len: got %d", len(inner))
	}
	if inner[0].SenderID != "" {
		t.Errorf("sender_id must be empty for partial doc: got %q", inner[0].SenderID)
	}
	if inner[0].SentAt != "" {
		t.Errorf("sent_at must be empty when timestamp=0: got %q", inner[0].SentAt)
	}
	// Wire-level omitempty: the JSON form must not emit these keys.
	out, err := json.Marshal(inner[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(out)
	for _, k := range []string{`"sender_id"`, `"sender_name"`, `"sent_at"`} {
		if strings.Contains(wire, k) {
			t.Errorf("partial-shape wire form must omit %s, got %s", k, wire)
		}
	}
}

// TestBuildInnerMessages_GuardClauses: returns nil for any non-forward shape
// or empty msgs[] so the response field is omitted entirely (omitempty).
func TestBuildInnerMessages_GuardClauses(t *testing.T) {
	if got := buildInnerMessages(nil); got != nil {
		t.Errorf("nil payload: got %+v", got)
	}
	if got := buildInnerMessages(&Payload{}); got != nil {
		t.Errorf("non-forward payload: got %+v", got)
	}
	if got := buildInnerMessages(&Payload{MergeForward: &MergeForwardPayload{}}); got != nil {
		t.Errorf("empty msgs[]: got %+v", got)
	}
}

func TestUnmarshalDoc_Image(t *testing.T) {
	src := `{"messageId":1,"messageSeq":1,"channelId":"g","timestamp":100,"payload":{"type":2,"image":{"url":"http://x","width":640,"height":480,"caption":"hi"}}}`
	var doc Doc
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Payload.Image == nil || doc.Payload.Image.URL != "http://x" {
		t.Fatalf("image: %+v", doc.Payload.Image)
	}
	if doc.Payload.Image.Width != 640 || doc.Payload.Image.Height != 480 {
		t.Fatalf("dims: %+v", doc.Payload.Image)
	}
}

func TestUnmarshalDoc_Video(t *testing.T) {
	src := `{"messageId":1,"messageSeq":1,"channelId":"g","timestamp":100,"payload":{"type":5,"video":{"url":"http://x","cover":"c","second":12,"width":640,"height":480}}}`
	var doc Doc
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Payload.Video.Second != 12 {
		t.Fatalf("second: got %d", doc.Payload.Video.Second)
	}
}

func TestUnmarshalDoc_File(t *testing.T) {
	src := `{"messageId":1,"messageSeq":1,"channelId":"g","timestamp":100,"payload":{"type":8,"file":{"url":"http://x","name":"a.pdf","size":12345,"extension":"pdf"}}}`
	var doc Doc
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Payload.File.SizeBytes != 12345 || doc.Payload.File.Ext != "pdf" {
		t.Fatalf("file: %+v", doc.Payload.File)
	}
}

// TestUnmarshalDoc_RichText covers the indexer's payload.richText shape: a
// payload.type=14 doc carrying the plain-text projection in
// payload.richText.searchText.
func TestUnmarshalDoc_RichText(t *testing.T) {
	src := `{"messageId":1,"messageSeq":1,"channelId":"g","timestamp":100,"payload":{"type":14,"richText":{"searchText":"标题 正文 图片说明"}}}`
	var doc Doc
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Payload.RichText == nil {
		t.Fatalf("richText payload must be populated")
	}
	if got := doc.Payload.RichText.SearchText; got != "标题 正文 图片说明" {
		t.Fatalf("searchText: got %q", got)
	}
	if payloadType(doc.Payload) != payloadTypeRichText {
		t.Fatalf("payloadType: got %d", payloadType(doc.Payload))
	}
}

// TestUnmarshalDoc_VirtualSubDoc covers the Part B virtual sub-document shape
// (richtext-virtual-docs-octo-server-dev.md §1): a rich-text-derived child
// carrying `virtual=true` + `parentMessageId` alongside the usual image/file
// payload. Both fields are reader-internal — they drive the must_not filter
// and the visibility coalesce, never the JSON response.
func TestUnmarshalDoc_VirtualSubDoc(t *testing.T) {
	src := `{
	  "messageId": 7777,
	  "messageSeq": 99,
	  "channelId": "g",
	  "timestamp": 1717000000,
	  "virtual": true,
	  "parentMessageId": 7777,
	  "payload": {"type": 2, "image": {"url": "http://x", "caption": "合同图片"}}
	}`
	var doc Doc
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !doc.Virtual {
		t.Fatalf("Virtual: want true, got false")
	}
	if doc.ParentMessageID == nil {
		t.Fatalf("ParentMessageID: want non-nil pointer")
	}
	if *doc.ParentMessageID != 7777 {
		t.Fatalf("ParentMessageID: got %d, want 7777", *doc.ParentMessageID)
	}
}

// TestUnmarshalDoc_PlainDocOmitsVirtual: a legacy / non-virtual doc must
// deserialise to Virtual=false and ParentMessageID=nil so the visibility
// coalesce keeps the existing behaviour (visKey = own messageId) and the
// text-search must_not filter still admits it.
func TestUnmarshalDoc_PlainDocOmitsVirtual(t *testing.T) {
	src := `{"messageId":1,"messageSeq":1,"channelId":"g","timestamp":100,"payload":{"type":1,"text":{"content":"hi"}}}`
	var doc Doc
	if err := json.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Virtual {
		t.Fatalf("Virtual must default to false on plain doc")
	}
	if doc.ParentMessageID != nil {
		t.Fatalf("ParentMessageID must be nil on plain doc; got %v", *doc.ParentMessageID)
	}
}

// TestMarshalDoc_PlainDocOmitsNewFieldsOnWire: a plain Doc (Virtual=false,
// ParentMessageID=nil) must marshal byte-identical to its pre-Part-B form —
// the new fields are tagged `omitempty` so neither key surfaces. Guards
// against accidentally widening the OS-facing JSON contract.
func TestMarshalDoc_PlainDocOmitsNewFieldsOnWire(t *testing.T) {
	d := Doc{MessageID: 1, MessageSeq: 1, ChannelID: "g", Timestamp: 100}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(out)
	for _, k := range []string{`"virtual"`, `"parentMessageId"`} {
		if strings.Contains(wire, k) {
			t.Errorf("plain-doc wire form must omit %s; got %s", k, wire)
		}
	}
}

func TestClassifyKind_Forward(t *testing.T) {
	p := &Payload{MergeForward: &MergeForwardPayload{ChildCount: 3}}
	if got := classifyKind(p); got != "forward" {
		t.Fatalf("forward: got %q", got)
	}
}

func TestClassifyKind_Text(t *testing.T) {
	if got := classifyKind(nil); got != "text" {
		t.Fatalf("nil: got %q", got)
	}
	tp := 1
	p := &Payload{Type: &tp, Text: &TextPayload{Content: "hi"}}
	if got := classifyKind(p); got != "text" {
		t.Fatalf("text: got %q", got)
	}
}

// Image (payload.type=2) gets its own kind so the client can dispatch to the
// media renderer. Reaches both /_search_all browse mode (DSL whitelists 2 when
// keyword="") and /_search_around (no type whitelist).
func TestClassifyKind_Image(t *testing.T) {
	tp := payloadTypeImage
	p := &Payload{Type: &tp, Image: &ImagePayload{URL: "http://x"}}
	if got := classifyKind(p); got != "image" {
		t.Fatalf("image kind: got %q", got)
	}
}

// Video (payload.type=5) — same as image. Both reach singleMessageHit only
// through /_search_all browse and /_search_around (the legacy /_search keeps
// its [1,11,14] whitelist).
func TestClassifyKind_Video(t *testing.T) {
	tp := payloadTypeVideo
	p := &Payload{Type: &tp, Video: &VideoPayload{Cover: "http://c"}}
	if got := classifyKind(p); got != "video" {
		t.Fatalf("video kind: got %q", got)
	}
}

// Forward beats image/video: a forward card whose first child is media still
// renders as a forward, because the outer mergeForward is what the row stands
// for.
func TestClassifyKind_ForwardBeatsMedia(t *testing.T) {
	tp := payloadTypeImage
	p := &Payload{
		Type:         &tp,
		Image:        &ImagePayload{URL: "http://x"},
		MergeForward: &MergeForwardPayload{ChildCount: 1},
	}
	if got := classifyKind(p); got != "forward" {
		t.Fatalf("forward must beat image: got %q", got)
	}
}

// Rich-text is folded into "text" — we deliberately don't expose a "richtext"
// kind on the wire (the swagger enum is ["text","forward"]), so the client
// keeps a stable two-value contract and renders richText via the existing
// text path. Forward still wins when both shapes are present.
func TestClassifyKind_RichTextFoldsIntoText(t *testing.T) {
	tp := payloadTypeRichText
	p := &Payload{Type: &tp, RichText: &RichTextPayload{SearchText: "hi"}}
	if got := classifyKind(p); got != "text" {
		t.Fatalf("richtext must fold into text: got %q", got)
	}
	// Forward wins over richtext (impossible in practice, but the priority is
	// part of the contract).
	p2 := &Payload{
		Type:         &tp,
		RichText:     &RichTextPayload{SearchText: "hi"},
		MergeForward: &MergeForwardPayload{ChildCount: 1},
	}
	if got := classifyKind(p2); got != "forward" {
		t.Fatalf("forward must beat richtext: got %q", got)
	}
}

func TestBuildOuterPreview_ForwardOnly(t *testing.T) {
	p := &Payload{MergeForward: &MergeForwardPayload{ChildCount: 7}}
	prev := buildOuterPreview(p)
	if prev == nil || prev.ChildCount != 7 {
		t.Fatalf("forward preview: %+v", prev)
	}
	if prev := buildOuterPreview(nil); prev != nil {
		t.Fatalf("nil payload: want nil preview")
	}
	if prev := buildOuterPreview(&Payload{}); prev != nil {
		t.Fatalf("text payload: want nil preview")
	}
	// P2-6 guard: a forward whose ChildCount is missing or non-positive
	// must NOT yield {child_count: 0} on the wire — the frontend reads
	// that as "0 messages" which is misleading.
	if prev := buildOuterPreview(&Payload{MergeForward: &MergeForwardPayload{}}); prev != nil {
		t.Fatalf("zero childCount: want nil preview, got %+v", prev)
	}
	if prev := buildOuterPreview(&Payload{MergeForward: &MergeForwardPayload{ChildCount: -1}}); prev != nil {
		t.Fatalf("negative childCount: want nil preview, got %+v", prev)
	}
}

func TestPickSnippet(t *testing.T) {
	hl := map[string][]string{
		"payload.text.content": {"hello <mark>world</mark>"},
		"payload.file.name":    {"<mark>doc</mark>.pdf"},
	}
	if got := pickSnippet(hl); !strings.Contains(got, "world") {
		t.Fatalf("priority: text content should win, got %q", got)
	}
	// image/file fields stay in the priority list — /_search_around (no
	// payload-type whitelist) routes hits through the same snippet picker, so
	// a media-only hit must still surface caption / filename rather than be
	// blanked out. /_search applies its [1,11,14] whitelist upstream of this
	// function, so the extra branches do not affect that path.
	if got := pickSnippet(map[string][]string{"payload.file.name": {"x"}}); got != "x" {
		t.Fatalf("file fallback: got %q", got)
	}
	if got := pickSnippet(map[string][]string{"payload.image.caption": {"y"}}); got != "y" {
		t.Fatalf("image fallback: got %q", got)
	}
	if got := pickSnippet(nil); got != "" {
		t.Fatalf("empty: got %q", got)
	}
	// RichText sits between text.content and mergeForward in priority: when
	// only richText highlighted (no plain text matched) it must win.
	if got := pickSnippet(map[string][]string{
		"payload.richText.searchText":          {"<mark>标</mark>题"},
		"payload.mergeForward.msgs.searchText": {"forward x"},
	}); got != "<mark>标</mark>题" {
		t.Fatalf("richText must beat mergeForward in priority, got %q", got)
	}
	// And text.content still beats richText.
	if got := pickSnippet(map[string][]string{
		"payload.text.content":        {"<mark>plain</mark>"},
		"payload.richText.searchText": {"rt"},
	}); got != "<mark>plain</mark>" {
		t.Fatalf("text.content must beat richText, got %q", got)
	}
}

func TestFallbackSnippet(t *testing.T) {
	txt := 5
	if got := fallbackSnippet(&Payload{Type: &txt, Text: &TextPayload{Content: "群聊测试消息"}}); got != "群聊测试消息" {
		t.Fatalf("text content: got %q", got)
	}
	// RichText fills in when text.content is absent (the empty-keyword
	// browse path on a type=14 doc).
	rt := payloadTypeRichText
	if got := fallbackSnippet(&Payload{Type: &rt, RichText: &RichTextPayload{SearchText: "富文本预览"}}); got != "富文本预览" {
		t.Fatalf("richText fallback: got %q", got)
	}
	// merge-forward: first non-empty child searchText wins.
	mf := &Payload{MergeForward: &MergeForwardPayload{Msgs: []MergeForwardMsg{
		{SearchText: ""}, {SearchText: "转发预览"},
	}}}
	if got := fallbackSnippet(mf); got != "转发预览" {
		t.Fatalf("merge-forward: got %q", got)
	}
	// image caption / file name still fall back — /_search_around exposes
	// media payloads (no payload-type whitelist), so the fallback path must
	// keep producing a snippet for them.
	if got := fallbackSnippet(&Payload{Image: &ImagePayload{Caption: "图说"}}); got != "图说" {
		t.Fatalf("image caption: got %q", got)
	}
	if got := fallbackSnippet(&Payload{File: &FilePayload{Name: "a.pdf"}}); got != "a.pdf" {
		t.Fatalf("file name: got %q", got)
	}
	// No textual projection (bare voice doc) or nil payload → empty, snippet omitted.
	if got := fallbackSnippet(&Payload{Voice: &VoicePayload{}}); got != "" {
		t.Fatalf("no text: got %q", got)
	}
	if got := fallbackSnippet(nil); got != "" {
		t.Fatalf("nil payload: got %q", got)
	}
}

func TestTruncateRunes(t *testing.T) {
	// Under the cap is returned verbatim, no ellipsis.
	if got := truncateRunes("abc", 5); got != "abc" {
		t.Fatalf("under cap: got %q", got)
	}
	// Over the cap is clipped on a rune boundary with a trailing ellipsis.
	long := strings.Repeat("中", 130)
	got := truncateRunes(long, snippetWindow)
	if r := []rune(got); len(r) != snippetWindow+1 || r[snippetWindow] != '…' {
		t.Fatalf("over cap: len=%d last=%q", len(r), string(r[len(r)-1]))
	}
}

func TestSingleMessageHit_SnippetFallback(t *testing.T) {
	var h Handler
	txt := 1
	doc := Doc{MessageID: 7, Payload: &Payload{Type: &txt, Text: &TextPayload{Content: "群聊测试消息"}}}

	// Keyword path: highlight wins, no fallback.
	hl := map[string][]string{"payload.text.content": {"群聊<mark>测试</mark>消息"}}
	if got := h.singleMessageHit(doc, "c", 0, hl).Snippet; got != "群聊<mark>测试</mark>消息" {
		t.Fatalf("highlight should win, got %q", got)
	}
	// Empty-keyword browse path: no highlight → fall back to raw content.
	if got := h.singleMessageHit(doc, "c", 0, nil).Snippet; got != "群聊测试消息" {
		t.Fatalf("empty highlight should fall back to content, got %q", got)
	}
}

// TestPayloadTypeConstants_AlignWithOctoLib pins the local payloadType*
// constants to octo-lib/common.ContentType so a renumber upstream surfaces
// here as a test failure rather than a silent mis-routing of message types.
func TestPayloadTypeConstants_AlignWithOctoLib(t *testing.T) {
	cases := []struct {
		name  string
		local int
		lib   common.ContentType
	}{
		{"text", payloadTypeText, common.Text},
		{"image", payloadTypeImage, common.Image},
		{"gif", payloadTypeGIF, common.GIF},
		{"voice", payloadTypeVoice, common.Voice},
		{"video", payloadTypeVideo, common.Video},
		{"file", payloadTypeFile, common.File},
		{"merge_forward", payloadTypeMergeForward, common.MultipleForward},
		{"cmd", payloadTypeCmd, common.CMD},
	}
	for _, tc := range cases {
		if int(tc.lib) != tc.local {
			t.Fatalf("%s: local=%d != octo-lib %d", tc.name, tc.local, tc.lib)
		}
	}
}
