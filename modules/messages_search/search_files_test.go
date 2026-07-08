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
	got := h.singleFileHit(doc, "", 0)
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
	got := h.singleFileHit(doc, "", 0)
	if got.FileName != "" || got.DownloadURL != "" {
		t.Errorf("nil payload should leave file fields empty: %+v", got)
	}
	if got.PreviewURL != nil {
		t.Errorf("preview_url should remain nil")
	}
}
