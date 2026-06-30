package sticker

import "testing"

// TestValidateStickerPath is a no-DB unit test for the registration-path guard.
// The accepted shape is "sticker/{loginUID}/<name>.<ext>" anywhere in the key
// tail, ext == the declared format and in the raster whitelist; everything else
// (other user, non-sticker bucket, external object, ext/format mismatch, nested
// segment, missing extension) is rejected. See validateStickerPath (PR#508).
func TestValidateStickerPath(t *testing.T) {
	const uid = "10000"

	cases := []struct {
		name   string
		path   string
		format string
		want   bool
	}{
		// --- accepted ---
		{"relative preview key", "file/preview/sticker/10000/abc.png", "png", true},
		{"absolute download url", "https://cdn.example.com/bucket/sticker/10000/abc.gif", "gif", true},
		{"absolute url with signing query", "https://s3.example.com/b/sticker/10000/x.webp?X-Amz-Signature=ab", "webp", true},
		{"path-style minio", "http://127.0.0.1:9000/dm/sticker/10000/u.jpg", "jpg", true},
		{"jpeg ext and format", "file/preview/sticker/10000/u.jpeg", "jpeg", true},
		{"uppercase ext normalizes", "file/preview/sticker/10000/U.PNG", "png", true},

		// --- rejected ---
		{"other user", "file/preview/sticker/99999/x.gif", "gif", false},
		{"non-sticker bucket", "file/preview/chat/10000/x.gif", "gif", false},
		{"external non-sticker", "https://evil.example.com/avatar/x.gif", "gif", false},
		{"ext contradicts format", "file/preview/sticker/10000/x.png", "gif", false},
		{"nested extra segment", "file/preview/sticker/10000/sub/x.gif", "gif", false},
		{"no extension", "file/preview/sticker/10000/x", "gif", false},
		{"missing uid segment", "file/preview/sticker/x.gif", "gif", false},
		{"empty path", "", "gif", false},
		{"uid as substring not segment", "file/preview/sticker/100009/x.gif", "gif", false},
		{"disallowed ext even if matches", "file/preview/sticker/10000/x.tiff", "tiff", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validateStickerPath(tc.path, uid, tc.format)
			if got != tc.want {
				t.Fatalf("validateStickerPath(%q, %q, %q) = %v, want %v", tc.path, uid, tc.format, got, tc.want)
			}
		})
	}
}
