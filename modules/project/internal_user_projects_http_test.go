package project

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	internalmembership "github.com/Mininglamp-OSS/octo-server/modules/internal_membership"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/require"
)

const (
	// iupInternalToken is a deployment-shaped credential: long enough to clear the
	// module's 32-byte floor, and unlike every sibling fixed token.
	iupInternalToken = "internal-user-projects-test-token-0123456789"

	iupProjectsPath = "/v1/internal/projects"
)

// iupMountInternalRoute mounts the REAL module route — the same deadline →
// strict IP limiter → internal token chain production runs — on a private
// router, with the enablement env in place. No hand-assembled middleware list:
// a case that passes here exercises the wiring in Route().
func iupMountInternalRoute(t *testing.T) *wkhttp.WKHttp {
	t.Helper()
	t.Setenv(internalmembership.MembershipInternalTokenEnv, iupInternalToken)
	t.Setenv(internalmembership.PeerMaxCacheAgeEnv, "30")
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	internalmembership.New(testCtx).Route(route)
	return route
}

func iupInternalGet(t *testing.T, route *wkhttp.WKHttp, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if token != "" {
		req.Header.Set("X-Internal-Token", token)
	}
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	return w
}

type iupErrorEnvelope struct {
	Error struct {
		Code       string                 `json:"code"`
		HTTPStatus int                    `json:"http_status"`
		Details    map[string]interface{} `json:"details"`
	} `json:"error"`
}

// iupDecodeError reads the shared i18n error envelope, not the legacy {msg,status}
// body: the field detail only exists in the former.
func iupDecodeError(t *testing.T, body []byte) iupErrorEnvelope {
	t.Helper()
	var env iupErrorEnvelope
	require.NoError(t, json.Unmarshal(body, &env), "body: %s", string(body))
	return env
}

