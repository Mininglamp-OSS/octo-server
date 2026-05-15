package file

import "testing"

func TestSplitBucketAndObject(t *testing.T) {
	allowed := map[string]bool{
		"chat": true,
		"file": true,
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
