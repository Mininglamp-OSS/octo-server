package card_template_catalog

// PR-C D4 evidence at the SQL boundary: the resolver issues its reads inside
// one transaction, in the right shape, and reduces the two candidate grant rows
// through the same precedence function the manager control plane uses.
//
// sqlmock is strict about ordering, so "begin, these queries, commit" is an
// assertion about the transaction itself — a future change that moves the grant
// read outside the tx, or drops the tx entirely, fails here.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const (
	authActivationPattern = `SELECT active_version, status, revision`
	authArtifactPattern   = `SELECT c.source, a.owner, a.visibility`
	authGrantPattern      = `SELECT scope_space_id, status, can_discover, can_send, can_edit, revision`
	authPrincipalPattern  = `SELECT template_id, scope_space_id, status`
	// The advertised set resolves every candidate's pointer and artifact in one
	// statement; the per-template pair above is the single-template path.
	authAdvertisePattern = `SELECT act.template_id, act.active_version, act.status, act.revision`
	authBatchGrantPrefix = `SELECT template_id, scope_space_id, status,
        can_discover, can_send, can_edit, revision
    FROM card_template_grant`
)

// advertiseRows are the columns of the batched activation+artifact read.
func advertiseRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"template_id", "active_version", "status", "revision",
		"source", "owner", "visibility", "engine_contract",
		"protocol", "contract_version", "content_sha256", "blocked",
	})
}

func dynamicAdvertiseRow(rows *sqlmock.Rows, id, version string, revision int) *sqlmock.Rows {
	return rows.AddRow(id, version, string(activationStatusActive), revision,
		claimSourceDynamic, "docs", cardtmpl.CatalogVisibilityPrivate,
		cardtmpl.JSONTemplateEngineV1, cardtmpl.Protocol, "1.0.0", strings.Repeat("a", 64), false)
}

func dynamicArtifactRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"source", "owner", "visibility", "engine_contract",
		"protocol", "contract_version", "content_sha256", "blocked",
	}).AddRow(claimSourceDynamic, "docs", cardtmpl.CatalogVisibilityPrivate,
		cardtmpl.JSONTemplateEngineV1, cardtmpl.Protocol, "1.0.0", strings.Repeat("a", 64), false)
}

func grantScopeRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"scope_space_id", "status", "can_discover", "can_send", "can_edit", "revision",
	})
}

func botAuthPrincipal() cardtmpl.CatalogPrincipal {
	return cardtmpl.CatalogPrincipal{
		Kind: cardtmpl.CatalogPrincipalBot, ID: "bot-1", SpaceID: "space-1",
	}
}

func TestStoreLoadAuthorizationReadsPointerArtifactAndGrantInOneTransaction(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(authActivationPattern)).
		WithArgs("test.runtime-card").
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}).
			AddRow("1.1.0", string(activationStatusActive), 6))
	mock.ExpectQuery(regexp.QuoteMeta(authArtifactPattern)).
		WithArgs("test.runtime-card", "1.1.0").
		WillReturnRows(dynamicArtifactRows())
	mock.ExpectQuery(regexp.QuoteMeta(authGrantPattern)).
		WithArgs("test.runtime-card", string(GrantPrincipalBot), "bot-1", "space-1").
		WillReturnRows(grantScopeRows().AddRow("space-1", string(GrantStatusActive), true, true, false, 4))
	mock.ExpectCommit()

	auth, err := store.LoadAuthorization(context.Background(), cardtmpl.RuntimeAuthorizationQuery{
		ID: "test.runtime-card", Principal: botAuthPrincipal(),
	})
	if err != nil {
		t.Fatalf("LoadAuthorization: %v", err)
	}
	if auth.Version != "1.1.0" || auth.Activation.Revision != 6 {
		t.Fatalf("authorization pointer = %+v", auth)
	}
	if auth.Artifact.Owner != "docs" || auth.Artifact.Source != cardtmpl.RuntimeSourceDynamic || auth.Artifact.Blocked {
		t.Fatalf("authorization artifact = %+v", auth.Artifact)
	}
	want := cardtmpl.RuntimeGrant{
		Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Revision: 4, Discover: true, Send: true,
	}
	if auth.Grant != want {
		t.Fatalf("authorization grant = %+v, want %+v", auth.Grant, want)
	}
	assertMockExpectations(t, mock)
}

