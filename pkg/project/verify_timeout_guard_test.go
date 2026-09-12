package project

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Source guards for the two robustness properties ProjectMemberships' transaction
// depends on, neither of which any behavioural test in this repository can fail.
//
// Both were added as a fix for an unbounded wait and then left unpinned, which is
// the shape this branch has been burned by: a comment asserts a guarantee, the code
// happens to provide it, and the next refactor removes it silently. A reviewer asked
// for exactly this ("neither half of the new robustness change is pinned by a test").
//
// They are source guards rather than behavioural ones on purpose. Reproducing a
// pool-exhaustion stall needs a hostile engine and a controlled connection pool;
// asserting the two properties that prevent it costs a parse.

// funcBodyIn returns one function's source text, from its declaration to the next
// top-level closing brace. Coarse on purpose — it only has to be wide enough to see
// the statements each guard is about.
func funcBodyIn(t *testing.T, file, decl string) string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	src := string(raw)
	start := strings.Index(src, decl)
	if start < 0 {
		t.Fatalf("%s: %q not found — if it moved, point this guard at the new location "+
			"rather than deleting it; a guard that cannot find its subject passes for the "+
			"wrong reason", file, decl)
	}
	body := src[start:]
	if end := strings.Index(body, "\n}\n"); end > 0 {
		body = body[:end]
	}
	return body
}

// TestVerifyTransactionIsBoundedByTheCallerAndByAStatementDeadline pins the two
// halves together, because each without the other leaves a real hole:
//
//   - the caller's context bounds the WAIT for a pooled connection, which is where a
//     retrying peer at the configured burst parks goroutines on a pool octo-lib
//     defaults to 100. context.Background() there cannot be cancelled at all.
//   - tx.Timeout bounds each STATEMENT once the connection is held. A read that
//     blocks holds the connection AND the repeatable-read view, so the expensive
//     half of the hazard is downstream of acquiring the connection, not at it.
func TestVerifyTransactionIsBoundedByTheCallerAndByAStatementDeadline(t *testing.T) {
	body := funcBodyIn(t, "membership.go", "func ProjectMemberships(")

	if !strings.Contains(body, "session.BeginTx(ctx,") {
		t.Error("ProjectMemberships must open its transaction with the CALLER's context. " +
			"sql.DB.BeginTx is where the wait for a pooled connection happens, and with " +
			"context.Background() nothing bounds it — a peer that hangs up frees nothing " +
			"and a retrying one at burst can starve every other module in the process.")
	}
	if strings.Contains(body, "BeginTx(context.Background()") {
		t.Error("ProjectMemberships must not open its transaction with context.Background(): " +
			"that is the unbounded pool wait this guard exists to keep closed")
	}
	if !strings.Contains(body, "tx.Timeout = membershipReadTimeout") {
		t.Error("ProjectMemberships must set tx.Timeout. Without it the transaction inherits " +
			"the session's, which is unset process-wide — i.e. no statement deadline at all, " +
			"and a stuck read holds both the connection and the read view.")
	}
}

// TestVerifyTransactionReadsAreAllTimeoutBearing is the half the comment could not
// promise on its own.
//
// tx.Timeout reaches a statement only through dbr's query()/exec(), which Load and
// LoadOne go through. queryRows() — behind .Rows(), .Iterate() and .IterateContext()
// — DISCARDS the runner's timeout deliberately ("the context should not be canceled
// implicitly here", dbr.go). So "every read on this transaction is bounded" is a
// property of the call shape, not of dbr, and a future read added in the other shape
// would lose the deadline with nothing failing.
//
// The sweep covers the functions that actually receive this transaction: the two
// inside this package, plus pkg/space and pkg/user, which are handed it as a
// dbr.SessionRunner. Adding a fifth callee means adding it here — that is the point.
func TestVerifyTransactionReadsAreAllTimeoutBearing(t *testing.T) {
	// The unbounded shapes, spelled as dbr method calls rather than as words so a
	// rename shows up as a failing guard rather than as a stale comment.
	unbounded := regexp.MustCompile(`\)\.(Rows|Iterate|IterateContext)\(`)

	for _, subject := range []struct {
		file string
		decl string
	}{
		{"membership.go", "func ProjectMemberships("},
		{"membership.go", "func ProjectEpochsInSpace("},
		{"../space/membership.go", "func ActiveMembers("},
		{"../space/membership.go", "func IsActiveSpace("},
		{"../user/liveness.go", "func ActiveAccounts("},
	} {
		body := funcBodyIn(t, subject.file, subject.decl)
		if loc := unbounded.FindString(body); loc != "" {
			t.Errorf("%s %s issues a read through %q, which dbr executes WITHOUT the "+
				"runner's timeout. Inside ProjectMemberships' transaction that read is "+
				"unbounded: it holds the pooled connection and the repeatable-read view "+
				"with nothing to cut it off. Use Load or LoadOne, or bound it explicitly "+
				"with a context of its own.",
				subject.file, subject.decl, strings.Trim(loc, ")("))
		}
		// Not vacuous: a body with no read at all would pass the check above for the
		// wrong reason, which is how the sweep would die if one of these moved.
		if !strings.Contains(body, ".Load(") && !strings.Contains(body, ".LoadOne(") {
			t.Errorf("%s %s issues no Load/LoadOne read; either it stopped being part of "+
				"the verify transaction (drop it from this list) or the parse stopped "+
				"finding its body (fix the parse)", subject.file, subject.decl)
		}
	}
}
