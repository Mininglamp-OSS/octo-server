package internal_membership_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	internalmembership "github.com/Mininglamp-OSS/octo-server/modules/internal_membership"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/gocraft/dbr/v2"
	"github.com/stretchr/testify/require"
)

const (
	// iupInternalToken is a deployment-shaped credential: long enough to clear the
	// module's 32-byte floor, and unlike every sibling fixed token.
	iupInternalToken = "internal-user-projects-test-token-0123456789"

	iupProjectsPath     = "/v1/internal/projects"
	iupCollationProbeDB = "octo_project_user_list_http_collation_probe"
)

// iupMountInternalRoute mounts the REAL module route — the same deadline →
// strict IP limiter → internal token chain production runs — on a private
// router, with the enablement env in place. No hand-assembled middleware list:
// a case that passes here exercises the wiring in Route().
func iupMountInternalRoute(t *testing.T, ctx *config.Context) *wkhttp.WKHttp {
	return iupMountInternalRouteWithContext(t, ctx)
}

func iupMountInternalRouteWithContext(t *testing.T, ctx *config.Context) *wkhttp.WKHttp {
	t.Helper()
	t.Setenv(internalmembership.MembershipInternalTokenEnv, iupInternalToken)
	t.Setenv(internalmembership.PeerMaxCacheAgeEnv, "30")
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	internalmembership.New(ctx).Route(route)
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

type iupHTTPProjectMetadata struct {
	ProjectID   string          `json:"project_id"`
	SpaceID     string          `json:"space_id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Logo        string          `json:"logo"`
	Role        json.RawMessage `json:"role"`
}

func iupDecodeProjectMetadata(t *testing.T, body []byte) []iupHTTPProjectMetadata {
	t.Helper()
	var rows []iupHTTPProjectMetadata
	require.NoError(t, json.Unmarshal(body, &rows), "body: %s", string(body))

	var wireRows []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &wireRows), "body: %s", string(body))
	for i, row := range wireRows {
		require.Len(t, row, 6, "project row must contain exactly the six contract fields")
		for _, field := range []string{"project_id", "space_id", "name", "description", "logo", "role"} {
			require.Contains(t, row, field)
		}
		require.Equal(t, "null", string(rows[i].Role), "role must be an explicit JSON null")
	}
	return rows
}

func iupProbeContext(t *testing.T, baseCtx *config.Context) *config.Context {
	t.Helper()
	addr := baseCtx.GetConfig().DB.MySQLAddr
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected DSN shape: %s", addr)
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}

	cfg := *baseCtx.GetConfig()
	cfg.DB.MySQLAddr = addr[:slash+1] + iupCollationProbeDB + opts
	ctx := config.NewContext(&cfg)
	session := ctx.DB()
	t.Cleanup(func() {
		if err := session.DB.Close(); err != nil {
			t.Errorf("close collation-probe database session: %v", err)
		}
	})
	return ctx
}

// iupHTTPProbeSession keeps the existing metadata HTTP collation test in an
// isolated schema after moving the test out of the Project package.
func iupHTTPProbeSession(t *testing.T, baseCtx *config.Context) *dbr.Session {
	t.Helper()
	addr := baseCtx.GetConfig().DB.MySQLAddr
	slash := strings.LastIndex(addr, "/")
	require.Positive(t, slash, "unexpected DSN shape: %s", addr)
	opts := ""
	if q := strings.Index(addr[slash:], "?"); q >= 0 {
		opts = addr[slash+q:]
	}
	admin, err := dbr.Open("mysql", addr[:slash+1]+opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close() })
	adminSess := admin.NewSession(nil)
	exec := func(sess *dbr.Session, stmt string) {
		_, e := sess.UpdateBySql(stmt).Exec()
		require.NoError(t, e, stmt)
	}
	exec(adminSess, "DROP DATABASE IF EXISTS `"+iupCollationProbeDB+"`")
	exec(adminSess, "CREATE DATABASE `"+iupCollationProbeDB+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	conn, err := dbr.Open("mysql", addr[:slash+1]+iupCollationProbeDB+opts, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = adminSess.UpdateBySql("DROP DATABASE IF EXISTS `" + iupCollationProbeDB + "`").Exec()
		_ = conn.Close()
	})
	sess := conn.NewSession(nil)
	var src string
	require.NoError(t, baseCtx.DB().SelectBySql("SELECT DATABASE()").LoadOne(&src))
	require.NotEmpty(t, src)
	for _, table := range []struct {
		name      string
		columns   []string
		collation string
	}{
		{"octo_project", []string{"project_id", "space_id"}, "utf8mb4_general_ci"},
		{"octo_project_member", []string{"project_id", "uid", "space_id"}, "utf8mb4_general_ci"},
		{"space", []string{"space_id"}, "utf8mb4_0900_ai_ci"},
		{"space_member", []string{"space_id", "uid"}, "utf8mb4_0900_ai_ci"},
		{"user", []string{"uid"}, "utf8mb4_0900_ai_ci"},
	} {
		exec(sess, "CREATE TABLE `"+table.name+"` LIKE `"+src+"`.`"+table.name+"`")
		mods := make([]string, 0, len(table.columns))
		for _, column := range table.columns {
			mods = append(mods, "MODIFY `"+column+"` VARCHAR(40) NOT NULL DEFAULT '' COLLATE "+table.collation)
		}
		exec(sess, "ALTER TABLE `"+table.name+"` "+strings.Join(mods, ", "))
	}
	return sess
}

