package project

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/require"
)

// P2 behavioural tests: agents at create time (D2/D3/D11/D16), initial
// all-member-group provisioning, agent-seat cleanup, and agent eligibility.
// The stand-in provisioner exercises the project-side registry contract;
// native group membership is intentionally independent after creation.

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

// stubAllMemberGroup installs stand-in hooks and returns a recorder.
//
// Every test that touches the create path installs the provisioner stand-in,
// because the real one is registered by modules/group in binaries that include it.
// The rename stand-in is installed as well for metadata-sync coverage.
type allMemberGroupStub struct {
	provisionCalls int
	groupNo        string
	groupNos       map[string]string
	provisionErr   error
	seeds          []AllMemberGroupSeed
	renames        []string
	// provisionedInsideTx records that the provisioner saw NO committed project
	// row -- i.e. it was called from inside the create transaction, which is the
	// lock-order inversion the registry contract exists to prevent.
	provisionedInsideTx bool
	provisionOrderErr   error
}

// assertProvisionedAfterCommit fails the case if any provisioning ran before the
// project transaction committed. Registered as a cleanup by stubAllMemberGroup so
// every stubbed case carries the check without restating it.
func (s *allMemberGroupStub) assertProvisionedAfterCommit(t *testing.T) {
	t.Helper()
	require.NoError(t, s.provisionOrderErr, "the ordering probe itself failed")
	require.False(t, s.provisionedInsideTx,
		"the provisioner ran while the create transaction was still open: it would take "+
			"`group` / `group_member` locks under the project row lock and invert the "+
			"declared lock order")
}

func stubAllMemberGroup(t *testing.T, groupNo string) *allMemberGroupStub {
	t.Helper()
	// Put the REAL hooks back when this case ends.
	//
	// The registry is process-wide and latest-wins, and this binary may contain
	// modules/group. Leaving a stand-in behind would silently disable the real
	// provisioner or rename hook for later cases, so restore both at cleanup.
	//
	// modules/space's removal-step registry carries the same warning about the
	// same hazard.
	restore := SnapshotAllMemberGroupHooksForTest()
	t.Cleanup(func() { RestoreAllMemberGroupHooksForTest(restore) })

	s := &allMemberGroupStub{groupNo: groupNo, groupNos: make(map[string]string)}
	RegisterAllMemberGroupProvisioner(func(_ *config.Context, seed AllMemberGroupSeed) (string, error) {
		s.provisionCalls++
		s.seeds = append(s.seeds, seed)
		// The registry contract's first clause, checked on EVERY stubbed
		// provisioning rather than in one dedicated case: the hook runs after the
		// project transaction has COMMITTED.
		//
		// This read goes out on a pooled connection that is not the create's
		// transaction, so the project row is visible here only if that transaction
		// is already committed. That ordering is not a nicety -- it is what keeps
		// the declared lock order (space_member -> space -> project -> group ->
		// group_member -> octo_project_member) intact, because everything this hook
		// goes on to do touches `group` and `group_member`. A hook called from
		// inside the project transaction would take those locks under the project
		// row lock and invert the order for every concurrent group write.
		var seen int
		if qerr := testCtx.DB().SelectBySql(
			"SELECT COUNT(*) FROM `octo_project` WHERE project_id = ?", seed.ProjectID,
		).LoadOne(&seen); qerr != nil {
			s.provisionOrderErr = qerr
		} else if seen == 0 {
			s.provisionedInsideTx = true
		}
		if s.provisionErr != nil {
			return "", s.provisionErr
		}
		// Insert a REAL `group` row, because the real provisioner does. Keeping the
		// row behind the stand-in lets rename and disband tests exercise the same
		// project/group relation as production.
		groupNoForProject, ok := s.groupNos[seed.ProjectID]
		if !ok {
			groupNoForProject = s.groupNo
			if len(s.groupNos) > 0 {
				groupNoForProject += "-" + seed.ProjectID
			}
			s.groupNos[seed.ProjectID] = groupNoForProject
		}
		_, err := testCtx.DB().InsertBySql(
			"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) "+
				"VALUES (?, ?, ?, 1, ?, ?) "+
				"ON DUPLICATE KEY UPDATE project_id = VALUES(project_id), status = 1",
			groupNoForProject, seed.Name, seed.Creator, seed.SpaceID, seed.ProjectID,
		).Exec()
		if err != nil {
			return "", err
		}
		return groupNoForProject, nil
	})
	RegisterAllMemberGroupRename(func(_ *config.Context, _, _, name string) error {
		s.renames = append(s.renames, name)
		return nil
	})
	t.Cleanup(func() { s.assertProvisionedAfterCommit(t) })
	return s
}

