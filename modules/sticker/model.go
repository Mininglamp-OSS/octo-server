package sticker

import (
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

// ---------- Request ----------

type addStickerReq struct {
	Path        string `json:"path"`
	Format      string `json:"format"`
	Placeholder string `json:"placeholder"`
}

// ---------- Response ----------

// stickerResp mirrors the shape the web client consumes (path / category /
// placeholder / format), plus sticker_id for the delete call. category is always
// the userStickerCategory sentinel.
type stickerResp struct {
	StickerID   string `json:"sticker_id"`
	Path        string `json:"path"`
	Category    string `json:"category"`
	Placeholder string `json:"placeholder"`
	Format      string `json:"format"`
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
	}
}