func TestInternalUserProjectsHTTPActivatedMembership(t *testing.T) {
	_, ctx := newMembershipHTTPTest(t)
	seedUserBareHTTP(t, ctx, "internal-reader")
	seedSpaceHTTP(t, ctx, iupSpaceA, 1)
	seedSpaceMemberHTTP(t, ctx, iupSpaceA, "internal-reader", 0, 1)
	_, err := ctx.DB().InsertBySql(
		"INSERT INTO octo_project (project_id, space_id, name, description, creator, status, activated_at, created_at, updated_at) "+
			"VALUES ('internal-ready', ?, 'Ready project', 'Ready description', 'internal-reader', 1, NOW(3), NOW(3), NOW(3)), "+
			"('internal-pending', ?, 'Pending project', '', 'internal-reader', 1, NULL, NOW(3), NOW(3))",
		iupSpaceA, iupSpaceA,
	).Exec()
	require.NoError(t, err)
	_, err = ctx.DB().InsertBySql(
		"INSERT INTO octo_project_member (project_id, uid, space_id, role, status, removing, created_at, updated_at) "+
			"VALUES ('internal-ready', 'internal-reader', ?, 0, 1, 0, NOW(3), NOW(3)), "+
			"('internal-pending', 'internal-reader', ?, 2, 1, 0, NOW(3), NOW(3))",
		iupSpaceA, iupSpaceA,
	).Exec()
	require.NoError(t, err)

	const token = "project-list-http-test-token-0123456789"
	t.Setenv(internalmembership.MembershipInternalTokenEnv, token)
	t.Setenv(internalmembership.PeerMaxCacheAgeEnv, "30")
	route := wkhttp.New()
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	internalmembership.New(ctx).Route(route)
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
	_, ctx := newMembershipHTTPTest(t)
	const owner = "iup-http-owner"
	seedUserBareHTTP(t, ctx, owner)
	seedSpaceHTTP(t, ctx, iupSpaceA, 1)
	seedSpaceMemberHTTP(t, ctx, iupSpaceA, owner, 0, 1)
	iupProjectHTTP(t, ctx, "iup-http-secret", iupSpaceA, owner, "Should not leak", "", "", 1, true)
	iupSeatHTTP(t, ctx, "iup-http-secret", owner, iupSpaceA, 0, 1, 0)

	route := iupMountInternalRoute(t, ctx)
	path := iupProjectsPath + "?space_id=" + iupSpaceA + "&uid=" + owner

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
	userToken := seedUserHTTP(t, ctx, "iup-http-session-user")
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
	_, ctx := newMembershipHTTPTest(t)
	const (
		target   = "iup-http-target"
		other    = "iup-http-other"
		outsider = "iup-http-outsider"
	)
	seedUserBareHTTP(t, ctx, target)
	otherToken := seedUserHTTP(t, ctx, other) // a REAL user session, minted for the forgery below
	seedUserBareHTTP(t, ctx, outsider)
	seedSpaceHTTP(t, ctx, iupSpaceA, 1)
	seedSpaceHTTP(t, ctx, iupSpaceB, 1)
	seedSpaceMemberHTTP(t, ctx, iupSpaceA, target, 0, 1)
	seedSpaceMemberHTTP(t, ctx, iupSpaceB, target, 0, 1)
	seedSpaceMemberHTTP(t, ctx, iupSpaceA, other, 0, 1)
	seedSpaceMemberHTTP(t, ctx, iupSpaceB, other, 0, 1)

	// octo_project.description is TEXT; go well past the historical 500-character
	// limit and assert the value comes back unmodified.
	longDescription := strings.Repeat("长", 1024)

	iupProjectHTTP(t, ctx, "iup-http-a-member", iupSpaceA, "someone-else", "Member project", longDescription, "", 1, true)
	iupProjectHTTP(t, ctx, "iup-http-a-owner", iupSpaceA, "someone-else", "Owner project", "", "https://cdn.test/logo.png", 1, true)
	iupProjectHTTP(t, ctx, "iup-http-a-pending", iupSpaceA, "someone-else", "Awaiting activation", "", "", 1, false)
	iupProjectHTTP(t, ctx, "iup-http-a-other", iupSpaceA, other, "Other user project", "", "", 1, true)
	iupProjectHTTP(t, ctx, "iup-http-b", iupSpaceB, "someone-else", "Project B", "description B", "", 1, true)

	iupSeatHTTP(t, ctx, "iup-http-a-member", target, iupSpaceA, 0, 1, 0)
	iupSeatHTTP(t, ctx, "iup-http-a-owner", target, iupSpaceA, 2, 1, 0)
	iupSeatHTTP(t, ctx, "iup-http-a-pending", target, iupSpaceA, 2, 1, 0)
	iupSeatHTTP(t, ctx, "iup-http-a-other", other, iupSpaceA, 0, 1, 0)
	iupSeatHTTP(t, ctx, "iup-http-b", target, iupSpaceB, 1, 1, 0)
	iupSeatHTTP(t, ctx, "iup-http-b", other, iupSpaceB, 1, 1, 0)

	srv := httptest.NewServer(iupMountInternalRoute(t, ctx))
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
		"space_id":         iupSpaceB,
		"X-Space-Id":       iupSpaceB,
	}

	resp, body := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+iupSpaceA+"&uid="+target, forged)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	require.Equal(t, []iupHTTPProject{
		{ProjectID: "iup-http-a-owner", SpaceID: iupSpaceA, Name: "Owner project", Description: "", Logo: "https://cdn.test/logo.png", Role: 2},
		{ProjectID: "iup-http-a-member", SpaceID: iupSpaceA, Name: "Member project", Description: longDescription, Logo: "", Role: 0},
	}, iupDecodeProjects(t, body),
		"the answer must be the QUERY target's activated projects in the QUERY Space, id DESC — no header may widen it")
	require.NotContains(t, string(body), "Other user project")
	// Field PRESENCE, not just decoded values: a role/logo dropped by omitempty would
	// read as "no member role" / "no logo setting" to a consumer.
	require.Contains(t, string(body), `"role":0`)
	require.Contains(t, string(body), `"logo":""`)

	// Pagination parameters are not part of this contract; the caller does not know
	// the project ids and cannot page, so they must not truncate a single row.
	_, pagedBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+iupSpaceA+"&uid="+target+"&page=1&limit=1", forged)
	require.Equal(t, iupDecodeProjects(t, body), iupDecodeProjects(t, pagedBody),
		"page/limit must be ignored, not applied as a cap")

	// The uid selector is what selects the subject: asking about the other user in
	// the SAME Space returns THEIR project, not the target's.
	_, otherBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+iupSpaceA+"&uid="+other, forged)
	require.Equal(t, []iupHTTPProject{
		{ProjectID: "iup-http-a-other", SpaceID: iupSpaceA, Name: "Other user project", Description: "", Logo: "", Role: 0},
	}, iupDecodeProjects(t, otherBody))

	// Space isolation on the wire.
	_, bBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+iupSpaceB+"&uid="+target, forged)
	require.Equal(t, []iupHTTPProject{
		{ProjectID: "iup-http-b", SpaceID: iupSpaceB, Name: "Project B", Description: "description B", Logo: "", Role: 1},
	}, iupDecodeProjects(t, bBody))

	// Truthful negatives are 200 with an EMPTY ARRAY.
	_, emptyBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id="+iupSpaceA+"&uid="+outsider, forged)
	require.Equal(t, "[]", strings.TrimSpace(string(emptyBody)), "no membership must be [] on the wire, not null")

	_, unknownBody := iupSocketGet(t, base, client, iupProjectsPath+"?space_id=space_unknown&uid="+target, forged)
	require.Equal(t, "[]", strings.TrimSpace(string(unknownBody)))
}

