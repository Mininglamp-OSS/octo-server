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
	"regexp"
	"strings"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"go.uber.org/zap"

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

// ---------- acceptance #31: the metric's entry breakdown, behaviourally ----------

// TestWriteRejectionsAreBrokenDownByEntryPoint is the behavioural evidence for the acceptance
// criterion "admission metrics are split per entry point".
//
// It exists because verification.md cited two tests for that criterion which were never in the
// tree (PR #841 round 4, S-2). The `entry` label did exist, and a source guard pinned the label
// set — but nothing asserted that two different write paths actually land on DIFFERENT label
// values, which is the whole property: the brief calls this breakdown "the signal that exposes a
// write path that skipped invariant I1", and a single undifferentiated counter cannot expose it.
func TestWriteRejectionsAreBrokenDownByEntryPoint(t *testing.T) {
	srv, p := setup(t)
	ownerTok, _, created := projectWithMembers(t, srv)
	r := mountProject(t, p)

	before := func(entry, reason string) float64 {
		return promtestutil.ToFloat64(writeRejected.WithLabelValues(entry, reason))
	}
	addBefore := before(entryMemberAdd, reasonNotSpaceMember)
	createBefore := before(entryCreateOwner, reasonNotSpaceMember)

	// Path 1 — member add, target holds no Space seat.
	seedUser(t, "e31a")
	w := doOn(t, r, http.MethodPost, "/v1/projects/"+created.ProjectID+"/members/add",
		ownerTok, map[string]any{"uids": []string{"e31a"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	assert.Equal(t, addBefore+1, before(entryMemberAdd, reasonNotSpaceMember),
		"the add rejection must be counted under its own entry point")
	assert.Equal(t, createBefore, before(entryCreateOwner, reasonNotSpaceMember),
		"and must NOT be attributed to the create path — an undifferentiated counter cannot "+
			"reveal which write path skipped I1, which is the reason this label exists")

	// Path 2 — create, the creator's Space seat is gone by write time. Same reason, different
	// entry: the two must move independently.
	seedSpace(t, spaceB, 1)
	tok := seedUser(t, "e31b")
	seedSpaceMember(t, spaceB, "e31b", 0, 1)
	wr := doOn(t, r, http.MethodGet, "/v1/space/"+spaceB+"/projects", tok, nil)
	require.Equal(t, http.StatusOK, wr.Code, "warm the Space cache: %s", wr.Body.String())
	removeSpaceMember(t, spaceB, "e31b")

	createBefore2 := before(entryCreateOwner, reasonNotSpaceMember)
	addBefore2 := before(entryMemberAdd, reasonNotSpaceMember)
	w = doOn(t, r, http.MethodPost, "/v1/space/"+spaceB+"/projects", tok,
		map[string]any{"name": "should-not-exist"})
	assertProjectErrorCode(t, w, "err.server.project.actor_not_space_member")

	assert.Equal(t, createBefore2+1, before(entryCreateOwner, reasonNotSpaceMember),
		"the create-owner rejection must be counted under entryCreateOwner")
	assert.Equal(t, addBefore2, before(entryMemberAdd, reasonNotSpaceMember),
		"and must not disturb the add path's counter")
}

// ---------- P2-4: transfer_to is validated even when no transfer can happen ----------

// TestLeaveIgnoresAnIrrelevantTransferTo pins that a successor is only required to hold a Space
// seat when a transfer will actually happen.
//
// requireSpaceSeatsTx refuses any named uid without an active Space seat, and leave passed
// transferTo unconditionally — before the code has established that a transfer is needed at all.
// So an ordinary member, or an owner who is not the last one, sending
// {"transfer_to": "<a colleague who left the Space last month>"} was refused for a successor
// irrelevant to their departure (PR #841 round 4, P2-4).
func TestLeaveIgnoresAnIrrelevantTransferTo(t *testing.T) {
	srv, p := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "p24a", "p24b")
	pid := created.ProjectID
	_ = ownerTok

	// p24b left the Space last month: a real project seat, no Space seat.
	removeSpaceMember(t, spaceA, "p24b")

	// p24a is an ordinary member. Their departure needs no transfer at all, so naming a
	// seatless successor must not refuse it.
	successor, err := p.leaveProject(pid, spaceA, "p24a", "p24b")
	assert.NoError(t, err,
		"an ordinary member's leave needs no transfer, so an irrelevant transfer_to must not "+
			"block it")
	assert.Empty(t, successor, "and nobody was promoted")

	seat, qErr := p.db.queryMember(pid, "p24a")
	require.NoError(t, qErr)
	require.NotNil(t, seat)
	assert.Equal(t, MemberStatusRemoved, seat.Status, "the leave must have taken effect")

	// The last owner IS required to name a successor who still holds a Space seat — the
	// protection must stay for the case it was written for.
	_, lastErr := p.leaveProject(pid, spaceA, "owner1", "p24b")
	assert.ErrorIs(t, lastErr, errNotSpaceMember,
		"the LAST owner's transfer target must still be an active Space member, or the cascade "+
			"closing that seat leaves the project ownerless")
	_ = tokens
}

// ---------- P2-2: the per-row alert log must be capped ----------

// TestReconcileAlertLogIsCappedPerTick pins the bound on per-row Error lines.
//
// Uncapped, each scan emits one line per violating row — up to
// reconcileMaxPages * ReconcileLimit = 25,000 per tick per pod, every interval, on every replica.
// Fine for a transient; wrong for the states this module documents as standing figures needing a
// human. Nothing disbands the projects of a disbanded Space, so one legitimate operator action
// produces N orphan lines per rotation forever (PR #841 round 4, P2-2).
func TestReconcileAlertLogIsCappedPerTick(t *testing.T) {
	_, p := setup(t)

	// Count REAL emissions, not the rows seen. Asserting on the row counter cannot detect a
	// missing cap — it increments unconditionally, before the comparison — which is why the first
	// version of this test survived a mutation that neutralised the cap entirely (round 5, P2-2).
	emitted := 0
	l := &logCapped{
		p:    p,
		scan: "probe",
		emit: func(msg string, fields ...zap.Field) { emitted++ },
	}
	rows := reconcileLogCap * 3
	for i := 0; i < rows; i++ {
		l.errorf("probe")
	}

	assert.Equal(t, reconcileLogCap+1, emitted,
		"exactly %d detail lines plus ONE summary may be emitted for %d violating rows; got %d "+
			"emissions. Uncapped this is up to reconcileMaxPages*ReconcileLimit = 25,000 lines "+
			"per tick per pod, forever, for states this module documents as needing a human — and "+
			"an alert that only ratchets up gets muted.", reconcileLogCap, rows, emitted)
	assert.Equal(t, rows, l.seen,
		"every violating row must still be COUNTED — the cap is on the log, not on the gauge")

	// And the source must route every per-row alert through the cap, not around it.
	src := readLinesWithoutComments(t, "reconcile.go")
	for _, scan := range scanFuncNames(src) {
		body := scanFuncBody(src, scan)
		if !strings.Contains(body, "log.errorf(") && !strings.Contains(body, "logCapped") {
			continue // a scan with no per-row alert is fine
		}
		// A word boundary, NOT a substring: "zap.Error(err)" contains "p.Error(" — the exact
		// class of false match the round-4 review criticised in this guard family, met here on
		// the first try.
		direct := regexp.MustCompile(`\bp\.Error\(`)
		assert.NotRegexp(t, direct, body,
			"%s emits a per-row alert with p.Error, bypassing the per-tick cap. At the "+
				"1000-projects-per-Space quota one disbanded Space would log 1000 lines every "+
				"five minutes, indefinitely, and an alert that only ratchets up gets muted.", scan)
	}
}
