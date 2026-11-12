package robot

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

func TestPermissionMyBotsExactSpaceAccess(t *testing.T) {
	for _, state := range []string{"member", "nonmember", "query_failure", "legacy"} {
		t.Run(state, func(t *testing.T) {
			db, mock, e := sqlmock.New()
			require.NoError(t, e)
			defer db.Close()
			conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
			rb := &Robot{db: robotDB{session: conn.NewSession(nil)}, Log: log.NewTLog("synthetic-scope-test")}
			if state != "legacy" {
				q := mock.ExpectQuery("SELECT COUNT.*FROM space_member sm.*INNER JOIN space.*WHERE sm.uid = 'caller' AND sm.space_id = 'space'")
				if state == "query_failure" {
					q.WillReturnError(errors.New("private cause"))
				} else {
					n := 0
					if state == "member" {
						n = 1
					}
					q.WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
				}
			}
			if state == "member" || state == "legacy" {
				mock.ExpectQuery("SELECT f.to_uid.*FROM friend f").WillReturnRows(sqlmock.NewRows([]string{"uid", "name", "description", "creator_uid", "bot_commands"}))
			}
			r := helperHarness(func(c *wkhttp.Context) { c.Set("uid", "caller"); rb.myBots(c) })
			rec := httptest.NewRecorder()
			path := "/probe?space_id=space"
			if state == "legacy" {
				path = "/probe"
			}
			r.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
			if state == "member" || state == "legacy" {
				require.Equal(t, 200, rec.Code, rec.Body.String())
				require.Equal(t, "[]", rec.Body.String())
			} else {
				require.Equal(t, 400, rec.Code)
				require.NotContains(t, rec.Body.String(), "private cause")
			}
			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

type routeTestReleases struct{ pin permissionbundle.CatalogPin }

func (r routeTestReleases) Load(_ context.Context, p permissionbundle.CatalogPin) (permissionbundle.Release, error) {
	if p != r.pin {
		return permissionbundle.Release{}, permissionbundle.ErrUnavailable
	}
	return permissionbundle.Release{Pin: p, Bytes: []byte(`{}`), Capabilities: map[string]bool{"op:group.get": true}}, nil
}
func (r routeTestReleases) SameIntent(context.Context, permissionbundle.CatalogPin, permissionbundle.CatalogPin, permissionbundle.Rule) (bool, error) {
	return true, nil
}
func (r routeTestReleases) Compatible(context.Context, permissionbundle.CatalogPin, permissionbundle.CatalogPin) error {
	return nil
}

type routeTestAuthorityDriver struct{ digest string }

func (d routeTestAuthorityDriver) RecoverAtLeast(context.Context, string) (permissionbundle.RecoveryFenceV1, error) {
	return permissionbundle.RecoveryFenceV1{
		AuthorityID:                "sql-ha",
		Incarnation:                strings.Repeat("a", 32),
		ElectionGeneration:         1,
		MinAckPosition:             1,
		RecoverableScopeCheckpoint: 1,
		CatalogDigest:              d.digest,
	}, nil
}
func (d routeTestAuthorityDriver) AcquireCurrent(context.Context, *dbr.Tx) (permissionbundle.ElectedAuthoritySession, error) {
	return permissionbundle.ElectedAuthoritySession{
		AuthorityID:        "sql-ha",
		Incarnation:        strings.Repeat("a", 32),
		ElectionGeneration: 1,
		Position:           1,
	}, nil
}
func (d routeTestAuthorityDriver) AssertCurrent(context.Context, *dbr.Tx, permissionbundle.ElectedAuthoritySession) error {
	return nil
}
func (d routeTestAuthorityDriver) RecordAcknowledgedPosition(context.Context, permissionbundle.ElectedAuthoritySession) error {
	return nil
}

func TestPermissionOptInRouteCompositionMountsWhenValidAndRemainsUnmountedWhenInvalid(t *testing.T) {
	pin := permissionbundle.CatalogPin{Version: "synthetic-v1", Digest: strings.Repeat("b", 64)}
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	session := conn.NewSession(nil)

	// Invalid authority mode fails closed and leaves routes unmounted.
	badCtx := config.NewContext(config.New())
	err = permissionbundle.InstallOptIn(badCtx, permissionbundle.InstallConfig{
		Enabled:       true,
		Session:       session,
		Origin:        "https://synthetic.invalid",
		Pin:           pin,
		Releases:      routeTestReleases{pin: pin},
		AuthorityMode: "sql_read_only_variables",
	})
	require.ErrorIs(t, err, permissionbundle.ErrUnavailable)
	require.Nil(t, permissionbundle.RuntimeFor(badCtx))

	// Valid opt-in installs Runtime and mounts Owner permission routes on Robot.Route.
	validCtx := config.NewContext(config.New())
	err = permissionbundle.InstallOptIn(validCtx, permissionbundle.InstallConfig{
		Enabled:         true,
		Session:         session,
		Origin:          "https://synthetic.invalid",
		Pin:             pin,
		Releases:        routeTestReleases{pin: pin},
		AuthorityMode:   permissionbundle.AuthorityModeExternalFencedV1,
		AuthorityDriver: routeTestAuthorityDriver{digest: pin.Digest},
	})
	require.NoError(t, err)
	require.NotNil(t, permissionbundle.RuntimeFor(validCtx))

	mock.ExpectQuery("FROM robot").WillReturnRows(sqlmock.NewRows([]string{"robot_id"}).AddRow(validCtx.GetConfig().Account.SystemUID))
	rb := &Robot{
		ctx:              validCtx,
		db:               robotDB{session: session},
		permissionBundle: permissionbundle.RuntimeFor(validCtx).Store(),
		Log:              log.NewTLog("optin-route-test"),
	}
	r := wkhttp.New()
	rb.Route(r)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/robot/bot_B/permission-bundle/v1?space_id=space"},
		{http.MethodPut, "/v1/robot/bot_B/permission-bundle/v1?space_id=space"},
		{http.MethodPost, "/v1/robot/bot_B/permission-bundle/v1/prepare?space_id=space"},
		{http.MethodGet, "/v1/robot/bot_B/permission-bundle/v1/catalog?space_id=space"},
		{http.MethodGet, "/v1/robot/bot_B/permission-bundle/v1/catalog/file?space_id=space"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		// Mounted routes reach AuthMiddleware (401 without token) instead of 404.
		require.Equal(t, http.StatusUnauthorized, rec.Code, "%s %s", tc.method, tc.path)
	}
}

