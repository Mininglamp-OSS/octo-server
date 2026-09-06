package user

import (
	"errors"
	"fmt"

	projectpkg "github.com/Mininglamp-OSS/octo-server/pkg/project"
)

// The Project half of POST /v1/auth/verify?include=context (D11).
//
// # Why this ships now rather than waiting for a consumer
//
// P1 is where a project seat first MEANS something: before it, a seat grants no
// group, no channel and no message, so a subsystem asking "is X in project P"
// gets an answer with no consequence attached. After it, the answer is a live
// authorization fact — and the moment it is, every service that needs it will
// improvise a predicate out of whatever octo-server happens to expose. That is
// not hypothetical: a subsystem shipped a Space gate built out of the HTTP
// STATUS CODE of GET /v1/space/:space_id, fail-open when no token was present.
// Shipping a permission model without the one authoritative way to read it is
// what produces those.
//
// "Wait for a consumer" is also a deadlock — they cannot integrate against an
// endpoint that does not exist — and it does not apply here anyway: this is an
// additive field on an endpoint the gateway already calls on every request. No
// new route, no new secret, no new attack surface, no new rate-limit dimension.
// The things that DO carry rot risk (a new internal route with a per-consumer
// secret and an unestimatable QPS, replica machinery nobody drives) stay out.
//
// # What a consumer may do with the answer
//
// ONLY narrow. A consumer must keep its existing Space check and layer this on
// top, because the Space→project cascade is asynchronous: between a Space
// removal committing and the cascade running, the project row still exists. The
// Space half fails first, so the conjunction is safe and the disjunction is not.

// maxVerifyProjectIDs bounds one request structurally, on top of any byte cap.
//
// 50 is generous for the intended shape — a caller names the projects on the
// resource rows it is serving — and small enough that the lookup stays one
// bounded query rather than something worth paging.
const maxVerifyProjectIDs = 50

var errTooManyProjectIDs = errors.New("user: too many project_ids")

// answerProjectMembership answers exactly the project ids asked about, in the
// order given.
//
// Exactly N answers for N ids, never more: the response never mentions a project
// the caller did not name, so it cannot be used to enumerate someone's projects.
//
// A project outside space_id, a disbanded one and one that does not exist are
// all answered `member: false` rather than rejected. Rejecting would turn the
// endpoint into a way to probe which Space a project lives in — the caller
// learns the difference between "wrong Space" and "not a member", which is
// exactly the fact being protected.
func (u *User) answerProjectMembership(uid, spaceID string, projectIDs []string) ([]verifyProjectAnswer, error) {
	if len(projectIDs) > maxVerifyProjectIDs {
		return nil, errTooManyProjectIDs
	}

	// One bounded query for the whole batch, not one per id (C9: this endpoint
	// runs on every request of every subsystem that fronts octo-server).
	rows, err := projectpkg.MembershipsInSpace(u.ctx.DB(), spaceID, uid, projectIDs)
	if err != nil {
		return nil, fmt.Errorf("user: project membership lookup: %w", err)
	}

	answers := make([]verifyProjectAnswer, 0, len(projectIDs))
	seen := make(map[string]bool, len(projectIDs))
	for _, pid := range projectIDs {
		if pid == "" || seen[pid] {
			continue
		}
		seen[pid] = true
		row, ok := rows[pid]
		if !ok {
			// Not a member, no such project, or another Space — one answer,
			// carrying nothing else. The distinguishing reason is not logged per
			// id either: at gateway volume that would be a log line per request.
			answers = append(answers, verifyProjectAnswer{ProjectID: pid, Member: false})
			continue
		}
		role := row.Role
		epoch := row.MemberEpoch
		answers = append(answers, verifyProjectAnswer{
			ProjectID:    pid,
			Member:       true,
			Role:         &role,
			Capabilities: projectCapabilitiesForRole(row.Role),
			MemberEpoch:  &epoch,
		})
	}
	return answers, nil
}

// projectCapabilitiesForRole maps a project role to the explicit capability
// list a consumer should act on.
//
// Emitted rather than left to the consumer BECAUSE a consumer that derives
// permissions from a role number re-implements this repository's permission
// matrix — and drifts from it the first time the matrix changes, silently, in
// whichever direction the stale copy happens to point. Adding a role or moving
// a permission then becomes a cross-repository migration instead of a change
// here.
//
// The names are deliberately about the PROJECT, not about groups: what a member
// may do inside a project group is the group's own permission model, and
// exporting a merged view would invite a consumer to skip the group check.
func projectCapabilitiesForRole(role int) []string {
	caps := []string{"project.read"}
	switch {
	case role >= 2: // owner
		caps = append(caps, "project.member.manage", "project.update", "project.disband")
	case role == 1: // admin
		caps = append(caps, "project.member.manage", "project.update")
	}
	return caps
}
