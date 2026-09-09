package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
)

// stubDeciderUsers resolves names from a fixed table; an unknown uid mimics the
// user service's "not found" error, and errFor forces a lookup error.
type stubDeciderUsers struct {
	names  map[string]string
	err    error
	calls  int
	lastID string
}

func (s *stubDeciderUsers) GetUser(uid string) (*user.Resp, error) {
	s.calls++
	s.lastID = uid
	if s.err != nil {
		return nil, s.err
	}
	if name, ok := s.names[uid]; ok {
		return &user.Resp{UID: uid, Name: name}, nil
	}
	return nil, errors.New("用户不存在！")
}

// stubDeciderSpaces answers active membership from spaceID -> uid -> name.
type stubDeciderSpaces struct {
	memberships map[string]map[string]string
	err         error
	calls       int
}

func (s *stubDeciderSpaces) ResolveActiveMemberSpaceName(spaceID, uid string) (string, bool, error) {
	s.calls++
	if s.err != nil {
		return "", false, s.err
	}
	if members, ok := s.memberships[spaceID]; ok {
		if name, ok := members[uid]; ok {
			return name, true, nil
		}
	}
	return "", false, nil
}

func TestResolveDeciderDisplayResolvesNameAndActiveSpace(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{"decider-1": "林澈"}}
	spaces := &stubDeciderSpaces{memberships: map[string]map[string]string{
		"space-op": {"decider-1": "运营中心"},
	}}
	got := resolveDeciderDisplay(users, spaces, "decider-1", "space-op", nil)
	if got.OperatorName != "林澈" || got.OperatorSpaceName != "运营中心" {
		t.Fatalf("resolved = %+v", got)
	}
}

// A user in several Spaces must resolve the Space name for the SPECIFIED
// decider_space_id, never another of their memberships.
func TestResolveDeciderDisplayMultiSpaceUsesSpecifiedSpace(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{"multi": "多空间用户"}}
	spaces := &stubDeciderSpaces{memberships: map[string]map[string]string{
		"space-a": {"multi": "Space A"},
		"space-b": {"multi": "Space B"},
	}}
	got := resolveDeciderDisplay(users, spaces, "multi", "space-b", nil)
	if got.OperatorSpaceName != "Space B" {
		t.Fatalf("multi-space resolution = %q, want Space B", got.OperatorSpaceName)
	}
}

// A decider_space_id the decider is not an active member of degrades the Space
// name to empty — it must never fall back to another Space.
func TestResolveDeciderDisplayInactiveMembershipDegradesSpace(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{"decider-1": "林澈"}}
	spaces := &stubDeciderSpaces{memberships: map[string]map[string]string{
		"space-other": {"decider-1": "别的空间"},
	}}
	got := resolveDeciderDisplay(users, spaces, "decider-1", "space-nonmember", nil)
	if got.OperatorName != "林澈" {
		t.Fatalf("name should still resolve: %+v", got)
	}
	if got.OperatorSpaceName != "" {
		t.Fatalf("non-member Space resolved to %q, want empty", got.OperatorSpaceName)
	}
}

// A missing user lookup degrades the name to empty (caller shows the generic
// placeholder) without erroring.
func TestResolveDeciderDisplayMissingUserDegradesName(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{}}
	spaces := &stubDeciderSpaces{}
	got := resolveDeciderDisplay(users, spaces, "ghost", "", nil)
	if got.OperatorName != "" {
		t.Fatalf("missing user name = %q, want empty", got.OperatorName)
	}
}

// A lookup error degrades to empty and reports via warn; it never propagates.
func TestResolveDeciderDisplayLookupErrorDegradesAndWarns(t *testing.T) {
	users := &stubDeciderUsers{err: errors.New("db down")}
	spaces := &stubDeciderSpaces{err: errors.New("db down")}
	var warnings int
	got := resolveDeciderDisplay(users, spaces, "decider-1", "space-op", func(string, error) { warnings++ })
	if got.OperatorName != "" || got.OperatorSpaceName != "" {
		t.Fatalf("errors should degrade to empty: %+v", got)
	}
	if warnings != 2 {
		t.Fatalf("warn count = %d, want 2 (name + space)", warnings)
	}
}

