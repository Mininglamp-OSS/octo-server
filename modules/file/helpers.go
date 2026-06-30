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

// ossNormalizeObjectKey returns the canonical OSS object key for an input
// `objectPath` that may carry a leading `/` and / or a leading
// `<bucketName>/` segment. Pure form of `ServiceOSS.normalizeOSSObjectKey`
// — broken out so unit tests can exercise the bucket-name-equals-prefix
// asymmetry path without a config context.
//
// Behavior:
//
//	bucketName="my-bucket", objectPath="my-bucket/chat/x.png" → "chat/x.png"
//	bucketName="my-bucket", objectPath="/my-bucket/chat/x.png" → "chat/x.png"
//	bucketName="chat",      objectPath="chat/2025/x.png"     → "2025/x.png"
//	bucketName="chat",      objectPath="/chat/2025/x.png"    → "2025/x.png"
//	bucketName="other",     objectPath="chat/2025/x.png"     → "chat/2025/x.png"
//
// Note the bucket-name-equals-prefix case (third row): the file API at
// `modules/file/api.go` emits `<fileType>/<...>` where `fileType` is the
// query string `type` (`chat`, `moment`, etc.). When a deployer's OSS
// bucket happens to be named `chat`, the prefix gets stripped — both
// `UploadFile` and `PresignedPutURL` apply the same rule so the two
// upload paths land at the same OSS key for the same logical input.
func ossNormalizeObjectKey(bucketName, objectPath string) string {
	objectPath = strings.TrimPrefix(objectPath, "/")
	prefix := bucketName + "/"
	return strings.TrimPrefix(objectPath, prefix)
}

// violatesStickerKeyspace reports whether a NON-sticker upload to objectPath
// would land in the sticker object keyspace (`sticker/…`), which the sticker
// module exclusively owns.
//
// Why this guard exists (cross-type overwrite, PR#509 review): a custom-sticker
// object lives at key `sticker/{uid}/{uuid}.ext` and is bound to an HMAC upload
// handle over (uid, public URL). On an OSS backend whose `BucketName` equals an
// upload type's prefix (e.g. `chat` — a documented, supported config; see
// ossNormalizeObjectKey), `ossNormalizeObjectKey` strips the leading `<bucket>/`
// segment on PUT. So a `type=chat` upload to `path=/sticker/{uid}/{uuid}.ext`
// stores `chat/sticker/{uid}/{uuid}.ext` → canonicalizes to the SAME key as the
// legitimate sticker — overwriting it with content that passed NONE of the
// type=sticker gates (1MB cap / magic number / raster-only / 512² dimension),
// while the already-minted handle (bound to the unchanged URL) still verifies.
// Reserving the keyspace at the upload boundary closes that overwrite on every
// backend; on backends that do not strip a prefix a non-sticker key never starts
// with `sticker/`, so this is a no-op there. Sticker uploads themselves are
// exempt (they ARE the owner). Match is the literal lowercase `sticker/` — the
// only form the sticker module and validateStickerPath ever produce/accept.
func violatesStickerKeyspace(fileType Type, objectPath string) bool {
	if fileType == TypeSticker {
		return false
	}
	return strings.HasPrefix(strings.TrimPrefix(objectPath, "/"), "sticker/")
}
