package stickersig

import "testing"

const testMasterKey = "0123456789abcdef0123456789abcdef" // 32 bytes

func TestSignVerify_RoundTrip(t *testing.T) {
	t.Setenv(masterKeyEnv, testMasterKey)

	h, ok := Sign("10000", "file/preview/sticker/10000/abc.png")
	if !ok {
		t.Fatal("Sign returned ok=false with a master key set")
	}
	if h == "" {
		t.Fatal("Sign returned an empty handle")
	}
	if !Verify("10000", "file/preview/sticker/10000/abc.png", h) {
		t.Fatal("Verify rejected a handle it just signed")
	}
}

func TestVerify_RejectsTamperedFields(t *testing.T) {
	t.Setenv(masterKeyEnv, testMasterKey)

	const uid = "10000"
	const path = "file/preview/sticker/10000/abc.png"
	h, _ := Sign(uid, path)

	cases := []struct {
		name      string
		uid, path string
	}{
		{"different uid", "99999", path},
		{"different path", uid, "file/preview/sticker/10000/other.png"},
		{"path with appended query", uid, path + "?x=1"},
		{"empty uid", "", path},
		{"empty path", uid, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if Verify(tc.uid, tc.path, h) {
				t.Fatalf("Verify accepted a handle bound to different fields (%s)", tc.name)
			}
		})
	}
}

func TestVerify_RejectsMalformedAndEmptyHandle(t *testing.T) {
	t.Setenv(masterKeyEnv, testMasterKey)

	const uid = "10000"
	const path = "file/preview/sticker/10000/abc.png"
	for _, h := range []string{"", "   ", "not!base64!", "AAAA"} {
		if Verify(uid, path, h) {
			t.Fatalf("Verify accepted a malformed/empty handle %q", h)
		}
	}
}

// Field-boundary collision guard: length-prefixing must make ("ab","c") and
// ("a","bc") produce distinct MACs, so a handle for one can't validate the
// other. A naive separator-join would collide here.
func TestVerify_NoFieldBoundaryCollision(t *testing.T) {
	t.Setenv(masterKeyEnv, testMasterKey)

	h, _ := Sign("ab", "c")
	if Verify("a", "bc", h) {
		t.Fatal("field boundary collision: ('a','bc') validated a handle for ('ab','c')")
	}
}

func TestDisabled_WhenNoMasterKey(t *testing.T) {
	t.Setenv(masterKeyEnv, "")

	if Enabled() {
		t.Fatal("Enabled() is true with no master key")
	}
	if _, ok := Sign("10000", "p"); ok {
		t.Fatal("Sign returned ok=true with no master key")
	}
	if Verify("10000", "p", "anything") {
		t.Fatal("Verify accepted a handle with no master key configured")
	}
}

// A master key that is not exactly 32 bytes is treated as unconfigured, mirroring
// modules/common's exact-32 contract (key_encryption.go). A short value would
// yield a low-entropy HMAC subkey, so we disable (fall back to the shape check)
// rather than mint a brute-forceable handle; an over-length value is likewise
// rejected so there is a single definition of a valid OCTO_MASTER_KEY.
func TestDisabled_WhenMasterKeyWrongLength(t *testing.T) {
	cases := map[string]string{
		"too short": "short",
		"31 bytes":  "0123456789abcdef0123456789abcde",   // one short of 32
		"33 bytes":  "0123456789abcdef0123456789abcdef0", // one over 32
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(masterKeyEnv, key)
			if Enabled() {
				t.Fatalf("Enabled() is true for a %d-byte key", len(key))
			}
			if _, ok := Sign("10000", "file/preview/sticker/10000/abc.png"); ok {
				t.Fatal("Sign returned ok=true for a wrong-length key")
			}
			if Verify("10000", "file/preview/sticker/10000/abc.png", "anything") {
				t.Fatal("Verify accepted a handle with a wrong-length key")
			}
		})
	}
}

// A handle minted under one master key must not verify under another (e.g. after
// a key rotation), confirming the subkey actually binds to OCTO_MASTER_KEY.
func TestVerify_RejectsHandleFromDifferentKey(t *testing.T) {
	t.Setenv(masterKeyEnv, testMasterKey)
	h, _ := Sign("10000", "file/preview/sticker/10000/abc.png")

	t.Setenv(masterKeyEnv, "fedcba9876543210fedcba9876543210")
	if Verify("10000", "file/preview/sticker/10000/abc.png", h) {
		t.Fatal("a handle verified under a different master key")
	}
}
