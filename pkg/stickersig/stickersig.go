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
//
// # Capability vs enforcement policy
//
// Enabled() reports the server CAPABILITY to mint/verify handles (i.e. whether a
// usable OCTO_MASTER_KEY is present). It must NOT be conflated with the
// enforcement POLICY of whether sticker registration REQUIRES a handle — because
// OCTO_MASTER_KEY is a mandatory production contract (modules/common also needs
// it to encrypt the IM RSA private key at rest), so Enabled() is effectively
// always true in production. Tying enforcement to Enabled() would silently flip
// the sticker-registration protocol the moment a master key exists and break
// older clients that do not yet send a handle.
//
// The enforcement policy is therefore a SEPARATE, independent switch that lives
// OUTSIDE this leaf package: the DB-backed system_setting `sticker.handle_required`
// (modules/common SystemSettings.StickerHandleRequired, default false), so it can
// be toggled and rolled back from the admin console without a redeploy. This
// package only exposes the capability (Enabled) and the sign/verify primitives;
// the two are deliberately orthogonal and are never derived from one another.
//
// # Two-step client flow
//
//  1. Upload the image: POST /v1/file/upload?type=sticker → response carries
//     `path` and (when Enabled) `sticker_handle`.
//  2. Register the sticker: POST /v1/sticker/user with `path` and pass the
//     `sticker_handle` value as the `handle` field.
//
// Stickers do NOT support presigned uploads: the handle can only be minted at the
// point modules/file has both the authenticated uploader and the content-validated
// bytes, so the image must transit the multipart upload endpoint.
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
