package bot_api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/message"
	"github.com/Mininglamp-OSS/octo-server/modules/messages_search"
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
	"github.com/stretchr/testify/require"
)

type botRouteTestReleases struct{ pin permissionbundle.CatalogPin }

func (r botRouteTestReleases) Load(_ context.Context, p permissionbundle.CatalogPin) (permissionbundle.Release, error) {
	if p != r.pin {
		return permissionbundle.Release{}, permissionbundle.ErrUnavailable
	}
	return permissionbundle.Release{Pin: p, Bytes: []byte(`{}`), Capabilities: map[string]bool{"op:group.get": true}}, nil
}
func (r botRouteTestReleases) SameIntent(context.Context, permissionbundle.CatalogPin, permissionbundle.CatalogPin, permissionbundle.Rule) (bool, error) {
	return true, nil
}
func (r botRouteTestReleases) Compatible(context.Context, permissionbundle.CatalogPin, permissionbundle.CatalogPin) error {
	return nil
}

type botRouteTestAuthorityDriver struct{ digest string }

func (d botRouteTestAuthorityDriver) RecoverAtLeast(context.Context, string) (permissionbundle.RecoveryFenceV1, error) {
	return permissionbundle.RecoveryFenceV1{
		AuthorityID:                "sql-ha",
		Incarnation:                strings.Repeat("a", 32),
		ElectionGeneration:         1,
		MinAckPosition:             1,
		RecoverableScopeCheckpoint: 1,
		CatalogDigest:              d.digest,
	}, nil
}
func (d botRouteTestAuthorityDriver) AcquireCurrent(context.Context, *dbr.Tx) (permissionbundle.ElectedAuthoritySession, error) {
	return permissionbundle.ElectedAuthoritySession{
		AuthorityID:        "sql-ha",
		Incarnation:        strings.Repeat("a", 32),
		ElectionGeneration: 1,
		Position:           1,
	}, nil
}
func (d botRouteTestAuthorityDriver) AssertCurrent(context.Context, *dbr.Tx, permissionbundle.ElectedAuthoritySession) error {
	return nil
}
func (d botRouteTestAuthorityDriver) RecordAcknowledgedPosition(context.Context, permissionbundle.ElectedAuthoritySession) error {
	return nil
}

type botRouteTestIMProvider struct{}

func (botRouteTestIMProvider) InspectDeliveredHeadersV1(context.Context, permissionbundle.Key, string, []string) ([]permissionbundle.IMResolvedDelivery, permissionbundle.IMAuthorityCut, error) {
	now := time.Now().UTC()
	return nil, permissionbundle.IMAuthorityCut{AuthorityID: "im", Incarnation: strings.Repeat("1", 32), Position: "1", ReadNonce: strings.Repeat("2", 32), StartedAt: now, CompletedAt: now}, nil
}
func (botRouteTestIMProvider) ListHistoryHeadersV1(context.Context, permissionbundle.Key, string, *string, uint64) (permissionbundle.IMHistoryHeadersPage, error) {
	return permissionbundle.IMHistoryHeadersPage{}, nil
}
func (botRouteTestIMProvider) IssueHistoryDisclosureV1(context.Context, permissionbundle.Key, string, string, []string) ([]permissionbundle.IMDisclosureItem, permissionbundle.IMAuthorityCut, error) {
	return nil, permissionbundle.IMAuthorityCut{}, nil
}
func (botRouteTestIMProvider) ValidateIMCut(context.Context, permissionbundle.Key, string, string, string, string) error {
	return nil
}

