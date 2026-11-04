package group

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectRelationArchivedStateRequiresMembership(t *testing.T) {
	_, ctx := newTestServer(t)
	defer func() { require.NoError(t, testutil.CleanAllTables(ctx)) }()
	enableProjectGroupsForWire(t, ctx)
	g := New(ctx)
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	g.routeProject(route)

	suffix := util.GenerUUID()[:8]
	spaceID, projectID := "relation-space-"+suffix, "relation-project-"+suffix
	actorUID, groupNo := "relation-actor-"+suffix, "relation-group-"+suffix
	seedGroupCreateUser(t, ctx, actorUID, 1)
	seedSpaceSeat(t, ctx, spaceID, actorUID)
	seedProject(t, ctx, projectID, spaceID)
	now := time.Now().UTC()
	_, err := ctx.DB().UpdateBySql(
		"UPDATE octo_project SET archived_at=?, archive_version=2, lifecycle_version=2, activated_at=? WHERE project_id=?",
		now, now, projectID).Exec()
	require.NoError(t, err)
	require.NoError(t, g.db.Insert(&Model{
		GroupNo: groupNo, Name: "Independent native group", Creator: actorUID,
		Status: GroupStatusNormal, Version: 1, SpaceID: spaceID,
	}))
	seedGroupMemberRow(t, ctx, groupNo, actorUID, MemberRoleCreator)
	token := "relation-token-" + suffix
	require.NoError(t, ctx.Cache().Set(ctx.GetConfig().Cache.TokenCachePrefix+token, actorUID+"@test"))
	body, err := json.Marshal(map[string]string{"project_id": projectID})
	require.NoError(t, err)

	for _, member := range []bool{false, true} {
		if member {
			seedProjectMember(t, ctx, projectID, spaceID, actorUID, 0)
		}
		request := httptest.NewRequest(http.MethodPut, "/v1/groups/"+groupNo+"/project", bytes.NewReader(body))
		request.Header.Set("token", token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		route.ServeHTTP(response, request)
		require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		envelope := decodeEnvelope(t, response.Body.Bytes())
		if member {
			assert.Equal(t, "err.server.project.archived", envelope.Error.Code)
		} else {
			assert.Equal(t, "err.server.group.view_forbidden", envelope.Error.Code,
				"a same-Space nonmember must not learn a private Project's archive state")
		}
		assertGroupProjectRelationRow(t, ctx, groupNo, "", "")
	}
}
