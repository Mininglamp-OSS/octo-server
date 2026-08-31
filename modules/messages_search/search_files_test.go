package messages_search

import (
	"strconv"
	"testing"
)

func TestBuildFileHits_FullFields(t *testing.T) {
	tp := payloadTypeFile
	doc := Doc{
		MessageID:  100,
		MessageSeq: 9,
		From:       "u1",
		Timestamp:  1717000000,
		Payload: &Payload{
			Type: &tp,
			File: &FilePayload{
				URL:       "http://example.com/a.pdf",
				Name:      "report.pdf",
				SizeBytes: 12345,
				Ext:       "pdf",
			},
		},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}
	got := h.singleFileHit(doc, "", 0, nil)
	if got.FileName != "report.pdf" {
		t.Errorf("file_name: got %q", got.FileName)
	}
	if got.FileSizeBytes != 12345 {
		t.Errorf("file_size_bytes: got %d", got.FileSizeBytes)
	}
	if got.FileExt != "pdf" {
		t.Errorf("file_ext: got %q", got.FileExt)
	}
	if got.DownloadURL == "" {
		t.Errorf("download_url should be set")
	}
	if got.PreviewURL != nil {
		t.Errorf("preview_url should always be nil this release")
	}
	if got.MessageID != strconv.FormatInt(100, 10) {
		t.Errorf("message_id: got %q", got.MessageID)
	}
}

func TestResolveFileExt_FromIndexer(t *testing.T) {
	// v1.8 indexer stores extension verbatim (no case folding) — pass through.
	if got := resolveFileExt(&FilePayload{Ext: "PDF"}); got != "PDF" {
		t.Errorf("expected verbatim PDF, got %q", got)
	}
	if got := resolveFileExt(&FilePayload{Ext: "pdf"}); got != "pdf" {
		t.Errorf("expected verbatim pdf, got %q", got)
	}
}

func TestResolveFileExt_FallbackFromName(t *testing.T) {
	if got := resolveFileExt(&FilePayload{Name: "Report.PDF"}); got != "PDF" {
		t.Errorf("expected PDF from name (verbatim), got %q", got)
	}
	if got := resolveFileExt(&FilePayload{Name: "noext"}); got != "" {
		t.Errorf("expected empty for no ext, got %q", got)
	}
	if got := resolveFileExt(&FilePayload{Name: "archive.tar.gz"}); got != "gz" {
		t.Errorf("filepath.Ext is the trailing segment; got %q", got)
	}
}

func TestSingleFileHit_NilPayload(t *testing.T) {
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}
	doc := Doc{MessageID: 1, MessageSeq: 1, Timestamp: 100}
	got := h.singleFileHit(doc, "", 0, nil)
	if got.FileName != "" || got.DownloadURL != "" {
		t.Errorf("nil payload should leave file fields empty: %+v", got)
	}
	if got.PreviewURL != nil {
		t.Errorf("preview_url should remain nil")
	}
}

func TestSingleFileHit_Highlights(t *testing.T) {
	tp := payloadTypeFile
	doc := Doc{
		MessageID: 5, MessageSeq: 1, From: "u1", Timestamp: 1717000000,
		Payload: &Payload{Type: &tp, File: &FilePayload{Name: "quarterly-report.pdf"}},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}

	// Name matched: name_highlight carries the marked name; content_snippet empty.
	nameOnly := h.singleFileHit(doc, "", 0, map[string][]string{
		"payload.file.name": {"quarterly-<mark>report</mark>.pdf"},
	})
	if nameOnly.NameHighlight != "quarterly-<mark>report</mark>.pdf" {
		t.Errorf("name_highlight: got %q", nameOnly.NameHighlight)
	}
	if nameOnly.ContentSnippet != "" {
		t.Errorf("content_snippet should be empty when body didn't match, got %q", nameOnly.ContentSnippet)
	}
	// FileName always echoes the raw name so the client can fall back.
	if nameOnly.FileName != "quarterly-report.pdf" {
		t.Errorf("file_name should stay raw, got %q", nameOnly.FileName)
	}

	// Body-only match: content_snippet carries the marked fragment; name_highlight empty.
	bodyOnly := h.singleFileHit(doc, "", 0, map[string][]string{
		"payload.file.content": {"...annual <mark>revenue</mark> grew..."},
	})
	if bodyOnly.NameHighlight != "" {
		t.Errorf("name_highlight should be empty for body-only hit, got %q", bodyOnly.NameHighlight)
	}
	if bodyOnly.ContentSnippet != "...annual <mark>revenue</mark> grew..." {
		t.Errorf("content_snippet: got %q", bodyOnly.ContentSnippet)
	}

	// Browse path (nil highlight): both empty.
	browse := h.singleFileHit(doc, "", 0, nil)
	if browse.NameHighlight != "" || browse.ContentSnippet != "" {
		t.Errorf("browse path should carry no highlight fields: %+v", browse)
	}
}
