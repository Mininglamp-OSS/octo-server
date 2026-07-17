package cardtmpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

const (
	DocsApproveActionID = "docs-access-approve"
	DocsDenyActionID    = "docs-access-deny"
	// DocsDenyReasonInputID is the id of the (hidden) Input.Text that declares the
	// reviewer deny-reason channel. The reason must ride under a *declared* input
	// id — the card/action endpoint fail-closes on undeclared inputs
	// (pkg/cardmsg.ValidateInputs) — so this id is the cross-repo contract the web
	// deny dialog submits (inputs[DocsDenyReasonInputID]) and the value reaches the
	// docs backend verbatim via DecisionRequest.Inputs.
	DocsDenyReasonInputID = "deny_reason"

	maxActorRunes     = 120
	maxTimestampRunes = 80
	// maxReasonRunes bounds the reason text rendered on the card and the hidden
	// input's maxLength hint. Note this is independent of the server-side submit
	// enforcement boundary: cardmsg.ValidateInputs caps a submitted deny_reason by
	// cardmsg.MaxInputTextBytes (4 KiB, byte-based), not by this rune count — a
	// client that ignores maxLength can still submit up to that byte cap, and the
	// display path re-truncates to this bound. Keep the two limits in mind separately.
	maxReasonRunes = MaxExcerptRunes
)

type ApprovalActions struct {
	ApproveTitle string
	DenyTitle    string
}

// DocsApprovalContent carries the enriched docs access-request card fields. All
// string fields except the *Label ones are caller-supplied display strings that
// this template escapes (escapeMarkdown) and bounds; the labels are server-owned
// localized copy (producer picks them by language). ActorAvatar is optional and
// must be an absolute https URL when present (empty => no avatar column).
type DocsApprovalContent struct {
	Title       string
	Actor       string
	ActorAvatar string
	Timestamp   string
	Reason      string
	Variant     string
	Source      Source

	HeaderLabel  string // e.g. "文档申请" / "Document access"
	StatusLabel  string // e.g. "待你处理" / "Pending"
	BannerSuffix string // e.g. "申请成为此文档的查看者。" (rendered subtle after the bold actor)
	RoleLabel    string // e.g. "申请人" / "Requester"
	ReasonLabel  string // e.g. "申请原因" / "Reason"
}

// DocsOutcomeContent carries the enriched terminal (已允许 / 已拒绝) card fields.
// The finalizer only has title + decision (+ the reviewer reason for denials), so
// this state renders header + title + a result box — not the full requester row.
type DocsOutcomeContent struct {
	Title   string
	Variant string
	Source  Source
	Denied  bool // true => denied (attention), false => approved (good)

	HeaderLabel string // "文档申请"
	StatusLabel string // "已允许" / "已拒绝"
	ResultText  string // "申请人已获得所申请的文档权限。" / "申请已被拒绝。"
	ReasonLabel string // "拒绝原因"
	Reason      string // reviewer deny reason (denied only); "" => omit
}