func TestResolveDeciderDisplayWithoutDeciderIsEmpty(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{"decider-1": "林澈"}}
	got := resolveDeciderDisplay(users, &stubDeciderSpaces{}, "", "space-op", nil)
	if got != (resolvedDeciderDisplay{}) {
		t.Fatalf("no decider uid should resolve to zero: %+v", got)
	}
	if users.calls != 0 {
		t.Fatalf("user service was called without a decider uid")
	}
}

// ---- DocsActionFinalizer.resolveAuthoritativeDeciderDisplay ----

func approvedResultWithForgedDisplay(deciderUID, deciderSpaceID string) cardactiondispatch.DecisionResult {
	return cardactiondispatch.DecisionResult{
		Disposition: cardactiondispatch.DispositionApplied, State: cardactiondispatch.StateApproved,
		RequesterUID: "user-a", DeciderUID: deciderUID, DeciderSpaceID: deciderSpaceID,
		DecidedAt: 1_787_223_660,
		Display: map[string]string{
			"title":               "Roadmap",
			"operator_name":       "FORGED NAME",
			"operator_space_name": "FORGED SPACE",
			"decided_at":          "1970-01-01",
		},
	}
}

// The authoritative ids win over caller-supplied display copy: a forged
// operator_name/operator_space_name is discarded and replaced with the
// internally resolved values, and decided_at comes from the authoritative field.
func TestResolveAuthoritativeDeciderDisplayDiscardsForgedNames(t *testing.T) {
	f := &DocsActionFinalizer{
		users:  &stubDeciderUsers{names: map[string]string{"decider-1": "林澈"}},
		spaces: &stubDeciderSpaces{memberships: map[string]map[string]string{"space-op": {"decider-1": "运营中心"}}},
	}
	out := f.resolveAuthoritativeDeciderDisplay(context.Background(), approvedResultWithForgedDisplay("decider-1", "space-op"))
	if out.Display["operator_name"] != "林澈" {
		t.Fatalf("operator_name = %q, want resolved 林澈 (forged discarded)", out.Display["operator_name"])
	}
	if out.Display["operator_space_name"] != "运营中心" {
		t.Fatalf("operator_space_name = %q, want resolved 运营中心", out.Display["operator_space_name"])
	}
	if out.Display["decided_at"] != "2026-08-20 19:01" {
		t.Fatalf("decided_at = %q, want authoritative value", out.Display["decided_at"])
	}
	if out.Display["title"] != "Roadmap" {
		t.Fatalf("unrelated display fields must be preserved: %+v", out.Display)
	}
}

// Duplicate/concurrent click: Docs returns the FIRST decider's ids, not the late
// clicker's. Resolution keys off result.DeciderUID, so the card shows the real
// decider even though the forged display named someone else.
// An authoritative decider without an authoritative timestamp must clear any
// caller-supplied legacy time rather than pairing it with the trusted actor.
func TestResolveAuthoritativeDeciderDisplayClearsForgedTimeWhenMissingAuthoritativeTime(t *testing.T) {
	f := &DocsActionFinalizer{
		users:  &stubDeciderUsers{names: map[string]string{"decider-1": "林澈"}},
		spaces: &stubDeciderSpaces{},
	}
	result := approvedResultWithForgedDisplay("decider-1", "")
	result.DecidedAt = 0
	out := f.resolveAuthoritativeDeciderDisplay(context.Background(), result)
	if _, exists := out.Display["decided_at"]; exists {
		t.Fatalf("forged decided_at survived without authoritative time: %+v", out.Display)
	}
}

