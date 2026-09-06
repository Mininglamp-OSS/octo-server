package project

// PR #841 review round 4 (yujiawei P1-1 / P1-2). The actor-Space-seat arms added to
// updateProjectHandler and disbandProjectHandler in round 3 have no `return`.
//
// Go's switch cases do not fall through to the next case — but they do fall OUT of the switch,
// so control resumes on the handler's SUCCESS path:
//
//	update:  toResp(nil) dereferences the model -> handler PANIC, after having written a
//	         project.update audit entry for a write that was refused and never reached the DB.
//	disband: no panic (a nil slice is fine), so instead it writes a project.disband audit entry
//	         with seats_closed=0 for a disband that was REFUSED, then calls c.ResponseOK() on
//	         top of the already-rendered error envelope — two JSON bodies under one 400.
//
// On a PR whose purpose is authorization correctness, an audit record claiming a destructive
// operation succeeded when it was denied is the worst of the three symptoms.
//
// The round-3 guard could not see any of it: `assert.Contains(body, "errActorNotSpaceMember")`
// checks that the arm EXISTS, not that it terminates. The round-3 mutation table records
// "delete the arm -> guard red" and nothing for "the arm is wrong", which is the same gap this
// PR ships a learning note about.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpdateRefusalDoesNotFallThroughIntoSuccess drives the real handler with the refusal
// injected at the service seam.
func TestUpdateRefusalDoesNotFallThroughIntoSuccess(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	rec := &auditRecorder{}
	p.auditSink = rec.sink
	r := mountProject(t, p)

	orig := p.updateFn
	t.Cleanup(func() { p.updateFn = orig })
	p.updateFn = func(projectID, actorUID, spaceID string, req updateReq) (*Model, error) {
		// Exactly what requireSpaceSeatsTx returns when the actor's Space seat has closed:
		// a nil model with the actor-level sentinel.
		return nil, errActorNotSpaceMember
	}

	w := doOn(t, r, http.MethodPut, "/v1/projects/"+created.ProjectID, ownerTok,
		map[string]any{"description": "should not land"})

	// The panic surfaces as a 500 through gin's recovery, so asserting the envelope is enough
	// to pin it — but assert the status explicitly too, since a recovered panic is the loudest
	// symptom.
	require.NotEqual(t, http.StatusInternalServerError, w.Code,
		"the handler must not panic on a refused update (nil model reaching toResp): body=%s",
		w.Body.String())
	assertProjectErrorCode(t, w, "err.server.project.actor_not_space_member")
	assert.Empty(t, rec.byAction(auditUpdate),
		"a refused update must not be audited as an update — the write never reached the database")
}

// TestDisbandRefusalDoesNotFallThroughIntoSuccess is the same defect on the disband handler,
// where the visible symptom is a fabricated audit entry rather than a panic.
func TestDisbandRefusalDoesNotFallThroughIntoSuccess(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	rec := &auditRecorder{}
	p.auditSink = rec.sink
	r := mountProject(t, p)

	orig := p.disbandFn
	t.Cleanup(func() { p.disbandFn = orig })
	p.disbandFn = func(projectID, actorUID, spaceID string) ([]string, error) {
		return nil, errActorNotSpaceMember
	}

	w := doOn(t, r, http.MethodDelete, "/v1/projects/"+created.ProjectID, ownerTok, nil)

	assertProjectErrorCode(t, w, "err.server.project.actor_not_space_member")
	assert.Empty(t, rec.byAction(auditDisband),
		"a REFUSED disband must never be audited as a disband — this is the finding I would "+
			"not merge past on a security_sensitive PR")
	assert.Equal(t, 1, countJSONBodies(w.Body.String()),
		"the refusal already rendered an envelope; falling through adds a second body "+
			"(ResponseOK) under the same status: %s", w.Body.String())

	// And the project must still be there.
	row, err := p.db.queryByProjectID(created.ProjectID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, StatusNormal, row.Status, "a refused disband must not have disbanded anything")
}

// countJSONBodies counts top-level JSON documents in a response body. Two concatenated objects
// are what a fall-through into ResponseOK produces.
func countJSONBodies(body string) int {
	depth, count := 0, 0
	inString, escaped := false, false
	for _, ch := range body {
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case inString:
			// skip
		case ch == '{' || ch == '[':
			if depth == 0 {
				count++
			}
			depth++
		case ch == '}' || ch == ']':
			depth--
		}
	}
	return count
}

// TestResetCursorsForTestCoversEveryCursorField guards the helper that keeps reconcile tests
// independent.
//
// It exists because the helper silently fell behind: the ownerless rotation added in round 3
// introduced two fields and neither was reset, so a truncated ownerless rotation could leak into
// the next case — with -shuffle=on, into an arbitrary one. Nothing failed, which is the problem.
func TestResetCursorsForTestCoversEveryCursorField(t *testing.T) {
	src := readLinesWithoutComments(t, "reconcile.go")
	// A struct ends at its own closing brace, not at the next `func` — funcBody would swallow
	// the `var cursors reconcileCursors` declaration that follows and read "var" as a field.
	structStart := strings.Index(src, "type reconcileCursors struct {")
	require.GreaterOrEqual(t, structStart, 0, "reconcileCursors must exist")
	structEnd := strings.Index(src[structStart:], "\n}")
	require.Positive(t, structEnd, "reconcileCursors must be a complete struct")
	structBody := src[structStart : structStart+structEnd]
	resetBody := funcBody(t, src, "func resetCursorsForTest(")

	fields := 0
	for _, line := range strings.Split(structBody, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "type ") || line == "}" {
			continue
		}
		name := strings.Fields(line)[0]
		if name == "mu" {
			continue // the mutex is taken, not reset
		}
		fields++
		assert.Contains(t, resetBody, "cursors."+name,
			"resetCursorsForTest does not reset %q. Every field of reconcileCursors must be "+
				"reset, or a truncated rotation leaks into the next test — and with -shuffle=on "+
				"that is an arbitrary test, which then passes or fails for a reason unrelated to "+
				"what it asserts.", name)
	}
	assert.GreaterOrEqual(t, fields, 8,
		"expected to inspect at least 8 cursor fields, saw %d — the struct parse probably broke, "+
			"which would make this guard vacuous", fields)
}
