package resourceshare

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testAdapter struct{}

func (testAdapter) Revalidate(context.Context, VerifiedIntent) (*RevalidatedResource, error) {
	return &RevalidatedResource{}, nil
}

func newIntentTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key
}

func validProviderSpec(t *testing.T, key *ecdsa.PrivateKey) ProviderSpec {
	t.Helper()
	return ProviderSpec{
		ID:            "smart-summary",
		Enabled:       true,
		ResourceType:  "smart-summary",
		Issuer:        "https://smart-summary.internal",
		Audience:      "octo-server:resource-share",
		IntentVersion: 1,
		VerificationKeys: []VerificationKey{{
			KeyID:     "intent-key-1",
			Algorithm: jose.ES256,
			PublicKey: &key.PublicKey,
		}},
		Templates: []TemplateRef{{ID: "summary-share", Version: 1}},
		Limits: ProviderLimits{
			MaxClaimsBytes:    32 << 10,
			MaxTargets:        20,
			MaxIntentLifetime: 5 * time.Minute,
			ClockSkew:         30 * time.Second,
		},
		ValidateClaims: func(raw json.RawMessage) error {
			var claims struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal(raw, &claims); err != nil {
				return err
			}
			if claims.Title == "" {
				return errors.New("title is required")
			}
			return nil
		},
		BuildDeepLink: func(ref ResourceRef) (*url.URL, error) {
			return url.Parse("https://app.example.test/summaries/" + ref.ID)
		},
		RenderCard: func(input ResourceCardInput, link *url.URL) (map[string]interface{}, error) {
			return map[string]interface{}{
				"type":    "AdaptiveCard",
				"version": "1.5",
				"body": []interface{}{
					map[string]interface{}{"type": "TextBlock", "text": input.Title},
				},
				"actions": []interface{}{
					map[string]interface{}{"type": "Action.OpenUrl", "title": "Open", "url": link.String()},
				},
			}, nil
		},
		Adapter: testAdapter{},
	}
}

func TestRegistry_AcceptsReviewedProviderAndCopiesConfiguration(t *testing.T) {
	key := newIntentTestKey(t)
	spec := validProviderSpec(t, key)

	registry, err := NewRegistry([]ProviderSpec{spec})
	require.NoError(t, err)

	provider, err := registry.Provider("smart-summary")
	require.NoError(t, err)
	assert.Equal(t, ProviderID("smart-summary"), provider.ID())

	// Mutating caller-owned slices after construction must not alter the
	// immutable startup registry.
	spec.VerificationKeys[0].KeyID = "attacker-key"
	spec.Templates[0].ID = "attacker-template"

	now := time.Unix(1_800_000_000, 0).UTC()
	compact := signIntent(t, key, jose.ES256, "intent-key-1", validIntent(now))
	verified, err := registry.VerifyIntent(context.Background(), compact, now)
	require.NoError(t, err)
	assert.Equal(t, "summary-share", verified.Intent.Template.ID)
}

func TestRegistry_UnknownAndDisabledProvidersFailClosed(t *testing.T) {
	registry, err := NewRegistry([]ProviderSpec{{ID: "docs", Enabled: false}})
	require.NoError(t, err)

	_, err = registry.Provider("missing")
	assert.ErrorIs(t, err, ErrProviderNotFound)

	_, err = registry.Provider("docs")
	assert.ErrorIs(t, err, ErrProviderDisabled)
}

func TestRegistry_RejectsDuplicateAndInvalidEnabledProviders(t *testing.T) {
	key := newIntentTestKey(t)
	valid := validProviderSpec(t, key)

	t.Run("duplicate provider", func(t *testing.T) {
		_, err := NewRegistry([]ProviderSpec{valid, valid})
		assert.ErrorIs(t, err, ErrProviderConfig)
	})

	tests := []struct {
		name   string
		mutate func(*ProviderSpec)
	}{
		{"invalid id", func(s *ProviderSpec) { s.ID = "Bad/Provider" }},
		{"missing resource type", func(s *ProviderSpec) { s.ResourceType = "" }},
		{"insecure issuer", func(s *ProviderSpec) { s.Issuer = "http://summary.internal" }},
		{"missing audience", func(s *ProviderSpec) { s.Audience = "" }},
		{"unsupported intent version", func(s *ProviderSpec) { s.IntentVersion = 2 }},
		{"missing keys", func(s *ProviderSpec) { s.VerificationKeys = nil }},
		{"duplicate kid", func(s *ProviderSpec) {
			s.VerificationKeys = append(s.VerificationKeys, s.VerificationKeys[0])
		}},
		{"unsupported algorithm", func(s *ProviderSpec) { s.VerificationKeys[0].Algorithm = jose.HS256 }},
		{"private verification key", func(s *ProviderSpec) { s.VerificationKeys[0].PublicKey = key }},
		{"missing templates", func(s *ProviderSpec) { s.Templates = nil }},
		{"duplicate template", func(s *ProviderSpec) { s.Templates = append(s.Templates, s.Templates[0]) }},
		{"claims limit exceeds platform", func(s *ProviderSpec) { s.Limits.MaxClaimsBytes = PlatformMaxClaimsBytes + 1 }},
		{"target limit exceeds platform", func(s *ProviderSpec) { s.Limits.MaxTargets = PlatformMaxTargets + 1 }},
		{"lifetime exceeds platform", func(s *ProviderSpec) { s.Limits.MaxIntentLifetime = PlatformMaxIntentLifetime + time.Second }},
		{"clock skew exceeds platform", func(s *ProviderSpec) { s.Limits.ClockSkew = PlatformMaxClockSkew + time.Second }},
		{"missing claims validator", func(s *ProviderSpec) { s.ValidateClaims = nil }},
		{"missing deep link builder", func(s *ProviderSpec) { s.BuildDeepLink = nil }},
		{"missing card renderer", func(s *ProviderSpec) { s.RenderCard = nil }},
		{"missing adapter", func(s *ProviderSpec) { s.Adapter = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validProviderSpec(t, key)
			tt.mutate(&spec)
			_, err := NewRegistry([]ProviderSpec{spec})
			assert.ErrorIs(t, err, ErrProviderConfig)
		})
	}
}
