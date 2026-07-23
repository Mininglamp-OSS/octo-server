package cardtmpl

import "strings"

const (
	docsV3DocumentIcon      = "https://api.iconify.design/lucide/file-text.svg?color=%236b7075"
	docsV3ExternalIcon      = "https://api.iconify.design/lucide/external-link.svg?color=%236b7075"
	docsV3RequesterIcon     = "https://api.iconify.design/lucide/user-round.svg?color=%236b7075"
	docsV3ClockIcon         = "https://api.iconify.design/lucide/clock-3.svg?color=%236b7075"
	docsV3MessageIcon       = "https://api.iconify.design/lucide/message-square.svg?color=%23555b61"
	docsV3ViewIcon          = "https://api.iconify.design/lucide/eye.svg?color=%23555b61"
	docsV3ApproveIcon       = "https://api.iconify.design/lucide/check.svg?color=%23ffffff"
	docsV3ApprovedStateIcon = "https://api.iconify.design/lucide/check.svg?color=%2341cd59"
	docsV3DenyIcon          = "https://api.iconify.design/lucide/x.svg?color=%23f54a45"
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
	spaceID string,
	content DocsAccessRequestV3Content,
	actions ApprovalActions,
) ([]interface{}, string, error) {
	_, cardActions, deepLink, err := BuildDocsAccessRequestBodyWithLang(
		lang, webLoginURL, docID, requestID, spaceID, content.DocsApprovalContent, actions,
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
			}
			if am["id"] == DocsDenyActionID {
				am["iconUrl"] = docsV3DenyIcon
			} else if am["id"] == DocsApproveActionID {
				am["iconUrl"] = docsV3ApproveIcon
			}
		}
	}

	labels := docsV3LabelsForLanguage(lang)
	items := []interface{}{
		docsV3Title(content.Title, deepLink, labels),
		docsV3Banner(content.Actor, content.BannerSuffix),
	}
	if row := docsV3RequesterRow(content.Actor, content.ActorAvatar, content.RoleLabel, content.Timestamp); row != nil {
		items = append(items, row)
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
	spaceID string,
	content DocsOutcomeContent,
) ([]interface{}, string, error) {
	_, deepLink, err := BuildDocsApprovalOutcomeBodyWithLang(lang, webLoginURL, docID, spaceID, content)
	if err != nil {
		return nil, "", err
	}
	statusColor, resultStyle, resultIcon := "Good", "good", docsV3ApprovedStateIcon
	if content.Denied {
		statusColor, resultStyle, resultIcon = "Attention", "attention", docsV3DenyIcon
	}
	labels := docsV3LabelsForLanguage(lang)
	items := []interface{}{docsV3Title(content.Title, deepLink, labels)}
	if strings.TrimSpace(content.Actor) != "" || strings.TrimSpace(content.BannerSuffix) != "" {
		items = append(items, docsV3Banner(content.Actor, content.BannerSuffix))
	}
	if row := docsV3RequesterRow(content.Actor, content.ActorAvatar, content.RoleLabel, content.RequestedAtDisplay); row != nil {
		items = append(items, row)
	}
	if reason := docsV3ReasonBox(content.RequestReasonLabel, content.RequestReason); reason != nil {
		items = append(items, reason)
	}
	items = append(items, docsV3ResultBox(resultStyle, resultIcon, content.StatusLabel,
		content.ResultText, content.DecisionSummary, content.ReasonLabel, content.Reason))

	viewAction := map[string]interface{}{
		"type": "Action.OpenUrl", "id": "view_document", "title": labelsForLanguage(lang).viewDetails,
		"url": deepLink, "iconUrl": docsV3ViewIcon,
	}
	body := []interface{}{
		docsV3Header(content.HeaderLabel, content.SourceName, content.StatusLabel,
			content.PermissionLabel, statusColor, "octo-badge-result-request-state", labels.document),
		docsV3Surface(items),
		docsV3Footer(content.MessageTimeLabel, content.MessageTimeDisplay, "", []interface{}{viewAction}),
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

func docsV3Banner(actor, suffix string) map[string]interface{} {
	actor = truncateRunes(strings.TrimSpace(actor), maxActorRunes)
	suffix = truncateRunes(strings.TrimSpace(suffix), maxTitleRunes)
	if actor == "" {
		return map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(suffix), "size": "Medium",
			"isSubtle": true, "wrap": true, "spacing": "Small",
		}
	}
	return map[string]interface{}{
		"type": "RichTextBlock", "spacing": "Small",
		"inlines": []interface{}{
			map[string]interface{}{"type": "TextRun", "text": actor + " ", "weight": "Bolder", "size": "Medium"},
			map[string]interface{}{"type": "TextRun", "text": suffix, "isSubtle": true, "size": "Medium"},
		},
	}
}

func docsV3RequesterRow(actor, avatar, roleLabel, timestamp string) map[string]interface{} {
	actor = truncateRunes(strings.TrimSpace(actor), maxActorRunes)
	timestamp = truncateRunes(strings.TrimSpace(timestamp), maxTimestampRunes)
	if actor == "" {
		return nil
	}
	columns := []interface{}{}
	if avatar != "" {
		columns = append(columns, map[string]interface{}{
			"type": "Column", "width": "auto",
			"items": []interface{}{map[string]interface{}{
				"type": "Image", "url": avatar, "altText": actor, "style": "Person",
				"width": "28px", "height": "28px", "spacing": "None",
			}},
		})
	}
	nameItems := []interface{}{map[string]interface{}{
		"type": "TextBlock", "text": escapeMarkdown(actor), "weight": "Bolder",
		"size": "Small", "spacing": "None", "wrap": false,
	}}
	if role := strings.TrimSpace(roleLabel); role != "" {
		nameItems = append(nameItems, map[string]interface{}{
			"type": "ColumnSet", "spacing": "None", "columns": []interface{}{
				map[string]interface{}{
					"type": "Column", "width": "auto",
					"items": []interface{}{map[string]interface{}{
						"type": "Image", "url": docsV3RequesterIcon, "altText": "",
						"width": "12px", "height": "12px", "spacing": "None",
					}},
				},
				map[string]interface{}{
					"type": "Column", "width": "auto", "spacing": "Small",
					"items": []interface{}{map[string]interface{}{
						"type": "TextBlock", "text": escapeMarkdown(role), "size": "Small",
						"isSubtle": true, "spacing": "None", "wrap": false,
					}},
				},
			},
		})
	}
	columns = append(columns, map[string]interface{}{
		"type": "Column", "width": "stretch", "spacing": "Default", "items": nameItems,
	})
	if timestamp != "" {
		columns = append(columns, map[string]interface{}{
			"type": "Column", "width": "auto", "verticalContentAlignment": "Bottom",
			"items": []interface{}{map[string]interface{}{
				"type": "ColumnSet", "spacing": "None", "columns": []interface{}{
					map[string]interface{}{
						"type": "Column", "width": "auto",
						"items": []interface{}{map[string]interface{}{
							"type": "Image", "url": docsV3ClockIcon, "altText": "",
							"width": "13px", "height": "13px", "spacing": "None",
						}},
					},
					map[string]interface{}{
						"type": "Column", "width": "auto", "spacing": "Small",
						"items": []interface{}{map[string]interface{}{
							"type": "TextBlock", "text": escapeMarkdown(timestamp), "size": "Small",
							"isSubtle": true, "spacing": "None", "wrap": false,
						}},
					},
				},
			}},
		})
	}
	return map[string]interface{}{
		"type": "ColumnSet", "spacing": "Medium", "verticalContentAlignment": "Center", "columns": columns,
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

func docsV3ResultBox(style, icon, statusLabel, resultText, decisionSummary, reasonLabel, reason string) map[string]interface{} {
	items := []interface{}{
		map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(strings.TrimSpace(resultText)),
			"size": "Small", "weight": "Bolder", "spacing": "None", "wrap": true,
		},
	}
	if summary := truncateRunes(strings.TrimSpace(decisionSummary), maxTitleRunes); summary != "" {
		items = append(items, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(summary),
			"size": "Small", "isSubtle": true, "spacing": "Small",
		})
	}
	if value := truncateRunes(strings.TrimSpace(reason), maxReasonRunes); value != "" {
		text := value
		if label := strings.TrimSpace(reasonLabel); label != "" {
			text = label + "：" + value
		}
		items = append(items, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(text),
			"size": "Small", "isSubtle": true, "spacing": "Small", "wrap": true,
		})
	}
	return map[string]interface{}{
		"type": "Container", "style": style, "spacing": "Large",
		"items": []interface{}{map[string]interface{}{
			"type": "ColumnSet", "spacing": "None", "columns": []interface{}{
				map[string]interface{}{
					"type": "Column", "width": "auto",
					"items": []interface{}{map[string]interface{}{
						"type": "Container", "style": "default", "spacing": "None",
						"items": []interface{}{map[string]interface{}{
							"type": "Image", "url": icon, "altText": statusLabel,
							"width": "16px", "height": "16px", "spacing": "None",
						}},
					}},
				},
				map[string]interface{}{
					"type": "Column", "width": "stretch", "spacing": "Default", "items": items,
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
	return map[string]interface{}{
		"type": "Container", "style": "emphasis", "bleed": true,
		"separator": true, "spacing": "None",
		"items": []interface{}{map[string]interface{}{
			"type": "ColumnSet", "spacing": "None", "verticalContentAlignment": "Center",
			"columns": []interface{}{
				map[string]interface{}{
					"type": "Column", "width": "stretch",
					"items": []interface{}{map[string]interface{}{
						"type": "TextBlock", "text": escapeMarkdown(timeText), "size": "Small",
						"isSubtle": true, "spacing": "None", "wrap": false,
					}},
				},
				map[string]interface{}{
					"type": "Column", "width": "auto",
					"items": []interface{}{actionSet},
				},
			},
		}},
	}
}
