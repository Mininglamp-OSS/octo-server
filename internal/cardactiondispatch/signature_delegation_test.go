package cardactiondispatch

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/octosign"
)

// TestSignatureDelegationDoesNotDrift pins this package's wrappers to pkg/octosign.
//
// The primitive moved to a leaf package to break an import cycle (see signature.go), and
// the whole reason it moved rather than being copied is that two subsystems must verify ONE
// canonical string. A wrapper that quietly grew its own behaviour would reintroduce exactly
// the drift the move avoided — and it would do so silently, because both sides would still
// verify against themselves.
func TestSignatureDelegationDoesNotDrift(t *testing.T) {
	const (
		secret = "0123456789abcdef0123456789abcdef"
		method = "post" // lower case on purpose: the canonical string upper-cases it
		path   = "/api/internal/workspaces/ensure"
		ts     = "1757232000"
		event  = "6e51885c4b2a56b5e9f8cfca79b1f90867f6bb93ab15188361b6d1d1d359a2fc"
	)
	body := []byte(`{"container_id":"octows-deadbeef"}`)

	if got, want := CanonicalRequest(method, path, ts, event, body),
		octosign.CanonicalRequest(method, path, ts, event, body); got != want {
		t.Errorf("CanonicalRequest drifted:\n have %q\n want %q", got, want)
	}
	sig := Sign(secret, method, path, ts, event, body)
	if want := octosign.Sign(secret, method, path, ts, event, body); sig != want {
		t.Errorf("Sign drifted:\n have %s\n want %s", sig, want)
	}
	if !octosign.Verify(secret, sig, method, path, ts, event, body) {
		t.Error("a signature from this package does not verify with pkg/octosign")
	}
	if !Verify(secret, octosign.Sign(secret, method, path, ts, event, body), method, path, ts, event, body) {
		t.Error("a signature from pkg/octosign does not verify with this package")
	}
	// And the version prefix is the shared one, since it is both the header prefix and the
	// canonical string's first line.
	if signatureVersion != octosign.Version {
		t.Errorf("signatureVersion = %q, octosign.Version = %q", signatureVersion, octosign.Version)
	}
	// A wrong secret must still fail, or every assertion above could be vacuous.
	if Verify(secret+"x", sig, method, path, ts, event, body) {
		t.Error("Verify accepted a signature under the wrong secret")
	}
}
