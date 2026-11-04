package internal_membership_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

const lifecycleHTTPPath = "/v1/internal/projects/%s/lifecycle"

func lifecycleInternalGet(t *testing.T, route http.Handler, target, token, clientIP string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	req.Header.Set("X-Real-Ip", clientIP)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	return w
}

func TestInternalProjectLifecycleRequiresCredentialAndSpaceScope(t *testing.T) {
	_, ctx := newMembershipHTTPTest(t)
	route := iupMountInternalRoute(t, ctx)
	path := fmt.Sprintf(lifecycleHTTPPath, "lifecycle-auth-project")

	userToken := seedUserHTTP(t, ctx, "lifecycle-session-user")
	for i, tc := range []struct {
		name  string
		token string
	}{
		{name: "missing internal credential"},
		{name: "wrong internal credential", token: "wrong-internal-token-0123456789abc"},
		{name: "user session is not a peer credential", token: userToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ip := fmt.Sprintf("198.51.100.%d", 151+i)
			w := lifecycleInternalGet(t, route, path+"?space_id="+iupSpaceA, tc.token, ip)
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			require.Equal(t, "err.shared.auth.token_invalid", iupDecodeError(t, w.Body.Bytes()).Error.Code)
		})
	}

	for i, query := range []string{
		"",
		"?space_id=",
		"?space_id=%20space_a",
		"?space_id=space_a&space_id=space_b",
	} {
		t.Run("invalid scope "+query, func(t *testing.T) {
			ip := fmt.Sprintf("198.51.100.%d", 161+i)
			w := lifecycleInternalGet(t, route, path+query, iupInternalToken, ip)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
			require.Equal(t, "err.shared.param.invalid", iupDecodeError(t, w.Body.Bytes()).Error.Code)
		})
	}
}

func TestInternalProjectLifecycleReturnsOnlyScopedActivatedNormalProjectFacts(t *testing.T) {
	_, ctx := newMembershipHTTPTest(t)
	const (
		activeID      = "lifecycle-http-active"
		pendingID     = "lifecycle-http-pending"
		disbandedID   = "lifecycle-http-disbanded"
		wrongSpaceID  = "lifecycle-http-wrong-space"
		inactiveID    = "lifecycle-http-inactive-parent"
		missingParent = "lifecycle-http-missing-parent"
		inactiveSpace = "lifecycle-http-inactive-space"
	)
	seedSpaceHTTP(t, ctx, iupSpaceA, 1)
	seedSpaceHTTP(t, ctx, iupSpaceB, 1)
	seedSpaceHTTP(t, ctx, inactiveSpace, 2)

	// Lifecycle reads are machine-to-machine facts, not membership answers: no
	// Project member rows are created for these fixtures.
	for _, row := range []struct {
		id, space string
		status    int
		activated bool
	}{
		{activeID, iupSpaceA, 1, true},
		{pendingID, iupSpaceA, 1, false},
		{disbandedID, iupSpaceA, 0, true},
		{wrongSpaceID, iupSpaceB, 1, true},
		{inactiveID, inactiveSpace, 1, true},
		{missingParent, "lifecycle-http-no-space", 1, true},
	} {
		iupProjectHTTP(t, ctx, row.id, row.space, "unrelated-creator", "Private name", "Private description", "", row.status, row.activated)
	}
	_, err := ctx.DB().UpdateBySql(
		"UPDATE octo_project SET lifecycle_version = 4 WHERE project_id = ?", activeID).Exec()
	require.NoError(t, err)

	route := iupMountInternalRoute(t, ctx)
	requestIndex := 0
	read := func(projectID, spaceID string) *httptest.ResponseRecorder {
		t.Helper()
		query := url.Values{}
		query.Set("space_id", spaceID)
		ip := fmt.Sprintf("198.51.100.%d", 171+requestIndex)
		requestIndex++
		return lifecycleInternalGet(t, route,
			fmt.Sprintf(lifecycleHTTPPath, projectID)+"?"+query.Encode(), iupInternalToken, ip)
	}

	active := read(activeID, iupSpaceA)
	require.Equal(t, http.StatusOK, active.Code, active.Body.String())
	require.Equal(t, "no-store", active.Header().Get("Cache-Control"))
	var activeFacts map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(active.Body.Bytes(), &activeFacts), active.Body.String())
	require.Equal(t, []string{"archive_version", "archived_at", "lifecycle_version", "project_id", "space_id"}, sortedJSONKeys(activeFacts))
	require.JSONEq(t,
		`{"project_id":"lifecycle-http-active","space_id":"space_a","lifecycle_version":4,"archive_version":0,"archived_at":null}`,
		active.Body.String())

	require.NotContains(t, active.Body.String(), "Private name")
	require.NotContains(t, active.Body.String(), "Private description")

	for _, tc := range []struct {
		name      string
		projectID string
		spaceID   string
	}{
		{name: "unknown Project", projectID: "lifecycle-http-unknown", spaceID: iupSpaceA},
		{name: "cross-Space scope", projectID: activeID, spaceID: iupSpaceB},
		{name: "inactive parent Space", projectID: inactiveID, spaceID: inactiveSpace},
		{name: "missing parent Space", projectID: missingParent, spaceID: "lifecycle-http-no-space"},
		{name: "awaiting activation", projectID: pendingID, spaceID: iupSpaceA},
		{name: "disbanded Project", projectID: disbandedID, spaceID: iupSpaceA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := read(tc.projectID, tc.spaceID)
			require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		})
	}
}

func sortedJSONKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