// BuildDocsAccessRequestCard renders the enriched docs access-request approval
// card: a header line (label + status), a large document title, a requester row
// (optional avatar + name + role + timestamp), a boxed reason section, a hidden
// deny-reason input, and the two server-authored Submit actions (+ view-details).
// The callback route never appears in card bytes; only bounded domain data the
// registered docs route needs is embedded.
func BuildDocsAccessRequestCard(
	ctx context.Context,
	webLoginURL string,
	docID string,
	requestID string,
	spaceID string,
	content DocsApprovalContent,
	actions ApprovalActions,
) (json.RawMessage, error) {
	if strings.TrimSpace(requestID) == "" || utf8.RuneCountInString(requestID) > 200 {
		return nil, errors.New("cardtmpl: request ID is invalid")
	}
	if strings.TrimSpace(actions.ApproveTitle) == "" || strings.TrimSpace(actions.DenyTitle) == "" ||
		utf8.RuneCountInString(actions.ApproveTitle) > 80 || utf8.RuneCountInString(actions.DenyTitle) > 80 {
		return nil, errors.New("cardtmpl: approval action labels are invalid")
	}
	if strings.TrimSpace(content.Title) == "" || utf8.RuneCountInString(content.Title) > maxTitleRunes {
		return nil, errors.New("cardtmpl: resource title is invalid")
	}
	if content.ActorAvatar != "" {
		if err := requireHTTPS(content.ActorAvatar); err != nil {
			return nil, fmt.Errorf("cardtmpl: actor avatar URL: %w", err)
		}
	}
	if content.Source.IconURL != "" {
		if err := requireHTTPS(content.Source.IconURL); err != nil {
			return nil, fmt.Errorf("cardtmpl: source icon URL: %w", err)
		}
	}
	deepLink, err := docsDeepLink(webLoginURL, docID, spaceID)
	if err != nil {
		return nil, err
	}
	lang := i18n.OutboundLanguage(ctx)
	labels := labelsForLanguage(lang)

	body := []interface{}{
		docsApprovalHeader(content.HeaderLabel, content.StatusLabel, "Warning"),
		docsApprovalTitle(content.Title),
		docsBanner(content.Actor, content.BannerSuffix),
	}
	if row := docsRequesterRow(content.Actor, content.ActorAvatar, content.RoleLabel, content.Timestamp); row != nil {
		body = append(body, row)
	}
	if box := docsReasonBox(content.ReasonLabel, content.Reason); box != nil {
		body = append(body, box)
	}
	// Hidden Input.Text declares the deny-reason id so a submitted
	// inputs[deny_reason] survives ValidateInputs. It is not shown in the card;
	// the web deny dialog owns reason capture and populates the submit inputs.
	body = append(body, map[string]interface{}{
		"type": "Input.Text", "id": DocsDenyReasonInputID, "isVisible": false,
		"isMultiline": true, "maxLength": maxReasonRunes,
	})

	baseData := map[string]interface{}{
		"owner":       "docs",
		"action_type": "access_request.decision",
		"doc_id":      docID,
		"request_id":  requestID,
		// doc_title / actor are display context the web deny dialog reads locally to
		// render "<actor> will not get access to <title>". They are not the reason
		// channel (that is the declared deny_reason input) and the server still
		// re-extracts data from the stored frame — a client copy is never trusted.
		// Bound both to the same caps the render paths use so the persisted frame /
		// wire payload can't grow unbounded (Title is already validated <= maxTitleRunes
		// at entry; Actor is only display-truncated in the helpers, so bound it here).
		"doc_title": content.Title,
		"actor":     truncateRunes(strings.TrimSpace(content.Actor), maxActorRunes),
	}
	approveData := copyActionData(baseData)
	approveData["decision"] = "approve"
	denyData := copyActionData(baseData)
	denyData["decision"] = "deny"

	document := map[string]interface{}{
		"type":     "AdaptiveCard",
		"version":  cardmsg.CardVersion,
		"metadata": buildMetadata(deepLink, content.Variant, content.Source),
		"body":     body,
		"actions": []interface{}{
			map[string]interface{}{"type": "Action.OpenUrl", "title": labels.viewDetails, "url": deepLink},
			map[string]interface{}{
				"type": "Action.Submit", "id": DocsDenyActionID, "title": actions.DenyTitle,
				"style": "destructive", "data": denyData,
			},
			map[string]interface{}{
				"type": "Action.Submit", "id": DocsApproveActionID, "title": actions.ApproveTitle,
				"style": "positive", "data": approveData,
			},
		},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("cardtmpl: marshal docs access request card: %w", err)
	}
	return raw, nil
}

