package file

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// normalizeStickerCompressFormat 兑现"完全大小写不敏感 + 前导点可选"的合约
// （review F2）。历史实现只匹配全大写/全小写变体，混合大小写会 fall through 到
// "other" —— 现在的实现走 strings.ToLower 保证 .Jpg / JPG / jpeg 都归到同一
// label 序列，避免 histogram cardinality 因大小写 typo 意外扩张。
func TestNormalizeStickerCompressFormat(t *testing.T) {
	cases := map[string]string{
		// 全 5 种支持格式的完整大小写 / 有/无前导点变体
		".jpg":  "jpg",
		"jpg":   "jpg",
		".JPG":  "jpg",
		"JPG":   "jpg",
		".Jpg":  "jpg",
		".jpeg": "jpeg",
		"jpeg":  "jpeg",
		"JPEG":  "jpeg",
		"Jpeg":  "jpeg",
		".png":  "png",
		"PNG":   "png",
		".Png":  "png",
		".gif":  "gif",
		"GIF":   "gif",
		".webp": "webp",
		"WEBP":  "webp",
		"WebP":  "webp",
		// 未识别 / 空 / 奇怪输入统一归到 "other"，保证 label 集合封闭
		"":       "other",
		".":      "other",
		".mp4":   "other",
		"bmp":    "other",
		"random": "other",
	}
	for in, want := range cases {
		assert.Equalf(t, want, normalizeStickerCompressFormat(in), "input=%q", in)
	}
}
