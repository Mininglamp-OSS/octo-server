package messages_search

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestBuildRichTextDetail_ArrayContent covers the modern shape: content is a
// block array carrying text/image/file blocks plus a server-generated plain
// string. All canonical block fields must round-trip onto the typed projection.
func TestBuildRichTextDetail_ArrayContent(t *testing.T) {
	raw := json.RawMessage(`{
	  "type": 14,
	  "content": [
	    {"type": "text", "text": "标题"},
	    {"type": "image", "url": "http://x/y.png", "width": 640, "height": 480, "name": "y.png", "caption": "图说"},
	    {"type": "file", "url": "http://x/a.pdf", "name": "a.pdf", "size": 12345, "extension": "pdf", "mime": "application/pdf", "caption": "合同"}
	  ],
	  "plain": "标题[图片][文件]"
	}`)
	d := buildRichTextDetail(raw)
	if d == nil {
		t.Fatalf("expected non-nil detail")
	}
	if d.Plain != "标题[图片][文件]" {
		t.Errorf("plain: got %q", d.Plain)
	}
	if got := len(d.Content); got != 3 {
		t.Fatalf("blocks: got %d", got)
	}
	if d.Content[0].Type != "text" || d.Content[0].Text != "标题" {
		t.Errorf("text block: %+v", d.Content[0])
	}
	img := d.Content[1]
	if img.Type != "image" || img.URL != "http://x/y.png" || img.Width != 640 || img.Height != 480 || img.Name != "y.png" || img.Caption != "图说" {
		t.Errorf("image block: %+v", img)
	}
	f := d.Content[2]
	if f.Type != "file" || f.URL != "http://x/a.pdf" || f.Name != "a.pdf" || f.Size != 12345 || f.Extension != "pdf" || f.Mime != "application/pdf" || f.Caption != "合同" {
		t.Errorf("file block: %+v", f)
	}
	if d.Mention != nil {
		t.Errorf("mention should be nil when absent, got %+v", d.Mention)
	}
}

// TestBuildRichTextDetail_LegacyStringContent: old payloads stored content as
// a plain JSON string; the builder must normalise that into a single text
// block so the renderer never sees the legacy shape.
func TestBuildRichTextDetail_LegacyStringContent(t *testing.T) {
	raw := json.RawMessage(`{"type":14,"content":"hello world","plain":"hello world"}`)
	d := buildRichTextDetail(raw)
	if d == nil {
		t.Fatalf("nil detail")
	}
	if len(d.Content) != 1 {
		t.Fatalf("blocks: got %d", len(d.Content))
	}
	if d.Content[0].Type != "text" || d.Content[0].Text != "hello world" {
		t.Errorf("normalised text block: %+v", d.Content[0])
	}
	if d.Plain != "hello world" {
		t.Errorf("plain: got %q", d.Plain)
	}
}

// TestBuildRichTextDetail_EmptyRaw guards the legacy / pre-payloadRaw fail-soft
// case: the projection must be nil so the caller falls back to snippet.
func TestBuildRichTextDetail_EmptyRaw(t *testing.T) {
	if d := buildRichTextDetail(nil); d != nil {
		t.Errorf("nil raw: want nil detail, got %+v", d)
	}
	if d := buildRichTextDetail(json.RawMessage{}); d != nil {
		t.Errorf("empty raw: want nil detail, got %+v", d)
	}
}

// TestBuildRichTextDetail_InvalidJSON: corrupt envelopes must fail-soft (nil)
// rather than surface a half-parsed detail or propagate the error.
func TestBuildRichTextDetail_InvalidJSON(t *testing.T) {
	if d := buildRichTextDetail(json.RawMessage(`not-json`)); d != nil {
		t.Errorf("garbage: want nil, got %+v", d)
	}
	// A legacy string content that is itself unparseable as a string (e.g.
	// content is a JSON number) must drop content but keep the envelope.
	d := buildRichTextDetail(json.RawMessage(`{"content": 42, "plain": "p"}`))
	if d == nil {
		t.Fatalf("envelope-valid number-content: want non-nil")
	}
	if len(d.Content) != 0 {
		t.Errorf("number content must yield empty blocks, got %+v", d.Content)
	}
	if d.Plain != "p" {
		t.Errorf("plain should still pass through, got %q", d.Plain)
	}
}

