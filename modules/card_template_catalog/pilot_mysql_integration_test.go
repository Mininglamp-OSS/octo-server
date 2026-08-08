package card_template_catalog

// PR-C D7 — the first non-production dynamic pilot, end to end.
//
// This drives the whole loop against real MySQL: version preflight → static
// baseline → publish → grant → activate → discover → authorize a send →
// same-version edit → rollback → revoke. Each step asserts the property the
// slice it came from is responsible for, so a regression anywhere in PR-C
// surfaces here as a specific failed step rather than as "the pilot broke".
//
// Two things this deliberately does NOT do:
//
//   - It never touches production. The bundle fixture is publish *input*; it is
//     not registered in DefaultRegistry, not embedded at startup, and its
//     presence in Git is not activation. The gates stay closed in production
//     regardless of what this test proves.
//   - It never reuses an identity. The version below is a dated prerelease, and
//     the preflight step below refuses to run if the catalog has ever seen it —
//     a version claim is permanent, so publishing over one would either be
//     rejected as an immutability violation or, worse, quietly resolve to
//     someone else's artifact.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const (
	// pilotTemplateID is a dedicated ID, NOT the live docs.access-request.
	//
	// An earlier revision reused the production ID so the shadow behaviour would
	// be observable on a real card. That is unsafe: the pilot bundle declares its
	// own `additionalProperties:false` contract, so activating it for the live ID
	// in any catalog where the notify docs producer runs would make every real
	// access-request card fail preflight with a 400 and zero delivery. The
	// shadow path is proven below by activating over a *seeded* static baseline
	// under this dedicated ID, which exercises the same code with none of that
	// blast radius.
	pilotTemplateID = cardtmpl.ID("docs.pilot-access-request")
	// pilotVersion is a dated prerelease that has never been claimed. It is not
	// a default and must not be edited in place: if the preflight below ever
	// finds it claimed, pick a new reviewed SemVer rather than overwriting.
	pilotVersion = "0.4.0-pilot.20260805"
	// pilotStaticBaseline is the known-good static exact the pilot activates
	// first, so the later rollback has a genuine previously-active target
	// instead of inferring one from the Registry default.
	pilotStaticBaseline = "0.3.0"
	// pilotProductionTemplateID is the live ID the pilot must never claim. The
	// test asserts this rather than leaving it to a reader of the constant.
	pilotProductionTemplateID = cardtmpl.ID("docs.access-request")

	pilotProducerID = "docs-notify"
	pilotSpaceID    = "space-cardtmpl-pilot"
	pilotActor      = "admin-pilot"

	// pilotCatalogDSNEnv names the shared non-production catalog the version
	// preflight interrogates. The per-test database cannot answer the question:
	// it is created empty moments before the check runs.
	pilotCatalogDSNEnv = "OCTO_PILOT_CATALOG_DSN"
)

func pilotBundlePath() string {
	return filepath.Join("testdata", "pilot",
		string(pilotTemplateID)+"@"+pilotVersion, "bundle.json")
}

func loadPilotBundle(t *testing.T) cardtmpl.Bundle {
	t.Helper()
	raw, err := os.ReadFile(pilotBundlePath())
	if err != nil {
		t.Fatalf("read pilot bundle: %v", err)
	}
	var bundle cardtmpl.Bundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatalf("decode pilot bundle: %v", err)
	}
	return bundle
}

// requirePilotVersionUnclaimed is the preflight gate.
//
// A version claim is permanent and global to a catalog, so "has anyone ever
// claimed this exact version" must be answered before the fixture is published
// anywhere. The subtlety is *which* catalog to ask.
//
// An earlier revision asked the per-test database — which
// newCatalogStoreIntegrationDB drops and recreates immediately beforehand, so
// the answer was unconditionally "no" and the gate could never fire. It read
// like a safety check and was theatre.
//
// The real question is about whatever shared non-production catalog the
// operator is publishing into, which this process only knows about through
// OCTO_PILOT_CATALOG_DSN. When that is set the gate queries it for real; when
// it is not, the gate says so out loud and skips rather than passing silently,
// because "no shared catalog configured" and "the version is free" are
// different answers and only one of them is evidence.
func requirePilotVersionUnclaimed(t *testing.T, id cardtmpl.ID, version string) {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv(pilotCatalogDSNEnv))
	if dsn == "" {
		t.Logf("pilot version preflight SKIPPED: %s is unset, so no shared catalog was "+
			"checked for %s@%s. Set it to the non-production catalog DSN before publishing "+
			"this fixture anywhere shared.", pilotCatalogDSNEnv, id, version)
		return
	}
	shared, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("pilot version preflight: open %s: %v", pilotCatalogDSNEnv, err)
	}
	defer func() { _ = shared.Close() }()

	var claimed int
	err = shared.QueryRow(`SELECT COUNT(*) FROM card_template_version_claim
        WHERE template_id = ? AND version = ?`, string(id), version).Scan(&claimed)
	if err != nil {
		t.Fatalf("pilot version preflight: query shared catalog: %v", err)
	}
	if claimed != 0 {
		t.Fatalf("pilot version preflight: %s@%s is already claimed in the shared catalog. "+
			"A claim is permanent — do not overwrite or reuse it. Choose a new reviewed "+
			"prerelease version, rename the testdata/pilot directory to match, and update "+
			"pilotVersion.", id, version)
	}
}

