package messages_search

import (
	"testing"
)

func TestSingleSearchAllHit_File(t *testing.T) {
	tp := payloadTypeFile
	doc := Doc{
		MessageID:  100,
		MessageSeq: 9,
		From:       "u1",
		Timestamp:  1717000000,
		Payload: &Payload{
			Type: &tp,
			File: &FilePayload{Name: "a.pdf", Ext: "pdf", URL: "http://x"},
		},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}
	got := h.singleSearchAllHit(doc, "g", channelTypeGroup, nil)
	if got.ResultType != "file" {
		t.Errorf("result_type: got %q", got.ResultType)
	}
	if got.File == nil || got.File.FileName != "a.pdf" {
		t.Fatalf("file should be populated: %+v", got.File)
	}
	if got.Message != nil {
		t.Errorf("message should be nil for file result: %+v", got.Message)
	}
	if got.SortedAt != got.File.SentAt {
		t.Errorf("sorted_at must mirror inner sent_at: got %q vs %q", got.SortedAt, got.File.SentAt)
	}
}

func TestSingleSearchAllHit_TextMessage(t *testing.T) {
	tp := payloadTypeText
	doc := Doc{
		MessageID:  101,
		MessageSeq: 10,
		From:       "u2",
		Timestamp:  1717000001,
		Payload: &Payload{
			Type: &tp,
			Text: &TextPayload{Content: "hello"},
		},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}
	hl := map[string][]string{"payload.text.content": {"<mark>hello</mark>"}}
	got := h.singleSearchAllHit(doc, "g", channelTypeGroup, hl)
	if got.ResultType != "message" {
		t.Errorf("result_type: got %q", got.ResultType)
	}
	if got.Message == nil || got.Message.Snippet == "" {
		t.Fatalf("message + snippet expected: %+v", got.Message)
	}
	if got.Message.MessageKind != "text" {
		t.Errorf("text kind: got %q", got.Message.MessageKind)
	}
	if got.File != nil {
		t.Errorf("file should be nil for message result")
	}
}

func TestSingleSearchAllHit_ForwardKeepsMessageType(t *testing.T) {
	tp := payloadTypeMergeForward
	doc := Doc{
		MessageID: 102,
		Timestamp: 100,
		Payload: &Payload{
			Type:         &tp,
			MergeForward: &MergeForwardPayload{ChildCount: 4},
		},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}
	got := h.singleSearchAllHit(doc, "g", channelTypeGroup, nil)
	if got.ResultType != "message" {
		t.Errorf("forward must be 'message' (file is type=8 only): got %q", got.ResultType)
	}
	if got.Message == nil || got.Message.MessageKind != "forward" {
		t.Errorf("forward kind: %+v", got.Message)
	}
	if got.Message.OuterPreview == nil || got.Message.OuterPreview.ChildCount != 4 {
		t.Errorf("outer_preview: %+v", got.Message.OuterPreview)
	}
}

// Rich-text (payload.type=14) keeps result_type=message — it is rendered as a
// message, not a file — and folds into the existing "text" kind so the wire
// contract stays at the two-value enum {text, forward}. Snippet falls back to
// the indexer's plain projection (payload.richText.searchText) when no
// highlight was attached (empty-keyword browse).
func TestSingleSearchAllHit_RichTextKeepsMessageType(t *testing.T) {
	tp := payloadTypeRichText
	doc := Doc{
		MessageID:  103,
		MessageSeq: 11,
		From:       "u3",
		Timestamp:  1717000002,
		Payload: &Payload{
			Type:     &tp,
			RichText: &RichTextPayload{SearchText: "富文本搜索 命中预览"},
		},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}
	// Keyword path: highlight on richText.searchText wins via pickSnippet.
	hl := map[string][]string{"payload.richText.searchText": {"富文本<mark>搜索</mark>"}}
	got := h.singleSearchAllHit(doc, "g", channelTypeGroup, hl)
	if got.ResultType != "message" {
		t.Errorf("richtext must be 'message': got %q", got.ResultType)
	}
	if got.Message == nil {
		t.Fatalf("message must be populated for richtext")
	}
	if got.Message.MessageKind != "text" {
		t.Errorf("richtext kind must fold into text: got %q", got.Message.MessageKind)
	}
	if got.Message.Snippet != "富文本<mark>搜索</mark>" {
		t.Errorf("richtext keyword snippet: got %q", got.Message.Snippet)
	}
	// Empty-keyword browse path: no highlight → fall back to raw richText.
	got2 := h.singleSearchAllHit(doc, "g", channelTypeGroup, nil)
	if got2.Message.Snippet != "富文本搜索 命中预览" {
		t.Errorf("richtext browse fallback snippet: got %q", got2.Message.Snippet)
	}
	if got.File != nil {
		t.Errorf("file should be nil for richtext result")
	}
}

