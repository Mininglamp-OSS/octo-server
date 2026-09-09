package project

// PR #841, raised in BOTH review rounds and unchanged: every reconcile scan discards the
// progress it made within a tick when a later page query fails. The comment on the error
// return claims the opposite ("keep the cursor and running total; the next tick retries from
// here"), which is what makes it worth a test rather than a note — the code and its own
// documentation disagree.
//
// With a persistent failure at page N+1, every tick re-scans pages 1..N: the same Error-level
// alert lines are re-emitted for the prefix on every tick, and no row past the failure point
// is ever reached.

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestI1ScanKeepsItsCursorWhenAPageFails drives the real scan with page 1 succeeding and
// page 2 failing, then asserts the saved cursor reflects page 1 rather than the start.
func TestI1ScanKeepsItsCursorWhenAPageFails(t *testing.T) {
	srv, p := setup(t)
	_, _, created := projectWithMembers(t, srv, "k1", "k2")
	require.NotEmpty(t, created.ProjectID)

	// A fresh cursor for this case, restored afterwards so nothing leaks between cases.
	// i1Save(..., done=true) is the reset: it clears the position and the running total.
	cursors.i1Save("", "", 0, true)
	t.Cleanup(func() { cursors.i1Save("", "", 0, true) })

	// Page 1 returns one inspected row (a full page, so the loop continues); page 2 fails.
	p.cfg.ReconcileLimit = 1
	pageErr := errors.New("probe: page query failed")
	page := 0
	orig := p.i1PageFn
	t.Cleanup(func() { p.i1PageFn = orig })
	p.i1PageFn = func(cursorProject, cursorUID string, limit int) ([]*i1Row, error) {
		page++
		if page == 1 {
			return []*i1Row{{ProjectID: "proj-A", UID: "uid-A", SpaceID: spaceA, Violating: false}}, nil
		}
		return nil, pageErr
	}

	p.scanI1Violations()
	require.Equal(t, 2, page, "the scan must have attempted a second page")

	gotProject, gotUID, _ := cursors.i1Resume()
	assert.Equal(t, "proj-A", gotProject,
		"the cursor from the page that SUCCEEDED must survive the failure of the next one")
	assert.Equal(t, "uid-A", gotUID, "same for the composite half of the cursor")
}

// TestReconcileScansKeepProgressOnAPageError is the source guard for the other three scans.
//
// One behavioural test plus this guard, rather than four seams: the four page loops are
// structurally identical, and the defect is a single token — `return` where the function's own
// comment promises the cursor is kept. A bare return inside a page-error branch skips the
// matching *Save call at the end of the function, which is the ONLY place progress is
// persisted.
func TestReconcileScansKeepProgressOnAPageError(t *testing.T) {
	src := readLinesWithoutComments(t, "reconcile.go")
	for _, scan := range []string{
		"func (p *Project) scanI1Violations(",
		"func (p *Project) scanOrphanProjects(",
		"func (p *Project) scanEpochSanity(",
		"func (p *Project) scanAbandonedCleanupLeak(",
	} {
		body := funcBody(t, src, scan)
		// Locate the page-error branch: the Warn call that reports a failed page.
		wi := strings.Index(body, "p.Warn(")
		require.Positive(t, wi, "%s must report a failed page", scan)
		branch := body[wi:]
		if end := strings.Index(branch, "\n\t\t}"); end > 0 {
			branch = branch[:end]
		}
		assert.NotContains(t, branch, "return",
			"%s must BREAK out of the page loop on a page error, not return: a return skips the "+
				"cursor save at the end of the function, so every tick re-scans the prefix and "+
				"never reaches a row past the failure", scan)
	}
}
