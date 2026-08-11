package bot_mention

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const (
	maxRequestBodyBytes = 32 * 1024
	maxIdentifierBytes  = 256
	maxTextBytes        = 10 * 1024
	maxURLBytes         = 2048

	featureEnabledEnv          = "OCTO_DOCS_BOT_MENTION_ENABLED"
	spaceAllowlistEnv          = "OCTO_DOCS_BOT_MENTION_SPACE_ALLOWLIST"
	documentAllowlistEnv       = "OCTO_DOCS_BOT_MENTION_DOC_ALLOWLIST"
	internalTokenEnv           = "OCTO_DOCS_BOT_MENTION_TOKEN"
	internalTokenHeader        = "X-Internal-Token"
	docCommentMentionEventType = "doc_comment_mention"

	// docKindHTML 标记 doc_id 是 octo-doc 的 slug(HTML 文档),而不是 docs-backend 的 docId。
	docKindHTML = "html"
)

type mentionRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	DocID          string `json:"doc_id"`
	// DocKind 说明 DocID 是哪一类文档的标识,决定消费端该走哪套 API。
	//
	// 为什么必须显式传而不是让消费端自己判:HTML 文档的 DocID 是 octo-doc 的 slug,
	// 而 docs-backend 的接口按 docId 寻址 —— 拿 slug 去查必然 404。而「404」和
	// 「文档真的不存在」无法区分,靠试错回退会把后者误判成 HTML 再失败一次。
	//
	// 空 = 默认 docs-backend 文档(doc/sheet/board),与加这个字段之前的行为一致。
	DocKind        string `json:"doc_kind,omitempty"`
	CommentID      string `json:"comment_id"`
	ParentID       string `json:"parent_id,omitempty"`
	FromUID        string `json:"from_uid"`
	BotUID         string `json:"bot_uid"`
	Text           string `json:"text"`
	URL            string `json:"url,omitempty"`
	SpaceID        string `json:"space_id,omitempty"`
}

type normalizedMention struct {
	IdempotencyKey string `json:"idempotency_key"`
	DocID          string `json:"doc_id"`
	DocKind        string `json:"doc_kind,omitempty"`
	CommentID      string `json:"comment_id"`
	ThreadID       string `json:"thread_id"`
	ParentID       string `json:"parent_id,omitempty"`
	FromUID        string `json:"from_uid"`
	BotUID         string `json:"bot_uid"`
	Text           string `json:"text"`
	URL            string `json:"url,omitempty"`
	SpaceID        string `json:"space_id,omitempty"`
}

type requestValidationError struct {
	field string
}

func (e *requestValidationError) Error() string {
	return fmt.Sprintf("invalid bot mention field %q", e.field)
}

func invalidField(err error) string {
	var validationErr *requestValidationError
	if errors.As(err, &validationErr) {
		return validationErr.field
	}
	return ""
}

