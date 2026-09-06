package project

// PR #841 review round 3 (both reviewers, P2): the actor/target classification introduced in
// round 2 was wired on 2 of the 6 privileged writes. On the other four, an actor whose own Space
// seat has closed gets:
//
//	PUT /:project_id     -> no matching arm -> default -> store_failed, Internal, HTTP 500
//	DELETE /:project_id  -> same
//	leave / role change   -> 403 whose message and metric blame the TARGET or SUCCESSOR
//
// An authorization refusal rendered as an Internal 500 also inflates the 5xx budget and
// mislabels write_rejected_total, which the brief calls "the signal that exposes a skipped I1
// check in P1".

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEveryWritePathNamesTheActorsOwnMissingSpaceSeat drives all six privileged writes at the
// service layer with an actor holding a project seat and no Space seat, and requires the
// ACTOR-level sentinel from each — the classification, not just the refusal.
func TestEveryWritePathNamesTheActorsOwnMissingSpaceSeat(t *testing.T) {
	srv, p := setup(t)
	ownerTok, tokens, created := projectWithMembers(t, srv, "ac9")
	_ = ownerTok
	pid := created.ProjectID

	w := doJSON(t, srv, http.MethodPut, "/v1/projects/"+pid+"/members/ac9/role",
		tokens["owner1"], map[string]any{"role": RoleAdmin})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	removeSpaceMember(t, spaceA, "ac9")

	seedUser(t, "acT")
	seedSpaceMember(t, spaceA, "acT", 0, 1)

	desc := "nope"
	_, uErr := p.updateProject(pid, "ac9", spaceA, updateReq{Description: &desc})
	assert.ErrorIs(t, uErr, errActorNotSpaceMember, "updateProject must name the ACTOR's seat")

	_, dErr := p.disbandProject(pid, "ac9", spaceA)
	assert.ErrorIs(t, dErr, errActorNotSpaceMember, "disbandProject must name the ACTOR's seat")

	_, rErr := p.removeMember(pid, spaceA, "ac9", "owner1")
	assert.ErrorIs(t, rErr, errActorNotSpaceMember, "removeMember must name the ACTOR's seat")

	_, lErr := p.leaveProject(pid, spaceA, "ac9", "")
	assert.ErrorIs(t, lErr, errActorNotSpaceMember, "leaveProject must name the ACTOR's seat")

	_, _, cErr := p.changeMemberRole(pid, spaceA, "ac9", "owner1", RoleCommon, "")
	assert.ErrorIs(t, cErr, errActorNotSpaceMember, "changeMemberRole must name the ACTOR's seat")

	_, aErr := p.addOneMember(pid, spaceA, "ac9", "acT")
	assert.ErrorIs(t, aErr, errActorNotSpaceMember, "addOneMember must name the ACTOR's seat")

	// And every one of them still satisfies the broader errors.Is, so existing consumers hold.
	for _, err := range []error{uErr, dErr, rErr, lErr, cErr, aErr} {
		assert.ErrorIs(t, err, errNotSpaceMember,
			"the actor sentinel WRAPS the target one; existing errors.Is checks must keep holding")
	}
}

