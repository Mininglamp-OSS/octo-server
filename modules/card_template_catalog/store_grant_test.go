package card_template_catalog

// PR-C Slice 2 RED (D2/D4): the grant model's pure invariants — canonical
// identity shape, the fixed permission matrix, and the exact-over-global /
// tombstone resolution order — are locked by unit tests before any SQL runs.
// The CAS/transaction behavior itself is proven by the MySQL integration
// suite (store_grant_mysql_integration_test.go).

import (
	"errors"
	"testing"
)

func validBotIdentity() GrantIdentity {
	return GrantIdentity{
		TemplateID:    "docs.access-request",
		PrincipalType: GrantPrincipalBot,
		PrincipalID:   "bot-1",
		ScopeSpaceID:  "space-1",
	}
}

func TestValidateGrantIdentityCanonicalShapes(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*GrantIdentity)
		wantErr bool
	}{
		{name: "bot exact scope", mutate: func(g *GrantIdentity) {}},
		{name: "bot global scope", mutate: func(g *GrantIdentity) { g.ScopeSpaceID = "" }},
		{name: "internal producer global", mutate: func(g *GrantIdentity) {
			g.PrincipalType = GrantPrincipalInternalProducer
			g.PrincipalID = "docs-notify"
			g.ScopeSpaceID = ""
		}},
		{name: "space canonical", mutate: func(g *GrantIdentity) {
			g.PrincipalType = GrantPrincipalSpace
			g.PrincipalID = "space-1"
			g.ScopeSpaceID = ""
		}},
		{name: "space with exact scope is non-canonical", mutate: func(g *GrantIdentity) {
			g.PrincipalType = GrantPrincipalSpace
			g.PrincipalID = "space-1"
			g.ScopeSpaceID = "space-1"
		}, wantErr: true},
		{name: "space scoped to another space", mutate: func(g *GrantIdentity) {
			g.PrincipalType = GrantPrincipalSpace
			g.PrincipalID = "space-1"
			g.ScopeSpaceID = "space-2"
		}, wantErr: true},
		{name: "system is not grantable", mutate: func(g *GrantIdentity) {
			g.PrincipalType = "system"
		}, wantErr: true},
		{name: "unknown principal type", mutate: func(g *GrantIdentity) {
			g.PrincipalType = "user"
		}, wantErr: true},
		{name: "empty principal id", mutate: func(g *GrantIdentity) {
			g.PrincipalID = ""
		}, wantErr: true},
		{name: "untrimmed principal id", mutate: func(g *GrantIdentity) {
			g.PrincipalID = " bot-1"
		}, wantErr: true},
		{name: "untrimmed scope", mutate: func(g *GrantIdentity) {
			g.ScopeSpaceID = "space-1 "
		}, wantErr: true},
		{name: "empty template id", mutate: func(g *GrantIdentity) {
			g.TemplateID = ""
		}, wantErr: true},
		{name: "oversized principal id", mutate: func(g *GrantIdentity) {
			raw := make([]byte, 129)
			for i := range raw {
				raw[i] = 'a'
			}
			g.PrincipalID = string(raw)
		}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity := validBotIdentity()
			tc.mutate(&identity)
			err := validateGrantIdentity(identity)
			if tc.wantErr && err == nil {
				t.Fatalf("non-canonical identity accepted: %+v", identity)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("canonical identity rejected: %v", err)
			}
			if tc.wantErr && !errors.Is(err, ErrGrantRequestInvalid) {
				t.Fatalf("error = %v, want ErrGrantRequestInvalid", err)
			}
		})
	}
}

func TestValidateGrantPermissionsMatrix(t *testing.T) {
	cases := []struct {
		name          string
		principalType GrantPrincipalType
		permissions   GrantPermissions
		wantErr       bool
	}{
		{name: "bot discover only", principalType: GrantPrincipalBot,
			permissions: GrantPermissions{Discover: true}},
		{name: "bot full", principalType: GrantPrincipalBot,
			permissions: GrantPermissions{Discover: true, Send: true, Edit: true}},
		{name: "producer send+discover", principalType: GrantPrincipalInternalProducer,
			permissions: GrantPermissions{Discover: true, Send: true}},
		{name: "space discover only", principalType: GrantPrincipalSpace,
			permissions: GrantPermissions{Discover: true}},
		{name: "no permission at all", principalType: GrantPrincipalBot,
			permissions: GrantPermissions{}, wantErr: true},
		{name: "send without discover is a dark grant", principalType: GrantPrincipalBot,
			permissions: GrantPermissions{Send: true}, wantErr: true},
		{name: "edit without discover is a dark grant", principalType: GrantPrincipalInternalProducer,
			permissions: GrantPermissions{Edit: true}, wantErr: true},
		{name: "space can never send", principalType: GrantPrincipalSpace,
			permissions: GrantPermissions{Discover: true, Send: true}, wantErr: true},
		{name: "space can never edit", principalType: GrantPrincipalSpace,
			permissions: GrantPermissions{Discover: true, Edit: true}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateGrantPermissions(tc.principalType, tc.permissions)
			if tc.wantErr && err == nil {
				t.Fatal("invalid permission shape accepted")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("valid permission shape rejected: %v", err)
			}
		})
	}
}

