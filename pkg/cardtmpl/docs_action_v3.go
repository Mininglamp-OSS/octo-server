package cardtmpl

import (
	"fmt"
	"strings"
)

const (
	docsV3DocumentIcon = "https://api.iconify.design/lucide/file-text.svg?color=%236b7075"
	docsV3ExternalIcon = "https://api.iconify.design/lucide/external-link.svg?color=%236b7075"
	docsV3BotIcon      = "https://api.iconify.design/lucide/bot.svg?color=%237f3bf5"
	docsV3ClockIcon    = "https://api.iconify.design/lucide/clock-3.svg?color=%236b7075"
	docsV3MessageIcon  = "https://api.iconify.design/lucide/message-square.svg?color=%23555b61"
	docsV3ViewIcon     = "https://api.iconify.design/lucide/eye.svg?color=%23555b61"
	docsV3ApproveIcon  = "https://api.iconify.design/lucide/check.svg?color=%23ffffff"
	docsV3DenyIcon     = "https://api.iconify.design/lucide/x.svg?color=%23f54a45"
)

// DocsAccessRequestV3Content extends the existing production content contract
// with the display fields needed by the Forge 0.3.0 header and footer. The
// embedded DocsApprovalContent remains the authority for validation, escaping,
// and server-authored action data.
type DocsAccessRequestV3Content struct {
	DocsApprovalContent
	SourceName          string
	PermissionLabel     string
	PermissionRoleLabel string
	MessageTimeDisplay  string
}

type docsV3Labels struct {
	document     string
	openDocument string
	externalAlt  string
	requestedAt  string
}

func docsV3LabelsForLanguage(lang string) docsV3Labels {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return docsV3Labels{
			document: "文档", openDocument: "打开文档", externalAlt: "在新窗口打开", requestedAt: "申请于",
		}
	}
	return docsV3Labels{
		document: "Docs", openDocument: "Open document", externalAlt: "Open in a new window", requestedAt: "Requested at",
	}
}

// BuildDocsAccessRequestV3BodyWithLang renders the Forge-aligned pending view.
// It deliberately reuses BuildDocsAccessRequestBodyWithLang first: all existing
// validation, deep-link construction, and production Submit data remain the
// single authority. Only the visual arrangement changes for 0.3.0.
func BuildDocsAccessRequestV3BodyWithLang(
	lang string,
	webLoginURL string,
	docID string,
	requestID string,
	content DocsAccessRequestV3Content,
	actions ApprovalActions,
) ([]interface{}, string, error) {
	_, cardActions, deepLink, err := BuildDocsAccessRequestBodyWithLang(
		lang, webLoginURL, docID, requestID, content.DocsApprovalContent, actions,
	)
	if err != nil {
		return nil, "", err
	}

	for _, action := range cardActions {
		am, _ := action.(map[string]interface{})
		if am == nil {
			continue
		}
		switch am["type"] {
		case "Action.OpenUrl":
			am["id"] = "view_document"
			am["iconUrl"] = docsV3ViewIcon
		case "Action.Submit":
			data, _ := am["data"].(map[string]interface{})
			if data != nil {
				data["actor_avatar_url"] = content.ActorAvatar
				data["request_reason"] = content.Reason
				data["requested_at_display"] = content.Timestamp
				data["message_time_display"] = content.MessageTimeDisplay
				data["permission_label"] = content.PermissionLabel
				data["permission_role_label"] = content.PermissionRoleLabel
				data["source_name"] = content.SourceName
				data["requester_space_name"] = content.RequesterSpaceName
				data["requested_bot_names"] = append([]string(nil), content.RequestedBotNames...)
			}
			if am["id"] == DocsDenyActionID {
				am["iconUrl"] = docsV3DenyIcon
			} else if am["id"] == DocsApproveActionID {
				am["iconUrl"] = docsV3ApproveIcon
			}
		}
	}

	labels := docsV3LabelsForLanguage(lang)
	items := []interface{}{docsV3Title(content.Title, deepLink, labels)}
	if summary := docsV3RequestSummary(lang, content.Actor, content.RequesterSpaceName, content.PermissionRoleLabel); summary != nil {
		items = append(items, summary)
	}
	if bots := docsV3BotBox(lang, content.RequestedBotNames); bots != nil {
		items = append(items, bots)
	}
	if reason := docsV3ReasonBox(content.ReasonLabel, content.Reason); reason != nil {
		items = append(items, reason)
	}
	// The web host dialog remains the production deny-reason UI. The hidden
	// input declares the only accepted input id so ValidateInputs stays closed
	// to client-invented keys.
	items = append(items, map[string]interface{}{
		"type": "Input.Text", "id": DocsDenyReasonInputID, "isVisible": false,
		"isMultiline": true, "maxLength": maxReasonRunes,
	})

	body := []interface{}{
		docsV3Header(content.HeaderLabel, content.SourceName, content.StatusLabel,
			content.PermissionLabel, "Warning", "octo-badge-warning-request-state", labels.document),
		docsV3Surface(items),
		docsV3Footer(labels.requestedAt, content.MessageTimeDisplay, "approval_actions", cardActions),
	}
	return body, deepLink, nil
}

