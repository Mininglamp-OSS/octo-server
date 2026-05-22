// Package bot_api — YUJ-1752 / octo-server PR#131 R7 regression tests.
//
// Bug: setGrantActive's activate path and createOrReactivateGrantAtomic
// acquired their row locks in opposite orders:
//
//   - setGrantActive (pre-fix): `obo_grants` FOR UPDATE → `user` FOR UPDATE
//   - createOrReactivateGrantAtomic: `user` FOR UPDATE → `obo_grants` FOR UPDATE
//
// Concurrent PUT /v1/obo/grants/:id {active:1} and POST /v1/obo/grants
// on the SAME grantor could AB-BA deadlock: the PUT held the grant row
// and waited for the user row; the POST held the user row and waited
// for the grant row via its demote-others FOR UPDATE scan. MySQL's
// deadlock detector eventually broke the cycle, but the loser's
// request failed.
//
// Fix: setGrantActive's activate path was reshaped to acquire
//
//	`user` row → `obo_grants` row
//
// matching createOrReactivateGrantAtomic. A leading unlocked SELECT
// resolves grantor_uid (immutable for an existing row, UNIQUE key
// covers it) so the user lock can be taken FIRST, then the grant
// row is re-read FOR UPDATE inside the same tx for the authoritative
// snapshot.
//
// These tests pin the invariant at the source level — a static scan of
// obo_db.go that asserts both functions take the `user` FOR UPDATE
// before any FOR UPDATE on `obo_grants`. Source-level checks are the
// right tool here because the fakeOBOStore used elsewhere in the bot_api
// suite does not model SQL row locks, so a behavioral test could only
// reproduce the original deadlock against a real MySQL instance.
// The static check is the cheapest, most reliable barrier against a
// future refactor silently reintroducing the AB-BA pattern.
package bot_api

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// readOBODBSource loads modules/bot_api/obo_db.go once per test. Tests
// run from the package directory, so the relative path is stable.
func readOBODBSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("obo_db.go")
	if err != nil {
		t.Fatalf("read obo_db.go: %v", err)
	}
	return string(b)
}

// extractFuncBody returns the body of the named method on *botAPIDB —
// everything between the opening `{` of the function signature and the
// matching closing `}`. Bracket-counting is good enough for the lock-
// order check; we are not parsing Go semantically, only scanning for
// SQL substrings inside a single function.
func extractFuncBody(t *testing.T, src, funcSig string) string {
	t.Helper()
	idx := strings.Index(src, funcSig)
	if idx < 0 {
		t.Fatalf("function not found: %q", funcSig)
	}
	open := strings.IndexByte(src[idx:], '{')
	if open < 0 {
		t.Fatalf("opening brace not found after %q", funcSig)
	}
	start := idx + open
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("unterminated function body for %q", funcSig)
	return ""
}

// userForUpdatePattern matches the user-row lock used by both
// setGrantActive and createOrReactivateGrantAtomic. Whitespace is
// flexible so the test survives gofmt drift; backticks are escaped
// in the Go raw-string by using `+"`"+` concatenation.
var userForUpdatePattern = regexp.MustCompile(
	"SELECT\\s+1\\s+FROM\\s+`user`\\s+WHERE\\s+uid=\\?\\s+FOR\\s+UPDATE",
)

// oboGrantForUpdatePattern matches any FOR UPDATE clause on the
// obo_grants table. Both the dbr builder form (`.Suffix("FOR UPDATE")`
// chained off `.From("obo_grants")`) and raw-SQL form
// (`FROM obo_grants ... FOR UPDATE`) are covered by checking for the
// table name followed by FOR UPDATE within a window, plus the dbr
// builder pattern explicitly. Two separate patterns make failure
// messages precise about which form tripped the assertion.
var (
	oboGrantsRawForUpdatePattern = regexp.MustCompile(
		"obo_grants[^;}\\n]{0,400}FOR\\s+UPDATE",
	)
	oboGrantsBuilderForUpdatePattern = regexp.MustCompile(
		`From\("obo_grants"\)[\s\S]{0,500}?Suffix\("FOR UPDATE"\)`,
	)
)

