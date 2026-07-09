package file

import (
	"bytes"
	"encoding/json"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/stickersig"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// api_sticker_compress_test.go — 服务端压缩管线在 uploadFile 里的集成 unit test
// （sticker-upload-compression 任务 Task #4）。
//
// 覆盖：
//   1) compress_enabled=true + jpg 大图 → 上传字节被替换成压缩后字节，resp.size
//      是压后大小，sticker_handle 基于最终 fullURL 签发。
//   2) compress_enabled=true + gif → 走 skipped 分支，字节流不变。
//   3) compress_enabled=true + jpg + target 极小 → over_limit 拒绝，落库为空。
//   4) compress_enabled=false → 走原路径，与老 unit test 等价（已由既有
//      TestUploadFile_StickerHandleMinted 覆盖：&File{} settings==nil 时 compress
//      被视为 disabled）。

// fakeStickerSystemSettings 实现 stickerSystemSettings；用于 api-level 集成测试
// 全套配置注入，无需 MySQL/Redis test server。
type fakeStickerSystemSettings struct {
	maxSizeKB       int
	maxDim          int
	compressMaxDim  int // 0 → 回落 maxDim（不缩放），镜像生产 getter 语义
	allowedFormats  []string
	compressEnabled bool
	targetKB        int
	maxConcurrency  int
	timeoutMs       int
}

func (f *fakeStickerSystemSettings) StickerUploadMaxSizeKB() int    { return f.maxSizeKB }
func (f *fakeStickerSystemSettings) StickerUploadMaxDimension() int { return f.maxDim }
func (f *fakeStickerSystemSettings) StickerUploadAllowedFormats() []string {
	out := make([]string, len(f.allowedFormats))
	copy(out, f.allowedFormats)
	return out
}
func (f *fakeStickerSystemSettings) StickerCompressEnabled() bool       { return f.compressEnabled }
func (f *fakeStickerSystemSettings) StickerCompressTargetKB() int       { return f.targetKB }
func (f *fakeStickerSystemSettings) StickerCompressMaxConcurrency() int { return f.maxConcurrency }
func (f *fakeStickerSystemSettings) StickerCompressTimeoutMs() int      { return f.timeoutMs }

// StickerCompressMaxDimension 镜像生产 getter：未设置(≤0) → 512 默认缩放目标；
// 设置了则用配置值（测试据此触发/关闭 downscale 路径）。
func (f *fakeStickerSystemSettings) StickerCompressMaxDimension() int {
	if f.compressMaxDim <= 0 {
		return 512 // = common.defaultStickerCompressMaxDimension
	}
	return f.compressMaxDim
}

// defaultFakeStickerSettings 复刻改动前的硬编码默认值 —— 大多数测试从这里起手，
// 只按需要覆盖字段（例如 compressEnabled）。
func defaultFakeStickerSettings() *fakeStickerSystemSettings {
	return &fakeStickerSystemSettings{
		maxSizeKB:       1024,
		maxDim:          512,
		allowedFormats:  []string{".gif", ".png", ".jpg", ".jpeg", ".webp"},
		compressEnabled: false,
		targetKB:        1024,
		maxConcurrency:  4,
		timeoutMs:       5000,
	}
}

// capturingMockService 是能抓获 UploadFile 收到字节的 mockService；用于验证
// "落库的是压缩后字节，而不是原字节"。
type capturingMockService struct {
	mockService
	uploaded     []byte
	uploadedPath string
}

func (c *capturingMockService) UploadFile(filePath, contentType, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	c.uploadedPath = filePath
	var buf bytes.Buffer
	if err := copyFileWriter(&buf); err != nil {
		return nil, err
	}
	c.uploaded = buf.Bytes()
	return nil, nil
}

// 构造一个 File，直接注入 fake settings + 生产 compressor + capturing service。
func newStickerCompressFile(t *testing.T, s *fakeStickerSystemSettings) (*File, *capturingMockService) {
	t.Helper()
	svc := &capturingMockService{
		mockService: mockService{downloadURL: "https://cdn.example.com/dm/sticker/10000/abc"},
	}
	return &File{
		Log:        log.NewTLog("FileTest"),
		service:    svc,
		settings:   s,
		compressor: newStickerCompressor(s),
	}, svc
}

// TestUploadFile_CompressorNilDoesNotPanic 保护异常构造路径：settings 挂了且
// compressEnabled=true，但 compressor==nil。生产 New(ctx) 里 settings+compressor
// 是一起初始化的，这种状态不该出现；但 File.compressor 的文档说 nil=disabled，
// uploadFile 就必须尊重这个合约（review F3）—— fail-open 走原字节，不 nil-panic。
func TestUploadFile_CompressorNilDoesNotPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 1024

	svc := &capturingMockService{
		mockService: mockService{downloadURL: "https://cdn.example.com/dm/sticker/" + uid + "/abc.jpg"},
	}
	f := &File{
		Log:      log.NewTLog("FileTest"),
		service:  svc,
		settings: settings,
		// compressor 特意留 nil —— 关键的异常构造点
	}

	origBytes := makeTestJPEG(t, 128, 128, 80)
	body, contentType := newMultipartFile(t, "abc.jpg", origBytes)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/file/upload?type=sticker&path=/"+uid+"/abc.jpg", body)
	c.Request.Header.Set("Content-Type", contentType)
	c.Set("uid", uid)
	wkCtx := &wkhttp.Context{Context: c}

	require.NotPanics(t, func() {
		f.uploadFile(wkCtx)
	}, "compressor==nil must not cause a nil-pointer dereference in uploadFile")

	require.Equalf(t, http.StatusOK, w.Code, "must fail-open on nil compressor; body: %s", w.Body.String())
	assert.Equal(t, origBytes, svc.uploaded,
		"fail-open path must upload the original bytes byte-for-byte")
}

