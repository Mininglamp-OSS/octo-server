package resourceshare

import (
	"crypto/ecdsa"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	jose "github.com/go-jose/go-jose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validProofContext(target Target) ProofContext {
	return ProofContext{
		ActorUID:   "user-a",
		SpaceID:    "space-a",
		ProviderID: "smart-summary",
		Resource: ResourceRef{
			Type:     "smart-summary",
			ID:       "summary-1",
			Revision: "rev-3",
		},
		Target:     target,
		DeliveryID: strings.Repeat("a", 64),
	}
}

func proofPayload() map[string]interface{} {
	return map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      cardmsg.ProfileV1,
		"card": map[string]interface{}{
			"type":    "AdaptiveCard",
			"version": cardmsg.CardVersion,
			"body": []interface{}{
				map[string]interface{}{"type": "TextBlock", "text": "Quarterly summary"},
			},
			"actions": []interface{}{
				map[string]interface{}{"type": "Action.OpenUrl", "title": "Open", "url": "https://app.example.test/summaries/summary-1"},
			},
		},
		// Seal must overwrite both fields authoritatively on its private copy.
		"plain":    "attacker controlled",
		"space_id": "wrong-space",
	}
}

func newProofHarness(t *testing.T) (*ProofSigner, *ProofVerifier, *ecdsa.PrivateKey) {
	t.Helper()
	key := newIntentTestKey(t)
	signer, err := NewProofSigner(ProofSigningKey{KeyID: "proof-key-1", PrivateKey: key})
	require.NoError(t, err)
	verifier, err := NewProofVerifier([]ProofVerificationKey{{
		KeyID:     "proof-key-1",
		PublicKey: &key.PublicKey,
	}})
	require.NoError(t, err)
	return signer, verifier, key
}

func cloneProofPayload(t *testing.T, payload map[string]interface{}) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	var cloned map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &cloned))
	return cloned
}

func TestProofSealAndVerify_Group(t *testing.T) {
	signer, verifier, _ := newProofHarness(t)
	input := proofPayload()
	ctx := validProofContext(Target{Kind: TargetGroup, GroupNo: "group-a"})

	sealed, err := signer.Seal(input, ctx)
	require.NoError(t, err)
	require.NotSame(t, input, sealed)
	assert.Equal(t, "attacker controlled", input["plain"], "Seal must not mutate caller-owned input")
	assert.Equal(t, "Quarterly summary", sealed["plain"])
	assert.Equal(t, "space-a", sealed["space_id"])
	assert.Contains(t, sealed, ProofField)

	err = verifier.Verify(sealed, ProofObservation{
		ActorUID: "user-a",
		SpaceID:  "space-a",
		Target:   Target{Kind: TargetGroup, GroupNo: "group-a"},
	})
	require.NoError(t, err)
}

func TestProofVerify_DMIsBoundToBothConversationParticipants(t *testing.T) {
	signer, verifier, _ := newProofHarness(t)
	sealed, err := signer.Seal(proofPayload(), validProofContext(Target{Kind: TargetDM, PeerUID: "user-b"}))
	require.NoError(t, err)

	tests := []struct {
		name        string
		observation ProofObservation
	}{
		{
			name: "sender view",
			observation: ProofObservation{
				ActorUID:  "user-a",
				ViewerUID: "user-a",
				SpaceID:   "space-a",
				Target:    Target{Kind: TargetDM, PeerUID: "user-b"},
			},
		},
		{
			name: "recipient view",
			observation: ProofObservation{
				ActorUID:  "user-a",
				ViewerUID: "user-b",
				SpaceID:   "space-a",
				Target:    Target{Kind: TargetDM, PeerUID: "user-a"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, verifier.Verify(sealed, tt.observation))
		})
	}
}

func TestProofVerify_RejectsTamperingAndWrongObservedContext(t *testing.T) {
	signer, verifier, _ := newProofHarness(t)
	sealed, err := signer.Seal(proofPayload(), validProofContext(Target{Kind: TargetThread, GroupNo: "group-a", ShortID: "topic-1"}))
	require.NoError(t, err)
	validObservation := ProofObservation{
		ActorUID: "user-a",
		SpaceID:  "space-a",
		Target:   Target{Kind: TargetThread, GroupNo: "group-a", ShortID: "topic-1"},
	}

	tests := []struct {
		name   string
		mutate func(map[string]interface{}, *ProofObservation)
	}{
		{"card payload", func(p map[string]interface{}, _ *ProofObservation) {
			p["card"].(map[string]interface{})["body"].([]interface{})[0].(map[string]interface{})["text"] = "tampered"
		}},
		{"plain", func(p map[string]interface{}, _ *ProofObservation) { p["plain"] = "tampered" }},
		{"proof provider", func(p map[string]interface{}, _ *ProofObservation) {
			p[ProofField].(map[string]interface{})["provider"] = "docs"
		}},
		{"proof resource", func(p map[string]interface{}, _ *ProofObservation) {
			p[ProofField].(map[string]interface{})["resource"].(map[string]interface{})["revision"] = "rev-4"
		}},
		{"proof delivery id", func(p map[string]interface{}, _ *ProofObservation) {
			p[ProofField].(map[string]interface{})["delivery_id"] = strings.Repeat("b", 64)
		}},
		{"observed actor", func(_ map[string]interface{}, o *ProofObservation) { o.ActorUID = "attacker" }},
		{"observed space", func(_ map[string]interface{}, o *ProofObservation) { o.SpaceID = "space-b" }},
		{"observed target", func(_ map[string]interface{}, o *ProofObservation) { o.Target.ShortID = "topic-2" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := cloneProofPayload(t, sealed)
			observation := validObservation
			tt.mutate(payload, &observation)
			assert.ErrorIs(t, verifier.Verify(payload, observation), ErrProofInvalid)
		})
	}
}

