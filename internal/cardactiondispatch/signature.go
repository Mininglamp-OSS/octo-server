package cardactiondispatch

import "github.com/Mininglamp-OSS/octo-server/pkg/octosign"

// The v1 signing primitive moved to pkg/octosign, a stdlib-only leaf package. These
// wrappers stay so every caller and test in this package is unchanged.
//
// The move was forced rather than cosmetic: internal/projectprovision reuses this scheme
// (one canonical string for every subsystem that verifies octo-server's signature), and
// once P1 made modules/group depend on modules/project that reuse closed an import cycle
// through pkg/cardtmpl → internal/carddispatch → modules/group. Copying the implementation
// into the second caller would have removed the cycle and reintroduced the risk the reuse
// existed to avoid — two canonical strings drifting apart, now that conformance vectors
// derived from this code are copied into other repositories.

const signatureVersion = octosign.Version

func CanonicalRequest(method, path, timestamp, eventID string, body []byte) string {
	return octosign.CanonicalRequest(method, path, timestamp, eventID, body)
}

func Sign(secret, method, path, timestamp, eventID string, body []byte) string {
	return octosign.Sign(secret, method, path, timestamp, eventID, body)
}

func Verify(secret, signature, method, path, timestamp, eventID string, body []byte) bool {
	return octosign.Verify(secret, signature, method, path, timestamp, eventID, body)
}
