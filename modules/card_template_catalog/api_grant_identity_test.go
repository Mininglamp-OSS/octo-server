package card_template_catalog

// Review P2 (yujiawei): the grant write path accepted principal identities the
// read path can never match.
//
// card_template_grant.principal_id / scope_space_id are utf8mb4_bin and the
// runtime resolver matches them byte-for-byte, while the existence checks the
// write path runs — `space`, `robot`, `app_bot` — are utf8mb4_general_ci. A
// `PUT .../grants/space/SPACE-1` against a real `space-1` therefore passed
// validation, stored `SPACE-1`, returned 200 with a revision, and authorized
// nothing, forever, with nothing anywhere connecting the two facts.
//
// Two tests, because the defect has two halves. The rule is unit-tested here;
// the collation premise that makes it a defect is proven against real MySQL in
// TestGeneralCiExistenceChecksAcceptASpellingBinLookupsRejectRealMySQL, since a
// claim about collation behaviour asserted in Go would be asserting nothing.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

func TestByteExactIdentityRejectsASpellingTheRuntimeCannotMatch(t *testing.T) {
	for _, test := range []struct {
		name             string
		stored, request  string
		wantErr          bool
		wantMentionsWhat bool
	}{
		{name: "identical spelling is accepted", stored: "space-1", request: "space-1"},
		{name: "case difference is rejected", stored: "space-1", request: "SPACE-1",
			wantErr: true, wantMentionsWhat: true},
		{name: "trailing space is rejected", stored: "space-1", request: "space-1 ",
			wantErr: true, wantMentionsWhat: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := byteExactIdentity("space id", test.stored, test.request)
			if test.wantErr != (err != nil) {
				t.Fatalf("byteExactIdentity(%q, %q) = %v, wantErr=%v",
					test.stored, test.request, err, test.wantErr)
			}
			if !test.wantErr {
				return
			}
			if !errors.Is(err, ErrGrantRequestInvalid) {
				t.Fatalf("err = %v, want ErrGrantRequestInvalid so the caller gets a 400", err)
			}
			// The canonical spelling belongs in the message: this route is
			// super-admin only, and "your write did nothing" without saying why
			// is the expensive half of this defect.
			if !strings.Contains(err.Error(), test.stored) {
				t.Fatalf("err = %v, want it to name the stored identity %q", err, test.stored)
			}
		})
	}
}

// The premise, measured rather than asserted. If general_ci did not match
// case-insensitively, the old COUNT(*) existence check would already have
// rejected `SPACE-1` and there would be nothing to fix; if the grant columns
// were not binary, the stored spelling would still match at read time. Both
// halves have to hold for the defect to exist, so both are measured here.
func TestGeneralCiExistenceChecksAcceptASpellingBinLookupsRejectRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)

	if _, err := db.Exec(`CREATE TEMPORARY TABLE collation_probe (
        ci VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci NOT NULL,
        bin VARCHAR(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL)`); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO collation_probe (ci, bin) VALUES ('space-1', 'space-1')`); err != nil {
		t.Fatalf("seed probe row: %v", err)
	}

	count := func(t *testing.T, query string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(query, "SPACE-1").Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("probe %q: %v", query, err)
		}
		return n
	}

	// The write side: an identity table matches a differently-cased spelling.
	if got := count(t, `SELECT COUNT(*) FROM collation_probe WHERE ci = ?`); got != 1 {
		t.Fatalf("utf8mb4_general_ci matched %d rows for SPACE-1, want 1 — the premise of this "+
			"defect is that the existence check accepts the wrong spelling", got)
	}
	// The read side: the grant table does not.
	if got := count(t, `SELECT COUNT(*) FROM collation_probe WHERE bin = ?`); got != 0 {
		t.Fatalf("utf8mb4_bin matched %d rows for SPACE-1, want 0 — if binary lookups were "+
			"case-insensitive the stored spelling would still authorize", got)
	}
}

// TestAnExactTombstoneIsInvisibleToANonCanonicalScopeRealMySQL is the other half
// of P0-1's evidence: the store is correct and the *input* is what has to be
// canonical.
//
// Nothing here is a defect in this package. `scope_space_id` is `utf8mb4_bin`
// and matching it byte-for-byte is exactly right — an exact-scope grant must not
// be found by a scope that is not that scope. What the test measures is the
// consequence for a caller that hands over an un-canonicalised spelling: the
// exact tombstone is invisible, the still-active global row answers in its
// place, and a revoke silently does nothing. `modules/bot_api` is where that is
// prevented (resolveBotGrantSpaceID now returns `space.space_id`); this is the
// measurement that says why it must be.
func TestAnExactTombstoneIsInvisibleToANonCanonicalScopeRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedAuthorizationTarget(t, db, "1.0.0")
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const canonical = "space-a"
	const miscased = "SPACE-A"
	identity := func(scope string) GrantIdentity {
		return GrantIdentity{
			TemplateID:    authorizationIntegrationTemplateID,
			PrincipalType: GrantPrincipalBot,
			PrincipalID:   "bot-tombstone",
			ScopeSpaceID:  scope,
		}
	}

	// A global grant that allows send, and an exact-scope tombstone that must
	// mask it for this Space (invariant 11).
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity(""), Permissions: GrantPermissions{Discover: true, Send: true},
		ActorUID: "admin-1", Reason: "global", ChangeTicket: "CHG-T1",
	}); err != nil {
		t.Fatalf("global grant: %v", err)
	}
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity(canonical), Permissions: GrantPermissions{Discover: true, Send: true},
		ActorUID: "admin-1", Reason: "exact", ChangeTicket: "CHG-T2",
	}); err != nil {
		t.Fatalf("exact grant: %v", err)
	}
	if _, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: identity(canonical), ExpectedRevision: 1,
		ActorUID: "admin-1", Reason: "cut this bot off", ChangeTicket: "CHG-T3",
	}); err != nil {
		t.Fatalf("revoke exact: %v", err)
	}

	ask := func(t *testing.T, scope string) cardtmpl.RuntimeGrant {
		t.Helper()
		auth, err := catalogStore.LoadAuthorization(ctx, cardtmpl.RuntimeAuthorizationQuery{
			ID: authorizationIntegrationTemplateID,
			Principal: cardtmpl.CatalogPrincipal{
				Kind: cardtmpl.CatalogPrincipalBot, ID: "bot-tombstone", SpaceID: scope,
			},
		})
		if err != nil {
			t.Fatalf("LoadAuthorization(scope=%q): %v", scope, err)
		}
		return auth.Grant
	}

	// Canonical spelling: the tombstone is found and masks the global row.
	if grant := ask(t, canonical); grant.Send {
		t.Fatalf("the exact tombstone did not mask the global grant: %+v", grant)
	}

	// Miscased spelling: the tombstone is not found, so the global grant
	// answers and the revoke has done nothing. This is the bypass, and the
	// reason resolveBotGrantSpaceID must never pass a caller's spelling here.
	if grant := ask(t, miscased); !grant.Send {
		t.Fatalf("fixture premise broken: %q already misses the global grant too, so this "+
			"test no longer demonstrates why the Space identity must be canonicalised "+
			"before it reaches the store (grant=%+v)", miscased, grant)
	}
}
