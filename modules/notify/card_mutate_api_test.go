package notify

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
)

// TestDocsAccessCardVariant is the capability-containment guard for the docs
// card-mutate endpoint (PR review P0): the shared `notification` bot sends docs,
// summary, and action-outcome cards, so the mutate endpoint must gate on the
// TARGET card's own variant, not merely on the sender bot. Only docs.access_*
// cards may be driven to terminal; any other notification card must be refused.
func TestDocsAccessCardVariant(t *testing.T) {
	envelope := func(variant string) []byte {
		if variant == "" {
			return []byte(`{"type":17,"card":{"metadata":{"octo":{}}}}`)
		}
		return []byte(`{"type":17,"card":{"metadata":{"octo":{"variant":"` + variant + `"}}}}`)
	}
	cases := []struct {
		name         string
		payload      []byte
		wantVariant  string
		wantIsAccess bool
	}{
		{"access_requested sibling card is allowed", envelope("docs.access_requested"), "docs.access_requested", true},
		{"access_granted terminal is allowed (idempotent re-mutate)", envelope("docs.access_granted"), "docs.access_granted", true},
		{"access_denied terminal is allowed", envelope("docs.access_denied"), "docs.access_denied", true},
		{"access_approved outcome is allowed", envelope("docs.access_approved"), "docs.access_approved", true},
		{"summary card is refused", envelope("summary.completed"), "summary.completed", false},
		{"action-outcome card is refused", envelope("action.outcome"), "action.outcome", false},
		{"a docs card outside the access family is refused", envelope("docs.shared"), "docs.shared", false},
		{"missing variant is refused", envelope(""), "", false},
		{"non-card payload is refused", []byte(`{"type":1,"card":{"metadata":{"octo":{"variant":"docs.access_requested"}}}}`), "", false},
		{"malformed json is refused", []byte(`{not json`), "", false},
		{"empty payload is refused", []byte(``), "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			variant, isAccess := docsAccessCardVariant(tc.payload)
			if isAccess != tc.wantIsAccess {
				t.Fatalf("docsAccessCardVariant isDocsAccess = %v, want %v (variant=%q)", isAccess, tc.wantIsAccess, variant)
			}
			if variant != tc.wantVariant {
				t.Fatalf("docsAccessCardVariant variant = %q, want %q", variant, tc.wantVariant)
			}
		})
	}
}

