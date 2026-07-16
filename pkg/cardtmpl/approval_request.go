package cardtmpl

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

const (
	ApprovalApproveActionID = "approval-approve"
	ApprovalDenyActionID    = "approval-deny"
	maxApprovalDataFields   = 32
	// MaxApprovalCustomActions caps the actions[] on approval_card. Five keeps
	// the terminal card readable on narrow layouts; enlarging it requires a
	// wire-contract review because clients render every button inline.
	MaxApprovalCustomActions = 5
	// MaxApprovalActionTitleRunes matches the shared 80-rune ceiling for
	// button labels; longer copy belongs in title/description.
	MaxApprovalActionTitleRunes = 80
)

var (
	approvalOwnerPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	approvalActionTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	approvalDataKeyPattern    = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	// approvalDecisionPattern matches the stable callback token surfaced back
	// to the consumer through DecisionRequest.decision. Lowercase, starts with
	// a letter, digits/underscore/dot/dash allowed, 1-48 chars.
	approvalDecisionPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,47}$`)

	// reservedApprovalDecisions matches the tokens the legacy approve/deny
	// template emits (`approval-approve` / `approval-deny`). A custom-actions
	// caller that picked either of these would render buttons whose IDs
	// collide with the legacy template on any client-side analytics or replay
	// log keyed only on action_id. Reject to keep the ID namespace clean.
	reservedApprovalDecisions = map[string]struct{}{
		"approve": {},
		"deny":    {},
	}
)

// ApprovalRequestAction lets the caller name a bounded, custom Action.Submit
// button on the generic approval card. Server owns the action ID, the injected
// owner/action_type metadata, and the card layout; callers only supply the
// stable callback token (decision) and a display label (title).
//
// The tokens "approve" and "deny" are reserved for the legacy 2-button
// template; passing either as Decision returns a validation error to keep the
// server-derived "approval-<decision>" action IDs collision-free with the
// legacy ApprovalApproveActionID / ApprovalDenyActionID.
type ApprovalRequestAction struct {
	Decision string
	Title    string
}

type ApprovalRequestCard struct {
	Title       string
	Description string
	Owner       string
	ActionType  string
	Data        map[string]string
	// ApproveTitle and DenyTitle drive the localized 2-button template when
	// Actions is nil/empty. Callers using Actions leave both empty.
	ApproveTitle string
	DenyTitle    string
	// Actions, when non-nil, replaces the approve/deny template with 1..N
	// server-built Action.Submit buttons where N = MaxApprovalCustomActions.
	// A nil slice preserves the legacy approve/deny wire form byte-compatibly;
	// an explicit empty slice is a caller bug and rejected by validation.
	Actions []ApprovalRequestAction
}

// BuildApprovalRequestCard is the shared server-owned approval template.
// Callers provide bounded business identifiers, never an action owner, URL, or
// arbitrary Adaptive Card JSON. When input.Actions is nil the output is
// identical to the pre-http-actions release (localized approve/deny buttons);
// a non-nil Actions slice is required to be 1..MaxApprovalCustomActions long
// and drives the button set instead.
func BuildApprovalRequestCard(input ApprovalRequestCard) (json.RawMessage, error) {
	title := truncateRunes(strings.TrimSpace(input.Title), maxTitleRunes)
	description := truncateRunes(strings.TrimSpace(input.Description), MaxExcerptRunes)
	if title == "" || !approvalOwnerPattern.MatchString(input.Owner) || !approvalActionTypePattern.MatchString(input.ActionType) {
		return nil, errors.New("cardtmpl: approval request identity is invalid")
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

	actions, err := buildApprovalActions(input, baseData)
	if err != nil {
		return nil, err
	}

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
		"actions": actions,
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("cardtmpl: marshal approval request card: %w", err)
	}
	return raw, nil
}

func buildApprovalActions(input ApprovalRequestCard, baseData map[string]interface{}) ([]interface{}, error) {
	// nil (unset) preserves the legacy approve/deny template.
	// A non-nil but empty slice signals the caller *tried* to send a custom
	// action set and picked zero; that is a caller bug, not a fallback.
	if input.Actions == nil {
		approveTitle := strings.TrimSpace(input.ApproveTitle)
		denyTitle := strings.TrimSpace(input.DenyTitle)
		if approveTitle == "" || denyTitle == "" ||
			utf8.RuneCountInString(approveTitle) > MaxApprovalActionTitleRunes ||
			utf8.RuneCountInString(denyTitle) > MaxApprovalActionTitleRunes {
			return nil, errors.New("cardtmpl: approval action labels are invalid")
		}
		approveData := copyActionData(baseData)
		approveData["decision"] = "approve"
		denyData := copyActionData(baseData)
		denyData["decision"] = "deny"
		return []interface{}{
			map[string]interface{}{
				"type": "Action.Submit", "id": ApprovalApproveActionID, "title": approveTitle,
				"style": "positive", "data": approveData,
			},
			map[string]interface{}{
				"type": "Action.Submit", "id": ApprovalDenyActionID, "title": denyTitle,
				"style": "destructive", "data": denyData,
			},
		}, nil
	}
	if input.ApproveTitle != "" || input.DenyTitle != "" {
		return nil, errors.New("cardtmpl: approval actions cannot mix approve/deny defaults with custom actions")
	}
	if len(input.Actions) == 0 {
		return nil, errors.New("cardtmpl: approval actions must contain at least one entry")
	}
	if len(input.Actions) > MaxApprovalCustomActions {
		return nil, errors.New("cardtmpl: too many approval actions")
	}
	seen := make(map[string]struct{}, len(input.Actions))
	built := make([]interface{}, 0, len(input.Actions))
	for _, action := range input.Actions {
		decision := action.Decision
		if !approvalDecisionPattern.MatchString(decision) {
			return nil, errors.New("cardtmpl: approval action decision is invalid")
		}
		if _, reserved := reservedApprovalDecisions[decision]; reserved {
			return nil, errors.New("cardtmpl: approval action decision collides with reserved approve/deny namespace")
		}
		if _, dup := seen[decision]; dup {
			return nil, errors.New("cardtmpl: approval action decisions must be unique")
		}
		seen[decision] = struct{}{}

		// Reject control characters on the *raw* title, before TrimSpace
		// strips whitespace-class runes like \t \n \r. Otherwise leading or
		// trailing tabs/newlines would sneak through as "trimmed clean".
		if containsControl(action.Title) {
			return nil, errors.New("cardtmpl: approval action title is invalid")
		}
		title := strings.TrimSpace(action.Title)
		if title == "" || utf8.RuneCountInString(title) > MaxApprovalActionTitleRunes {
			return nil, errors.New("cardtmpl: approval action title is invalid")
		}
		data := copyActionData(baseData)
		data["decision"] = decision
		built = append(built, map[string]interface{}{
			"type":  "Action.Submit",
			"id":    "approval-" + decision,
			"title": title,
			"data":  data,
		})
	}
	return built, nil
}

func containsControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
