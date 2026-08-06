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

// The batch resolver exists only to save round trips, so the property that
// matters is that it saves nothing else: for every template it must return
// exactly what the per-ID resolver would have. A divergence here would be
// invisible — the manifest and the send it advertises would simply disagree,
// and only for whichever templates happened to differ.
func TestLoadAuthorizationsAgreesWithThePerTemplateResolverRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	principal := authorizationIntegrationPrincipal()

	// One template of each shape the resolver distinguishes.
	seedAuthorizationTarget(t, db, "1.0.0") // active pointer, grant added below
	seedDiscoveryVersion(t, db, "test.batch-no-activation", "1.0.0",
		cardtmpl.CatalogVisibilityPrivate, false) // claimed, never activated
	seedDiscoveryVersion(t, db, "test.batch-blocked", "1.0.0",
		cardtmpl.CatalogVisibilityPublic, true)
	seedDiscoveryActivation(t, db, "test.batch-blocked", "1.0.0")
	seedDiscoveryVersion(t, db, "test.batch-ungranted", "1.0.0",
		cardtmpl.CatalogVisibilityPrivate, false)
	seedDiscoveryActivation(t, db, "test.batch-ungranted", "1.0.0")
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity:    authorizationIntegrationIdentity(),
		Permissions: GrantPermissions{Discover: true, Send: true},
		ActorUID:    "admin-1", Reason: "pilot", ChangeTicket: "CHG-B1",
	}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	ids := []cardtmpl.ID{
		authorizationIntegrationTemplateID,
		"test.batch-no-activation",
		"test.batch-blocked",
		"test.batch-ungranted",
		"test.batch-does-not-exist",
	}
	batch, err := catalogStore.LoadAuthorizations(ctx, ids, principal)
	if err != nil {
		t.Fatalf("LoadAuthorizations: %v", err)
	}
	if len(batch) != len(ids) {
		t.Fatalf("batch answered %d of %d templates", len(batch), len(ids))
	}
	for _, id := range ids {
		single, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
			ID: id, Principal: principal,
		})
		if err != nil {
			t.Fatalf("per-template LoadAuthorization(%s): %v", id, err)
		}
		if batch[id] != single {
			t.Fatalf("%s: batch = %+v, per-template = %+v", id, batch[id], single)
		}
	}

	// The interesting rows, spelled out so a future change that makes both
	// paths wrong in the same way still fails.
	if got := batch[authorizationIntegrationTemplateID]; got.Version != "1.0.0" || !got.Grant.Send {
		t.Fatalf("granted active template = %+v", got)
	}
	if got := batch["test.batch-no-activation"]; got.Version != "" || got.Grant.Found {
		t.Fatalf("unactivated template = %+v, want a zero version and no grant", got)
	}
	if got := batch["test.batch-blocked"]; got.Version != "1.0.0" || !got.Artifact.Blocked {
		t.Fatalf("blocked template = %+v", got)
	}
	if got := batch["test.batch-ungranted"]; got.Version != "1.0.0" || got.Grant.Found {
		t.Fatalf("ungranted active template = %+v", got)
	}
	if got := batch["test.batch-does-not-exist"]; got.Version != "" || got.Grant.Found {
		t.Fatalf("unknown template = %+v", got)
	}

	// A revoke has to move both paths at the same instant.
	if _, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: authorizationIntegrationIdentity(), ExpectedRevision: 1,
		ActorUID: "admin-1", Reason: "offboard", ChangeTicket: "CHG-B2",
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	after, err := catalogStore.LoadAuthorizations(ctx, ids, principal)
	if err != nil {
		t.Fatalf("post-revoke LoadAuthorizations: %v", err)
	}
	single, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: authorizationIntegrationTemplateID, Principal: principal,
	})
	if err != nil {
		t.Fatalf("post-revoke LoadAuthorization: %v", err)
	}
	if after[authorizationIntegrationTemplateID] != single {
		t.Fatalf("post-revoke batch = %+v, per-template = %+v",
			after[authorizationIntegrationTemplateID], single)
	}
	if after[authorizationIntegrationTemplateID].Grant.Send {
		t.Fatal("the batch still reports send after a revoke")
	}
}

