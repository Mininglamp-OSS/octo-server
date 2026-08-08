package file

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/authtree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ===========================================================================
// PR #713 review (lml2468 P1 / yujiawei S-1) — the presigned-upload integrity
// hole rejectCallerObjectKey exists for.
//
// getUploadCredentials signs a 30-minute PUT for whatever object key the caller
// names in `path`, and nothing below it checks who owns that key. On the uk tree
// that means a long-lived non-interactive bearer token handed to a CLI/drive client
// can obtain write access to any object whose key it knows, and overwrite it. The
// guard removes the caller's ability to name a target at all, so the key is always
// server-minted.
//
// Every case below drives the PRODUCTION route literals: newUKFileRoutes installs
// the uk tree's real mounter and then lets File.Route contribute, so deleting the
// guard from the Add site in api.go — not just from the guard body — turns these
// red. Credential gating (`uk_*` only) needs the real tree middleware and is
// asserted DB-backed in
// modules/botfather.TestUserKeyPresignedUploadRefusesCallerObjectKey.
// ===========================================================================

// newUKFileRoutes mounts the file module's uk-tree contributions the way botfather
// does — authtree.MountOn over a /v1/user group — and returns the mock storage so a
// case can tell whether the handler ran at all. The tree's own auth/tenant
// middleware is absent on purpose: these cases are about the route-local guard, and
// leaving auth out means a request that gets refused can only have been refused by
// the guard.
func newUKFileRoutes(t *testing.T) (*wkhttp.WKHttp, *mockService) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	svc := &mockService{}
	f := &File{
		ctx:     config.NewContext(cfg),
		Log:     log.NewTLog("uk-file-guard-test"),
		service: svc,
	}
	r := wkhttp.New()
	// Register the mount point first so File.Route's authtree.Add calls install
	// immediately into a group we can send requests to.
	authtree.Mount(authtree.TreeUserKey, r, authtree.MountOn(r.Group("/v1/user")))
	f.Route(r)
	return r, svc
}

func ukPresignedUploadGet(t *testing.T, r *wkhttp.WKHttp, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/user/file/upload/presigned?"+query, nil))
	return w
}

// A caller-named object key must never reach the signer. The whitespace case is
// listed separately because it is the one a TrimSpace-based guard would leak:
// getUploadCredentials branches on the raw query value, so `path=%20` still takes
// the custom-key branch and would get `common/ ` signed.
func TestUKPresignedUploadRefusesCallerNamedObjectKey(t *testing.T) {
	for _, query := range []string{
		"type=common&path=/common/victim/secret.pdf&filename=a.pdf&fileSize=10",
		"type=common&path=common/victim/secret.pdf&filename=a.pdf&fileSize=10",
		"type=chat&path=/chat/1/other-tenant.jpg&fileSize=10",
		"type=common&path=%20&filename=a.pdf&fileSize=10",
	} {
		t.Run(query, func(t *testing.T) {
			r, svc := newUKFileRoutes(t)

			w := ukPresignedUploadGet(t, r, query)
			assert.Equal(t, http.StatusBadRequest, w.Code,
				"a caller-named object key must be refused: %s", w.Body.String())
			assert.Contains(t, w.Body.String(), "自定义上传路径",
				"the refusal must be the guard's, not an unrelated validation error")
			assert.Empty(t, svc.lastObjectPath,
				"no PUT may be signed for a caller-named key")
		})
	}
}

// The positive control: without `path` the route still works, so the guard narrows
// what a uk key can name rather than disabling drive uploads. This is the exact
// shape octo-drive sends (its octoserver client omits `path` and stores whatever
// `key` comes back).
func TestUKPresignedUploadMintsServerSideObjectKey(t *testing.T) {
	r, svc := newUKFileRoutes(t)

	w := ukPresignedUploadGet(t, r, "type=common&filename=report.pdf&fileSize=2048")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	key, ok := resp["key"].(string)
	require.True(t, ok, "response must carry the object key: %s", w.Body.String())

	// <type>/<unix ts>/<uuid>/<uuid><ext> — every segment but the allow-listed type
	// prefix and the allow-listed extension is server-generated.
	parts := strings.Split(key, "/")
	require.Len(t, parts, 4, "key must be the server-minted shape, got %q", key)
	assert.Equal(t, "common", parts[0])
	assert.True(t, strings.HasSuffix(key, ".pdf"), "extension is preserved: %q", key)
	assert.NotContains(t, key, "report", "the caller's filename must not reach the key")
	assert.Equal(t, key, svc.lastObjectPath, "the signed key is the one returned")
}

// An absent `path` and a present-but-empty `path` are the same request to the
// handler, so both must pass the guard. Were it the other way, `?path=` would 400
// and break a client that always emits the parameter.
func TestUKPresignedUploadTreatsEmptyPathAsAbsent(t *testing.T) {
	r, _ := newUKFileRoutes(t)

	w := ukPresignedUploadGet(t, r, "type=common&path=&filename=report.pdf&fileSize=2048")
	assert.Equal(t, http.StatusOK, w.Code,
		"an empty path is not a caller-named key: %s", w.Body.String())
}

// The tenant declarations are what a reviewer reads instead of tracing into the
// handlers, so pin them to the actual posture: upload is confined by its own route
// guard, download deliberately is not (buckets are public-read, so an object key is
// already the read capability — see pkg/authtree's census).
func TestUKFileRouteTenantDeclarationsMatchTheGuards(t *testing.T) {
	captured := map[string]authtree.Route{}
	cfg := config.New()
	cfg.Test = true
	f := &File{ctx: config.NewContext(cfg), Log: log.NewTLog("uk-file-guard-test"), service: &mockService{}}
	r := wkhttp.New()
	authtree.Mount(authtree.TreeUserKey, r, func(rt authtree.Route) { captured[rt.Path] = rt })
	f.Route(r)

	upload, ok := captured["/file/upload/presigned"]
	require.True(t, ok, "the upload route must be contributed to the uk tree")
	assert.Equal(t, authtree.ScopeRouteGuard, upload.Tenant)
	assert.Len(t, upload.Middlewares, 1, "the declaration must come with the guard that backs it")

	download, ok := captured["/file/download/url"]
	require.True(t, ok, "the download route must be contributed to the uk tree")
	assert.Equal(t, authtree.ScopeUnscoped, download.Tenant)
	assert.Empty(t, download.Middlewares)
}

// The negative control that pins the guard as load-bearing rather than decorative:
// the same handler, invoked the way the human session route mounts it, still
// honours a caller-named key. Custom `path` is a capability octo-web uses today
// (file/upload/credentials?path=...), so this asserts the tightening did NOT leak
// into the shared handler.
func TestGetUploadCredentialsWithoutGuardStillHonoursCallerPath(t *testing.T) {
	svc := &mockService{}
	f := &File{Log: log.NewTLog("uk-file-guard-test"), service: svc}
	r := wkhttp.New()
	r.Group("/v1/file").GET("/upload/presigned", f.getUploadCredentials)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/v1/file/upload/presigned?type=common&path=/victim/secret.pdf&fileSize=10", nil))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.Equal(t, "common/victim/secret.pdf", svc.lastObjectPath,
		"the human route's custom-path capability must be untouched")
}
