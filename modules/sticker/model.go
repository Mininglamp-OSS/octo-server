package sticker

import (
	"encoding/json"
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
	Status      int
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
