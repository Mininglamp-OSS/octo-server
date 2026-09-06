// Package project_test holds the cases that need the WHOLE module registry.
//
// It is an EXTERNAL test package on purpose, and the reason is a build failure that would
// only appear later: in P1 modules/group will import modules/project, so an IN-PACKAGE test
// that blank-imported octo-server/internal (which imports group) would form an import
// cycle — Go rejects that for in-package tests and permits it only for an external test
// package. Choosing the package now costs nothing; discovering it in P1 means rewriting
// working tests. Same shape as modules/bot_provision/bot_api_test.go.
//
// There is no TestMain here: an in-package and an external test package compile into ONE
// test binary, which may declare only one. The setup both need lives in api_test.go's
// TestMain.
package project_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	failures := 0
	spacemod.RegisterMemberRemovalCleanupStep("project_member",
		func(_ *config.Context, _ spacemod.MemberRemoval) error {
			failures++
			return errAlwaysFails{}
		})
	t.Cleanup(func() {
		spacemod.RegisterMemberRemovalCleanupStep("project_member",
			func(_ *config.Context, _ spacemod.MemberRemoval) error { return nil })
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

	// The worker runs in a background goroutine, so wait for the job to record its first
	// failure rather than sleeping a fixed amount.
	//
	// The budget is generous on purpose. modules/space guards its worker with a process-wide
	// `removalCleanupRunning` flag, so if any earlier test in this binary left a batch in
	// flight, the kick this removal issues is skipped and the job waits for the module's 10s
	// scheduled sweep instead. 40s covers several of those ticks; a 15s budget left roughly
	// one tick of margin and produced a rare order-dependent failure in exactly the test that
	// guards a security cascade.
	require.Eventually(t, func() bool {
		var attempts []int
		if _, err := ctx.DB().SelectBySql(
			"SELECT attempts FROM space_member_removal_cleanup WHERE space_id = ? AND uid = ?",
			spaceID, victim,
		).Load(&attempts); err != nil {
			return false
		}
		return len(attempts) == 1 && attempts[0] >= 1
	}, 40*time.Second, 200*time.Millisecond,
		"the cleanup job should have been claimed and released after the project step failed")

	require.Greater(t, failures, 0, "the deliberately failing project step must have run")

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
