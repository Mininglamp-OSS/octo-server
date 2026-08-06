package card_template_catalog

// PR-C D4 real-MySQL evidence for the authorization linearization point.
//
// The property under test is not "a revoked grant is denied" — that is obvious
// from the rows. It is *when* the denial takes effect: a snapshot opened before
// the revoke commits keeps its answer for its whole lifetime, and a snapshot
// opened afterwards can never see the pre-revoke state. sqlmock cannot express
// that; only a real InnoDB REPEATABLE READ transaction can.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const authorizationIntegrationTemplateID = cardtmpl.ID("test.authorization-target")

func seedAuthorizationTarget(t *testing.T, db *sql.DB, version string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO card_template_version_claim
        (template_id, version, source) VALUES (?, ?, 'dynamic')`,
		string(authorizationIntegrationTemplateID), version); err != nil {
		t.Fatalf("seed authorization claim: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO card_template_artifact
        (template_id, version, owner, visibility, engine_contract, protocol,
         contract_version, canonical_bundle, content_sha256, created_by)
        VALUES (?, ?, 'docs', ?, ?, ?, '1.0.0', ?, ?, 'admin-1')`,
		string(authorizationIntegrationTemplateID), version,
		cardtmpl.CatalogVisibilityPrivate, cardtmpl.JSONTemplateEngineV1, cardtmpl.Protocol,
		`{"canonical":true}`, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("seed authorization artifact: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO card_template_activation
        (template_id, active_version, status, revision, updated_by, reason, change_ticket)
        VALUES (?, ?, 'active', 1, 'admin-1', 'pilot', 'CHG-A1')`,
		string(authorizationIntegrationTemplateID), version); err != nil {
		t.Fatalf("seed authorization activation: %v", err)
	}
}

func authorizationIntegrationIdentity() GrantIdentity {
	return GrantIdentity{
		TemplateID:    authorizationIntegrationTemplateID,
		PrincipalType: GrantPrincipalBot,
		PrincipalID:   "bot-authorization",
		ScopeSpaceID:  "space-authorization",
	}
}

func authorizationIntegrationPrincipal() cardtmpl.CatalogPrincipal {
	return cardtmpl.CatalogPrincipal{
		Kind:    cardtmpl.CatalogPrincipalBot,
		ID:      authorizationIntegrationIdentity().PrincipalID,
		SpaceID: authorizationIntegrationIdentity().ScopeSpaceID,
	}
}

func TestStoreLoadAuthorizationSnapshotIsTheLinearizationPointRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedAuthorizationTarget(t, db, "1.0.0")
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	identity := authorizationIntegrationIdentity()
	query := cardtmpl.RuntimeAuthorizationQuery{
		ID: authorizationIntegrationTemplateID, Principal: authorizationIntegrationPrincipal(),
	}

	// Ungranted: the pointer and artifact resolve, but nothing is allowed.
	auth, err := catalogStore.LoadAuthorization(ctx, query)
	if err != nil {
		t.Fatalf("ungranted LoadAuthorization: %v", err)
	}
	if auth.Version != "1.0.0" || auth.Artifact.Owner != "docs" || auth.Grant.Found {
		t.Fatalf("ungranted authorization = %+v", auth)
	}

	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: GrantPermissions{Discover: true, Send: true},
		ActorUID: "admin-1", Reason: "pilot", ChangeTicket: "CHG-A2",
	}); err != nil {
		t.Fatalf("grant send: %v", err)
	}
	auth, err = catalogStore.LoadAuthorization(ctx, query)
	if err != nil {
		t.Fatalf("granted LoadAuthorization: %v", err)
	}
	if !auth.Grant.Allows(cardtmpl.CatalogPurposeNewSend) ||
		auth.Grant.Scope != cardtmpl.RuntimeGrantScopeExact || auth.Grant.Revision != 1 {
		t.Fatalf("granted authorization grant = %+v", auth.Grant)
	}
	if auth.Grant.Allows(cardtmpl.CatalogPurposeHistoricalEdit) {
		t.Fatal("a send grant leaked edit permission")
	}

	// Hold a snapshot open, revoke from another connection, and confirm the
	// open snapshot is unaffected while a fresh one is already denied. This is
	// the whole contract: in-flight requests finish, new ones see the revoke.
	held, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatalf("open held snapshot: %v", err)
	}
	defer func() { _ = held.Rollback() }()
	beforeRevoke, err := loadGrantDecision(ctx, held, authorizationIntegrationTemplateID,
		authorizationIntegrationPrincipal())
	if err != nil {
		t.Fatalf("held snapshot first read: %v", err)
	}
	if !beforeRevoke.Send {
		t.Fatalf("held snapshot first read = %+v, want send", beforeRevoke)
	}

	if _, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: identity, ExpectedRevision: 1,
		ActorUID: "admin-1", Reason: "offboard", ChangeTicket: "CHG-A3",
	}); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}

	stillHeld, err := loadGrantDecision(ctx, held, authorizationIntegrationTemplateID,
		authorizationIntegrationPrincipal())
	if err != nil {
		t.Fatalf("held snapshot second read: %v", err)
	}
	if stillHeld != beforeRevoke {
		t.Fatalf("held snapshot changed under a concurrent revoke: %+v -> %+v", beforeRevoke, stillHeld)
	}

	afterRevoke, err := catalogStore.LoadAuthorization(ctx, query)
	if err != nil {
		t.Fatalf("post-revoke LoadAuthorization: %v", err)
	}
	if afterRevoke.Grant.Allows(cardtmpl.CatalogPurposeNewSend) {
		t.Fatalf("post-revoke grant still allows send: %+v", afterRevoke.Grant)
	}
	if !afterRevoke.Grant.Found || afterRevoke.Grant.Revision != 2 {
		t.Fatalf("post-revoke grant = %+v, want the tombstone at revision 2", afterRevoke.Grant)
	}
}

// An exact tombstone must shadow an active global grant against real rows and
// real collations, not only in the reducer's unit test.
func TestStoreLoadAuthorizationExactTombstoneShadowsGlobalRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedAuthorizationTarget(t, db, "1.0.0")
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	global := authorizationIntegrationIdentity()
	global.ScopeSpaceID = ""
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: global, Permissions: GrantPermissions{Discover: true, Send: true, Edit: true},
		ActorUID: "admin-1", Reason: "fleet-wide", ChangeTicket: "CHG-G1",
	}); err != nil {
		t.Fatalf("global grant: %v", err)
	}
	query := cardtmpl.RuntimeAuthorizationQuery{
		ID: authorizationIntegrationTemplateID, Principal: authorizationIntegrationPrincipal(),
	}
	auth, err := catalogStore.LoadAuthorization(ctx, query)
	if err != nil {
		t.Fatalf("global-only LoadAuthorization: %v", err)
	}
	if auth.Grant.Scope != cardtmpl.RuntimeGrantScopeGlobal || !auth.Grant.Send {
		t.Fatalf("global-only grant = %+v", auth.Grant)
	}

	exact := authorizationIntegrationIdentity()
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: exact, Permissions: GrantPermissions{Discover: true},
		ActorUID: "admin-1", Reason: "narrow this space", ChangeTicket: "CHG-G2",
	}); err != nil {
		t.Fatalf("exact grant: %v", err)
	}
	if _, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: exact, ExpectedRevision: 1,
		ActorUID: "admin-1", Reason: "block this space", ChangeTicket: "CHG-G3",
	}); err != nil {
		t.Fatalf("exact revoke: %v", err)
	}

	auth, err = catalogStore.LoadAuthorization(ctx, query)
	if err != nil {
		t.Fatalf("shadowed LoadAuthorization: %v", err)
	}
	// The global row is still active and still grants send/edit. It must not
	// be reachable: permissions are never unioned across scopes.
	if auth.Grant.Scope != cardtmpl.RuntimeGrantScopeExact ||
		auth.Grant.Discover || auth.Grant.Send || auth.Grant.Edit {
		t.Fatalf("shadowed grant = %+v, want the exact tombstone to win", auth.Grant)
	}

	// A principal in a different Space still falls through to the global row:
	// the tombstone shadows one Space, not the whole grant.
	elsewhere := authorizationIntegrationPrincipal()
	elsewhere.SpaceID = "space-elsewhere"
	auth, err = catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: authorizationIntegrationTemplateID, Principal: elsewhere,
	})
	if err != nil {
		t.Fatalf("other-space LoadAuthorization: %v", err)
	}
	if auth.Grant.Scope != cardtmpl.RuntimeGrantScopeGlobal || !auth.Grant.Send {
		t.Fatalf("other-space grant = %+v, want the global row", auth.Grant)
	}
}

// A blocked artifact and a missing claim are both reported through the
// snapshot rather than by a second read, so the authorizer upstream can fail
// closed on the same data it authorized against.
func TestStoreLoadAuthorizationReportsBlockAndUnknownFromTheSnapshotRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedAuthorizationTarget(t, db, "1.0.0")
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := db.Exec(`UPDATE card_template_artifact SET blocked_at = NOW(), blocked_by = 'admin-1'
        WHERE template_id = ? AND version = '1.0.0'`, string(authorizationIntegrationTemplateID)); err != nil {
		t.Fatalf("block artifact: %v", err)
	}
	auth, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: authorizationIntegrationTemplateID, Principal: authorizationIntegrationPrincipal(),
	})
	if err != nil {
		t.Fatalf("blocked LoadAuthorization: %v", err)
	}
	if !auth.Artifact.Blocked {
		t.Fatalf("blocked artifact = %+v", auth.Artifact)
	}

	if _, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: authorizationIntegrationTemplateID, Version: "9.9.9",
		Principal: authorizationIntegrationPrincipal(),
	}); !errors.Is(err, cardtmpl.ErrTemplateUnknown) {
		t.Fatalf("unknown pinned version error = %v, want ErrTemplateUnknown", err)
	}
}

// runtimeAuthorizationSource is the seam every consumer outside this module
// reads grants through — the Bot capability manifest chiefly. Two things about
// it are load-bearing and neither is visible from the store alone: it must
// answer from the same store the runtime resolves against (a second reader
// would be a second grant truth), and NewSendEnabled must come from the
// deployment gate rather than from the data, so a dark deployment answers
// "no new-send" without acquiring the DB dependency it did not have before
// PR-C.
func TestRuntimeAuthorizationSourceReadsTheStoreAndTheGateRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedAuthorizationTarget(t, db, "1.0.0")
	catalogStore := newStore(db)
	identity := authorizationIntegrationIdentity()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: GrantPermissions{Discover: true, Send: true},
		ActorUID: "admin-1", Reason: "pilot", ChangeTicket: "CHG-A4",
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	dark := &runtimeAuthorizationSource{store: catalogStore, gates: runtimeCatalogGates{}}
	if dark.NewSendEnabled() {
		t.Fatal("a gated-off deployment reported new-send as enabled")
	}
	live := &runtimeAuthorizationSource{
		store: catalogStore,
		gates: runtimeCatalogGates{controlEnabled: true, newSendEnabled: true},
	}
	if !live.NewSendEnabled() {
		t.Fatal("an enabled deployment reported new-send as dark")
	}

	// The grant the source reports must be the grant the store holds, down to
	// the permission bits — this is the read the capability manifest trusts.
	auth, err := live.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: authorizationIntegrationTemplateID, Principal: authorizationIntegrationPrincipal(),
	})
	if err != nil {
		t.Fatalf("source LoadAuthorization: %v", err)
	}
	if !auth.Grant.Found || !auth.Grant.Send {
		t.Fatalf("source grant = %+v", auth.Grant)
	}

	advertised, err := live.ListAuthorizedTemplates(ctx, authorizationIntegrationPrincipal(), 10)
	if err != nil {
		t.Fatalf("source ListAuthorizedTemplates: %v", err)
	}
	if len(advertised) != 1 || advertised[0].ID != authorizationIntegrationTemplateID {
		t.Fatalf("source advertised %+v", advertised)
	}
}
