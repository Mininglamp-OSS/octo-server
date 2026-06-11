package avatarrender

import (
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
