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
		assert.Contains(t, body, "errActorNotSpaceMember",
			"%s has no arm for errActorNotSpaceMember. Every write path now returns it (one "+
				"helper takes all the seat locks), so without an arm an authorization refusal "+
				"falls to default and renders as store_failed — Internal, HTTP 500 — which "+
				"inflates the 5xx budget and mislabels write_rejected_total.", handler)
	}
}
