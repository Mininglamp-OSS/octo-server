package avatarrender

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"sync"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
)

// 字体：思源黑体（Noto Sans CJK SC）Bold，SIL OFL 1.1，可商用。已子集化为中日韩
// 全覆盖（CJK 统一汉字基本区全 + 假名 + 谚文音节全 + 拉丁/标点），去掉用不到的
// CJK 扩展区与其他文字以控制体积。许可证见 fonts/OFL.txt，来源与子集范围见 fonts/README.md。
// 端上设计稿用 PingFang SC（Apple 专有，不可分发），服务端以思源黑体替代，
// 字形会有细微差异，这是服务端出图的固有取舍。
//
//go:embed fonts/NotoSansCJKsc-Bold-cjk-subset.otf
var fontData []byte

var (
	fontOnce   sync.Once
	parsedFont *sfnt.Font
	fontErr    error
)

func loadFont() (*sfnt.Font, error) {
	fontOnce.Do(func() {
		parsedFont, fontErr = opentype.Parse(fontData)
	})
	return parsedFont, fontErr
}

// Renderable 报告 s 非空且其中每个字符在内嵌字体里都有字形（非 .notdef）。
// 截出的昵称文字若含本字体无字形的字符（典型是 emoji），渲染会出豆腐块，
// 调用方应据此回退到其它兜底图。sfnt.Buffer 为局部变量，本函数并发安全。
func Renderable(s string) bool {
	if s == "" {
		return false
	}
	fnt, err := loadFont()
	if err != nil {
		return false
	}
	var buf sfnt.Buffer
	for _, r := range s {
		idx, err := fnt.GlyphIndex(&buf, r)
		if err != nil || idx == 0 { // 0 = .notdef
			return false
		}
	}
	return true
}

const (
	// DefaultSize 是默认输出边长（与历史 generateDefaultAvatar 的 200 保持一致）。
	DefaultSize = 200
	// supersample 是超采样倍数：先在 size*ss 上以硬边渲染，再高质量缩小，
	// 一次性得到圆形与文字的抗锯齿效果。
	supersample = 4
	// fontSizeRatio 取自设计稿：32px 容器内字号 10px。
	fontSizeRatio = 10.0 / 32.0
)

// Options 描述一次头像渲染。
type Options struct {
	// Text 是已截好的展示文字（如昵称后两字）。为空时返回错误，由调用方兜底。
	Text string
	// Bg 是背景圆颜色。
	Bg color.RGBA
	// Size 是输出 PNG 的边长（像素）；<=0 时用 DefaultSize。
	Size int
}

// Render 渲染一张「纯色圆 + 居中白色文字」的 PNG（圆外透明），返回编码后的字节。
// 文字颜色固定为白色（与设计稿一致，不做对比度切换）。
func Render(opts Options) ([]byte, error) {
	if opts.Text == "" {
		return nil, fmt.Errorf("avatarrender: empty text")
	}
	size := opts.Size
	if size <= 0 {
		size = DefaultSize
	}
	fnt, err := loadFont()
	if err != nil {
		return nil, fmt.Errorf("avatarrender: parse font: %w", err)
	}

	big := size * supersample

	// 1. 在放大画布上画硬边圆（圆外保持透明）。
	canvas := image.NewRGBA(image.Rect(0, 0, big, big))
	drawCircle(canvas, opts.Bg)

	// 2. 居中渲染白色文字。
	if err := drawCenteredText(canvas, fnt, opts.Text, big); err != nil {
		return nil, err
	}

	// 3. 高质量缩小到目标尺寸，得到抗锯齿结果。
	out := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(out, out.Bounds(), canvas, canvas.Bounds(), xdraw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("avatarrender: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

// drawCircle 在 img 上填充一个充满边界的实心圆，圆外像素保持透明。
func drawCircle(img *image.RGBA, c color.RGBA) {
	b := img.Bounds()
	d := float64(b.Dx())
	cx, cy := d/2, d/2
	radius := d/2 - 1
	radiusSq := radius * radius
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dx := float64(x) - cx + 0.5
			dy := float64(y) - cy + 0.5
			if dx*dx+dy*dy <= radiusSq {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

// drawCenteredText 在 size×size 画布上水平+垂直居中渲染白色文字。
func drawCenteredText(img *image.RGBA, fnt *sfnt.Font, text string, size int) error {
	fontPx := float64(size) * fontSizeRatio
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{
		Size:    fontPx,
		DPI:     72, // DPI=72 时 Size 即像素
		Hinting: font.HintingFull,
	})
	if err != nil {
		return fmt.Errorf("avatarrender: new face: %w", err)
	}
	defer face.Close()

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(color.White),
		Face: face,
	}

	// 用实际字形墨迹边界做居中，而不是 face.Metrics() 的 Ascent/Descent —— 后者是
	// 含行间距的行盒度量，CJK 字形相对 em 的留白不对称，直接用会让文字偏离视觉中心
	// （实测偏下数 px）。BoundString 给出墨迹包围盒，据此把墨迹中心对齐画布中心。
	bounds, advance := d.BoundString(text)
	startX := (fixed.I(size) - advance) / 2
	inkMidY := (bounds.Min.Y + bounds.Max.Y) / 2
	baselineY := fixed.I(size)/2 - inkMidY

	d.Dot = fixed.Point26_6{X: startX, Y: baselineY}
	d.DrawString(text)
	return nil
}
