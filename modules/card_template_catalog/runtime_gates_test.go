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
	authorize := runtimeDynamicAuthorizer(runtimeCatalogGates{}, nil)
	if err := authorize(context.Background(), access, cardtmpl.RuntimeArtifactMeta{}); !errors.Is(err, cardtmpl.ErrRuntimeCatalogNewSendDisabled) {
		t.Fatalf("dark new-send error = %v, want ErrRuntimeCatalogNewSendDisabled", err)
	}
	authorize = runtimeDynamicAuthorizer(runtimeCatalogGates{newSendEnabled: true}, nil)
	if err := authorize(context.Background(), access, cardtmpl.RuntimeArtifactMeta{}); !errors.Is(err, cardtmpl.ErrRuntimeCatalogNotAuthorized) {
		t.Fatalf("ungranted new-send error = %v, want ErrRuntimeCatalogNotAuthorized", err)
	}
	authorize = runtimeDynamicAuthorizer(runtimeCatalogGates{newSendEnabled: true},
		func(context.Context, cardtmpl.CatalogAccess, cardtmpl.RuntimeArtifactMeta) error { return nil })
	if err := authorize(context.Background(), access, cardtmpl.RuntimeArtifactMeta{Owner: "ext.vendor"}); !errors.Is(err, cardtmpl.ErrRuntimeCatalogNotAuthorized) {
		t.Fatalf("unapproved owner error = %v, want ErrRuntimeCatalogNotAuthorized", err)
	}
}