func TestInternalProjectsByIDHTTPMetadataFiltersAndRename(t *testing.T) {
	_, ctx := newMembershipHTTPTest(t)
	const (
		readyID       = "IUP-ID-Ready"
		pendingID     = "iup-id-pending"
		disbandedID   = "iup-id-disbanded"
		otherSpaceID  = "iup-id-other-space"
		inactiveSpace = "iup-id-inactive-space"
		inactiveID    = "iup-id-inactive-space-project"
	)
	seedSpaceHTTP(t, ctx, iupSpaceA, 1)
	seedSpaceHTTP(t, ctx, iupSpaceB, 1)
	seedSpaceHTTP(t, ctx, inactiveSpace, 2)
	longDescription := strings.Repeat("长", 1024)
	iupProjectHTTP(t, ctx, readyID, iupSpaceA, "unrelated-creator", "Current project name", longDescription, "", 1, true)
	iupProjectHTTP(t, ctx, pendingID, iupSpaceA, "unrelated-creator", "Awaiting activation", "", "", 1, false)
	iupProjectHTTP(t, ctx, disbandedID, iupSpaceA, "unrelated-creator", "Disbanded", "", "", 2, true)
	iupProjectHTTP(t, ctx, otherSpaceID, iupSpaceB, "unrelated-creator", "Other Space", "", "", 1, true)
	iupProjectHTTP(t, ctx, inactiveID, inactiveSpace, "unrelated-creator", "Inactive Space", "", "", 1, true)
	// No user, Space-member, or Project-member fixtures are created: metadata reads
	// must not acquire an implicit user or membership precondition.

	srv := httptest.NewServer(iupMountInternalRoute(t, ctx))
	t.Cleanup(srv.Close)
	headers := map[string]string{
		"X-Internal-Token": iupInternalToken,
		"X-Real-Ip":        "198.51.100.41",
	}
	query := url.Values{}
	query.Set("space_id", iupSpaceA)
	query.Add("project_ids", " ,iup-id-ready,, ")
	query.Add("project_ids", "IUP-ID-READY,unknown-id,"+pendingID+","+disbandedID+","+otherSpaceID)
	query.Set("page", "9")
	query.Set("limit", "1")
	response, body := iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, []iupHTTPProjectMetadata{{
		ProjectID:   readyID,
		SpaceID:     iupSpaceA,
		Name:        "Current project name",
		Description: longDescription,
		Logo:        "",
		Role:        json.RawMessage("null"),
	}}, iupDecodeProjectMetadata(t, body))

	// A subsequent read sees the committed database name, without process restart
	// or cache invalidation.
	_, err := ctx.DB().UpdateBySql(
		"UPDATE octo_project SET name = ? WHERE project_id = ?", "Renamed project", readyID).Exec()
	require.NoError(t, err)
	headers["X-Real-Ip"] = "198.51.100.42"
	query.Set("project_ids", "iup-id-ready")
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	rows := iupDecodeProjectMetadata(t, body)
	require.Len(t, rows, 1)
	require.Equal(t, "Renamed project", rows[0].Name)
	headers["X-Real-Ip"] = "198.51.100.46"
	query.Set("space_id", iupSpaceA)
	query.Set("project_ids", "unknown-id")
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, "[]", strings.TrimSpace(string(body)),
		"an all-unmatched ID request must be a literal empty array")

	// The Space selector isolates the other-tenant project: it was excluded above
	// under spaceA and is returned when queried in its actual active Space.
	headers["X-Real-Ip"] = "198.51.100.43"
	query.Set("space_id", iupSpaceB)
	query.Set("project_ids", otherSpaceID)
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, []iupHTTPProjectMetadata{{
		ProjectID: otherSpaceID,
		SpaceID:   iupSpaceB,
		Name:      "Other Space",
		Role:      json.RawMessage("null"),
	}}, iupDecodeProjectMetadata(t, body))

	headers["X-Real-Ip"] = "198.51.100.44"
	query.Set("space_id", inactiveSpace)
	query.Set("project_ids", inactiveID)
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, "[]", strings.TrimSpace(string(body)))

	headers["X-Real-Ip"] = "198.51.100.45"
	query.Set("space_id", "iup-unknown-space")
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, "[]", strings.TrimSpace(string(body)))
}

