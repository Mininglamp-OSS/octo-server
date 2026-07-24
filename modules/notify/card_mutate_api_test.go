package notify

import "testing"

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
