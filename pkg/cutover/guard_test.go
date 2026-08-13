package cutover

import "testing"

// The guard's three-state semantics were review findings in both domains that
// previously hand-rolled them (#697 P1-4: a typo'd value silently disarmed the
// guard), so the decision tree is pinned exhaustively here.

func TestParseExpectedMode(t *testing.T) {
	values := map[string]int{"legacy": ModeInactive, "incr": ModeActive}
	cases := []struct {
		name      string
		raw       string
		set       bool
		malformed bool
		mode      int
	}{
		{name: "unset makes no assertion", raw: "", set: false},
		{name: "whitespace-only is unset", raw: "  \n", set: false},
		{name: "valid inactive spelling", raw: "legacy", set: true, mode: ModeInactive},
		{name: "valid active spelling", raw: "incr", set: true, mode: ModeActive},
		{name: "trailing newline from a ConfigMap is trimmed", raw: "incr\n", set: true, mode: ModeActive},
		{name: "typo fails closed, not open", raw: "inrc", set: true, malformed: true},
		{name: "unknown value fails closed", raw: "transactional", set: true, malformed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := ParseExpectedMode(tc.raw, values)
			if g.IsSet() != tc.set {
				t.Fatalf("IsSet()=%v want %v", g.IsSet(), tc.set)
			}
			if g.IsMalformed() != tc.malformed {
				t.Fatalf("IsMalformed()=%v want %v", g.IsMalformed(), tc.malformed)
			}
			mode, ok := g.Expected()
			wantOK := tc.set && !tc.malformed
			if ok != wantOK {
				t.Fatalf("Expected() ok=%v want %v", ok, wantOK)
			}
			if ok && mode != tc.mode {
				t.Fatalf("Expected() mode=%d want %d", mode, tc.mode)
			}
		})
	}
}

func TestExpectedModeCheck(t *testing.T) {
	values := map[string]int{"legacy": ModeInactive, "active": ModeActive}
	cases := []struct {
		name     string
		raw      string
		resolved int
		want     GuardVerdict
	}{
		{name: "unset passes inactive", raw: "", resolved: ModeInactive, want: GuardOK},
		{name: "unset passes active", raw: "", resolved: ModeActive, want: GuardOK},
		{name: "match passes", raw: "active", resolved: ModeActive, want: GuardOK},
		{name: "mismatch fails closed", raw: "active", resolved: ModeInactive, want: GuardMismatch},
		{name: "reverse mismatch fails closed", raw: "legacy", resolved: ModeActive, want: GuardMismatch},
		// A malformed guard must refuse every resolution: it can neither
		// assert nor stand aside, and silently standing aside is the defect
		// the malformed state exists to prevent.
		{name: "malformed fails closed on inactive", raw: "atcive", resolved: ModeInactive, want: GuardMalformed},
		{name: "malformed fails closed on active", raw: "atcive", resolved: ModeActive, want: GuardMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseExpectedMode(tc.raw, values).Check(tc.resolved); got != tc.want {
				t.Fatalf("Check(%d)=%v want %v", tc.resolved, got, tc.want)
			}
		})
	}
}
