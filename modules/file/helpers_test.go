package file

import "testing"

func TestSplitBucketAndObject(t *testing.T) {
	allowed := map[string]bool{
		"chat":     true,
		"file":     true,
		"download": true,
	}

	cases := []struct {
		name           string
		input          string
		defaultBucket  string
		allowed        map[string]bool
		expectedBucket string
		expectedObject string
	}{
		{
			name:           "bucket prefix in allow-list",
			input:          "chat/2024/01/foo.png",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "chat",
			expectedObject: "2024/01/foo.png",
		},
		{
			name:           "leading slash is tolerated",
			input:          "/chat/2024/foo.png",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "chat",
			expectedObject: "2024/foo.png",
		},
		{
			name:           "missing slash returns default bucket",
			input:          "loose-name.png",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "file",
			expectedObject: "loose-name.png",
		},
		{
			name:           "empty input returns default bucket and empty object",
			input:          "",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "file",
			expectedObject: "",
		},
		{
			name:           "leading slash with no body returns default bucket",
			input:          "/",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "file",
			expectedObject: "",
		},
		{
			name:           "first segment not in allow-list falls back to default",
			input:          "evil/2024/foo.png",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "file",
			expectedObject: "evil/2024/foo.png",
		},
		{
			name:           "nil allow-list disables bucket extraction",
			input:          "chat/2024/foo.png",
			defaultBucket:  "default-bucket",
			allowed:        nil,
			expectedBucket: "default-bucket",
			expectedObject: "chat/2024/foo.png",
		},
		{
			name:           "trailing slash is preserved on object",
			input:          "chat/dir/",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "chat",
			expectedObject: "dir/",
		},
		// Boundary regression cases pinned during PR#50 R3 (codex 2.4).
		// Historical context: an earlier shape of this helper looked at
		// only the leading character and used a default bucket whenever
		// the input did not literally start with "<allowed>/". The
		// current shape tolerates a leading slash and treats single-
		// segment input as a bare object key against the default
		// bucket. The cases below pin those two shapes so a future
		// refactor cannot silently regress either one.
		{
			// Leading slash + allow-listed first segment: must split
			// into the allowed bucket and the rest of the path with
			// the slash already consumed. Same shape callers get when
			// they hand us a path sourced from Content-Disposition or
			// url.URL.Path without first stripping the leading slash.
			name:           "leading slash + short key resolves to allowed bucket",
			input:          "/chat/foo.png",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "chat",
			expectedObject: "foo.png",
		},
		{
			// Single-segment input must NOT be reinterpreted as a
			// bucket name even when the segment happens to match an
			// allow-list entry. There is no "<bucket>/<object>" split
			// to make, so the whole input is the object key against
			// the default bucket. (Without this guard, a request for
			// `/file/download` would be promoted to bucket=download,
			// key="" — the very shape commit 5 rejects up front.)
			name:           "single-segment input falls back to default bucket",
			input:          "download",
			defaultBucket:  "file",
			allowed:        allowed,
			expectedBucket: "file",
			expectedObject: "download",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bucket, object := splitBucketAndObject(tc.input, tc.defaultBucket, tc.allowed)
			if bucket != tc.expectedBucket {
				t.Errorf("bucket: got %q, want %q", bucket, tc.expectedBucket)
			}
			if object != tc.expectedObject {
				t.Errorf("object: got %q, want %q", object, tc.expectedObject)
			}
		})
	}
}

// TestOSSNormalizeObjectKey pins the canonical key derivation used by
// ServiceOSS.UploadFile / PresignedPutURL / PresignedGetURL.
// PR#50 R5 codex finding 2.4: the two upload paths must agree for the
// same logical input, especially in the bucket-name-equals-prefix case.
func TestOSSNormalizeObjectKey(t *testing.T) {
	cases := []struct {
		name       string
		bucketName string
		input      string
		want       string
	}{
		{
			name:       "no leading slash, no bucket prefix",
			bucketName: "my-bucket",
			input:      "chat/2025/x.png",
			want:       "chat/2025/x.png",
		},
		{
			name:       "leading slash stripped",
			bucketName: "my-bucket",
			input:      "/chat/2025/x.png",
			want:       "chat/2025/x.png",
		},
		{
			name:       "bucket prefix stripped",
			bucketName: "my-bucket",
			input:      "my-bucket/chat/2025/x.png",
			want:       "chat/2025/x.png",
		},
		{
			name:       "leading slash + bucket prefix both stripped",
			bucketName: "my-bucket",
			input:      "/my-bucket/chat/2025/x.png",
			want:       "chat/2025/x.png",
		},
		{
			// The asymmetry path that PR#50 R5 codex finding 2.4 calls
			// out: when the deployer's bucket name happens to match a
			// `fileType` prefix from modules/file/api.go (`chat`), the
			// helper strips it. Both UploadFile and PresignedPutURL
			// route through this helper now, so they land at the SAME
			// key (`2025/x.png`) — previously UploadFile kept the raw
			// `chat/2025/x.png` while PresignedPutURL stripped to
			// `2025/x.png`, and the two upload paths fragmented the
			// object namespace.
			name:       "bucket name equals fileType prefix (chat)",
			bucketName: "chat",
			input:      "chat/2025/x.png",
			want:       "2025/x.png",
		},
		{
			name:       "bucket name equals fileType prefix with leading slash",
			bucketName: "chat",
			input:      "/chat/2025/x.png",
			want:       "2025/x.png",
		},
		{
			// Bucket name that is a strict prefix of the first segment
			// (but not equal to it) must NOT be stripped.
			name:       "bucket name is non-segment prefix only",
			bucketName: "ch",
			input:      "chat/2025/x.png",
			want:       "chat/2025/x.png",
		},
		{
			name:       "empty input",
			bucketName: "my-bucket",
			input:      "",
			want:       "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ossNormalizeObjectKey(tc.bucketName, tc.input)
			if got != tc.want {
				t.Errorf("ossNormalizeObjectKey(%q, %q) = %q, want %q",
					tc.bucketName, tc.input, got, tc.want)
			}
		})
	}
}
