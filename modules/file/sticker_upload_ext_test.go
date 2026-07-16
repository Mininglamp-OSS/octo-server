package file

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
)

// F1: stickerUploadExtForRequest 应尊重 settings.StickerUploadAllowedFormats
// 的当前值，让 GET-side preflight URL 与 POST-side 校验用同一套允许集，避免
// 运营收窄格式后客户端拿到之后会被 upload 拒绝的扩展名。

func TestStickerUploadExtForRequest_DefaultAllowlistMatchesFilename(t *testing.T) {
	settings := defaultFakeStickerSettings()
	f := &File{Log: log.NewTLog("FileTest"), settings: settings}

	// 默认允许全 5 种：filename 的扩展名直接命中即可，无 fallback。
	cases := map[string]string{
		"a.gif":      ".gif",
		"a.png":      ".png",
		"a.jpg":      ".jpg",
		"a.jpeg":     ".jpeg",
		"a.webp":     ".webp",
		"A.PNG":      ".png",
		"stuff.JpeG": ".jpeg",
	}
	for filename, want := range cases {
		assert.Equalf(t, want, f.stickerUploadExtForRequest(filename),
			"filename=%q default allowlist should match extension", filename)
	}
}

// filename 缺失 / 无扩展名 / 不认识的扩展名 → 走 fallback。默认允许集下 .gif
// 仍在，回退到 .gif（复刻老 stickerUploadExt 的历史 fallback）。
func TestStickerUploadExtForRequest_FallbackToGifByDefault(t *testing.T) {
	settings := defaultFakeStickerSettings()
	f := &File{Log: log.NewTLog("FileTest"), settings: settings}

	cases := []string{"", "noext", "a.svg", "a.mp4", "a.pdf"}
	for _, filename := range cases {
		assert.Equalf(t, ".gif", f.stickerUploadExtForRequest(filename),
			"filename=%q default fallback should be .gif", filename)
	}
}

// 运营剔除 .gif 后（例如只允许 png/jpg），filename 命中直接返回；miss 时不能
// 回 .gif（那是配置里已经明确剔除的），按 raster allowlist 固定顺序 fallback
// 到 .png（首个允许项）。这是 F1 的核心不变式：GET 侧只吐当前配置允许的扩展名。
func TestStickerUploadExtForRequest_NarrowedFormatsSkipDisabledExt(t *testing.T) {
	settings := defaultFakeStickerSettings()
	settings.allowedFormats = []string{".png", ".jpg"} // 剔除 .gif/.jpeg/.webp
	f := &File{Log: log.NewTLog("FileTest"), settings: settings}

	// filename 命中允许集：原样返回
	assert.Equal(t, ".png", f.stickerUploadExtForRequest("a.png"))
	assert.Equal(t, ".jpg", f.stickerUploadExtForRequest("a.jpg"))

	// filename 命中已剔除项 → 走 fallback，绝不返回被剔除的 .gif
	// fallback 顺序 .gif → .png → ...，.gif 不在允许集 → .png
	assert.Equal(t, ".png", f.stickerUploadExtForRequest("a.gif"))
	assert.Equal(t, ".png", f.stickerUploadExtForRequest("a.jpeg"))
	assert.Equal(t, ".png", f.stickerUploadExtForRequest("a.webp"))
	assert.Equal(t, ".png", f.stickerUploadExtForRequest("")) // filename 缺失
	assert.Equal(t, ".png", f.stickerUploadExtForRequest("noext"))
}

// 配置进一步收窄到不含 .gif 也不含 .png，验证 fallback 沿固定顺序 .png→.jpg→
// .jpeg→.webp 挑第一个允许项。稳定性重要 —— 客户端 URL 生成必须 deterministic。
func TestStickerUploadExtForRequest_FallbackDeterministicWithoutGifPng(t *testing.T) {
	settings := defaultFakeStickerSettings()
	settings.allowedFormats = []string{".jpeg", ".webp"} // 剔除 .gif/.png/.jpg
	f := &File{Log: log.NewTLog("FileTest"), settings: settings}

	assert.Equal(t, ".jpeg", f.stickerUploadExtForRequest("a.gif")) // fallback → .jpeg
	assert.Equal(t, ".jpeg", f.stickerUploadExtForRequest(""))
	assert.Equal(t, ".webp", f.stickerUploadExtForRequest("a.webp")) // 命中 .webp
}

// 未挂 settings（老 unit test 构造 &File{}）走 stickerLimits() nil-safe 分支，
// 应与老 pkg-level stickerUploadExt 行为逐字节等价。这条 test 是回归保障，
// 确保新 method 不破坏 &File{} 单测。
func TestStickerUploadExtForRequest_NilSettingsMatchesLegacy(t *testing.T) {
	f := &File{Log: log.NewTLog("FileTest")}

	cases := []string{"a.gif", "a.png", "a.jpg", "a.jpeg", "a.webp", "", "noext", "a.svg"}
	for _, filename := range cases {
		legacy := stickerUploadExt(filename)
		assert.Equalf(t, legacy, f.stickerUploadExtForRequest(filename),
			"filename=%q must match legacy stickerUploadExt when settings is nil", filename)
	}
}
