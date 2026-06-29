package avatarrender

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPaletteHex 校验对客户端暴露的色板十六进制视图：长度=PaletteSize、按下标有序、
// 三套色均为 #RRGGBB，且与内部 palette/groupFillPalette/groupIconBackPalette 同源
// （首档钉死设计稿色，防止有人改了内部色板却忘了它是对外契约）。
func TestPaletteHex(t *testing.T) {
	hex := PaletteHex()
	require.Len(t, hex, PaletteSize())

	hexRe := regexp.MustCompile(`^#[0-9A-F]{6}$`)
	for i, h := range hex {
		require.Equal(t, i, h.Index, "entries must be ordered by palette index")
		require.Regexp(t, hexRe, h.Main, "main must be #RRGGBB")
		require.Regexp(t, hexRe, h.Fill, "fill must be #RRGGBB")
		require.Regexp(t, hexRe, h.IconBack, "icon_back must be #RRGGBB")

		// 同源:与 GroupStyleByIndex 解出的同一档 RGBA 一致。
		style, ok := GroupStyleByIndex(i)
		require.True(t, ok)
		require.Equal(t, hexOf(style.Main), h.Main)
		require.Equal(t, hexOf(style.Fill), h.Fill)
		require.Equal(t, hexOf(style.IconBack), h.IconBack)
	}

	// 钉死设计稿首档(#14C0FF ← palette[0])，捕获顺序/取值漂移。
	require.Equal(t, "#14C0FF", hex[0].Main)
	require.Equal(t, "#ECF9FE", hex[0].Fill)
	require.Equal(t, "#7EDAFB", hex[0].IconBack)
}
