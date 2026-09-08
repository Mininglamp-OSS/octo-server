package project_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/server"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// End-to-end coverage of the all-member group, through the REAL modules/group
// implementation.
//
// # Why this belongs in the external test package, and why it is possible at all
//
// modules/project must never import modules/group, so the in-package tests drive
// the feature through stand-in hooks. That constrains the PACKAGE, not the test
// BINARY: this external package blank-imports octo-server/internal, so
// module.Setup registers every module and modules/group's real provisioner,
// admitter, owner sync and rename are all live here.
//
// So "the boundary cannot be tested end to end" was wrong, and it was wrong in
// the direction that matters — the seam between the two modules is exactly where
// a stand-in proves nothing. What the stand-ins cover is the registry contract
// (the hook runs after commit; its failure never fails the caller). What only
// these cases cover is that a real project ends up owning a real group with real
// members in it.
//
// These need a WuKongIM broker, because a real CreateGroup creates an IM channel
// and rolls the group back if it cannot. That is the same requirement
// modules/group's own suite already carries.

// newE2EServer is testutil.NewTestServer plus the i18n error renderer.
//
// testutil.NewTestServer does not install one (main.go does), and without it a
// refused request falls back to the legacy {msg,status} body with no error.code —
// so an assertion on a code would silently compare against "" and pass for the
// wrong reason. api_test.go's TestMain wires it on the shared server for the same
// reason; these cases build their own, so they have to wire their own.
func newE2EServer(t *testing.T) (*server.Server, *config.Context) {
	t.Helper()
	srv, ctx := testutil.NewTestServer()
	srv.GetRoute().SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	require.NoError(t, testutil.CleanAllTables(ctx))
	return srv, ctx
}

// createProjectE2E drives the real create endpoint and returns the decoded response.
func createProjectE2E(t *testing.T, srv *server.Server, spaceID, token string, body map[string]any) map[string]any {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/v1/space/"+spaceID+"/projects", bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "body: %s", w.Body.String())
	return resp
}

func postJSONE2E(t *testing.T, srv *server.Server, path, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

// liveGroupMembers returns the group's active member uids.
func liveGroupMembers(t *testing.T, ctx *config.Context, groupNo string) []string {
	t.Helper()
	var uids []string
	_, err := ctx.DB().SelectBySql(
		"SELECT uid FROM group_member WHERE group_no = ? AND is_deleted = 0 AND status = 1 ORDER BY uid",
		groupNo,
	).Load(&uids)
	require.NoError(t, err)
	return uids
}

type e2eGroupRow struct {
	GroupNo   string `db:"group_no"`
	Name      string `db:"name"`
	Creator   string `db:"creator"`
	SpaceID   string `db:"space_id"`
	ProjectID string `db:"project_id"`
	Status    int    `db:"status"`
}

func groupRowE2E(t *testing.T, ctx *config.Context, groupNo string) *e2eGroupRow {
	t.Helper()
	var rows []*e2eGroupRow
	_, err := ctx.DB().SelectBySql(
		"SELECT group_no, name, creator, space_id, project_id, status FROM `group` WHERE group_no = ?",
		groupNo,
	).Load(&rows)
	require.NoError(t, err)
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

// seedE2EAgent creates a bot exactly as botfather mints one: a robot=1 user, an
// active robot row owned by ownerUID, and a Space seat.
func seedE2EAgent(t *testing.T, ctx *config.Context, spaceID, agentUID, ownerUID string) {
	t.Helper()
	exec(t, ctx, "INSERT INTO `user` (uid, name, short_no, robot) VALUES (?, ?, ?, 1)",
		agentUID, "agent-"+agentUID, agentUID)
	exec(t, ctx, "INSERT INTO robot (robot_id, token, status, creator_uid, agent_hosting) VALUES (?, ?, 1, ?, 'octo_hosted')",
		agentUID, "tok-"+agentUID, ownerUID)
	exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)",
		spaceID, agentUID)
}