func normalizeMentionRequest(req mentionRequest) (normalizedMention, error) {
	normalized := normalizedMention{
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey),
		DocID:          strings.TrimSpace(req.DocID),
		DocKind:        strings.ToLower(strings.TrimSpace(req.DocKind)),
		CommentID:      strings.TrimSpace(req.CommentID),
		ParentID:       strings.TrimSpace(req.ParentID),
		FromUID:        strings.TrimSpace(req.FromUID),
		BotUID:         strings.TrimSpace(req.BotUID),
		Text:           req.Text,
		URL:            strings.TrimSpace(req.URL),
		SpaceID:        strings.TrimSpace(req.SpaceID),
	}

	// 白名单,未知值直接拒而不是当成空。
	//
	// 静默降级成「普通文档」的代价是消费端拿 slug 去打 docs-backend 的 docId 接口,
	// 报出来的是一个看不出根因的 404;拒在入口,调用方拼错立刻知道。
	if normalized.DocKind != "" && normalized.DocKind != docKindHTML {
		return normalizedMention{}, &requestValidationError{field: "doc_kind"}
	}

	for _, field := range []struct {
		name     string
		value    string
		required bool
	}{
		{name: "idempotency_key", value: normalized.IdempotencyKey, required: true},
		{name: "doc_id", value: normalized.DocID, required: true},
		{name: "comment_id", value: normalized.CommentID, required: true},
		{name: "parent_id", value: normalized.ParentID},
		{name: "from_uid", value: normalized.FromUID, required: true},
		{name: "bot_uid", value: normalized.BotUID, required: true},
		{name: "space_id", value: normalized.SpaceID},
	} {
		if (field.required && field.value == "") || len(field.value) > maxIdentifierBytes {
			return normalizedMention{}, &requestValidationError{field: field.name}
		}
	}
	if strings.TrimSpace(normalized.Text) == "" || len(normalized.Text) > maxTextBytes {
		return normalizedMention{}, &requestValidationError{field: "text"}
	}
	if normalized.URL != "" {
		if len(normalized.URL) > maxURLBytes {
			return normalizedMention{}, &requestValidationError{field: "url"}
		}
		parsed, err := url.Parse(normalized.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return normalizedMention{}, &requestValidationError{field: "url"}
		}
	}
	normalized.ThreadID = normalized.ParentID
	if normalized.ThreadID == "" {
		normalized.ThreadID = normalized.CommentID
	}
	return normalized, nil
}

func mentionFingerprint(req normalizedMention) string {
	raw, err := json.Marshal(req)
	if err != nil {
		// normalizedMention contains only strings, so marshaling cannot fail.
		panic(fmt.Sprintf("marshal normalized bot mention: %v", err))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func mentionClaimLogHash(claimKey string) string {
	sum := sha256.Sum256([]byte(claimKey))
	return hex.EncodeToString(sum[:6])
}

func resolveBotMentionInternalToken(getenv func(string) string) (string, error) {
	if getenv == nil {
		return "", errors.New("OCTO_DOCS_BOT_MENTION_TOKEN lookup unavailable; bot mention capability disabled")
	}
	token := getenv(internalTokenEnv)
	switch {
	case token == "":
		return "", errors.New("OCTO_DOCS_BOT_MENTION_TOKEN not set; internal bot mention API will reject all requests")
	case token == getenv("NOTIFY_INTERNAL_TOKEN"):
		return "", errors.New("OCTO_DOCS_BOT_MENTION_TOKEN must differ from NOTIFY_INTERNAL_TOKEN; bot mention capability disabled")
	case token == getenv("OCTO_DOCS_NOTIFY_TOKEN"):
		return "", errors.New("OCTO_DOCS_BOT_MENTION_TOKEN must differ from OCTO_DOCS_NOTIFY_TOKEN; bot mention capability disabled")
	default:
		return token, nil
	}
}

type featureGate struct {
	enabled bool
	spaces  map[string]struct{}
	docs    map[string]struct{}
}

func newFeatureGate(enabled bool, spaceAllowlist, documentAllowlist string) featureGate {
	return featureGate{
		enabled: enabled,
		spaces:  parseAllowlist(spaceAllowlist),
		docs:    parseAllowlist(documentAllowlist),
	}
}

func featureGateFromEnv() featureGate {
	enabled, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(featureEnabledEnv)))
	if err != nil {
		enabled = false
	}
	return newFeatureGate(enabled, os.Getenv(spaceAllowlistEnv), os.Getenv(documentAllowlistEnv))
}

func parseAllowlist(raw string) map[string]struct{} {
	result := make(map[string]struct{})
	for _, item := range strings.Split(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}

func (g featureGate) Allows(docID, spaceID string) bool {
	if !g.enabled {
		return false
	}
	if _, ok := g.docs["*"]; ok {
		return true
	}
	if _, ok := g.docs[docID]; ok {
		return true
	}
	if spaceID == "" {
		return false
	}
	if _, ok := g.spaces["*"]; ok {
		return true
	}
	_, ok := g.spaces[spaceID]
	return ok
}
