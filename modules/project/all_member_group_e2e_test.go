package project_test

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// TestAllMemberGroupDoesNotConsumeTheDailyGroupQuota pins D9 against the real
// provisioner.
//
// The daily cap (Group.SameDayCreateMaxCount) is a rule about a PERSON creating
// groups by hand; the all-member group is created by the platform on their behalf.
// Charging it to the creator's daily quota would mean a user near their cap could
// create a project and silently get one with no group — the failure mode D4 makes
// survivable but that nobody should be walked into by a quota they did not spend.
//
// Set to zero, which refuses even the first manual create, so the assertion cannot
// pass by the quota simply being generous. The provisioner reaches CreateGroup at
// the SERVICE layer and the cap lives in the HTTP handler, which is what makes this
// hold; a future refactor that pushed the cap down into the service would break it,
// and this case is what would say so.
func TestAllMemberGroupDoesNotConsumeTheDailyGroupQuota(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_quota_space"
		owner   = "e2e_quota_owner"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 2, 1)", spaceID, owner)
	exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", owner, owner, owner)

	cfg := ctx.GetConfig()
	restore := cfg.Group.SameDayCreateMaxCount
	cfg.Group.SameDayCreateMaxCount = 0
	t.Cleanup(func() { cfg.Group.SameDayCreateMaxCount = restore })

	token := seedToken(t, ctx, owner)
	resp := createProjectE2E(t, srv, spaceID, token, map[string]any{"name": "quota-free"})

	groupNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, groupNo,
		"the all-member group must be built even with the daily manual-create cap at zero: %v", resp)
	require.NotNil(t, groupRowE2E(t, ctx, groupNo), "and the group must really exist")
}

