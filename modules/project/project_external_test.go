// Package project_test exercises Project HTTP routes with the complete
// module registry; the external package avoids an import cycle with Group.
package project_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	spacemod "github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	// Every module, so module.Setup applies the full schema and mounts the full route
	// tree — which is exactly what makes the boot smoke test meaningful.
	_ "github.com/Mininglamp-OSS/octo-server/internal"
	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
)

// TestServerBootsWithProjectRegistered is verification C1.
//
// The failure mode this catches is a PANIC during init or Route, not a wrong response —
// and because modules/project is blank-imported from internal/modules.go, such a panic
// takes down the whole process rather than just the Project endpoints. It is not
// hypothetical: api_i18n.go's mustLookupSharedCode is designed to panic at init when a
// shared code is unregistered, so one missing registration means no IM service at all.
//
// A unit test cannot see this; only building the real server with the real registry can.
func TestServerBootsWithProjectRegistered(t *testing.T) {
	srv, ctx := testutil.NewTestServer()
	require.NotNil(t, srv)
	require.NotNil(t, ctx)

	// And the routes really are mounted: a 404 here would mean the process came up but
	// Route() never registered anything.
	for _, path := range []string{
		"/v1/space/some_space/projects",
		"/v1/projects/" + util.GenerUUID(),
	} {
		w := httptest.NewRecorder()
		req, err := http.NewRequest(http.MethodGet, path, nil)
		require.NoError(t, err)
		srv.GetRoute().ServeHTTP(w, req)
		assert.NotEqual(t, http.StatusNotFound, w.Code,
			"%s must be mounted; a 404 means Route() did not register it", path)
	}
}

// TestSpaceRemovalStillRemovesFromGroupsWhenProjectStepFails is verification C2, driven
// through the REAL removal endpoint.
//
// The Project step joins an existing security cascade shared with the group and
// conversation cleanup steps. A step that keeps returning errors keeps its job being
// re-claimed, consuming lease cycles and batch slots unrelated removals need — degrading a
// path that today removes people from their groups.
//
// So this substitutes a deliberately failing project step and asserts on the OUTCOME OF THE
// EXISTING STEP (the group_member row is flagged deleted), not on log output. If the shared
// job were fail-fast, or if step order let one failure starve the rest, the group row would
// survive and this would fail.
func TestSpaceRemovalStillRemovesFromGroupsWhenProjectStepFails(t *testing.T) {
	srv, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))

	const (
		spaceID  = "c2_space"
		groupNo  = "c2_group"
		victim   = "c2_victim"
		operator = "c2_owner"
	)
	exec(t, ctx, "INSERT INTO `space` (space_id, name, creator, status) VALUES (?, ?, ?, 1)",
		spaceID, spaceID, operator)
	// role 2 = owner, so the operator may remove an ordinary member.
	exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 2, 1)",
		spaceID, operator)
	exec(t, ctx, "INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, 0, 1)",
		spaceID, victim)
	// short_no must be distinct: `user` has a UNIQUE index on it, so two rows defaulting
	// to '' collide with MySQL 1062.
	exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", operator, operator, operator)
	exec(t, ctx, "INSERT INTO `user` (uid, name, short_no) VALUES (?, ?, ?)", victim, victim, victim)
	exec(t, ctx, "INSERT INTO `group` (group_no, name, creator, status, space_id) VALUES (?, ?, ?, 1, ?)",
		groupNo, groupNo, operator, spaceID)
	exec(t, ctx, "INSERT INTO group_member (group_no, uid, role, is_deleted, status, version) VALUES (?, ?, 0, 0, 1, 1)",
		groupNo, victim)

	// Substitute a project step that always fails. Registration is name-keyed and
	// latest-wins, which is exactly what makes this possible without touching
	// modules/space. Restored afterwards so no later case inherits a broken step.
	var failures atomic.Int32
	spacemod.RegisterMemberRemovalCleanupStep("project_member",
		func(_ *config.Context, _ spacemod.MemberRemoval) error {
			failures.Add(1)
			return errAlwaysFails{}
		})
	t.Cleanup(func() {
		// Restore the REAL step (latest-wins): leaving a no-op registered would silently
		// disable the project cascade for every later test in this binary.
		restored := projectmod.New(ctx)
		spacemod.RegisterMemberRemovalCleanupStep("project_member", restored.CleanupSpaceMemberProjects)
	})

	// Drive the production path: the removal endpoint commits the membership change, writes
	// the outbox row in the same transaction, and kicks the worker from a background
	// goroutine.
	token := seedToken(t, ctx, operator)
	body, err := json.Marshal(map[string]any{"uids": []string{victim}})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, "/v1/space/"+spaceID+"/members/remove", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("token", token)
	w := httptest.NewRecorder()
	srv.GetRoute().ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	// The worker runs in a background goroutine. Claiming the job increments attempts
	// before any cleanup step runs, so attempts alone is not proof that the deliberately
	// failing callback (or the later group cleanup) has completed. Wait for the callback
	// and the persisted error written after every registered step has run.
	//
	// The budget is generous on purpose. modules/space guards its worker with a process-wide
	// `removalCleanupRunning` flag, so if any earlier test in this binary left a batch in
	// flight, the kick this removal issues is skipped and the job waits for the module's 10s
	// scheduled sweep instead. 40s covers several of those ticks; a 15s budget left roughly
	// one tick of margin and produced a rare order-dependent failure in exactly the test that
	// guards a security cascade.
	require.Eventually(t, func() bool {
		if failures.Load() == 0 {
			return false
		}
		var lastError []string
		if _, err := ctx.DB().SelectBySql(
			"SELECT last_error FROM space_member_removal_cleanup WHERE space_id = ? AND uid = ?",
			spaceID, victim,
		).Load(&lastError); err != nil {
			return false
		}
		return len(lastError) == 1 && lastError[0] != ""
	}, 40*time.Second, 200*time.Millisecond,
		"the cleanup job should have run every step and persisted the project failure")

	require.Greater(t, failures.Load(), int32(0), "the deliberately failing project step must have run")

	// The assertion that matters: the group cascade still completed.
	var deleted []int
	_, err = ctx.DB().SelectBySql(
		"SELECT is_deleted FROM group_member WHERE group_no = ? AND uid = ?", groupNo, victim,
	).Load(&deleted)
	require.NoError(t, err)
	require.Len(t, deleted, 1)
	assert.Equal(t, 1, deleted[0],
		"a failing project step must not starve the group cascade it shares the job with")

	// And the failure is attributed to the project step, so an operator reading last_error
	// is pointed at the right module rather than at the cascade that worked.
	var lastError []string
	_, err = ctx.DB().SelectBySql(
		"SELECT last_error FROM space_member_removal_cleanup WHERE space_id = ? AND uid = ?",
		spaceID, victim,
	).Load(&lastError)
	require.NoError(t, err)
	require.Len(t, lastError, 1)
	assert.Contains(t, lastError[0], "project_member")
}

