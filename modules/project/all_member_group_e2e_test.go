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
// module.Setup registers every module and modules/group's real provisioner and
// rename hook are live here.
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

// writeProjectE2E drives one authenticated JSON write through the real Project route.
func writeProjectE2E(
	t *testing.T,
	srv *server.Server,
	method, path, token string,
	body map[string]any,
) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	req, err := http.NewRequest(method, path, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	return w
}

func projectSeatStateE2E(ctx *config.Context, projectID, uid string) (status, removing int, ok bool) {
	var rows []struct {
		Status   int `db:"status"`
		Removing int `db:"removing"`
	}
	_, err := ctx.DB().SelectBySql(
		"SELECT status, removing FROM octo_project_member WHERE project_id = ? AND uid = ?",
		projectID, uid,
	).Load(&rows)
	if err != nil || len(rows) == 0 {
		return 0, 0, false
	}
	return rows[0].Status, rows[0].Removing, true
}

func activeGroupMemberE2E(ctx *config.Context, groupNo, uid string) bool {
	var uids []string
	_, err := ctx.DB().SelectBySql(
		"SELECT uid FROM group_member WHERE group_no = ? AND uid = ? "+
			"AND is_deleted = 0 AND status = 1",
		groupNo, uid,
	).Load(&uids)
	return err == nil && len(uids) == 1
}
func activeGroupMemberRoleE2E(ctx *config.Context, groupNo, uid string) (role int, ok bool) {
	var roles []int
	_, err := ctx.DB().SelectBySql(
		"SELECT role FROM group_member WHERE group_no = ? AND uid = ? "+
			"AND is_deleted = 0 AND status = 1",
		groupNo, uid,
	).Load(&roles)
	if err != nil || len(roles) == 0 {
		return 0, false
	}
	return roles[0], true
}

func latestRemovalJobStatusE2E(ctx *config.Context, projectID, uid string) (int, bool) {
	var statuses []int
	_, err := ctx.DB().SelectBySql(
		"SELECT status FROM octo_project_member_removal_cleanup "+
			"WHERE project_id = ? AND uid = ? ORDER BY id DESC LIMIT 1",
		projectID, uid,
	).Load(&statuses)
	if err != nil || len(statuses) == 0 {
		return 0, false
	}
	return statuses[0], true
}

