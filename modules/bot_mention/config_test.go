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

// modules/space rejects a value shared with OCTO_DOCS_BOT_MENTION_TOKEN; this
// module must run the mirror-image comparison, so a deployment that sets one
// value for both fails BOTH capabilities closed instead of disabling the
// marketplace endpoint and leaving bot mention serving on the shared value.
func TestResolveBotMentionInternalTokenRejectsMarketplaceToken(t *testing.T) {
	const shared = "shared-secret-shared-secret-1234"
	const env = "OCTO_MARKETPLACE_INTERNAL_TOKEN"
	getenv := func(key string) string {
		if key == internalTokenEnv || key == env {
			return shared
		}
		return ""
	}
	token, err := resolveBotMentionInternalToken(getenv)
	if err == nil {
		t.Fatalf("expected %s == %s to disable the bot mention capability",
			internalTokenEnv, env)
	}
	if token != "" {
		t.Fatalf("token = %q; a colliding token must come back empty", token)
	}
	if strings.Contains(err.Error(), shared) {
		t.Fatalf("error message leaked the token value: %v", err)
	}
	if !strings.Contains(err.Error(), env) {
		t.Fatalf("error %q should name the colliding env %s", err, env)
	}
}

// The pre-existing exclusion set is left alone. OCTO_DRIVE_INTERNAL_TOKEN is
// rejected by modules/internal_resolve but was never rejected here; adding the
// marketplace token is no reason to start disabling a deployment that runs that
// (pre-existing) collision today.
func TestResolveBotMentionInternalTokenLeavesPreExistingPairsAlone(t *testing.T) {
	const shared = "shared-secret-shared-secret-1234"
	getenv := func(key string) string {
		if key == internalTokenEnv || key == "OCTO_DRIVE_INTERNAL_TOKEN" {
			return shared
		}
		return ""
	}
	token, err := resolveBotMentionInternalToken(getenv)
	if err != nil {
		t.Fatalf("resolveBotMentionInternalToken() = %v; the drive pair predates this "+
			"change and must keep behaving as it did", err)
	}
	if token != shared {
		t.Fatalf("token = %q, want the configured value", token)
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

// doc_kind 决定消费端走哪套 API。HTML 文档的 doc_id 是 octo-doc 的 slug,拿它去打
// docs-backend 的 docId 接口必然 404 —— 所以这个字段错一次,任务就静默打不中目标。
func TestNormalizeMentionRequestDocKind(t *testing.T) {
	base := mentionRequest{
		IdempotencyKey: "idem-1",
		DocID:          "doc-1",
		CommentID:      "comment-1",
		FromUID:        "user-1",
		BotUID:         "bot-1",
		Text:           "update it",
	}

	t.Run("默认为空:普通文档的行为不变", func(t *testing.T) {
		got, err := normalizeMentionRequest(base)
		if err != nil {
			t.Fatalf("normalize: %v", err)
		}
		if got.DocKind != "" {
			t.Fatalf("DocKind = %q, want empty", got.DocKind)
		}
	})

	t.Run("html 大小写与空格都规范化", func(t *testing.T) {
		for _, in := range []string{"html", " HTML ", "Html"} {
			req := base
			req.DocKind = in
			got, err := normalizeMentionRequest(req)
			if err != nil {
				t.Fatalf("doc_kind=%q: %v", in, err)
			}
			if got.DocKind != docKindHTML {
				t.Fatalf("doc_kind=%q → %q, want %q", in, got.DocKind, docKindHTML)
			}
		}
	})

	t.Run("未知值被拒,不静默降级成普通文档", func(t *testing.T) {
		// ★ 这条是这个字段的要害。降级放过去的话,消费端会拿 slug 去查 docId 接口,
		// 用户看到的是一个「文档不存在」的 404,根因(类型标错)完全看不出来。
		for _, bad := range []string{"sheet", "htm", "html_ppt", "doc"} {
			req := base
			req.DocKind = bad
			if _, err := normalizeMentionRequest(req); err == nil {
				t.Fatalf("doc_kind=%q 应当被拒", bad)
			} else if invalidField(err) != "doc_kind" {
				t.Fatalf("doc_kind=%q 报的字段是 %q,want doc_kind", bad, invalidField(err))
			}
		}
	})
}

// 事件载荷:doc_kind 为空时**一个字节都不该多**。
// 既有消费者(插件)对 event_data 的字段集有精确断言,凭空多一个 key 会让它无端变红。
func TestMentionEventDataOmitsEmptyDocKind(t *testing.T) {
	base := normalizedMention{
		IdempotencyKey: "idem-1",
		DocID:          "doc-1",
		CommentID:      "comment-1",
		ThreadID:       "comment-1",
		FromUID:        "user-1",
		BotUID:         "bot-1",
		Text:           "update it",
	}

	if _, ok := mentionEventData(base, 100)["doc_kind"]; ok {
		t.Fatal("doc_kind 为空时不该出现在事件载荷里")
	}

	base.DocKind = docKindHTML
	if got := mentionEventData(base, 100)["doc_kind"]; got != docKindHTML {
		t.Fatalf("doc_kind = %v, want %q", got, docKindHTML)
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
