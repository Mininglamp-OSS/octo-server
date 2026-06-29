package avatarrender

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// 群默认头像文字排版参数。与个人头像（Render，单行后两字）不同：群头像文字最多 4 个
// CJK 字（自定义 avatar_text；自动群名取字现为前 2，多为单行），3~4 字时按设计稿排成
// **两行**（上少下多），需要为两行预留更多高度。
const (
	groupMaxInkWidthRatio  = 0.56 // 单行墨迹宽度上限（占容器）：对齐设计稿 2×2 留白
	groupMaxBlockHeightTwo = 0.66 // 两行文字块高度上限（占容器）
	groupMaxFontEmRatio    = 0.42 // 字号硬上限（占容器）
	groupLineHeightEm      = 0.98 // 行高（相对字号 em）：CJK 两行紧凑堆叠
)

// GroupAvatarLines 决定群默认头像文字的排版行，对齐设计稿「群组头像颜色枚举」：
//   - 含 CJK 等宽字符且 >= 3 字 → 两行，上少下多（floor/ceil 切分）：
//     "架构讨论"→["架构","讨论"]（2+2）、"三个字"→["三","个字"]（1+2）；
//   - 纯拉丁/窄字符，或 <= 2 字 → 单行（设计稿 abcd/efgh/开发 均单行）。
func GroupAvatarLines(text string) []string {
	runes := []rune(text)
	if len(runes) <= 2 || !hasWideRune(runes) {
		return []string{text}
	}
	top := len(runes) / 2 // floor：上行少、下行多
	return []string{string(runes[:top]), string(runes[top:])}
}

func hasWideRune(runes []rune) bool {
	for _, r := range runes {
		if isWideRune(r) {
			return true
		}
	}
	return false
}

// isWideRune 粗略判断 r 是否为 CJK/假名/谚文等「全宽」字符（用于排版换行决策，
// 非严格 East Asian Width）。
func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115F) || // Hangul Jamo
		(r >= 0x2E80 && r <= 0xA4CF) || // CJK 部首 .. 彝文（含 CJK 统一汉字、假名等）
		(r >= 0xAC00 && r <= 0xD7A3) || // 谚文音节
		(r >= 0xF900 && r <= 0xFAFF) || // CJK 兼容汉字
		(r >= 0xFF00 && r <= 0xFF60) // 全角 ASCII 变体
}

// RenderGroup 渲染群默认头像「浅底描边圆 + 居中主题色文字」，圆外**透明**（输出带 alpha
// 通道的 RGBA PNG，客户端在任意背景上合成时圆外不出白方块），文字按 GroupAvatarLines
// 决定单行或两行（2×2）。个人头像继续走 Render（单行实心圆），本函数仅供群头像使用，
// 故可独立调整视觉而不影响个人头像。
func RenderGroup(text string, style GroupStyle, size int) ([]byte, error) {
	if text == "" {
		return nil, fmt.Errorf("avatarrender: empty text")
	}
	if size <= 0 {
		size = DefaultSize
	}
	fnt, err := loadFont()
	if err != nil {
		return nil, fmt.Errorf("avatarrender: parse font: %w", err)
	}
	lines := GroupAvatarLines(text)

	big := size * supersample
	// 画布零值即全透明，不再铺白底：圆外保持透明，png.Encode 自动输出带 alpha 的 RGBA PNG。
	canvas := image.NewRGBA(image.Rect(0, 0, big, big))
	drawCircleFilledStroked(canvas, style.Fill, style.Main, groupCircleStrokeRatio)

	if err := drawCenteredLines(canvas, fnt, lines, big, style.Main); err != nil {
		return nil, err
	}

	out := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(out, out.Bounds(), canvas, canvas.Bounds(), xdraw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("avatarrender: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// fitFontPxLines 返回多行文字在 size×size 画布上的字号：在参考字号下测量各行墨迹，
// 使最宽行墨迹 <= size*groupMaxInkWidthRatio 且整块高度 <= size*groupMaxBlockHeightTwo，
// 再施加 groupMaxFontEmRatio 硬上限。单行时退化为与 Render 近似的宽度自适应。
func fitFontPxLines(fnt *sfnt.Font, lines []string, size int) float64 {
	s := float64(size)
	const probePx = 100.0
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{Size: probePx, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return s * baseFontEmRatio
	}
	defer face.Close()
	d := &font.Drawer{Face: face}

	maxInkW := 0.0
	for _, ln := range lines {
		b, _ := d.BoundString(ln)
		w := float64(b.Max.X-b.Min.X) / 64
		if w > maxInkW {
			maxInkW = w
		}
	}
	// 整块高度（probe 字号下）：行高 * 行数。行高按 em 估算，对 CJK 稳定。
	blockH := probePx * groupLineHeightEm * float64(len(lines))
	if maxInkW <= 0 || blockH <= 0 {
		return s * baseFontEmRatio
	}
	scale := math.Min(s*groupMaxInkWidthRatio/maxInkW, s*groupMaxBlockHeightTwo/blockH)
	return math.Min(probePx*scale, s*groupMaxFontEmRatio)
}

// drawCenteredLines 在 size×size 画布上水平居中渲染每一行、并使整块文字垂直居中。
func drawCenteredLines(img *image.RGBA, fnt *sfnt.Font, lines []string, size int, textColor color.RGBA) error {
	fontPx := fitFontPxLines(fnt, lines, size)
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{Size: fontPx, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return fmt.Errorf("avatarrender: new face: %w", err)
	}
	defer face.Close()

	d := &font.Drawer{Dst: img, Src: image.NewUniform(textColor), Face: face}
	lineH := fixed.Int26_6(int(fontPx * groupLineHeightEm * 64))

	// 用首行墨迹上沿与末行墨迹下沿求整块墨迹包围盒，使其垂直居中（对齐 drawCenteredText
	// 的“按墨迹居中”策略，CJK 视觉更准）。基线 b_i = B + i*lineH。
	first, _ := d.BoundString(lines[0])
	last, _ := d.BoundString(lines[len(lines)-1])
	n := len(lines)
	// inkTop(相对B) = first.Min.Y ; inkBottom(相对B) = (n-1)*lineH + last.Max.Y
	inkSpanMid := (first.Min.Y + fixed.Int26_6(n-1)*lineH + last.Max.Y) / 2
	baseB := fixed.I(size)/2 - inkSpanMid

	for i, ln := range lines {
		_, advance := d.BoundString(ln)
		startX := (fixed.I(size) - advance) / 2
		d.Dot = fixed.Point26_6{X: startX, Y: baseB + fixed.Int26_6(i)*lineH}
		d.DrawString(ln)
	}
	return nil
}