// Image (payload.type=2) surfaces in /_search_all browse mode (keyword="")
// and must come back as a renderable MessageHit:
//   - result_type=message (SearchAllHit dispatcher keeps the message slot)
//   - message_kind=image so the client switches to the media renderer
//   - thumb_url / width / height mirror MediaHit's projection (v1.8 has no
//     separate thumb URL — the original image URL is what gets surfaced)
//   - snippet falls back to the image caption when present
//
// Regression for PR #467: before this change the projection stamped image
// docs as kind="text" with snippet="" and no renderable URL, leaving the
// client with no way to distinguish or render media in the unified feed.
func TestSingleSearchAllHit_Image(t *testing.T) {
	tp := payloadTypeImage
	doc := Doc{
		MessageID:  201,
		MessageSeq: 21,
		From:       "u4",
		Timestamp:  1717000010,
		Payload: &Payload{
			Type: &tp,
			Image: &ImagePayload{
				URL:     "https://cdn/x.jpg",
				Caption: "野餐合影",
				Width:   1080,
				Height:  720,
			},
		},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}
	got := h.singleSearchAllHit(doc, "g", channelTypeGroup, nil)
	if got.ResultType != "message" {
		t.Errorf("result_type: got %q want message", got.ResultType)
	}
	if got.Message == nil {
		t.Fatalf("message must be populated for image hit")
	}
	if got.Message.MessageKind != "image" {
		t.Errorf("kind: got %q want image", got.Message.MessageKind)
	}
	if got.Message.ThumbURL != "https://cdn/x.jpg" {
		t.Errorf("thumb_url must mirror payload.image.url: got %q", got.Message.ThumbURL)
	}
	if got.Message.Width != 1080 || got.Message.Height != 720 {
		t.Errorf("dimensions: got %dx%d want 1080x720", got.Message.Width, got.Message.Height)
	}
	if got.Message.DurationMs != 0 {
		t.Errorf("image must not carry duration_ms: got %d", got.Message.DurationMs)
	}
	if got.Message.Snippet != "野餐合影" {
		t.Errorf("snippet must fall back to caption in browse mode: got %q", got.Message.Snippet)
	}
	if got.File != nil {
		t.Errorf("file should be nil for image result")
	}
}

// Video (payload.type=5) — same shape as image plus duration_ms (seconds → ms).
// Empty Snippet is intentional: VideoPayload has no caption/title field in the
// v1.8 indexer projection, so the renderable bits are delivered via
// thumb_url / duration_ms instead of the snippet (see fallbackSnippet).
func TestSingleSearchAllHit_Video(t *testing.T) {
	tp := payloadTypeVideo
	doc := Doc{
		MessageID:  202,
		MessageSeq: 22,
		From:       "u5",
		Timestamp:  1717000020,
		Payload: &Payload{
			Type: &tp,
			Video: &VideoPayload{
				URL:    "https://cdn/v.mp4",
				Cover:  "https://cdn/v.jpg",
				Width:  1280,
				Height: 720,
				Second: 42,
			},
		},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}
	got := h.singleSearchAllHit(doc, "g", channelTypeGroup, nil)
	if got.ResultType != "message" {
		t.Errorf("result_type: got %q want message", got.ResultType)
	}
	if got.Message == nil {
		t.Fatalf("message must be populated for video hit")
	}
	if got.Message.MessageKind != "video" {
		t.Errorf("kind: got %q want video", got.Message.MessageKind)
	}
	if got.Message.ThumbURL != "https://cdn/v.jpg" {
		t.Errorf("thumb_url must mirror payload.video.cover: got %q", got.Message.ThumbURL)
	}
	if got.Message.VideoURL != "https://cdn/v.mp4" {
		t.Errorf("video_url must mirror payload.video.url: got %q", got.Message.VideoURL)
	}
	if got.Message.Width != 1280 || got.Message.Height != 720 {
		t.Errorf("dimensions: got %dx%d want 1280x720", got.Message.Width, got.Message.Height)
	}
	if got.Message.DurationMs != 42000 {
		t.Errorf("duration_ms: got %d want 42000 (42s * 1000)", got.Message.DurationMs)
	}
	if got.Message.Snippet != "" {
		t.Errorf("video snippet must be empty (VideoPayload has no text field): got %q", got.Message.Snippet)
	}
	if got.File != nil {
		t.Errorf("file should be nil for video result")
	}
}