// firstOBOGrantsForUpdate returns the earliest offset at which a
// FOR UPDATE lock on obo_grants appears in body, or -1 if none.
func firstOBOGrantsForUpdate(body string) int {
	best := -1
	if loc := oboGrantsRawForUpdatePattern.FindStringIndex(body); loc != nil {
		best = loc[0]
	}
	if loc := oboGrantsBuilderForUpdatePattern.FindStringIndex(body); loc != nil {
		if best < 0 || loc[0] < best {
			best = loc[0]
		}
	}
	return best
}

// assertLockOrder enforces: every FOR UPDATE on `user` (uid=?) in body
// MUST appear strictly before the FIRST FOR UPDATE on obo_grants.
// `funcName` is used only for error messages.
func assertLockOrder(t *testing.T, funcName, body string) {
	t.Helper()
	userLocks := userForUpdatePattern.FindAllStringIndex(body, -1)
	grantLockIdx := firstOBOGrantsForUpdate(body)

	if len(userLocks) == 0 && grantLockIdx < 0 {
		// Function takes no row locks at all — vacuously fine.
		return
	}
	if grantLockIdx < 0 {
		// Only user lock present — also fine.
		return
	}
	if len(userLocks) == 0 {
		t.Fatalf(
			"%s acquires `obo_grants` FOR UPDATE at offset %d but never "+
				"takes the grantor `user` FOR UPDATE lock — lock-order "+
				"invariant requires user → obo_grants (YUJ-1752 / PR#131 R7).",
			funcName, grantLockIdx,
		)
	}

	firstUserLock := userLocks[0][0]
	if firstUserLock >= grantLockIdx {
		t.Fatalf(
			"%s lock-order invariant violated (YUJ-1752 / PR#131 R7): "+
				"first `user` FOR UPDATE at offset %d but `obo_grants` "+
				"FOR UPDATE appears earlier at offset %d. "+
				"Concurrent PUT /v1/obo/grants/:id {active:1} and POST "+
				"/v1/obo/grants on the same grantor will AB-BA deadlock. "+
				"Required order: SELECT 1 FROM `user` WHERE uid=? FOR UPDATE "+
				"MUST run BEFORE any FOR UPDATE on obo_grants.",
			funcName, firstUserLock, grantLockIdx,
		)
	}
}

// TestOBO_YUJ1752_LockOrder_SetGrantActive — pins that
// setGrantActive's activate path locks the grantor `user` row
// BEFORE acquiring any FOR UPDATE on obo_grants.
func TestOBO_YUJ1752_LockOrder_SetGrantActive(t *testing.T) {
	src := readOBODBSource(t)
	body := extractFuncBody(t, src, "func (d *botAPIDB) setGrantActive(")
	assertLockOrder(t, "setGrantActive", body)
}

// TestOBO_YUJ1752_LockOrder_CreateOrReactivateGrantAtomic — pins
// that createOrReactivateGrantAtomic also follows user → obo_grants.
// This is the function setGrantActive is required to MIRROR; if this
// one ever flips, the invariant must be re-evaluated for both sides.
func TestOBO_YUJ1752_LockOrder_CreateOrReactivateGrantAtomic(t *testing.T) {
	src := readOBODBSource(t)
	body := extractFuncBody(t, src, "func (d *botAPIDB) createOrReactivateGrantAtomic(")
	assertLockOrder(t, "createOrReactivateGrantAtomic", body)
}

// TestOBO_YUJ1752_LockOrderInvariantMarkerPresent — guard against
// silent removal of the source-level invariant comment. If a future
// refactor sweeps the comment, this test fails and forces the author
// to think about whether the invariant itself still holds before
// deleting the marker.
func TestOBO_YUJ1752_LockOrderInvariantMarkerPresent(t *testing.T) {
	src := readOBODBSource(t)
	// Both functions should carry the marker; count to make sure
	// neither side dropped it.
	const marker = "LOCK ORDER INVARIANT"
	n := strings.Count(src, marker)
	if n < 2 {
		t.Fatalf("expected the %q comment to appear in BOTH setGrantActive "+
			"and createOrReactivateGrantAtomic (>=2 occurrences in obo_db.go); "+
			"found %d. Re-add the marker rather than silently dropping it — "+
			"see YUJ-1752 / PR#131 R7 for context.", marker, n)
	}
}