// TestUploadFile_CompressEnabled_LargeJPEG_UsesCompressedBytes 集成验证：开启
// 压缩后，大 JPEG 上传后落库的是**压缩后**字节；resp.size 是压后值；handle 基于
// 最终 fullURL 签发；path/ext 在方案 C 下保持不变。
func TestUploadFile_CompressEnabled_LargeJPEG_UsesCompressedBytes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	// 允许 5MB 原始大小 + 1024px 维度，让 900px 源图能通过维度门。压缩靠
	// quality 降低（95→85）+ 去元数据 shrink，无需 downscale：显式把缩放目标设成
	// 1024（≥源 900）以隔离本用例只测重编码、不测 downscale。
	settings.maxSizeKB = 5120
	settings.maxDim = 1024
	settings.compressMaxDim = 1024
	settings.targetKB = 512

	f, svc := newStickerCompressFile(t, settings)
	svc.downloadURL = "https://cdn.example.com/dm/sticker/" + uid + "/abc.jpg"

	origBytes := makeTestJPEG(t, 900, 900, 95)
	require.Greater(t, len(origBytes), 200*1024, "seed must be big enough that compression measurably shrinks it")
	body, contentType := newMultipartFile(t, "abc.jpg", origBytes)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/file/upload?type=sticker&path=/"+uid+"/abc.jpg", body)
	c.Request.Header.Set("Content-Type", contentType)
	c.Set("uid", uid)
	wkCtx := &wkhttp.Context{Context: c}

	f.uploadFile(wkCtx)

	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	// 落库字节是压缩后的（严格短于原字节）。
	assert.Less(t, len(svc.uploaded), len(origBytes), "compressed bytes must be shorter than original")
	// 落库字节仍是 valid JPEG。dim 保持在 maxDim 内（本用例源就 <=maxDim）。
	dec, format, err := image.Decode(bytes.NewReader(svc.uploaded))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	bounds := dec.Bounds()
	assert.LessOrEqual(t, bounds.Dx(), settings.maxDim)
	assert.LessOrEqual(t, bounds.Dy(), settings.maxDim)
	// 源图正方形 → 输出也应正方形（本用例宽高比 1:1，压缩不能拉伸）。
	assert.Equal(t, bounds.Dx(), bounds.Dy(),
		"aspect ratio must be preserved: 900×900 source must produce square output")

	// resp.size 是压后大小；handle verify 基于返回 path。
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	respSize, _ := resp["size"].(float64)
	assert.EqualValues(t, len(svc.uploaded), int64(respSize), "resp.size must be the post-compression byte count")
	handle, _ := resp["sticker_handle"].(string)
	require.NotEmpty(t, handle)
	path, _ := resp["path"].(string)
	assert.True(t, stickersig.Verify(uid, path, handle),
		"handle must verify for (uid, final path) — the tuple sticker.add checks")
}

