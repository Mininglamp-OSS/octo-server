package resourceshare

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validIntent(now time.Time) Intent {
	return Intent{
		Version:        1,
		Provider:       "smart-summary",
		Issuer:         "https://smart-summary.internal",
		Audience:       "octo-server:resource-share",
		ActorUID:       "user-a",
		SpaceID:        "space-a",
		IssuedAt:       now.Unix(),
		ExpiresAt:      now.Add(2 * time.Minute).Unix(),
		Nonce:          "nonce-0123456789",
		IdempotencyKey: "idem-0123456789",
		Resource: ResourceRef{
			Type:     "smart-summary",
			ID:       "summary-1",
			Revision: "rev-3",
		},
		Template: TemplateRef{ID: "summary-share", Version: 1},
		Targets: []Target{
			{Kind: TargetDM, PeerUID: "user-b"},
			{Kind: TargetGroup, GroupNo: "group-a"},
		},
		Claims: json.RawMessage(`{"title":"Quarterly summary","count":1}`),
	}
}

func signIntent(t *testing.T, key interface{}, alg jose.SignatureAlgorithm, kid string, intent Intent) string {
	t.Helper()
	raw, err := json.Marshal(intent)
	require.NoError(t, err)
	return signIntentRaw(t, key, alg, kid, raw)
}

func signIntentRaw(t *testing.T, key interface{}, alg jose.SignatureAlgorithm, kid string, raw []byte) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), kid)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	require.NoError(t, err)
	signed, err := signer.Sign(raw)
	require.NoError(t, err)
	compact, err := signed.CompactSerialize()
	require.NoError(t, err)
	return compact
}

func newIntentRegistry(t *testing.T, key *ecdsa.PrivateKey, mutate ...func(*ProviderSpec)) *Registry {
	t.Helper()
	spec := validProviderSpec(t, key)
	for _, fn := range mutate {
		fn(&spec)
	}
	registry, err := NewRegistry([]ProviderSpec{spec})
	require.NoError(t, err)
	return registry
}

func TestVerifyIntent_AcceptsValidSignedIntent(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := newIntentTestKey(t)
	registry := newIntentRegistry(t, key)

	verified, err := registry.VerifyIntent(context.Background(), signIntent(t, key, jose.ES256, "intent-key-1", validIntent(now)), now)
	require.NoError(t, err)
	assert.Equal(t, ProviderID("smart-summary"), verified.ProviderID)
	assert.Equal(t, "user-a", verified.Intent.ActorUID)
	assert.NotEqual(t, IntentFingerprint{}, verified.Fingerprint)
}

func TestVerifyIntent_FingerprintUsesJCSCanonicalPayload(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := newIntentTestKey(t)
	registry := newIntentRegistry(t, key)

	first := validIntent(now)
	first.Claims = json.RawMessage(`{"title":"Quarterly summary","count":1}`)
	second := validIntent(now)
	second.Claims = json.RawMessage(`{"count":1,"title":"Quarterly summary"}`)

	v1, err := registry.VerifyIntent(context.Background(), signIntent(t, key, jose.ES256, "intent-key-1", first), now)
	require.NoError(t, err)
	v2, err := registry.VerifyIntent(context.Background(), signIntent(t, key, jose.ES256, "intent-key-1", second), now)
	require.NoError(t, err)
	assert.Equal(t, v1.Fingerprint, v2.Fingerprint)
}

func TestVerifyIntent_RejectsSignatureKidAndAlgorithmFailures(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := newIntentTestKey(t)
	otherKey := newIntentTestKey(t)
	registry := newIntentRegistry(t, key)

	tests := []struct {
		name    string
		compact func(*testing.T) string
	}{
		{"wrong signature", func(t *testing.T) string {
			return signIntent(t, otherKey, jose.ES256, "intent-key-1", validIntent(now))
		}},
		{"unknown kid", func(t *testing.T) string {
			return signIntent(t, key, jose.ES256, "unknown-key", validIntent(now))
		}},
		{"algorithm confusion", func(t *testing.T) string {
			return signIntent(t, []byte("01234567890123456789012345678901"), jose.HS256, "intent-key-1", validIntent(now))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registry.VerifyIntent(context.Background(), tt.compact(t), now)
			assert.ErrorIs(t, err, ErrIntentSignature)
		})
	}
}

