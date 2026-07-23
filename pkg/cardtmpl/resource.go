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
	"unicode"
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
	// Variant is the stable, low-cardinality identifier the renderer / client
	// can key off to style card families independently (e.g. "summary.completed",
	// "summary.failed", "docs.shared", "docs.commented"). Emitted into
	// `metadata.octo.variant` on the AdaptiveCard root; unknown values pass
	// straight through — the renderer decides whether to specialise.
	// See docs/summary-notify-card.md §2 for the reserved namespace.
	Variant string
	// Source names the originating capability so renderers can surface a
	// "来自 XX" / "From XX" chip / badge (角标) next to the card. Both fields
	// are optional and land in `metadata.octo.source`; the current template
	// does NOT auto-render them into the card body — surfacing is a renderer
	// decision (chip / prefix / silent), which lets the visual pilot stay
	// unchanged while machine consumers already have the source signal.
	Source Source
}

// Source describes the producer family that authored the card, for both
// display badging and machine identification. Values are server-picked (never
// user-supplied) and land in `metadata.octo.source`.
type Source struct {
	// Label is a human-readable source name. It is already localized by the
	// caller: producers pass it from their per-language label set
	// (modules/notify summaryLabelsFor/docsLabelsFor, keyed by
	// i18n.OutboundLanguage) — e.g. "智能总结"/"Smart Summary",
	// "文档"/"Docs" — so this template stores whatever localized string it
	// receives and does not itself pick a language.
	Label string
	// IconURL is an optional absolute https icon URL; renderers may use it for
	// the chip. Empty by default (the pilot sets Label only).
	IconURL string
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
	deepLink, err := summaryDeepLink(webLoginURL, taskID, spaceID)
	if err != nil {
		return nil, err
	}
	return buildResourceCard(ctx, deepLink, resource)
}

// BuildSummaryResourceCardBodyWithLang is the fragment-only counterpart of
// BuildSummaryResourceCard for Registry-backed octo/v1 summary Templates
// (roadmap C PR-2: summary.completed / summary.failed). Shape mirrors
// BuildDocsResourceCardBodyWithLang — returns the assembled body + https
// deep-link and never marshals the AC top level or writes metadata; that is
// Registry.Render's job. Both this helper and the legacy BuildSummaryResourceCard
// share assembleResourceCardBody, so migrated cards render byte-identical bodies.
func BuildSummaryResourceCardBodyWithLang(
	lang string,
	webLoginURL string,
	taskID string,
	spaceID string,
	resource ResourceCard,
) (body []interface{}, deepLink string, err error) {
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(spaceID) == "" {
		return nil, "", errors.New("cardtmpl: task ID and space ID are required")
	}
	deepLink, err = summaryDeepLink(webLoginURL, taskID, spaceID)
	if err != nil {
		return nil, "", err
	}
	body, err = assembleResourceCardBody(lang, deepLink, resource)
	if err != nil {
		return nil, "", err
	}
	return body, deepLink, nil
}

// BuildDocsResourceCard renders an octo/v1 ResourceCard for a docs-notify
// notification (docs-backend automated flows: share/comment/access request).
// The deep-link points at octo-web's /d/:docId standalone route (mirrors
// summaryDeepLink); labels/attribution mapping stay in the producer module.
func BuildDocsResourceCard(
	ctx context.Context,
	webLoginURL string,
	docID string,
	spaceID string,
	resource ResourceCard,
) (json.RawMessage, error) {
	if strings.TrimSpace(docID) == "" || strings.TrimSpace(spaceID) == "" {
		return nil, errors.New("cardtmpl: doc ID and space ID are required")
	}
	deepLink, err := docsDeepLink(webLoginURL, docID, spaceID)
	if err != nil {
		return nil, err
	}
	return buildResourceCard(ctx, deepLink, resource)
}

// buildResourceCard assembles the octo/v1 body (header + optional excerpt +
// optional FactSet + ActionSet) from a validated ResourceCard and an already
// built https deep-link URL. Callers own scenario-specific input validation
// (identifier presence, deep-link shape); this helper enforces the
// scenario-independent bounds (title length, fact count, copy text size,
// icon-must-be-https) and returns the marshalled AC 1.5 document.
func buildResourceCard(
	ctx context.Context,
	deepLink string,
	resource ResourceCard,
) (json.RawMessage, error) {
	body, err := assembleResourceCardBody(i18n.OutboundLanguage(ctx), deepLink, resource)
	if err != nil {
		return nil, err
	}
	document := map[string]interface{}{
		"type":     "AdaptiveCard",
		"version":  cardmsg.CardVersion,
		"metadata": buildMetadata(deepLink, resource.Variant, resource.Source),
		"body":     body,
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("cardtmpl: marshal resource card: %w", err)
	}
	return json.RawMessage(raw), nil
}

