package user

import (
	"context"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	libcommon "github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/auth"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	"github.com/stretchr/testify/require"
)

// The scheduled finalizer has no compensation path for WuKongIM: once the
// destroy transaction commits, `checkDestroyExpired` never selects the account
// again (is_destroy advanced past 1). A post-commit Redis revocation failure is
// retried by the outbox worker, so it must not suppress the independent IM
// logout.
func TestFinalizeDestroyStillQuitsIMDevicesAfterCommittedRevocationFailure(t *testing.T) {
	_, ctx, userAPI, _ := newTokenHTTPTestServer(t)
	uid := "fin-" + util.GenerUUID()[:12]
	require.NoError(t, userAPI.db.Insert(&Model{
		UID: uid, Name: "finalize user", Username: "fin_" + uid,
		ShortNo: "fin_" + uid, Phone: "13800000001", Zone: "0086",
		Status: int(libcommon.UserAvailable),
	}))
	require.NoError(t, userAPI.db.applyDestroy(uid, time.Now().Add(-30*24*time.Hour), time.Now().Add(-time.Hour)))

	var imMu sync.Mutex
	deviceQuitCalls := 0
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/device_quit" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":0}`))
			return
		}
		imMu.Lock()
		deviceQuitCalls++
		imMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(im.Close)
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	store := &flakyRevokeAllSessionStore{}
	userAPI.sessionStore = store

	expired, err := userAPI.db.queryDestroyExpired(time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)

	require.NoError(t, userAPI.finalizeDestroy(expired[0]),
		"a committed finalization is terminal: a deferred Redis apply is not a finalizer failure")

	finalized, err := userAPI.db.QueryByUID(uid)
	require.NoError(t, err)
	require.Equal(t, IsDestroyDone, finalized.IsDestroy, "the destroy transaction must remain committed")
	require.Equal(t, 1, store.callCount())
	imMu.Lock()
	require.Equal(t, 1, deviceQuitCalls,
		"post-commit Redis failure must not skip the independent WuKongIM logout")
	imMu.Unlock()

	var pending int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").
		Where("uid=? AND status=?", uid, sessionRevocationPending).Load(&pending)
	require.NoError(t, err)
	require.Equal(t, 1, pending, "the failed apply must stay pending for the outbox worker")
}

// The other side of the same boundary: a mutation that never committed emits no
// external side effects at all.
func TestFinalizeDestroyEmitsNoSideEffectsWhenTransactionDidNotCommit(t *testing.T) {
	_, ctx, userAPI, _ := newTokenHTTPTestServer(t)
	uid := "cnl-" + util.GenerUUID()[:12]
	require.NoError(t, userAPI.db.Insert(&Model{
		UID: uid, Name: "cancel racer", Username: "cnl_" + uid,
		ShortNo: "cnl_" + uid, Phone: "13800000002", Zone: "0086",
		Status: int(libcommon.UserAvailable),
	}))
	require.NoError(t, userAPI.db.applyDestroy(uid, time.Now().Add(-30*24*time.Hour), time.Now().Add(-time.Hour)))

	var imMu sync.Mutex
	deviceQuitCalls := 0
	im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user/device_quit" {
			imMu.Lock()
			deviceQuitCalls++
			imMu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":0}`))
	}))
	t.Cleanup(im.Close)
	ctx.GetConfig().WuKongIM.APIURL = im.URL

	revokeStore := &recordingDeviceRevokeStore{
		tokens:   map[int]string{int(config.APP): "conflict-app-token"},
		failures: make(map[string]error),
	}
	userAPI.sessionStore = revokeStore

	expired, err := userAPI.db.queryDestroyExpired(time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, expired, 1)
	// The user cancels between selection and the finalizing write.
	require.NoError(t, userAPI.db.cancelDestroy(uid))

	require.NoError(t, userAPI.finalizeDestroy(expired[0]),
		"a state conflict must be skipped silently, not reported as a finalizer failure")

	unchanged, err := userAPI.db.QueryByUID(uid)
	require.NoError(t, err)
	require.Equal(t, IsDestroyNo, unchanged.IsDestroy)
	require.NotContains(t, unchanged.Phone, "@delete")
	require.Empty(t, revokeStore.revoked, "an uncommitted finalization must not revoke bearers")
	imMu.Lock()
	require.Zero(t, deviceQuitCalls, "an uncommitted finalization must not log devices out")
	imMu.Unlock()
}

