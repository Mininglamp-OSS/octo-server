// Package stickersig mints and verifies the short-lived "upload handle" that
// proves a custom-sticker object was produced by a specific user's
// content-validated upload.
//
// # Why this exists
//
// A custom sticker is registered (POST /v1/sticker/user) by handing the server
// a `path` that a prior multipart upload (GET/POST /v1/file/upload?type=sticker)
// returned. The sticker module cannot, from the path string alone, prove that
// the object behind it really went through the type=sticker upload gate (1MB
// cap + magic-number check + raster-only whitelist) rather than some looser path
// (e.g. type=chat at 100MB), nor that THIS caller is the uploader. The pragmatic
// object-key shape check (sticker.validateStickerPath) is a best-effort prefix
// match and, by design, accepts any URL carrying a ".../sticker/{uid}/x.ext"
// tail — including a chat-bucket object "chat/sticker/{uid}/x.ext".
//
// The handle closes that gap cryptographically: modules/file signs (uid, path)
// with an HMAC at upload time — i.e. only AFTER the bytes passed the
// type=sticker gate — and returns it; sticker.add verifies it. A client cannot
// forge a handle for an object it never uploaded, so the cross-type / size-cap
// bypass and the other-user / foreign-host cases are all refused regardless of
// the path's shape.
//
// # Key material
//
// The HMAC key is derived from OCTO_MASTER_KEY (the same 32-byte master key
// modules/common requires at boot) via one HMAC-SHA256 pass over a fixed
// domain-separation label, so the sticker-handle subkey is independent of every
// other use of the master key (e.g. common's AES-GCM key encryption): a handle
// can never be confused with — or forged from — another subsystem's MAC. When
// OCTO_MASTER_KEY is unset or not exactly 32 bytes, signing is disabled (Enabled
// reports false) and callers fall back to the non-cryptographic path-shape check
// — the same posture as before handles existed, so deployments without a master
// key are not regressed.
package stickersig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"hash"
	"os"
	"strings"
)

// masterKeyEnv mirrors modules/common.masterKeyEnv. It is duplicated here rather
// than imported because this is a leaf package (modules/file must depend on it
// without dragging in modules/common). The env var is a deployment contract.
const masterKeyEnv = "OCTO_MASTER_KEY"

// derivationLabel domain-separates the sticker-upload-handle subkey from every
// other use of OCTO_MASTER_KEY. The "/v1" suffix leaves room to rotate the
// scheme later; handles are consumed seconds after issue, so a bump simply
// invalidates any in-flight handle (the client re-uploads).
var derivationLabel = []byte("octo/sticker-upload-handle/v1")

// subkey derives the HMAC key from the master key, or returns nil when no usable
// master key is configured. A master key is usable only when it is exactly 32
// bytes — the same contract modules/common enforces for its AES-256-GCM key
// (key_encryption.go rejects len != 32). Mirroring it here keeps ONE definition
// of "valid OCTO_MASTER_KEY" across subsystems and stops a short, low-entropy
// value from minting brute-forceable handles: a wrong-length key is treated
// exactly like an unset one (signing disabled → callers fall back to the
// path-shape check). common validates lazily (on first encrypt/decrypt), so a
// deployment that sets a malformed key but never exercises key-encryption would
// otherwise reach this code with a weak key.
func subkey() []byte {
	master := os.Getenv(masterKeyEnv)
	if len(master) != 32 {
		return nil
	}
	mac := hmac.New(sha256.New, []byte(master))
	mac.Write(derivationLabel)
	return mac.Sum(nil)
}

// Enabled reports whether handle signing/verification is active, i.e. whether
// OCTO_MASTER_KEY is configured as a usable (exactly 32-byte) key. sticker.add
// uses this to decide between the cryptographic handle check (enabled) and the
// path-shape fallback (disabled).
func Enabled() bool {
	return subkey() != nil
}

// Sign returns a base64url upload handle binding the uploader uid to the stored
// object path. The second return is false when no master key is configured (the
// caller then omits the handle and the verifier falls back to the shape check).
func Sign(uid, path string) (string, bool) {
	key := subkey()
	if key == nil {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(compute(key, uid, path)), true
}

// Verify reports, in constant time, whether handle is a valid signature over
// (uid, path). It returns false when no master key is configured, the handle is
// empty, or the handle is malformed — never panics on attacker input.
func Verify(uid, path, handle string) bool {
	key := subkey()
	if key == nil || handle == "" {
		return false
	}
	got, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(handle))
	if err != nil {
		return false
	}
	return hmac.Equal(got, compute(key, uid, path))
}

// compute is the canonical MAC over the fields. Each field is length-prefixed
// (8-byte big-endian) so that ("a","bc") and ("ab","c") cannot produce the same
// input — a plain separator could collide if a field contained the separator.
func compute(key []byte, fields ...string) []byte {
	mac := hmac.New(sha256.New, key)
	for _, f := range fields {
		writeField(mac, f)
	}
	return mac.Sum(nil)
}

func writeField(h hash.Hash, s string) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write([]byte(s))
}
