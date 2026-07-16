package cardtmpl

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

// TestBuildApprovalRequestCardCustomActionsRenderServerBuiltButtons locks the
// http-actions follow-up: when Actions is non-empty the card renders exactly
// those Action.Submit buttons with server-owned IDs and reserved metadata; the
// old approve/deny path stays untouched when Actions is nil.
func TestBuildApprovalRequestCardCustomActionsRenderServerBuiltButtons(t *testing.T) {
	raw, err := BuildApprovalRequestCard(ApprovalRequestCard{
		Title:      "Execute task",
		Owner:      "tasks",
		ActionType: "task.execute.decision",
		Data:       map[string]string{"task_id": "task-1"},
		Actions: []ApprovalRequestAction{
			{Decision: "execute", Title: "执行"},
			{Decision: "reject", Title: "拒绝"},
			{Decision: "cancel", Title: "取消"},
		},
	})
	if err != nil {
		t.Fatalf("BuildApprovalRequestCard() error = %v", err)
	}
	var card map[string]interface{}
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("decode: %v", err)
	}
	actions, _ := card["actions"].([]interface{})
	if len(actions) != 3 {
		t.Fatalf("len(actions) = %d, want 3", len(actions))
	}
	want := map[string]string{
		"approval-execute": "执行",
		"approval-reject":  "拒绝",
		"approval-cancel":  "取消",
	}
	for _, value := range actions {
		action, _ := value.(map[string]interface{})
		id, _ := action["id"].(string)
		title, _ := action["title"].(string)
		if expected, ok := want[id]; !ok || expected != title {
			t.Fatalf("action id=%q title=%q not in wanted set %+v", id, title, want)
		}
		delete(want, id)
		data, _ := action["data"].(map[string]interface{})
		if data["owner"] != "tasks" || data["action_type"] != "task.execute.decision" || data["task_id"] != "task-1" {
			t.Fatalf("reserved metadata not injected on %s: %+v", id, data)
		}
		decision, _ := data["decision"].(string)
		if want, _ := strings.CutPrefix(id, "approval-"); decision != want {
			t.Fatalf("decision on %s = %q, want %q", id, decision, want)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing actions in output: %+v", want)
	}
	if strings.Contains(string(raw), "http") {
		t.Fatalf("card leaked a URL: %s", raw)
	}
}

