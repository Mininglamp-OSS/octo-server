package avatarrender

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
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

	// 动态字号：在参考字号下测量文字墨迹包围盒，再线性缩放进目标墨迹盒。
	// 设计稿只标注了 CJK 双字场景（32px 容器内 10px 字号，墨迹宽约占 60%）；
	// 拉丁字母墨迹远窄于 CJK，同字号下视觉明显偏小（"er" 墨迹仅占 ~30%），
	// 因此按实际墨迹自适应，而不是所有文字一个固定 em 比例。
	//
	//  - maxInkWidthRatio：墨迹宽度上限。两个 CJK 字符（墨迹 ~1.9em）在此
	//    约束下解出 em ≈ 0.34·size，与设计稿的 10/32 ≈ 0.31 基本一致；
	//  - maxInkHeightRatio：墨迹高度上限，约束高瘦文字；
	//  - maxFontEmRatio：字号硬上限。窄墨迹文字（单字母、"er"）按宽度放大
	//    会失控，用它封顶，同时也保证墨迹盒四角不出圆。
	maxInkWidthRatio  = 0.64
	maxInkHeightRatio = 0.42
	maxFontEmRatio    = 0.46
	// baseFontEmRatio 是墨迹测量失败时的兜底字号（设计稿 32px 容器内 10px）。
	baseFontEmRatio = 10.0 / 32.0

	// groupCircleStrokeRatio 是群头像描边宽度相对圆直径的比例。设计稿头像约 31px
	// 直径配 1px 描边；服务端输出 200px 后通常由客户端缩小展示，所以按直径比例绘制。
	groupCircleStrokeRatio = 1.0 / 31.0
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

// Render 渲染一张「纯色圆 + 居中白色文字」的 PNG，返回编码后的字节。圆外**透明**
//（png.Encode 输出带 alpha 通道的 RGBA PNG），客户端在任意背景上合成时圆外不出白方块。
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

	// 1. 画布零值即全透明，不铺底色，直接画硬边圆。圆外保持透明，png.Encode 自动输出
	// 带 alpha 通道的 RGBA PNG（客户端在任意背景上合成时圆外不出白方块）。
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

// drawCircle 在 img 上填充一个充满边界的实心圆；圆外像素不动（画布零值即透明，调用方不铺底色）。
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

// drawCircleFilledStroked 在 img 上绘制群头像专用圆：浅色填充 + 主题色描边。
// 本函数只改圆内像素，圆外不动（画布零值即透明，调用方不铺底色）。
func drawCircleFilledStroked(img *image.RGBA, fill, stroke color.RGBA, strokeRatio float64) {
	b := img.Bounds()
	d := float64(b.Dx())
	cx, cy := d/2, d/2
	radius := d/2 - 1
	strokeWidth := d * strokeRatio
	if strokeWidth < 1 {
		strokeWidth = 1
	}
	innerRadius := radius - strokeWidth
	if innerRadius < 0 {
		innerRadius = 0
	}
	radiusSq := radius * radius
	innerRadiusSq := innerRadius * innerRadius
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dx := float64(x) - cx + 0.5
			dy := float64(y) - cy + 0.5
			distSq := dx*dx + dy*dy
			switch {
			case distSq <= innerRadiusSq:
				img.SetRGBA(x, y, fill)
			case distSq <= radiusSq:
				img.SetRGBA(x, y, stroke)
			}
		}
	}
}

// fitFontPx 返回 text 在 size×size 画布上应使用的字号（像素）：在参考字号下
// 测量墨迹包围盒，线性缩放到目标墨迹盒（maxInkWidthRatio × maxInkHeightRatio）
// 内，再施加 maxFontEmRatio 硬上限。CJK 双字先触宽度约束（结果≈设计稿的
// 10/32），窄墨迹的拉丁字母则放大到字号上限，避免同字号下视觉偏小。
// 测量失败或墨迹为空（理论上不会：调用方已用 Renderable 过滤）回退 baseFontEmRatio。
func fitFontPx(fnt *sfnt.Font, text string, size int) float64 {
	s := float64(size)
	// 参考字号任取即可：无 hinting 时字形度量随字号线性缩放。
	const probePx = 100.0
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{
		Size:    probePx,
		DPI:     72,
		Hinting: font.HintingNone,
	})
	if err != nil {
		return s * baseFontEmRatio
	}
	defer face.Close()
	bounds, _ := (&font.Drawer{Face: face}).BoundString(text)
	inkW := float64(bounds.Max.X-bounds.Min.X) / 64
	inkH := float64(bounds.Max.Y-bounds.Min.Y) / 64
	if inkW <= 0 || inkH <= 0 {
		return s * baseFontEmRatio
	}
	scale := math.Min(s*maxInkWidthRatio/inkW, s*maxInkHeightRatio/inkH)
	return math.Min(probePx*scale, s*maxFontEmRatio)
}

