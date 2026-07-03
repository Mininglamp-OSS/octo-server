package sticker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/db"
)

// userStickerCategory is the single, fixed "category" value carried by every
// personal custom sticker. Stickers are flat (no packs) — but the chat client's
// LottieSticker message payload still has a `category` field, so we emit a
// stable sentinel here so the existing client send-path keeps working unchanged.
const userStickerCategory = "user"

// StickerModel 用户自定义贴纸（个人维度，扁平、不分包）。
type StickerModel struct {
	StickerID   string
	UID         string
	Path        string
	Placeholder string
	Format      string
	Sort        int
	Shortcode   string
	Keywords    string
	SourcePath  string
	// SourcePathHash is set only for "collect from message" records. Direct
	// upload registrations keep it empty so the collect-only unique key does not
	// affect existing custom-sticker creation.
	SourcePathHash string
	Status         int
	db.BaseModel
}

// allowedStickerFormats is the whitelist of raster image formats a user may
// upload as a custom sticker. Lottie/TGS is intentionally excluded — end users
// cannot author it; it is reserved for built-in animated stickers.
var allowedStickerFormats = map[string]bool{
	"gif":  true,
	"png":  true,
	"jpg":  true,
	"jpeg": true,
	"webp": true,
}

// normalizeStickerFormat lowercases and strips a leading dot so "PNG", ".png"
// and "png" all collapse to the canonical "png".
func normalizeStickerFormat(format string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".")
}

// isAllowedStickerFormat reports whether format (already normalized) is accepted.
func isAllowedStickerFormat(format string) bool {
	return allowedStickerFormats[format]
}

const (
	minStickerShortcodeLen     = 2
	maxStickerShortcodeLen     = 32
	maxStickerKeywordCount     = 10
	maxStickerKeywordLen       = 20
	maxStickerKeywordsStoreLen = 255
)

var stickerShortcodeRe = regexp.MustCompile(`^[a-z0-9_]{2,32}$`)

func normalizeStickerShortcode(raw string) (string, bool) {
	shortcode := strings.ToLower(strings.TrimSpace(raw))
	if shortcode == "" {
		return "", true
	}
	if len(shortcode) < minStickerShortcodeLen || len(shortcode) > maxStickerShortcodeLen {
		return "", false
	}
	if !stickerShortcodeRe.MatchString(shortcode) {
		return "", false
	}
	return shortcode, true
}

func normalizeStickerKeywords(raw []string) (string, []string, bool) {
	keywords := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, item := range raw {
		keyword := strings.TrimSpace(item)
		if keyword == "" {
			continue
		}
		if len([]rune(keyword)) > maxStickerKeywordLen {
			return "", nil, false
		}
		if _, ok := seen[keyword]; ok {
			continue
		}
		seen[keyword] = struct{}{}
		keywords = append(keywords, keyword)
		if len(keywords) > maxStickerKeywordCount {
			return "", nil, false
		}
	}
	if len(keywords) == 0 {
		return "", []string{}, true
	}
	data, err := json.Marshal(keywords)
	if err != nil || len(data) > maxStickerKeywordsStoreLen {
		return "", nil, false
	}
	return string(data), keywords, true
}

func decodeStickerKeywords(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var keywords []string
	if err := json.Unmarshal([]byte(raw), &keywords); err != nil {
		return []string{}
	}
	if keywords == nil {
		return []string{}
	}
	return keywords
}

// stickerObjectKeyRe matches the object-key tail the multipart uploader always
// produces for a sticker: ".../sticker/<uid>/<name>.<ext>" (see
// modules/file/api.go getFilePath TypeSticker → key "sticker/{loginUID}/{uuid}.ext").
// Matching the stable key segment lets us validate ownership without resolving
// each storage backend's URL shape, so it works whether req.Path is a relative
// preview key or an absolute S3/MinIO/OSS/COS/CDN download URL.
var stickerObjectKeyRe = regexp.MustCompile(`(?:^|/)sticker/([^/]+)/[^/]+\.([A-Za-z0-9]+)$`)

// validateStickerPath reports whether path refers to an object produced by THIS
// user's sticker-hardened upload: its object key must be
// "sticker/{loginUID}/<name>.<ext>" with <ext> an allowed raster format equal to
// the (already normalized) declared format. This closes the cross-type
// registration bypass — uploading via type=chat (looser 100MB cap + general
// allowlist) and registering that URL as a sticker — and the foreign/other-user
// path case, without a per-backend URL normalizer.
//
// Pragmatic prefix check, by design (PR#508, maintainer-approved): an absolute
// URL on an UNCONFIGURED origin that happens to carry the right
// ".../sticker/{loginUID}/x.gif" tail still passes — we deliberately do NOT pin
// the host to configured storage origins. The residual is self-scoped: the
// forged sticker only ever renders back to the registering user's own list (no
// server-side consumer reads sticker.path for another user, and the message-send
// path already accepts client-supplied sticker URLs unvalidated), so it grants
// no capability the sender does not already have.
func validateStickerPath(path, loginUID, format string) bool {
	// Strip query/fragment so a signed download URL (…?X-Amz-Signature=…) still
	// matches on its key tail.
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	m := stickerObjectKeyRe.FindStringSubmatch(path)
	if m == nil {
		return false
	}
	if m[1] != loginUID {
		return false
	}
	ext := normalizeStickerFormat(m[2])
	return ext == format && isAllowedStickerFormat(ext)
}

type collectStickerSource struct {
	SourceKey   string
	DisplayPath string
	Format      string
}

var (
	stickerSourceObjectKeyExactRe = regexp.MustCompile(`^sticker/([^/]+)/([^/]+)\.([A-Za-z0-9]+)$`)
	stickerSourceObjectKeyTailRe  = regexp.MustCompile(`(?:^|/)sticker/([^/]+)/([^/]+)\.([A-Za-z0-9]+)$`)
)