// TestEveryWriteHandlerClassifiesTheActorSeatSentinel is the wire half, and it is a SOURCE
// guard rather than a behavioural test — deliberately, with the reason recorded because the
// absence of a behavioural test is usually a smell.
//
// In steady state this rendering is unreachable: projectMiddleware resolves Space membership
// with a LIVE, uncached MemberRole read, so a seatless actor gets the anti-enumeration 404
// before any handler runs. The path is reachable only through the middleware-to-transaction
// race window — the actor's seat closing between the middleware's read and the transaction's.
// That window cannot be opened from a test without a seam on each handler, and adding two seams
// to render one error code is a worse trade than guarding the switch.
//
// It still has to be fixed: fail-closed is not the same as correctly labelled. Without an arm,
// an authorization refusal falls to `default` and renders as store_failed — Internal, HTTP 500,
// logged at Error — which inflates the 5xx budget and mislabels write_rejected_total, the signal
// the brief names as the one that exposes a skipped I1 check in P1.
func TestEveryWriteHandlerClassifiesTheActorSeatSentinel(t *testing.T) {
	src := readLinesWithoutComments(t, "api.go") + readLinesWithoutComments(t, "api_member.go")
	for _, handler := range []string{
		"func (p *Project) updateProjectHandler(",
		"func (p *Project) disbandProjectHandler(",
		"func (p *Project) leaveProjectHandler(",
		"func (p *Project) updateMemberRoleHandler(",
		"func (p *Project) addMembersHandler(",
		"func (p *Project) removeMembersHandler(",
	} {
		body := funcBody(t, src, handler)
		require.Contains(t, body, "errActorNotSpaceMember",
			"%s has no arm for errActorNotSpaceMember. Every write path now returns it (one "+
				"helper takes all the seat locks), so without an arm an authorization refusal "+
				"falls to default and renders as store_failed — Internal, HTTP 500 — which "+
				"inflates the 5xx budget and mislabels write_rejected_total.", handler)

		// Existence is not enough, and this is the round-4 lesson: BOTH single-shot handlers
		// had the arm and neither terminated it, so control fell out of the switch onto the
		// success path — one panicked on a nil model, the other audited a refused disband.
		// A substring check for the sentinel cannot see that.
		assertArmDoesNotReachTheSuccessPath(t, handler, body, "errActorNotSpaceMember")
	}
}

// assertArmDoesNotReachTheSuccessPath requires the switch arm matching sentinel to terminate
// WHEN the switch has code after it.
//
// The judgement is deliberately conditional, because "every arm must return" is not this
// module's style and would be a false alarm: leaveProjectHandler and updateMemberRoleHandler
// end WITH their switch, so an arm falling out of it just ends the function — every arm there
// omits the return. The defect is specific to a switch that is followed by the handler's
// SUCCESS path, which is exactly where round 4 found it:
//
//	update:  toResp(nil) -> panic, after auditing a write that never happened
//	disband: audits a REFUSED disband, then ResponseOK on top of the error envelope
//
// Batch handlers are excluded for the opposite reason: their switch sits inside the per-target
// loop, where NOT terminating means "carry on to the next uid" and is the intended behaviour.
// Their actor-level arms are covered behaviourally instead (review5_blocker_test.go).
func assertArmDoesNotReachTheSuccessPath(t *testing.T, where, body, sentinel string) {
	t.Helper()
	if strings.Contains(body[:strings.Index(body, "switch")], "for ") {
		return // batch handler: the switch is inside the per-target loop
	}
	// Does the switch have code after it? The switch closes at the first "\n\t}" line.
	swStart := strings.Index(body, "\tswitch ")
	require.GreaterOrEqual(t, swStart, 0, "%s: expected a switch", where)
	swEnd := strings.Index(body[swStart:], "\n\t}")
	require.GreaterOrEqual(t, swEnd, 0, "%s: could not find the end of the switch", where)
	after := strings.TrimSpace(body[swStart+swEnd+len("\n\t}"):])
	if after == "" || after == "}" {
		return // the switch ends the function; falling out of it is harmless and idiomatic here
	}

	// It does have a success path after it, so the arm MUST terminate.
	i := strings.Index(body, sentinel)
	require.GreaterOrEqual(t, i, 0, "%s: no arm for %s", where, sentinel)
	rest := body[i:]
	end := len(rest)
	for _, term := range []string{"\n\tcase ", "\n\tdefault:"} {
		if k := strings.Index(rest, term); k >= 0 && k < end {
			end = k
		}
	}
	arm := rest[:end]
	assert.Contains(t, arm, "return",
		"%s: the %s arm does not terminate, and this switch is followed by the handler's "+
			"SUCCESS path. Go switch cases do not fall through to the next case, but they DO "+
			"fall out of the switch — round 4 found exactly this: one handler panicked on a nil "+
			"model, the other audited a REFUSED disband and then called ResponseOK on top of the "+
			"rendered error envelope. Arm:\n%s", where, sentinel, arm)
}
