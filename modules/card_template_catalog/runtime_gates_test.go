package card_template_catalog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

func TestRuntimeCatalogGatesFailClosedOnMissingAndInvalidValues(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]string
		want   runtimeCatalogGates
		warns  int
	}{
		{name: "missing", values: map[string]string{}, want: runtimeCatalogGates{}, warns: 2},
		{name: "invalid", values: map[string]string{
			runtimeCatalogControlEnv: "yes", runtimeCatalogNewSendEnv: "1",
		}, want: runtimeCatalogGates{}, warns: 2},
		{name: "explicit", values: map[string]string{
			runtimeCatalogControlEnv: "true", runtimeCatalogNewSendEnv: "true",
		}, want: runtimeCatalogGates{controlEnabled: true, newSendEnabled: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, warnings := resolveRuntimeCatalogGates(func(key string) string { return test.values[key] })
			if got != test.want || len(warnings) != test.warns {
				t.Fatalf("gates/warnings = (%+v, %v), want (%+v, %d)", got, warnings, test.want, test.warns)
			}
		})
	}
}

func TestRuntimeCatalogCacheConfigFromEnvIsBounded(t *testing.T) {
	values := map[string]string{
		runtimeCatalogCacheEntriesEnv: "32",
		runtimeCatalogCacheBytesEnv:   "16777216",
		runtimeCatalogCompileMSEnv:    "2500",
	}
	config, err := runtimeCatalogConfigFromEnv(cardtmpl.RuntimeCatalogConfig{}, func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if config.MaxCacheEntries != 32 || config.MaxCacheBytes != 16<<20 || config.CompileTimeout != 2500*time.Millisecond {
		t.Fatalf("config = %+v", config)
	}
	values[runtimeCatalogCacheEntriesEnv] = "257"
	if _, err := runtimeCatalogConfigFromEnv(cardtmpl.RuntimeCatalogConfig{}, func(key string) string { return values[key] }); err == nil {
		t.Fatal("over-hard-max cache entries accepted")
	}
}

func TestRuntimeDynamicAuthorizerKeepsNewSendDarkAndRequiresGrant(t *testing.T) {
	access := cardtmpl.CatalogAccess{Purpose: cardtmpl.CatalogPurposeNewSend}
	sendGrant := cardtmpl.RuntimeGrant{Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Discover: true, Send: true}
	approved := cardtmpl.RuntimeArtifactMeta{Owner: "docs"}

	// The new-send gate is checked before anything principal-specific, so a
	// dark deployment cannot be used to probe grants.
	authorize := runtimeDynamicAuthorizer(runtimeCatalogGates{}, nil)
	if err := authorize(context.Background(), access, approved, sendGrant); !errors.Is(err, cardtmpl.ErrRuntimeCatalogNewSendDisabled) {
		t.Fatalf("dark new-send error = %v, want ErrRuntimeCatalogNewSendDisabled", err)
	}
	authorize = runtimeDynamicAuthorizer(runtimeCatalogGates{newSendEnabled: true}, nil)
	if err := authorize(context.Background(), access, approved, cardtmpl.RuntimeGrant{}); !errors.Is(err, cardtmpl.ErrRuntimeCatalogNotAuthorized) {
		t.Fatalf("ungranted new-send error = %v, want ErrRuntimeCatalogNotAuthorized", err)
	}
	if err := authorize(context.Background(), access, cardtmpl.RuntimeArtifactMeta{Owner: "ext.vendor"}, sendGrant); !errors.Is(err, cardtmpl.ErrRuntimeCatalogNotAuthorized) {
		t.Fatalf("unapproved owner error = %v, want ErrRuntimeCatalogNotAuthorized", err)
	}
	if err := authorize(context.Background(), access, approved, sendGrant); err != nil {
		t.Fatalf("granted new-send: %v", err)
	}
}

// A grant is per-permission, not a blanket pass: holding discover never
// implies send, and a revoked tombstone (Found with nothing allowed) denies
// exactly like an absent row.
func TestRuntimeDynamicAuthorizerMatchesPurposeToPermission(t *testing.T) {
	authorize := runtimeDynamicAuthorizer(runtimeCatalogGates{newSendEnabled: true}, nil)
	meta := cardtmpl.RuntimeArtifactMeta{Owner: "docs"}
	discoverOnly := cardtmpl.RuntimeGrant{Found: true, Scope: cardtmpl.RuntimeGrantScopeGlobal, Discover: true}
	tombstone := cardtmpl.RuntimeGrant{Found: true, Scope: cardtmpl.RuntimeGrantScopeExact, Revision: 4}

	for _, test := range []struct {
		name    string
		purpose cardtmpl.CatalogPurpose
		grant   cardtmpl.RuntimeGrant
		allow   bool
	}{
		{"discover does not imply send", cardtmpl.CatalogPurposeNewSend, discoverOnly, false},
		{"discover does not imply edit", cardtmpl.CatalogPurposeHistoricalEdit, discoverOnly, false},
		{"send does not imply edit", cardtmpl.CatalogPurposeNewSend,
			cardtmpl.RuntimeGrant{Found: true, Discover: true, Send: true}, true},
		{"edit covers historical edit", cardtmpl.CatalogPurposeHistoricalEdit,
			cardtmpl.RuntimeGrant{Found: true, Discover: true, Edit: true}, true},
		{"edit covers action context", cardtmpl.CatalogPurposeActionContext,
			cardtmpl.RuntimeGrant{Found: true, Discover: true, Edit: true}, true},
		{"send alone cannot answer an action", cardtmpl.CatalogPurposeActionContext,
			cardtmpl.RuntimeGrant{Found: true, Discover: true, Send: true}, false},
		{"tombstone denies send", cardtmpl.CatalogPurposeNewSend, tombstone, false},
		{"tombstone denies edit", cardtmpl.CatalogPurposeHistoricalEdit, tombstone, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := authorize(context.Background(),
				cardtmpl.CatalogAccess{Purpose: test.purpose}, meta, test.grant)
			if test.allow && err != nil {
				t.Fatalf("authorize = %v, want allow", err)
			}
			if !test.allow && !errors.Is(err, cardtmpl.ErrRuntimeCatalogNotAuthorized) {
				t.Fatalf("authorize = %v, want ErrRuntimeCatalogNotAuthorized", err)
			}
		})
	}
}