func TestVerifyIntent_RejectsMalformedOrUnauthorizedClaims(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := newIntentTestKey(t)
	registry := newIntentRegistry(t, key)

	tests := []struct {
		name   string
		mutate func(*Intent)
	}{
		{"wrong version", func(i *Intent) { i.Version = 2 }},
		{"wrong issuer", func(i *Intent) { i.Issuer = "https://attacker.invalid" }},
		{"wrong audience", func(i *Intent) { i.Audience = "other" }},
		{"missing actor", func(i *Intent) { i.ActorUID = "" }},
		{"missing space", func(i *Intent) { i.SpaceID = "" }},
		{"expired", func(i *Intent) { i.ExpiresAt = now.Add(-time.Minute).Unix() }},
		{"issued in future", func(i *Intent) { i.IssuedAt = now.Add(time.Minute).Unix() }},
		{"lifetime too long", func(i *Intent) { i.ExpiresAt = now.Add(6 * time.Minute).Unix() }},
		{"far-future lifetime overflow", func(i *Intent) { i.ExpiresAt = i.IssuedAt + 10_000_000_000 }},
		{"missing nonce", func(i *Intent) { i.Nonce = "" }},
		{"missing idempotency key", func(i *Intent) { i.IdempotencyKey = "" }},
		{"wrong resource type", func(i *Intent) { i.Resource.Type = "docs" }},
		{"missing resource id", func(i *Intent) { i.Resource.ID = "" }},
		{"missing revision", func(i *Intent) { i.Resource.Revision = "" }},
		{"unsupported template", func(i *Intent) { i.Template.ID = "attacker-template" }},
		{"claims rejected by provider", func(i *Intent) { i.Claims = json.RawMessage(`{"count":1}`) }},
		{"targets not canonical", func(i *Intent) { i.Targets[0], i.Targets[1] = i.Targets[1], i.Targets[0] }},
		{"duplicate target", func(i *Intent) { i.Targets[1] = i.Targets[0] }},
		{"self dm", func(i *Intent) { i.Targets = []Target{{Kind: TargetDM, PeerUID: i.ActorUID}} }},
		{"mixed target fields", func(i *Intent) { i.Targets = []Target{{Kind: TargetDM, PeerUID: "user-b", GroupNo: "g"}} }},
		{"empty targets", func(i *Intent) { i.Targets = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			intent := validIntent(now)
			tt.mutate(&intent)
			_, err := registry.VerifyIntent(context.Background(), signIntent(t, key, jose.ES256, "intent-key-1", intent), now)
			assert.ErrorIs(t, err, ErrIntentInvalid)
		})
	}
}

func TestVerifyIntent_RejectsBoundsUnknownFieldsAndDuplicateJSONKeys(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := newIntentTestKey(t)
	registry := newIntentRegistry(t, key)

	t.Run("compact JWS too large", func(t *testing.T) {
		_, err := registry.VerifyIntent(context.Background(), strings.Repeat("x", PlatformMaxCompactIntentBytes+1), now)
		assert.ErrorIs(t, err, ErrIntentInvalid)
	})

	t.Run("claims too large", func(t *testing.T) {
		intent := validIntent(now)
		intent.Claims = json.RawMessage(`{"title":"` + string(bytes.Repeat([]byte("x"), PlatformMaxClaimsBytes)) + `"}`)
		_, err := registry.VerifyIntent(context.Background(), signIntent(t, key, jose.ES256, "intent-key-1", intent), now)
		assert.ErrorIs(t, err, ErrIntentInvalid)
	})

	t.Run("too many targets", func(t *testing.T) {
		intent := validIntent(now)
		intent.Targets = make([]Target, 0, PlatformMaxTargets+1)
		for n := 0; n <= PlatformMaxTargets; n++ {
			intent.Targets = append(intent.Targets, Target{Kind: TargetDM, PeerUID: "peer-" + strings.Repeat("x", n)})
		}
		_, err := registry.VerifyIntent(context.Background(), signIntent(t, key, jose.ES256, "intent-key-1", intent), now)
		assert.ErrorIs(t, err, ErrIntentInvalid)
	})

	t.Run("unknown top-level field", func(t *testing.T) {
		intent := validIntent(now)
		raw, err := json.Marshal(intent)
		require.NoError(t, err)
		raw = append(raw[:len(raw)-1], []byte(`,"from_uid":"attacker"}`)...)
		_, err = registry.VerifyIntent(context.Background(), signIntentRaw(t, key, jose.ES256, "intent-key-1", raw), now)
		assert.ErrorIs(t, err, ErrIntentInvalid)
	})

	t.Run("duplicate JSON key", func(t *testing.T) {
		intent := validIntent(now)
		raw, err := json.Marshal(intent)
		require.NoError(t, err)
		raw = append(raw[:len(raw)-1], []byte(`,"actor_uid":"attacker"}`)...)
		_, err = registry.VerifyIntent(context.Background(), signIntentRaw(t, key, jose.ES256, "intent-key-1", raw), now)
		assert.ErrorIs(t, err, ErrIntentInvalid)
	})
}