// BuildDocsApprovalOutcomeV3BodyWithLang renders the Forge-aligned terminal
// view while preserving the existing result content and server deep link.
func BuildDocsApprovalOutcomeV3BodyWithLang(
	lang string,
	webLoginURL string,
	docID string,
	content DocsOutcomeContent,
) ([]interface{}, string, error) {
	_, deepLink, err := BuildDocsApprovalOutcomeBodyWithLang(lang, webLoginURL, docID, content)
	if err != nil {
		return nil, "", err
	}
	statusColor := "Good"
	if content.Denied {
		statusColor = "Attention"
	}
	labels := docsV3LabelsForLanguage(lang)
	items := []interface{}{docsV3Title(content.Title, deepLink, labels)}
	if summary := docsV3RequestSummary(lang, content.Actor, content.RequesterSpaceName, content.PermissionRoleLabel); summary != nil {
		items = append(items, summary)
	}
	if bots := docsV3BotBox(lang, content.RequestedBotNames); bots != nil {
		items = append(items, bots)
	}
	if reason := docsV3ReasonBox(content.RequestReasonLabel, content.RequestReason); reason != nil {
		items = append(items, reason)
	}
	// Terminal status already lives in the top-right badge. Do not repeat it in
	// a large green/red result block. A denial reason remains visible as a compact
	// neutral detail block.
	if content.Denied {
		if reason := docsV3ReasonBox(content.ReasonLabel, content.Reason); reason != nil {
			items = append(items, reason)
		}
	}

	viewAction := map[string]interface{}{
		"type": "Action.OpenUrl", "id": "view_document", "title": labelsForLanguage(lang).viewDetails,
		"url": deepLink, "iconUrl": docsV3ViewIcon,
	}
	body := []interface{}{
		docsV3Header(content.HeaderLabel, content.SourceName, content.StatusLabel,
			content.PermissionLabel, statusColor, "octo-badge-result-request-state", labels.document),
		docsV3Surface(items),
		docsV3TerminalFooter(lang, content.RequestedAtDisplay, content.MessageTimeDisplay,
			content.DecisionOperatorName, content.DecisionOperatorSpaceName, viewAction),
	}
	return body, deepLink, nil
}

func docsV3Header(headerLabel, sourceName, statusLabel, permissionLabel, statusColor, statusID, documentAlt string) map[string]interface{} {
	leftColumns := []interface{}{
		map[string]interface{}{
			"type": "Column", "width": "auto",
			"items": []interface{}{map[string]interface{}{
				"type": "Image", "url": docsV3DocumentIcon, "altText": documentAlt,
				"width": "16px", "height": "16px", "spacing": "None",
			}},
		},
		map[string]interface{}{
			"type": "Column", "width": "auto", "spacing": "Default",
			"items": []interface{}{map[string]interface{}{
				"type": "TextBlock", "text": escapeMarkdown(headerLabel), "size": "Small",
				"weight": "Bolder", "spacing": "None", "wrap": false,
			}},
		},
	}
	if source := truncateRunes(strings.TrimSpace(sourceName), maxTitleRunes); source != "" {
		leftColumns = append(leftColumns, map[string]interface{}{
			"type": "Column", "width": "stretch", "spacing": "Small",
			"items": []interface{}{map[string]interface{}{
				"type": "TextBlock", "text": escapeMarkdown("·  " + source), "size": "Small",
				"isSubtle": true, "spacing": "None", "wrap": false,
			}},
		})
	}
	rightColumns := []interface{}{}
	if strings.TrimSpace(statusLabel) != "" {
		rightColumns = append(rightColumns, map[string]interface{}{
			"type": "Column", "width": "auto",
			"items": []interface{}{map[string]interface{}{
				"type": "TextBlock", "id": statusID, "text": escapeMarkdown(statusLabel),
				"size": "Small", "weight": "Bolder", "color": statusColor, "spacing": "None", "wrap": false,
			}},
		})
	}
	if permission := truncateRunes(strings.TrimSpace(permissionLabel), maxTimestampRunes); permission != "" {
		rightColumns = append(rightColumns, map[string]interface{}{
			"type": "Column", "width": "auto", "spacing": "Default",
			"items": []interface{}{map[string]interface{}{
				"type": "TextBlock", "id": "octo-badge-neutral-permission", "text": escapeMarkdown(permission),
				"size": "Small", "isSubtle": true, "spacing": "None", "wrap": false,
			}},
		})
	}
	return map[string]interface{}{
		"type": "ColumnSet", "spacing": "None", "minHeight": "28px", "verticalContentAlignment": "Center",
		"columns": []interface{}{
			map[string]interface{}{
				"type": "Column", "width": "stretch", "verticalContentAlignment": "Center",
				"items": []interface{}{map[string]interface{}{
					"type": "ColumnSet", "spacing": "None", "verticalContentAlignment": "Center", "columns": leftColumns,
				}},
			},
			map[string]interface{}{
				"type": "Column", "width": "auto",
				"items": []interface{}{map[string]interface{}{
					"type": "ColumnSet", "spacing": "None", "columns": rightColumns,
				}},
			},
		},
	}
}