// TestSpaceRemovalRejoinDoesNotRestoreNativeGroupMembership exercises Space
// revocation and re-entry through the production Project and Group hooks.
// The HTTP selector uses a case variant of the stored Space ID: removal
// clears native group membership, while re-entry preserves the Project Owner
// seat, advances its epoch, and leaves native group membership unchanged.
func TestSpaceRemovalRejoinDoesNotRestoreNativeGroupMembership(t *testing.T) {
	srv, ctx := newE2EServer(t)

	const (
		spaceID    = "e2e_raw_rejoin_space"
		rawSpaceID = "E2E_RAW_REJOIN_SPACE"
		spaceOwner = "e2e_raw_rejoin_owner"
		target     = "e2e_raw_rejoin_target"
		projectID  = "e2e_raw_rejoin_project"
		groupNo    = "e2e_raw_rejoin_group"
	)

	// The test only needs the broker HTTP boundary. Seeded native rows avoid
	// making group creation part of this regression, while the cleanup hooks remain
	// the production Group/Project implementations.
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	cfg := ctx.GetConfig()
	previousIMURL := cfg.WuKongIM.APIURL
	cfg.WuKongIM.APIURL = im.URL
	t.Cleanup(func() {
		cfg.WuKongIM.APIURL = previousIMURL
		im.Close()
	})

	exec(t, ctx,
		"INSERT INTO `space` (space_id, name, creator, status, max_users) VALUES (?, ?, ?, 1, 100)",
		spaceID, spaceID, spaceOwner)
	for _, uid := range []string{spaceOwner, target} {
		role := 0
		if uid == spaceOwner {
			role = 2
		}
		exec(t, ctx,
			"INSERT INTO space_member (space_id, uid, role, status) VALUES (?, ?, ?, 1)",
			spaceID, uid, role)
		exec(t, ctx,
			"INSERT INTO `user` (uid, name, short_no, status, robot) VALUES (?, ?, ?, 1, 0)",
			uid, uid, uid)
	}
	exec(t, ctx,
		"INSERT INTO `octo_project` "+
			"(project_id, space_id, name, creator, status, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, 1, NOW(), NOW())",
		projectID, spaceID, projectID, spaceOwner)
	exec(t, ctx,
		"INSERT INTO `octo_project_member` "+
			"(project_id, uid, space_id, role, status, removing, invite_uid, created_at, joined_at, updated_at) "+
			"VALUES (?, ?, ?, 2, 1, 0, ?, NOW(), NOW(), NOW())",
		projectID, target, spaceID, target)
	// An ordinary Project-associated group: the relation is group.project_id, and
	// there is no dedicated pointer left to distinguish it.
	exec(t, ctx,
		"INSERT INTO `group` "+
			"(group_no, name, creator, status, version, space_id, project_id) "+
			"VALUES (?, ?, ?, 1, 1, ?, ?)",
		groupNo, groupNo, target, spaceID, projectID)
	exec(t, ctx,
		"INSERT INTO group_member (group_no, uid, role, is_deleted, status, version) "+
			"VALUES (?, ?, 1, 0, 1, 1)",
		groupNo, target)

	ownerToken := seedToken(t, ctx, spaceOwner)
	w := writeProjectE2E(t, srv, http.MethodPost,
		"/v1/space/"+rawSpaceID+"/members/remove", ownerToken,
		map[string]any{"uids": []string{target}})
	require.Equal(t, http.StatusOK, w.Code, "Space removal body: %s", w.Body.String())
	require.Eventually(t, func() bool {
		return !activeGroupMemberE2E(ctx, groupNo, target)
	}, 40*time.Second, 200*time.Millisecond,
		"Space removal worker must still remove the native group member")
	status, removing, ok := projectSeatStateE2E(ctx, projectID, target)
	require.True(t, ok)
	assert.Equal(t, 1, status, "Project Owner identity must remain active")
	assert.Equal(t, 0, removing)
	epochBeforeRejoin := projectEpochE2E(ctx, projectID)

	// Reopening the Space seat changes the Project epoch without re-admitting
	// the member into a previously revoked native group.
	w = writeProjectE2E(t, srv, http.MethodPost,
		"/v1/space/"+rawSpaceID+"/members/add", ownerToken,
		map[string]any{"uids": []string{target}})
	require.Equal(t, http.StatusOK, w.Code, "Space rejoin body: %s", w.Body.String())
	assert.False(t, activeGroupMemberE2E(ctx, groupNo, target),
		"Space rejoin must not restore native group membership")
	status, removing, ok = projectSeatStateE2E(ctx, projectID, target)
	require.True(t, ok)
	assert.Equal(t, 1, status, "the retained Owner seat must survive the rejoin")
	assert.Zero(t, removing)
	assert.Greater(t, projectEpochE2E(ctx, projectID), epochBeforeRejoin,
		"reopening the Space seat must still move the Project epoch")
}

// projectEpochE2E reads octo_project.member_epoch straight from the row.
func projectEpochE2E(ctx *config.Context, projectID string) int64 {
	var epochs []int64
	if _, err := ctx.DB().SelectBySql(
		"SELECT member_epoch FROM `octo_project` WHERE project_id = ?", projectID,
	).Load(&epochs); err != nil || len(epochs) == 0 {
		return 0
	}
	return epochs[0]
}

// errAlwaysFails is the sentinel the deliberately failing step returns.
type errAlwaysFails struct{}

func (errAlwaysFails) Error() string { return "project step failing on purpose (C2)" }

func exec(t *testing.T, ctx *config.Context, sql string, args ...interface{}) {
	t.Helper()
	_, err := ctx.DB().UpdateBySql(sql, args...).Exec()
	require.NoError(t, err)
}

// seedToken registers a token for uid in the same cache octo-lib's legacyTokenParser reads.
func seedToken(t *testing.T, ctx *config.Context, uid string) string {
	t.Helper()
	token := "extok-" + uid + "-" + util.GenerUUID()[:8]
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+token, uid+"@"+uid))
	return token
}
