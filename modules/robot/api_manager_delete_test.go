package robot

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/modules/group"
	"github.com/Mininglamp-OSS/octo-server/pkg/aiteam"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupRobotManagerDeleteTest(t *testing.T) (http.Handler, *Manager) {
	t.Helper()
	_, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))

	const managerToken = "robot-manager-delete-token"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+managerToken,
		testutil.UID+"@superadmin@"+string(wkhttp.SuperAdmin),
	))
	t.Cleanup(func() {
		_ = ctx.GetRedisConn().Del(ctx.GetConfig().Cache.TokenCachePrefix + managerToken)
		_ = testutil.CleanAllTables(ctx)
	})

	m := NewManager(ctx)
	m.Route(route)
	return route, m
}

func deleteManagedRobot(t *testing.T, route http.Handler, robotID string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodDelete, "/v1/manager/robots/"+robotID, nil)
	require.NoError(t, err)
	req.Header.Set("token", "robot-manager-delete-token")
	route.ServeHTTP(w, req)
	return w
}

func TestRobotDeleteRejectsNonRobotBeforeGroupCleanup(t *testing.T) {
	route, manager := setupRobotManagerDeleteTest(t)
	const humanUID = "human-not-a-robot"
	const groupNo = "group-with-human"

	_, err := manager.ctx.DB().InsertBySql(
		"INSERT INTO `group` (group_no, name, creator, status) VALUES (?, ?, ?, 1)",
		groupNo, "ordinary", "other-owner",
	).Exec()
	require.NoError(t, err)
	require.NoError(t, group.NewDB(manager.ctx).InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: humanUID, Role: group.MemberRoleCommon, Status: 1,
		Version: 1, Vercode: util.GenerUUID(),
	}))

	w := deleteManagedRobot(t, route, humanUID)
	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "err.server.robot.not_found")

	member, err := group.NewDB(manager.ctx).QueryMemberWithUID(humanUID, groupNo)
	require.NoError(t, err)
	require.NotNil(t, member)
	assert.Zero(t, member.IsDeleted, "a non-robot UID must never reach destructive lifecycle cleanup")
}

func TestRobotDeleteRemovesProtectedContainerMembership(t *testing.T) {
	route, manager := setupRobotManagerDeleteTest(t)
	botUID := "manager-bot-" + util.GenerUUID()[:8]
	groupNo := "manager-ai-" + util.GenerUUID()[:8]

	_, err := manager.ctx.DB().InsertBySql(
		"INSERT INTO `user` (uid, name, short_no, robot, status) VALUES (?, ?, ?, 1, 1)",
		botUID, "manager bot", botUID,
	).Exec()
	require.NoError(t, err)
	_, err = manager.ctx.DB().InsertBySql(
		"INSERT INTO robot (robot_id, status, creator_uid) VALUES (?, 1, ?)",
		botUID, testutil.UID,
	).Exec()
	require.NoError(t, err)
	require.NoError(t, group.NewDB(manager.ctx).Insert(&group.Model{
		GroupNo: groupNo, Name: "manager AI container", Creator: testutil.UID,
		Status: group.GroupStatusNormal, Purpose: aiteam.GroupPurpose,
	}))
	require.NoError(t, group.NewDB(manager.ctx).InsertMember(&group.MemberModel{
		GroupNo: groupNo, UID: botUID, Role: group.MemberRoleCommon, Robot: 1,
		Status: 1, Version: 1, Vercode: util.GenerUUID(),
	}))

	w := deleteManagedRobot(t, route, botUID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var memberDeleted int
	err = manager.ctx.DB().Select("is_deleted").From("group_member").
		Where("uid=? AND group_no=?", botUID, groupNo).LoadOne(&memberDeleted)
	require.NoError(t, err)
	assert.Equal(t, 1, memberDeleted)

	robot, err := manager.db.queryRobotWithRobtID(botUID)
	require.NoError(t, err)
	require.NotNil(t, robot)
	assert.Zero(t, robot.Status)
}