// A pinned read is what historical edit and action context perform. It must not
// issue the activation query at all: reading the pointer there is how a version
// switch would silently retarget an already-sent card.
func TestStoreLoadAuthorizationPinnedVersionSkipsActivationRead(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(authArtifactPattern)).
		WithArgs("test.runtime-card", "1.0.0").
		WillReturnRows(dynamicArtifactRows())
	mock.ExpectQuery(regexp.QuoteMeta(authGrantPattern)).
		WithArgs("test.runtime-card", string(GrantPrincipalInternalProducer), "docs-notify", "space-1").
		WillReturnRows(grantScopeRows().AddRow("", string(GrantStatusActive), true, false, true, 2))
	mock.ExpectCommit()

	auth, err := store.LoadAuthorization(context.Background(), cardtmpl.RuntimeAuthorizationQuery{
		ID: "test.runtime-card", Version: "1.0.0",
		Principal: cardtmpl.CatalogPrincipal{
			Kind: cardtmpl.CatalogPrincipalInternalProducer, ID: "docs-notify", SpaceID: "space-1",
		},
	})
	if err != nil {
		t.Fatalf("LoadAuthorization: %v", err)
	}
	if auth.Activation.Exists || auth.Version != "1.0.0" {
		t.Fatalf("pinned authorization = %+v, want no pointer state", auth)
	}
	if auth.Grant.Scope != cardtmpl.RuntimeGrantScopeGlobal || !auth.Grant.Edit || auth.Grant.Send {
		t.Fatalf("pinned grant = %+v", auth.Grant)
	}
	assertMockExpectations(t, mock)
}

// The precedence rule is exercised through the real SQL path so that the row
// shapes the query returns — not just the in-memory records — are known to
// reduce correctly. Especially: an exact tombstone wins over an active global
// row, and permissions are never unioned across scopes.
func TestStoreLoadAuthorizationAppliesExactOverGlobalPrecedence(t *testing.T) {
	type row struct {
		scope    string
		status   GrantStatus
		discover bool
		send     bool
		edit     bool
		revision uint64
	}
	for _, test := range []struct {
		name string
		rows []row
		want cardtmpl.RuntimeGrant
	}{
		{
			name: "no rows is no grant",
			want: cardtmpl.RuntimeGrant{},
		},
		{
			name: "global row applies when there is no exact row",
			rows: []row{{"", GrantStatusActive, true, true, false, 2}},
			want: cardtmpl.RuntimeGrant{
				Found: true, Scope: cardtmpl.RuntimeGrantScopeGlobal, Revision: 2, Discover: true, Send: true},
		},
		{
			name: "exact row overrides the global row entirely",
			rows: []row{
				{"", GrantStatusActive, true, true, true, 2},
				{"space-1", GrantStatusActive, true, false, false, 5},
			},
			want: cardtmpl.RuntimeGrant{
				Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Revision: 5, Discover: true},
		},
		{
			name: "exact tombstone shadows an active global grant",
			rows: []row{
				{"", GrantStatusActive, true, true, true, 2},
				{"space-1", GrantStatusRevoked, false, false, false, 9},
			},
			want: cardtmpl.RuntimeGrant{Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Revision: 9},
		},
		{
			name: "global tombstone denies without consulting anything else",
			rows: []row{{"", GrantStatusRevoked, false, false, false, 3}},
			want: cardtmpl.RuntimeGrant{Found: true, Scope: cardtmpl.RuntimeGrantScopeGlobal, Revision: 3},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, mock, closeDB := newMockStore(t)
			defer closeDB()

			grantRows := grantScopeRows()
			for _, r := range test.rows {
				grantRows.AddRow(r.scope, string(r.status), r.discover, r.send, r.edit, r.revision)
			}
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta(authArtifactPattern)).
				WillReturnRows(dynamicArtifactRows())
			mock.ExpectQuery(regexp.QuoteMeta(authGrantPattern)).WillReturnRows(grantRows)
			mock.ExpectCommit()

			auth, err := store.LoadAuthorization(context.Background(), cardtmpl.RuntimeAuthorizationQuery{
				ID: "test.runtime-card", Version: "1.0.0", Principal: botAuthPrincipal(),
			})
			if err != nil {
				t.Fatalf("LoadAuthorization: %v", err)
			}
			if auth.Grant != test.want {
				t.Fatalf("grant = %+v, want %+v", auth.Grant, test.want)
			}
			assertMockExpectations(t, mock)
		})
	}
}

