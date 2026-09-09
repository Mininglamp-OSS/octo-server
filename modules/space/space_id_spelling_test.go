package space

// The WRITE side of the identity rule.
//
// seat_identity_collation_test.go pins that a seat TRANSITION hands its tx step the
// bytes `space_member` stores. That is only worth anything if those stored bytes are
// themselves the Space's — and they were not: every handler here validated the Space
// with `c.Param("space_id")` and then INSERTED that same parameter into space_member
// and space_invitation.
//
// `space` and `space_member` are utf8mb4_0900_ai_ci in production while octo_* are
// pinned utf8mb4_general_ci. So a drifted space_id (fullwidth forms, KELVIN, the
// ordinal letters — every ASCII character an id can contain has one) passes the
// activeness check, gets stored on the seat, and is afterwards invisible to the
// project-side enumeration the membership epoch depends on. The seat funnel then
// faithfully propagates poisoned bytes: member_epoch freezes while membership moves,
// and the async cascade completes as a successful no-op. A stale GRANT survives.
//
// Two pins, and neither is sufficient alone:
//
//   - the helper returns the row's spelling (below), and
//   - every caller rebinds to it (the source guard below that).
//
// A helper that returns the right value and a caller that drops it is exactly the
// shape of the four preceding instances of this class on this branch.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsSpaceActiveReturnsTheStoredSpelling drives the helper with a drifted spelling.
//
// ASCII case is the probe because the shared test schema is all utf8mb4_general_ci,
// where that is the drift that still matches. Under production's split the reachable
// class is wider — the assertion is that the BYTES come from the row, not that some
// collation bridges them.
func TestIsSpaceActiveReturnsTheStoredSpelling(t *testing.T) {
	_, _, err := setup(t)
	require.NoError(t, err)

	const stored = "sidSpellingProbe"
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status, created_at, updated_at) "+
			"VALUES (?, ?, 'probeop', 1, NOW(), NOW())", stored, stored).Exec()
	require.NoError(t, err)

	drifted := strings.ToUpper(stored)
	require.NotEqual(t, stored, drifted)

	got, active, err := testSpaceDB.isSpaceActive(drifted)
	require.NoError(t, err)
	require.True(t, active,
		"the drifted spelling must still resolve the Space — this check has always matched "+
			"under the row's collation, and narrowing it would be a behaviour change, not a fix")
	assert.Equal(t, stored, got,
		"isSpaceActive must return the space_id the ROW holds. Its callers insert this value "+
			"into space_member and space_invitation, and a seat stored under the caller's "+
			"spelling is unreachable from the project-side epoch enumeration, which compares "+
			"under a stricter collation.")

	_, active, err = testSpaceDB.isSpaceActive("sidSpellingProbeMissing")
	require.NoError(t, err)
	assert.False(t, active, "a Space that does not exist must still refuse")
}

// checkSpaceActiveCall matches a call and captures what it is bound to.
var checkSpaceActiveCall = regexp.MustCompile(`(?m)^(.*)s\.checkSpaceActive\(c, `)

// TestEverySpaceActiveCheckRebindsTheSpaceID is the half the unit test above cannot
// reach: a helper that returns the canonical spelling is worth nothing if a caller
// keeps using its own.
//
// It requires every call to be bound as `spaceId, <flag> := s.checkSpaceActive(...)`.
// Discarding the first value with `_` fails, which is the specific regression this
// guards — it compiles, it passes every behavioural test, and it silently restores the
// defect for that one handler.
func TestEverySpaceActiveCheckRebindsTheSpaceID(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	calls := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, readErr := os.ReadFile(filepath.Clean(name))
		require.NoError(t, readErr)
		body := string(raw)
		// The definition itself is not a call.
		for _, m := range checkSpaceActiveCall.FindAllStringSubmatch(body, -1) {
			prefix := strings.TrimSpace(m[1])
			calls++
			assert.Truef(t, strings.HasPrefix(prefix, "spaceId,"),
				"%s: every checkSpaceActive call must rebind spaceId to the returned spelling, "+
					"as `spaceId, refused := s.checkSpaceActive(c, spaceId)`. Found %q.\n\n"+
					"Dropping it with `_` compiles and passes every behavioural test while that "+
					"handler goes on inserting the caller's URL parameter into space_member — "+
					"which is the defect this returns the value to prevent.", name, prefix)
		}
	}
	require.GreaterOrEqual(t, calls, 12,
		"the sweep found %d checkSpaceActive calls; there were 12 when this was written. A "+
			"lower count means the guard stopped matching them, and a guard that matches "+
			"nothing reports every caller as correct", calls)
}