// TestBuildApprovalRequestCardOmittedActionsIsByteCompatible verifies the
// http-actions change does not perturb the legacy approve/deny output. The
// exact bytes below are captured from the pre-http-actions release; any drift
// in ordering, keys, escaping, or reserved metadata will fail this test and
// force a wire-contract review before shipping.
func TestBuildApprovalRequestCardOmittedActionsIsByteCompatible(t *testing.T) {
	raw, err := BuildApprovalRequestCard(ApprovalRequestCard{
		Title:        "Publish summary",
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
	const golden = `{"actions":[{"data":{"action_type":"summary.publish.decision","decision":"approve","owner":"smart-summary","task_no":"task-1"},"id":"approval-approve","title":"Allow","type":"Action.Submit"},{"data":{"action_type":"summary.publish.decision","decision":"deny","owner":"smart-summary","task_no":"task-1"},"id":"approval-deny","title":"Deny","type":"Action.Submit"}],"body":[{"text":"Publish summary","type":"TextBlock","weight":"Bolder","wrap":true},{"text":"Review before publishing","type":"TextBlock","wrap":true}],"type":"AdaptiveCard","version":"1.5"}`
	if string(raw) != golden {
		t.Fatalf("legacy approve/deny bytes drifted from #588\n  got: %s\n want: %s", raw, golden)
	}
}

// TestBuildApprovalRequestCardNonNilEmptyActionsFailsClosed ensures a caller
// that ships `"actions": []` is treated as a caller bug rather than silently
// downgraded to the default approve/deny template. nil (omitted) still routes
// to the legacy path.
func TestBuildApprovalRequestCardNonNilEmptyActionsFailsClosed(t *testing.T) {
	base := ApprovalRequestCard{
		Title: "Execute task", Owner: "tasks", ActionType: "task.execute.decision",
	}
	if _, err := BuildApprovalRequestCard(func() ApprovalRequestCard {
		c := base
		c.Actions = []ApprovalRequestAction{}
		return c
	}()); err == nil {
		t.Fatal("BuildApprovalRequestCard(actions=[]) error = nil, want caller-bug rejection")
	}
	// nil (unset) plus approve/deny labels still succeeds — the legacy path.
	if _, err := BuildApprovalRequestCard(func() ApprovalRequestCard {
		c := base
		c.ApproveTitle = "Allow"
		c.DenyTitle = "Deny"
		return c
	}()); err != nil {
		t.Fatalf("BuildApprovalRequestCard(actions=nil) error = %v, want legacy success", err)
	}
}

func TestBuildApprovalRequestCardRejectsInvalidCustomActions(t *testing.T) {
	base := ApprovalRequestCard{
		Title: "Execute task", Owner: "tasks", ActionType: "task.execute.decision",
	}
	tests := []struct {
		name    string
		actions []ApprovalRequestAction
	}{
		{"single item empty decision and title", []ApprovalRequestAction{{Decision: "", Title: ""}}},
		{"missing decision", []ApprovalRequestAction{{Title: "Execute"}}},
		{"uppercase decision", []ApprovalRequestAction{{Decision: "Execute", Title: "Execute"}}},
		{"decision with space", []ApprovalRequestAction{{Decision: "do it", Title: "Execute"}}},
		{"decision too long", []ApprovalRequestAction{{Decision: strings.Repeat("a", 49), Title: "Execute"}}},
		{"decision starting with digit", []ApprovalRequestAction{{Decision: "1exec", Title: "Execute"}}},
		{"reserved-looking still valid pattern still rejected via duplicate", []ApprovalRequestAction{
			{Decision: "execute", Title: "Execute"},
			{Decision: "execute", Title: "Redo"},
		}},
		{"reserved decision approve collides with legacy template", []ApprovalRequestAction{
			{Decision: "approve", Title: "Allow"},
		}},
		{"reserved decision deny collides with legacy template", []ApprovalRequestAction{
			{Decision: "deny", Title: "Reject"},
		}},
		{"reserved decision approve mixed with other valid decisions", []ApprovalRequestAction{
			{Decision: "execute", Title: "Execute"},
			{Decision: "approve", Title: "Allow"},
		}},
		{"empty title", []ApprovalRequestAction{{Decision: "execute", Title: ""}}},
		{"whitespace title", []ApprovalRequestAction{{Decision: "execute", Title: "   "}}},
		{"title too long", []ApprovalRequestAction{{Decision: "execute", Title: strings.Repeat("字", 81)}}},
		{"title with control char", []ApprovalRequestAction{{Decision: "execute", Title: "Execute\x07"}}},
		{"title with leading tab (trim would hide it)", []ApprovalRequestAction{{Decision: "execute", Title: "\tExecute"}}},
		{"title with trailing newline (trim would hide it)", []ApprovalRequestAction{{Decision: "execute", Title: "Execute\n"}}},
		{"title with carriage return (trim would hide it)", []ApprovalRequestAction{{Decision: "execute", Title: "Execute\r"}}},
		{"title with vertical tab", []ApprovalRequestAction{{Decision: "execute", Title: "Exec\vute"}}},
		{"exceeds max count", func() []ApprovalRequestAction {
			out := make([]ApprovalRequestAction, MaxApprovalCustomActions+1)
			for i := range out {
				out[i] = ApprovalRequestAction{
					Decision: string(rune('a'+i)) + "cmd",
					Title:    "T",
				}
			}
			return out
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base
			input.Actions = tt.actions
			if _, err := BuildApprovalRequestCard(input); err == nil {
				t.Fatalf("BuildApprovalRequestCard(actions=%+v) error = nil, want rejection", tt.actions)
			}
		})
	}
}

func TestBuildApprovalRequestCardRejectsMixingCustomActionsWithApproveDenyDefaults(t *testing.T) {
	input := ApprovalRequestCard{
		Title: "Execute task", Owner: "tasks", ActionType: "task.execute.decision",
		ApproveTitle: "Allow",
		Actions: []ApprovalRequestAction{
			{Decision: "execute", Title: "Execute"},
		},
	}
	if _, err := BuildApprovalRequestCard(input); err == nil {
		t.Fatal("BuildApprovalRequestCard() error = nil; want mixing to be rejected")
	}
}

func TestBuildApprovalRequestCardMaxCustomActionsIsAccepted(t *testing.T) {
	actions := make([]ApprovalRequestAction, MaxApprovalCustomActions)
	for i := range actions {
		actions[i] = ApprovalRequestAction{
			Decision: string(rune('a'+i)) + "opt",
			Title:    "T",
		}
	}
	if _, err := BuildApprovalRequestCard(ApprovalRequestCard{
		Title: "Choose", Owner: "tasks", ActionType: "task.pick.decision",
		Actions: actions,
	}); err != nil {
		t.Fatalf("BuildApprovalRequestCard(max) error = %v", err)
	}
}

// TestBuildApprovalRequestCardRoundTripsThroughSubmitAction stitches the
// builder to the runtime extractor. The two sides are exercised separately
// elsewhere, but the click path relies on their shapes agreeing: builder emits
// `id = "approval-<decision>"` with a top-level `actions[]`, and
// cardmsg.SubmitAction traverses `card["actions"]` matching by type/id and
// returns the authored `data`. A silent drift in either side (id derivation
// change, key rename, moving actions off the top-level) would fail this test
// while every isolated test in this PR still passed.
//
// Covers both branches — custom actions and the legacy approve/deny — plus a
// synthetic action ID lookup that must miss.
func TestBuildApprovalRequestCardRoundTripsThroughSubmitAction(t *testing.T) {
	newEnvelope := func(t *testing.T, raw json.RawMessage) []byte {
		t.Helper()
		var card map[string]interface{}
		if err := json.Unmarshal(raw, &card); err != nil {
			t.Fatalf("decode card: %v", err)
		}
		envelope, err := json.Marshal(map[string]interface{}{
			"type":         cardmsg.InteractiveCard.Int(),
			"card_version": cardmsg.CardVersion,
			"profile":      cardmsg.ProfileV2,
			"card":         card,
		})
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		return envelope
	}

	t.Run("custom actions", func(t *testing.T) {
		raw, err := BuildApprovalRequestCard(ApprovalRequestCard{
			Title:      "Execute task",
			Owner:      "tasks",
			ActionType: "task.execute.decision",
			Data:       map[string]string{"task_id": "task-1"},
			Actions: []ApprovalRequestAction{
				{Decision: "execute", Title: "Execute"},
				{Decision: "reject", Title: "Reject"},
				{Decision: "cancel", Title: "Cancel"},
			},
		})
		if err != nil {
			t.Fatalf("BuildApprovalRequestCard() error = %v", err)
		}
		envelope := newEnvelope(t, raw)

		for decision, wantTaskID := range map[string]string{"execute": "task-1", "reject": "task-1", "cancel": "task-1"} {
			id := "approval-" + decision
			data, found := cardmsg.SubmitAction(envelope, id)
			if !found {
				t.Fatalf("SubmitAction(%q) found = false; builder emitted a button the extractor cannot find", id)
			}
			if data["decision"] != decision {
				t.Fatalf("SubmitAction(%q).decision = %v, want %q", id, data["decision"], decision)
			}
			if data["owner"] != "tasks" || data["action_type"] != "task.execute.decision" {
				t.Fatalf("reserved metadata lost on %q: %+v", id, data)
			}
			if data["task_id"] != wantTaskID {
				t.Fatalf("shared data lost on %q: task_id = %v, want %q", id, data["task_id"], wantTaskID)
			}
		}

		if _, found := cardmsg.SubmitAction(envelope, "approval-nope"); found {
			t.Fatal("SubmitAction(bogus) found = true; extractor accepted an ID the builder never emitted")
		}
	})

	t.Run("legacy approve deny", func(t *testing.T) {
		raw, err := BuildApprovalRequestCard(ApprovalRequestCard{
			Title:        "Publish summary",
			Owner:        "smart-summary",
			ActionType:   "summary.publish.decision",
			Data:         map[string]string{"task_no": "task-7"},
			ApproveTitle: "Allow",
			DenyTitle:    "Deny",
		})
		if err != nil {
			t.Fatalf("BuildApprovalRequestCard() error = %v", err)
		}
		envelope := newEnvelope(t, raw)

		approveData, found := cardmsg.SubmitAction(envelope, ApprovalApproveActionID)
		if !found || approveData["decision"] != "approve" ||
			approveData["owner"] != "smart-summary" || approveData["task_no"] != "task-7" {
			t.Fatalf("legacy approve action lost round-trip: found=%v data=%+v", found, approveData)
		}
		denyData, found := cardmsg.SubmitAction(envelope, ApprovalDenyActionID)
		if !found || denyData["decision"] != "deny" {
			t.Fatalf("legacy deny action lost round-trip: found=%v data=%+v", found, denyData)
		}
	})
}