// BuildDocsApprovalOutcomeCard renders the terminal (已允许 / 已拒绝) docs card:
// header (label + colored status) + title + a result box. The denied box surfaces
// the reviewer's reason. No decision actions remain; view-details stays.
func BuildDocsApprovalOutcomeCard(
	ctx context.Context,
	webLoginURL string,
	docID string,
	spaceID string,
	content DocsOutcomeContent,
) (json.RawMessage, error) {
	if strings.TrimSpace(content.Title) == "" || utf8.RuneCountInString(content.Title) > maxTitleRunes {
		return nil, errors.New("cardtmpl: resource title is invalid")
	}
	if content.Source.IconURL != "" {
		if err := requireHTTPS(content.Source.IconURL); err != nil {
			return nil, fmt.Errorf("cardtmpl: source icon URL: %w", err)
		}
	}
	deepLink, err := docsDeepLink(webLoginURL, docID, spaceID)
	if err != nil {
		return nil, err
	}
	lang := i18n.OutboundLanguage(ctx)
	labels := labelsForLanguage(lang)

	statusColor := "Good"
	boxStyle := "good"
	if content.Denied {
		statusColor, boxStyle = "Attention", "attention"
	}
	body := []interface{}{
		docsApprovalHeader(content.HeaderLabel, content.StatusLabel, statusColor),
		docsApprovalTitle(content.Title),
		docsResultBox(boxStyle, statusColor, content.ResultText, content.ReasonLabel, content.Reason),
		// view-details rides as a body ActionSet (not a root action) so the
		// terminal card carries no decision/submit actions — the approve/deny
		// buttons are gone once the request is resolved.
		map[string]interface{}{
			"type": "ActionSet",
			"actions": []interface{}{
				map[string]interface{}{"type": "Action.OpenUrl", "title": labels.viewDetails, "url": deepLink},
			},
		},
	}
	document := map[string]interface{}{
		"type":     "AdaptiveCard",
		"version":  cardmsg.CardVersion,
		"metadata": buildMetadata(deepLink, content.Variant, content.Source),
		"body":     body,
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("cardtmpl: marshal docs outcome card: %w", err)
	}
	return raw, nil
}

// docsApprovalHeader is the shared top row: a bold source label on the left and a
// small colored status label on the right (color drives pending/approved/denied).
func docsApprovalHeader(headerLabel, statusLabel, statusColor string) map[string]interface{} {
	columns := []interface{}{
		map[string]interface{}{
			"type": "Column", "width": "stretch", "verticalContentAlignment": "Center",
			"items": []interface{}{
				map[string]interface{}{
					"type": "TextBlock", "text": escapeMarkdown(headerLabel),
					"weight": "Bolder", "size": "Small", "wrap": false, "spacing": "None",
				},
			},
		},
	}
	if strings.TrimSpace(statusLabel) != "" {
		columns = append(columns, map[string]interface{}{
			"type": "Column", "width": "auto", "verticalContentAlignment": "Center",
			"items": []interface{}{
				map[string]interface{}{
					"type": "TextBlock", "text": escapeMarkdown(statusLabel),
					"weight": "Bolder", "size": "Small", "color": statusColor,
					"horizontalAlignment": "Right", "wrap": false, "spacing": "None",
				},
			},
		})
	}
	return map[string]interface{}{"type": "ColumnSet", "spacing": "None", "columns": columns}
}

func docsApprovalTitle(title string) map[string]interface{} {
	return map[string]interface{}{
		"type": "TextBlock", "text": escapeMarkdown(title),
		"size": "ExtraLarge", "weight": "Bolder", "wrap": true,
		"separator": true, "spacing": "Medium",
	}
}

// docsBanner renders "<bold actor> <subtle suffix>". With no actor it degrades to
// the subtle suffix alone (producer passes the subject-less sentence).
func docsBanner(actor, suffix string) map[string]interface{} {
	actor = truncateRunes(strings.TrimSpace(actor), maxActorRunes)
	suffix = truncateRunes(strings.TrimSpace(suffix), maxTitleRunes)
	if actor == "" {
		return map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(suffix),
			"size": "Medium", "isSubtle": true, "wrap": true, "spacing": "Default",
		}
	}
	// TextRun renders its text as literal plain text (no markdown surface — see
	// pkg/cardmsg/validate.go "TextRun 不渲染 markdown" and plain.go), so it must NOT
	// be escapeMarkdown'd: escaping here would show literal backslashes for actors
	// with markdown metachars (e.g. "Wang (FE)" → "Wang \(FE\)"). Values are still
	// truncateRunes-bounded above, and TextRun carries no link/URL surface, so raw
	// text cannot inject markup. (The anonymous branch above uses a TextBlock, which
	// DOES render markdown, so its escapeMarkdown is correct and stays.)
	return map[string]interface{}{
		"type": "RichTextBlock", "spacing": "Default",
		"inlines": []interface{}{
			map[string]interface{}{"type": "TextRun", "text": actor + " ", "size": "Medium", "weight": "Bolder"},
			map[string]interface{}{"type": "TextRun", "text": suffix, "size": "Medium", "isSubtle": true},
		},
	}
}