func TestInternalProjectsByIDHTTPAcceptsExactlyFiftyUniqueIDs(t *testing.T) {
	_, ctx := newMembershipHTTPTest(t)
	seedSpaceHTTP(t, ctx, iupSpaceA, 1)
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = fmt.Sprintf("iup-id-limit-%02d", i)
		iupProjectHTTP(t, ctx, ids[i], iupSpaceA, "unrelated-creator", ids[i], "", "", 1, true)
	}

	srv := httptest.NewServer(iupMountInternalRoute(t, ctx))
	t.Cleanup(srv.Close)
	headers := map[string]string{
		"X-Internal-Token": iupInternalToken,
		"X-Real-Ip":        "198.51.100.51",
	}
	query := url.Values{}
	query.Set("space_id", iupSpaceA)
	query.Set("project_ids", ", "+ids[0]+", ,"+strings.Join(ids[1:], ", ")+", "+ids[0]+",")
	query.Set("page", "1")
	query.Set("limit", "1")
	response, body := iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	rows := iupDecodeProjectMetadata(t, body)
	require.Len(t, rows, 50)
	got := make(map[string]bool, len(rows))
	for _, row := range rows {
		got[row.ProjectID] = true
	}
	for _, id := range ids {
		require.True(t, got[id], "missing project %q", id)
	}

	headers["X-Real-Ip"] = "198.51.100.52"
	ids = append(ids, "iup-id-limit-50")
	query.Set("project_ids", strings.Join(ids, ","))
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusBadRequest, response.StatusCode, string(body))
	errEnv := iupDecodeError(t, body)
	require.Equal(t, "err.shared.param.invalid", errEnv.Error.Code)
	require.Equal(t, "project_ids", errEnv.Error.Details["field"])
}

