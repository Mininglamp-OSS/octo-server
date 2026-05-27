package i18n

import (
	"fmt"
	"net"
	"os"
	"strings"
)

const (
	// EnvDefaultLanguage controls the runtime fallback language for requests
	// that carry no explicit language signal.
	EnvDefaultLanguage = "OCTO_DEFAULT_LANGUAGE"

	// EnvTrustedLangHeaderCIDRs controls which direct peer CIDRs may supply
	// X-Octo-Lang for service-to-service calls.
	EnvTrustedLangHeaderCIDRs = "OCTO_TRUSTED_LANG_HEADER_CIDRS"

	// EnvTrustedProxyCIDRs controls which direct peer CIDRs are trusted
	// reverse proxies for X-Forwarded-For peeling.
	EnvTrustedProxyCIDRs = "OCTO_TRUSTED_PROXY_CIDRS"

	// DefaultLanguage preserves the legacy deployment behavior for clients that
	// do not send Accept-Language yet.
	DefaultLanguage = "zh-CN"
)

// DefaultLanguageFromEnv resolves OCTO_DEFAULT_LANGUAGE into a supported BCP-47
// language tag. Empty env uses DefaultLanguage; invalid values are rejected so
// rollout misconfiguration fails during startup instead of surfacing per request.
func DefaultLanguageFromEnv() (string, error) {
	return ResolveDefaultLanguage(os.Getenv(EnvDefaultLanguage))
}

// TrustedLangHeaderCIDRsFromEnv parses OCTO_TRUSTED_LANG_HEADER_CIDRS.
func TrustedLangHeaderCIDRsFromEnv() ([]*net.IPNet, error) {
	cidrs, err := ParseCIDRList(os.Getenv(EnvTrustedLangHeaderCIDRs))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", EnvTrustedLangHeaderCIDRs, err)
	}
	return cidrs, nil
}

// TrustedProxyCIDRsFromEnv parses OCTO_TRUSTED_PROXY_CIDRS.
func TrustedProxyCIDRsFromEnv() ([]*net.IPNet, error) {
	cidrs, err := ParseCIDRList(os.Getenv(EnvTrustedProxyCIDRs))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", EnvTrustedProxyCIDRs, err)
	}
	return cidrs, nil
}

// ResolveDefaultLanguage normalizes the configured default language.
func ResolveDefaultLanguage(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultLanguage, nil
	}
	if lang, ok := MatchSupportedLanguage(raw); ok {
		return lang, nil
	}
	return "", fmt.Errorf("%s must be one of %s, got %q", EnvDefaultLanguage, strings.Join(SupportedLanguages(), ", "), raw)
}

// SupportedLanguages returns the current runtime language matrix.
func SupportedLanguages() []string {
	out := make([]string, 0, len(supportedLanguageTags))
	for _, tag := range supportedLanguageTags {
		out = append(out, tag.String())
	}
	return out
}

// ValidateRuntimeLocales checks the locale files required for startup:
// source language and configured default language must both have active TOML
// files in the embedded runtime bundle.
func ValidateRuntimeLocales(defaultLang string) error {
	normalizedDefault, err := ResolveDefaultLanguage(defaultLang)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, lang := range []string{SourceLanguage, normalizedDefault} {
		if seen[lang] {
			continue
		}
		seen[lang] = true
		if !activeLocaleExists(lang) {
			return fmt.Errorf("i18n active locale for %s is missing", lang)
		}
	}
	if _, err := Bundle(); err != nil {
		return err
	}
	return nil
}

func activeLocaleExists(lang string) bool {
	_, err := localesFS.ReadFile("locales/active." + lang + ".toml")
	return err == nil
}