func docsV3Surface(items []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "Container", "style": "accent", "bleed": true,
		"separator": true, "spacing": "None", "items": items,
	}
}

func docsV3Title(title, deepLink string, labels docsV3Labels) map[string]interface{} {
	return map[string]interface{}{
		"type": "ColumnSet", "spacing": "None", "verticalContentAlignment": "Center",
		"selectAction": map[string]interface{}{
			"type": "Action.OpenUrl", "title": labels.openDocument, "url": deepLink,
		},
		"columns": []interface{}{
			map[string]interface{}{
				"type": "Column", "width": "auto",
				"items": []interface{}{map[string]interface{}{
					"type": "TextBlock", "text": escapeMarkdown(title), "size": "ExtraLarge",
					"weight": "Bolder", "spacing": "None", "wrap": true,
				}},
			},
			map[string]interface{}{
				"type": "Column", "width": "auto", "verticalContentAlignment": "Center", "spacing": "Small",
				"items": []interface{}{map[string]interface{}{
					"type": "Image", "url": docsV3ExternalIcon, "altText": labels.externalAlt,
					"width": "13px", "height": "13px", "spacing": "None",
				}},
			},
			map[string]interface{}{"type": "Column", "width": "stretch", "items": []interface{}{}},
		},
	}
}

func docsV3RequestSummary(lang, actor, sourceSpaceName, roleLabel string) map[string]interface{} {
	actor = truncateRunes(strings.TrimSpace(actor), maxActorRunes)
	spaceName := truncateRunes(strings.TrimSpace(sourceSpaceName), maxTitleRunes)
	roleLabel = truncateRunes(strings.TrimSpace(roleLabel), maxTimestampRunes)
	if actor == "" && spaceName == "" && roleLabel == "" {
		return nil
	}
	applicantLabel, permissionLabel := "Applicant: ", "Permission: "
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		applicantLabel, permissionLabel = "申请者：", "申请权限："
	}
	inlines := []interface{}{}
	appendPart := func(label, value string) {
		if value == "" {
			return
		}
		if len(inlines) > 0 {
			inlines = append(inlines, map[string]interface{}{
				"type": "TextRun", "text": "  ·  ", "isSubtle": true, "size": "Small",
			})
		}
		if label != "" {
			inlines = append(inlines, map[string]interface{}{
				"type": "TextRun", "text": label, "isSubtle": true, "size": "Small",
			})
		}
		inlines = append(inlines, map[string]interface{}{
			"type": "TextRun", "text": value, "weight": "Bolder", "size": "Small",
		})
	}
	appendPart(applicantLabel, actor)
	appendPart("", spaceName)
	appendPart(permissionLabel, roleLabel)
	return map[string]interface{}{
		"type": "RichTextBlock", "spacing": "Medium", "inlines": inlines,
	}
}

const requestedBotNamesDisplayBudget = 56