func TestBotPermissionOptInRouteCompositionMountsPolicyAndFactsWhenValid(t *testing.T) {
	pin := permissionbundle.CatalogPin{Version: "synthetic-v1", Digest: strings.Repeat("b", 64)}
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	session := conn.NewSession(nil)

	// Unconfigured context leaves Bot permission routes unmounted (404).
	unconfiguredCtx := config.NewContext(config.New())
	baUnconfigured := &BotAPI{
		ctx:           unconfiguredCtx,
		rateLimiters:  newBotRateLimiters(unconfiguredCtx.GetConfig()),
		searchHandler: messages_search.Shared(unconfiguredCtx),
		Log:           log.NewTLog("bot-unconfigured-test"),
	}
	rUnconfigured := wkhttp.New()
	baUnconfigured.Route(rUnconfigured)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/bot/permission-bundle/v1?space_id=space"},
		{http.MethodGet, "/v1/bot/permission-bundle/v1/catalog?space_id=space"},
		{http.MethodGet, "/v1/bot/permission-bundle/v1/catalog/file?space_id=space"},
		{http.MethodPost, "/v1/bot/permission-facts/v1?space_id=space"},
		{http.MethodPost, "/v1/bot/permission-facts/v1/bind-delivery?space_id=space"},
	} {
		rec := httptest.NewRecorder()
		rUnconfigured.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
		require.Equal(t, http.StatusNotFound, rec.Code, "%s %s", tc.method, tc.path)
	}

	// Plain message sync fact mode fails closed at opt-in.
	badCtx := config.NewContext(config.New())
	err = permissionbundle.InstallOptIn(badCtx, permissionbundle.InstallConfig{
		Enabled:          true,
		Session:          session,
		Origin:           "https://synthetic.invalid",
		Pin:              pin,
		Releases:         botRouteTestReleases{pin: pin},
		AuthorityMode:    permissionbundle.AuthorityModeExternalFencedV1,
		AuthorityDriver:  botRouteTestAuthorityDriver{digest: pin.Digest},
		FactProviderMode: "im_sync_channel_message",
	})
	require.ErrorIs(t, err, permissionbundle.ErrUnavailable)
	require.Nil(t, permissionbundle.RuntimeFor(badCtx))

	// Valid opt-in with FencedAuthority and NativeIMFactProvider mounts both policy and fact routes.
	validCtx := config.NewContext(config.New())
	err = permissionbundle.InstallOptIn(validCtx, permissionbundle.InstallConfig{
		Enabled:          true,
		Session:          session,
		Origin:           "https://synthetic.invalid",
		Pin:              pin,
		Releases:         botRouteTestReleases{pin: pin},
		AuthorityMode:    permissionbundle.AuthorityModeExternalFencedV1,
		AuthorityDriver:  botRouteTestAuthorityDriver{digest: pin.Digest},
		FactProviderMode: permissionbundle.NativeFactProviderModeIMV1,
		IMFactProvider:   botRouteTestIMProvider{},
	})
	require.NoError(t, err)
	rt := permissionbundle.RuntimeFor(validCtx)
	require.NotNil(t, rt)
	require.True(t, rt.HasFacts())

	baValid := &BotAPI{
		ctx:              validCtx,
		permissionBundle: rt.Store(),
		rateLimiters:     newBotRateLimiters(validCtx.GetConfig()),
		searchHandler:    messages_search.Shared(validCtx),
		Log:              log.NewTLog("bot-optin-test"),
	}
	rValid := wkhttp.New()
	baValid.Route(rValid)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/bot/permission-bundle/v1?space_id=space"},
		{http.MethodGet, "/v1/bot/permission-bundle/v1/catalog?space_id=space"},
		{http.MethodGet, "/v1/bot/permission-bundle/v1/catalog/file?space_id=space"},
		{http.MethodPost, "/v1/bot/permission-facts/v1?space_id=space"},
	} {
		rec := httptest.NewRecorder()
		rValid.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
		// Mounted routes enter authBot (400 error envelope on missing token) instead of 404.
		require.NotEqual(t, http.StatusNotFound, rec.Code, "%s %s", tc.method, tc.path)
	}
}

