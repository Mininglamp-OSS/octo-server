package bot_mention

import (
	"errors"
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
		{name: "space wildcard requires non-empty space", gate: newFeatureGate(true, "*", ""), docID: "doc-z"},
		{name: "space wildcard allows any non-empty space", gate: newFeatureGate(true, "*", ""), docID: "doc-z", spaceID: "space-z", wantAllow: true},
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

func TestFeatureGateFromEnv(t *testing.T) {
	t.Setenv(featureEnabledEnv, "true")
	t.Setenv(spaceAllowlistEnv, "space-env")
	t.Setenv(documentAllowlistEnv, "doc-env")
	gate := featureGateFromEnv()
	if !gate.Allows("doc-env", "") || !gate.Allows("other", "space-env") {
		t.Fatal("environment allowlists were not loaded")
	}

	t.Setenv(featureEnabledEnv, "not-a-bool")
	if featureGateFromEnv().Allows("doc-env", "space-env") {
		t.Fatal("invalid enabled value must fail closed")
	}
}

func TestResolveBotMentionInternalToken(t *testing.T) {
	tests := []struct {
		name         string
		mentionToken string
		notifyToken  string
		docsToken    string
		want         string
		wantErr      bool
	}{
		{name: "independent token enabled", mentionToken: "mention-secret", notifyToken: "notify-secret", docsToken: "docs-notify-secret", want: "mention-secret"},
		{name: "empty token disabled", notifyToken: "notify-secret", docsToken: "docs-notify-secret", wantErr: true},
		{name: "legacy notify collision disabled", mentionToken: "shared-secret", notifyToken: "shared-secret", docsToken: "docs-notify-secret", wantErr: true},
		{name: "docs notify collision disabled", mentionToken: "shared-secret", notifyToken: "notify-secret", docsToken: "shared-secret", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getenv := func(key string) string {
				switch key {
				case internalTokenEnv:
					return tt.mentionToken
				case "NOTIFY_INTERNAL_TOKEN":
					return tt.notifyToken
				case "OCTO_DOCS_NOTIFY_TOKEN":
					return tt.docsToken
				default:
					return ""
				}
			}
			got, err := resolveBotMentionInternalToken(getenv)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveBotMentionInternalToken() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("token = %q, want %q", got, tt.want)
			}
		})
	}

	if token, err := resolveBotMentionInternalToken(nil); err == nil || token != "" {
		t.Fatalf("nil getenv = %q, %v; want disabled error", token, err)
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

func TestRequestValidationError(t *testing.T) {
	err := &requestValidationError{field: "doc_id"}
	if got := err.Error(); !strings.Contains(got, "doc_id") {
		t.Fatalf("Error() = %q", got)
	}
	if got := invalidField(errors.New("different error")); got != "" {
		t.Fatalf("invalidField(non-validation) = %q", got)
	}
}
