package oidc

import (
	"testing"
	"time"
)

// Adversarial: the predicate the commit claims ("this vendor's access_token is opaque,
// so a JWT-shaped value can never be a legitimate credential here") is a vendor fact
// that does NOT depend on whether our bearer verifier is configured. But
// UnverifiableJWTMustNotBeForwarded only fires when verifierConfigured==false. When the
// secret IS configured (the "fixed" state), a JWT-shaped credential that simply fails our
// HMAC (wrong/rotated secret, or attacker-forged) still falls through IsForeignToken and
// gets forwarded to /userinfo.
func TestAdversarial_ConfiguredSecretStillForwardsForeignJWTShaped(t *testing.T) {
	m := newMockOAuth2Provider(t)
	prov, err := newOAuth2Provider(m.providerConfig())
	if err != nil {
		t.Fatalf("newOAuth2Provider: %v", err)
	}
	o := newOAuth2ExchangeTestOIDC(t, prov)
	o.ownCred = newDetector(&fakeTokenReader{})

	// Secret IS configured correctly this time.
	secret := []byte("our-configured-secret-32-bytes!!")
	o.bearerJWT = newBearerJWTVerifierForTest(secret, prov.Issuer()+bearerJWTIssuerSuffix)

	// Token is JWT-shaped, HS256, but signed under a DIFFERENT secret (e.g. rotated-out
	// secret, or attacker junk). HMAC will not match -> ErrJWTForeign -> falls through.
	wrongSecret := []byte("a-different-secret-also-32-bytes")
	tok := signBearerTesting(t, wrongSecret, 2200099, "desk.user", time.Now().Add(15*24*time.Hour))

	m.mu.Lock()
	m.LastUserInfoRequest = nil
	m.mu.Unlock()
	postExchange(o, `{"access_token":"`+tok+`"}`)

	m.mu.Lock()
	last := m.LastUserInfoRequest
	m.mu.Unlock()
	if last != nil {
		t.Errorf("PROVEN: with the bearer verifier CONFIGURED, a JWT-shaped credential "+
			"that merely fails HMAC (rotated/foreign secret) was still forwarded to the "+
			"upstream IdP (userinfo query=%q). The vendor's access_token is opaque per the "+
			"commit's own stated invariant, so this JWT-shaped value could never succeed "+
			"there either -- the guard was gated on verifierConfigured==false instead of on "+
			"the actual predicate (OpaqueClientCredential alone).", last.Query.Encode())
	}
}