// TestUploadFile_CompressEnabled_GIF_SkipsUnchanged 集成验证：开启压缩后 gif
// 走 compress_skipped 分支，落库字节与源字节字节级相等（不压缩）。方案 C 下
// gif 不进入压缩管线，即使 compress_enabled=true 也不改内容。
func TestUploadFile_CompressEnabled_GIF_SkipsUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true

	f, svc := newStickerCompressFile(t, settings)
	svc.downloadURL = "https://cdn.example.com/dm/sticker/" + uid + "/x.gif"

	// 最小 valid GIF89a (1x1 pixel)。
	gifBytes := []byte{
		0x47, 0x49, 0x46, 0x38, 0x39, 0x61, // GIF89a
		0x01, 0x00, 0x01, 0x00, 0x80, 0x00, 0x00,
		0x00, 0x00, 0x00, 0xff, 0xff, 0xff,
		0x2c, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00,
		0x02, 0x02, 0x44, 0x01, 0x00, 0x3b,
	}
	body, contentType := newMultipartFile(t, "x.gif", gifBytes)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/file/upload?type=sticker&path=/"+uid+"/x.gif", body)
	c.Request.Header.Set("Content-Type", contentType)
	c.Set("uid", uid)
	wkCtx := &wkhttp.Context{Context: c}

	f.uploadFile(wkCtx)

	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, gifBytes, svc.uploaded, "gif must pass through byte-for-byte when compress is enabled")

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	respSize, _ := resp["size"].(float64)
	assert.EqualValues(t, len(gifBytes), int64(respSize), "resp.size must equal source size when gif skipped")
}

// TestUploadFile_CompressEnabled_OverLimitRejects 集成验证：压后仍超 target 时
// 直接拒绝上传（capturingMockService.uploaded 不应被写入）。
func TestUploadFile_CompressEnabled_OverLimitRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.targetKB = 1 // 1KB 压后目标 —— 300x300 随机像素 png 达不到
	settings.maxDim = 512
	settings.maxSizeKB = 5120 // 允许 5MB 原始上传，让 png 能过 size 门

	f, svc := newStickerCompressFile(t, settings)

	origBytes := makeTestPNG(t, 300, 300)
	require.Greater(t, len(origBytes), 10*1024, "seed PNG must exceed 10KB")
	body, contentType := newMultipartFile(t, "abc.png", origBytes)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/file/upload?type=sticker&path=/"+uid+"/abc.png", body)
	c.Request.Header.Set("Content-Type", contentType)
	c.Set("uid", uid)
	wkCtx := &wkhttp.Context{Context: c}

	f.uploadFile(wkCtx)

	require.NotEqual(t, http.StatusOK, w.Code, "over_limit must reject; got body=%s", w.Body.String())
	assert.Contains(t, strings.ToLower(w.Body.String()), "kb", "error should mention size limit")
	assert.Nil(t, svc.uploaded, "over_limit must not reach storage")
}

// TestUploadFile_CompressEnabled_HandleTiesToFinalBytes: sticker_handle 必须绑定
// 到最终存储对象的 URL。方案 C 下 path/ext 不变，因此 fullURL 也不变，handle 与
// 压缩前理论上一致 —— 关键不变量是"handle verify 用的 URL 与 mockService 收到
// 的落库路径完全对应"，保证客户端拿 handle 后指向的是压缩后对象。
func TestUploadFile_CompressEnabled_HandleTiesToFinalStoredObject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.targetKB = 2048
	settings.maxDim = 512

	f, svc := newStickerCompressFile(t, settings)
	svc.downloadURL = "https://cdn.example.com/dm/sticker/" + uid + "/final.jpg"

	origBytes := makeTestJPEG(t, 256, 256, 90)
	body, contentType := newMultipartFile(t, "final.jpg", origBytes)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/file/upload?type=sticker&path=/"+uid+"/final.jpg", body)
	c.Request.Header.Set("Content-Type", contentType)
	c.Set("uid", uid)
	wkCtx := &wkhttp.Context{Context: c}

	// 用一个远大于 imaging.Encode 耗时的 timeout，避免 flaky。
	settings.timeoutMs = 10_000

	f.uploadFile(wkCtx)

	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	require.Contains(t, svc.uploadedPath, "sticker/"+uid+"/final.jpg",
		"落库对象路径必须以 sticker keyspace + uid + 原扩展名结尾")

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	handle, _ := resp["sticker_handle"].(string)
	respPath, _ := resp["path"].(string)
	require.NotEmpty(t, handle)
	// path/ext 未变 → 客户端拿到的 handle 校验的是最终对象 URL。
	assert.True(t, stickersig.Verify(uid, respPath, handle),
		"handle must verify against the final stored URL")
	// 别的 uid 不通过 —— handle 绑定了上传者。
	assert.False(t, stickersig.Verify("99999", respPath, handle))
	// 花几毫秒的 compressor 结束后 svc.uploaded 已被填。
	_ = time.Millisecond
	assert.NotEmpty(t, svc.uploaded, "capturing service must have received bytes")
}