func TestReplaceSiblingWithRegistryResultUsesV3AndStoredContext(t *testing.T) {
	updater := &captureViewUpdater{}
	snapshot := carddispatch.CardMutationSnapshot{Envelope: json.RawMessage(`{
		"type":17,
		"profile":"octo/v2",
		"space_id":"space-1",
		"card":{
			"metadata":{"octo":{"variant":"docs.access_requested"}},
			"actions":[{
				"type":"Action.Submit",
				"id":"docs-access-approve",
				"data":{
					"owner":"docs",
					"action_type":"access_request.decision",
					"decision":"approve",
					"doc_id":"doc-1",
					"request_id":"request-1",
					"doc_title":"Roadmap",
					"actor":"Alice",
					"actor_avatar_url":"https://cdn.example.com/alice.png",
					"request_reason":"Need quarterly access",
					"requested_at_display":"2026-07-24 10:00",
					"message_time_display":"10:01",
					"permission_label":"Access",
					"permission_role_label":"Viewer",
					"source_name":"Docs"
				}
			}]
		}
	}`)}
	req := CardMutateReq{
		SpaceID: "space-1", ChannelID: "reviewer-b", ChannelType: 1,
		MessageID: "1002", Kind: DocsCardKindAccessGranted, DocID: "doc-1", Title: "caller title ignored",
	}

	if err := replaceSiblingWithRegistryResult(context.Background(), updater, req, snapshot, 99, "en"); err != nil {
		t.Fatalf("replaceSiblingWithRegistryResult: %v", err)
	}
	if updater.id != docsaccessrequest.TemplateID || updater.version != docsaccessrequest.TemplateVersionV3 ||
		updater.state != docsaccessrequest.StateApproved {
		t.Fatalf("update selection = %s@%s/%s", updater.id, updater.version, updater.state)
	}
	if updater.target.MessageID != req.MessageID || updater.target.CardSeq != 99 || updater.target.Target.SpaceID != req.SpaceID {
		t.Fatalf("update target = %+v", updater.target)
	}
	var fields map[string]any
	if err := json.Unmarshal(updater.fields, &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["requestId"] != "request-1" || fields["state"] != "approved" {
		t.Fatalf("result fields = %+v", fields)
	}
	document := fields["document"].(map[string]any)
	requester := fields["requester"].(map[string]any)
	permission := fields["permission"].(map[string]any)
	decision := fields["decision"].(map[string]any)
	if document["docId"] != "doc-1" || document["title"] != "Roadmap" || document["sourceName"] != "Docs" ||
		requester["name"] != "Alice" || requester["avatarUrl"] != "https://cdn.example.com/alice.png" ||
		permission["label"] != "Access" || permission["roleLabel"] != "Viewer" ||
		fields["requestReason"] != "Need quarterly access" || fields["requestedAtDisplay"] != "2026-07-24 10:00" ||
		fields["messageTimeDisplay"] != "10:01" || decision["operatorName"] != "Reviewer" {
		t.Fatalf("stored result context = %+v", fields)
	}
}

func TestReplaceSiblingWithRegistryResultRejectsCallerIdentityMismatch(t *testing.T) {
	snapshot := carddispatch.CardMutationSnapshot{Envelope: json.RawMessage(`{
		"type":17,
		"profile":"octo/v2",
		"space_id":"space-1",
		"card":{"metadata":{"octo":{"variant":"docs.access_requested"}},"actions":[{
			"type":"Action.Submit","id":"docs-access-approve",
			"data":{"doc_id":"doc-1","request_id":"request-1","doc_title":"Roadmap","actor":"Alice"}
		}]}
	}`)}
	tests := []struct {
		name string
		req  CardMutateReq
	}{
		{
			name: "space mismatch",
			req: CardMutateReq{SpaceID: "space-2", ChannelID: "reviewer-b", ChannelType: 1,
				MessageID: "1002", Kind: DocsCardKindAccessGranted, DocID: "doc-1"},
		},
		{
			name: "document mismatch",
			req: CardMutateReq{SpaceID: "space-1", ChannelID: "reviewer-b", ChannelType: 1,
				MessageID: "1002", Kind: DocsCardKindAccessGranted, DocID: "doc-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := replaceSiblingWithRegistryResult(context.Background(), &captureViewUpdater{}, tt.req, snapshot, 99, "en")
			if err == nil {
				t.Fatal("replaceSiblingWithRegistryResult error = nil")
			}
		})
	}
}

func TestReplaceSiblingWithRegistryResultMapsDeniedState(t *testing.T) {
	updater := &captureViewUpdater{}
	snapshot := carddispatch.CardMutationSnapshot{Envelope: json.RawMessage(`{
		"type":17,
		"profile":"octo/v2",
		"space_id":"space-1",
		"card":{"metadata":{"octo":{"variant":"docs.access_requested"}},"actions":[{
			"type":"Action.Submit","id":"docs-access-deny",
			"data":{"doc_id":"doc-1","request_id":"request-1","doc_title":"Roadmap","actor":"Alice"}
		}]}
	}`)}
	req := CardMutateReq{SpaceID: "space-1", ChannelID: "reviewer-b", ChannelType: 1,
		MessageID: "1002", Kind: DocsCardKindAccessDenied, DocID: "doc-1", DenyReason: "Out of scope"}

	if err := replaceSiblingWithRegistryResult(context.Background(), updater, req, snapshot, 100, "en"); err != nil {
		t.Fatalf("replaceSiblingWithRegistryResult: %v", err)
	}
	if updater.state != docsaccessrequest.StateRejected {
		t.Fatalf("state = %q, want %q", updater.state, docsaccessrequest.StateRejected)
	}
	var fields struct {
		State    string `json:"state"`
		Decision struct {
			OperatorName    string `json:"operatorName"`
			RejectionReason string `json:"rejectionReason"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(updater.fields, &fields); err != nil {
		t.Fatal(err)
	}
	if fields.State != "rejected" || fields.Decision.OperatorName != "Reviewer" || fields.Decision.RejectionReason != "Out of scope" {
		t.Fatalf("fields = %+v", fields)
	}
}
