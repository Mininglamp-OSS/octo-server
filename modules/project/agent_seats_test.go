package project

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Agent seats at create time (D2/D3/D11/D16), agent-seat cleanup, and agent
// eligibility. Native group membership is intentionally independent: nothing
// here provisions, renames, or admits to a group.

// ---------- fixtures ----------

// seedAgent creates a bot user plus its robot row and a Space seat, i.e. exactly
// what botfather produces when it mints one.
func seedAgent(t *testing.T, spaceID, agentUID, ownerUID, hosting string) {
	t.Helper()
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, robot) VALUES (?, ?, ?, 1)",
		agentUID, "agent-"+agentUID, agentUID,
	).Exec()
	require.NoError(t, err)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, token, status, creator_uid, agent_hosting) VALUES (?, ?, 1, ?, ?)",
		agentUID, "tok-"+agentUID, ownerUID, hosting,
	).Exec()
	require.NoError(t, err)
	seedSpaceMember(t, spaceID, agentUID, 0, 1)
}

// memberRow reads one seat straight from the table.
func memberRow(t *testing.T, projectID, uid string) *MemberModel {
	t.Helper()
	var rows []*MemberModel
	_, err := testCtx.DB().SelectBySql(
		"SELECT project_id, uid, space_id, role, status, removing, invite_uid, created_at, "+
			"COALESCE(joined_at, created_at) AS joined_at, updated_at "+
			"FROM `octo_project_member` WHERE project_id = ? AND uid = ?", projectID, uid,
	).Load(&rows)
	require.NoError(t, err)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

// ---------- D2 / D3 / D11 / D16: agents at create time ----------

func TestCreateProjectSeatsTheCreatorsAgents(t *testing.T) {
	srv, p := setup(t)
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedAgent(t, spaceA, "bot_mine_1", "u_owner", "octo_hosted")
	seedAgent(t, spaceA, "bot_mine_2", "u_owner", "")
	_ = srv

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
		"name":       "供应链运营协同",
		"agent_uids": []string{"bot_mine_1", "bot_mine_2"},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeResp(t, w)

	ownerRow := memberRow(t, resp.ProjectID, "u_owner")
	require.NotNil(t, ownerRow, "creator should hold the Owner seat")
	require.False(t, ownerRow.JoinedAt.IsZero())
	assert.Equal(t, ownerRow.CreatedAt, ownerRow.JoinedAt,
		"the initial Owner admission starts both timestamps together")

	// Both agents hold an active seat, written in the SAME transaction as the
	// project — so if the project exists, they do.
	for _, uid := range []string{"bot_mine_1", "bot_mine_2"} {
		row := memberRow(t, resp.ProjectID, uid)
		require.NotNil(t, row, "agent %s should hold a seat", uid)
		require.Equal(t, MemberStatusActive, row.Status)
		require.Equal(t, 0, row.Removing)
		require.Equal(t, RoleCommon, row.Role)
		require.Equal(t, "u_owner", row.InviteUID)
		require.False(t, row.JoinedAt.IsZero(), "agent %s joined_at must be populated", uid)
		require.Equal(t, ownerRow.JoinedAt, row.JoinedAt,
			"all initial seats must use the create transaction's admission time")
	}

	// D11's intent survives, its VALUE does not: agents are part of the roster coming
	// into existence rather than a change to it, so seating them must not cost an EXTRA
	// bump — but a fresh project can no longer read 0.
	//
	// 0 is now reserved by the membership integration contract for "this project does
	// not exist or is not visible" (pkg/project.AbsentEpochSentinel), and the read layer
	// REFUSES to serve an active project holding it
	// (pkg/project.ErrLiveProjectOnAbsentSentinel) rather than hand a consumer a grant it
	// can cache forever. So creation bumps once, to 1, whether or not agents came with
	// it — see the bump at the end of createProjectOnce and
	// TestFreshProjectNeverShipsOnTheAbsentSentinel.
	//
	// Asserted as exactly 1, not ">= 1": the point of D11 is that agents add no bump of
	// their own, and only an exact value can express that.
	require.Equal(t, int64(1), epochOf(t, resp.ProjectID),
		"creating with agents must cost exactly ONE bump — the same as creating without "+
			"them. A fresh project reads 1 because 0 is the contract's \"does not exist\" "+
			"sentinel and the read layer refuses to serve an active project on it")

	// D16 — the counts are split, and the total still matches the seats the quota
	// counts.
	require.Equal(t, 1, resp.HumanMemberCount, "human_member_count counts humans only")
	require.Equal(t, 2, resp.AgentMemberCount)
	require.Equal(t, 3, resp.MemberCount,
		"member_count is the TOTAL — one owner plus two agents. It means what it meant "+
			"before D16 and what modules/opanalytics means by the same name; the human/agent "+
			"split lives in the two fields above")
	require.Equal(t, resp.HumanMemberCount+resp.AgentMemberCount, resp.MemberCount,
		"and the three must add up, which is why toResp derives the total from the two "+
			"halves instead of reading a third count under a third read view")
}

// TestCreateProjectRejectsTheWholeRequestOnOneIneligibleAgent pins D3: no partial
// success, and every reason renders identically.
func TestCreateProjectRejectsTheWholeRequestOnOneIneligibleAgent(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedUser(t, "u_other")
	seedSpaceMember(t, spaceA, "u_other", 0, 1)

	seedAgent(t, spaceA, "bot_ok", "u_owner", "octo_hosted")
	seedAgent(t, spaceA, "bot_theirs", "u_other", "octo_hosted")    // not mine
	seedAgent(t, spaceA, "bot_local", "u_owner", "self_hosted")     // self-hosted
	seedAgent(t, spaceB, "bot_elsewhere", "u_owner", "octo_hosted") // another Space

	// The account itself is deactivated / destroyed while the robot row and the Space
	// seat are still live. D2 says eligibility tracks the directory, and the directory
	// passes u.status = 1 AND COALESCE(u.is_destroy, 0) <> 2, so create must refuse
	// these too.
	//
	// The members/add half of this rule was covered from the round it was added; the
	// CREATE half was not, and the two run DIFFERENT queries (queryAgentRowsTx here,
	// queryAgentClassTx there) — so "one of them is tested" was not coverage of this
	// expression at all. PR #855s fifth review, Q4.
	seedAgent(t, spaceA, "bot_deactivated", "u_owner", "octo_hosted")
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `user` SET status = 0 WHERE uid = ?", "bot_deactivated").Exec()
	require.NoError(t, err)
	seedAgent(t, spaceA, "bot_destroyed", "u_owner", "octo_hosted")
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `user` SET is_destroy = 2 WHERE uid = ?", "bot_destroyed").Exec()
	require.NoError(t, err)

	type refusal struct{ bad, details string }
	refusals := make([]refusal, 0, 6)
	for _, bad := range []string{
		"bot_theirs", "bot_local", "bot_elsewhere", "u_other",
		"bot_deactivated", "bot_destroyed",
	} {
		w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
			"name":       "proj-" + bad,
			"agent_uids": []string{"bot_ok", bad},
		})
		require.Equal(t, http.StatusBadRequest, w.Code, "case %s body: %s", bad, w.Body.String())

		var env struct {
			Error struct {
				Code    string          `json:"code"`
				Details json.RawMessage `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env), "body: %s", w.Body.String())
		require.Equal(t, "err.server.project.agent_not_eligible", env.Error.Code, "case %s", bad)
		refusals = append(refusals, refusal{bad: bad, details: string(env.Error.Details)})

		// Nothing was written: not the project, and not the eligible agent's seat.
		var count int
		require.NoError(t, testCtx.DB().SelectBySql(
			"SELECT COUNT(*) FROM `octo_project` WHERE name = ?", "proj-"+bad,
		).LoadOne(&count))
		require.Zero(t, count, "case %s must not create the project", bad)
	}

	// Every refusal carries the same SHAPE — the INELIGIBLE uids and nothing else.
	// The four reasons are indistinguishable apart from which uid is named, which
	// is the caller's own input.
	//
	// bot_ok must NOT appear. D3 rejects the whole request, but "the request was
	// rejected" and "this uid was the problem" are different facts, and only the
	// second belongs in details: a picker that highlights every submitted row
	// leaves the user re-choosing from scratch to find the one that was wrong.
	// The first version echoed the full submitted list, so this assertion is the
	// pin, not a restatement.
	for _, ref := range refusals {
		require.Contains(t, ref.details, ref.bad,
			"case %s: details must name the ineligible uid", ref.bad)
		require.NotContains(t, ref.details, "bot_ok",
			"case %s: details must not name the ELIGIBLE agent — it was not the problem", ref.bad)
		require.NotContains(t, ref.details, "reason", "case %s must not leak WHY", ref.bad)
	}
}

func TestCreateProjectRejectsAnOverLongAgentBatch(t *testing.T) {
	_, p := setup(t)
	p.cfg.MemberBatchMax = 2
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
		"name":       "too many",
		"agent_uids": []string{"a", "b", "c"},
	})
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), "err.server.project.batch_too_large")
}

// TestCreateProjectRefusesWhenAgentsExceedTheMemberQuota pins that the seat quota
// counts agents, and that the refusal is the registered 4xx code rather than the
// Internal store_failed it fell through to before P2 registered one.
func TestCreateProjectRefusesWhenAgentsExceedTheMemberQuota(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedAgent(t, spaceA, "bot_q1", "u_owner", "octo_hosted")
	seedAgent(t, spaceA, "bot_q2", "u_owner", "octo_hosted")

	// max_members = 2 leaves room for the owner and one agent, not two.
	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
		"name":        "tight",
		"max_members": 2,
		"agent_uids":  []string{"bot_q1", "bot_q2"},
	})
	require.Equal(t, http.StatusBadRequest, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), "err.server.project.quota_members")
}

// ---------- D13: agents follow their owner out ----------

// TestRemovingAMemberClosesTheirAgentsSeatsInTheSameTransaction is the core D13
// case: project removal must close the departing member's own agent seats too,
// so those seats cannot outlive the member's Project qualification.
func TestRemovingAMemberClosesTheirAgentsSeatsInTheSameTransaction(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedUser(t, "u_member")
	seedSpaceMember(t, spaceA, "u_member", 0, 1)
	seedAgent(t, spaceA, "bot_of_member", "u_member", "octo_hosted")
	seedAgent(t, spaceA, "bot_of_owner", "u_owner", "octo_hosted")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
		"name":       "d13",
		"agent_uids": []string{"bot_of_owner"},
	})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)

	// The owner admits the member and then seats that member's agent on their behalf.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		addMembersPayload("u_member"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		addMembersPayload("bot_of_member"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, memberRow(t, resp.ProjectID, "bot_of_member"))

	before := epochOf(t, resp.ProjectID)

	// The owner removes the member. Their agent must go with them.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/remove", owner,
		map[string]any{"uids": []string{"u_member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	memberSeat := memberRow(t, resp.ProjectID, "u_member")
	require.NotNil(t, memberSeat)
	require.Equal(t, 1, memberSeat.Removing, "D4 — the seat closes in two phases")

	agentSeat := memberRow(t, resp.ProjectID, "bot_of_member")
	require.NotNil(t, agentSeat)
	require.Equal(t, 1, agentSeat.Removing,
		"D13 — the departing member's own agent must be closing too")

	// Somebody else's agent is untouched.
	ownerAgent := memberRow(t, resp.ProjectID, "bot_of_owner")
	require.NotNil(t, ownerAgent)
	require.Equal(t, 0, ownerAgent.Removing, "another member's agent must not be swept up")

	// ONE epoch bump for the member and their agent together: it is one
	// membership change, and the epoch is asserted to move by exactly +1.
	require.Equal(t, before+1, epochOf(t, resp.ProjectID),
		"member + agents leaving is ONE change, so exactly one bump")

	// Each closing seat got its own cascade job — the worker is keyed
	// (project_id, uid) and re-reads that row, so a job covering two uids could
	// not be cancelled for one of them.
	var jobs int
	require.NoError(t, testCtx.DB().SelectBySql(
		"SELECT COUNT(*) FROM `octo_project_member_removal_cleanup` "+
			"WHERE project_id = ? AND uid IN (?, ?) AND status = 0",
		resp.ProjectID, "u_member", "bot_of_member",
	).LoadOne(&jobs))
	require.Equal(t, 2, jobs, "the member and their agent each need their own cascade job")
}

// TestLeavingAProjectTakesTheLeaversAgents covers the self-service half of D13.
func TestLeavingAProjectTakesTheLeaversAgents(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	member := seedUser(t, "u_member")
	seedSpaceMember(t, spaceA, "u_member", 0, 1)
	seedAgent(t, spaceA, "bot_leaver", "u_member", "octo_hosted")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "leave"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		addMembersPayload("u_member"))
	require.Equal(t, http.StatusOK, w.Code)
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		addMembersPayload("bot_leaver"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/leave", member, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	agentSeat := memberRow(t, resp.ProjectID, "bot_leaver")
	require.NotNil(t, agentSeat)
	require.Equal(t, 1, agentSeat.Removing, "an agent follows its owner out on leave too")
}

// ---------- D16: the roster distinguishes agents ----------

func TestMemberRosterFlagsAgentsAndNamesTheirOwner(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedAgent(t, spaceA, "bot_r", "u_owner", "octo_hosted")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
		"name":       "roster",
		"agent_uids": []string{"bot_r"},
	})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)

	w = doOn(t, r, http.MethodGet, "/v1/projects/"+resp.ProjectID+"/members", owner, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var roster []MemberResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &roster), "body: %s", w.Body.String())

	byUID := map[string]MemberResp{}
	for _, m := range roster {
		byUID[m.UID] = m
	}
	require.Equal(t, 0, byUID["u_owner"].Robot)
	require.Empty(t, byUID["u_owner"].OwnerUID)
	require.Equal(t, 1, byUID["bot_r"].Robot, "an agent must be flagged as one")
	require.Equal(t, "u_owner", byUID["bot_r"].OwnerUID, "and must name its owner")
}

// ---------- authorization regressions ----------

// TestAnOrdinaryMemberCannotTellAnAgentFromAHuman pins the anti-enumeration fix.
//
// Agent eligibility is checked only after the actor's management capability, so
// an ordinary member gets the same response for an ineligible bot and a human.
func TestAnOrdinaryMemberCannotTellAnAgentFromAHuman(t *testing.T) {
	_, p := setup(t)
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	plain := seedUser(t, "u_plain")
	seedSpaceMember(t, spaceA, "u_plain", 0, 1)
	seedUser(t, "u_third")
	seedSpaceMember(t, spaceA, "u_third", 0, 1)
	seedUser(t, "u_human")
	seedSpaceMember(t, spaceA, "u_human", 0, 1)
	seedAgent(t, spaceA, "bot_of_third", "u_third", "octo_hosted")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "oracle"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		addMembersPayload("u_plain"))
	require.Equal(t, http.StatusOK, w.Code)

	// An ordinary member naming somebody else's live bot, and naming a plain human.
	// The two answers must be indistinguishable.
	botBody := doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", plain,
		addMembersPayload("bot_of_third")).Body.String()
	humanBody := doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", plain,
		addMembersPayload("u_human")).Body.String()
	require.Equal(t, humanBody, botBody,
		"an ordinary member must not be able to tell somebody else's bot from a human; "+
			"bot=%s human=%s", botBody, humanBody)
	require.Nil(t, memberRow(t, resp.ProjectID, "bot_of_third"), "and nothing was seated")

	// Deliberately NOT asserting that a uid with no Space seat is indistinguishable
	// too. That one is refused earlier, by P0's in-transaction Space seat check,
	// which necessarily runs before any project role is read (it is first in the
	// declared lock order). So it answers not_space_member, and it did so before
	// this change. It is also not a leak: the caller holds a seat in that Space and
	// can already enumerate its members through the Space roster endpoint.
	//
	// The contract this case pins is the one the review found broken and the one
	// ErrProjectAgentNotEligible exists for: whether a uid is somebody else AI
	// agent must not be readable from the refusal.
}

// ---------- audit: the agent write paths ----------

// TestAgentSeatsAreAudited covers the two membership writes P2 added that no
// handler issues a request for: the agent seats written inside the create
// transaction (D2/D3) and the agent seats closed when their owner leaves (D13).
//
// TestEveryWritePathEmitsAnAuditEntry does not reach either — it never sends
// agent_uids and seeds no agents — so before this case both wrote membership rows
// that left no trace at all, and "how did this bot get access to the project" was
// unanswerable from the trail.
func TestAgentSeatsAreAudited(t *testing.T) {
	_, p := setup(t)
	rec := &auditRecorder{}
	p.auditSink = rec.sink
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	member := seedUser(t, "u_member")
	seedSpaceMember(t, spaceA, "u_member", 0, 1)
	seedAgent(t, spaceA, "bot_at_create", "u_owner", "octo_hosted")
	seedAgent(t, spaceA, "bot_of_member", "u_member", "octo_hosted")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
		"name":       "audited-agents",
		"agent_uids": []string{"bot_at_create"},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeResp(t, w)

	require.True(t, auditHasTarget(rec.entries, auditMemberAdd, "bot_at_create"),
		"an agent seated by the create must be audited like any other member add")
	for _, e := range rec.byAction(auditMemberAdd) {
		require.Equal(t, "u_owner", e.ActorUID, "the creator is the actor")
		require.Equal(t, auditReasonAgentOnCreate, e.Reason,
			"the reason must say the seat rode in on the create, not on a members/add")
		require.Equal(t, resp.ProjectID, e.ProjectID)
		require.Equal(t, spaceA, e.SpaceID)
	}

	// Now the removal half: the owner seats the member and their agent, then the member leaves.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		addMembersPayload("u_member"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		addMembersPayload("bot_of_member"))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/leave", member, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var followed *AuditEntry
	removals := rec.byAction(auditMemberRemove)
	for i := range removals {
		if removals[i].TargetUID == "bot_of_member" {
			followed = &removals[i]
		}
	}
	require.NotNil(t, followed,
		"D13 closes the leaver's agent seat, so the trail must carry that removal")
	require.Equal(t, auditReasonAgentFollowsOwner, followed.Reason,
		"the reason must say the agent was not removed on its own account")
	require.Equal(t, "u_member", followed.ActorUID,
		"the leaver is the actor: they closed their own seat and the agent followed")
	require.Equal(t, resp.ProjectID, followed.ProjectID)
	require.Equal(t, spaceA, followed.SpaceID)
}
