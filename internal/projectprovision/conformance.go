package projectprovision

import "crypto/subtle"

// Conformance vectors for a subsystem's `ensure` endpoint.
//
// # Why these exist
//
// The receiver owes three things this repository cannot enforce (see the package
// comment): verify the signature, reject a stale timestamp, and treat `container_id` as
// the idempotency key. Two of them are checkable from the outside with fixed inputs, and
// before these vectors existed they were prose in a design document that each of two
// separate codebases would reimplement from scratch. A reimplementation that gets the
// canonical string wrong fails CLOSED and is therefore self-announcing; one that is
// lenient about the timestamp fails OPEN and is silent — which is the clause that
// actually matters, and the one nothing here can detect.
//
// Go `internal/` packages are not importable from another repository, so a receiver
// cannot call this code. That is deliberate and is why the vectors are literal data:
// copy the four rows into the receiver's own test. The Go side exists so the literals
// cannot drift away from the implementation that produces them —
// TestConformanceVectorsMatchTheImplementation recomputes every signature and fails if
// this table and Sign ever disagree. A published vector that no longer matches the code
// is worse than none.
//
// # The canonical string, spelled out
//
// Six fields joined by a single "\n" (0x0A), no trailing newline:
//
//	v1
//	POST
//	<url path, percent-encoded, no query>
//	<X-Octo-Timestamp, unix seconds, decimal>
//	<X-Octo-Event-ID>
//	<lowercase hex of sha256(raw request body bytes)>
//
// `X-Octo-Signature` is `"v1=" + lowercase_hex(HMAC_SHA256(secret, canonical))`, compared
// in constant time. `X-Octo-Event-ID` is `lowercase_hex(sha256(container_id))` — the hash,
// not the id, because the id is a capability and headers are logged far more often than
// bodies. The ensure URL never carries a query string (ValidateTarget refuses one), so
// the path field is unambiguous.
//
// # What is deliberately NOT required
//
// The receiver is NOT asked to dedupe requests it has already seen, and the reason is a
// correctness one rather than leniency: octo-server retries the same row, and two
// deliveries that land in the same second carry the same timestamp and therefore the same
// signature, so a receiver refusing an identical repeat would refuse a LEGITIMATE retry
// and burn an attempt. The timestamp window is the bound instead.
//
// The residual risk is stated rather than papered over: inside the skew window, a captured
// request can be replayed, and because ensure is idempotent the effect is that one
// specific container — one octo-server itself asked to create — is recreated after the
// subsystem reclaimed it. It cannot create a container for another project, cannot change
// any field (the body is inside the MAC), and discloses nothing the captor did not already
// hold. Keep the window small (minutes, not hours) and that is the whole exposure.

// ConformanceVector is one fixed request plus the verdict the receiver must reach.
type ConformanceVector struct {
	// Name is stable; quote it in the receiver's test so a failure is greppable across
	// repositories.
	Name string
	// Secret is the per-target HMAC secret the receiver should be configured with for
	// this vector. Note that WrongSecret's signature was produced with a DIFFERENT
	// secret — the receiver still uses Secret and must reject.
	Secret string
	// Method / Path / Timestamp / EventID / Body are the request as it arrives on the
	// wire. Body is the exact byte sequence; do not reformat the JSON.
	Method    string
	Path      string
	Timestamp string
	EventID   string
	Body      string
	// Signature is the X-Octo-Signature header value as sent.
	Signature string
	// NowUnix is the receiver's clock when the request arrives. With
	// MaxSkewSeconds as the configured window, MustAccept is the required verdict.
	NowUnix int64
	// MustAccept is the verdict. False means the request must be refused — with a 4xx,
	// and without creating or touching any container.
	MustAccept bool
	// Why records which MUST clause the vector exercises.
	Why string
}

// MaxSkewSeconds is the timestamp window these vectors assume. A receiver may choose a
// smaller one; anything much larger widens the replay exposure described above.
const MaxSkewSeconds = 300

