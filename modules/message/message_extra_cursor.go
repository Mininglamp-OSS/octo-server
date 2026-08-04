package message

import "strconv"

// parseCachedMessageExtraSyncCursor returns -1 for malformed cache data so the
// resolver treats it as poisoned and overwrites it with a normalized cursor.
func parseCachedMessageExtraSyncCursor(raw string) int64 {
	if raw == "" {
		return 0
	}
	version, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return -1
	}
	return version
}

// resolveMessageExtraSyncCursor preserves the existing monotonic cache behavior
// while keeping untrusted request/cache values inside the range the server has
// actually issued for this storage channel.
//
// A poisoned cached value is replaced by the normalized request rather than by
// issuedMax: the client may genuinely be behind, so replaying from its claimed
// position is safer than silently skipping to the channel high-water.
func resolveMessageExtraSyncCursor(requested, cached, issuedMax int64) (cursor int64, persist bool) {
	if issuedMax < 0 {
		issuedMax = 0
	}
	if requested < 0 {
		requested = 0
	} else if requested > issuedMax {
		requested = issuedMax
	}

	if cached < 0 || cached > issuedMax {
		return requested, true
	}
	if cached >= requested {
		return cached, false
	}
	return requested, true
}