// seedPilotStaticBaseline records the known-good static exact as active before
// any dynamic activation, per D7. Without it the later rollback would have no
// previously-active target and the pilot would prove a rollback that operators
// could not actually perform.
func seedPilotStaticBaseline(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO card_template_version_claim
        (template_id, version, source) VALUES (?, ?, 'static')`,
		string(pilotTemplateID), pilotStaticBaseline); err != nil {
		t.Fatalf("seed static baseline claim: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO card_template_activation
        (template_id, active_version, status, revision, updated_by, reason, change_ticket)
        VALUES (?, ?, 'active', 1, ?, 'pilot static baseline', 'PILOT-0')`,
		string(pilotTemplateID), pilotStaticBaseline, pilotActor); err != nil {
		t.Fatalf("seed static baseline activation: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO card_template_audit
        (actor_uid, operation, template_id, version, resulting_version,
         previous_revision, resulting_revision, result, reason, change_ticket)
        VALUES (?, 'activate', ?, ?, ?, 0, 1, 'ok', 'pilot static baseline', 'PILOT-0')`,
		pilotActor, string(pilotTemplateID), pilotStaticBaseline, pilotStaticBaseline); err != nil {
		t.Fatalf("seed static baseline audit: %v", err)
	}
}

func pilotProducerPrincipal() cardtmpl.CatalogPrincipal {
	return cardtmpl.CatalogPrincipal{
		Kind: cardtmpl.CatalogPrincipalInternalProducer, ID: pilotProducerID, SpaceID: pilotSpaceID,
	}
}

func pilotGrantIdentity() GrantIdentity {
	return GrantIdentity{
		TemplateID:    pilotTemplateID,
		PrincipalType: GrantPrincipalInternalProducer,
		PrincipalID:   pilotProducerID,
		ScopeSpaceID:  pilotSpaceID,
	}
}

// The pilot bundle must compile with no database at all. Keeping this separate
// means a malformed fixture is caught on every developer machine, not only
// where MySQL happens to be running.
func TestPilotBundleCompilesAndProjectsSafely(t *testing.T) {
	bundle := loadPilotBundle(t)
	artifact, err := cardtmpl.CompileJSONArtifact(context.Background(), bundle, cardtmpl.DefaultCompileLimits())
	if err != nil {
		t.Fatalf("compile pilot bundle: %v", err)
	}
	if artifact.Meta.ID != pilotTemplateID || artifact.Meta.Version != pilotVersion {
		t.Fatalf("pilot identity = %s@%s", artifact.Meta.ID, artifact.Meta.Version)
	}
	if artifact.Owner != "docs" || artifact.Visibility != cardtmpl.CatalogVisibilityPrivate {
		t.Fatalf("pilot owner/visibility = %s/%s, want docs/private", artifact.Owner, artifact.Visibility)
	}
	contract := artifact.Meta.ActionContract
	if contract == nil || contract.Owner != "docs" || contract.ActionType != "access_request.decision" {
		t.Fatalf("pilot action contract = %+v, want the existing docs RouteSpec", contract)
	}
	export := artifact.Meta.Export()
	if export == nil {
		t.Fatal("pilot bundle produced no export projection")
	}
	// D5 requires the pilot to prove the export path end to end, which means at
	// least one allowlisted synthetic sample — and nothing beyond the allowlist.
	if len(export.Samples) != 1 {
		t.Fatalf("pilot exported samples = %v, want exactly the allowlisted one", export.Samples)
	}
	if _, allowlisted := export.Samples["pending"]; !allowlisted {
		t.Fatalf("pilot exported the wrong sample set: %v", export.Samples)
	}
	// The fixture declares its own strict data contract, so claiming the live
	// template ID would make activating it reject every real access-request
	// card. Assert the separation rather than trusting a comment on a constant.
	if artifact.Meta.ID == pilotProductionTemplateID {
		t.Fatalf("the pilot fixture claims the production template ID %s", pilotProductionTemplateID)
	}
}

// TestPilotDynamicCatalogEndToEndRealMySQL is the D7 loop.
func TestPilotDynamicCatalogEndToEndRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// 1. Preflight: the exact version has never been claimed.
	requirePilotVersionUnclaimed(t, pilotTemplateID, pilotVersion)

	// 2. Baseline: the known-good static exact is active and audited, so the
	//    rollback in step 9 has a real previously-active target.
	seedPilotStaticBaseline(t, db)

	// 3. Publish the prerelease dynamic artifact. Publishing is not activation
	//    and not authorization — the next two steps prove both.
	artifact, err := cardtmpl.CompileJSONArtifact(ctx, loadPilotBundle(t), cardtmpl.DefaultCompileLimits())
	if err != nil {
		t.Fatalf("compile pilot bundle: %v", err)
	}
	published, err := catalogStore.Publish(ctx, PublishRequest{
		Artifact: artifact, ActorUID: pilotActor,
		Reason: "non-production pilot", ChangeTicket: "PILOT-1",
	})
	if err != nil {
		t.Fatalf("publish pilot artifact: %v", err)
	}
	if !published.Created || published.Blocked {
		t.Fatalf("publish result = %+v, want a fresh unblocked claim", published)
	}

	// 4. Published but neither active nor granted: a new send still resolves to
	//    the static baseline, and the producer holds nothing.
	auth, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: pilotTemplateID, Principal: pilotProducerPrincipal(),
	})
	if err != nil {
		t.Fatalf("post-publish authorization: %v", err)
	}
	if auth.Version != pilotStaticBaseline || auth.Grant.Found {
		t.Fatalf("publish implied activation or a grant: %+v", auth)
	}

	// 5. Grant send+edit+discover to the producer in the pilot Space.
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity:    pilotGrantIdentity(),
		Permissions: GrantPermissions{Discover: true, Send: true, Edit: true},
		ActorUID:    pilotActor, Reason: "pilot producer", ChangeTicket: "PILOT-2",
	}); err != nil {
		t.Fatalf("grant pilot producer: %v", err)
	}
	// A grant is still not an activation: new sends keep resolving to static.
	auth, err = catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: pilotTemplateID, Principal: pilotProducerPrincipal(),
	})
	if err != nil {
		t.Fatalf("post-grant authorization: %v", err)
	}
	if auth.Version != pilotStaticBaseline {
		t.Fatalf("grant moved the activation pointer to %q", auth.Version)
	}
	if !auth.Grant.Allows(cardtmpl.CatalogPurposeNewSend) {
		t.Fatalf("granted producer cannot send: %+v", auth.Grant)
	}

	// 6. Activate the dynamic exact with the baseline's current revision.
	activated, err := catalogStore.Activate(ctx, ActivationRequest{
		TemplateID: pilotTemplateID, TargetVersion: pilotVersion, ExpectedRevision: 1,
		ActorUID: pilotActor, Reason: "pilot activation", ChangeTicket: "PILOT-3",
		targetReceipt: stateTargetReceipt{meta: cardtmpl.RuntimeArtifactMeta{
			ID: pilotTemplateID, Version: pilotVersion, Source: cardtmpl.RuntimeSourceDynamic,
			Engine: cardtmpl.JSONTemplateEngineV1, Hash: artifact.Hash, Owner: artifact.Owner,
			Visibility: artifact.Visibility, Protocol: cardtmpl.Protocol,
			ContractVersion: artifact.ContractVersion,
		}},
	})
	if err != nil {
		t.Fatalf("activate pilot version: %v", err)
	}
	if activated.ActiveVersion != pilotVersion {
		t.Fatalf("active version = %q, want the pilot prerelease", activated.ActiveVersion)
	}

	// 7. The dynamic version now shadows the static baseline for new sends, and
	//    the whole decision comes from one snapshot.
	auth, err = catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: pilotTemplateID, Principal: pilotProducerPrincipal(),
	})
	if err != nil {
		t.Fatalf("post-activate authorization: %v", err)
	}
	if auth.Version != pilotVersion || auth.Artifact.Source != cardtmpl.RuntimeSourceDynamic ||
		auth.Artifact.Blocked || !auth.Grant.Allows(cardtmpl.CatalogPurposeNewSend) {
		t.Fatalf("dynamic new-send authorization = %+v", auth)
	}

	// 8. Discovery: the Space sees the private pilot only with a space grant,
	//    and a Space without one gets the same answer as for a template that
	//    does not exist.
	if _, err := catalogStore.LoadDiscoverable(ctx, pilotSpaceID, pilotTemplateID, pilotVersion); !errors.Is(
		err, ErrDiscoveryNotVisible) {
		t.Fatalf("private pilot was discoverable without a space grant: %v", err)
	}
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: GrantIdentity{
			TemplateID: pilotTemplateID, PrincipalType: GrantPrincipalSpace, PrincipalID: pilotSpaceID,
		},
		Permissions: GrantPermissions{Discover: true},
		ActorUID:    pilotActor, Reason: "pilot discovery", ChangeTicket: "PILOT-4",
	}); err != nil {
		t.Fatalf("grant space discovery: %v", err)
	}
	row, err := catalogStore.LoadDiscoverable(ctx, pilotSpaceID, pilotTemplateID, pilotVersion)
	if err != nil {
		t.Fatalf("granted space cannot discover the pilot: %v", err)
	}
	if row.Visibility != cardtmpl.CatalogVisibilityPrivate || !row.ActiveForNewSend ||
		row.ContentSHA256 != artifact.Hash {
		t.Fatalf("discovered row = %+v", row)
	}
	if _, err := catalogStore.LoadDiscoverable(ctx, "space-somewhere-else",
		pilotTemplateID, pilotVersion); !errors.Is(err, ErrDiscoveryNotVisible) {
		t.Fatalf("another Space could see the pilot: %v", err)
	}

	// 9. A historical card pins its stored exact version, so a rollback of the
	//    pointer must not change what an existing card resolves to. Read the
	//    pinned authorization before and after the rollback and compare.
	pinnedBefore, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: pilotTemplateID, Version: pilotVersion, Principal: pilotProducerPrincipal(),
	})
	if err != nil {
		t.Fatalf("pinned authorization before rollback: %v", err)
	}
	if !pinnedBefore.Grant.Allows(cardtmpl.CatalogPurposeHistoricalEdit) ||
		!pinnedBefore.Grant.Allows(cardtmpl.CatalogPurposeActionContext) {
		t.Fatalf("pinned edit/action authorization = %+v", pinnedBefore.Grant)
	}
	if _, err := catalogStore.Rollback(ctx, RollbackRequest{
		TemplateID: pilotTemplateID, TargetVersion: pilotStaticBaseline, ExpectedRevision: activated.Revision,
		ActorUID: pilotActor, Reason: "pilot rollback", ChangeTicket: "PILOT-5",
		targetReceipt: stateTargetReceipt{meta: cardtmpl.RuntimeArtifactMeta{
			ID: pilotTemplateID, Version: pilotStaticBaseline, Source: cardtmpl.RuntimeSourceStatic,
		}},
	}); err != nil {
		t.Fatalf("rollback to the static baseline: %v", err)
	}
	pinnedAfter, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: pilotTemplateID, Version: pilotVersion, Principal: pilotProducerPrincipal(),
	})
	if err != nil {
		t.Fatalf("pinned authorization after rollback: %v", err)
	}
	if pinnedAfter.Version != pilotVersion || !pinnedAfter.Grant.Allows(cardtmpl.CatalogPurposeHistoricalEdit) {
		t.Fatalf("rollback broke historical edit of an already-sent card: %+v", pinnedAfter)
	}
	// New sends, meanwhile, are back on the static baseline.
	auth, err = catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: pilotTemplateID, Principal: pilotProducerPrincipal(),
	})
	if err != nil {
		t.Fatalf("post-rollback authorization: %v", err)
	}
	if auth.Version != pilotStaticBaseline {
		t.Fatalf("post-rollback new-send version = %q, want the static baseline", auth.Version)
	}

	// 10. Revoke: the very next decision is denied, for the pinned historical
	//     path as well as for new sends.
	if _, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: pilotGrantIdentity(), ExpectedRevision: 1,
		ActorUID: pilotActor, Reason: "pilot complete", ChangeTicket: "PILOT-6",
	}); err != nil {
		t.Fatalf("revoke pilot grant: %v", err)
	}
	revoked, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
		ID: pilotTemplateID, Version: pilotVersion, Principal: pilotProducerPrincipal(),
	})
	if err != nil {
		t.Fatalf("post-revoke authorization: %v", err)
	}
	if revoked.Grant.Allows(cardtmpl.CatalogPurposeHistoricalEdit) ||
		revoked.Grant.Allows(cardtmpl.CatalogPurposeNewSend) {
		t.Fatalf("revoked producer still authorized: %+v", revoked.Grant)
	}
	if !revoked.Grant.Found {
		t.Fatal("revoke deleted the row instead of writing a tombstone")
	}
}
