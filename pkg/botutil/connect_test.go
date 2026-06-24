package botutil

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

func TestPluginPackage_DefaultAndOverride(t *testing.T) {
	// Default: no env set → the canonical package.
	t.Setenv(PluginPackageEnv, "")
	if got := PluginPackage(); got != DefaultPluginPackage {
		t.Fatalf("PluginPackage() default = %q, want %q", got, DefaultPluginPackage)
	}
	if DefaultPluginPackage != "create-openclaw-octo" {
		t.Fatalf("DefaultPluginPackage = %q, want create-openclaw-octo", DefaultPluginPackage)
	}

	// Override: env wins (rename / canary touches only the backend).
	t.Setenv(PluginPackageEnv, "openclaw-channel-canary")
	if got := PluginPackage(); got != "openclaw-channel-canary" {
		t.Fatalf("PluginPackage() override = %q, want openclaw-channel-canary", got)
	}

	// Whitespace-only override is ignored (falls back to default).
	t.Setenv(PluginPackageEnv, "   ")
	if got := PluginPackage(); got != DefaultPluginPackage {
		t.Fatalf("PluginPackage() blank override = %q, want %q", got, DefaultPluginPackage)
	}
}

func TestDeriveAPIURL(t *testing.T) {
	cfg := config.New()

	// Configured BaseURL is the public Bot API entry, used verbatim.
	cfg.External.BaseURL = "https://api.example.com"
	if got := DeriveAPIURL(cfg); got != "https://api.example.com" {
		t.Fatalf("DeriveAPIURL(BaseURL set) = %q, want https://api.example.com", got)
	}

	// Empty BaseURL → direct-access fallback derived from External.IP.
	cfg.External.BaseURL = ""
	cfg.External.IP = "10.0.0.5"
	if got := DeriveAPIURL(cfg); got != "http://10.0.0.5:8090" {
		t.Fatalf("DeriveAPIURL(BaseURL empty) = %q, want http://10.0.0.5:8090", got)
	}
}