// An exact tombstone must shadow a live global grant in the batch reducer too —
// the batch reads both scopes per template for exactly this reason, and a
// reducer that took "first row wins" would silently re-grant every offboarded
// principal that still holds a global row.
func TestLoadAuthorizationsAppliesExactOverGlobalPrecedenceRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	identity := authorizationIntegrationIdentity()
	seedAuthorizationTarget(t, db, "1.0.0")

	global := identity
	global.ScopeSpaceID = ""
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: global, Permissions: GrantPermissions{Discover: true, Send: true},
		ActorUID: "admin-1", Reason: "fleet", ChangeTicket: "CHG-B3",
	}); err != nil {
		t.Fatalf("seed global grant: %v", err)
	}
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: GrantPermissions{Discover: true, Send: true},
		ActorUID: "admin-1", Reason: "exact", ChangeTicket: "CHG-B4",
	}); err != nil {
		t.Fatalf("seed exact grant: %v", err)
	}
	if _, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: identity, ExpectedRevision: 1,
		ActorUID: "admin-1", Reason: "offboard", ChangeTicket: "CHG-B5",
	}); err != nil {
		t.Fatalf("revoke exact grant: %v", err)
	}

	ids := []cardtmpl.ID{authorizationIntegrationTemplateID}
	batch, err := catalogStore.LoadAuthorizations(ctx, ids, authorizationIntegrationPrincipal())
	if err != nil {
		t.Fatalf("LoadAuthorizations: %v", err)
	}
	single, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: authorizationIntegrationTemplateID, Principal: authorizationIntegrationPrincipal(),
	})
	if err != nil {
		t.Fatalf("LoadAuthorization: %v", err)
	}
	if batch[authorizationIntegrationTemplateID] != single {
		t.Fatalf("batch = %+v, per-template = %+v", batch[authorizationIntegrationTemplateID], single)
	}
	if batch[authorizationIntegrationTemplateID].Grant.Send {
		t.Fatal("an exact tombstone failed to shadow the global grant in the batch reducer")
	}
}

// One disabled template must not cost the batch its whole reason to exist. The
// per-ID resolver signals a disabled pointer with an error because an error is
// its only channel; a batch that did the same would fail every other template
// in the call and send the caller back to one-ID-at-a-time reads for as long as
// an operator left that template disabled.
func TestLoadAuthorizationsKeepsAnsweringAroundADisabledTemplateRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	seedAuthorizationTarget(t, db, "1.0.0")
	seedDiscoveryVersion(t, db, "test.disabled-one", "1.0.0", cardtmpl.CatalogVisibilityPrivate, false)
	seedDiscoveryActivation(t, db, "test.disabled-one", "1.0.0")
	if _, err := db.Exec(`UPDATE card_template_activation
        SET status = 'disabled', active_version = NULL WHERE template_id = ?`,
		"test.disabled-one"); err != nil {
		t.Fatalf("disable pointer: %v", err)
	}

	ids := []cardtmpl.ID{authorizationIntegrationTemplateID, "test.disabled-one"}
	batch, err := catalogStore.LoadAuthorizations(ctx, ids, authorizationIntegrationPrincipal())
	if err != nil {
		t.Fatalf("a disabled template failed the whole batch: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("batch answered %d of 2: %+v", len(batch), batch)
	}
	// The healthy template still resolves normally.
	if batch[authorizationIntegrationTemplateID].Version != "1.0.0" {
		t.Fatalf("healthy template = %+v", batch[authorizationIntegrationTemplateID])
	}
	// The disabled one reports its status rather than a version, so a caller
	// cannot mistake it for "no dynamic version, keep your static behaviour".
	disabled := batch["test.disabled-one"]
	if disabled.Version != "" {
		t.Fatalf("disabled template offered version %q", disabled.Version)
	}
	if disabled.Activation.Status != cardtmpl.RuntimeActivationDisabled {
		t.Fatalf("disabled template = %+v, want the disabled status carried through", disabled)
	}
	// The per-ID form still reports it as an error — that difference is the
	// documented contract, not a drift.
	if _, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: "test.disabled-one", Principal: authorizationIntegrationPrincipal(),
	}); !errors.Is(err, cardtmpl.ErrRuntimeCatalogDisabled) {
		t.Fatalf("per-ID disabled error = %v, want ErrRuntimeCatalogDisabled", err)
	}
}
