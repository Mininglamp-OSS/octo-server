package space

// Structural guard over the seat-transition funnel.
//
// # What it is for
//
// These checks constrain the call shape of the established seat-transition
// funnel. They do not enumerate every possible SQL writer. The owning modules'
// engine tests exercise the resulting seat status and Project epoch changes.
//
// # Why this shape rather than counting again
//
// Six of the twelve review rounds on this branch found the same mechanism: a
// hand-maintained enumeration missing one element — a writer, a direction, two of four
// doors, one hop of a propagation. The census is itself such an enumeration, and three
// of its baseline REASONS have been false at some point while its counts were right.
// This assertion is not an enumeration: it names one function and requires its call
// sites to be in one place. There is nothing to keep in sync.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const seatStepsCallee = "runSeatTransitionTxSteps("

// TestSeatTransitionStepsHaveOneCaller pins that only the funnel runs the tx steps.
func TestSeatTransitionStepsHaveOneCaller(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	callers := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Clean(name))
		if readErr != nil {
			t.Fatalf("read %s: %v", name, readErr)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			code := line
			// Line comments only, and quote-unaware is fine here: this callee name
			// never appears inside a string literal in this package. The census's
			// stripper is the one that had to be careful, because it counts SQL.
			if i := strings.Index(code, "//"); i >= 0 {
				code = code[:i]
			}
			if strings.Contains(code, seatStepsCallee) {
				callers[name]++
			}
		}
	}

	// The definition itself is a declaration, not a call, and lives elsewhere.
	delete(callers, "member_removal.go")

	if len(callers) != 1 {
		t.Fatalf("runSeatTransitionTxSteps must be called from exactly ONE file "+
			"(seat_transition.go), found %d: %v\n\n"+
			"A seat door that writes space_member.status itself and then runs the steps by "+
			"hand is how this branch shipped six P1s: the hand-written sequence is five "+
			"steps long, two of them are \"must remember\", and each new door repeated all "+
			"five. Call openSeatTx or closeSeatTx instead — they do the write, resolve the "+
			"canonical uid out of space_member, and run the steps as one unit.", len(callers), callers)
	}
	if n := callers["seat_transition.go"]; n != 2 {
		t.Errorf("seat_transition.go must call runSeatTransitionTxSteps exactly twice — once "+
			"in openSeatTx and once in closeSeatTx — found %d.\n\n"+
			"Fewer than two means one DIRECTION stopped signalling, which is round 8's P1 "+
			"verbatim: the removal side was wired and the rejoin side was not, so a "+
			"consumer's cached DENIAL kept agreeing with an epoch that never moved and a "+
			"valid returning member stayed locked out with no upper bound.", n)
	}
}

// TestSeatDoorsUseTheFunnel pins the other direction: the known doors call it.
//
// This list covers established seat doors. It detects those call sites being
// rewired away from the funnel, but does not discover arbitrary new SQL writers.
func TestSeatDoorsUseTheFunnel(t *testing.T) {
	doors := map[string][]string{
		"db_manager.go": {
			"openSeatTx(tx, spaceId, uid, nil)",                                          // upsertMembersOnce
			"closeSeatTx(tx, spaceId, uid, operatorUID, MemberRemoveReasonForceRemoved)", // removeMembersForceOnce
			"closeSeatTx(tx, spaceId, uid, operatorUID, reason)",                         // removeMemberLockedOnce
		},
		"db.go": {
			"openSeatTx(tx, spaceId, uid, &role)",           // reactivateMember
			"openSeatTx(tx, spaceId, uid, &roleCommon)",     // atomicReactivateMemberIfNotFullOnce
			"openSeatTx(tx, spaceId, row.UID, &roleCommon)", // approveJoinApplyAtomic
		},
		"member_removal_all_spaces.go": {
			"closeSeatTx(tx, spaceID, uid, operatorUID, reason)", // closeOneSeatAndEnqueueTx
		},
	}
	for file, wants := range doors {
		raw, err := os.ReadFile(filepath.Clean(file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		body := string(raw)
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s no longer contains %q.\n\n"+
					"If the call was renamed or its arguments changed, update this list. If a "+
					"door went back to writing space_member.status directly, it also stopped "+
					"resolving the canonical uid and stopped signalling — put it back through "+
					"the funnel.", file, want)
			}
		}
	}
}