// `system` and blank principals are control-plane identities, not producers.
// The resolver must not even ask the grant table about them — an empty-string
// principal_id in a query is one schema slip away from matching a real row.
func TestStoreLoadAuthorizationSkipsGrantLookupForUngrantablePrincipals(t *testing.T) {
	for _, principal := range []cardtmpl.CatalogPrincipal{
		{Kind: cardtmpl.CatalogPrincipalSystem, ID: "startup", SpaceID: "space-1"},
		{Kind: cardtmpl.CatalogPrincipalBot, ID: "   ", SpaceID: "space-1"},
		{},
	} {
		store, mock, closeDB := newMockStore(t)
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta(authArtifactPattern)).WillReturnRows(dynamicArtifactRows())
		mock.ExpectCommit()

		auth, err := store.LoadAuthorization(context.Background(), cardtmpl.RuntimeAuthorizationQuery{
			ID: "test.runtime-card", Version: "1.0.0", Principal: principal,
		})
		if err != nil {
			t.Fatalf("LoadAuthorization(%+v): %v", principal, err)
		}
		if auth.Grant.Found {
			t.Fatalf("principal %+v produced a grant %+v", principal, auth.Grant)
		}
		assertMockExpectations(t, mock)
		closeDB()
	}
}

// A template with no activation row resolves to no version, so the artifact
// read is skipped — the caller falls back to the frozen Registry.
func TestStoreLoadAuthorizationWithoutActivationSkipsArtifactRead(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(authActivationPattern)).
		WillReturnRows(sqlmock.NewRows([]string{"active_version", "status", "revision"}))
	mock.ExpectQuery(regexp.QuoteMeta(authGrantPattern)).WillReturnRows(grantScopeRows())
	mock.ExpectCommit()

	auth, err := store.LoadAuthorization(context.Background(), cardtmpl.RuntimeAuthorizationQuery{
		ID: "test.runtime-card", Principal: botAuthPrincipal(),
	})
	if err != nil {
		t.Fatalf("LoadAuthorization: %v", err)
	}
	if auth.Version != "" || auth.Activation.Exists {
		t.Fatalf("authorization = %+v, want no activation", auth)
	}
	assertMockExpectations(t, mock)
}

func TestStoreLoadAuthorizationRequiresATemplate(t *testing.T) {
	store, _, closeDB := newMockStore(t)
	defer closeDB()

	if _, err := store.LoadAuthorization(context.Background(), cardtmpl.RuntimeAuthorizationQuery{
		Principal: botAuthPrincipal(),
	}); !errors.Is(err, cardtmpl.ErrTemplateUnknown) {
		t.Fatalf("blank template error = %v, want ErrTemplateUnknown", err)
	}
}

// The advertised set reads every candidate from the same snapshot, and drops a
// template whose pointer is absent or disabled instead of failing the whole
// listing — one broken template must not blank a Bot's capability manifest.
func TestStoreListAuthorizedTemplatesSkipsUnadvertisableTemplates(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(authPrincipalPattern)).
		// One row past the budget so a truncated read is detectable.
		WithArgs(string(GrantPrincipalBot), "bot-1", "space-1", maxAuthorizedTemplates*2+1).
		WillReturnRows(sqlmock.NewRows([]string{
			"template_id", "scope_space_id", "status",
			"can_discover", "can_send", "can_edit", "revision",
		}).
			AddRow("test.active", "space-1", string(GrantStatusActive), true, true, false, 3).
			AddRow("test.disabled", "", string(GrantStatusActive), true, true, false, 1).
			AddRow("test.revoked", "space-1", string(GrantStatusRevoked), false, false, false, 8))

	// One batched state read for both surviving candidates, sorted by template
	// ID. The revoked one is not among them at all — a tombstone is decided
	// before touching activation. test.disabled has no row in the result
	// because the inner join on active_version drops a disabled pointer, which
	// is the same outcome the per-template loop produced by discarding it.
	mock.ExpectQuery(regexp.QuoteMeta(authAdvertisePattern)).
		WithArgs("test.active", "test.disabled").
		WillReturnRows(dynamicAdvertiseRow(advertiseRows(), "test.active", "2.0.0", 4))
	mock.ExpectCommit()

	templates, err := store.ListAuthorizedTemplates(context.Background(), botAuthPrincipal(), 0)
	if err != nil {
		t.Fatalf("ListAuthorizedTemplates: %v", err)
	}
	if len(templates) != 1 || templates[0].ID != "test.active" {
		t.Fatalf("templates = %+v, want only test.active", templates)
	}
	if templates[0].Authorization.Version != "2.0.0" || !templates[0].Authorization.Grant.Send {
		t.Fatalf("advertised authorization = %+v", templates[0].Authorization)
	}
	assertMockExpectations(t, mock)
}

func TestStoreListAuthorizedTemplatesIgnoresUngrantablePrincipals(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	templates, err := store.ListAuthorizedTemplates(context.Background(),
		cardtmpl.CatalogPrincipal{Kind: cardtmpl.CatalogPrincipalSystem, ID: "startup"}, 10)
	if err != nil || templates != nil {
		t.Fatalf("templates = %+v err = %v, want no lookup at all", templates, err)
	}
	assertMockExpectations(t, mock)
}