// TestCreateProjectProducesARealAllMemberGroup is the case the stand-in tests
// cannot give: one create request, and afterwards a real group row exists,
// attributed to the project, owned by the creator, holding the creator and the
// agents they picked.
//
// Every layer is the production one — the create handler, the seat writes, the
// provisioner hook, modules/group's CreateGroup, the I2 admission gate inside the
// group transaction, and the IM channel.
//
// The agent assertion is the load-bearing half. The gate refuses any uid that is
// not an active project member, and the agents' seats are written in the create
// transaction that commits BEFORE the hook runs. So an agent appearing in the
// group is evidence that the ordering actually holds; if the seats landed after
// the group, or not at all, the gate would refuse them and the group would come
// back with only its creator.
func TestCreateProjectProducesARealAllMemberGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_amg_space"
		owner   = "e2e_amg_owner"
		agentA  = "e2e_amg_bot_a"
		agentB  = "e2e_amg_bot_b"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 2, 1)", spaceID, owner)
	exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", owner, owner, owner)
	seedE2EAgent(t, ctx, spaceID, agentA, owner)
	seedE2EAgent(t, ctx, spaceID, agentB, owner)

	token := seedToken(t, ctx, owner)
	resp := createProjectE2E(t, srv, spaceID, token, map[string]any{
		"name":       "供应链运营协同",
		"agent_uids": []string{agentA, agentB},
	})

	groupNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, groupNo,
		"a real provisioner is registered here, so the create must come back with a group: %v", resp)

	row := groupRowE2E(t, ctx, groupNo)
	require.NotNil(t, row, "the group must really exist, not just be named in the response")
	assert.Equal(t, resp["project_id"], row.ProjectID, "the group must be attributed to the project")
	assert.Equal(t, spaceID, row.SpaceID)
	assert.Equal(t, owner, row.Creator, "the project owner owns the group (D6)")
	assert.Equal(t, "供应链运营协同", row.Name, "the group is named after the project (D8)")

	assert.ElementsMatch(t, []string{owner, agentA, agentB}, liveGroupMembers(t, ctx, groupNo),
		"the creator and both picked agents must be IN the group. The admission gate refuses "+
			"any uid that is not an active project member, so this also proves the agent seats "+
			"were committed before the provisioner ran")
}

// TestAddingAProjectMemberPutsThemInTheRealAllMemberGroup covers D12 against the
// real admitter: the seat and the group membership are two writes in two modules,
// and only an end-to-end case shows that the second one happens.
func TestAddingAProjectMemberPutsThemInTheRealAllMemberGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID  = "e2e_add_space"
		owner    = "e2e_add_owner"
		newcomer = "e2e_add_new"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	for _, uid := range []string{owner, newcomer} {
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
	}

	token := seedToken(t, ctx, owner)
	resp := createProjectE2E(t, srv, spaceID, token, map[string]any{"name": "add-e2e"})
	projectID, _ := resp["project_id"].(string)
	groupNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, groupNo)
	require.Equal(t, []string{owner}, liveGroupMembers(t, ctx, groupNo),
		"a project created with no agents still gets a group — with just its owner in it, "+
			"which is the case CreateGroup used to refuse outright")

	w := postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", token,
		map[string]any{"uids": []string{newcomer}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	assert.ElementsMatch(t, []string{owner, newcomer}, liveGroupMembers(t, ctx, groupNo),
		"joining the project must put the member in its all-member group (D12)")
}

// TestRemovingAProjectMemberTakesThemAndTheirAgentOutOfTheRealGroup drives D13
// all the way through: the project seat closes, the outbox job is written, the
// worker claims it, and modules/group's real detach removes both the member and
// the agent they brought.
//
// This is the case that would have caught the D13 gap had it existed at the group
// boundary rather than in the project seat: the group side removes a leaver's
// bots by robot.creator_uid on its own, so an agent whose project seat stayed
// open would be out of the group and still on the roster — I4 broken, and broken
// silently.
func TestRemovingAProjectMemberTakesThemAndTheirAgentOutOfTheRealGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_rm_space"
		owner   = "e2e_rm_owner"
		member  = "e2e_rm_member"
		agent   = "e2e_rm_bot"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	for _, uid := range []string{owner, member} {
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
	}
	seedE2EAgent(t, ctx, spaceID, agent, member)

	ownerToken := seedToken(t, ctx, owner)
	memberToken := seedToken(t, ctx, member)

	resp := createProjectE2E(t, srv, spaceID, ownerToken, map[string]any{"name": "rm-e2e"})
	projectID, _ := resp["project_id"].(string)
	groupNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, groupNo)

	// The member joins, then brings their own agent (D15 gives an ordinary member
	// exactly that and nothing else).
	w := postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{member}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", memberToken,
		map[string]any{"uids": []string{agent}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.ElementsMatch(t, []string{owner, member, agent}, liveGroupMembers(t, ctx, groupNo),
		"precondition: all three are in the group before the removal")

	// Remove the member. The agent must go with them.
	w = postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{member}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// The cascade is an outbox worker, so wait for the outcome rather than sleeping.
	// The budget matches the one the C2 case above uses and for the same reason: a
	// batch left in flight by an earlier test makes this job wait for a scheduled
	// tick instead of the kick.
	require.Eventually(t, func() bool {
		live := liveGroupMembers(t, ctx, groupNo)
		return len(live) == 1 && live[0] == owner
	}, 60*time.Second, 500*time.Millisecond,
		"the departing member AND their agent must both leave the all-member group; "+
			"an agent left holding a project seat while out of the group is I4 broken "+
			"with nothing to repair it. current members: %v", liveGroupMembers(t, ctx, groupNo))

	// And the agent's project seat really is closed, not merely its group row.
	var statuses []int
	_, err := ctx.DB().SelectBySql(
		"SELECT status FROM `octo_project_member` WHERE project_id = ? AND uid = ?", projectID, agent,
	).Load(&statuses)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, 0, statuses[0], "D13 — the agent's project seat must be closed too")
}

