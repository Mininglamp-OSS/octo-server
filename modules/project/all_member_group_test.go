package project

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
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
	// The registry is process-wide and latest-wins, and this binary CONTAINS
	// modules/group: the external test package imports octo-server/internal, so
	// module.Setup registers the real provisioner and admitter, and the
	// end-to-end cases depend on them. Leaving a stand-in behind silently
	// disables the feature for every later case — those cases passed when run
	// alone and failed in a full run until this restore existed, which is exactly
	// the order-dependent failure -shuffle=on is meant to surface.
	//
	// modules/space's removal-step registry carries the same warning about the
	// same hazard.
	restore := SnapshotAllMemberGroupHooksForTest()
	t.Cleanup(func() { RestoreAllMemberGroupHooksForTest(restore) })

	s := &allMemberGroupStub{groupNo: groupNo}
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
		// Insert a REAL `group` row, because the real provisioner does.
		//
		// queryAllMemberGroupNo requires the group side to agree that it belongs to
		// this project — without that, a group P1's cascade has detached to
		// Space-direct would still be admitted into and renamed. A stand-in that
		// returned a group number with no row behind it would make every caller of
		// that lookup read "no group", which is a stand-in disagreeing with
		// production about the thing under test.
		_, err := testCtx.DB().InsertBySql(
			"INSERT INTO `group` (group_no, name, creator, status, space_id, project_id) "+
				"VALUES (?, ?, ?, 1, ?, ?) "+
				"ON DUPLICATE KEY UPDATE project_id = VALUES(project_id), status = 1",
			s.groupNo, seed.Name, seed.Creator, seed.SpaceID, seed.ProjectID,
		).Exec()
		if err != nil {
			return "", err
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

	type refusal struct{ bad, details string }
	refusals := make([]refusal, 0, 4)
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
	first, firstLease, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now)
	require.NoError(t, err)
	require.True(t, first, "the first claim must win")
	require.False(t, firstLease.IsZero(), "a winning claim must hand back its deadline")

	second, _, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now)
	require.NoError(t, err)
	require.False(t, second, "a second claim inside the lease must lose")

	// Once the group is written back, no further claim succeeds at all — that is
	// what makes the rebuild idempotent rather than merely serialized.
	ok, err := p.db.setAllMemberGroupNo(model.ProjectID, "grp_race")
	require.NoError(t, err)
	require.True(t, ok)

	third, _, err := p.db.claimAllMemberGroupProvision(model.ProjectID, now.Add(time.Hour))
	require.NoError(t, err)
	require.False(t, third, "a project that already has a group has no work to claim")

	// A late write-back from a claim whose lease expired must NOT overwrite.
	overwritten, err := p.db.setAllMemberGroupNo(model.ProjectID, "grp_late")
	require.NoError(t, err)
	require.False(t, overwritten)

	// Read the COLUMN, not queryAllMemberGroupNo: this case drives the CAS
	// protocol directly and never runs a provisioner, so no `group` row exists and
	// the join-backed lookup would correctly answer "no group". What is under test
	// here is which value the column holds.
	var stored []string
	_, err = testCtx.DB().SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` WHERE project_id = ?", model.ProjectID,
	).Load(&stored)
	require.NoError(t, err)
	require.Equal(t, []string{"grp_race"}, stored)
}

// TestAllMemberGroupProvisioningIsSkippedWhenUnregistered pins the binary that
// contains modules/project but not modules/group: the create succeeds, the group
// is absent, and nothing panics.
func TestAllMemberGroupProvisioningIsSkippedWhenUnregistered(t *testing.T) {
	_, p := setup(t)
	restore := SnapshotAllMemberGroupHooksForTest()
	t.Cleanup(func() { RestoreAllMemberGroupHooksForTest(restore) })
	RegisterAllMemberGroupProvisioner(nil)
	RegisterAllMemberGroupAdmitter(nil)

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

// ---------- regressions from the code review ----------

// TestRebuildUsesAnActiveOwnerNotTheOriginalCreator pins the fix for a defect
// that made a project's group unrecoverable.
//
// The rebuild used octo_project.creator, which never changes. Once that uid had
// left the project the admission gate refused them — they are not an active
// project member — so every rebuild attempt claimed the lease, failed to create
// the group, released the lease, and the project could NEVER get its group back.
// Reconcile scan A would report it forever with nothing able to repair it.
func TestRebuildUsesAnActiveOwnerNotTheOriginalCreator(t *testing.T) {
	_, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_rebuild_owner")
	stub.provisionErr = errStubProvision
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	founder := seedUser(t, "u_founder")
	seedSpaceMember(t, spaceA, "u_founder", 0, 1)
	seedUser(t, "u_successor")
	seedSpaceMember(t, spaceA, "u_successor", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", founder,
		map[string]any{"name": "handover"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)
	require.Empty(t, resp.AllMemberGroupNo, "provisioning was made to fail")

	// The founder hands the project over and leaves. octo_project.creator still
	// names them; they are no longer a member.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", founder,
		map[string]any{"uids": []string{"u_successor"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/leave", founder,
		map[string]any{"transfer_to": "u_successor"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	row, err := testDB.queryByProjectID(resp.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "u_founder", row.Creator, "creator is immutable, which is the trap")

	// Now the rebuild must succeed, seeded with the CURRENT owner.
	stub.provisionErr = nil
	stub.seeds = nil
	p.ensureAllMemberGroup(resp.ProjectID, spaceA)

	require.Len(t, stub.seeds, 1, "the rebuild must have run")
	require.Equal(t, "u_successor", stub.seeds[0].Creator,
		"the rebuild must seed the group with an ACTIVE owner; seeding the departed "+
			"creator means the admission gate refuses and the project can never get a group")
}

// TestQueryAllMemberGroupNoIgnoresADetachedGroup pins that the project side and
// pkg/project.IsAllMemberGroup answer the same question.
//
// P1's cascade reverts a group to Space-direct when its creator leaves and nobody
// can inherit, and cannot clear the project's pointer (modules/group may not write
// octo_project). Reading the pointer alone then makes the admitter, the rename and
// the owner sync all write to a group that is no longer the project's.
func TestQueryAllMemberGroupNoIgnoresADetachedGroup(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_detach_probe")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "detach"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)

	// The stand-in provisioner already inserted the real `group` row, as the real
	// one does, so there is nothing to seed here.
	got, err := testDB.queryAllMemberGroupNo(resp.ProjectID)
	require.NoError(t, err)
	require.Equal(t, "grp_detach_probe", got, "both halves agree, so the project owns it")

	// P1's detach: the group goes Space-direct; the pointer stays behind.
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `group` SET project_id = '' WHERE group_no = ?", "grp_detach_probe").Exec()
	require.NoError(t, err)

	got, err = testDB.queryAllMemberGroupNo(resp.ProjectID)
	require.NoError(t, err)
	require.Empty(t, got,
		"a detached group must read as absent, or the admitter would put new project "+
			"members into a group the project no longer owns and a rename would rename it")
}

// TestAnOrdinaryMemberCannotTellAnAgentFromAHuman pins the anti-enumeration fix.
//
// The agent check used to run before the permission gate, so a caller with no
// member-management right got a per-uid agent_not_eligible for somebody else's
// live bot and an actor-level permission_denied for a human — turning members/add
// into the oracle ErrProjectAgentNotEligible was registered to prevent, for the
// least privileged caller there is.
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
		map[string]any{"uids": []string{"u_plain"}})
	require.Equal(t, http.StatusOK, w.Code)

	// An ordinary member naming somebody else's live bot, and naming a plain human.
	// The two answers must be indistinguishable.
	botBody := doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", plain,
		map[string]any{"uids": []string{"bot_of_third"}}).Body.String()
	humanBody := doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", plain,
		map[string]any{"uids": []string{"u_human"}}).Body.String()
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

// TestRebuildRecoversAProjectWhoseGroupWasDetached pins the fix for a deadlock
// that the previous round's own fix introduced.
//
// Tightening queryAllMemberGroupNo to verify the group side made the READ correct
// and left the WRITE behind: the lookup answered "no group", so ensureAllMemberGroup
// proceeded, while the claim CAS keys on all_member_group_no being empty and the
// pointer was still set. The claim then failed on every attempt, silently — no
// error, no metric — so the rebuild never ran, every later member's admission
// no-opped, and reconcile scan A reported a project nothing could repair.
//
// Two predicates disagreeing about one fact, which is the same shape as the bug
// the tightening was fixing.
func TestRebuildRecoversAProjectWhoseGroupWasDetached(t *testing.T) {
	_, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_detached_first")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "detached-rebuild"})
	require.Equal(t, http.StatusOK, w.Code)
	resp := decodeResp(t, w)
	require.Equal(t, "grp_detached_first", resp.AllMemberGroupNo)

	// P1's cascade: the group's creator left and nobody could inherit, so the
	// group reverts to Space-direct. modules/group cannot clear the project's
	// pointer, so it stays.
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `group` SET project_id = '' WHERE group_no = ?", "grp_detached_first").Exec()
	require.NoError(t, err)

	require.Empty(t, allMemberGroupNoOf(t, resp.ProjectID),
		"precondition: the lookup already reports no usable group")

	// The next write path must actually rebuild, not sit stuck behind a pointer
	// the claim can never satisfy.
	stub.groupNo = "grp_rebuilt_after_detach"
	p.ensureAllMemberGroup(resp.ProjectID, spaceA)

	require.Equal(t, "grp_rebuilt_after_detach", allMemberGroupNoOf(t, resp.ProjectID),
		"a project whose group was detached must be able to get a new one; leaving the "+
			"stale pointer in place makes the claim CAS fail forever, with no error and "+
			"no metric, and reconcile scan A reports it with nothing able to fix it")
}

// ---------- D12: the projection is self-healing ----------

// TestReAddingAnActiveMemberReAdmitsThemToTheGroup pins the repair the brief
// documents for an I4 scan-B gap.
//
// Scan B reports "the seat exists, the group row does not" and repairs nothing by
// design; the operator action it names is "an admin re-adds the member". That only
// works if a re-add actually runs the admitter — and the first version gated the
// admission on `admitted`, which is false precisely when the seat is already there.
// So the one repair path the gauge points at was a silent no-op, and the gauge
// stayed red however many times it was tried.
func TestReAddingAnActiveMemberReAdmitsThemToTheGroup(t *testing.T) {
	_, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_repair")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	seedUser(t, "u_new")
	seedSpaceMember(t, spaceA, "u_new", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "repair"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeResp(t, w)

	// The admitter fails, so the seat commits without a group row — the exact state
	// scan B reports.
	stub.admitErr = errStubAdmit
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_new"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotContains(t, stub.admitted, "u_new", "the gap exists: seat yes, group row no")

	seat := memberRow(t, resp.ProjectID, "u_new")
	require.NotNil(t, seat)
	require.Equal(t, MemberStatusActive, seat.Status)

	// The repair: the same add again. The SEAT write is a no-op — that is the whole
	// point — but the admission must still run.
	stub.admitErr = nil
	before := epochOf(t, resp.ProjectID)
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_new"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Contains(t, stub.admitted, "u_new",
		"re-adding an active member must re-run the admission; that is the documented "+
			"repair for an I4 scan-B gap")
	require.Equal(t, before, epochOf(t, resp.ProjectID),
		"the seat did not change, so the epoch must not move — the repair touches the "+
			"projection only")
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

	// Now the removal half: the member joins with their own agent, then leaves.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", member,
		map[string]any{"uids": []string{"bot_of_member"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/leave", member, map[string]any{})
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

// ---------- D13 on the Space-removal path ----------

// TestSpaceCascadeBeginsATwoPhaseCloseForTheDepartingMembersAgents pins the
// mechanism the end-to-end case exercises: the HUMAN's seat closes directly on
// this path, the AGENTS get the two-phase close plus a job each.
//
// The distinction is the finding. modules/group's cleanupSpaceMemberGroups covers
// the human because it walks THEIR groups; it cannot cover an agent sitting in a
// group its owner never joined. Only a project-side removal job reaches those,
// because P1's detach step removes a uid from EVERY group of the project.
//
// Direct-closing the agents would not merely skip the job — it would make one
// useless if it were enqueued anyway: removalCancelled retires any job whose
// member reads removing = 0, which a directly-closed seat does. So "close the seat
// and also enqueue" is not a fix; the two-phase close is the fix.
func TestSpaceCascadeBeginsATwoPhaseCloseForTheDepartingMembersAgents(t *testing.T) {
	srv, p := setup(t)
	stubAllMemberGroup(t, "grp_space_d13")
	_, _, created := projectWithMembers(t, srv, "member1")

	seedAgent(t, spaceA, "bot_of_member1", "member1", "octo_hosted")
	// Seated by SQL rather than through members/add: the epoch assertion below is
	// about the removal, and an add would move it first.
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO `octo_project_member` "+
			"(project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, 0, 1, 0, ?, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
		created.ProjectID, "bot_of_member1", spaceA, "member1",
	).Exec()
	require.NoError(t, err)

	before := epochOf(t, created.ProjectID)

	// The member loses their Space seat; the cascade runs for this project.
	removeSpaceMember(t, spaceA, "member1")
	changed, err := p.deactivateSeatForCascade(created.ProjectID, spaceA, "member1", "op", "force_removed")
	require.NoError(t, err)
	require.True(t, changed)

	humanStatus, humanRemoving := seatState(t, created.ProjectID, "member1")
	assert.Equal(t, MemberStatusRemoved, humanStatus,
		"the human still closes directly: the group side already covers their groups")
	assert.Equal(t, 0, humanRemoving)

	agentStatus, agentRemoving := seatState(t, created.ProjectID, "bot_of_member1")
	assert.Equal(t, MemberStatusActive, agentStatus,
		"the agent must NOT be closed directly — a closed seat makes its own removal job "+
			"read as cancelled, so the group-side detach would never run")
	assert.Equal(t, 1, agentRemoving,
		"removing = 1 is what makes the seat stop authorizing while the cascade runs")

	assert.Equal(t, 1, pendingJobsFor(t, created.ProjectID, "bot_of_member1"),
		"one job per agent, keyed (project_id, uid): P1's detach then takes the agent out "+
			"of EVERY group of the project, including the ones its owner was never in")

	assert.Equal(t, before+1, epochOf(t, created.ProjectID),
		"a member leaving with their agents is ONE membership change, so exactly one bump")
}

// ---------- D2 on members/add (PR #855 review, S4) ----------

// TestMembersAddAppliesTheSameAgentPredicateAsCreate pins that the two entry
// points agree about what an eligible agent is.
//
// They did not. `create` ran the full D2 rule; `members/add` decided on
// robot.creator_uid alone. Two consequences, and the second is the one that bites:
//
//   - a self-hosted agent that create refuses was accepted here from the same
//     user, which is exactly the hole D2's self-hosted clause is written about
//     ("a user cannot see it in the picker, so it should not be nameable in a
//     request");
//   - a DISABLED or ORPHANED bot read as "not an agent" at all and fell through to
//     the human branch, so an admin could seat it as an ordinary member. That seat
//     is then unreclaimable: queryOwnedAgentSeatsTx requires robot.status = 1, so
//     D13 never takes it when its owner leaves, and no cascade revisits an active
//     seat.
//
// Every refusal is the SAME code, matching create — the six reasons are not
// distinguishable on the wire.
func TestMembersAddAppliesTheSameAgentPredicateAsCreate(t *testing.T) {
	_, p := setup(t)
	stubAllMemberGroup(t, "grp_s4")
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	// Self-hosted, owned by the caller: create refuses it, so this must too.
	seedAgent(t, spaceA, "bot_selfhosted", "u_owner", "self_hosted")
	// robot.status = 0 — disabled. Seeded by hand because seedAgent only makes live ones.
	seedUser(t, "bot_disabled")
	_, err := testCtx.DB().UpdateBySql(
		"UPDATE `user` SET robot = 1 WHERE uid = ?", "bot_disabled").Exec()
	require.NoError(t, err)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, token, status, creator_uid, agent_hosting) "+
			"VALUES (?, ?, 0, ?, 'octo_hosted')",
		"bot_disabled", "tok-bot_disabled", "u_owner").Exec()
	require.NoError(t, err)
	seedSpaceMember(t, spaceA, "bot_disabled", 0, 1)
	// robot = 1 on `user` with NO robot row at all — an orphan.
	seedUser(t, "bot_orphan")
	_, err = testCtx.DB().UpdateBySql(
		"UPDATE `user` SET robot = 1 WHERE uid = ?", "bot_orphan").Exec()
	require.NoError(t, err)
	seedSpaceMember(t, spaceA, "bot_orphan", 0, 1)

	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "s4"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeResp(t, w)

	for _, bad := range []string{"bot_selfhosted", "bot_disabled", "bot_orphan"} {
		w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
			map[string]any{"uids": []string{bad}})
		require.Equal(t, http.StatusOK, w.Code, "case %s body: %s", bad, w.Body.String())

		var outcomes []memberOutcome
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &outcomes), "body: %s", w.Body.String())
		require.Len(t, outcomes, 1)
		assert.False(t, outcomes[0].OK, "case %s must be refused", bad)
		assert.Equal(t, reasonAgentNotEligible, outcomes[0].Reason,
			"case %s must render as the single agent refusal, like create", bad)

		assert.Nil(t, memberRow(t, resp.ProjectID, bad),
			"case %s must not have gained a project seat. For the disabled and orphan cases "+
				"that seat would be unreclaimable: D13 reads robot.status = 1, so no owner "+
				"departure would ever take it back", bad)
	}

	// The control: the same owner's live, octo-hosted agent still goes in.
	seedAgent(t, spaceA, "bot_good", "u_owner", "octo_hosted")
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"bot_good"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.NotNil(t, memberRow(t, resp.ProjectID, "bot_good"),
		"the predicate must still admit an eligible agent — a guard that refuses everything "+
			"would pass every assertion above")
}

// TestRebuildBringsTheWHOLERosterIntoTheNewGroup is PR #855's review, Q11.
//
// The rebuild used to seed the new group with nil members, on a comment claiming
// admitAllMemberGroup would fill the rest in one by one. Nothing did:
// admitAllMemberGroup runs only for the uids of the request that triggered the
// rebuild. A project already holding A and B, rebuilt while adding C, came out as
// {owner, C} — and A and B were then reachable only by an admin re-adding each of
// them, because scan B reports the gap and by decision does not repair it.
func TestRebuildBringsTheWHOLERosterIntoTheNewGroup(t *testing.T) {
	_, p := setup(t)
	stub := stubAllMemberGroup(t, "grp_rebuild_roster")
	stub.provisionErr = errStubProvision
	r := mountProject(t, p)

	seedSpace(t, spaceA, 1)
	owner := seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)
	for _, uid := range []string{"u_a", "u_b", "u_c"} {
		seedUser(t, uid)
		seedSpaceMember(t, spaceA, uid, 0, 1)
	}

	// Provisioning fails, so the project is created with no group.
	w := doOn(t, r, http.MethodPost, "/v1/space/"+spaceA+"/projects", owner,
		map[string]any{"name": "roster"})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	resp := decodeResp(t, w)
	require.Empty(t, allMemberGroupNoOf(t, resp.ProjectID))

	// A and B join while there is still no group. Their admissions no-op — there is
	// nothing to admit them into — which is the state this case is about.
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_a", "u_b"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Empty(t, allMemberGroupNoOf(t, resp.ProjectID), "still no group")

	// Now provisioning works again and C is added, triggering the rebuild.
	stub.provisionErr = nil
	stub.seeds = nil
	w = doOn(t, r, http.MethodPost, "/v1/projects/"+resp.ProjectID+"/members/add", owner,
		map[string]any{"uids": []string{"u_c"}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Equal(t, "grp_rebuild_roster", allMemberGroupNoOf(t, resp.ProjectID))

	require.Len(t, stub.seeds, 1, "the rebuild must have run exactly once")
	require.Equal(t, "u_owner", stub.seeds[0].Creator, "the group owner is the project's active owner")
	assert.ElementsMatch(t, []string{"u_a", "u_b"}, stub.seeds[0].Members,
		"the rebuild must carry the members who joined BEFORE it. The owner is not in "+
			"Members — CreateGroup adds them as creator — and u_c is not either, because "+
			"the seat that triggered this rebuild had not committed when the roster was "+
			"read; admitAllMemberGroup puts them in right after. Seeding nil here is what "+
			"left A and B out of their own project's group with no path back in")

	// And C really does land, so the two mechanisms together cover everyone.
	assert.Contains(t, stub.admitted, "u_c",
		"the triggering add still goes through the admitter")
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

	seedSpace(t, spaceA, 1)
	seedUser(t, "u_owner")
	seedSpaceMember(t, spaceA, "u_owner", 0, 1)

	model, err := p.createProjectOnce(createInput{
		SpaceID: spaceA, Creator: "u_owner", Name: "lease-fence",
		Discoverability: DiscoverabilitySpaceListed,
	})
	require.NoError(t, err)

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
