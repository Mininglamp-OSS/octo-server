package file

import (
	"bytes"
	"encoding/json"
	"image"
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
	// quality 降低（95→85）+ 去元数据 shrink，无需 downscale。
	settings.maxSizeKB = 5120
	settings.maxDim = 1024
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
