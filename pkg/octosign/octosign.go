// Package octosign is the v1 request-signing primitive shared by octo-server's outbound
// service-to-service calls.
//
// It is a LEAF package — standard library only — and that is the reason it exists as a
// package at all rather than living next to its first caller.
//
// The implementation started in internal/cardactiondispatch, and internal/projectprovision
// reused it from there because two subsystems verifying octo-server's signature should not
// have to verify two different canonical strings. That reuse became an import cycle the
// moment P1 (#846) made modules/group depend on modules/project:
//
//	modules/group → modules/project → internal/projectprovision
//	  → internal/cardactiondispatch → pkg/cardtmpl → internal/carddispatch → modules/group
//
// The fix is not to copy the fifteen lines into the second caller. A second copy is how the
// two canonical strings drift apart, and this repository now ships CONFORMANCE VECTORS
// (internal/projectprovision/conformance.go) that other repositories copy — a drifted
// signature would send another team chasing a mismatch that is ours. So the primitive moved
// down to a leaf where anything may depend on it, and internal/cardactiondispatch delegates
// so every existing caller and test is untouched.
package octosign

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Version is the signature scheme version, and it is the first line of the canonical
// string as well as the prefix of the header value.
const Version = "v1"

// Header names carried by a signed request.
const (
	HeaderSignature = "X-Octo-Signature"
	HeaderTimestamp = "X-Octo-Timestamp"
	HeaderEventID   = "X-Octo-Event-ID"
)

// CanonicalRequest builds the exact string that gets signed: six fields joined by a single
// "\n", with no trailing newline.
//
//	v1
//	<METHOD, upper-case>
//	<url path, percent-encoded, no query>
//	<timestamp, unix seconds, decimal>
//	<event id>
//	<lowercase hex of sha256(raw body bytes)>
//
// The body is hashed rather than embedded so the string stays bounded, and the path carries
// no query — callers are expected to reject a query on a signed URL, since the query would
// otherwise travel outside the MAC.
func CanonicalRequest(method, path, timestamp, eventID string, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		Version,
		strings.ToUpper(method),
		path,
		timestamp,
		eventID,
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// Sign returns the header value: "v1=" followed by lowercase hex HMAC-SHA256.
func Sign(secret, method, path, timestamp, eventID string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(CanonicalRequest(method, path, timestamp, eventID, body)))
	return Version + "=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify compares a received signature against the expected one in constant time.
//
// It deliberately checks the MAC ONLY. Freshness is not verifiable from these inputs — it
// needs the receiver's clock and its chosen skew window — so a caller that cares about
// replay must check the timestamp itself. Callers have got this wrong before, which is why
// it is stated here rather than left to be inferred from the signature.
func Verify(secret, signature, method, path, timestamp, eventID string, body []byte) bool {
	want := Sign(secret, method, path, timestamp, eventID, body)
	return hmac.Equal([]byte(signature), []byte(want))
}