func TestGrantPermissionsLabel(t *testing.T) {
	if got := grantPermissionsLabel(GrantPermissions{}); got != "" {
		t.Fatalf("empty label = %q", got)
	}
	if got := grantPermissionsLabel(GrantPermissions{Discover: true}); got != "discover" {
		t.Fatalf("discover label = %q", got)
	}
	if got := grantPermissionsLabel(GrantPermissions{Discover: true, Send: true, Edit: true}); got != "discover,send,edit" {
		t.Fatalf("full label = %q", got)
	}
}

// resolveGrantRows is the single precedence rule shared by every consumer
// (Slice 3's one-snapshot authorizer included): an exact row — active OR
// tombstone — completely shadows the global row; permissions never union.
func TestResolveGrantRowsPrecedence(t *testing.T) {
	active := func(scope string, perms GrantPermissions) *GrantRecord {
		identity := validBotIdentity()
		identity.ScopeSpaceID = scope
		return &GrantRecord{Identity: identity, Status: GrantStatusActive, Permissions: perms, Revision: 3}
	}
	revoked := func(scope string) *GrantRecord {
		identity := validBotIdentity()
		identity.ScopeSpaceID = scope
		return &GrantRecord{Identity: identity, Status: GrantStatusRevoked, Revision: 4}
	}
	cases := []struct {
		name          string
		exact, global *GrantRecord
		wantAllowed   GrantPermissions
		wantSource    string
	}{
		{name: "no rows denies", wantAllowed: GrantPermissions{}, wantSource: ""},
		{name: "global only", global: active("", GrantPermissions{Discover: true, Send: true}),
			wantAllowed: GrantPermissions{Discover: true, Send: true}, wantSource: "global"},
		{name: "exact overrides wider global", exact: active("space-1", GrantPermissions{Discover: true}),
			global:      active("", GrantPermissions{Discover: true, Send: true, Edit: true}),
			wantAllowed: GrantPermissions{Discover: true}, wantSource: "exact"},
		{name: "exact tombstone blocks active global", exact: revoked("space-1"),
			global:      active("", GrantPermissions{Discover: true, Send: true}),
			wantAllowed: GrantPermissions{}, wantSource: "exact"},
		{name: "revoked global denies", global: revoked(""),
			wantAllowed: GrantPermissions{}, wantSource: "global"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := resolveGrantRows(tc.exact, tc.global)
			if decision.Allowed != tc.wantAllowed {
				t.Fatalf("allowed = %+v, want %+v", decision.Allowed, tc.wantAllowed)
			}
			if decision.ScopeSource != tc.wantSource {
				t.Fatalf("scope source = %q, want %q", decision.ScopeSource, tc.wantSource)
			}
		})
	}
}

func TestValidateUpsertGrantRequestBounds(t *testing.T) {
	valid := UpsertGrantRequest{
		Identity:    validBotIdentity(),
		Permissions: GrantPermissions{Discover: true},
		ActorUID:    "admin", Reason: "pilot", ChangeTicket: "CHG-1",
	}
	if err := validateUpsertGrantRequest(valid); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	missingReason := valid
	missingReason.Reason = " "
	if err := validateUpsertGrantRequest(missingReason); !errors.Is(err, ErrGrantRequestInvalid) {
		t.Fatalf("blank reason error = %v", err)
	}
	missingActor := valid
	missingActor.ActorUID = ""
	if err := validateUpsertGrantRequest(missingActor); !errors.Is(err, ErrGrantRequestInvalid) {
		t.Fatalf("missing actor error = %v", err)
	}
	darkGrant := valid
	darkGrant.Permissions = GrantPermissions{Send: true}
	if err := validateUpsertGrantRequest(darkGrant); !errors.Is(err, ErrGrantRequestInvalid) {
		t.Fatalf("dark grant error = %v", err)
	}
}