func TestProofSealRejectsUnreviewedEnvelopeFieldsAndInteractiveProfile(t *testing.T) {
	signer, _, _ := newProofHarness(t)
	ctx := validProofContext(Target{Kind: TargetGroup, GroupNo: "group-a"})

	tests := []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{"caller proof", func(p map[string]interface{}) { p[ProofField] = map[string]interface{}{} }},
		{"mention metadata", func(p map[string]interface{}) { p["mention"] = map[string]interface{}{"all": 1} }},
		{"OBO metadata", func(p map[string]interface{}) { p["__obo_grantor_uid"] = "user-x" }},
		{"interactive profile", func(p map[string]interface{}) { p["profile"] = cardmsg.ProfileV2 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := proofPayload()
			tt.mutate(payload)
			_, err := signer.Seal(payload, ctx)
			assert.ErrorIs(t, err, ErrProofInvalid)
		})
	}
}

func TestProofVerify_MissingOrMalformedProofFailsClosed(t *testing.T) {
	_, verifier, _ := newProofHarness(t)
	observation := ProofObservation{ActorUID: "user-a", SpaceID: "space-a", Target: Target{Kind: TargetGroup, GroupNo: "group-a"}}

	payload := proofPayload()
	assert.ErrorIs(t, verifier.Verify(payload, observation), ErrProofMissing)

	payload[ProofField] = map[string]interface{}{"v": 1}
	assert.ErrorIs(t, verifier.Verify(payload, observation), ErrProofInvalid)
}

func TestProofVerifier_KeyRotationOverlap(t *testing.T) {
	oldKey := newIntentTestKey(t)
	newKey := newIntentTestKey(t)
	oldSigner, err := NewProofSigner(ProofSigningKey{KeyID: "proof-old", PrivateKey: oldKey})
	require.NoError(t, err)
	sealed, err := oldSigner.Seal(proofPayload(), validProofContext(Target{Kind: TargetGroup, GroupNo: "group-a"}))
	require.NoError(t, err)
	observation := ProofObservation{ActorUID: "user-a", SpaceID: "space-a", Target: Target{Kind: TargetGroup, GroupNo: "group-a"}}

	overlap, err := NewProofVerifier([]ProofVerificationKey{
		{KeyID: "proof-old", PublicKey: &oldKey.PublicKey},
		{KeyID: "proof-new", PublicKey: &newKey.PublicKey},
	})
	require.NoError(t, err)
	require.NoError(t, overlap.Verify(sealed, observation))
	assert.Len(t, overlap.JWKS().Keys, 2)

	newOnly, err := NewProofVerifier([]ProofVerificationKey{{KeyID: "proof-new", PublicKey: &newKey.PublicKey}})
	require.NoError(t, err)
	assert.ErrorIs(t, newOnly.Verify(sealed, observation), ErrProofInvalid)
}

func TestProofKeyConfigurationFailsClosed(t *testing.T) {
	key := newIntentTestKey(t)
	_, err := NewProofSigner(ProofSigningKey{})
	assert.ErrorIs(t, err, ErrProofConfig)
	_, err = NewProofSigner(ProofSigningKey{KeyID: "bad/key", PrivateKey: key})
	assert.ErrorIs(t, err, ErrProofConfig)

	_, err = NewProofVerifier(nil)
	assert.ErrorIs(t, err, ErrProofConfig)
	_, err = NewProofVerifier([]ProofVerificationKey{
		{KeyID: "duplicate", PublicKey: &key.PublicKey},
		{KeyID: "duplicate", PublicKey: &key.PublicKey},
	})
	assert.ErrorIs(t, err, ErrProofConfig)
}

type proofVector struct {
	PublicJWK       jose.JSONWebKey        `json:"public_jwk"`
	UnsignedPayload map[string]interface{} `json:"unsigned_payload"`
	Context         ProofContext           `json:"context"`
	Observation     ProofObservation       `json:"observation"`
	SealedPayload   map[string]interface{} `json:"sealed_payload"`
	CanonicalInput  string                 `json:"canonical_signing_input"`
}

func TestProofConformanceVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/share_proof_v1.json")
	require.NoError(t, err)
	var vector proofVector
	require.NoError(t, json.Unmarshal(raw, &vector))

	verifier, err := NewProofVerifier([]ProofVerificationKey{{
		KeyID:     vector.PublicJWK.KeyID,
		PublicKey: vector.PublicJWK.Key,
	}})
	require.NoError(t, err)
	require.NoError(t, verifier.Verify(vector.SealedPayload, vector.Observation))

	proof, unsigned, err := splitSealedPayload(vector.SealedPayload)
	require.NoError(t, err)
	canonical, err := canonicalProofInput(unsigned, proof.metadata())
	require.NoError(t, err)
	assert.JSONEq(t, vector.CanonicalInput, string(canonical))
}