// deleteAdminUsers hard-deletes the user row and then, in the shipped `expand`
// default, revokes bearers through Session Store. That revocation is the only
// one there is: no revocation intent exists in this mode and the row is gone, so
// neither the runtime nor an operator can retry it. A disconnected client must
// therefore not be able to cancel it.
func TestDeleteAdminUserRevokesEveryBearerDespiteCanceledRequestContext(t *testing.T) {
	server, _, _, managerAPI := newTokenHTTPTestServer(t)
	wireI18nRendererForUserTest(server)
	uid := "adm-" + util.GenerUUID()[:12]
	require.NoError(t, managerAPI.userDB.Insert(&Model{
		UID: uid, Name: "deleted admin", Username: "adm_" + uid,
		ShortNo: "adm_" + uid, Role: string(wkhttp.Admin),
		Status: int(libcommon.UserAvailable),
	}))

	store := &recordingDeviceRevokeStore{
		tokens: map[int]string{
			int(config.APP): "admin-app-token",
			int(config.Web): "admin-web-token",
			int(config.PC):  "admin-pc-token",
		},
		failures: make(map[string]error),
	}
	managerAPI.sessionStore = store
	require.False(t, sessionRevocationActive(managerAPI.sessionStore),
		"this regression covers the shipped compatibility mode")

	server.GetRoute().Any("/test/manager/admin/delete", func(c *wkhttp.Context) {
		c.Set("uid", "root")
		c.Set("role", string(wkhttp.SuperAdmin))
		managerAPI.deleteAdminUsers(c)
	})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/test/manager/admin/delete?uid="+uid, nil)
	server.GetRoute().ServeHTTP(recorder, request.WithContext(canceled))

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	deleted, err := managerAPI.userDB.QueryByUID(uid)
	require.NoError(t, err)
	require.Nil(t, deleted, "the administrator row must be deleted")
	require.ElementsMatch(t,
		[]string{"admin-app-token", "admin-web-token", "admin-pc-token"},
		store.revoked,
		"a committed administrator deletion must revoke every device bearer even if the client disconnected")
}

// A committed mutation whose revocation intent is already leased by the shared
// worker has succeeded — the intent is applied within the lease. Reporting that
// as a failure tells the user their password change failed after it committed,
// which invites a second change.
func TestClaimSessionRevocationTreatsForeignLeaseAsDeferredApply(t *testing.T) {
	_, _, userAPI, _ := newTokenHTTPTestServer(t)
	uid := "lease-" + util.GenerUUID()[:10]
	require.NoError(t, userAPI.db.Insert(&Model{
		UID: uid, Name: "lease user", Username: "ls_" + uid, ShortNo: "ls_" + uid,
		Password: "before", Status: int(libcommon.UserAvailable),
	}))
	intent, err := userAPI.db.updateUserFieldWithSessionRevocation(
		context.Background(), uid, "password", "after", "password_change")
	require.NoError(t, err)

	now := time.Now().UTC()
	claimed, err := userAPI.db.claimSessionRevocationByID(context.Background(), intent, "worker-owner", now)
	require.NoError(t, err)
	require.True(t, claimed, "the worker takes the lease first")

	// The synchronous request path arrives second. It cannot take the lease, but
	// the revocation is not lost: the current owner applies it within the lease.
	contender := *intent
	claimed, err = userAPI.db.claimSessionRevocationByID(context.Background(), &contender, "sync-owner", now)
	require.NoError(t, err,
		"another owner holding a live lease is deferred apply, not a failed revocation")
	require.False(t, claimed)

	// The lease must still be reclaimable once it expires, or a crashed owner
	// would strand the intent.
	claimed, err = userAPI.db.claimSessionRevocationByID(
		context.Background(), &contender, "sync-later", now.Add(sessionRevocationLease+time.Second))
	require.NoError(t, err)
	require.True(t, claimed, "an expired lease must be reclaimable")

	// A genuinely missing intent must still be an error: this must not become a
	// blanket "any claim failure is fine".
	missing := *intent
	missing.ID = intent.ID + 1_000_000
	_, err = userAPI.db.claimSessionRevocationByID(context.Background(), &missing, "sync-missing", now)
	require.Error(t, err, "a vanished intent row is a real fault, not deferred apply")
}

// End-to-end shape of the same failure: the caller gets 200 and the password is
// changed even though the worker owns the pending intent.
func TestPasswordChangeSucceedsWhileWorkerOwnsRevocationIntent(t *testing.T) {
	_, ctx, userAPI, _ := newTokenHTTPTestServer(t)
	uid := "wlease-" + util.GenerUUID()[:9]
	require.NoError(t, userAPI.db.Insert(&Model{
		UID: uid, Name: "worker lease user", Username: "wl_" + uid, ShortNo: "wl_" + uid,
		Password: "before", Status: int(libcommon.UserAvailable),
	}))

	client := octoredis.NewInstrumentedClient(ctx.GetConfig())
	store := auth.NewRedisSessionStore(
		client,
		"post-commit-token:"+util.GenerUUID()+":",
		"post-commit-uid:"+util.GenerUUID()+":",
		time.Hour,
		auth.WithSessionMode(auth.SessionModeRevoke),
		auth.WithSessionMaxPerUID(4),
	)

	// Reproduce the race deterministically: commit the intent, let the worker
	// take its lease, then run the request path's apply step.
	intent, err := userAPI.db.updateUserFieldWithSessionRevocation(
		context.Background(), uid, "password", "after", "password_change")
	require.NoError(t, err)
	claimed, err := userAPI.db.claimSessionRevocationByID(
		context.Background(), intent, "worker-owner", time.Now().UTC())
	require.NoError(t, err)
	require.True(t, claimed)

	require.NoError(t, applyAndMarkSessionRevocation(context.Background(), userAPI.db, store, intent),
		"a committed change whose intent is being applied by the worker must not report failure")

	changed, err := userAPI.db.QueryByUID(uid)
	require.NoError(t, err)
	require.Equal(t, "after", changed.Password)
	var pending int
	_, err = ctx.DB().Select("COUNT(*)").From("user_session_revocation_intent").
		Where("uid=? AND status=?", uid, sessionRevocationPending).Load(&pending)
	require.NoError(t, err)
	require.Equal(t, 1, pending, "the intent stays pending for its current owner")
}

