package project

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/require"
)

// P2 behavioural tests: agents at create time (D2/D3/D11/D16), the all-member
// group's provisioning and rebuild (D4/D5), the admitter (D12), agents following
// their owner out (D13), and the D15 authorization rules.
//
// The group side is driven through a STAND-IN provisioner/admitter rather than
// modules/group, for two reasons. modules/project must never import
// modules/group, so a real one is not reachable from this package at all; and the
// registry contract — "the hook runs after the project transaction commits, and
// its failure never fails the caller" — is a property of THIS module, testable
// only by making the hook fail on purpose. The real implementations are covered
// from modules/group.

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
// Every test that touches the create or add paths must install these, because the
// real ones are registered by modules/group and are therefore absent from this
// package's binary — leaving them unregistered would make provisioning fail for a
// reason unrelated to what is being asserted.
type allMemberGroupStub struct {
	provisionCalls int
	admitted       []string
	groupNo        string
	provisionErr   error
	admitErr       error
	seeds          []AllMemberGroupSeed
	ownerSyncs     []string
	renames        []string
}

func stubAllMemberGroup(t *testing.T, groupNo string) *allMemberGroupStub {
	t.Helper()
	s := &allMemberGroupStub{groupNo: groupNo}
	RegisterAllMemberGroupProvisioner(func(_ *config.Context, seed AllMemberGroupSeed) (string, error) {
		s.provisionCalls++
		s.seeds = append(s.seeds, seed)
		if s.provisionErr != nil {
			return "", s.provisionErr
		}
		return s.groupNo, nil
	})
	RegisterAllMemberGroupAdmitter(func(_ *config.Context, _, _, uid string) error {
		if s.admitErr != nil {
			return s.admitErr
		}
		s.admitted = append(s.admitted, uid)
		return nil
	})
	RegisterAllMemberGroupOwnerTransfer(func(_ *config.Context, projectID, _ string) error {
		s.ownerSyncs = append(s.ownerSyncs, projectID)
		return nil
	})
	RegisterAllMemberGroupRename(func(_ *config.Context, _, name string) error {
		s.renames = append(s.renames, name)
		return nil
	})
	// Restore nothing on cleanup: the registry is latest-wins and every test that
	// cares installs its own. Clearing to nil instead would make the ORDER of
	// cases decide whether a later one sees a hook, which is exactly the
	// cross-case coupling -shuffle=on exists to find.
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
	require.Equal(t, 1, resp.MemberCount, "member_count counts humans only")
	require.Equal(t, 2, resp.AgentCount)

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

	bodies := make([]string, 0, 4)
	for _, bad := range []string{"bot_theirs", "bot_local", "bot_elsewhere", "u_other"} {
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
		bodies = append(bodies, string(env.Error.Details))

		// Nothing was written: not the project, and not the eligible agent's seat.
		var count int
		require.NoError(t, testCtx.DB().SelectBySql(
			"SELECT COUNT(*) FROM `octo_project` WHERE name = ?", "proj-"+bad,
		).LoadOne(&count))
		require.Zero(t, count, "case %s must not create the project", bad)
	}

	// Every refusal carries the same SHAPE — the submitted uids and nothing else.
	// The four reasons are indistinguishable apart from which uid is named, which
	// is the caller's own input.
	for i, body := range bodies {
		require.Contains(t, body, "bot_ok", "case %d details must echo the submitted uids", i)
		require.NotContains(t, body, "reason", "case %d must not leak WHY", i)
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

// ---------- D4: provisioning failure and rebuild ----------

// TestProjectSurvivesAProvisioningFailureAndIsRebuiltOnTheNextWrite pins D4 end
// to end: the create succeeds with an empty group, and the next write path
// rebuilds under the lease.
func TestProjectSurvivesAProvisioningFailureAndIsRebuiltOnTheNextWrite(t *testing.T) {
	_, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_rebuilt")
	stub.provisionErr = errStubProvision
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedUser(t, "u_new")
	seedSpaceMember(t, spaceA, "u_new", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "resilient"})
	require.Equal(t, http.StatusOK, w.Code, "a provisioning failure must NOT fail the create")
	resp := decodeResp(t, w)
	require.Empty(t, resp.AllMemberGroupNo, "the response must report the group as absent")
	require.Empty(t, allMemberGroupNoOf(t, resp.ProjectID))

	// The lease was released on failure, so the next write path can claim it
	// immediately rather than waiting out allMemberGroupLease.
	stub.provisionErr = nil
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_new"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Equal(t, "grp_rebuilt", allMemberGroupNoOf(t, resp.ProjectID),
		"the next write path must rebuild the all-member group")
	// D12 — and the member just added went into it.
	require.Contains(t, stub.admitted, "u_new")
}

