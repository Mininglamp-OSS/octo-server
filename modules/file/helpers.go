package file

import "strings"

// allowedMinioBuckets is the whitelist of bucket prefixes the MinIO backend
// will auto-create and accept on upload. Keeping it in one place lets every
// MinIO code path (UploadFile, GetFile, presigned URLs) agree on the same
// policy without drifting copies.
var allowedMinioBuckets = map[string]bool{
	"file":     true,
	"chat":     true,
	"moment":   true,
	"sticker":  true,
	"report":   true,
	"chatbg":   true,
	"common":   true,
	"download": true,
	"group":    true,
	"avatar":   true,
}

// splitBucketAndObject parses an object path of the form "<bucket>/<object>"
// into the bucket name and the remaining object key. The first segment is
// treated as the bucket only when it is in the allow-list (`allowed`); any
// other shape — leading slash, missing slash, single segment, empty string —
// falls back to the default bucket and keeps the full input as the object
// key.
//
// The leading slash is tolerated so callers can pass paths sourced from
// Content-Disposition or URL parsing without first having to normalize them.
func splitBucketAndObject(objectPath string, defaultBucket string, allowed map[string]bool) (bucket string, object string) {
	trimmed := strings.TrimPrefix(objectPath, "/")
	if trimmed == "" {
		return defaultBucket, ""
	}
	idx := strings.Index(trimmed, "/")
	if idx <= 0 {
		// No slash, or the whole input is one segment — there is no
		// "<bucket>/<object>" split to make. Hand the whole thing back as
		// the object key against the default bucket.
		return defaultBucket, trimmed
	}
	first := trimmed[:idx]
	rest := trimmed[idx+1:]
	// A nil or empty allow-list means "no buckets are whitelisted" — fall
	// back to the default bucket. This matches the safest reading of the
	// existing MinIO bucket-creation policy: never trust the first path
	// segment as a bucket name unless it is on the explicit allow-list.
	if len(allowed) == 0 || !allowed[first] {
		return defaultBucket, trimmed
	}
	return first, rest
}
