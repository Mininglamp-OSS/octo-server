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

// TestEscapeHighlightFragment pins the stored-XSS defence on the two new file
// fields: every HTML metacharacter in the OpenSearch fragment is entity-
// escaped in Go except the literal <mark>/</mark> tags configured on the
// highlighter, so uploader-controlled source text (payload.file.name is fully
// user-controlled; payload.file.content is Tika-extracted uploader body)
// cannot round-trip as executable markup in a client that renders the fields
// as HTML.
func TestEscapeHighlightFragment(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "no HTML metachars, no <mark>",
			in:   "just plain text",
			want: "just plain text",
		},
		{
			name: "keyword inside <mark>",
			in:   "foo <mark>bar</mark> baz",
			want: "foo <mark>bar</mark> baz",
		},
		{
			name: "hostile file name (uploader HTML) — mark preserved, HTML escaped",
			in:   "<img src=x onerror=alert(1)>.<mark>pdf</mark>",
			want: "&lt;img src=x onerror=alert(1)&gt;.<mark>pdf</mark>",
		},
		{
			name: "hostile body fragment — mark preserved, <script> escaped",
			in:   "quarterly <script>alert(1)</script> <mark>report</mark>",
			want: "quarterly &lt;script&gt;alert(1)&lt;/script&gt; <mark>report</mark>",
		},
		{
			name: "ampersand / quote round-trip",
			in:   `R&D "v2" <mark>foo</mark>`,
			want: `R&amp;D &#34;v2&#34; <mark>foo</mark>`,
		},
		{
			name: "non-mark HTML tag inside a marked span is still escaped",
			in:   "prefix <mark><b>bold</b></mark> suffix",
			want: "prefix <mark>&lt;b&gt;bold&lt;/b&gt;</mark> suffix",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := escapeHighlightFragment(tc.in)
			if got != tc.want {
				t.Errorf("escapeHighlightFragment(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSingleFileHit_HostileContentEscaped pins the end-to-end wire behaviour:
// a hostile file name / body fragment reaching pickFileNameHighlight /
// pickFileContentSnippet gets its uploader-controlled HTML entity-escaped
// while the <mark> highlight stays live in FileHit.NameHighlight /
// ContentSnippet. Any consumer that renders these fields as HTML gets one
// live tag and no XSS surface.
func TestSingleFileHit_HostileContentEscaped(t *testing.T) {
	tp := payloadTypeFile
	doc := Doc{
		MessageID: 7, MessageSeq: 2, From: "u1", Timestamp: 1717000000,
		Payload: &Payload{Type: &tp, File: &FilePayload{Name: "<img src=x onerror=alert(1)>.pdf"}},
	}
	h := &Handler{cfg: SearchConfig{}, cache: newSenderCache(8, 0)}

	hit := h.singleFileHit(doc, "", 0, map[string][]string{
		"payload.file.name":    {"<img src=x onerror=alert(1)>.<mark>pdf</mark>"},
		"payload.file.content": {"quarterly <script>alert(1)</script> <mark>report</mark>"},
	})

	wantName := "&lt;img src=x onerror=alert(1)&gt;.<mark>pdf</mark>"
	if hit.NameHighlight != wantName {
		t.Errorf("hostile name highlight not escaped:\n got: %q\nwant: %q", hit.NameHighlight, wantName)
	}
	wantSnippet := "quarterly &lt;script&gt;alert(1)&lt;/script&gt; <mark>report</mark>"
	if hit.ContentSnippet != wantSnippet {
		t.Errorf("hostile content snippet not escaped:\n got: %q\nwant: %q", hit.ContentSnippet, wantSnippet)
	}
	// FileName wire field carries the raw uploader-controlled name (long-shipped
	// behaviour, contract unchanged) — clients that render FileName must sanitise
	// on their own end just as they did before this PR.
	if hit.FileName != "<img src=x onerror=alert(1)>.pdf" {
		t.Errorf("file_name should stay raw for backward compat, got %q", hit.FileName)
	}
}