// BuildDocsResourceCardBodyWithLang is the fragment-only counterpart of
// BuildDocsResourceCard used by Registry-backed octo/v1 display Templates
// (e.g. pkg/cardtmpl/docs_commented). It returns the assembled body (header +
// optional excerpt + optional FactSet + in-body ActionSet) and the https
// deep-link, but does NOT marshal the AC top level or write metadata — that is
// Registry.Render's job. It shares assembleResourceCardBody with the legacy
// BuildDocsResourceCard wrapper, so migrated cards render byte-identical bodies.
//
// lang is passed explicitly (resolved by the caller from i18n.OutboundLanguage);
// this helper never touches ctx, matching BuildDocsAccessRequestBodyWithLang.
func BuildDocsResourceCardBodyWithLang(
	lang string,
	webLoginURL string,
	docID string,
	spaceID string,
	resource ResourceCard,
) (body []interface{}, deepLink string, err error) {
	if strings.TrimSpace(docID) == "" || strings.TrimSpace(spaceID) == "" {
		return nil, "", errors.New("cardtmpl: doc ID and space ID are required")
	}
	deepLink, err = docsDeepLink(webLoginURL, docID, spaceID)
	if err != nil {
		return nil, "", err
	}
	body, err = assembleResourceCardBody(lang, deepLink, resource)
	if err != nil {
		return nil, "", err
	}
	return body, deepLink, nil
}

// assembleResourceCardBody validates the scenario-independent bounds (title
// length, fact count/shape, copy-text size, icon-must-be-https, source bounds)
// and assembles the octo/v1 body: header (optional icon column) + optional
// excerpt + optional FactSet + a trailing ActionSet (view-details + optional
// copy). Both buildResourceCard (marshals a full document + metadata) and
// BuildDocsResourceCardBodyWithLang (fragment-only) call it, so the rendered
// body is a single source of truth across the legacy and Registry paths.
//
// The excerpt is truncated to MaxExcerptRunes here (render-time cap); a card's
// input schema declares the same maxLength so the two never drift (see the
// docs_commented G9 field-cap test).
func assembleResourceCardBody(lang string, deepLink string, resource ResourceCard) ([]interface{}, error) {
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
	if resource.Source.IconURL != "" {
		if err := requireHTTPS(resource.Source.IconURL); err != nil {
			return nil, fmt.Errorf("cardtmpl: source icon URL: %w", err)
		}
	}
	if utf8.RuneCountInString(resource.Source.Label) > maxTitleRunes {
		return nil, errors.New("cardtmpl: source label too long")
	}

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

	return body, nil
}

func summaryDeepLink(webLoginURL, taskID, spaceID string) (string, error) {
	origin, err := webOrigin(webLoginURL)
	if err != nil {
		return "", err
	}
	return origin + "/s/" + url.PathEscape(taskID) + "?sp=" + url.QueryEscape(spaceID), nil
}

// docsDeepLink mirrors summaryDeepLink but targets octo-web's already-live
// standalone doc route /d/:docId (cold-load → login bounce → multi-session
// sid recovery, XIN-398 suite). The origin comes from External.WebLoginURL
// (positive-allowlisted absolute https).
func docsDeepLink(webLoginURL, docID, spaceID string) (string, error) {
	origin, err := webOrigin(webLoginURL)
	if err != nil {
		return "", err
	}
	return origin + "/d/" + url.PathEscape(docID) + "?sp=" + url.QueryEscape(spaceID), nil
}

// buildMetadata assembles the AdaptiveCard 1.5 `metadata` object. The standard
// `webUrl` sub-field (canonical browser URL for this card) is always set from
// the same deep-link that drives the primary "View details" action, so
// client-side features that key off the URL don't need to walk actions. The
// `octo` sub-object namespaces our extensions:
//   - `variant`: stable low-cardinality identifier (`summary.completed` etc.)
//   - `source`:  originating capability {label, iconUrl} for renderer badges
//
// Both octo sub-fields are omitted when empty to keep the wire minimal.
func buildMetadata(deepLink, variant string, source Source) map[string]interface{} {
	metadata := map[string]interface{}{"webUrl": deepLink}
	octo := map[string]interface{}{}
	if strings.TrimSpace(variant) != "" {
		octo["variant"] = variant
	}
	if strings.TrimSpace(source.Label) != "" || strings.TrimSpace(source.IconURL) != "" {
		s := map[string]interface{}{}
		if strings.TrimSpace(source.Label) != "" {
			s["label"] = source.Label
		}
		if strings.TrimSpace(source.IconURL) != "" {
			s["iconUrl"] = source.IconURL
		}
		octo["source"] = s
	}
	if len(octo) > 0 {
		metadata["octo"] = octo
	}
	return metadata
}

func webOrigin(webLoginURL string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(webLoginURL))
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return "", errors.New("cardtmpl: web base URL must be absolute https")
	}
	return (&url.URL{Scheme: base.Scheme, Host: base.Host}).String(), nil
}

// requireHTTPS 委托 AbsoluteHTTPSURL (render.go) —— 单一真源,避免与 preflight
// 的绝对-https 判定漂移。空串由本包各调用点在调用前 guard (空头像/空 icon 合法)。
func requireHTTPS(raw string) error {
	return AbsoluteHTTPSURL(raw)
}

// SanitizeLine collapses any control character (newline, CR, tab, …) in a
// caller-supplied string to a single space and trims the result. Text-path
// (plain-text DM) fallbacks are line-structured, so an embedded "\n" in a
// caller field (title, excerpt, actor name) could otherwise inject a spoofed
// attribution/label line. Card rendering has its own defence (escapeMarkdown);
// this is the text-path equivalent. It is a strict superset of strings.TrimSpace
// for our inputs. Shared source of truth for modules/notify and each L2a
// Template's FallbackText — do NOT re-implement (G5, roadmap C).
func SanitizeLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s))
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