func TestBotPermissionOptInRouteCompositionWithRealServerMessageFactHooks(t *testing.T) {
	pin := permissionbundle.CatalogPin{Version: "synthetic-v1", Digest: strings.Repeat("b", 64)}
	db, _, err := sqlmock.New()
	require.NoError(t, err)
	defer db.Close()
	conn := &dbr.Connection{DB: db, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	session := conn.NewSession(nil)

	// 1. Missing authority still fails closed even when real ServerMessageFactHooks are supplied.
	noAuthCtx := config.NewContext(config.New())
	err = permissionbundle.InstallOptIn(noAuthCtx, permissionbundle.InstallConfig{
		Enabled:                true,
		Session:                session,
		Origin:                 "https://synthetic.invalid",
		Pin:                    pin,
		Releases:               botRouteTestReleases{pin: pin},
		FactProviderMode:       permissionbundle.NativeFactProviderModeServerStoreV1,
		ServerMessageHooks:     message.NewServerMessageFactHooks(noAuthCtx),
		ServerStoreIncarnation: strings.Repeat("5", 32),
		ServerPermitKey:        []byte(strings.Repeat("k", 32)),
	})
	require.ErrorIs(t, err, permissionbundle.ErrUnavailable)
	require.Nil(t, permissionbundle.RuntimeFor(noAuthCtx))

	// 2. Missing hooks/incarnation/permit key in octo_server_message_store_v1 mode fails closed.
	missingHooksCtx := config.NewContext(config.New())
	err = permissionbundle.InstallOptIn(missingHooksCtx, permissionbundle.InstallConfig{
		Enabled:                true,
		Session:                session,
		Origin:                 "https://synthetic.invalid",
		Pin:                    pin,
		Releases:               botRouteTestReleases{pin: pin},
		AuthorityMode:          permissionbundle.AuthorityModeExternalFencedV1,
		AuthorityDriver:        botRouteTestAuthorityDriver{digest: pin.Digest},
		FactProviderMode:       permissionbundle.NativeFactProviderModeServerStoreV1,
		ServerStoreIncarnation: strings.Repeat("5", 32),
		ServerPermitKey:        []byte(strings.Repeat("k", 32)),
	})
	require.ErrorIs(t, err, permissionbundle.ErrUnavailable)
	require.Nil(t, permissionbundle.RuntimeFor(missingHooksCtx))

	// 3. Valid opt-in with real message.NewServerMessageFactHooks composes ServerMessageFactProvider
	// and mounts policy, facts, and bind-delivery routes.
	validCtx := config.NewContext(config.New())
	err = permissionbundle.InstallOptIn(validCtx, permissionbundle.InstallConfig{
		Enabled:                true,
		Session:                session,
		Origin:                 "https://synthetic.invalid",
		Pin:                    pin,
		Releases:               botRouteTestReleases{pin: pin},
		AuthorityMode:          permissionbundle.AuthorityModeExternalFencedV1,
		AuthorityDriver:        botRouteTestAuthorityDriver{digest: pin.Digest},
		FactProviderMode:       permissionbundle.NativeFactProviderModeServerStoreV1,
		ServerMessageHooks:     message.NewServerMessageFactHooks(validCtx),
		ServerStoreIncarnation: strings.Repeat("5", 32),
		ServerPermitKey:        []byte(strings.Repeat("k", 32)),
	})
	require.NoError(t, err)
	rt := permissionbundle.RuntimeFor(validCtx)
	require.NotNil(t, rt)
	require.True(t, rt.HasFacts())
	require.True(t, rt.HasDeliveryBinding())

	baValid := &BotAPI{
		ctx:              validCtx,
		permissionBundle: rt.Store(),
		rateLimiters:     newBotRateLimiters(validCtx.GetConfig()),
		searchHandler:    messages_search.Shared(validCtx),
		Log:              log.NewTLog("bot-server-store-optin-test"),
	}
	rValid := wkhttp.New()
	baValid.Route(rValid)
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1/bot/permission-bundle/v1?space_id=space"},
		{http.MethodGet, "/v1/bot/permission-bundle/v1/catalog?space_id=space"},
		{http.MethodGet, "/v1/bot/permission-bundle/v1/catalog/file?space_id=space"},
		{http.MethodPost, "/v1/bot/permission-facts/v1?space_id=space"},
		{http.MethodPost, "/v1/bot/permission-facts/v1/bind-delivery?space_id=space"},
	} {
		rec := httptest.NewRecorder()
		rValid.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
		require.NotEqual(t, http.StatusNotFound, rec.Code, "%s %s", tc.method, tc.path)
	}
}