func TestVerifyIntent_RejectsUnknownOrDisabledProviderBeforeTransport(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := newIntentTestKey(t)
	registry := newIntentRegistry(t, key)

	intent := validIntent(now)
	intent.Provider = "docs"
	_, err := registry.VerifyIntent(context.Background(), signIntent(t, key, jose.ES256, "intent-key-1", intent), now)
	assert.ErrorIs(t, err, ErrProviderNotFound)

	disabled, err := NewRegistry([]ProviderSpec{{ID: "smart-summary", Enabled: false}})
	require.NoError(t, err)
	_, err = disabled.VerifyIntent(context.Background(), signIntent(t, key, jose.ES256, "intent-key-1", validIntent(now)), now)
	assert.ErrorIs(t, err, ErrProviderDisabled)
}

func TestVerifyIntent_RespectsContextCancellation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	key := newIntentTestKey(t)
	registry := newIntentRegistry(t, key)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := registry.VerifyIntent(ctx, signIntent(t, key, jose.ES256, "intent-key-1", validIntent(now)), now)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestVerifyIntentForRetry_AuthenticatesExpiredIntentWithoutReauthorizingIt(t *testing.T) {
	key := newIntentTestKey(t)
	registry := newIntentRegistry(t, key)
	issuedAt := time.Unix(1_800_000_000, 0).UTC()
	intent := validIntent(issuedAt)
	compact := signIntent(t, key, jose.ES256, "intent-key-1", intent)
	afterExpiry := time.Unix(intent.ExpiresAt, 0).Add(PlatformMaxClockSkew + time.Second)

	_, err := registry.VerifyIntent(context.Background(), compact, afterExpiry)
	assert.ErrorIs(t, err, ErrIntentInvalid)
	verified, err := registry.VerifyIntentForRetry(context.Background(), compact, afterExpiry)
	require.NoError(t, err)
	assert.True(t, verified.Expired)
	assert.Equal(t, intent.Nonce, verified.Intent.Nonce)

	parts := strings.Split(compact, ".")
	require.Len(t, parts, 3)
	parts[2] = "A" + parts[2][1:]
	tampered := strings.Join(parts, ".")
	_, err = registry.VerifyIntentForRetry(context.Background(), tampered, afterExpiry)
	assert.ErrorIs(t, err, ErrIntentSignature)
}

func TestClassifyReplay_DistinguishesRetryFromConflict(t *testing.T) {
	first := IntentFingerprint{1}
	same := IntentFingerprint{1}
	different := IntentFingerprint{2}

	disposition, err := ClassifyReplay(nil, first)
	require.NoError(t, err)
	assert.Equal(t, ReplayFirstUse, disposition)

	disposition, err = ClassifyReplay(&first, same)
	require.NoError(t, err)
	assert.Equal(t, ReplayRetry, disposition)

	_, err = ClassifyReplay(&first, different)
	assert.True(t, errors.Is(err, ErrIntentReplay))
}

func TestIntentSigningHelperUsesP256(t *testing.T) {
	// Guard the fixture itself: ES256 must never silently drift to another
	// curve, otherwise algorithm-confusion tests lose their intended boundary.
	key := newIntentTestKey(t)
	assert.Equal(t, elliptic.P256(), key.Curve)
	_, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
}
