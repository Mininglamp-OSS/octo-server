package i18n

import (
	"strings"
	"testing"
)

func TestResolveDefaultLanguage(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "empty uses runtime default", raw: "", want: DefaultLanguage},
		{name: "normalizes zh alias", raw: "zh", want: "zh-CN"},
		{name: "normalizes en alias", raw: "en", want: "en-US"},
		{name: "rejects unsupported", raw: "fr-FR", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveDefaultLanguage(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ResolveDefaultLanguage returned nil error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveDefaultLanguage returned error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ResolveDefaultLanguage = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultLanguageFromEnv(t *testing.T) {
	t.Setenv(EnvDefaultLanguage, "")
	got, err := DefaultLanguageFromEnv()
	if err != nil {
		t.Fatalf("DefaultLanguageFromEnv returned error: %v", err)
	}
	if got != DefaultLanguage {
		t.Fatalf("DefaultLanguageFromEnv = %q, want %q", got, DefaultLanguage)
	}

	t.Setenv(EnvDefaultLanguage, "en-US")
	got, err = DefaultLanguageFromEnv()
	if err != nil {
		t.Fatalf("DefaultLanguageFromEnv returned error: %v", err)
	}
	if got != "en-US" {
		t.Fatalf("DefaultLanguageFromEnv = %q, want en-US", got)
	}
}

func TestTrustedLangHeaderCIDRsFromEnvPrefersOctoName(t *testing.T) {
	t.Setenv(EnvTrustedLangHeaderCIDRs, "10.0.0.0/8")

	got, err := TrustedLangHeaderCIDRsFromEnv()
	if err != nil {
		t.Fatalf("TrustedLangHeaderCIDRsFromEnv returned error: %v", err)
	}
	if len(got) != 1 || got[0].String() != "10.0.0.0/8" {
		t.Fatalf("TrustedLangHeaderCIDRsFromEnv = %v, want OCTO env CIDR", got)
	}
}

func TestTrustedLangHeaderCIDRsFromEnvIgnoresLegacyName(t *testing.T) {
	t.Setenv(EnvTrustedLangHeaderCIDRs, "")
	// Set legacy env to an INVALID CIDR: if the helper still consulted it,
	// ParseCIDRList would surface a parse error. A nil error therefore proves
	// the legacy var is never read.
	t.Setenv("DM_TRUSTED_LANG_HEADER_CIDRS", "not-cidr")

	got, err := TrustedLangHeaderCIDRsFromEnv()
	if err != nil {
		t.Fatalf("TrustedLangHeaderCIDRsFromEnv returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("TrustedLangHeaderCIDRsFromEnv = %v, want empty list", got)
	}
}

func TestTrustedLangHeaderCIDRsFromEnvReportsOctoName(t *testing.T) {
	t.Setenv(EnvTrustedLangHeaderCIDRs, "not-cidr")

	_, err := TrustedLangHeaderCIDRsFromEnv()
	if err == nil {
		t.Fatal("TrustedLangHeaderCIDRsFromEnv returned nil error")
	}
	if want := EnvTrustedLangHeaderCIDRs; !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %q, want env name %q", err.Error(), want)
	}
}

func TestTrustedProxyCIDRsFromEnv(t *testing.T) {
	t.Setenv(EnvTrustedProxyCIDRs, "172.16.0.0/12")
	t.Setenv("DM_TRUSTED_PROXY_CIDRS", "192.168.0.0/16")

	got, err := TrustedProxyCIDRsFromEnv()
	if err != nil {
		t.Fatalf("TrustedProxyCIDRsFromEnv returned error: %v", err)
	}
	if len(got) != 1 || got[0].String() != "172.16.0.0/12" {
		t.Fatalf("TrustedProxyCIDRsFromEnv = %v, want OCTO proxy CIDR", got)
	}
}

func TestTrustedProxyCIDRsFromEnvIgnoresLegacyName(t *testing.T) {
	t.Setenv(EnvTrustedProxyCIDRs, "")
	t.Setenv("DM_TRUSTED_PROXY_CIDRS", "192.168.0.0/16")

	got, err := TrustedProxyCIDRsFromEnv()
	if err != nil {
		t.Fatalf("TrustedProxyCIDRsFromEnv returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("TrustedProxyCIDRsFromEnv = %v, want empty list", got)
	}
}

func TestValidateRuntimeLocales(t *testing.T) {
	resetBundle()
	t.Cleanup(resetBundle)

	for _, lang := range []string{DefaultLanguage, SourceLanguage} {
		t.Run(lang, func(t *testing.T) {
			if err := ValidateRuntimeLocales(lang); err != nil {
				t.Fatalf("ValidateRuntimeLocales(%q) returned error: %v", lang, err)
			}
		})
	}
}

func TestActiveLocaleExists(t *testing.T) {
	if !activeLocaleExists(SourceLanguage) {
		t.Fatalf("source locale %q should exist", SourceLanguage)
	}
	if !activeLocaleExists(DefaultLanguage) {
		t.Fatalf("default locale %q should exist", DefaultLanguage)
	}
	if activeLocaleExists("fr-FR") {
		t.Fatal("fr-FR active locale should not exist")
	}
}
