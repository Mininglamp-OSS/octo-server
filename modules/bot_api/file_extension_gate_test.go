package bot_api

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// POST /v1/bot/file/upload 的扩展名门（行为测试，非源码扫描）。
//
// 这条 multipart 路径此前**完全不校验扩展名** —— 运营在管理台封堵一个格式后，
// /v1/file/upload 和预签名签发都会拒绝，而这里照样把文件写进对象存储。一个入口
// 收紧、另一个敞着，就是跨模块的绕过路径。
//
// 断言不止看状态码，还看 mock 的 UploadFile **有没有被调用** —— 真正要防的是
// 字节落进对象存储，返回什么码是次要的。
//
// 零 infra：直驱 handler + mock IService，同 file_presigned_test.go 的范式。
// ---------------------------------------------------------------------------

// countingFileService 记录 UploadFile 是否被调用过。
type countingFileService struct {
	mockFileServiceForPresigned
	uploadCalls int
}

func (m *countingFileService) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	m.uploadCalls++
	return map[string]interface{}{"path": filePath}, nil
}

// stubPolicySettings 让测试直接控制 modules/file 的上传策略快照。
type stubPolicySettings struct {
	allowed []string
	blocked []string
	maxKB   int
}

func (s stubPolicySettings) FileExtraAllowedExtensions() []string { return s.allowed }
func (s stubPolicySettings) FileExtraBlockedExtensions() []string { return s.blocked }
func (s stubPolicySettings) FileMaxSizeKB() int                   { return s.maxKB }

func withUploadPolicy(t *testing.T, s file.PolicySettings) {
	t.Helper()
	file.SetPolicySettings(s)
	// nil settings 会让 modules/file 回落到 env + baseline，即改动前的默认行为。
	t.Cleanup(func() { file.SetPolicySettings(nil) })
}

func multipartUploadRequest(t *testing.T, filename string, size int) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write(bytes.Repeat([]byte("a"), size))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	req, err := http.NewRequest(http.MethodPost, "/v1/bot/file/upload?type=chat", &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

func runBotUpload(t *testing.T, filename string) (*httptest.ResponseRecorder, *countingFileService) {
	t.Helper()
	return runBotUploadSized(t, filename, 16)
}

func runBotUploadSized(t *testing.T, filename string, size int) (*httptest.ResponseRecorder, *countingFileService) {
	t.Helper()
	return runBotUploadFull(t, filename, "", size)
}

func runBotUploadWithPath(t *testing.T, filename, uploadPath string) (*httptest.ResponseRecorder, *countingFileService) {
	t.Helper()
	return runBotUploadFull(t, filename, uploadPath, 16)
}

func runBotUploadFull(t *testing.T, filename, uploadPath string, size int) (*httptest.ResponseRecorder, *countingFileService) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mockFS := &countingFileService{}
	ba := &BotAPI{fileService: mockFS, Log: log.NewTLog("BotAPI-test")}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = multipartUploadRequest(t, filename, size)
	if uploadPath != "" {
		c.Request.URL.RawQuery = "type=chat&path=" + url.QueryEscape(uploadPath)
	}
	ba.botUploadFile(&wkhttp.Context{Context: c})
	return w, mockFS
}

// 运营封堵的扩展名必须在这条路径上也被拒，且字节不得落进对象存储。
func TestBotUploadFile_RejectsOperatorBlockedExtension(t *testing.T) {
	withUploadPolicy(t, stubPolicySettings{blocked: []string{".pdf"}, maxKB: 102400})

	w, mockFS := runBotUpload(t, "report.pdf")

	assert.NotEqual(t, http.StatusOK, w.Code, "封堵的 .pdf 必须被拒：%s", w.Body.String())
	assert.Zero(t, mockFS.uploadCalls, "被拒的上传绝不能写进对象存储")
}

// 内置黑名单同理 —— 这条路径此前连 .exe 都放行。
func TestBotUploadFile_RejectsBuiltinBlockedExtension(t *testing.T) {
	withUploadPolicy(t, stubPolicySettings{maxKB: 102400})

	for _, name := range []string{"payload.exe", "shell.sh", "webshell.php"} {
		t.Run(name, func(t *testing.T) {
			w, mockFS := runBotUpload(t, name)
			assert.NotEqual(t, http.StatusOK, w.Code, "%s 必须被拒", name)
			assert.Zero(t, mockFS.uploadCalls, "%s 绝不能写进对象存储", name)
		})
	}
}

// 不在白名单里的未知扩展名同样拒绝（与 /v1/file/upload 行为一致）。
func TestBotUploadFile_RejectsUnknownExtension(t *testing.T) {
	withUploadPolicy(t, stubPolicySettings{maxKB: 102400})

	w, mockFS := runBotUpload(t, "data.nosuchext")
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Zero(t, mockFS.uploadCalls)
}

func TestBotUploadFile_RejectsMissingExtension(t *testing.T) {
	withUploadPolicy(t, stubPolicySettings{maxKB: 102400})

	w, mockFS := runBotUpload(t, "noextension")
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Zero(t, mockFS.uploadCalls)
}

// 正常扩展名必须照常放行 —— 收紧不能误伤既有用法。
func TestBotUploadFile_AllowsPermittedExtension(t *testing.T) {
	withUploadPolicy(t, stubPolicySettings{maxKB: 102400})

	w, mockFS := runBotUpload(t, "photo.jpg")
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, 1, mockFS.uploadCalls)
}

// 运营放开的扩展名在这条路径上也要立即可传。
func TestBotUploadFile_AllowsOperatorAllowedExtension(t *testing.T) {
	withUploadPolicy(t, stubPolicySettings{allowed: []string{".dwg"}, maxKB: 102400})

	w, mockFS := runBotUpload(t, "drawing.dwg")
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, 1, mockFS.uploadCalls)
}

// 大小上限同样按当前快照生效（本路径的动态化回归）。
func TestBotUploadFile_RejectsOversizeByDynamicCap(t *testing.T) {
	withUploadPolicy(t, stubPolicySettings{maxKB: 1})

	// 1KB 上限，传 2KB。
	w, mockFS := runBotUploadSized(t, "photo.jpg", 2048)
	assert.NotEqual(t, http.StatusOK, w.Code, fmt.Sprintf("超过 1KB 上限必须被拒：%s", w.Body.String()))
	assert.Zero(t, mockFS.uploadCalls)
}
