package card_template_catalog

// PR-C Slice 2 (D4) real-MySQL evidence: grant create/update/revoke CAS with
// exactly one concurrent winner, tombstone ABA protection, the same-transaction
// audit invariant, and the version-claim target guard. Shares
// newCatalogStoreIntegrationDB with the PR-A/PR-B suites, so every run also
// exercises the grant migration Up and (via cleanup) Down.

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const grantIntegrationTemplateID = cardtmpl.ID("test.grant-target")

func seedGrantTargetClaim(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO card_template_version_claim
        (template_id, version, source) VALUES (?, '1.0.0', 'dynamic')`,
		string(grantIntegrationTemplateID)); err != nil {
		t.Fatalf("seed grant target claim: %v", err)
	}
}

func grantIntegrationIdentity() GrantIdentity {
	return GrantIdentity{
		TemplateID:    grantIntegrationTemplateID,
		PrincipalType: GrantPrincipalBot,
		PrincipalID:   "bot-integration",
		ScopeSpaceID:  "space-integration",
	}
}

func countGrantAuditRows(t *testing.T, db *sql.DB, operation string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM card_template_audit
        WHERE template_id = ? AND operation = ? AND result = 'ok'`,
		string(grantIntegrationTemplateID), operation).Scan(&count); err != nil {
		t.Fatalf("count %s audit: %v", operation, err)
	}
	return count
}

// PR-C review round 5 (Jerry-Xin, plus the S4 identity item): the grant table's
// CHECK constraints are the last line that holds when a write does not come
// through the Go validator, so every domain the validator enforces has to be
// stated in the schema too.
//
// The two gaps this covers were both reachable by direct SQL against the
// previous schema. can_send/can_edit outside {0,1} on an active row was caught
// only downstream, where GrantPermissions scans into Go bools and the driver
// refuses the value — fail-closed by accident of the scan type rather than by
// the schema. A blank principal_id was accepted outright.
func TestGrantSchemaRejectsWritesTheValidatorWouldRefuseRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedGrantTargetClaim(t, db)

	insert := `INSERT INTO card_template_grant
        (template_id, principal_type, principal_id, scope_space_id, status,
         can_discover, can_send, can_edit, revision, updated_by, reason, change_ticket)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 'operator', 'reason', 'ticket')`
	tmpl := string(grantIntegrationTemplateID)

	for _, test := range []struct {
		name string
		args []any
	}{
		{
			name: "can_send outside the boolean domain",
			args: []any{tmpl, "bot", "bot-send", "", "active", 1, 7, 0},
		},
		{
			name: "can_edit outside the boolean domain",
			args: []any{tmpl, "bot", "bot-edit", "", "active", 1, 0, 2},
		},
		{
			// Redundant with chk_card_template_grant_shape, which already pins
			// can_discover to 1 on an active row and 0 on a tombstone — so this
			// cell passes with or without the new constraint. It is here because
			// the column belongs in the stated domain regardless of which
			// constraint currently enforces it.
			name: "can_discover outside the boolean domain",
			args: []any{tmpl, "bot", "bot-discover", "", "active", 3, 0, 0},
		},
		{
			name: "a grant with no principal",
			args: []any{tmpl, "bot", "", "", "active", 1, 1, 0},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A distinct principal per case, so that a schema which wrongly
			// accepts one row cannot make a later case fail on a duplicate key
			// instead of on its own claim.
			if _, err := db.Exec(insert, test.args...); err == nil {
				t.Fatal("the schema accepted a row the validator would refuse")
			}
		})
	}

	// The canonical shape it must still accept, so the constraints are not
	// merely rejecting everything.
	if _, err := db.Exec(insert, tmpl, "bot", "bot-valid", "", "active", 1, 1, 1); err != nil {
		t.Fatalf("a valid grant was rejected: %v", err)
	}
}

func TestStoreGrantLifecycleRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedGrantTargetClaim(t, db)
	catalogStore := newStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	identity := grantIntegrationIdentity()

	// Target without any claim fails closed before any write.
	orphan := identity
	orphan.TemplateID = "test.never-claimed"
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: orphan, Permissions: GrantPermissions{Discover: true},
		ActorUID: "admin", Reason: "r", ChangeTicket: "t",
	}); !errors.Is(err, ErrGrantTargetUnknown) {
		t.Fatalf("orphan target error = %v, want ErrGrantTargetUnknown", err)
	}

	// Create requires expected_revision=0 and lands revision 1 + audit in the
	// same transaction.
	created, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: GrantPermissions{Discover: true, Send: true},
		ExpectedRevision: 0, ActorUID: "admin", Reason: "pilot", ChangeTicket: "CHG-1",
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	if !created.Created || created.Record.Revision != 1 {
		t.Fatalf("create result = %+v", created)
	}
	if got := countGrantAuditRows(t, db, auditOperationGrant); got != 1 {
		t.Fatalf("grant audit rows = %d, want 1", got)
	}

	// Update with a stale revision loses; with the exact revision wins.
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: GrantPermissions{Discover: true},
		ExpectedRevision: 0, ActorUID: "admin", Reason: "stale", ChangeTicket: "CHG-2",
	}); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("stale create-as-update error = %v, want ErrGrantConflict", err)
	}
	updated, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: GrantPermissions{Discover: true, Send: true, Edit: true},
		ExpectedRevision: 1, ActorUID: "admin", Reason: "widen", ChangeTicket: "CHG-3",
	})
	if err != nil || updated.Created || updated.Record.Revision != 2 {
		t.Fatalf("update result = %+v err=%v", updated, err)
	}

	// Revoke writes a tombstone with zeroed permissions and bumps revision.
	revoked, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: identity, ExpectedRevision: 2,
		ActorUID: "admin", Reason: "offboard", ChangeTicket: "CHG-4",
	})
	if err != nil || revoked.Record.Status != GrantStatusRevoked || revoked.Record.Revision != 3 {
		t.Fatalf("revoke result = %+v err=%v", revoked, err)
	}
	var status string
	var discover, send, edit bool
	if err := db.QueryRow(`SELECT status, can_discover, can_send, can_edit FROM card_template_grant
        WHERE template_id = ? AND principal_type = 'bot' AND principal_id = ? AND scope_space_id = ?`,
		string(identity.TemplateID), identity.PrincipalID, identity.ScopeSpaceID,
	).Scan(&status, &discover, &send, &edit); err != nil {
		t.Fatalf("read tombstone: %v", err)
	}
	if status != string(GrantStatusRevoked) || discover || send || edit {
		t.Fatalf("tombstone row = %s/%v/%v/%v", status, discover, send, edit)
	}

	// Double revoke and revoke of a missing grant are typed failures.
	if _, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: identity, ExpectedRevision: 3,
		ActorUID: "admin", Reason: "again", ChangeTicket: "CHG-5",
	}); !errors.Is(err, ErrGrantAlreadyRevoked) {
		t.Fatalf("double revoke error = %v, want ErrGrantAlreadyRevoked", err)
	}
	missing := identity
	missing.PrincipalID = "bot-ghost"
	if _, err := catalogStore.RevokeGrant(ctx, RevokeGrantRequest{
		Identity: missing, ExpectedRevision: 1,
		ActorUID: "admin", Reason: "noop", ChangeTicket: "CHG-6",
	}); !errors.Is(err, ErrGrantNotFound) {
		t.Fatalf("missing revoke error = %v, want ErrGrantNotFound", err)
	}

	// ABA guard: re-activating the tombstone must carry its CURRENT revision;
	// expected_revision=0 can never disguise a create again.
	if _, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: GrantPermissions{Discover: true},
		ExpectedRevision: 0, ActorUID: "admin", Reason: "aba", ChangeTicket: "CHG-7",
	}); !errors.Is(err, ErrGrantConflict) {
		t.Fatalf("tombstone create-disguise error = %v, want ErrGrantConflict", err)
	}
	reactivated, err := catalogStore.UpsertGrant(ctx, UpsertGrantRequest{
		Identity: identity, Permissions: GrantPermissions{Discover: true},
		ExpectedRevision: 3, ActorUID: "admin", Reason: "re-grant", ChangeTicket: "CHG-8",
	})
	if err != nil || reactivated.Created || reactivated.Record.Revision != 4 ||
		reactivated.Record.Status != GrantStatusActive {
		t.Fatalf("re-activate result = %+v err=%v", reactivated, err)
	}

	// The audit ledger recorded every successful transition with principal
	// identity and permission diffs.
	if got := countGrantAuditRows(t, db, auditOperationGrant); got != 3 {
		t.Fatalf("grant audit rows = %d, want 3", got)
	}
	if got := countGrantAuditRows(t, db, auditOperationRevoke); got != 1 {
		t.Fatalf("revoke audit rows = %d, want 1", got)
	}
	var principalID, previousPerms, resultingPerms string
	if err := db.QueryRow(`SELECT principal_id, previous_permissions, resulting_permissions
        FROM card_template_audit
        WHERE template_id = ? AND operation = 'revoke'
        ORDER BY id DESC LIMIT 1`,
		string(identity.TemplateID)).Scan(&principalID, &previousPerms, &resultingPerms); err != nil {
		t.Fatalf("read revoke audit: %v", err)
	}
	if principalID != identity.PrincipalID || previousPerms != "discover,send,edit" || resultingPerms != "" {
		t.Fatalf("revoke audit = %s / %q -> %q", principalID, previousPerms, resultingPerms)
	}

	// Bounded list returns both shapes for the manager summary.
	records, truncated, err := catalogStore.ListGrants(ctx, identity.TemplateID, 10)
	if err != nil || truncated || len(records) != 1 {
		t.Fatalf("list grants = %+v truncated=%v err=%v", records, truncated, err)
	}
}