// TestDisbandingAProjectLeavesItsAllMemberGroupAsAnOrdinaryGroup pins D10 against
// the real detach step: the group survives with its members, and stops being the
// project's.
func TestDisbandingAProjectLeavesItsAllMemberGroupAsAnOrdinaryGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_dis_space"
		owner   = "e2e_dis_owner"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, owner)
	exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", owner, owner, owner)

	token := seedToken(t, ctx, owner)
	resp := createProjectE2E(t, srv, spaceID, token, map[string]any{"name": "dis-e2e"})
	projectID, _ := resp["project_id"].(string)
	groupNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, groupNo)

	req, err := http.NewRequest(http.MethodDelete, "/v1/projects/"+projectID, nil)
	require.NoError(t, err)
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// The disband steps run after the commit, so wait for the detach.
	require.Eventually(t, func() bool {
		row := groupRowE2E(t, ctx, groupNo)
		return row != nil && row.ProjectID == ""
	}, 30*time.Second, 200*time.Millisecond,
		"the group must revert to Space-direct (D10)")

	row := groupRowE2E(t, ctx, groupNo)
	require.NotNil(t, row)
	assert.NotEqual(t, 2, row.Status, "the group must NOT be disbanded with the project")
	assert.Equal(t, []string{owner}, liveGroupMembers(t, ctx, groupNo),
		"its members are left untouched — the group becomes an ordinary Space group")

	// And the project no longer points at it, so nothing treats it as protected.
	var pointer []string
	_, err = ctx.DB().SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` WHERE project_id = ?", projectID,
	).Load(&pointer)
	require.NoError(t, err)
	require.Equal(t, []string{""}, pointer)
}

// TestProtectedOperationsAreRefusedOnARealAllMemberGroup drives D7 against a group
// that was really provisioned, rather than one a fixture declared to be the
// all-member group.
//
// The difference matters: the predicate requires the project's pointer AND the
// group's own project_id to agree, and only a real provisioning run sets both.
func TestProtectedOperationsAreRefusedOnARealAllMemberGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_d7_space"
		owner   = "e2e_d7_owner"
		member  = "e2e_d7_member"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	for _, uid := range []string{owner, member} {
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
	}

	ownerToken := seedToken(t, ctx, owner)
	memberToken := seedToken(t, ctx, member)
	resp := createProjectE2E(t, srv, spaceID, ownerToken, map[string]any{"name": "d7-e2e"})
	projectID, _ := resp["project_id"].(string)
	groupNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, groupNo)

	w := postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{member}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// The member cannot leave the group; they have to leave the project.
	w = postJSONE2E(t, srv, "/v1/groups/"+groupNo+"/exit", memberToken, nil)
	assert.Contains(t, w.Body.String(), "err.server.group.all_member_group_protected",
		"exit must be refused on a real all-member group: body=%s", w.Body.String())
	assert.ElementsMatch(t, []string{owner, member}, liveGroupMembers(t, ctx, groupNo),
		"and nobody may actually have left")

	// The owner cannot disband it either.
	req, err := http.NewRequest(http.MethodDelete, "/v1/groups/"+groupNo+"/disband", nil)
	require.NoError(t, err)
	req.Header.Set("token", ownerToken)
	rec := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(rec, req)
	assert.Contains(t, rec.Body.String(), "err.server.group.all_member_group_protected",
		"disband must be refused: body=%s", rec.Body.String())

	row := groupRowE2E(t, ctx, groupNo)
	require.NotNil(t, row)
	assert.NotEqual(t, 2, row.Status, "the group must not be disbanded")

	// Leaving the PROJECT is the route that works, and it takes the member out of
	// the group — which is the whole point of refusing the group-side exit.
	w = postJSONE2E(t, srv, "/v1/projects/"+projectID+"/leave", memberToken, map[string]any{})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Eventually(t, func() bool {
		live := liveGroupMembers(t, ctx, groupNo)
		return len(live) == 1 && live[0] == owner
	}, 60*time.Second, 500*time.Millisecond,
		"leaving the project is the supported way out, and it must actually work. "+
			"current members: %v", liveGroupMembers(t, ctx, groupNo))
}

// TestRenamingAProjectRenamesTheRealAllMemberGroup pins D8 through the real
// rename hook, including the truncation the group side owns (a project name may
// be 64 runes, a group name 50).
func TestRenamingAProjectRenamesTheRealAllMemberGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_ren_space"
		owner   = "e2e_ren_owner"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, owner)
	exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", owner, owner, owner)

	token := seedToken(t, ctx, owner)
	resp := createProjectE2E(t, srv, spaceID, token, map[string]any{"name": "before"})
	projectID, _ := resp["project_id"].(string)
	groupNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, groupNo)

	payload, err := json.Marshal(map[string]any{"name": "after"})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPut, "/v1/projects/"+projectID, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Eventually(t, func() bool {
		row := groupRowE2E(t, ctx, groupNo)
		return row != nil && row.Name == "after"
	}, 20*time.Second, 200*time.Millisecond,
		"the all-member group's name must follow the project's (D8)")
}

// TestDeletingAnAgentClosesItsProjectSeatAndRemovesItFromTheGroup is D14 end to
// end, and it is the case that shows why D14 had to exist.
//
// The bot's Space seats close through the removal outbox, P0's step closes its
// project seat, and P1's step takes it out of the project's groups. Before D14 the
// seat was closed by a bare UPDATE that enqueued nothing, so none of that ran: the
// agent stayed on the project roster forever.
//
// Driven through modules/space's exported entry rather than BotFather's chat
// command, because the command needs a whole conversation to be scripted; the
// entry point is the thing D14 added and the thing botfather now calls.
func TestDeletingAnAgentClosesItsProjectSeatAndRemovesItFromTheGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_del_space"
		owner   = "e2e_del_owner"
		agent   = "e2e_del_bot"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, owner)
	exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", owner, owner, owner)
	seedE2EAgent(t, ctx, spaceID, agent, owner)

	token := seedToken(t, ctx, owner)
	resp := createProjectE2E(t, srv, spaceID, token, map[string]any{
		"name":       "del-e2e",
		"agent_uids": []string{agent},
	})
	projectID, _ := resp["project_id"].(string)
	groupNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, groupNo)
	require.ElementsMatch(t, []string{owner, agent}, liveGroupMembers(t, ctx, groupNo))

	// The bot is deleted: every Space seat closes, each leaving a cleanup job.
	closed, err := spacemod.CloseAllSpaceSeats(ctx, agent, agent, spacemod.MemberRemoveReasonBotDeleted)
	require.NoError(t, err)
	require.Equal(t, []string{spaceID}, closed)

	require.Eventually(t, func() bool {
		var statuses []int
		if _, err := ctx.DB().SelectBySql(
			"SELECT status FROM `octo_project_member` WHERE project_id = ? AND uid = ?", projectID, agent,
		).Load(&statuses); err != nil {
			return false
		}
		return len(statuses) == 1 && statuses[0] == 0
	}, 60*time.Second, 500*time.Millisecond,
		"a deleted agent's PROJECT seat must close through the cascade — the defect D14 fixes "+
			"is that it never did, leaving a nonexistent account on the roster permanently")

	require.Eventually(t, func() bool {
		live := liveGroupMembers(t, ctx, groupNo)
		return len(live) == 1 && live[0] == owner
	}, 60*time.Second, 500*time.Millisecond,
		"and it must be out of the all-member group. current members: %v",
		liveGroupMembers(t, ctx, groupNo))
}