func TestInternalProjectsByIDHTTPBindsMixedCollationIdentitiesAndFailsClosed(t *testing.T) {
	_, ctx := newMembershipHTTPTest(t)
	session := iupHTTPProbeSession(t, ctx)
	_, err := session.InsertBySql(
		"INSERT INTO `space` (space_id, name, creator, status) VALUES " +
			"('s', 'active', 'creator', 1), ('ß', 'inactive', 'creator', 2)").Exec()
	require.NoError(t, err)

	insertProject := func(projectID, projectSpace, name string) {
		_, insertErr := session.InsertBySql(
			"INSERT INTO octo_project "+
				"(project_id, space_id, name, description, logo, creator, status, activated_at, created_at, updated_at) "+
				"VALUES (?, ?, ?, ?, ?, 'unrelated-creator', 1, UTC_TIMESTAMP(3), UTC_TIMESTAMP(3), UTC_TIMESTAMP(3))",
			projectID, projectSpace, name, "stored description", "",
		).Exec()
		require.NoError(t, insertErr)
	}
	insertProject("IUP-ID-ASCII", "s", "ASCII case project")
	insertProject("iup-id-cross-space", "ß", "Other identity project")
	insertProject("project-ß", "s", "Collation-equivalent but distinct project ID")

	// Assert that MySQL's general_ci equality really does collapse the hostile
	// pairs; otherwise this fixture would not exercise the Go-side identity bind.
	var crossSpaceMatch int
	require.NoError(t, session.SelectBySql(
		"SELECT COUNT(*) FROM octo_project WHERE project_id = ? AND space_id = ?",
		"iup-id-cross-space", "s",
	).LoadOne(&crossSpaceMatch))
	require.Equal(t, 1, crossSpaceMatch)
	var projectIDMatch int
	require.NoError(t, session.SelectBySql(
		"SELECT COUNT(*) FROM octo_project WHERE project_id = ?", "project-s",
	).LoadOne(&projectIDMatch))
	require.Equal(t, 1, projectIDMatch)

	probeCtx := iupProbeContext(t, ctx)
	srv := httptest.NewServer(iupMountInternalRouteWithContext(t, probeCtx))
	t.Cleanup(srv.Close)
	headers := map[string]string{
		"X-Internal-Token": iupInternalToken,
		"X-Real-Ip":        "198.51.100.61",
	}
	query := url.Values{}
	query.Set("space_id", "S")
	query.Set("project_ids", "iup-id-ascii")
	response, body := iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, []iupHTTPProjectMetadata{{
		ProjectID:   "IUP-ID-ASCII",
		SpaceID:     "s",
		Name:        "ASCII case project",
		Description: "stored description",
		Logo:        "",
		Role:        json.RawMessage("null"),
	}}, iupDecodeProjectMetadata(t, body))

	headers["X-Real-Ip"] = "198.51.100.62"
	query.Set("project_ids", "iup-id-cross-space")
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, "[]", strings.TrimSpace(string(body)),
		"a SQL-collation-equal Space must not be treated as the resolved Space identity")

	headers["X-Real-Ip"] = "198.51.100.63"
	query.Set("project_ids", "project-s")
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusOK, response.StatusCode, string(body))
	require.Equal(t, "[]", strings.TrimSpace(string(body)),
		"a SQL-collation-equal Project ID must not expand the ASCII FoldID request set")

	// The fixture database is disposable and is dropped by iupHTTPProbeSession cleanup.
	// Removing just the project table makes the real query fail after Space lookup.
	_, err = session.UpdateBySql("RENAME TABLE `octo_project` TO `octo_project_offline`").Exec()
	require.NoError(t, err)
	headers["X-Real-Ip"] = "198.51.100.64"
	query.Set("project_ids", "iup-id-ascii")
	response, body = iupSocketGet(t, srv.URL, srv.Client(), iupProjectsPath+"?"+query.Encode(), headers)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode, string(body))
	require.Equal(t, "err.shared.internal", iupDecodeError(t, body).Error.Code)
}