// TestProjectLifecycleKeepsDedicatedAndOrdinaryGroupsSeparate covers the live
// projection contract through Project's real HTTP writes and Group's real
// provision/admission/removal hooks. A second Project-associated group is
// deliberately seeded with the same project_id: it must remain a native
// snapshot while only the pointer-linked all-member group follows the roster.
func TestProjectLifecycleKeepsDedicatedAndOrdinaryGroupsSeparate(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID = "e2e_lifecycle_space"
		owner   = "e2e_lifecycle_owner"
		targetA = "e2e_lifecycle_target_a"
		targetB = "e2e_lifecycle_target_b"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, owner)
	for _, uid := range []string{owner, targetA, targetB} {
		role := 0
		if uid == owner {
			role = 2
		}
		exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, ?, 1)",
			spaceID, uid, role)
		exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)",
			uid, uid, uid)
	}

	ownerToken := seedToken(t, ctx, owner)
	targetAToken := seedToken(t, ctx, targetA)
	resp := createProjectE2E(t, srv, spaceID, ownerToken, map[string]any{
		"name": "生命周期投影",
	})
	projectID, _ := resp["project_id"].(string)
	dedicatedNo, _ := resp["all_member_group_no"].(string)
	require.NotEmpty(t, projectID)
	require.NotEmpty(t, dedicatedNo)

	// This group shares the Project relation but is not the Project's
	// all_member_group_no pointer. It is intentionally independent.
	ordinaryNo := "e2e_lifecycle_ordinary"
	exec(t, ctx,
		"INSERT INTO `group` (group_no, name, creator, status, version, space_id, project_id) "+
			"VALUES (?, ?, ?, 1, 1, ?, ?)",
		ordinaryNo, ordinaryNo, owner, spaceID, projectID)
	for _, uid := range []string{owner, targetA, targetB} {
		role := 0
		if uid == owner {
			role = 1
		}
		exec(t, ctx,
			"INSERT INTO group_member (group_no, uid, role, is_deleted, status, version) "+
				"VALUES (?, ?, ?, 0, 1, 1)",
			ordinaryNo, uid, role)
	}

	w := writeProjectE2E(t, srv, http.MethodPost,
		"/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"members": []map[string]any{
			{"uid": targetA, "role": 0},
			{"uid": targetB, "role": 0},
		}})
	require.Equal(t, http.StatusOK, w.Code, "add body: %s", w.Body.String())
	assert.ElementsMatch(t, []string{owner, targetA, targetB},
		liveGroupMembers(t, ctx, dedicatedNo),
		"Project admission updates only the pointer-linked dedicated group")
	assert.ElementsMatch(t, []string{owner, targetA, targetB},
		liveGroupMembers(t, ctx, ordinaryNo),
		"the ordinary associated group keeps its native snapshot")

	w = writeProjectE2E(t, srv, http.MethodPut,
		"/v1/projects/"+projectID+"/owner", ownerToken,
		map[string]any{"uid": targetA})
	require.Equal(t, http.StatusOK, w.Code, "transfer body: %s", w.Body.String())
	require.Eventually(t, func() bool {
		role, ok := activeGroupMemberRoleE2E(ctx, dedicatedNo, targetA)
		return ok && role == 1
	}, 20*time.Second, 200*time.Millisecond,
		"the dedicated group's native owner role must follow the human Project owner")

	// Transfer back so owner can exercise the removal endpoint. The second
	// transfer also proves that owner synchronization is not one-way.
	w = writeProjectE2E(t, srv, http.MethodPut,
		"/v1/projects/"+projectID+"/owner", targetAToken,
		map[string]any{"uid": owner})
	require.Equal(t, http.StatusOK, w.Code, "transfer-back body: %s", w.Body.String())
	require.Eventually(t, func() bool {
		role, ok := activeGroupMemberRoleE2E(ctx, dedicatedNo, owner)
		return ok && role == 1
	}, 20*time.Second, 200*time.Millisecond,
		"the dedicated group's native owner role must follow the transfer back")

	// A completed removal must leave the ordinary associated group's native
	// member untouched.
	w = writeProjectE2E(t, srv, http.MethodPost,
		"/v1/projects/"+projectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{targetB}})
	require.Equal(t, http.StatusOK, w.Code, "remove body: %s", w.Body.String())
	require.Eventually(t, func() bool {
		status, removing, ok := projectSeatStateE2E(ctx, projectID, targetB)
		return ok && status == 0 && removing == 0 &&
			!activeGroupMemberE2E(ctx, dedicatedNo, targetB)
	}, 40*time.Second, 200*time.Millisecond,
		"the two-phase removal must close the Project seat and dedicated membership")
	assert.False(t, activeGroupMemberE2E(ctx, dedicatedNo, targetB))
	assert.True(t, activeGroupMemberE2E(ctx, ordinaryNo, targetB),
		"removing a Project member must not mutate an ordinary associated group")
	jobStatus, ok := latestRemovalJobStatusE2E(ctx, projectID, targetB)
	require.True(t, ok)
	assert.Equal(t, 1, jobStatus, "the completed removal outbox row must be terminal done")

	// Remove and immediately re-add targetA. The admission transaction clears
	// removing and retires the pending job before the worker's next poll; the
	// dedicated projection must therefore retain the rejoined member.
	w = writeProjectE2E(t, srv, http.MethodPost,
		"/v1/projects/"+projectID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{targetA}})
	require.Equal(t, http.StatusOK, w.Code, "remove-for-rejoin body: %s", w.Body.String())
	w = writeProjectE2E(t, srv, http.MethodPost,
		"/v1/projects/"+projectID+"/members/add", ownerToken,
		map[string]any{"members": []map[string]any{{"uid": targetA, "role": 0}}})
	require.Equal(t, http.StatusOK, w.Code, "rejoin body: %s", w.Body.String())
	require.Eventually(t, func() bool {
		status, removing, seatOK := projectSeatStateE2E(ctx, projectID, targetA)
		return seatOK && status == 1 && removing == 0 &&
			activeGroupMemberE2E(ctx, dedicatedNo, targetA)
	}, 20*time.Second, 200*time.Millisecond,
		"rejoin must cancel the fence and preserve the dedicated projection")
	assert.True(t, activeGroupMemberE2E(ctx, ordinaryNo, targetA),
		"rejoin must leave ordinary associated membership untouched")
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
// provisioner hook, modules/group's Project-group transaction, and the IM channel.
//
// The agent assertion is the load-bearing half. The agents' seats are written in
// the create transaction that commits BEFORE the hook runs. So an agent appearing
// in the group is evidence that provisioning consumed the committed Project
// roster; if the seats landed after the group, or not at all, the real group
// would come back with only its creator.
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
