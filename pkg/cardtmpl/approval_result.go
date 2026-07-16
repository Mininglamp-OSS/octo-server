package cardtmpl

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

type ApprovalResultCard struct {
	Title   string
	Status  string
	Variant string
	Source  string
}

// BuildApprovalResultCard renders the shared terminal/outcome visual for
// config-only approval consumers. It deliberately has no URL or arbitrary
// callback-authored layout: the consumer supplies display text only and octo
// owns the complete Adaptive Card shape.
func BuildApprovalResultCard(result ApprovalResultCard) (json.RawMessage, error) {
	title := truncateRunes(strings.TrimSpace(result.Title), maxTitleRunes)
	status := truncateRunes(strings.TrimSpace(result.Status), maxFactRunes)
	if title == "" || status == "" {
		return nil, errors.New("cardtmpl: approval result title and status are required")
	}
	if utf8.RuneCountInString(result.Source) > maxTitleRunes {
		return nil, errors.New("cardtmpl: approval result source too long")
	}
	octo := map[string]interface{}{}
	if variant := strings.TrimSpace(result.Variant); variant != "" {
		octo["variant"] = variant
	}
	if source := strings.TrimSpace(result.Source); source != "" {
		octo["source"] = map[string]interface{}{"label": source}
	}
	card := map[string]interface{}{
		"type":    "AdaptiveCard",
		"version": cardmsg.CardVersion,
		"body": []interface{}{
			map[string]interface{}{"type": "TextBlock", "text": escapeMarkdown(title), "weight": "Bolder", "wrap": true},
			map[string]interface{}{"type": "TextBlock", "text": escapeMarkdown(status), "isSubtle": true, "spacing": "None", "wrap": true},
		},
	}
	if len(octo) > 0 {
		card["metadata"] = map[string]interface{}{"octo": octo}
	}
	raw, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("cardtmpl: marshal approval result card: %w", err)
	}
	return raw, nil
}