// docsRequesterRow builds [avatar?] [name / role] [timestamp]. Returns nil for an
// anonymous request (no actor): the banner already conveys it, and a lone
// role/timestamp with no name reads as broken.
func docsRequesterRow(actor, avatar, roleLabel, timestamp string) map[string]interface{} {
	actor = truncateRunes(strings.TrimSpace(actor), maxActorRunes)
	timestamp = truncateRunes(strings.TrimSpace(timestamp), maxTimestampRunes)
	if actor == "" {
		return nil
	}
	var columns []interface{}
	if avatar != "" {
		columns = append(columns, map[string]interface{}{
			"type": "Column", "width": "auto", "verticalContentAlignment": "Center",
			"items": []interface{}{
				map[string]interface{}{
					"type": "Image", "url": avatar, "style": "Person",
					"width": "36px", "height": "36px",
				},
			},
		})
	}
	nameItems := []interface{}{
		map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(actor),
			"weight": "Bolder", "wrap": false, "spacing": "None",
		},
	}
	if strings.TrimSpace(roleLabel) != "" {
		nameItems = append(nameItems, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(roleLabel),
			"isSubtle": true, "size": "Small", "wrap": false, "spacing": "None",
		})
	}
	columns = append(columns, map[string]interface{}{
		"type": "Column", "width": "stretch", "verticalContentAlignment": "Center",
		"items": nameItems,
	})
	if timestamp != "" {
		columns = append(columns, map[string]interface{}{
			"type": "Column", "width": "auto", "verticalContentAlignment": "Center",
			"items": []interface{}{
				map[string]interface{}{
					"type": "TextBlock", "text": escapeMarkdown(timestamp),
					"isSubtle": true, "size": "Small", "horizontalAlignment": "Right", "wrap": false,
				},
			},
		})
	}
	return map[string]interface{}{"type": "ColumnSet", "spacing": "Medium", "columns": columns}
}

// docsReasonBox is the emphasis-styled reason section. Returns nil when empty.
func docsReasonBox(label, reason string) map[string]interface{} {
	reason = truncateRunes(strings.TrimSpace(reason), maxReasonRunes)
	if reason == "" {
		return nil
	}
	items := []interface{}{}
	if strings.TrimSpace(label) != "" {
		items = append(items, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(label),
			"size": "Small", "weight": "Bolder", "isSubtle": true, "spacing": "None",
		})
	}
	items = append(items, map[string]interface{}{
		"type": "TextBlock", "text": escapeMarkdown(reason), "size": "Medium", "wrap": true, "spacing": "Small",
	})
	return map[string]interface{}{"type": "Container", "style": "emphasis", "spacing": "Medium", "items": items}
}

// docsResultBox is the terminal good/attention result section. For denials it
// appends the reviewer reason as a labeled sub-line.
func docsResultBox(boxStyle, textColor, resultText, reasonLabel, reason string) map[string]interface{} {
	items := []interface{}{
		map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(strings.TrimSpace(resultText)),
			"size": "Medium", "weight": "Bolder", "color": textColor, "wrap": true, "spacing": "None",
		},
	}
	if r := truncateRunes(strings.TrimSpace(reason), maxReasonRunes); r != "" {
		label := strings.TrimSpace(reasonLabel)
		if label != "" {
			items = append(items, map[string]interface{}{
				"type": "TextBlock", "text": escapeMarkdown(label),
				"size": "Small", "weight": "Bolder", "isSubtle": true, "spacing": "Small",
			})
		}
		items = append(items, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(r), "size": "Medium", "wrap": true, "spacing": "None",
		})
	}
	return map[string]interface{}{"type": "Container", "style": boxStyle, "spacing": "Medium", "items": items}
}

func copyActionData(source map[string]interface{}) map[string]interface{} {
	dst := make(map[string]interface{}, len(source)+1)
	for key, value := range source {
		dst[key] = value
	}
	return dst
}
