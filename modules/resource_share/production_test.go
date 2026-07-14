package resource_share

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeJSONFile(t *testing.T, name string, value interface{}) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	path := t.TempDir() + "/" + name
	require.NoError(t, os.WriteFile(path, encoded, 0o600))
	return path
}

func newProofTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key
}

func TestLoadProofMaterialCombinesActiveAndRetainedKeys(t *testing.T) {
	active := newProofTestKey(t)
	retained := newProofTestKey(t)
	signingFile := writeJSONFile(t, "active.jwk", jose.JSONWebKey{
		Key: active, KeyID: "active-key", Algorithm: "ES256", Use: "sig",
	})
	verificationFile := writeJSONFile(t, "retained.jwks", jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &retained.PublicKey, KeyID: "retained-key", Algorithm: "ES256", Use: "sig",
	}}})

	signer, verifier, err := loadProofMaterial(signingFile, verificationFile)
	require.NoError(t, err)
	require.NotNil(t, signer)
	require.NotNil(t, verifier)
	jwks := verifier.JWKS()
	require.Len(t, jwks.Keys, 2)
	assert.Equal(t, "active-key", jwks.Keys[0].KeyID)
	assert.Equal(t, "retained-key", jwks.Keys[1].KeyID)
	for _, key := range jwks.Keys {
		_, private := key.Key.(*ecdsa.PrivateKey)
		assert.False(t, private)
	}
}

func TestLoadProofMaterialSupportsVerificationOnlyRollback(t *testing.T) {
	retained := newProofTestKey(t)
	verificationFile := writeJSONFile(t, "retained.jwks", jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &retained.PublicKey, KeyID: "retained-key", Algorithm: "ES256", Use: "sig",
	}}})

	signer, verifier, err := loadProofMaterial("", verificationFile)
	require.NoError(t, err)
	assert.Nil(t, signer)
	require.NotNil(t, verifier)
	assert.Len(t, verifier.JWKS().Keys, 1)

	signer, verifier, err = loadProofMaterial("", "")
	require.NoError(t, err)
	assert.Nil(t, signer)
	assert.Nil(t, verifier)
}

func TestLoadProofMaterialRejectsMalformedConflictingOrOversizedKeys(t *testing.T) {
	active := newProofTestKey(t)
	other := newProofTestKey(t)
	validSigning := writeJSONFile(t, "active.jwk", jose.JSONWebKey{
		Key: active, KeyID: "same-key", Algorithm: "ES256", Use: "sig",
	})

	t.Run("conflicting retained kid", func(t *testing.T) {
		verification := writeJSONFile(t, "conflict.jwks", jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: &other.PublicKey, KeyID: "same-key", Algorithm: "ES256", Use: "sig",
		}}})
		_, _, err := loadProofMaterial(validSigning, verification)
		assert.Error(t, err)
	})

	t.Run("public signing key", func(t *testing.T) {
		path := writeJSONFile(t, "public.jwk", jose.JSONWebKey{
			Key: &active.PublicKey, KeyID: "public-key", Algorithm: "ES256", Use: "sig",
		})
		_, _, err := loadProofMaterial(path, "")
		assert.Error(t, err)
	})

	t.Run("wrong algorithm metadata", func(t *testing.T) {
		path := writeJSONFile(t, "wrong-alg.jwk", jose.JSONWebKey{
			Key: active, KeyID: "active-key", Algorithm: "ES384", Use: "sig",
		})
		_, _, err := loadProofMaterial(path, "")
		assert.Error(t, err)
	})

	t.Run("oversized file", func(t *testing.T) {
		path := t.TempDir() + "/oversized.jwk"
		require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", maxProofKeyFileBytes+1)), 0o600))
		_, _, err := loadProofMaterial(path, "")
		assert.Error(t, err)
	})
}

func TestLoadRuntimeConfigUsesBoundedDefaultsAndRejectsInvalidValues(t *testing.T) {
	for _, key := range []string{
		"DM_RESOURCE_SHARE_ENABLED", "DM_RESOURCE_SHARE_MAX_CONCURRENT_DISPATCHES",
		"DM_RESOURCE_SHARE_GLOBAL_RPS", "DM_RESOURCE_SHARE_GLOBAL_BURST",
		"DM_RESOURCE_SHARE_DM_RPS", "DM_RESOURCE_SHARE_DM_BURST",
		"DM_RESOURCE_SHARE_CHANNEL_RPS", "DM_RESOURCE_SHARE_CHANNEL_BURST",
		"DM_RESOURCE_SHARE_LIMIT_FAILURE_RETRY_SECONDS",
		"DM_RESOURCE_SHARE_PROOF_SIGNING_JWK_FILE", "DM_RESOURCE_SHARE_PROOF_VERIFICATION_JWKS_FILE",
	} {
		t.Setenv(key, "")
	}
	config, err := loadRuntimeConfigFromEnv()
	require.NoError(t, err)
	assert.False(t, config.FeatureEnabled)
	assert.Positive(t, config.MaxConcurrentDispatches)
	assert.Positive(t, config.GlobalBudget.RatePerSecond)
	assert.Positive(t, config.GlobalBudget.Burst)
	assert.GreaterOrEqual(t, config.LimitFailureRetry, time.Second)
	assert.LessOrEqual(t, config.LimitFailureRetry, time.Minute)

	t.Setenv("DM_RESOURCE_SHARE_MAX_CONCURRENT_DISPATCHES", "0")
	_, err = loadRuntimeConfigFromEnv()
	assert.Error(t, err)
	t.Setenv("DM_RESOURCE_SHARE_MAX_CONCURRENT_DISPATCHES", "16")
	t.Setenv("DM_RESOURCE_SHARE_GLOBAL_RPS", "NaN")
	_, err = loadRuntimeConfigFromEnv()
	assert.Error(t, err)
}

func TestProductionWiringSourceUsesReviewedDependencies(t *testing.T) {
	data, err := os.ReadFile("production.go")
	require.NoError(t, err)
	source := string(data)
	for _, required := range []string{
		"resourceshare.NewRegistry", "resourceshare.NewDurableStore", "resourceshare.NewHumanTargetAuthorizer",
		"resourceshare.NewRedisTargetLimiter", "ctx.SendMessageWithResult", "MaxConcurrentDispatches",
	} {
		assert.Contains(t, source, required)
	}
}