type iupHTTPProject struct {
	ProjectID   string `json:"project_id"`
	SpaceID     string `json:"space_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Logo        string `json:"logo"`
	Role        int    `json:"role"`
}

func iupDecodeProjects(t *testing.T, body []byte) []iupHTTPProject {
	t.Helper()
	var rows []iupHTTPProject
	require.NoError(t, json.Unmarshal(body, &rows), "body: %s", string(body))
	return rows
}

func TestInternalUserProjectsHTTPActivatedMembership(t *testing.T) {
	setup(t)
	seedUserBare(t, "internal-reader")
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, "internal-reader", 0, 1)
	_, err := testCtx.DB().InsertBySql(
		"INSERT INTO octo_project (project_id, space_id, name, description, creator, status, activated_at, created_at, updated_at) "+
			"VALUES ('internal-ready', ?, 'Ready project', 'Ready description', 'internal-reader', 1, NOW(3), NOW(3), NOW(3)), "+
			"('internal-pending', ?, 'Pending project', '', 'internal-reader', 1, NULL, NOW(3), NOW(3))",
		spaceA, spaceA,
	).Exec()
	require.NoError(t, err)
	_, err = testCtx.DB().InsertBySql(
		"INSERT INTO octo_project_member (project_id, uid, space_id, role, status, removing, created_at, updated_at) "+
			"VALUES ('internal-ready', 'internal-reader', ?, 0, 1, 0, NOW(3), NOW(3)), "+
			"('internal-pending', 'internal-reader', ?, 2, 1, 0, NOW(3), NOW(3))",
		spaceA, spaceA,
	).Exec()
	require.NoError(t, err)

	const token = "project-list-http-test-token-0123456789"
	t.Setenv(internalmembership.MembershipInternalTokenEnv, token)
	t.Setenv(internalmembership.PeerMaxCacheAgeEnv, "30")
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	internalmembership.New(testCtx).Route(route)
	req := httptest.NewRequest(http.MethodGet, "/v1/internal/projects?space_id=space_a&uid=internal-reader", nil)
	req.Header.Set("X-Internal-Token", token)
	response := httptest.NewRecorder()
	route.ServeHTTP(response, req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.JSONEq(t, `[{"project_id":"internal-ready","space_id":"space_a","name":"Ready project","description":"Ready description","logo":"","role":0}]`, response.Body.String())
}

// TestInternalUserProjectsHTTPRequiresInternalCredential covers the two refusal
// modes a peer can hit. A user SESSION token in the `token` header is explicitly
// not a substitute — the endpoint's subject is a third party named in the query,
// so there is no login identity it could authorize against.
func TestInternalUserProjectsHTTPRequiresInternalCredential(t *testing.T) {
	setup(t)
	const owner = "iup-http-owner"
	seedUserBare(t, owner)
	seedSpace(t, spaceA, 1)
	seedSpaceMember(t, spaceA, owner, 0, 1)
	iupProject(t, "iup-http-secret", spaceA, owner, "Should not leak", "", "", 1, true)
	iupSeat(t, "iup-http-secret", owner, spaceA, 0, 1, 0)

	route := iupMountInternalRoute(t)
	path := iupProjectsPath + "?space_id=" + spaceA + "&uid=" + owner

	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "missing credential", token: ""},
		{name: "wrong credential", token: "wrong-internal-token-0123456789abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := iupInternalGet(t, route, path, tc.token)
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			env := iupDecodeError(t, w.Body.Bytes())
			require.Equal(t, "err.shared.auth.token_invalid", env.Error.Code)
			require.NotContains(t, w.Body.String(), "Should not leak",
				"a refused request must not answer with data")
		})
	}

	// A real user token, in the header that endpoint would accept, is still not an
	// internal credential.
	userToken := seedUser(t, "iup-http-session-user")
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("token", userToken)
	w := httptest.NewRecorder()
	route.ServeHTTP(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code,
		"a user session must not stand in for the service credential")
	require.Equal(t, "err.shared.auth.token_invalid", iupDecodeError(t, w.Body.Bytes()).Error.Code)
}

// iupSocketGet runs one request over a real loopback connection.
func iupSocketGet(t *testing.T, base string, client *http.Client, target string, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+target, nil)
	require.NoError(t, err)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, body
}

// TestInternalUserProjectsHTTPSelectorsShapeAndIsolation is the HTTP smoke the
// design asks for — real module route, real database, real socket — and it is ONE
// fixture serving every positive wire assertion:
//
//   - the QUERY selectors are authoritative: a forged `token` (a real user
//     session), uid-naming and space-naming headers change nothing, because this
//     surface has no session identity and `uid` is a token-authorized target;
//   - `space_id` isolates Spaces, and a live user with no Space seat gets [];
//   - `page`/`limit` are not interpreted at all — the answer is the complete list;
//   - the exact six fields cross the wire, including `role: 0` and `"logo": ""`
//     for a member, and a 500-character description untruncated;
//   - an empty result is `[]`, never `null`.
func TestInternalUserProjectsHTTPSelectorsShapeAndIsolation(t *testing.T) {
	setup(t)
	const (
		target   = "iup-http-target"
		other    = "iup-http-other"
		outsider = "iup-http-outsider"
	)
	seedUserBare(t, target)
	otherToken := seedUser(t, other) // a REAL user session, minted for the forgery below
	seedUserBare(t, outsider)
	seedSpace(t, spaceA, 1)
	seedSpace(t, spaceB, 1)
	seedSpaceMember(t, spaceA, target, 0, 1)
	seedSpaceMember(t, spaceB, target, 0, 1)
	seedSpaceMember(t, spaceA, other, 0, 1)
	seedSpaceMember(t, spaceB, other, 0, 1)

	// octo_project.description is TEXT; go well past the historical 500-character
	// limit and assert the value comes back unmodified.
	longDescription := strings.Repeat("长", 1024)

	iupProject(t, "iup-http-a-member", spaceA, "someone-else", "Member project", longDescription, "", 1, true)
	iupProject(t, "iup-http-a-owner", spaceA, "someone-else", "Owner project", "", "https://cdn.test/logo.png", 1, true)
	iupProject(t, "iup-http-a-pending", spaceA, "someone-else", "Awaiting activation", "", "", 1, false)
	iupProject(t, "iup-http-a-other", spaceA, other, "Other user project", "", "", 1, true)
	iupProject(t, "iup-http-b", spaceB, "someone-else", "Project B", "description B", "", 1, true)

	iupSeat(t, "iup-http-a-member", target, spaceA, 0, 1, 0)
	iupSeat(t, "iup-http-a-owner", target, spaceA, 2, 1, 0)
	iupSeat(t, "iup-http-a-pending", target, spaceA, 2, 1, 0)
	iupSeat(t, "iup-http-a-other", other, spaceA, 0, 1, 0)
	iupSeat(t, "iup-http-b", target, spaceB, 1, 1, 0)
	iupSeat(t, "iup-http-b", other, spaceB, 1, 1, 0)

	srv := httptest.NewServer(iupMountInternalRoute(t))
	t.Cleanup(srv.Close)
	base, client := srv.URL, srv.Client()

	// Forged identity in every place a naive implementation might look: a genuine
	// user session token, uid-naming headers, and space-naming headers. None may
	// override the query selectors.
	forged := map[string]string{
		"X-Internal-Token": iupInternalToken,
		"token":            otherToken,
		"X-User-Id":        other,
		"X-UID":            other,
		"uid":              other,
		"space_id":         spaceB,
		"X-Space-Id":       spaceB,
	}

	resp, body := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+spaceA+"&uid="+target, forged)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	require.Equal(t, []iupHTTPProject{
		{ProjectID: "iup-http-a-owner", SpaceID: spaceA, Name: "Owner project", Description: "", Logo: "https://cdn.test/logo.png", Role: 2},
		{ProjectID: "iup-http-a-member", SpaceID: spaceA, Name: "Member project", Description: longDescription, Logo: "", Role: 0},
	}, iupDecodeProjects(t, body),
		"the answer must be the QUERY target's activated projects in the QUERY Space, id DESC — no header may widen it")
	require.NotContains(t, string(body), "Other user project")
	// Field PRESENCE, not just decoded values: a role/logo dropped by omitempty would
	// read as "no member role" / "no logo setting" to a consumer.
	require.Contains(t, string(body), `"role":0`)
	require.Contains(t, string(body), `"logo":""`)

	// Pagination parameters are not part of this contract; the caller does not know
	// the project ids and cannot page, so they must not truncate a single row.
	_, pagedBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+spaceA+"&uid="+target+"&page=1&limit=1", forged)
	require.Equal(t, iupDecodeProjects(t, body), iupDecodeProjects(t, pagedBody),
		"page/limit must be ignored, not applied as a cap")

	// The uid selector is what selects the subject: asking about the other user in
	// the SAME Space returns THEIR project, not the target's.
	_, otherBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+spaceA+"&uid="+other, forged)
	require.Equal(t, []iupHTTPProject{
		{ProjectID: "iup-http-a-other", SpaceID: spaceA, Name: "Other user project", Description: "", Logo: "", Role: 0},
	}, iupDecodeProjects(t, otherBody))

	// Space isolation on the wire.
	_, bBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+spaceB+"&uid="+target, forged)
	require.Equal(t, []iupHTTPProject{
		{ProjectID: "iup-http-b", SpaceID: spaceB, Name: "Project B", Description: "description B", Logo: "", Role: 1},
	}, iupDecodeProjects(t, bBody))

	// Truthful negatives are 200 with an EMPTY ARRAY.
	_, emptyBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+spaceA+"&uid="+outsider, forged)
	require.Equal(t, "[]", strings.TrimSpace(string(emptyBody)), "no membership must be [] on the wire, not null")

	_, unknownBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id=space_unknown&uid="+target, forged)
	require.Equal(t, "[]", strings.TrimSpace(string(unknownBody)))
}