func requestedBotNamesForOneLine(names []string, zh bool) []string {
	if len(names) == 0 {
		return nil
	}
	separator := ", "
	if zh {
		separator = "、"
	}
	visible := make([]string, 0, len(names))
	for _, name := range names {
		candidate := append(append([]string(nil), visible...), name)
		hidden := len(names) - len(candidate)
		text := strings.Join(candidate, separator)
		if hidden > 0 {
			if zh {
				text += fmt.Sprintf("，等 %d 个", hidden)
			} else {
				text += fmt.Sprintf(" and %d more", hidden)
			}
		}
		if displayWidth(text) > requestedBotNamesDisplayBudget {
			break
		}
		visible = candidate
	}
	if len(visible) == 0 {
		hidden := len(names) - 1
		suffix := ""
		if hidden > 0 {
			if zh {
				suffix = fmt.Sprintf("，等 %d 个", hidden)
			} else {
				suffix = fmt.Sprintf(" and %d more", hidden)
			}
		}
		visible = append(visible, truncateDisplayWidth(names[0], requestedBotNamesDisplayBudget-displayWidth(suffix)))
	}
	return visible
}

func truncateDisplayWidth(value string, maxWidth int) string {
	if maxWidth <= 0 || displayWidth(value) <= maxWidth {
		return value
	}
	const ellipsis = "…"
	target := maxWidth - displayWidth(ellipsis)
	if target <= 0 {
		return ellipsis
	}
	width := 0
	runes := make([]rune, 0, len(value))
	for _, r := range value {
		runeWidth := 1
		if r > 0x7f {
			runeWidth = 2
		}
		if width+runeWidth > target {
			break
		}
		runes = append(runes, r)
		width += runeWidth
	}
	return string(runes) + ellipsis
}

func displayWidth(value string) int {
	width := 0
	for _, r := range value {
		if r > 0x7f {
			width += 2
		} else {
			width++
		}
	}
	return width
}