func TestResolveAuthoritativeDeciderDisplayHonorsDeciderNotClicker(t *testing.T) {
	f := &DocsActionFinalizer{
		users: &stubDeciderUsers{names: map[string]string{
			"first-decider": "原审批人", "late-clicker": "迟到点击者",
		}},
		spaces: &stubDeciderSpaces{memberships: map[string]map[string]string{
			"space-first": {"first-decider": "首审空间"},
		}},
	}
	result := approvedResultWithForgedDisplay("first-decider", "space-first")
	result.Display["operator_name"] = "迟到点击者" // as if a late clicker's name leaked in
	out := f.resolveAuthoritativeDeciderDisplay(context.Background(), result)
	if out.Display["operator_name"] != "原审批人" {
		t.Fatalf("operator_name = %q, want the authoritative decider 原审批人", out.Display["operator_name"])
	}
	if out.Display["operator_space_name"] != "首审空间" {
		t.Fatalf("operator_space_name = %q, want 首审空间", out.Display["operator_space_name"])
	}
}

// A failed internal lookup must NOT fail the committed decision: the display just
// degrades (empty → generic placeholder downstream) and the caller-supplied
// forged value is still discarded.
func TestResolveAuthoritativeDeciderDisplayFailureDegradesNotFails(t *testing.T) {
	f := &DocsActionFinalizer{
		users:  &stubDeciderUsers{err: errors.New("db down")},
		spaces: &stubDeciderSpaces{err: errors.New("db down")},
	}
	out := f.resolveAuthoritativeDeciderDisplay(context.Background(), approvedResultWithForgedDisplay("decider-1", "space-op"))
	if out.Display["operator_name"] != "" || out.Display["operator_space_name"] != "" {
		t.Fatalf("lookup failure should degrade display to empty: %+v", out.Display)
	}
}

// Legacy callbacks with no decider ids keep their supplied display untouched
// (bounded compatibility fallback) and never touch the resolvers.
func TestResolveAuthoritativeDeciderDisplayLegacyFallback(t *testing.T) {
	users := &stubDeciderUsers{}
	spaces := &stubDeciderSpaces{}
	f := &DocsActionFinalizer{users: users, spaces: spaces}
	legacy := cardactiondispatch.DecisionResult{
		Disposition: cardactiondispatch.DispositionApplied, State: cardactiondispatch.StateApproved,
		RequesterUID: "user-a",
		Display:      map[string]string{"operator_name": "Caller Name", "operator_space_name": "Caller Space"},
	}
	out := f.resolveAuthoritativeDeciderDisplay(context.Background(), legacy)
	if out.Display["operator_name"] != "Caller Name" || out.Display["operator_space_name"] != "Caller Space" {
		t.Fatalf("legacy display must be preserved: %+v", out.Display)
	}
	if users.calls != 0 || spaces.calls != 0 {
		t.Fatalf("legacy path must not call resolvers: users=%d spaces=%d", users.calls, spaces.calls)
	}
}

// A non-terminal state carries no decision block, so resolution is skipped even
// when decider ids are present.
func TestResolveAuthoritativeDeciderDisplaySkipsNonTerminal(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{"decider-1": "林澈"}}
	f := &DocsActionFinalizer{users: users, spaces: &stubDeciderSpaces{}}
	cancelled := cardactiondispatch.DecisionResult{
		State: cardactiondispatch.StateCancelled, DeciderUID: "decider-1",
		Display: map[string]string{"operator_name": "unchanged"},
	}
	out := f.resolveAuthoritativeDeciderDisplay(context.Background(), cancelled)
	if out.Display["operator_name"] != "unchanged" {
		t.Fatalf("non-terminal display must be untouched: %+v", out.Display)
	}
	if users.calls != 0 {
		t.Fatalf("non-terminal must not resolve: users=%d", users.calls)
	}
}

// ---- /v1/internal/cards/mutate authoritative resolution ----

