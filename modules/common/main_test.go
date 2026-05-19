package common

import (
	"os"
	"testing"
)

// TestMain ensures OCTO_MASTER_KEY is set for tests that boot the full
// common module via testutil.NewTestServer — that path runs
// insertAppConfigIfNeed which encrypts the RSA private key on first run
// and panics if the master key is missing. CI sets the same value (see
// .github/workflows/ci.yml); this fallback keeps local runs ergonomic
// without leaking the production key into the source tree.
//
// Tests that specifically need the env unset (see key_encryption_test.go)
// must Unset+restore around their assertions; this defaults to set.
func TestMain(m *testing.M) {
	if os.Getenv(masterKeyEnv) == "" {
		_ = os.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	}
	os.Exit(m.Run())
}