// Class guard for the defect above: this is the fourth call site where a
// post-commit Session Store revocation was handed a cancelable request context,
// so assert the property instead of the individual call sites.
//
// A function is "cancel-bound" when it reaches a revocation primitive without
// detaching from its caller's cancellation (postCommitSecurityContext /
// context.WithoutCancel / context.Background). Handing a request-derived context
// to such a function makes the revocation abortable by the client.
func TestCommittedRevocationNeverReceivesRequestDerivedContext(t *testing.T) {
	// Session Store methods (and package helpers) that delete or rotate
	// credentials. Reaching any of these is what makes a function security
	// critical rather than merely context-taking.
	revocationPrimitives := map[string]bool{
		"RevokeAll":              true,
		"RevokeCurrent":          true,
		"RevokeIssued":           true,
		"DeleteToken":            true,
		"DeleteUIDToken":         true,
		"InvalidateCurrentToken": true,
		"revokeDeviceTokens":     true,
	}
	detachers := map[string]bool{
		"postCommitSecurityContext": true,
		"WithoutCancel":             true,
		"Background":                true,
		"TODO":                      true,
	}

	type function struct {
		file    string
		name    string
		body    *ast.BlockStmt
		calls   map[string]bool
		detach  bool
		reaches bool
	}

	entries, err := os.ReadDir(".")
	require.NoError(t, err)
	functions := make([]*function, 0, 256)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		fset := gotoken.NewFileSet()
		parsed, err := parser.ParseFile(fset, entry.Name(), nil, 0)
		require.NoError(t, err)
		for _, declaration := range parsed.Decls {
			declared, ok := declaration.(*ast.FuncDecl)
			if !ok || declared.Body == nil {
				continue
			}
			current := &function{
				file:  entry.Name(),
				name:  declared.Name.Name,
				body:  declared.Body,
				calls: make(map[string]bool),
			}
			ast.Inspect(declared.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calledFunctionName(call.Fun)
				current.calls[name] = true
				if detachers[name] {
					current.detach = true
				}
				if revocationPrimitives[name] {
					current.reaches = true
				}
				return true
			})
			functions = append(functions, current)
		}
	}
	require.NotEmpty(t, functions)

	// A cancel-bound function reaches a primitive without detaching. Propagate
	// through the call graph until it settles: a caller inherits the property
	// unless it detaches first.
	cancelBound := make(map[string]bool)
	for name := range revocationPrimitives {
		cancelBound[name] = true
	}
	for changed := true; changed; {
		changed = false
		for _, current := range functions {
			if current.detach || cancelBound[current.name] {
				continue
			}
			for callee := range current.calls {
				if cancelBound[callee] {
					cancelBound[current.name] = true
					changed = true
					break
				}
			}
		}
	}
	// Sanity-check the detector itself: the compatibility revocation helper is
	// cancel-bound by construction, and the detaching wrappers are not.
	require.True(t, cancelBound["revokeAllDeviceTokens"],
		"revokeAllDeviceTokens reaches Session Store without detaching; the detector must see it")
	require.False(t, cancelBound["finishCommittedUserSecurityMutation"],
		"finishCommittedUserSecurityMutation detaches, so callers may pass a request context")
	require.False(t, cancelBound["invalidateCurrentUserToken"],
		"invalidateCurrentUserToken detaches, so callers may pass a request context")

	var violations []string
	for _, current := range functions {
		ast.Inspect(current.body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !cancelBound[calledFunctionName(call.Fun)] {
				return true
			}
			for _, argument := range call.Args {
				if !isRequestDerivedContext(argument) {
					continue
				}
				violations = append(violations, current.file+"/"+current.name+" -> "+calledFunctionName(call.Fun))
			}
			return true
		})
	}
	sort.Strings(violations)
	require.Empty(t, violations,
		"a Session Store revocation that follows a committed write must run on a context detached "+
			"from the request (postCommitSecurityContext); client disconnect would otherwise skip it "+
			"with no retry path: %v", violations)
}

// isRequestDerivedContext reports whether the expression yields a context tied
// to the inbound HTTP request — `c.Request.Context()` or a bare `c.Context`.
func isRequestDerivedContext(expression ast.Expr) bool {
	switch typed := expression.(type) {
	case *ast.CallExpr:
		selector, ok := typed.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Context" {
			return false
		}
		inner, ok := selector.X.(*ast.SelectorExpr)
		return ok && inner.Sel.Name == "Request"
	case *ast.SelectorExpr:
		return typed.Sel.Name == "Context"
	default:
		return false
	}
}
