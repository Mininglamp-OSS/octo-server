package sticker

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

// TestMain ensures OCTO_MASTER_KEY is set before any test boots. common.Setup
// (called transitively from module.Setup) refuses to start without a 32-byte
// master key used to encrypt the IM private key. Mirrors modules/category and
// modules/user main_test.go: don't overwrite an externally-provided key so
// CI / dev shells can pin one.
func TestMain(m *testing.M) {
	if os.Getenv("OCTO_MASTER_KEY") == "" {
		key := make([]byte, 16)
		_, _ = rand.Read(key)
		_ = os.Setenv("OCTO_MASTER_KEY", hex.EncodeToString(key))
	}
	os.Exit(m.Run())
}