func TestStoreGrantConcurrentCASHasOneWinnerRealMySQL(t *testing.T) {
	db := newCatalogStoreIntegrationDB(t)
	seedGrantTargetClaim(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	identity := grantIntegrationIdentity()
	stores := []*store{newStore(db), newStore(db)}

	// Concurrent create: exactly one revision-1 winner.
	start := make(chan struct{})
	results := make([]error, len(stores))
	var wg sync.WaitGroup
	for i, contender := range stores {
		wg.Add(1)
		go func(i int, s *store) {
			defer wg.Done()
			<-start
			_, err := s.UpsertGrant(ctx, UpsertGrantRequest{
				Identity: identity, Permissions: GrantPermissions{Discover: true},
				ExpectedRevision: 0, ActorUID: "admin", Reason: "race", ChangeTicket: "CHG-R1",
			})
			results[i] = err
		}(i, contender)
	}
	close(start)
	wg.Wait()
	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrGrantConflict) {
			t.Fatalf("concurrent create error = %v, want ErrGrantConflict", err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent create winners = %d, want 1", winners)
	}

	// Concurrent revoke against the same revision: one winner, one conflict.
	start = make(chan struct{})
	for i, contender := range stores {
		wg.Add(1)
		go func(i int, s *store) {
			defer wg.Done()
			<-start
			_, err := s.RevokeGrant(ctx, RevokeGrantRequest{
				Identity: identity, ExpectedRevision: 1,
				ActorUID: "admin", Reason: "race", ChangeTicket: "CHG-R2",
			})
			results[i] = err
		}(i, contender)
	}
	close(start)
	wg.Wait()
	winners = 0
	for _, err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrGrantConflict) && !errors.Is(err, ErrGrantAlreadyRevoked) {
			t.Fatalf("concurrent revoke error = %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent revoke winners = %d, want 1", winners)
	}

	// State and audit agree: revision 2, one grant + one revoke audit row.
	var revision uint64
	if err := db.QueryRow(`SELECT revision FROM card_template_grant
        WHERE template_id = ? AND principal_type = 'bot' AND principal_id = ? AND scope_space_id = ?`,
		string(identity.TemplateID), identity.PrincipalID, identity.ScopeSpaceID).Scan(&revision); err != nil {
		t.Fatalf("read final revision: %v", err)
	}
	if revision != 2 {
		t.Fatalf("final revision = %d, want 2", revision)
	}
	if grants, revokes := countGrantAuditRows(t, db, auditOperationGrant),
		countGrantAuditRows(t, db, auditOperationRevoke); grants != 1 || revokes != 1 {
		t.Fatalf("audit rows grant/revoke = %d/%d, want 1/1", grants, revokes)
	}
}
