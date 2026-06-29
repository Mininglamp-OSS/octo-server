package avatarrender

import (
	"fmt"
	"hash/crc32"
	"image/color"
)

// palette 是个人默认头像的固定背景色板，取自 UI 设计稿（Frame 1912055909）。
// 顺序与设计稿 order 0-9 一一对应，不可随意调整——颜色由 seed 稳定映射，
// 改动顺序会让既有用户的头像换色。
var palette = []color.RGBA{
	{R: 0x14, G: 0xC0, B: 0xFF, A: 0xFF}, // #14C0FF
	{R: 0x00, G: 0xD6, B: 0xB9, A: 0xFF}, // #00D6B9
	{R: 0x34, G: 0xC7, B: 0x24, A: 0xFF}, // #34C724
	{R: 0xB3, G: 0xD6, B: 0x00, A: 0xFF}, // #B3D600
	{R: 0xFF, G: 0xC6, B: 0x0A, A: 0xFF}, // #FFC60A
	{R: 0xFF, G: 0x88, B: 0x00, A: 0xFF}, // #FF8800
	{R: 0xF0, G: 0x1D, B: 0x94, A: 0xFF}, // #F01D94
	{R: 0xD1, G: 0x36, B: 0xD1, A: 0xFF}, // #D136D1
	{R: 0x7F, G: 0x3B, B: 0xF5, A: 0xFF}, // #7F3BF5
	{R: 0x49, G: 0x54, B: 0xE6, A: 0xFF}, // #4954E6
}

// ColorForSeed 按 seed 稳定地从色板选一个背景色。seed 用 uid（不随昵称变化），
// 保证同一用户在任何页面颜色一致、且改名后颜色不变。
func ColorForSeed(seed string) color.RGBA {
	idx := crc32.ChecksumIEEE([]byte(seed)) % uint32(len(palette))
	return palette[idx]
}

// groupFillPalette 是群默认头像的圆**填充色**（浅色），与 palette 主色一一对应、同序。
// 取自设计稿「群组头像颜色枚举」：群头像是「浅底圆 + 主题色描边 + 主题色内容」，
// 区别于个人头像的「实心主题色圆 + 白字」。下标与 palette 对齐，故同一 seed 解出的
// 主色（描边/文字）与填充色配套。
var groupFillPalette = []color.RGBA{
	{R: 0xEC, G: 0xF9, B: 0xFE, A: 0xFF}, // #ECF9FE ← #14C0FF
	{R: 0xEA, G: 0xFA, B: 0xF8, A: 0xFF}, // #EAFAF8 ← #00D6B9
	{R: 0xF0, G: 0xFB, B: 0xEF, A: 0xFF}, // #F0FBEF ← #34C724
	{R: 0xF7, G: 0xFA, B: 0xE5, A: 0xFF}, // #F7FAE5 ← #B3D600
	{R: 0xFD, G: 0xF9, B: 0xED, A: 0xFF}, // #FDF9ED ← #FFC60A
	{R: 0xFF, G: 0xF5, B: 0xEB, A: 0xFF}, // #FFF5EB ← #FF8800
	{R: 0xFE, G: 0xF1, B: 0xF8, A: 0xFF}, // #FEF1F8 ← #F01D94
	{R: 0xFC, G: 0xEE, B: 0xFC, A: 0xFF}, // #FCEEFC ← #D136D1
	{R: 0xF6, G: 0xF1, B: 0xFE, A: 0xFF}, // #F6F1FE ← #7F3BF5
	{R: 0xF2, G: 0xF3, B: 0xFD, A: 0xFF}, // #F2F3FD ← #4954E6
}

// groupIconBackPalette 是群默认头像**图标兜底**（群名空/不可渲染）双人剪影中**后景人**
// 的浅色，与 palette 主色同序对应。设计稿双人为双色：前景人用主题主色，后景人用本表
// 同色系浅色。下标与 palette 对齐。
var groupIconBackPalette = []color.RGBA{
	{R: 0x7E, G: 0xDA, B: 0xFB, A: 0xFF}, // #7EDAFB ← #14C0FF
	{R: 0x64, G: 0xE8, B: 0xD6, A: 0xFF}, // #64E8D6 ← #00D6B9
	{R: 0x8E, G: 0xE0, B: 0x85, A: 0xFF}, // #8EE085 ← #34C724
	{R: 0xD2, G: 0xE7, B: 0x6A, A: 0xFF}, // #D2E76A ← #B3D600
	{R: 0xF7, G: 0xDC, B: 0x82, A: 0xFF}, // #F7DC82 ← #FFC60A
	{R: 0xFF, G: 0xBA, B: 0x6B, A: 0xFF}, // #FFBA6B ← #FF8800
	{R: 0xF5, G: 0x7A, B: 0xC0, A: 0xFF}, // #F57AC0 ← #F01D94
	{R: 0xE5, G: 0x8F, B: 0xE5, A: 0xFF}, // #E58FE5 ← #D136D1
	{R: 0xAD, G: 0x82, B: 0xF7, A: 0xFF}, // #AD82F7 ← #7F3BF5
	{R: 0x7B, G: 0x83, B: 0xEA, A: 0xFF}, // #7B83EA ← #4954E6
}