func docsV3BotBox(lang string, names []string) map[string]interface{} {
	clean := make([]string, 0, len(names))
	for _, name := range names {
		if value := truncateRunes(strings.TrimSpace(name), maxActorRunes); value != "" {
			clean = append(clean, value)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	total := len(clean)
	zh := strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh")
	visible := requestedBotNamesForOneLine(clean, zh)
	label := fmt.Sprintf("AI assistants requesting access (%d)", total)
	namesText := strings.Join(visible, ", ")
	if hidden := total - len(visible); hidden > 0 {
		namesText += fmt.Sprintf(" and %d more", hidden)
	}
	if zh {
		label = fmt.Sprintf("同时申请权限的 AI 助手（%d）", total)
		namesText = strings.Join(visible, "、")
		if hidden := total - len(visible); hidden > 0 {
			namesText += fmt.Sprintf("，等 %d 个", hidden)
		}
	}
	return map[string]interface{}{
		"type": "Container", "style": "emphasis", "spacing": "Large",
		"items": []interface{}{map[string]interface{}{
			"type": "ColumnSet", "spacing": "None", "columns": []interface{}{
				map[string]interface{}{
					"type": "Column", "width": "auto",
					"items": []interface{}{map[string]interface{}{
						"type": "Image", "url": docsV3BotIcon, "altText": "AI",
						"width": "18px", "height": "18px", "spacing": "None",
					}},
				},
				map[string]interface{}{
					"type": "Column", "width": "stretch", "spacing": "Default",
					"items": []interface{}{
						map[string]interface{}{
							"type": "TextBlock", "text": escapeMarkdown(label), "size": "Small",
							"weight": "Bolder", "spacing": "None",
						},
						map[string]interface{}{
							"type": "TextBlock", "text": escapeMarkdown(namesText),
							"size": "Small", "isSubtle": true, "spacing": "Small", "wrap": true, "maxLines": 2,
						},
					},
				},
			},
		}},
	}
}

func docsV3ReasonBox(label, reason string) map[string]interface{} {
	reason = truncateRunes(strings.TrimSpace(reason), maxReasonRunes)
	if reason == "" {
		return nil
	}
	return map[string]interface{}{
		"type": "Container", "style": "emphasis", "spacing": "Large",
		"items": []interface{}{map[string]interface{}{
			"type": "ColumnSet", "spacing": "None", "columns": []interface{}{
				map[string]interface{}{
					"type": "Column", "width": "auto",
					"items": []interface{}{map[string]interface{}{
						"type": "Image", "url": docsV3MessageIcon, "altText": "",
						"width": "18px", "height": "18px", "spacing": "None",
					}},
				},
				map[string]interface{}{
					"type": "Column", "width": "stretch", "spacing": "Default",
					"items": []interface{}{
						map[string]interface{}{
							"type": "TextBlock", "text": escapeMarkdown(label), "size": "Small",
							"weight": "Bolder", "spacing": "None",
						},
						map[string]interface{}{
							"type": "TextBlock", "text": escapeMarkdown(reason), "size": "Small",
							"isSubtle": true, "spacing": "Small", "wrap": true,
						},
					},
				},
			},
		}},
	}
}

func docsV3TerminalFooter(lang, requestedAt, decidedAt, operatorName, operatorSpaceName string, viewAction map[string]interface{}) map[string]interface{} {
	requestedAt = truncateRunes(strings.TrimSpace(requestedAt), maxTimestampRunes)
	decidedAt = truncateRunes(strings.TrimSpace(decidedAt), maxTimestampRunes)
	operatorName = truncateRunes(strings.TrimSpace(operatorName), maxActorRunes)
	operatorSpaceName = truncateRunes(strings.TrimSpace(operatorSpaceName), maxTitleRunes)
	requestedLabel, decidedLabel := "Requested at", "Processed at"
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		requestedLabel, decidedLabel = "申请于", "处理于"
	}
	leftItems := []interface{}{}
	if requestedAt != "" {
		leftItems = append(leftItems, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(requestedLabel + " " + requestedAt),
			"size": "Small", "isSubtle": true, "spacing": "None", "wrap": false,
		})
	}
	if decidedAt != "" {
		leftItems = append(leftItems, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(decidedLabel + " " + decidedAt),
			"size": "Small", "isSubtle": true, "spacing": "Small", "wrap": false,
		})
	}
	operatorText := strings.Trim(strings.TrimSpace(operatorName)+" · "+strings.TrimSpace(operatorSpaceName), " ·")
	if operatorText != "" {
		leftItems = append(leftItems, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(operatorText),
			"size": "Small", "weight": "Bolder", "spacing": "Small", "wrap": false,
		})
	}
	rightItems := []interface{}{}
	rightItems = append(rightItems, map[string]interface{}{
		"type": "ActionSet", "spacing": "Small", "actions": []interface{}{viewAction},
	})
	return map[string]interface{}{
		"type": "Container", "style": "emphasis", "bleed": true,
		"separator": true, "spacing": "None",
		"items": []interface{}{map[string]interface{}{
			"type": "ColumnSet", "spacing": "None", "verticalContentAlignment": "Center",
			"columns": []interface{}{
				map[string]interface{}{
					"type": "Column", "width": "stretch", "verticalContentAlignment": "Center", "items": leftItems,
				},
				map[string]interface{}{
					"type": "Column", "width": "auto", "verticalContentAlignment": "Center", "items": rightItems,
				},
			},
		}},
	}
}

func docsV3Footer(timeLabel, timeDisplay, actionSetID string, actions []interface{}) map[string]interface{} {
	timeDisplay = truncateRunes(strings.TrimSpace(timeDisplay), maxTimestampRunes)
	timeText := strings.Trim(strings.TrimSpace(timeLabel)+" "+timeDisplay, " ")
	actionSet := map[string]interface{}{
		"type": "ActionSet", "spacing": "None", "actions": actions,
	}
	if actionSetID != "" {
		actionSet["id"] = actionSetID
	}
	columns := []interface{}{}
	if timeText != "" {
		columns = append(columns, map[string]interface{}{
			"type": "Column", "width": "stretch", "verticalContentAlignment": "Center",
			"items": []interface{}{map[string]interface{}{
				"type": "TextBlock", "text": escapeMarkdown(timeText), "size": "Small",
				"isSubtle": true, "spacing": "None", "wrap": false,
			}},
		})
	} else {
		columns = append(columns, map[string]interface{}{"type": "Column", "width": "stretch", "items": []interface{}{}})
	}
	columns = append(columns, map[string]interface{}{
		"type": "Column", "width": "auto", "verticalContentAlignment": "Center",
		"items": []interface{}{actionSet},
	})
	return map[string]interface{}{
		"type": "Container", "style": "emphasis", "bleed": true,
		"separator": true, "spacing": "None",
		"items": []interface{}{map[string]interface{}{
			"type": "ColumnSet", "spacing": "None", "verticalContentAlignment": "Center", "columns": columns,
		}},
	}
}