// TestSpaceRemovalTakesTheAgentOutOfEVERYProjectGroup is the P1 finding from PR
// #855's review, end to end.
//
// The Space-removal cascade closed a departing member's agents' project seats but
// enqueued nothing, on the reasoning that modules/group's cleanupSpaceMemberGroups
// already pulled the leaver's bots out with them (#354). That covers only the
// groups THE LEAVER IS IN: cleanupSpaceMemberGroups enumerates the departing
// person's groups and RemoveGroupMembers cascades their bots within those. An
// agent sitting in a project group its owner is not a member of was never visited.
//
// D15 makes that arrangement ordinary rather than exotic — any member may seat
// their own agent, and any member may create a project group — so this is the
// shape below: alice's agent is in a group bob created and alice never joined.
//
// The end state before the fix was an I2 violation nothing repairs: the agent
// holds no project seat and is still an active member of that group, its own
// space_member row was never touched so no Space cascade revisits it, and the I2
// scan is report-only. Alice, now outside the Space, keeps a proxy reading a
// project group.
//
// The all-member group is the CONTROL here: alice is in it, so the old code
// removed the agent from that one. Only the second group discriminates.
func TestSpaceRemovalTakesTheAgentOutOfEVERYProjectGroup(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_sr_space"
		owner   = "e2e_sr_owner"
		alice   = "e2e_sr_alice"
		bob     = "e2e_sr_bob"
		agent   = "e2e_sr_bot"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	for _, uid := range []string{owner, alice, bob} {
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
	}
	seedE2EAgent(t, ctx, spaceID, agent, alice)

	ownerToken := seedToken(t, ctx, owner)
	aliceToken := seedToken(t, ctx, alice)
	bobToken := seedToken(t, ctx, bob)

	resp := createProjectE2E(t, srv, spaceID, ownerToken, map[string]any{"name": "space-removal-e2e"})
	projectID, _ := resp["project_id"].(string)
	allMemberGroup, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, allMemberGroup)

	w := postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{alice, bob}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", aliceToken,
		map[string]any{"uids": []string{agent}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Bob creates an ordinary project group whose only member is alice's agent.
	// Through the real endpoint, so the I2 admission gate is what makes this state
	// legal: the agent is an active project member, therefore admissible. Seeding
	// the rows directly would prove the removal works on a state nothing can reach.
	w = postJSONE2E(t, srv, "/v1/group/create", bobToken, map[string]any{
		"space_id":   spaceID,
		"project_id": projectID,
		"members":    []string{agent},
	})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	var created struct {
		GroupNo string `json:"group_no"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &created), "body: %s", w.Body.String())
	otherGroup := created.GroupNo
	require.NotEmpty(t, otherGroup)

	require.Contains(t, liveGroupMembers(t, ctx, otherGroup), agent,
		"precondition: the agent is in a project group of its own")
	require.NotContains(t, liveGroupMembers(t, ctx, otherGroup), alice,
		"precondition: and its OWNER is not — that is what the old cascade could not see")

	// Alice loses her Space seat. Everything below is what the cascade must do.
	closed, err := spacemod.CloseAllSpaceSeats(ctx, alice, owner, spacemod.MemberRemoveReasonForceRemoved)
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
		"D13 — the agent's project seat must close when its owner loses their Space seat")

	require.Eventually(t, func() bool {
		return !contains(liveGroupMembers(t, ctx, otherGroup), agent)
	}, 60*time.Second, 500*time.Millisecond,
		"the agent must be out of a project group ITS OWNER WAS NEVER IN. This is the whole "+
			"finding: without a project-side removal job the group side only ever walks the "+
			"departing person's groups, so this one is missed and the agent keeps an active "+
			"membership with no project seat behind it — I2 broken, and nothing repairs it. "+
			"current members: %v", liveGroupMembers(t, ctx, otherGroup))

	assert.NotContains(t, liveGroupMembers(t, ctx, allMemberGroup), agent,
		"and out of the all-member group, which the old code did handle because alice was in it")
	assert.NotContains(t, liveGroupMembers(t, ctx, allMemberGroup), alice)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// allMemberGroupNoE2E reads the project's pointer from outside the package.
func allMemberGroupNoE2E(t *testing.T, ctx *config.Context, projectID string) string {
	t.Helper()
	var v []string
	_, err := ctx.DB().SelectBySql(
		"SELECT all_member_group_no FROM `octo_project` WHERE project_id = ?", projectID).Load(&v)
	require.NoError(t, err)
	require.Len(t, v, 1)
	return v[0]
}

// leakI1Seat produces the state modules/project's i1_abandoned_cleanup_leak gauge
// is named after: an active project seat whose Space seat is gone, with no cleanup
// job coming.
//
// Reachable in production, and terminal: P0's Space→Project cascade is a leased
// worker with a retry budget and an ABANDONED end state, and the gauge's own help
// text says "nothing re-drives these; a non-zero value needs manual repair".
func leakI1Seat(t *testing.T, ctx *config.Context, spaceID, uid string) {
	t.Helper()
	exec(t, ctx, "UPDATE space_member SET status = 0 WHERE space_id = ? AND uid = ?", spaceID, uid)
}

// TestRebuildProducesAGroupMatchingTheAdmissibleRoster is the behavioural
// invariant over D4's rebuild that PR #855's second review asked for, and it is
// deliberately stated as an invariant rather than as a regression case:
//
//	after a rebuild, the group's active member set equals the project's ADMISSIBLE
//	roster, and the group has exactly one creator who is an active project owner.
//
// Four blocking findings on this PR have now come out of ensureAllMemberGroup /
// provisionAllMemberGroup — the stale pointer that wedged the rebuild, the owner
// sync that could leave a group with no creator, the nil roster, and this one —
// and all four were invisible to tests that assert on what the STAND-IN
// provisioner was called with. This one asserts on the `group_member` rows that
// came out of the real one.
//
// The state under test is an I1 leak: a project member with no Space seat. The
// rebuild reads its inputs from octo_project_member and CreateGroup validates them
// against space_member, so before the fix ONE leaked member refused the whole
// rebuild — permanently, since the inputs are a deterministic total order and
// every later add repeats it verbatim. The project never gets a group, scan A
// reports it forever, and the repair the design points at is the broken thing.
func TestRebuildProducesAGroupMatchingTheAdmissibleRoster(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_rb_space"
		owner   = "e2e_rb_owner"
		leaked  = "e2e_rb_leaked"
		kept    = "e2e_rb_kept"
		late    = "e2e_rb_late"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	for _, uid := range []string{owner, leaked, kept, late} {
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
	}

	ownerToken := seedToken(t, ctx, owner)
	resp := createProjectE2E(t, srv, spaceID, ownerToken, map[string]any{"name": "rebuild-e2e"})
	projectID, _ := resp["project_id"].(string)
	firstGroup, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, firstGroup)

	w := postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{leaked, kept}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Put the project back into "provisioning never succeeded": the pointer is the
	// empty sentinel and the group it used to name is gone. This is scan A's state 1,
	// and it is what every rebuild starts from.
	exec(t, ctx, "UPDATE `group` SET status = 2 WHERE group_no = ?", firstGroup)
	exec(t, ctx, "UPDATE `octo_project` SET all_member_group_no = '', all_member_group_lease_until = NULL "+
		"WHERE project_id = ?", projectID)

	// One member becomes an I1 leak: project seat active, Space seat gone.
	leakI1Seat(t, ctx, spaceID, leaked)

	// Trigger the rebuild through a real add.
	w = postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{late}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	rebuilt := allMemberGroupNoE2E(t, ctx, projectID)
	require.NotEmpty(t, rebuilt,
		"the rebuild must produce a group. One project member with no Space seat used to "+
			"refuse the whole CreateGroup — and because the inputs are a deterministic total "+
			"order, every later add failed identically, so the project could never get one")
	require.NotEqual(t, firstGroup, rebuilt)

	// The invariant.
	require.Eventually(t, func() bool {
		live := liveGroupMembers(t, ctx, rebuilt)
		return len(live) == 3
	}, 30*time.Second, 250*time.Millisecond,
		"current members: %v", liveGroupMembers(t, ctx, rebuilt))
	assert.ElementsMatch(t, []string{owner, kept, late}, liveGroupMembers(t, ctx, rebuilt),
		"the group's active set must equal the project's ADMISSIBLE roster: everyone who "+
			"holds both a project seat and a Space seat. The leaked member is left out — they "+
			"are an I1 violation, scan B reports them as a gap, and that is strictly better "+
			"than no group at all")

	row := groupRowE2E(t, ctx, rebuilt)
	require.NotNil(t, row)
	assert.Equal(t, projectID, row.ProjectID)
	assert.Equal(t, owner, row.Creator, "and exactly one creator, who is an active project owner")
}

// TestRebuildSkipsAnOwnerWhoLostTheirSpaceSeat is leg A of the same finding.
//
// queryActiveOwnerForProvision picks the senior active project owner with no Space
// predicate, and hands it to CreateGroup, whose first act is a Space membership
// check on the creator. A senior owner who is an I1 leak therefore failed the
// rebuild — identically every time, because the pick is a deterministic total
// order — even though the project had a second owner who would have passed.
func TestRebuildSkipsAnOwnerWhoLostTheirSpaceSeat(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID     = "e2e_rbo_space"
		seniorOwner = "e2e_rbo_a_senior"
		juniorOwner = "e2e_rbo_b_junior"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, seniorOwner)
	for _, uid := range []string{seniorOwner, juniorOwner} {
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
	}

	seniorToken := seedToken(t, ctx, seniorOwner)
	resp := createProjectE2E(t, srv, spaceID, seniorToken, map[string]any{"name": "rebuild-owner-e2e"})
	projectID, _ := resp["project_id"].(string)
	firstGroup, _ := resp["all_member_group_no"].(string)

	w := postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", seniorToken,
		map[string]any{"uids": []string{juniorOwner}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	// Promoted by SQL: the role endpoint is a PUT and this case is about the
	// rebuild's owner pick, not about the promotion path.
	exec(t, ctx, "UPDATE `octo_project_member` SET role = 2 WHERE project_id = ? AND uid = ?",
		projectID, juniorOwner)

	exec(t, ctx, "UPDATE `group` SET status = 2 WHERE group_no = ?", firstGroup)
	exec(t, ctx, "UPDATE `octo_project` SET all_member_group_no = '', all_member_group_lease_until = NULL "+
		"WHERE project_id = ?", projectID)

	// The SENIOR owner — the one the deterministic pick returns first — is the leak.
	leakI1Seat(t, ctx, spaceID, seniorOwner)

	// A write path that is not the leaked owner's own triggers the rebuild.
	juniorToken := seedToken(t, ctx, juniorOwner)
	w = postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", juniorToken,
		map[string]any{"uids": []string{juniorOwner}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	rebuilt := allMemberGroupNoE2E(t, ctx, projectID)
	require.NotEmpty(t, rebuilt,
		"the rebuild must fall through to an owner who can actually hold the group")
	row := groupRowE2E(t, ctx, rebuilt)
	require.NotNil(t, row)
	assert.Equal(t, juniorOwner, row.Creator,
		"the senior owner has no Space seat, so CreateGroup would refuse them as creator; "+
			"the pick must move on rather than failing the rebuild forever")
}

// putJSONE2E is postJSONE2E for PUT, which the role-change route uses.
func putJSONE2E(t *testing.T, srv *server.Server, path, token string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPut, path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

// groupCreatorsE2E returns the uids currently holding role=creator in a group.
//
// Plural on purpose: "how many creators are there" is half of what the owner sync
// is for, and a test that read a single row could not tell one creator from two.
func groupCreatorsE2E(t *testing.T, ctx *config.Context, groupNo string) []string {
	t.Helper()
	var uids []string
	_, err := ctx.DB().SelectBySql(
		"SELECT uid FROM group_member WHERE group_no = ? AND role = 1 AND is_deleted = 0 ORDER BY uid",
		groupNo,
	).Load(&uids)
	require.NoError(t, err)
	return uids
}

// TestSpaceRemovalLeavesTheAllMemberGroupOwnedByAProjectOwner is the fourth departure
// path's owner convergence — the one that was missing.
//
// A project owner can lose their seat four ways: kicked, left, role changed, and
// removed from the Space. The first three call the D6 owner sync directly. This one
// did not, and what happens instead is precisely what the leave path's own comment
// says the sync exists to prevent: the group cascade hands the group over by GROUP
// seniority, with no project-role filter, so it can land on an ordinary member — and
// D7 then forbids that person from transferring, exiting, disbanding or blacklisting
// the group. Nobody can move it, and no scan reports it: I4's scans compare member
// SETS, never creator-versus-owner.
//
// The seniority order below is the point of the setup: bob joins before alice, so the
// group-seniority handover picks BOB, who is an ordinary member — while alice, the
// project's other owner, is the right answer. If the two orders agreed, this test
// would pass with the convergence deleted.
func TestSpaceRemovalLeavesTheAllMemberGroupOwnedByAProjectOwner(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_own_space"
		admin   = "e2e_own_admin" // the Space's creator; never in the project
		powner  = "e2e_own_owner" // the project's creator, and the group's creator
		bob     = "e2e_own_bob"   // ordinary member, SENIOR in the group
		alice   = "e2e_own_alice" // promoted to project owner, JUNIOR in the group
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, admin)
	for _, uid := range []string{admin, powner, bob, alice} {
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
	}
	ownerToken := seedToken(t, ctx, powner)

	resp := createProjectE2E(t, srv, spaceID, ownerToken, map[string]any{"name": "owner-converge-e2e"})
	projectID, _ := resp["project_id"].(string)
	allMemberGroup, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, allMemberGroup)

	// Two adds, not one, so the group's created_at order is bob then alice.
	w := postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{bob}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	w = postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{alice}})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// Backdate bob's group row so the seniority pick is DETERMINISTIC.
	//
	// group_member.created_at is second-granular and both adds land in the same
	// second, and querySecondOldestNonBotMemberTx orders by created_at with no
	// tie-break — so without this the successor is whichever row the storage engine
	// hands back first, which happened to be alice. That made the first version of
	// this test pass with the convergence deleted: it was asserting a coin flip.
	exec(t, ctx, "UPDATE group_member SET created_at = DATE_SUB(created_at, INTERVAL 1 HOUR) "+
		"WHERE group_no = ? AND uid = ?", allMemberGroup, bob)

	// Alice becomes the project's second owner. The group's creator (powner) is still
	// an active owner, so this sync is a no-op — which is what makes the assertion at
	// the end about the Space-removal path and not about this call.
	w = putJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/"+alice+"/role", ownerToken,
		map[string]any{"role": 2})
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	require.Equal(t, []string{powner}, groupCreatorsE2E(t, ctx, allMemberGroup),
		"precondition: the project's creator owns the group")

	// The group owner loses their Space seat. Everything after this is the cascade.
	closed, err := spacemod.CloseAllSpaceSeats(ctx, powner, admin, spacemod.MemberRemoveReasonForceRemoved)
	require.NoError(t, err)
	require.Equal(t, []string{spaceID}, closed)

	assert.Eventually(t, func() bool {
		creators := groupCreatorsE2E(t, ctx, allMemberGroup)
		return len(creators) == 1 && creators[0] == alice
	}, 60*time.Second, 500*time.Millisecond, "waiting for the owner convergence")
	// Re-read for the failure message: Eventually's message arguments are evaluated
	// at the call site, before it waits, so a value passed there reports the state
	// BEFORE the cascade ran — which is exactly the wrong thing to print.
	require.Equal(t, []string{alice}, groupCreatorsE2E(t, ctx, allMemberGroup),
		"the all-member group must end up owned by an ACTIVE PROJECT OWNER. The group cascade "+
			"hands over by group seniority, which here picks bob — an ordinary project member "+
			"whom D7 then forbids from transferring, exiting, disbanding or blacklisting the "+
			"group. Nothing repairs that: it self-heals only if the project later sees a kick, "+
			"a leave or a role change, and no scan reports it because I4 compares member sets, "+
			"not creator-versus-owner")

	assert.NotContains(t, liveGroupMembers(t, ctx, allMemberGroup), powner,
		"and the departed owner is out of the group entirely")
	assert.Contains(t, liveGroupMembers(t, ctx, allMemberGroup), bob,
		"while bob stays a member — the convergence demotes, it does not evict")
}

// TestRebuildAtAFullRosterStaysInsideAnHTTPBudget measures the one thing the
// description carried as unmeasured: what a full-roster rebuild costs the caller
// who happens to trigger it.
//
// The rebuild is synchronous, inside the HTTP request, and D15 widened the caller
// set — an ordinary project member adding their own agent now reaches it. The worst
// case is this one: a CAS claim, a roster read of up to max_members+1 rows, a
// batched Space-active read, then CreateGroup inserting ~500 member rows with a
// Redis GenSeq each plus one blocking IM call, then the write-back.
//
// The ceiling is deliberately loose. This is not a benchmark and the runner is
// shared, so a tight bound would be a flake generator; what it has to catch is the
// difference between "seconds" and "the client times out". The measured number goes
// in the log line, which is the part that answers the review.
//
// Reachable only while a project has no usable group, and the lease serialises
// concurrent triggers, so exactly one caller pays this.
func TestRebuildAtAFullRosterStaysInsideAnHTTPBudget(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID    = "e2e_perf_space"
		owner      = "e2e_perf_owner"
		late       = "e2e_perf_late"
		rosterSize = 500 // the default OCTO_PROJECT_MAX_MEMBERS
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	for _, uid := range []string{owner, late} {
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
	}

	ownerToken := seedToken(t, ctx, owner)
	resp := createProjectE2E(t, srv, spaceID, ownerToken, map[string]any{"name": "rebuild-perf-e2e"})
	projectID, _ := resp["project_id"].(string)
	firstGroup, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, firstGroup)

	// Fill the roster by seeding rows rather than by calling the add endpoint 498
	// times: what is being measured is the rebuild, not the adds.
	for i := 0; i < rosterSize-2; i++ {
		uid := fmt.Sprintf("e2e_perf_m%03d", i)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", uid, uid, uid)
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)", spaceID, uid)
		exec(t, ctx, "INSERT INTO `octo_project_member` "+
			"(project_id, uid, space_id, role, status, removing, invite_uid, created_at, updated_at) "+
			"VALUES (?, ?, ?, 0, 1, 0, ?, NOW(3), NOW(3))", projectID, uid, spaceID, owner)
	}

	// Back to "provisioning never succeeded", which is where every rebuild starts.
	exec(t, ctx, "UPDATE `group` SET status = 2 WHERE group_no = ?", firstGroup)
	exec(t, ctx, "UPDATE `octo_project` SET all_member_group_no = '', all_member_group_lease_until = NULL "+
		"WHERE project_id = ?", projectID)

	start := time.Now()
	w := postJSONE2E(t, srv, "/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"uids": []string{late}})
	elapsed := time.Since(start)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	rebuilt := allMemberGroupNoE2E(t, ctx, projectID)
	require.NotEmpty(t, rebuilt, "the request that pays for the rebuild must also produce one")

	live := liveGroupMembers(t, ctx, rebuilt)
	t.Logf("full-roster rebuild: %d members admitted in %s (request end to end)", len(live), elapsed)
	assert.GreaterOrEqual(t, len(live), rosterSize,
		"the rebuild must admit the whole roster — a capped rebuild is the incomplete-group "+
			"defect the third review established, so a fast run that admitted fewer members "+
			"would be measuring the wrong thing")
	assert.Less(t, elapsed, 60*time.Second,
		"a full-roster rebuild must stay well inside any sane HTTP budget. If this fires, the "+
			"answer is not a bigger number: it is to stop doing the rebuild inside the request "+
			"that triggered it, because the seats have already committed and the caller gets a "+
			"timeout on an operation that half-succeeded")
}
