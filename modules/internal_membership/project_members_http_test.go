package internal_membership_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	projectmod "github.com/Mininglamp-OSS/octo-server/modules/project"
	"github.com/stretchr/testify/require"
)

func TestInternalMembershipListMatchesPublicRoster(t *testing.T) {
	srv, ctx := newMembershipHTTPTest(t)
	ownerToken, created := iupProjectWithMembers(t, srv, ctx, "internal-list-human")
	seedAgentHTTP(t, ctx, created.SpaceID, "internal-list-agent", "owner1", "")
	iupSeatHTTP(t, ctx, created.ProjectID, "internal-list-agent", created.SpaceID, projectmod.RoleCommon, projectmod.MemberStatusActive, 0)
	// Neither a closed seat nor an incorrectly scoped seat belongs in the roster.
	for _, seat := range []struct {
		uid, space       string
		status, removing int
	}{
		{"internal-list-removed", created.SpaceID, 0, 0},
		{"internal-list-removing", created.SpaceID, 1, 1},
		{"internal-list-wrong-space", iupSpaceB, 1, 0},
	} {
		iupSeatHTTP(t, ctx, created.ProjectID, seat.uid, seat.space, projectmod.RoleCommon, seat.status, seat.removing)
	}
	catalog := iupRoleCatalog(t, srv, created.ProjectID, ownerToken)
	frontend := iupRoleNamed(t, catalog, "前端")
	designer := iupCreateRole(t, srv, created.ProjectID, ownerToken, "设计")
	w := writeProjectE2E(t, srv, http.MethodPut,
		"/v1/projects/"+created.ProjectID+"/members/internal-list-human/collaboration-roles", ownerToken,
		map[string]any{"role_ids": []string{designer.RoleID, frontend.RoleID}})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, err := ctx.DB().UpdateBySql("UPDATE octo_project_member SET joined_at=NULL WHERE project_id=? AND uid=?", created.ProjectID, "internal-list-human").Exec()
	require.NoError(t, err)
	route := iupMountInternalRoute(t, ctx)
	for _, query := range []string{"", "?page=1&limit=1", "?page=2&limit=1", "?page=99&limit=1", "?limit=0", "?limit=500", "?page=-1&limit=0", "?page=invalid&limit=invalid", "?page=100001&limit=201"} {
		t.Run(query, func(t *testing.T) {
			public := writeProjectE2E(t, srv, http.MethodGet, "/v1/projects/"+created.ProjectID+"/members"+query, ownerToken, nil)
			separator := "&"
			page := query
			if page != "" {
				page = separator + page[1:]
			}
			internal := iupInternalGet(t, route, fmt.Sprintf("/v1/internal/project-memberships?space_id=%s&project_id=%s%s", created.SpaceID, created.ProjectID, page), iupInternalToken)
			require.Equal(t, http.StatusOK, public.Code, public.Body.String())
			require.Equal(t, http.StatusOK, internal.Code, internal.Body.String())
			require.JSONEq(t, public.Body.String(), internal.Body.String())
			require.Equal(t, public.Header().Get("X-Total-Count"), internal.Header().Get("X-Total-Count"))
			require.Equal(t, "3", internal.Header().Get("X-Total-Count"))
			require.Equal(t, "no-store", internal.Header().Get("Cache-Control"))
			if query == "" {
				var members []projectmod.MemberResp
				require.NoError(t, json.Unmarshal(internal.Body.Bytes(), &members))
				require.Len(t, members, 3)
				require.Equal(t, "owner1", members[0].UID)
				byUID := make(map[string]projectmod.MemberResp)
				for _, member := range members {
					byUID[member.UID] = member
					require.NotEmpty(t, member.CreatedAt)
					require.NotEmpty(t, member.JoinedAt)
					require.NotNil(t, member.CollaborationRoles)
				}
				human := byUID["internal-list-human"]
				require.NotEmpty(t, human.Name)
				require.Equal(t, projectmod.RoleCommon, human.Role)
				require.Equal(t, "owner1", human.InviteUID)
				require.Equal(t, human.CreatedAt, human.JoinedAt)
				require.Equal(t, []projectmod.CollaborationRoleResp{
					{RoleID: frontend.RoleID, BuiltinKey: "frontend", Name: "前端", Source: "builtin"},
					{RoleID: designer.RoleID, Name: "设计", Source: "custom"},
				}, human.CollaborationRoles)
				agent := byUID["internal-list-agent"]
				require.Equal(t, "agent-internal-list-agent", agent.Name)
				require.Equal(t, 1, agent.Robot)
				require.Equal(t, "owner1", agent.OwnerUID)
				require.Empty(t, agent.CollaborationRoles)
			}
		})
	}
}

func TestInternalMembershipListRejectsCrossSpaceAndInactiveProjects(t *testing.T) {
	srv, ctx := newMembershipHTTPTest(t)
	_, created := iupProjectWithMembers(t, srv, ctx, "internal-list-member")
	route := iupMountInternalRoute(t, ctx)
	seedSpaceHTTP(t, ctx, iupSpaceB, 1)
	for _, space := range []string{iupSpaceB, "missing-space"} {
		w := iupInternalGet(t, route, fmt.Sprintf("/v1/internal/project-memberships?space_id=%s&project_id=%s", space, created.ProjectID), iupInternalToken)
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	}
	for _, status := range []int{0, 2} {
		setSpaceStatusHTTP(t, ctx, created.SpaceID, status)
		w := iupInternalGet(t, route, fmt.Sprintf("/v1/internal/project-memberships?space_id=%s&project_id=%s", created.SpaceID, created.ProjectID), iupInternalToken)
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
	}
	setSpaceStatusHTTP(t, ctx, created.SpaceID, 1)
	// ASCII case aliases are the same ID; unrelated selectors remain unavailable.
	w := iupInternalGet(t, route, fmt.Sprintf("/v1/internal/project-memberships?space_id=%s&project_id=%s", strings.ToUpper(created.SpaceID), strings.ToUpper(created.ProjectID)), iupInternalToken)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	_, err := ctx.DB().UpdateBySql("UPDATE octo_project SET status=0 WHERE project_id=?", created.ProjectID).Exec()
	require.NoError(t, err)
	w = iupInternalGet(t, route, fmt.Sprintf("/v1/internal/project-memberships?space_id=%s&project_id=%s", created.SpaceID, created.ProjectID), iupInternalToken)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestInternalMembershipListHonorsRequestCancellation(t *testing.T) {
	srv, ctx := newMembershipHTTPTest(t)
	_, created := iupProjectWithMembers(t, srv, ctx)
	route := iupMountInternalRoute(t, ctx)
	target := fmt.Sprintf("/v1/internal/project-memberships?space_id=%s&project_id=%s", created.SpaceID, created.ProjectID)
	control := iupInternalGet(t, route, target, iupInternalToken)
	require.Equal(t, http.StatusOK, control.Code, control.Body.String())
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, target, nil).WithContext(requestCtx)
	req.Header.Set("X-Internal-Token", iupInternalToken)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusInternalServerError, w.Code, w.Body.String())
	require.Equal(t, "err.shared.internal", iupDecodeError(t, w.Body.Bytes()).Error.Code)
	require.Empty(t, w.Header().Get("X-Total-Count"))
}
