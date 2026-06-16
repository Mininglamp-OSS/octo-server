package messages_search

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
)

// cursorPayload is the search-after key serialised inside an opaque cursor.
// Score is set only for `relevance` sort, where OS sorts by 3 keys
// (timestamp, _score, messageId) and search_after must echo all three.
// `omitempty` keeps time_desc / time_asc cursors byte-identical to the
// pre-relevance-fix encoding, so already-issued cursors decode unchanged.
type cursorPayload struct {
	TS    int64    `json:"ts"`          // OS `timestamp` field, epoch seconds
	MsgID int64    `json:"id"`          // OS `messageId` tiebreaker
	Score *float64 `json:"s,omitempty"` // _score, relevance sort only
	// Depth is the cumulative number of results the caller has already been
	// served BEFORE the page this cursor opens. The server enforces the
	// max-pagination-depth cap against this value, not against page_size, so
	// changing page_size between requests cannot be used to walk past the cap
	// (YUJ-4667 step 4 / V7 depth cap). omitempty keeps pre-depth cursors
	// (Depth==0 first page) byte-identical to the old encoding.
	Depth int64 `json:"d,omitempty"`
}

// maxPaginationDepth caps the cumulative number of results a single
// cursor-walk may traverse, aligned with OpenSearch's default
// max_result_window (10000). Beyond this the from+size / search_after window
// stops being cheap and deep paging is the wrong tool — callers narrow the
// query instead. Enforced on the cumulative count carried in the cursor.
const maxPaginationDepth = 10000

// cursorSigLen is the HMAC tail length appended after the JSON body. 8 bytes
// of SHA-256 is plenty for a non-monetary tamper check while keeping the
// cursor short on the wire.
const cursorSigLen = 8

// hmacKeyFn returns the keyed HMAC secret. Indirected so tests can swap a
// deterministic value via SetHMACKeyForTest.
var hmacKeyFn = func(cfg SearchConfig) []byte {
	if cfg.CursorHMAC == "" {
		return []byte("octo-messages-search-default-cursor-key")
	}
	return []byte(cfg.CursorHMAC)
}

// encodeCursor packs (timestamp, messageId, score?) into a base64url-encoded
// opaque cursor with an 8-byte HMAC tail. Pass score=nil for time_desc /
// time_asc; pass a non-nil pointer for relevance sort. Depth defaults to 0
// (the cumulative-depth cap treats a depth-less cursor as a fresh walk); use
// encodeCursorWithDepth to carry the running total.
func encodeCursor(cfg SearchConfig, ts, msgID int64, score *float64) string {
	return encodeCursorWithDepth(cfg, ts, msgID, score, 0)
}

// encodeCursorWithDepth is encodeCursor plus the cumulative result-depth the
// next page will start from. The depth is HMAC-signed along with the rest of
// the payload so a client cannot tamper it back to 0 to bypass the cap.
func encodeCursorWithDepth(cfg SearchConfig, ts, msgID int64, score *float64, depth int64) string {
	p := cursorPayload{TS: ts, MsgID: msgID, Score: score, Depth: depth}
	body, _ := json.Marshal(p)
	mac := hmac.New(sha256.New, hmacKeyFn(cfg))
	mac.Write(body)
	sig := mac.Sum(nil)[:cursorSigLen]
	return base64.RawURLEncoding.EncodeToString(append(body, sig...))
}

// decodeCursor reverses encodeCursor, validating the HMAC. Any structural or
// signature failure surfaces as a single "malformed cursor" error so the
// handler can map to VALIDATION_ERROR(field=cursor). The returned score is
// nil for legacy 2-tuple cursors (time_*) and non-nil for relevance cursors.
func decodeCursor(cfg SearchConfig, s string) (int64, int64, *float64, error) {
	ts, msgID, score, _, err := decodeCursorWithDepth(cfg, s)
	return ts, msgID, score, err
}

// decodeCursorWithDepth is decodeCursor plus the cumulative depth carried in
// the cursor (0 for legacy cursors that predate the field).
func decodeCursorWithDepth(cfg SearchConfig, s string) (int64, int64, *float64, int64, error) {
	if s == "" {
		return 0, 0, nil, 0, errors.New("cursor: empty")
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) < cursorSigLen+1 {
		return 0, 0, nil, 0, errors.New("cursor: malformed")
	}
	body, sig := raw[:len(raw)-cursorSigLen], raw[len(raw)-cursorSigLen:]
	mac := hmac.New(sha256.New, hmacKeyFn(cfg))
	mac.Write(body)
	if !hmac.Equal(mac.Sum(nil)[:cursorSigLen], sig) {
		return 0, 0, nil, 0, errors.New("cursor: bad signature")
	}
	var p cursorPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return 0, 0, nil, 0, errors.New("cursor: unmarshal")
	}
	return p.TS, p.MsgID, p.Score, p.Depth, nil
}