// TestAllMemberGroupRebuildIsClaimedOnce pins the CAS lease: two concurrent
// rebuild attempts produce one group, not two.
//
// The lease is what makes this safe, and it has to be, because the hooks run
// AFTER the project transaction commits — the project row lock is already gone by
// then, so it cannot serialize them.
func TestAllMemberGroupRebuildIsClaimedOnce(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_race")
	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "race",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)
	require.Empty(t, model.AllMemberGroupNo)

	now := time.Now().UTC()
	first, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now)
	require.NoError(t, err)
	require.True(t, first, "the first claim must win")

	second, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now)
	require.NoError(t, err)
	require.False(t, second, "a second claim inside the lease must lose")

	// Once the group is written back, no further claim succeeds at all — that is
	// what makes the rebuild idempotent rather than merely serialized.
	ok, err := p.db.setAllMemberGroupNo(model.ProjectID, "grp_race")
	require.NoError(t, err)
	require.True(t, ok)

	third, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, third, "a project that already has a group has no work to claim")

	// A late write-back from a claim whose lease expired must NOT overwrite.
	overwritten, err := p.db.setAllMemberGroupNo(model.ProjectID, "grp_late")
	require.NoError(t, err)
	require.False(t, overwritten)
	require.Equal(t, "grp_race", allMemberGroupNoOf(t, model.ProjectID))
}