// memberRow reads one seat straight from the table.
func memberRow(t *testing.T, projectID, uid string) *MemberModel {
	t.Helper()
	var rows []*MemberModel
	_, err := testCtx.DB().SelectBySql(
		"SELECT project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at "+
			"FROM `octo_project_member` WHERE project_id = ? AND uid = ?", projectID, uid,
	).Load(&rows)
	require.NoError(t, err)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func allMemberGroupNoOf(t *testing.T, projectID string) string {
	t.Helper()
	no, err := testDB.queryAllMemberGroupNo(projectID)
	require.NoError(t, err)
	return no
}

// ---------- D2 / D3 / D11 / D16: agents at create time ----------

func TestCreateProjectSeatsTheCreatorsAgents(t *testing.T) {
	srv, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_all_1")
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

	// Both agents hold an active seat, written in the SAME transaction as the
	// project — so if the project exists, they do.
	for _, uid := range []string{"bot_mine_1", "bot_mine_2"} {
		row := memberRow(t, resp.ProjectID, uid)
		require.NotNil(t, row, "agent %s should hold a seat", uid)
		require.Equal(t, MemberStatusActive, row.Status)
		require.Equal(t, 0, row.Removing)
		require.Equal(t, RoleCommon, row.Role)
		require.Equal(t, "u_owner", row.InviteUID)
	}

	// D11 — the epoch stays at 0. Agents are part of the roster coming into
	// existence, not a change to it.
	require.Equal(t, int64(0), epochOf(t, resp.ProjectID),
		"creating with agents must not move member_epoch")

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

	// The provisioner was seeded with the agents and NOT with the creator (the
	// group side adds its own creator).
	require.Equal(t, 1, stub.provisionCalls)
	require.Equal(t, []string{"bot_mine_1", "bot_mine_2"}, stub.seeds[0].Members)
	require.Equal(t, "u_owner", stub.seeds[0].Creator)
	require.Equal(t, "供应链运营协同", stub.seeds[0].Name)
	require.Equal(t, "grp_all_1", resp.AllMemberGroupNo)
}

// TestCreateProjectRejectsTheWholeRequestOnOneIneligibleAgent pins D3: no partial
// success, and every reason renders identically.
func TestCreateProjectRejectsTheWholeRequestOnOneIneligibleAgent(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_all_2")
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
	stubAllMemberGroup(t, "grp_all_3")
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
	stubAllMemberGroup(t, "grp_all_4")
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

// ---------- initial provisioning ----------

// TestAllMemberGroupProvisioningIsSkippedWhenUnregistered pins the binary that
// contains modules/project but not modules/group: the create succeeds, the group
// is absent, and nothing panics.
func TestAllMemberGroupProvisioningIsSkippedWhenUnregistered(t *testing.T) {
	_, p := setup(t)
	restore := SnapshotAllMemberGroupHooksForTest()
	t.Cleanup(func() { RestoreAllMemberGroupHooksForTest(restore) })
	RegisterAllMemberGroupProvisioner(nil)

	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "no hooks",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	require.NotNil(t, model)

	p.provisionAllMemberGroup(model.ProjectID, spaceA, "u_owner", "no hooks", nil)
	require.Empty(t, allMemberGroupNoOf(t, model.ProjectID))
}

// ---------- D13: agents follow their owner out ----------

// TestRemovingAMemberClosesTheirAgentsSeatsInTheSameTransaction is the core D13
// case: project removal must close the departing member's own agent seats too,
// so those seats cannot outlive the member's Project qualification.
func TestRemovingAMemberClosesTheirAgentsSeatsInTheSameTransaction(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_d13")
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
	stubAllMemberGroup(t, "grp_d13_leave")
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

// ---------- D8: rename sync ----------

func TestRenamingAProjectSyncsTheGroupNameOnlyWhenTheNameChanges(t *testing.T) {
	_, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_rename")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "before"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)

	w = doOn(t, r, http.MethodPut, "/v1/projects/"+resp.ProjectID, owner,
		map[string]any{"name": "after"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, []string{"after"}, stub.renames)

	// An update that touches only the description must NOT push a group rename:
	// it would bump the group's version and refresh every client for nothing.
	w = doOn(t, r, http.MethodPut, "/v1/projects/"+resp.ProjectID, owner,
		map[string]any{"description": "new goal"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, []string{"after"}, stub.renames, "description-only update must not rename")
}

// ---------- D16: the roster distinguishes agents ----------

func TestMemberRosterFlagsAgentsAndNamesTheirOwner(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_roster")
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
	stubAllMemberGroup(t, "grp_oracle")
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
	stubAllMemberGroup(t, "grp_audit")
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

// TestReleasingAStaleClaimDoesNotClearTheSuccessorsLease is PR #855's review, Q1.
//
// The release used to key on the project alone, so ANY claimant's lease was
// cleared. A provisioning attempt that outlives allMemberGroupLease then wipes,
// on its way out, the lease a successor is actively holding — and a third writer
// claims immediately, so two rebuilds run concurrently. That is the one thing the
// lease exists to prevent, defeated by the failure path of the attempt it was
// meant to fence.
func TestReleasingAStaleClaimDoesNotClearTheSuccessorsLease(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_lease_fence")

	model := leaseTestProject(t, p, "lease-fence")

	now := time.Now().UTC()
	stale, staleLease, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now)
	require.NoError(t, err)
	require.True(t, stale)

	// The stale attempt runs past its lease; a successor claims.
	after := now.Add(allMemberGroupLease + time.Minute)
	successor, successorLease, err := p.db.claimAllMemberGroupProvision(model.ProjectID, after)
	require.NoError(t, err)
	require.True(t, successor, "an expired lease must be re-claimable")
	require.NotEqual(t, staleLease, successorLease)

	// Now the stale attempt fails and releases. It must not touch the successor's lease.
	require.NoError(t, p.db.releaseAllMemberGroupProvision(model.ProjectID, staleLease))

	third, _, err := p.db.claimAllMemberGroupProvision(model.ProjectID, after)
	require.NoError(t, err)
	require.False(t, third,
		"the successor still holds the lease, so nobody else may claim. Without the "+
			"deadline fence the stale release cleared it and this claim succeeded — two "+
			"rebuilds running at once, which is exactly what the lease is for")

	// And the successor's own release still works.
	require.NoError(t, p.db.releaseAllMemberGroupProvision(model.ProjectID, successorLease))
	fourth, _, err := p.db.claimAllMemberGroupProvision(model.ProjectID, after)
	require.NoError(t, err)
	require.True(t, fourth, "a released lease must be immediately re-claimable")
}