func parseCollectStickerSourcePath(raw string) (collectStickerSource, bool) {
	pathValue := strings.TrimSpace(raw)
	if pathValue == "" {
		return collectStickerSource{}, false
	}
	if i := strings.IndexAny(pathValue, "?#"); i >= 0 {
		pathValue = pathValue[:i]
	}

	candidate := pathValue
	if u, err := url.Parse(pathValue); err == nil && u.Scheme != "" && u.Host != "" {
		candidate = u.Path
	}
	candidate = strings.TrimPrefix(candidate, "/")

	if strings.HasPrefix(candidate, "file/preview/") {
		return parseCollectStickerObjectKey(strings.TrimPrefix(candidate, "file/preview/"), stickerSourceObjectKeyExactRe)
	}
	if strings.HasPrefix(candidate, "sticker/") {
		return parseCollectStickerObjectKey(candidate, stickerSourceObjectKeyExactRe)
	}
	return parseCollectStickerObjectKey(candidate, stickerSourceObjectKeyTailRe)
}

func parseCollectStickerObjectKey(candidate string, re *regexp.Regexp) (collectStickerSource, bool) {
	m := re.FindStringSubmatch(candidate)
	if m == nil {
		return collectStickerSource{}, false
	}
	format := normalizeStickerFormat(m[3])
	if !isAllowedStickerFormat(format) {
		return collectStickerSource{}, false
	}
	sourceKey := "sticker/" + m[1] + "/" + m[2] + "." + m[3]
	// Reject path-traversal / relative segments before the key is ever stored.
	// The regex's `[^/]+` matches "." and "..", so `sticker/../a.png` parses to
	// SourceKey "sticker/../a.png". That key is later resolved by renderablePath
	// via DownloadURL → url.JoinPath, which RESOLVES "..", collapsing the
	// "sticker/" prefix and escaping the sticker keyspace to the bucket root
	// (e.g. "<base>/bucket/a.png"). Confining at ingress keeps traversal keys out
	// of the DB and out of every downstream consumer (this one and file/preview).
	if hasUnsafeSegment(sourceKey) {
		return collectStickerSource{}, false
	}
	return collectStickerSource{
		SourceKey:   sourceKey,
		DisplayPath: "file/preview/" + sourceKey,
		Format:      format,
	}, true
}

// hasUnsafeSegment reports whether key contains a "." or ".." path segment. A
// ".." segment lets url.JoinPath (used by every storage backend's DownloadURL)
// escape the object key's intended prefix; "." is normalized away and never
// legitimate in a generated sticker key. Percent-encoded forms ("%2e%2e") are
// intentionally NOT decoded here: url.JoinPath leaves them literal (verified),
// so they cannot traverse — only literal segments can.
func hasUnsafeSegment(key string) bool {
	for _, seg := range strings.Split(key, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func stickerSourcePathHash(sourceKey string) string {
	sum := sha256.Sum256([]byte(sourceKey))
	return hex.EncodeToString(sum[:])
}

// ---------- Request ----------

type addStickerReq struct {
	Path        string   `json:"path"`
	Format      string   `json:"format"`
	Placeholder string   `json:"placeholder"`
	Sort        int      `json:"sort"`
	Shortcode   string   `json:"shortcode"`
	Keywords    []string `json:"keywords"`
	// Handle is the HMAC upload handle returned by /v1/file/upload?type=sticker
	// (response field "sticker_handle"). It proves Path was produced by this
	// caller's content-validated sticker upload. Whether it is REQUIRED is
	// governed by the system_setting sticker.handle_required policy
	// (SystemSettings.StickerHandleRequired) — NOT merely by OCTO_MASTER_KEY being
	// configured (stickersig.Enabled, the signing capability). An invalid handle is
	// always rejected; a missing handle is rejected only when the policy is on. See
	// classifyStickerPath.
	Handle string `json:"handle"`
}

type collectStickerReq struct {
	Path        string   `json:"path"`
	Placeholder string   `json:"placeholder"`
	Sort        int      `json:"sort"`
	Shortcode   string   `json:"shortcode"`
	Keywords    []string `json:"keywords"`
}

type updateStickerReq struct {
	Placeholder *string   `json:"placeholder"`
	Sort        *int      `json:"sort"`
	Shortcode   *string   `json:"shortcode"`
	Keywords    *[]string `json:"keywords"`
}

// ---------- Response ----------

// stickerResp mirrors the shape the web client consumes (path / category /
// placeholder / format), plus sticker_id for the delete call. category is always
// the userStickerCategory sentinel.
type stickerResp struct {
	StickerID   string   `json:"sticker_id"`
	Path        string   `json:"path"`
	Category    string   `json:"category"`
	Placeholder string   `json:"placeholder"`
	Format      string   `json:"format"`
	Sort        int      `json:"sort"`
	Shortcode   string   `json:"shortcode"`
	Keywords    []string `json:"keywords"`
}

// listStickerResp is the GET /v1/sticker/user envelope: { "list": [...] }.
// List is always non-nil so an empty collection serializes as [] (never null),
// which is the whole point of the endpoint existing (issue #26: stop the 404).
type listStickerResp struct {
	List []stickerResp `json:"list"`
}

func toStickerResp(m *StickerModel) stickerResp {
	return stickerResp{
		StickerID:   m.StickerID,
		Path:        m.Path,
		Category:    userStickerCategory,
		Placeholder: m.Placeholder,
		Format:      m.Format,
		Sort:        m.Sort,
		Shortcode:   m.Shortcode,
		Keywords:    decodeStickerKeywords(m.Keywords),
	}
}
