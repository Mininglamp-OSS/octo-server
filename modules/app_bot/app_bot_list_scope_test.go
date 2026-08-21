package app_bot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newScopeListingRoute is newPlatformGateRouteWithMock with one difference: the query
// matcher accepts everything and records the SQL it saw. dbr interpolates arguments into
// the statement under the MySQL dialect, so sqlmock never sees bound arguments and the
// predicates can only be asserted by reading the SQL. Recording also lets a test prove a
// predicate is *absent*, which is the whole point of scope=all.
func newScopeListingRoute(t *testing.T) (*wkhttp.WKHttp, sqlmock.Sqlmock, *[]string, func()) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	ctx := config.NewContext(cfg)
	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.Admin)))

	seen := &[]string{}
	rawDB, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherFunc(func(_, actualSQL string) error {
			*seen = append(*seen, actualSQL)
			return nil
		}),
	))
	require.NoError(t, err)
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}

	route := wkhttp.New()
	ab := &AppBot{
		ctx: ctx,
		db:  &appBotDB{ctx: ctx, session: conn.NewSession(nil)},
		Log: log.NewTLog("AppBotListScopeTest"),
	}
	ab.Route(route)
	return route, mock, seen, func() { _ = rawDB.Close() }
}

func botRows(scope, spaceID string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "uid", "display_name", "status", "scope", "space_id"}).
		AddRow("scout", "app_scout_bot", "Scout", 1, scope, spaceID)
}

func listScoped(t *testing.T, query string, arrange func(sqlmock.Sqlmock)) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	route, mock, seen, cleanup := newScopeListingRoute(t)
	defer cleanup()
	if arrange != nil {
		arrange(mock)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/admin/app_bot"+query, nil)
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(w, req)
	return w, *seen
}

// listingSQL returns the statement that read the page (as opposed to the count, or the
// space-name lookup), which is where the scope predicate has to be visible.
func listingSQL(t *testing.T, seen []string) string {
	t.Helper()
	for _, sql := range seen {
		if strings.Contains(sql, "FROM app_bot") && !strings.Contains(sql, "count(") {
			return sql
		}
	}
	t.Fatalf("no app_bot page query was issued; saw %v", seen)
	return ""
}

// The default is unchanged: no scope parameter still means platform-only, so a client
// written against the old behaviour sees exactly what it saw before.
func TestListAppBots_DefaultsToPlatformScope(t *testing.T) {
	w, seen := listScoped(t, "", func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery("count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery("app_bot").WillReturnRows(botRows("platform", ""))
	})

	require.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, listingSQL(t, seen), "scope='platform'")
}

// scope=all drops the scope predicate — that is the entire point. A bot that moved into
// a space stays visible to the operator responsible for the deployment.
func TestListAppBots_ScopeAllDropsThePredicate(t *testing.T) {
	w, seen := listScoped(t, "?scope=all", func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery("count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery("app_bot").WillReturnRows(botRows("space", "space-x"))
		mock.ExpectQuery("space").WillReturnRows(
			sqlmock.NewRows([]string{"space_id", "name"}).AddRow("space-x", "Research"))
	})

	require.Equal(t, http.StatusOK, w.Code)
	assert.NotContains(t, listingSQL(t, seen), "scope=", "scope=all must not filter by scope")

	body := w.Body.String()
	assert.Contains(t, body, `"space_id":"space-x"`, "rows carry the owning space id")
	assert.Contains(t, body, `"space_name":"Research"`, "and the space is named, not just id'd")
}

// scope=space narrows to one space when a space_id comes with it.
func TestListAppBots_ScopeSpaceNarrowsToOneSpace(t *testing.T) {
	w, seen := listScoped(t, "?scope=space&space_id=space-x", func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery("count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery("app_bot").WillReturnRows(botRows("space", "space-x"))
		mock.ExpectQuery("space").WillReturnRows(
			sqlmock.NewRows([]string{"space_id", "name"}).AddRow("space-x", "Research"))
	})

	require.Equal(t, http.StatusOK, w.Code)
	sql := listingSQL(t, seen)
	assert.Contains(t, sql, "scope='space'")
	assert.Contains(t, sql, "space_id='space-x'")
}

// An unknown scope is rejected rather than quietly treated as one of the valid ones: a
// typo must not change what an operator believes they are looking at.
func TestListAppBots_UnknownScopeRejected(t *testing.T) {
	w, _ := listScoped(t, "?scope=everything", nil)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// A failed space-name lookup degrades to bare ids instead of failing the listing.
func TestListAppBots_SpaceNameLookupFailureStillLists(t *testing.T) {
	w, _ := listScoped(t, "?scope=all", func(mock sqlmock.Sqlmock) {
		mock.ExpectQuery("count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		mock.ExpectQuery("app_bot").WillReturnRows(botRows("space", "space-x"))
		mock.ExpectQuery("space").WillReturnError(assert.AnError)
	})

	require.Equal(t, http.StatusOK, w.Code)
	body := w.Body.String()
	assert.Contains(t, body, `"space_id":"space-x"`)
	assert.NotContains(t, body, "space_name")
}

// The widening is read-only. Listing may now cross scopes; mutating must not. This pins
// the boundary botInRouteScope draws, next to the existing reveal-token regression test —
// a wider listing must never become a way to reach another tenant's bot.
func TestScopeWidening_DoesNotOpenMutationsAcrossScopes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"publish", http.MethodPost, "/v1/admin/app_bot/space-bot-1/publish"},
		{"unpublish", http.MethodPost, "/v1/admin/app_bot/space-bot-1/unpublish"},
		{"delete", http.MethodDelete, "/v1/admin/app_bot/space-bot-1"},
		{"rotate token", http.MethodPost, "/v1/admin/app_bot/space-bot-1/token"},
		{"reveal token", http.MethodPost, "/v1/admin/app_bot/space-bot-1/token/reveal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route, mock, _, cleanup := newScopeListingRoute(t)
			defer cleanup()
			mock.ExpectQuery("app_bot").WillReturnRows(
				sqlmock.NewRows([]string{"id", "scope", "space_id", "status", "token"}).
					AddRow("space-bot-1", "space", "X", 1, "super-secret-space-token"))

			w := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("token", testutil.Token)
			route.ServeHTTP(w, req)

			assert.Equal(t, http.StatusNotFound, w.Code,
				"platform route must still refuse to mutate a space bot")
			assert.NotContains(t, w.Body.String(), "super-secret-space-token")
		})
	}
}