// TestBuildRichTextDetail_Mention transports the mention object verbatim, both
// the entity slice and the three @-all flags.
func TestBuildRichTextDetail_Mention(t *testing.T) {
	raw := json.RawMessage(`{
	  "content": [{"type":"text","text":"@张三 你好"}],
	  "plain": "@张三 你好",
	  "mention": {
	    "entities": [{"uid":"u_zhang","offset":0,"length":3}],
	    "all": 1, "humans": 1, "ais": 0
	  }
	}`)
	d := buildRichTextDetail(raw)
	if d == nil || d.Mention == nil {
		t.Fatalf("expected mention, got %+v", d)
	}
	if len(d.Mention.Entities) != 1 {
		t.Fatalf("entities: got %d", len(d.Mention.Entities))
	}
	e := d.Mention.Entities[0]
	if e.UID != "u_zhang" || e.Offset != 0 || e.Length != 3 {
		t.Errorf("entity: %+v", e)
	}
	if d.Mention.All != 1 || d.Mention.Humans != 1 || d.Mention.Ais != 0 {
		t.Errorf("flags: %+v", d.Mention)
	}
}

// TestSingleMessageHit_RichText covers the projection contract on the hit:
//   - type=14 + non-empty payloadRaw → rich_text populated, snippet still set
//   - type=14 + missing payloadRaw   → rich_text nil, snippet/text untouched
//   - non-richtext type              → rich_text nil regardless of payloadRaw
func TestSingleMessageHit_RichText(t *testing.T) {
	var h Handler
	rt := payloadTypeRichText
	raw := json.RawMessage(`{"type":14,"content":[{"type":"text","text":"标题"}],"plain":"标题"}`)

	doc := Doc{
		MessageID:  101,
		MessageSeq: 9,
		Payload:    &Payload{Type: &rt, RichText: &RichTextPayload{SearchText: "标题"}},
		PayloadRaw: raw,
	}
	mh := h.singleMessageHit(doc, "g", 0, nil)
	if mh.RichText == nil {
		t.Fatalf("rich_text must be populated for type=14 + payloadRaw")
	}
	if len(mh.RichText.Content) != 1 || mh.RichText.Content[0].Text != "标题" {
		t.Errorf("rich_text.content: %+v", mh.RichText.Content)
	}
	if mh.RichText.Plain != "标题" {
		t.Errorf("rich_text.plain: %q", mh.RichText.Plain)
	}
	// Snippet fallback still runs — searchText feeds fallbackSnippet so the
	// hit is renderable even on a client that ignores rich_text.
	if mh.Snippet != "标题" {
		t.Errorf("snippet fallback should still emit searchText, got %q", mh.Snippet)
	}
	if mh.MessageKind != "text" {
		t.Errorf("kind should fold into text, got %q", mh.MessageKind)
	}

	// Legacy doc indexed before payloadRaw existed: fail-soft to nil.
	docNoRaw := Doc{
		MessageID: 102,
		Payload:   &Payload{Type: &rt, RichText: &RichTextPayload{SearchText: "标题"}},
	}
	mhNoRaw := h.singleMessageHit(docNoRaw, "g", 0, nil)
	if mhNoRaw.RichText != nil {
		t.Errorf("missing payloadRaw must yield nil rich_text, got %+v", mhNoRaw.RichText)
	}
	if mhNoRaw.Snippet != "标题" {
		t.Errorf("snippet fallback must still fire on legacy doc, got %q", mhNoRaw.Snippet)
	}

	// Non-richtext docs must never carry rich_text, even if a stray payloadRaw
	// blob is present on the source.
	tp := payloadTypeText
	docText := Doc{
		MessageID:  103,
		Payload:    &Payload{Type: &tp, Text: &TextPayload{Content: "hi"}},
		PayloadRaw: raw,
	}
	mhText := h.singleMessageHit(docText, "g", 0, nil)
	if mhText.RichText != nil {
		t.Errorf("non-richtext type must not project rich_text, got %+v", mhText.RichText)
	}
}

// TestMessageHit_RichTextOmitemptyOnWire pins the wire contract: rich_text
// must NOT appear on plain text hits (otherwise old clients see an unexpected
// field on every response).
func TestMessageHit_RichTextOmitemptyOnWire(t *testing.T) {
	mh := MessageHit{MessageID: "1", MessageKind: "text", SenderID: "u"}
	out, err := json.Marshal(mh)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), `"rich_text"`) {
		t.Errorf("nil rich_text must be omitted on the wire, got %s", string(out))
	}
}
