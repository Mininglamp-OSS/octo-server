package cardtmpl

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

const (
	ApprovalApproveActionID = "approval-approve"
	ApprovalDenyActionID    = "approval-deny"
	maxApprovalDataFields   = 32
)

var (
	approvalOwnerPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	approvalActionTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	approvalDataKeyPattern    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

type ApprovalRequestCard struct {
	Title        string
	Description  string
	Owner        string
	ActionType   string
	Data         map[string]string
	ApproveTitle string
	DenyTitle    string
}

// BuildApprovalRequestCard is the shared server-owned approve/deny template.
// Callers provide bounded business identifiers, never an action owner, URL, or
// arbitrary Adaptive Card JSON.
func BuildApprovalRequestCard(input ApprovalRequestCard) (json.RawMessage, error) {
	title := truncateRunes(strings.TrimSpace(input.Title), maxTitleRunes)
	description := truncateRunes(strings.TrimSpace(input.Description), MaxExcerptRunes)
	approveTitle := strings.TrimSpace(input.ApproveTitle)
	denyTitle := strings.TrimSpace(input.DenyTitle)
	if title == "" || !approvalOwnerPattern.MatchString(input.Owner) || !approvalActionTypePattern.MatchString(input.ActionType) {
		return nil, errors.New("cardtmpl: approval request identity is invalid")
	}
	if approveTitle == "" || denyTitle == "" || utf8.RuneCountInString(approveTitle) > 80 || utf8.RuneCountInString(denyTitle) > 80 {
		return nil, errors.New("cardtmpl: approval action labels are invalid")
	}
	if len(input.Data) > maxApprovalDataFields {
		return nil, errors.New("cardtmpl: too many approval data fields")
	}
	baseData := make(map[string]interface{}, len(input.Data)+2)
	for key, value := range input.Data {
		if key == "owner" || key == "action_type" || key == "decision" ||
			!approvalDataKeyPattern.MatchString(key) || utf8.RuneCountInString(value) > maxFactRunes {
			return nil, errors.New("cardtmpl: approval data field is invalid")
		}
		baseData[key] = value
	}
	baseData["owner"] = input.Owner
	baseData["action_type"] = input.ActionType
	approveData := copyActionData(baseData)
	approveData["decision"] = "approve"
	denyData := copyActionData(baseData)
	denyData["decision"] = "deny"

	body := []interface{}{
		map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(title), "weight": "Bolder", "wrap": true,
		},
	}
	if description != "" {
		body = append(body, map[string]interface{}{
			"type": "TextBlock", "text": escapeMarkdown(description), "wrap": true,
		})
	}
	document := map[string]interface{}{
		"type": "AdaptiveCard", "version": cardmsg.CardVersion, "body": body,
		"actions": []interface{}{
			map[string]interface{}{
				"type": "Action.Submit", "id": ApprovalApproveActionID, "title": approveTitle, "data": approveData,
			},
			map[string]interface{}{
				"type": "Action.Submit", "id": ApprovalDenyActionID, "title": denyTitle, "data": denyData,
			},
		},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("cardtmpl: marshal approval request card: %w", err)
	}
	return raw, nil
}