// makeTestAPNG 生成 (w,h) 的 APNG：先用 image/png 编一张真 PNG（IHDR 声明 w×h，
// 供 image.DecodeConfig 取维度），再在 IHDR 之后、IDAT 之前插入一个 acTL chunk，
// 让 isAnimatedPNGSource 判定为动图（acTL 只按结构识别、不校验 CRC）。
// PNG 布局固定：8B 签名 + IHDR chunk(4 len + 4 "IHDR" + 13 data + 4 crc = 25B)。
func makeTestAPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	// 复用 makeTestPNG 生成真 PNG（签名 + IHDR + … + IDAT + IEND），再在 IHDR 之后、
	// IDAT 之前插入一个 acTL chunk 使其成为 APNG。CRC 必须**有效**：Go 的 image.Decode
	// 会校验未知 chunk 的 CRC，若为 0 整图解码失败——那样一旦动图检测被破坏，图会走
	// decode-failed 而非 compressed，测试就无法区分"识别为动图"与"字节损坏"（review）。
	// PNG 布局固定：8B 签名 + IHDR chunk(4 len + 4 "IHDR" + 13 data + 4 crc = 25B)。
	b := makeTestPNG(t, w, h)
	const ihdrEnd = 8 + 25
	require.Greater(t, len(b), ihdrEnd, "encoded PNG shorter than signature+IHDR")
	data := []byte{
		0x00, 0x00, 0x00, 0x01, // num_frames = 1
		0x00, 0x00, 0x00, 0x00, // num_plays = 0
	}
	crc := crc32.ChecksumIEEE(append([]byte("acTL"), data...)) // CRC 覆盖 type+data
	actl := []byte{0x00, 0x00, 0x00, 0x08, 'a', 'c', 'T', 'L'}
	actl = append(actl, data...)
	actl = append(actl, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
	out := make([]byte, 0, len(b)+len(actl))
	out = append(out, b[:ihdrEnd]...)
	out = append(out, actl...)
	out = append(out, b[ihdrEnd:]...)
	return out
}

// 命名回归（review 共识）：一张 >512 的 APNG（.png 扩展名）通过放宽到 1024 的维度门，
// 但压缩阶段被识别为 animated → skipped:animated（无法缩放），落库维度守卫据源尺寸
// fail-closed 拒绝。用 compress_oversized_rejected 计数正向断言"是守卫拒的、不是维度门
// 拒的"（两处错误文案相同，无法只靠 body 区分）；有效 acTL CRC 保证一旦动图检测失效，
// 图会解码→缩到 512→落库(200) 而非静默通过本用例。
func TestUploadFile_OversizedGuard_AnimatedPNGRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 512
	settings.timeoutMs = 10_000

	f, svc := newStickerCompressFile(t, settings)

	before := testutil.ToFloat64(metricStickerUploadTotal.WithLabelValues("compress_oversized_rejected"))
	w := uploadStickerForTest(t, f, uid, "anim.png", makeTestAPNG(t, 600, 600))

	require.NotEqualf(t, http.StatusOK, w.Code, "a 600² APNG can't be shrunk and must be rejected, not stored; body: %s", w.Body.String())
	assert.Nil(t, svc.uploaded, "oversized animated PNG must not reach storage")
	after := testutil.ToFloat64(metricStickerUploadTotal.WithLabelValues("compress_oversized_rejected"))
	assert.Equal(t, float64(1), after-before,
		"must be rejected by the oversized store-guard, not the dimension gate")
}