// Fixed inputs shared by the vectors. Exported so a receiver can assert it is configured
// with the same secret while running them.
const (
	conformanceSecret      = "conformance-secret-0123456789abcd"
	conformanceOtherSecret = "conformance-secret-DIFFERENT-0123"
	conformancePath        = "/api/internal/workspaces/ensure"
	conformanceContainerID = "octows-00112233445566778899aabbccddeeff"
	conformanceEventID     = "6e51885c4b2a56b5e9f8cfca79b1f90867f6bb93ab15188361b6d1d1d359a2fc"
	conformanceBody        = `{"container_id":"octows-00112233445566778899aabbccddeeff","project_id":"p-conformance","octo_space_id":"s-conformance","name":"octo-project"}`
	// conformanceTamperedBody differs from conformanceBody in project_id only, and is
	// sent under the signature computed over the ORIGINAL body.
	conformanceTamperedBody = `{"container_id":"octows-00112233445566778899aabbccddeeff","project_id":"p-attacker000","octo_space_id":"s-conformance","name":"octo-project"}`
	// conformanceNow is a fixed wall clock (2025-09-07T08:00:00Z) so the vectors do not
	// depend on when they are run. The value is what the signatures below were computed
	// over, so it must not be "corrected" to the current year — TestConformanceEpochLabel
	// pins the label against the constant instead.
	conformanceNow = int64(1757232000)
)

// ConformanceVectors returns the four vectors, in the order a receiver should run them.
func ConformanceVectors() []ConformanceVector {
	return []ConformanceVector{
		{
			Name:       "valid",
			Secret:     conformanceSecret,
			Method:     "POST",
			Path:       conformancePath,
			Timestamp:  "1757232000",
			EventID:    conformanceEventID,
			Body:       conformanceBody,
			Signature:  "v1=479bd366d66909e2868508af3d50284d9056d2657cc75be1b68668dc2389b4dc",
			NowUnix:    conformanceNow,
			MustAccept: true,
			Why:        "baseline: a well-formed, fresh, correctly signed ensure must create the container",
		},
		{
			Name:      "stale_timestamp",
			Secret:    conformanceSecret,
			Method:    "POST",
			Path:      conformancePath,
			Timestamp: "1757228400", // one hour before NowUnix
			EventID:   conformanceEventID,
			Body:      conformanceBody,
			Signature: "v1=456c16db4fb5d67c733cd3dd63eb3e122fc70df3d5fd698db3d1763aa8cfabe4",
			NowUnix:   conformanceNow,
			// The signature is VALID. This vector exists because MAC verification alone
			// accepts it, and it is the only clause whose absence is silent: a receiver
			// that skips freshness passes every other vector.
			MustAccept: false,
			Why:        "freshness: a correctly signed request outside the skew window must be refused",
		},
		{
			Name:      "tampered_body",
			Secret:    conformanceSecret,
			Method:    "POST",
			Path:      conformancePath,
			Timestamp: "1757232000",
			EventID:   conformanceEventID,
			// project_id was rewritten in transit; the signature is the one computed over
			// the original body. A receiver that hashes the body it parsed rather than the
			// raw bytes it received can pass this by accident, which is why the body is
			// specified as exact bytes.
			Body:       conformanceTamperedBody,
			Signature:  "v1=479bd366d66909e2868508af3d50284d9056d2657cc75be1b68668dc2389b4dc",
			NowUnix:    conformanceNow,
			MustAccept: false,
			Why:        "integrity: the body is inside the MAC, so an in-transit edit must be refused",
		},
		{
			Name:      "wrong_secret",
			Secret:    conformanceSecret,
			Method:    "POST",
			Path:      conformancePath,
			Timestamp: "1757232000",
			EventID:   conformanceEventID,
			Body:      conformanceBody,
			// Signed with conformanceOtherSecret while the receiver holds
			// conformanceSecret.
			Signature:  "v1=d7eec8ce4bf69a373795574bd1c7f9a6f0d3442884b205f87b156e2964b59ffe",
			NowUnix:    conformanceNow,
			MustAccept: false,
			Why:        "authentication: a signature from any other key must be refused",
		},
	}
}

// isPublishedConformanceSecret reports whether s is one of the secrets this file publishes.
//
// Kept next to the literals it guards rather than in client.go, so adding a fifth vector
// with a new secret cannot leave the check behind: whoever adds the constant is editing
// this file.
func isPublishedConformanceSecret(s string) bool {
	for _, published := range []string{conformanceSecret, conformanceOtherSecret} {
		if subtle.ConstantTimeCompare([]byte(s), []byte(published)) == 1 {
			return true
		}
	}
	return false
}