// drawCenteredText 在 size×size 画布上水平+垂直居中渲染白色文字。
func drawCenteredText(img *image.RGBA, fnt *sfnt.Font, text string, size int) error {
	fontPx := fitFontPx(fnt, text, size)
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

//go:embed icons/group-person-front.png
var groupIconFrontData []byte

//go:embed icons/group-person-back.png
var groupIconBackData []byte

var (
	groupIconOnce     sync.Once
	groupIconFrontImg image.Image
	groupIconBackImg  image.Image
	groupIconErr      error
)

func loadGroupIconMasks() (image.Image, image.Image, error) {
	groupIconOnce.Do(func() {
		groupIconFrontImg, groupIconErr = png.Decode(bytes.NewReader(groupIconFrontData))
		if groupIconErr != nil {
			return
		}
		groupIconBackImg, groupIconErr = png.Decode(bytes.NewReader(groupIconBackData))
	})
	return groupIconFrontImg, groupIconBackImg, groupIconErr
}

// RenderIcon 渲染「浅底描边圆 + 双色群组图标」的 PNG，圆外**透明**，用于群名为空或
// 无法取字时的兜底。style 来自群头像专用色板，不影响个人头像 Render。
func RenderIcon(style GroupStyle) ([]byte, error) {
	return renderIconSized(style, DefaultSize)
}

func renderIconSized(style GroupStyle, size int) ([]byte, error) {
	if size <= 0 {
		size = DefaultSize
	}
	front, back, err := loadGroupIconMasks()
	if err != nil {
		return nil, fmt.Errorf("avatarrender: decode group icon masks: %w", err)
	}

	big := size * supersample
	// 画布零值即全透明，不铺白底：圆外保持透明，输出带 alpha 的 RGBA PNG。
	canvas := image.NewRGBA(image.Rect(0, 0, big, big))
	drawCircleFilledStroked(canvas, style.Fill, style.Main, groupCircleStrokeRatio)

	backTint := tintMask(back, style.IconBack)
	frontTint := tintMask(front, style.Main)
	xdraw.CatmullRom.Scale(canvas, canvas.Bounds(), backTint, backTint.Bounds(), xdraw.Over, nil)
	xdraw.CatmullRom.Scale(canvas, canvas.Bounds(), frontTint, frontTint.Bounds(), xdraw.Over, nil)

	out := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(out, out.Bounds(), canvas, canvas.Bounds(), xdraw.Over, nil)

	var buf bytes.Buffer
	if err := png.Encode(&buf, out); err != nil {
		return nil, fmt.Errorf("avatarrender: encode png: %w", err)
	}
	return buf.Bytes(), nil
}

func tintMask(mask image.Image, c color.RGBA) *image.RGBA {
	bounds := mask.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	if nm, ok := mask.(*image.NRGBA); ok {
		// 快路径：直读 NRGBA.Pix，避免 mask.At() 每像素把颜色装箱进 color.Color
		// 接口（800×800 mask ≈ 64 万次/张分配）。复刻 color.NRGBA.RGBA() 的预乘后
		// max(r,g,b) 作为覆盖 alpha，输出与通用路径逐字节一致。
		w, h := bounds.Dx(), bounds.Dy()
		for y := 0; y < h; y++ {
			si := nm.PixOffset(bounds.Min.X, bounds.Min.Y+y)
			di := out.PixOffset(0, y)
			for x := 0; x < w; x++ {
				alpha := premulMax(uint32(nm.Pix[si]), uint32(nm.Pix[si+1]), uint32(nm.Pix[si+2]), uint32(nm.Pix[si+3]))
				out.Pix[di] = c.R
				out.Pix[di+1] = c.G
				out.Pix[di+2] = c.B
				out.Pix[di+3] = uint8(alpha >> 8)
				si += 4
				di += 4
			}
		}
		return out
	}
	// 通用回退路径（非 NRGBA 解码结果）：行为与历史一致。
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := mask.At(x, y).RGBA()
			alpha := uint8(max16(r, g, b) >> 8)
			out.SetRGBA(x-bounds.Min.X, y-bounds.Min.Y, color.RGBA{R: c.R, G: c.G, B: c.B, A: alpha})
		}
	}
	return out
}

// premulMax 复刻 color.NRGBA.RGBA() 的「预乘 alpha 后取 max(r,g,b)」，返回 16-bit。
// 用于从非预乘 NRGBA 字节直接求白色/灰度剪影的覆盖 alpha，避免接口装箱。
func premulMax(r8, g8, b8, a8 uint32) uint32 {
	r := r8 | r8<<8
	r = r * a8 / 0xff
	g := g8 | g8<<8
	g = g * a8 / 0xff
	b := b8 | b8<<8
	b = b * a8 / 0xff
	return max16(r, g, b)
}

func max16(a, b, c uint32) uint32 {
	if b > a {
		a = b
	}
	if c > a {
		a = c
	}
	return a
}