// TestAllMemberGroupProvisioningIsSkippedWhenUnregistered pins the binary that
// contains modules/project but not modules/group: the create succeeds, the group
// is absent, and nothing panics.
func TestAllMemberGroupProvisioningIsSkippedWhenUnregistered(t *testing.T) {
	_, p := setup(t)
	RegisterAllMemberGroupProvisioner(nil)
	RegisterAllMemberGroupAdmitter(nil)
	t.Cleanup(func() { stubAllMemberGroup(t, "grp_restore") })

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

// ---------- D12: admission failure does not fail the add ----------

func TestAdmitFailureLeavesTheSeatCommitted(t *testing.T) {
	_, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_admit")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedUser(t, "u_new")
	seedSpaceMember(t, spaceA, "u_new", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "admit"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)

	stub.admitErr = errStubAdmit
	before := epochOf(t, resp.ProjectID)
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_new"}})
	require.Equal(t, http.StatusOK, w.Code,
		"the seat is the authorization fact; a failed group admission must not fail the add")

	row := memberRow(t, resp.ProjectID, "u_new")
	require.NotNil(t, row)
	require.Equal(t, MemberStatusActive, row.Status)
	require.Greater(t, epochOf(t, resp.ProjectID), before, "the epoch moved with the seat")
	require.NotContains(t, stub.admitted, "u_new")
}

// ---------- D13: agents follow their owner out ----------

// TestRemovingAMemberClosesTheirAgentsSeatsInTheSameTransaction is the core D13
// case: without it the agent keeps an ACTIVE seat while the group side has
// already pulled it out of the project's groups, and nothing ever revisits it.
func TestRemovingAMemberClosesTheirAgentsSeatsInTheSameTransaction(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_d13")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	member := seedUser(t, "u_member")
	seedSpaceMember(t, spaceA, "u_member", 0, 1)
	seedAgent(t, spaceA, "bot_of_member", "u_member", "octo_hosted")
	seedAgent(t, spaceA, "bot_of_owner", "u_owner", "octo_hosted")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner, map[string]any{
		"name":       "d13",
		"agent_uids": []string{"bot_of_owner"},
	})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)

	// The member joins and brings their own agent (D15 lets them).
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", member,
		map[string]any{"uids": []string{"bot_of_member"}})
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
		map[string]any{"uids": []string{"u_member"}})
	require.Equal(t, http.StatusOK, w.Code)
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", member,
		map[string]any{"uids": []string{"bot_leaver"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/leave", member, map[string]any{})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	agentSeat := memberRow(t, resp.ProjectID, "bot_leaver")
	require.NotNil(t, agentSeat)
	require.Equal(t, 1, agentSeat.Removing, "an agent follows its owner out on leave too")
}

// ---------- D15: who may seat and unseat an agent ----------

func TestOnlyTheOwnerMaySeatTheirOwnAgent(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_d15")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	admin := seedUser(t, "u_admin")
	seedSpaceMember(t, spaceA, "u_admin", 0, 1)
	member := seedUser(t, "u_member")
	seedSpaceMember(t, spaceA, "u_member", 0, 1)
	seedUser(t, "u_outsider")
	seedSpaceMember(t, spaceA, "u_outsider", 0, 1)
	seedAgent(t, spaceA, "bot_member", "u_member", "octo_hosted")
	seedAgent(t, spaceA, "bot_outsider", "u_outsider", "octo_hosted")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", admin,
		map[string]any{"name": "d15"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", admin,
		map[string]any{"uids": []string{"u_member"}})
	require.Equal(t, http.StatusOK, w.Code)

	// The project OWNER may not seat somebody else's agent, despite holding
	// can_manage_member. The dialog promises "only your own agents"; an admin
	// acting for another person is exactly what that excludes.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", admin,
		map[string]any{"uids": []string{"bot_member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Contains(t, w.Body.String(), reasonAgentNotEligible)
	require.Nil(t, memberRow(t, resp.ProjectID, "bot_member"))

	// Its owner may, holding only can_manage_own_agents.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", member,
		map[string]any{"uids": []string{"bot_member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, memberRow(t, resp.ProjectID, "bot_member"))

	// An agent whose owner is not in the project is refused even to its owner —
	// they are not a member, so they have no capability here at all.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", admin,
		map[string]any{"uids": []string{"bot_outsider"}})
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), reasonAgentNotEligible)
	require.Nil(t, memberRow(t, resp.ProjectID, "bot_outsider"))
}

func TestAMemberMayUnseatTheirOwnAgent(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_d15_rm")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	member := seedUser(t, "u_member")
	seedSpaceMember(t, spaceA, "u_member", 0, 1)
	seedAgent(t, spaceA, "bot_mine", "u_member", "octo_hosted")

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "unseat"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_member"}})
	require.Equal(t, http.StatusOK, w.Code)
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", member,
		map[string]any{"uids": []string{"bot_mine"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// An ordinary member removing their own agent: allowed, even though
	// canActOnTargetRole would refuse a member acting on anyone.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/remove", member,
		map[string]any{"uids": []string{"bot_mine"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	row := memberRow(t, resp.ProjectID, "bot_mine")
	require.NotNil(t, row)
	require.Equal(t, 1, row.Removing, "the agent's seat must be closing")
}

// ---------- D10: disband clears the pointer ----------

func TestDisbandClearsTheAllMemberGroupPointer(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_disband")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "gone"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)
	require.Equal(t, "grp_disband", allMemberGroupNoOf(t, resp.ProjectID))

	w = doOn(t, r, http.MethodDelete, "/v1/projects/"+resp.ProjectID, owner, nil)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// queryAllMemberGroupNo filters on status, so a disbanded project answers ""
	// either way. Read the column directly to prove it was actually cleared,
	// which is what keeps "disbanded project still pointing at a group" out of
	// the states the protection and rebuild predicates have to reason about.
	var stored []string
	_, err := testCtx.DB().SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` WHERE project_id = ?", resp.ProjectID,
	).Load(&stored)
	require.NoError(t, err)
	require.Equal(t, []string{""}, stored)
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

// Stand-in failures, declared once so a test can distinguish "the hook refused"
// from any real error the path might produce.
var (
	errStubProvision = errStub("stub: provisioner refused")
	errStubAdmit     = errStub("stub: admitter refused")
)

type errStub string

func (e errStub) Error() string { return string(e) }