func TestMutateResolvesDeciderDisplayAndIgnoresCallerNames(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{"decider-1": "Reviewer B"}}
	spaces := &stubDeciderSpaces{memberships: map[string]map[string]string{"operator-space-b": {"decider-1": "Operator Space B"}}}
	n := &Notify{deciderUsers: users, deciderSpaces: spaces, Log: log.NewTLog("test")}
	req := CardMutateReq{
		DeciderUID: "decider-1", DeciderSpaceID: "operator-space-b",
		OperatorName: "FORGED", OperatorSpaceName: "FORGED SPACE",
	}
	n.resolveMutateOperatorDisplay(&req)
	if req.OperatorName != "Reviewer B" || req.OperatorSpaceName != "Operator Space B" {
		t.Fatalf("mutate resolution = name:%q space:%q (forged must be discarded)", req.OperatorName, req.OperatorSpaceName)
	}
}

// New decider_uid with no authoritative time clears a forged legacy display
// timestamp instead of carrying it onto the terminal sibling card.
func TestMutateClearsForgedTimeWhenAuthoritativeTimeMissing(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{"decider-1": "Reviewer B"}}
	n := &Notify{deciderUsers: users, deciderSpaces: &stubDeciderSpaces{}, Log: log.NewTLog("test")}
	req := CardMutateReq{
		DeciderUID: "decider-1", OperatorName: "FORGED", OperatorSpaceName: "FORGED SPACE",
		DecidedAtDisplay: "2099-01-01 00:00",
	}
	n.resolveMutateOperatorDisplay(&req)
	if req.DecidedAtDisplay != "" {
		t.Fatalf("forged decided_at_display survived: %q", req.DecidedAtDisplay)
	}
}

// The older operator fields are display context, not aliases for authoritative
// decider identity. Without decider_uid, this compatibility path does no lookup.
func TestMutateDoesNotTreatLegacyOperatorFieldsAsDeciderAliases(t *testing.T) {
	users := &stubDeciderUsers{names: map[string]string{"decider-1": "Reviewer B"}}
	spaces := &stubDeciderSpaces{memberships: map[string]map[string]string{"space-op": {"decider-1": "Ops"}}}
	n := &Notify{deciderUsers: users, deciderSpaces: spaces, Log: log.NewTLog("test")}
	req := CardMutateReq{OperatorUID: "decider-1", OperatorSpaceID: "space-op", OperatorName: "Legacy Name", OperatorSpaceName: "Legacy Space", DecidedAtDisplay: "legacy-time"}
	n.resolveMutateOperatorDisplay(&req)
	if req.OperatorName != "Legacy Name" || req.OperatorSpaceName != "Legacy Space" || req.DecidedAtDisplay != "legacy-time" {
		t.Fatalf("legacy display context changed = name:%q space:%q time:%q", req.OperatorName, req.OperatorSpaceName, req.DecidedAtDisplay)
	}
	if users.calls != 0 || spaces.calls != 0 {
		t.Fatalf("legacy fields triggered authoritative lookup: users=%d spaces=%d", users.calls, spaces.calls)
	}
}

// No decider id → the caller's display strings are left intact (bounded compat).
func TestMutateKeepsCallerDisplayWhenNoDeciderID(t *testing.T) {
	users := &stubDeciderUsers{}
	n := &Notify{deciderUsers: users, deciderSpaces: &stubDeciderSpaces{}, Log: log.NewTLog("test")}
	req := CardMutateReq{OperatorName: "Trusted Caller", OperatorSpaceName: "Trusted Space"}
	n.resolveMutateOperatorDisplay(&req)
	if req.OperatorName != "Trusted Caller" || req.OperatorSpaceName != "Trusted Space" {
		t.Fatalf("legacy display must survive: name:%q space:%q", req.OperatorName, req.OperatorSpaceName)
	}
	if users.calls != 0 {
		t.Fatalf("no decider id must not call the user service")
	}
}