// makeTestGIF 生成 (w,h) 的单帧 GIF；gif.Encode 会把 RGBA 量化到调色板。
func makeTestGIF(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{byte(x % 255), byte(y % 255), 128, 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, gif.Encode(&buf, img, nil))
	return buf.Bytes()
}

// uploadStickerForTest 跑一次贴纸上传，返回 recorder 供断言。集中构造 multipart +
// gin 上下文，避免每个门测试重复样板。
func uploadStickerForTest(t *testing.T, f *File, uid, name string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	mbody, contentType := newMultipartFile(t, name, body)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodPost, "/v1/file/upload?type=sticker&path=/"+uid+"/"+name, mbody)
	c.Request.Header.Set("Content-Type", contentType)
	c.Set("uid", uid)
	f.uploadFile(&wkhttp.Context{Context: c})
	return w
}

// TestUploadFile_OversizedJPEG_AcceptedAndDownscaled_DefaultUploadDim 是
// sticker-oversized-default 的核心验证：upload_max_dimension 保持默认 512，但压缩
// 开启后一张 1024² jpg **不再被 512 门拒**（可压格式放宽到 1024 接收），并被 downscale
// 到 compress_max_dimension=512 落库。
func TestUploadFile_OversizedJPEG_AcceptedAndDownscaled_DefaultUploadDim(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120     // 允许大原图过 size 门
	settings.maxDim = 512         // upload_max_dimension 保持默认 512
	settings.compressMaxDim = 512 // 缩放目标 512
	settings.targetKB = 2048
	settings.timeoutMs = 10_000

	f, svc := newStickerCompressFile(t, settings)
	svc.downloadURL = "https://cdn.example.com/dm/sticker/" + uid + "/big.jpg"

	w := uploadStickerForTest(t, f, uid, "big.jpg", makeTestJPEG(t, 1024, 1024, 90))

	require.Equalf(t, http.StatusOK, w.Code, "1024² jpg must be accepted (gate widened to 1024) not rejected at 512; body: %s", w.Body.String())
	dec, format, err := image.Decode(bytes.NewReader(svc.uploaded))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	b := dec.Bounds()
	assert.Equal(t, 512, b.Dx(), "long edge must be downscaled to compress_max_dimension (512)")
	assert.Equal(t, 512, b.Dy())
}

// TestUploadFile_OversizedGIF_RejectedWhenCompressOn 验证：gif 无法缩放，压缩开启
// 也**不**放宽接收门 —— 一张 >512 的 gif 仍在 upload_max_dimension(512) 门被拒，
// 不会被当作大图存进来。
func TestUploadFile_OversizedGIF_RejectedWhenCompressOn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 512 // gif 仍受此约束

	f, svc := newStickerCompressFile(t, settings)

	w := uploadStickerForTest(t, f, uid, "big.gif", makeTestGIF(t, 600, 600))

	require.NotEqualf(t, http.StatusOK, w.Code, "600² gif must be rejected at the 512 gate; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "512")
	assert.Nil(t, svc.uploaded, "rejected gif must not reach storage")
}

