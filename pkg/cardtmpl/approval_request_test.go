package cardtmpl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

func TestBuildApprovalRequestCardOwnsActionsAndReservedMetadata(t *testing.T) {
	raw, err := BuildApprovalRequestCard(ApprovalRequestCard{
		Title:        "**Publish** summary",
		Description:  "Review before publishing",
		Owner:        "smart-summary",
		ActionType:   "summary.publish.decision",
		Data:         map[string]string{"task_no": "task-1"},
		ApproveTitle: "Allow",
		DenyTitle:    "Deny",
	})
	if err != nil {
		t.Fatalf("BuildApprovalRequestCard() error = %v", err)
	}
	var card map[string]interface{}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if err := cardmsg.Validate(map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": cardmsg.ProfileV2, "card": card,
	}); err != nil {
		t.Fatalf("approval request card is invalid: %v", err)
	}
	actions, _ := card["actions"].([]interface{})
	if len(actions) != 2 {
		t.Fatalf("actions = %+v", actions)
	}
	decisions := map[string]bool{}
	for _, value := range actions {
		action, _ := value.(map[string]interface{})
		data, _ := action["data"].(map[string]interface{})
		if data["owner"] != "smart-summary" || data["action_type"] != "summary.publish.decision" || data["task_no"] != "task-1" {
			t.Fatalf("action data = %+v", data)
		}
		decision, _ := data["decision"].(string)
		decisions[decision] = true
	}
	if !decisions["approve"] || !decisions["deny"] {
		t.Fatalf("decisions = %+v", decisions)
	}
	if strings.Contains(string(raw), "http") {
		t.Fatalf("approval request leaked a callback URL: %s", raw)
	}
	if !strings.Contains(string(raw), `\\*\\*Publish\\*\\* summary`) {
		t.Fatalf("title was not markdown escaped: %s", raw)
	}
}

func TestBuildApprovalRequestCardRejectsReservedAndUnboundedData(t *testing.T) {
	base := ApprovalRequestCard{
		Title: "Approve", Owner: "tasks", ActionType: "task.decision",
		ApproveTitle: "Allow", DenyTitle: "Deny",
	}
	for _, data := range []map[string]string{
		{"owner": "spoofed"},
		{"decision": "approve"},
		{"bad key": "value"},
		{"task_id": strings.Repeat("x", 501)},
	} {
		input := base
		input.Data = data
		if _, err := BuildApprovalRequestCard(input); err == nil {
			t.Fatalf("BuildApprovalRequestCard(data=%v) error = nil", data)
		}
	}
}
