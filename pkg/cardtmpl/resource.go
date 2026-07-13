// Package cardtmpl owns reviewed server-side Adaptive Card templates. It does
// not dispatch messages; producer modules pass its document to carddispatch.
package cardtmpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

const (
	MaxExcerptRunes = 300
	maxFacts        = 20
	maxTitleRunes   = 200
	maxFactRunes    = 500
)

type Fact struct {
	Title string
	Value string
}

type ResourceCard struct {
	IconURL     string
	Title       string
	Attribution string
	Excerpt     string
	Facts       []Fact
	CopyText    string
}

func BuildSummaryResourceCard(
	ctx context.Context,
	webLoginURL string,
	taskID string,
	spaceID string,
	resource ResourceCard,
) (json.RawMessage, error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(spaceID) == "" {
		return nil, errors.New("cardtmpl: task ID and space ID are required")
	}
	if strings.TrimSpace(resource.Title) == "" || utf8.RuneCountInString(resource.Title) > maxTitleRunes {
		return nil, errors.New("cardtmpl: resource title is invalid")
	}
	if len(resource.Facts) > maxFacts {
		return nil, errors.New("cardtmpl: too many facts")
	}
	for _, fact := range resource.Facts {
		if strings.TrimSpace(fact.Title) == "" || strings.TrimSpace(fact.Value) == "" ||
			utf8.RuneCountInString(fact.Title) > maxFactRunes || utf8.RuneCountInString(fact.Value) > maxFactRunes {
			return nil, errors.New("cardtmpl: invalid fact")
		}
	}
	if len(resource.CopyText) > cardmsg.MaxCopyTextBytes {
		return nil, errors.New("cardtmpl: copy text too large")
	}
	// The icon is optional; when present it must be an absolute https URL (the
	// same positive-allowlist rule cardmsg.Validate re-checks). An empty icon
	// simply omits the leading image column.
	if resource.IconURL != "" {
		if err := requireHTTPS(resource.IconURL); err != nil {
			return nil, fmt.Errorf("cardtmpl: icon URL: %w", err)
		}
	}
	deepLink, err := summaryDeepLink(webLoginURL, taskID, spaceID)
	if err != nil {
		return nil, err
	}

	lang := i18n.OutboundLanguage(ctx)
	labels := labelsForLanguage(lang)
	titleItems := []interface{}{
		map[string]interface{}{
			"type":   "TextBlock",
			"text":   escapeMarkdown(resource.Title),
			"weight": "Bolder",
			"wrap":   true,
		},
	}
	if strings.TrimSpace(resource.Attribution) != "" {
		titleItems = append(titleItems, map[string]interface{}{
			"type":     "TextBlock",
			"text":     escapeMarkdown(resource.Attribution),
			"isSubtle": true,
			"spacing":  "None",
			"wrap":     true,
		})
	}
	var header map[string]interface{}
	if resource.IconURL != "" {
		header = map[string]interface{}{
			"type": "ColumnSet",
			"columns": []interface{}{
				map[string]interface{}{
					"type":  "Column",
					"width": "auto",
					"items": []interface{}{
						map[string]interface{}{"type": "Image", "url": resource.IconURL, "size": "Small"},
					},
				},
				map[string]interface{}{
					"type":  "Column",
					"width": "stretch",
					"items": titleItems,
				},
			},
		}
	} else {
		// No icon: place the title block(s) directly, without a column wrapper.
		header = map[string]interface{}{"type": "Container", "items": titleItems}
	}
	body := []interface{}{header}
	if excerpt := truncateRunes(strings.TrimSpace(resource.Excerpt), MaxExcerptRunes); excerpt != "" {
		body = append(body, map[string]interface{}{
			"type": "TextBlock",
			"text": escapeMarkdown(excerpt),
			"wrap": true,
		})
	}
	if len(resource.Facts) > 0 {
		facts := make([]interface{}, 0, len(resource.Facts))
		for _, fact := range resource.Facts {
			facts = append(facts, map[string]interface{}{
				"title": escapeMarkdown(fact.Title),
				"value": escapeMarkdown(fact.Value),
			})
		}
		body = append(body, map[string]interface{}{"type": "FactSet", "facts": facts})
	}
	actions := []interface{}{
		map[string]interface{}{"type": "Action.OpenUrl", "title": labels.viewDetails, "url": deepLink},
	}
	if resource.CopyText != "" {
		actions = append(actions, map[string]interface{}{
			"type": "Action.CopyToClipboard", "title": labels.copy, "text": resource.CopyText,
		})
	}
	body = append(body, map[string]interface{}{"type": "ActionSet", "actions": actions})

	document := map[string]interface{}{
		"type":    "AdaptiveCard",
		"version": cardmsg.CardVersion,
		"body":    body,
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("cardtmpl: marshal resource card: %w", err)
	}
	return json.RawMessage(raw), nil
}

func summaryDeepLink(webLoginURL, taskID, spaceID string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(webLoginURL))
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return "", errors.New("cardtmpl: web base URL must be absolute https")
	}
	origin := (&url.URL{Scheme: base.Scheme, Host: base.Host}).String()
	return origin + "/s/" + url.PathEscape(taskID) + "?sp=" + url.QueryEscape(spaceID), nil
}

func requireHTTPS(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return errors.New("must be absolute https")
	}
	return nil
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit <= 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}

func escapeMarkdown(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`*`, `\*`,
		`_`, `\_`,
		`[`, `\[`,
		`]`, `\]`,
		`(`, `\(`,
		`)`, `\)`,
		`<`, `\<`,
		`>`, `\>`,
		"`", "\\`",
		`#`, `\#`,
		`~`, `\~`,
		`|`, `\|`,
	)
	return replacer.Replace(value)
}

type localizedLabels struct {
	viewDetails string
	copy        string
}

func labelsForLanguage(lang string) localizedLabels {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh-") {
		return localizedLabels{viewDetails: "查看详情", copy: "复制"}
	}
	return localizedLabels{viewDetails: "View details", copy: "Copy"}
}