// GroupStyle 是群默认头像的一套配色：Main 为主题主色（圆描边 + 文字 + 图标前景人），
// Fill 为圆填充浅色，IconBack 为图标后景人浅色。三者来自设计稿同一色组。
type GroupStyle struct {
	Main     color.RGBA
	Fill     color.RGBA
	IconBack color.RGBA
}

// styleAt 按色板下标组装 GroupStyle。三张表同序同长，调用方须保证 idx 合法。
func styleAt(idx int) GroupStyle {
	return GroupStyle{
		Main:     palette[idx],
		Fill:     groupFillPalette[idx],
		IconBack: groupIconBackPalette[idx],
	}
}

// GroupStyleForSeed 按 seed（群号）稳定地解出一套群头像配色。下标算法与 ColorForSeed
// **完全一致**（crc32 % len(palette)），故 Main 等于历史 ColorForSeed(seed) —— 保证
// 「改名不变色」「与旧派生色一致」，已合成/历史群切到新风格后主色不跳变。
func GroupStyleForSeed(seed string) GroupStyle {
	idx := crc32.ChecksumIEEE([]byte(seed)) % uint32(len(palette))
	return styleAt(int(idx))
}

// GroupStyleByIndex 返回群主在二次弹窗显式选定的自定义色组。i 越界时 ok=false，
// 调用方应回退到 GroupStyleForSeed。取值范围与 ColorByIndex/PaletteSize 一致。
func GroupStyleByIndex(i int) (GroupStyle, bool) {
	if i < 0 || i >= len(palette) {
		return GroupStyle{}, false
	}
	return styleAt(i), true
}

// PaletteSize 返回固定色板的颜色数，供调用方校验自定义色板下标的取值范围。
func PaletteSize() int {
	return len(palette)
}

// ColorByIndex 返回色板第 i 个颜色（用户在二次弹窗显式选定的自定义色）。
// i 越界时 ok=false，调用方应回退到 ColorForSeed。
func ColorByIndex(i int) (color.RGBA, bool) {
	if i < 0 || i >= len(palette) {
		return color.RGBA{}, false
	}
	return palette[i], true
}

// GroupColorHex 是一档群头像配色的十六进制（#RRGGBB）形式，供**客户端本地渲染**
// （如「修改头像」实时预览、色圈选择）使用：客户端用 Index 选色、用 Main/Fill/IconBack
// 复刻服务端 PNG 的视觉（主色描边/文字、浅底圆、图标后景人），保证预览与建群/改群后
// 服务端渲染出的真图一致。
type GroupColorHex struct {
	Index    int    // 色板下标 [0,PaletteSize)，与 avatar_color 取值一致
	Main     string // 主题主色：圆描边 + 文字 + 图标前景人
	Fill     string // 圆填充浅色
	IconBack string // 图标兜底（双人剪影）后景人浅色
}

// PaletteHex 返回整套群头像色板的十六进制形式（按下标 0..PaletteSize 顺序），作为
// 色板的**唯一数据源**对客户端暴露——前端不再硬编码色值，避免与服务端 palette.go 漂移。
func PaletteHex() []GroupColorHex {
	out := make([]GroupColorHex, len(palette))
	for i := range palette {
		out[i] = GroupColorHex{
			Index:    i,
			Main:     hexOf(palette[i]),
			Fill:     hexOf(groupFillPalette[i]),
			IconBack: hexOf(groupIconBackPalette[i]),
		}
	}
	return out
}

// hexOf 把 RGBA 主色格式化为 #RRGGBB（忽略 alpha——色板均为不透明设计色）。
func hexOf(c color.RGBA) string {
	return fmt.Sprintf("#%02X%02X%02X", c.R, c.G, c.B)
}
