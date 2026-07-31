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
)

type mentionRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	DocID          string `json:"doc_id"`
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
		CommentID:      strings.TrimSpace(req.CommentID),
		ParentID:       strings.TrimSpace(req.ParentID),
		FromUID:        strings.TrimSpace(req.FromUID),
		BotUID:         strings.TrimSpace(req.BotUID),
		Text:           req.Text,
		URL:            strings.TrimSpace(req.URL),
		SpaceID:        strings.TrimSpace(req.SpaceID),
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
	if _, ok := g.spaces["*"]; ok {
		return true
	}
	if _, ok := g.docs[docID]; ok {
		return true
	}
	if spaceID != "" {
		_, ok := g.spaces[spaceID]
		return ok
	}
	return false
}
