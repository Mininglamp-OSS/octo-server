package bot_mention

import (
	"strings"
	"testing"
)

func TestFeatureGate(t *testing.T) {
	tests := []struct {
		name      string
		gate      featureGate
		docID     string
		spaceID   string
		wantAllow bool
	}{
		{name: "global switch off", gate: newFeatureGate(false, "space-a", "doc-a"), docID: "doc-a", spaceID: "space-a"},
		{name: "empty allowlists fail closed", gate: newFeatureGate(true, "", ""), docID: "doc-a", spaceID: "space-a"},
		{name: "doc allowlist hit", gate: newFeatureGate(true, "", "doc-a, doc-b"), docID: "doc-b", wantAllow: true},
		{name: "space allowlist hit", gate: newFeatureGate(true, "space-a", ""), docID: "doc-z", spaceID: "space-a", wantAllow: true},
		{name: "missing space cannot hit space allowlist", gate: newFeatureGate(true, "space-a", ""), docID: "doc-z"},
		{name: "explicit wildcard enables all", gate: newFeatureGate(true, "", "*"), docID: "doc-z", wantAllow: true},
		{name: "whitespace is normalized", gate: newFeatureGate(true, " space-a , space-b ", ""), spaceID: "space-b", wantAllow: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.gate.Allows(tt.docID, tt.spaceID); got != tt.wantAllow {
				t.Fatalf("Allows(%q, %q) = %v, want %v", tt.docID, tt.spaceID, got, tt.wantAllow)
			}
		})
	}
}

func TestNormalizeMentionRequest(t *testing.T) {
	valid := mentionRequest{
		IdempotencyKey: " idem-1 ",
		DocID:          " doc-1 ",
		CommentID:      " comment-1 ",
		ParentID:       " root-1 ",
		FromUID:        " user-1 ",
		BotUID:         " bot-1 ",
		Text:           "  please update the document  ",
		URL:            " https://docs.example.test/doc-1 ",
		SpaceID:        " space-1 ",
	}

	got, err := normalizeMentionRequest(valid)
	if err != nil {
		t.Fatalf("normalize valid request: %v", err)
	}
	if got.IdempotencyKey != "idem-1" || got.DocID != "doc-1" || got.CommentID != "comment-1" {
		t.Fatalf("identifiers were not normalized: %+v", got)
	}
	if got.ThreadID != "root-1" {
		t.Fatalf("ThreadID = %q, want root-1", got.ThreadID)
	}
	if got.Text != valid.Text {
		t.Fatalf("Text = %q, want original %q", got.Text, valid.Text)
	}
	if got.URL != "https://docs.example.test/doc-1" {
		t.Fatalf("URL = %q", got.URL)
	}

	valid.ParentID = ""
	got, err = normalizeMentionRequest(valid)
	if err != nil {
		t.Fatalf("normalize root comment: %v", err)
	}
	if got.ThreadID != "comment-1" {
		t.Fatalf("root ThreadID = %q, want comment-1", got.ThreadID)
	}
}

func TestNormalizeMentionRequestRejectsInvalidFields(t *testing.T) {
	base := mentionRequest{
		IdempotencyKey: "idem-1",
		DocID:          "doc-1",
		CommentID:      "comment-1",
		FromUID:        "user-1",
		BotUID:         "bot-1",
		Text:           "update it",
	}

	tests := []struct {
		name   string
		mutate func(*mentionRequest)
		field  string
	}{
		{name: "missing idempotency key", mutate: func(r *mentionRequest) { r.IdempotencyKey = " " }, field: "idempotency_key"},
		{name: "oversized doc id", mutate: func(r *mentionRequest) { r.DocID = strings.Repeat("d", maxIdentifierBytes+1) }, field: "doc_id"},
		{name: "missing comment id", mutate: func(r *mentionRequest) { r.CommentID = "" }, field: "comment_id"},
		{name: "oversized parent id", mutate: func(r *mentionRequest) { r.ParentID = strings.Repeat("p", maxIdentifierBytes+1) }, field: "parent_id"},
		{name: "missing actor", mutate: func(r *mentionRequest) { r.FromUID = "" }, field: "from_uid"},
		{name: "missing bot", mutate: func(r *mentionRequest) { r.BotUID = "" }, field: "bot_uid"},
		{name: "blank text", mutate: func(r *mentionRequest) { r.Text = " \n\t " }, field: "text"},
		{name: "oversized text", mutate: func(r *mentionRequest) { r.Text = strings.Repeat("x", maxTextBytes+1) }, field: "text"},
		{name: "relative url", mutate: func(r *mentionRequest) { r.URL = "/doc/1" }, field: "url"},
		{name: "unsafe url scheme", mutate: func(r *mentionRequest) { r.URL = "javascript:alert(1)" }, field: "url"},
		{name: "url without host", mutate: func(r *mentionRequest) { r.URL = "https:///doc/1" }, field: "url"},
		{name: "oversized url", mutate: func(r *mentionRequest) { r.URL = "https://example.test/" + strings.Repeat("u", maxURLBytes) }, field: "url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.mutate(&req)
			_, err := normalizeMentionRequest(req)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if got := invalidField(err); got != tt.field {
				t.Fatalf("invalidField = %q, want %q (err=%v)", got, tt.field, err)
			}
		})
	}
}

func TestMentionFingerprintIsStableAndCoversExecutionFields(t *testing.T) {
	req, err := normalizeMentionRequest(mentionRequest{
		IdempotencyKey: "idem-1", DocID: "doc-1", CommentID: "comment-1",
		FromUID: "user-1", BotUID: "bot-1", Text: "update it",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := mentionFingerprint(req)
	if first == "" || first != mentionFingerprint(req) {
		t.Fatalf("fingerprint must be non-empty and stable: %q", first)
	}
	req.Text = "different instruction"
	if first == mentionFingerprint(req) {
		t.Fatal("fingerprint must change when execution-relevant content changes")
	}
}
