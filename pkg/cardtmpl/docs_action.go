package cardtmpl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	DocsApproveActionID = "docs-access-approve"
	DocsDenyActionID    = "docs-access-deny"
)

type ApprovalActions struct {
	ApproveTitle string
	DenyTitle    string
}

// BuildDocsAccessRequestCard extends the reviewed docs resource template with
// the two server-authored Submit actions used by the docs approval pilot. The
// callback route itself never appears in card bytes; only bounded domain data
// needed by the registered docs route is embedded.
func BuildDocsAccessRequestCard(
	ctx context.Context,
	webLoginURL string,
	docID string,
	requestID string,
	spaceID string,
	resource ResourceCard,
	actions ApprovalActions,
) (json.RawMessage, error) {
	if strings.TrimSpace(requestID) == "" || utf8.RuneCountInString(requestID) > 200 {
		return nil, errors.New("cardtmpl: request ID is invalid")
	}
	if strings.TrimSpace(actions.ApproveTitle) == "" || strings.TrimSpace(actions.DenyTitle) == "" ||
		utf8.RuneCountInString(actions.ApproveTitle) > 80 || utf8.RuneCountInString(actions.DenyTitle) > 80 {
		return nil, errors.New("cardtmpl: approval action labels are invalid")
	}
	document, err := BuildDocsResourceCard(ctx, webLoginURL, docID, spaceID, resource)
	if err != nil {
		return nil, err
	}
	var card map[string]interface{}
	if err := json.Unmarshal(document, &card); err != nil {
		return nil, fmt.Errorf("cardtmpl: decode docs resource card: %w", err)
	}
	baseData := map[string]interface{}{
		"owner":       "docs",
		"action_type": "access_request.decision",
		"doc_id":      docID,
		"request_id":  requestID,
	}
	approveData := copyActionData(baseData)
	approveData["decision"] = "approve"
	denyData := copyActionData(baseData)
	denyData["decision"] = "deny"
	card["actions"] = []interface{}{
		map[string]interface{}{
			"type":  "Action.Submit",
			"id":    DocsApproveActionID,
			"title": actions.ApproveTitle,
			"data":  approveData,
		},
		map[string]interface{}{
			"type":  "Action.Submit",
			"id":    DocsDenyActionID,
			"title": actions.DenyTitle,
			"data":  denyData,
		},
	}
	raw, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("cardtmpl: marshal docs access request card: %w", err)
	}
	return raw, nil
}

func copyActionData(source map[string]interface{}) map[string]interface{} {
	copy := make(map[string]interface{}, len(source)+1)
	for key, value := range source {
		copy[key] = value
	}
	return copy
}