// The batch resolver's whole reason to exist is the shape of its transaction:
// one snapshot, one activation/artifact statement, one grant statement, however
// many templates were asked about. sqlmock is the only place that shape can be
// asserted — a real-MySQL test proves the answers agree but says nothing about
// how many round trips produced them.
func TestStoreLoadAuthorizationsUsesTwoStatementsForManyTemplates(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(authAdvertisePattern)).
		WithArgs("test.a", "test.b", "test.c").
		WillReturnRows(dynamicAdvertiseRow(advertiseRows(), "test.a", "1.0.0", 2))
	mock.ExpectQuery(regexp.QuoteMeta(authBatchGrantPrefix)).
		WithArgs(string(GrantPrincipalBot), "bot-1", "space-1", "test.a", "test.b", "test.c").
		WillReturnRows(sqlmock.NewRows([]string{
			"template_id", "scope_space_id", "status",
			"can_discover", "can_send", "can_edit", "revision",
		}).AddRow("test.a", "space-1", string(GrantStatusActive), true, true, false, 5))
	mock.ExpectCommit()

	got, err := store.LoadAuthorizations(context.Background(),
		[]cardtmpl.ID{"test.a", "test.b", "test.c"}, botAuthPrincipal())
	if err != nil {
		t.Fatalf("LoadAuthorizations: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("answered %d of 3 templates: %+v", len(got), got)
	}
	if got["test.a"].Version != "1.0.0" || !got["test.a"].Grant.Send {
		t.Fatalf("test.a = %+v", got["test.a"])
	}
	// A template with no activation row still gets an entry, because "no
	// dynamic version" is an answer the caller acts on (it keeps its static
	// behaviour) rather than an absence it has to guess about.
	for _, id := range []cardtmpl.ID{"test.b", "test.c"} {
		if entry, ok := got[id]; !ok || entry.Version != "" || entry.Grant.Found {
			t.Fatalf("%s = %+v (present=%v)", id, entry, ok)
		}
	}
	assertMockExpectations(t, mock)
}

// A duplicated ID must not widen the IN list, and a blank one is a caller bug
// that has to surface rather than quietly resolving to nothing.
func TestStoreLoadAuthorizationsRejectsBlankIDsAndDeduplicates(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	if _, err := store.LoadAuthorizations(context.Background(),
		[]cardtmpl.ID{"test.a", "  "}, botAuthPrincipal()); !errors.Is(err, cardtmpl.ErrTemplateUnknown) {
		t.Fatalf("blank id error = %v, want ErrTemplateUnknown", err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(authAdvertisePattern)).
		WithArgs("test.a").
		WillReturnRows(advertiseRows())
	mock.ExpectQuery(regexp.QuoteMeta(authBatchGrantPrefix)).
		WithArgs(string(GrantPrincipalBot), "bot-1", "space-1", "test.a").
		WillReturnRows(sqlmock.NewRows([]string{
			"template_id", "scope_space_id", "status",
			"can_discover", "can_send", "can_edit", "revision",
		}))
	mock.ExpectCommit()
	got, err := store.LoadAuthorizations(context.Background(),
		[]cardtmpl.ID{"test.a", "test.a"}, botAuthPrincipal())
	if err != nil {
		t.Fatalf("LoadAuthorizations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("deduplicated result = %+v", got)
	}
	assertMockExpectations(t, mock)
}

// An ungrantable principal issues no grant query, exactly as the single-template
// path does — `system` and blank identities are control-plane concepts, and a
// lookup on them could accidentally match an empty-string row.
func TestStoreLoadAuthorizationsSkipsTheGrantQueryForUngrantablePrincipals(t *testing.T) {
	store, mock, closeDB := newMockStore(t)
	defer closeDB()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(authAdvertisePattern)).
		WithArgs("test.a").
		WillReturnRows(dynamicAdvertiseRow(advertiseRows(), "test.a", "1.0.0", 2))
	mock.ExpectCommit()

	got, err := store.LoadAuthorizations(context.Background(), []cardtmpl.ID{"test.a"},
		cardtmpl.CatalogPrincipal{Kind: cardtmpl.CatalogPrincipalSystem, ID: "startup"})
	if err != nil {
		t.Fatalf("LoadAuthorizations: %v", err)
	}
	if got["test.a"].Grant.Found {
		t.Fatalf("a system principal was issued a grant: %+v", got["test.a"])
	}
	assertMockExpectations(t, mock)
}