// TestUploadFile_OversizedJPEG_RejectedWhenCompressOff 是零影响回归：压缩关闭时，
// 即使 jpg 可压，也**不**放宽接收门 —— >512 jpg 仍被 512 门拒，与改动前一致。
func TestUploadFile_OversizedJPEG_RejectedWhenCompressOff(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = false // 关键：压缩关闭
	settings.maxSizeKB = 5120
	settings.maxDim = 512

	f, svc := newStickerCompressFile(t, settings)

	w := uploadStickerForTest(t, f, uid, "big.jpg", makeTestJPEG(t, 600, 600, 90))

	require.NotEqualf(t, http.StatusOK, w.Code, "with compress off, >512 jpg must be rejected at 512 (zero-impact); body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "512")
	assert.Nil(t, svc.uploaded, "rejected jpg must not reach storage")
}

// TestUploadFile_CompressEnabled_LargeImage_DownscaledAndStored 集成验证
// sticker-downscale-store：当 compress_max_dimension < upload_max_dimension 时，
// 一张在接收门内（≤ upload_max_dimension）但大于缩放目标的静态图，被等比缩到
// 缩放目标后再落库。这条路径在解耦前不可达（门/目标同源，Fit 恒不触发）。
func TestUploadFile_CompressEnabled_LargeImage_DownscaledAndStored(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120     // 允许大原图过 size 门
	settings.maxDim = 1024        // 接收上限：1024² 源图能过维度门
	settings.compressMaxDim = 512 // 缩放目标：解耦后小于接收门 → 触发 Fit
	settings.targetKB = 2048      // 缩后 512² 远小于此，不会 over_limit
	settings.timeoutMs = 10_000   // 远大于编码耗时，避免 flaky

	f, svc := newStickerCompressFile(t, settings)
	svc.downloadURL = "https://cdn.example.com/dm/sticker/" + uid + "/big.jpg"

	w := uploadStickerForTest(t, f, uid, "big.jpg", makeTestJPEG(t, 1024, 1024, 90))

	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	dec, format, err := image.Decode(bytes.NewReader(svc.uploaded))
	require.NoError(t, err)
	assert.Equal(t, "jpeg", format)
	bounds := dec.Bounds()
	// 1024² 正方形 Fit 进 512×512 外接框 → 恰好 512×512（等比、不放大）。
	assert.Equal(t, 512, bounds.Dx(), "long edge must be downscaled to compress_max_dimension")
	assert.Equal(t, 512, bounds.Dy())
	assert.LessOrEqual(t, bounds.Dx(), settings.compressMaxDim)

	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	respSize, _ := resp["size"].(float64)
	assert.EqualValues(t, len(svc.uploaded), int64(respSize), "resp.size must be the post-downscale byte count")
	handle, _ := resp["sticker_handle"].(string)
	path, _ := resp["path"].(string)
	require.NotEmpty(t, handle)
	assert.True(t, stickersig.Verify(uid, path, handle),
		"handle must verify against the post-downscale stored object")
}

// TestUploadFile_CompressEnabled_DefaultCompressMaxDim_ShrinksOversizedTo512 是
// sticker-oversized-default 的默认行为验证：**未配置** compress_max_dimension（回落
// 默认 512）时，压缩开启后一张 800² jpg 被自动缩到 512 —— 无需运营额外调旋钮。
func TestUploadFile_CompressEnabled_DefaultCompressMaxDim_ShrinksOversizedTo512(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 512 // upload_max_dimension 保持默认
	// compressMaxDim 特意留 0 → fake 回落默认 512，与生产 getter 语义一致。
	settings.targetKB = 4096
	settings.timeoutMs = 10_000

	f, svc := newStickerCompressFile(t, settings)
	svc.downloadURL = "https://cdn.example.com/dm/sticker/" + uid + "/big.jpg"

	w := uploadStickerForTest(t, f, uid, "big.jpg", makeTestJPEG(t, 800, 800, 90))

	require.Equalf(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	dec, _, err := image.Decode(bytes.NewReader(svc.uploaded))
	require.NoError(t, err)
	bounds := dec.Bounds()
	assert.Equal(t, 512, bounds.Dx(), "default compress_max_dimension (512) must shrink an 800² source to 512")
	assert.Equal(t, 512, bounds.Dy())
}

// ----- sticker-oversized-store-guard regression (review findings 1/2/5) -----
//
// 维度门为 jpg/png 放宽到 1024 的前提是压缩会把图缩到 upload_max_dimension 内。以下
// 用例覆盖"压缩实际没缩到位"的各条 fail-open 路径：都必须 fail-closed 拒绝，绝不把
// 超过 upload_max_dimension 的大图落库。

// compressor==nil：整块跳过，源 1024² jpg 若不拦就会原样落库。
func TestUploadFile_OversizedGuard_NilCompressorRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 512 // upload_max_dimension

	svc := &capturingMockService{
		mockService: mockService{downloadURL: "https://cdn.example.com/dm/sticker/" + uid + "/big.jpg"},
	}
	f := &File{
		Log:      log.NewTLog("FileTest"),
		service:  svc,
		settings: settings,
		// compressor 特意留 nil
	}

	w := uploadStickerForTest(t, f, uid, "big.jpg", makeTestJPEG(t, 1024, 1024, 90))

	require.NotEqualf(t, http.StatusOK, w.Code, "nil compressor must not store an oversized image; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "512")
	assert.Nil(t, svc.uploaded, "oversized image must not reach storage on the nil-compressor path")
}

// failed（decode/encode 出错，fail-open 走原字节）：注入必败 doCompress。
func TestUploadFile_OversizedGuard_CompressFailedRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 512
	settings.targetKB = 4096
	settings.timeoutMs = 10_000

	f, svc := newStickerCompressFile(t, settings)
	f.compressor.doCompress = func(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error) {
		return stickerCompressResult{}, errors.New("injected decode failure")
	}

	w := uploadStickerForTest(t, f, uid, "big.jpg", makeTestJPEG(t, 1024, 1024, 90))

	require.NotEqualf(t, http.StatusOK, w.Code, "compress-failed fail-open must not store an oversized image; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "512")
	assert.Nil(t, svc.uploaded, "oversized image must not reach storage on the failed path")
}

// skipped:timeout（并发饱和/超时同类）：注入慢 doCompress + 极小 timeout。
func TestUploadFile_OversizedGuard_CompressTimeoutRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 512
	settings.targetKB = 4096
	settings.timeoutMs = 5 // 极小超时，doCompress 必然抢占失败

	f, svc := newStickerCompressFile(t, settings)
	f.compressor.doCompress = func(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error) {
		time.Sleep(300 * time.Millisecond) // 远超 5ms timeout → Compress 返回 skipped:timeout
		return stickerCompressResult{Outcome: stickerCompressOutcomeCompressed, Bytes: src, Size: int64(len(src)), OutMaxDim: 512}, nil
	}

	w := uploadStickerForTest(t, f, uid, "big.jpg", makeTestJPEG(t, 1024, 1024, 90))

	require.NotEqualf(t, http.StatusOK, w.Code, "compress-timeout skip must not store an oversized image; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "512")
	assert.Nil(t, svc.uploaded, "oversized image must not reach storage on the timeout-skip path")
}

// 纵深防御（review P2）：compressed 结局但 OutMaxDim<=0（未来 compressor 变体忘填 /
// 返回 0），守卫不可盲信 —— 回退到源尺寸判断，超限即拒。注入 OutMaxDim=0 + 1024² 源
// → 必须 fail-closed 拒绝，而不是把未知尺寸的图放行落库。（注入的 doCompress 直接返回
// compressed，忽略 maxDim/targetKB，故本用例不设 compressMaxDim/targetKB。）
func TestUploadFile_OversizedGuard_CompressedZeroOutMaxDimRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 512
	settings.timeoutMs = 10_000

	f, svc := newStickerCompressFile(t, settings)
	// 注入"压缩成功但没报告输出尺寸"的 compressor（OutMaxDim 留 0）。
	f.compressor.doCompress = func(ext string, src []byte, maxDim, targetKB int) (stickerCompressResult, error) {
		return stickerCompressResult{
			Outcome:   stickerCompressOutcomeCompressed,
			Bytes:     src,
			Size:      int64(len(src)),
			OutMaxDim: 0, // 关键：未填充
		}, nil
	}

	w := uploadStickerForTest(t, f, uid, "x.jpg", makeTestJPEG(t, 1024, 1024, 90))

	require.NotEqualf(t, http.StatusOK, w.Code, "compressed outcome with OutMaxDim<=0 on an oversized source must fail closed; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "512")
	assert.Nil(t, svc.uploaded, "must not store when OutMaxDim is untrustworthy on an oversized source")
}

// 配置陷阱：compress_max_dimension(1024) > upload_max_dimension(512)。1024² jpg
// 压后仍 1024（Fit 目标 1024，不缩），guard 用压后实际尺寸 fail-closed 拒绝。
func TestUploadFile_OversizedGuard_CompressMaxDimAboveUploadDimRejects(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const uid = "10000"
	settings := defaultFakeStickerSettings()
	settings.compressEnabled = true
	settings.maxSizeKB = 5120
	settings.maxDim = 512          // upload_max_dimension
	settings.compressMaxDim = 1024 // 大于 upload_max_dimension 的错误配置
	settings.targetKB = 4096
	settings.timeoutMs = 10_000

	f, svc := newStickerCompressFile(t, settings)

	w := uploadStickerForTest(t, f, uid, "big.jpg", makeTestJPEG(t, 1024, 1024, 90))

	require.NotEqualf(t, http.StatusOK, w.Code, "compressed-but-not-shrunk (target>upload_max) must be rejected; body: %s", w.Body.String())
	assert.Contains(t, w.Body.String(), "512")
	assert.Nil(t, svc.uploaded, "image compressed but still above upload_max_dimension must not be stored")
}
