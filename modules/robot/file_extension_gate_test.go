package robot

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// POST /v1/robot/file/upload 的扩展名门（行为测试，非源码扫描）。
//
// 这条 multipart 路径此前只拒空扩展名，不查黑白名单 —— 运营封堵一个格式后，
// /v1/file/upload 与预签名签发都会拒绝，而这里照样把文件写进对象存储。
// 与 bot_api 的同名用例成对存在：一个入口收紧、另一个敞着就是绕过路径，
// 两个 sibling 必须同时守住。
//
// 断言不止看状态码，还看 mock 的 UploadFile **有没有被调用**。
// 零 infra：直驱 handler + mock IService。
// ---------------------------------------------------------------------------

type countingRobotFileService struct {
	uploadCalls int
}

func (m *countingRobotFileService) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	m.uploadCalls++
	return map[string]interface{}{"path": filePath}, nil
}
func (m *countingRobotFileService) DownloadURL(path string, filename string) (string, error) {
	return "https://example.com/download/" + path, nil
}
func (m *countingRobotFileService) GetFile(path string) (io.ReadCloser, string, error) {
	return nil, "", nil
}
func (m *countingRobotFileService) DownloadAndMakeCompose(uploadPath string, downloadURLs []string) (map[string]interface{}, error) {
	return nil, nil
}
func (m *countingRobotFileService) DownloadImage(u string, ctx context.Context) (io.ReadCloser, error) {
	return nil, nil
}
func (m *countingRobotFileService) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	return "https://example.com/signed-get/" + objectPath, nil
}
func (m *countingRobotFileService) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (string, string, error) {
	return "https://example.com/upload", "https://example.com/download", nil
}

type stubRobotPolicySettings struct {
	allowed []string
	blocked []string
	maxKB   int
}

func (s stubRobotPolicySettings) FileExtraAllowedExtensions() []string { return s.allowed }
func (s stubRobotPolicySettings) FileExtraBlockedExtensions() []string { return s.blocked }
func (s stubRobotPolicySettings) FileMaxSizeKB() int                   { return s.maxKB }

func withRobotUploadPolicy(t *testing.T, s file.PolicySettings) {
	t.Helper()
	file.SetPolicySettings(s)
	t.Cleanup(func() { file.SetPolicySettings(nil) })
}

func runRobotUpload(t *testing.T, filename string, size int) (*httptest.ResponseRecorder, *countingRobotFileService) {
	t.Helper()
	return runRobotUploadFull(t, filename, "", size)
}

func runRobotUploadWithPath(t *testing.T, filename, uploadPath string) (*httptest.ResponseRecorder, *countingRobotFileService) {
	t.Helper()
	return runRobotUploadFull(t, filename, uploadPath, 16)
}

func runRobotUploadFull(t *testing.T, filename, uploadPath string, size int) (*httptest.ResponseRecorder, *countingRobotFileService) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	require.NoError(t, err)
	_, err = fw.Write(bytes.Repeat([]byte("a"), size))
	require.NoError(t, err)
	require.NoError(t, mw.Close())

	target := "/v1/robot/file/upload?type=chat"
	if uploadPath != "" {
		target += "&path=" + url.QueryEscape(uploadPath)
	}
	req, err := http.NewRequest(http.MethodPost, target, &buf)
	require.NoError(t, err)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	mockFS := &countingRobotFileService{}
	rb := &Robot{fileService: mockFS, Log: log.NewTLog("Robot-test")}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	rb.botUploadFile(&wkhttp.Context{Context: c})
	return w, mockFS
}

func TestRobotUploadFile_RejectsOperatorBlockedExtension(t *testing.T) {
	withRobotUploadPolicy(t, stubRobotPolicySettings{blocked: []string{".pdf"}, maxKB: 102400})

	w, mockFS := runRobotUpload(t, "report.pdf", 16)
	assert.NotEqual(t, http.StatusOK, w.Code, "封堵的 .pdf 必须被拒：%s", w.Body.String())
	assert.Zero(t, mockFS.uploadCalls, "被拒的上传绝不能写进对象存储")
}

func TestRobotUploadFile_RejectsBuiltinBlockedExtension(t *testing.T) {
	withRobotUploadPolicy(t, stubRobotPolicySettings{maxKB: 102400})

	for _, name := range []string{"payload.exe", "shell.sh", "webshell.php"} {
		t.Run(name, func(t *testing.T) {
			w, mockFS := runRobotUpload(t, name, 16)
			assert.NotEqual(t, http.StatusOK, w.Code, "%s 必须被拒", name)
			assert.Zero(t, mockFS.uploadCalls, "%s 绝不能写进对象存储", name)
		})
	}
}

func TestRobotUploadFile_RejectsUnknownExtension(t *testing.T) {
	withRobotUploadPolicy(t, stubRobotPolicySettings{maxKB: 102400})

	w, mockFS := runRobotUpload(t, "data.nosuchext", 16)
	assert.NotEqual(t, http.StatusOK, w.Code)
	assert.Zero(t, mockFS.uploadCalls)
}

func TestRobotUploadFile_AllowsPermittedExtension(t *testing.T) {
	withRobotUploadPolicy(t, stubRobotPolicySettings{maxKB: 102400})

	w, mockFS := runRobotUpload(t, "photo.jpg", 16)
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, 1, mockFS.uploadCalls)
}

func TestRobotUploadFile_AllowsOperatorAllowedExtension(t *testing.T) {
	withRobotUploadPolicy(t, stubRobotPolicySettings{allowed: []string{".dwg"}, maxKB: 102400})

	w, mockFS := runRobotUpload(t, "drawing.dwg", 16)
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, 1, mockFS.uploadCalls)
}

func TestRobotUploadFile_RejectsOversizeByDynamicCap(t *testing.T) {
	withRobotUploadPolicy(t, stubRobotPolicySettings{maxKB: 1})

	w, mockFS := runRobotUpload(t, "photo.jpg", 2048)
	assert.NotEqual(t, http.StatusOK, w.Code, "超过 1KB 上限必须被拒：%s", w.Body.String())
	assert.Zero(t, mockFS.uploadCalls)
}

// ?path= 与 filename 的扩展名必须一致：校验的是 filename，落库的是 path，
// 两者不一致时 `?path=/x.svg` 配 `x.png` 就能在 .svg 被封堵后仍写出 .svg 对象。
func TestRobotUploadFile_RejectsPathExtensionMismatch(t *testing.T) {
	withRobotUploadPolicy(t, stubRobotPolicySettings{maxKB: 102400})

	w, mockFS := runRobotUploadWithPath(t, "photo.png", "/evil.svg")
	assert.NotEqual(t, http.StatusOK, w.Code, "path 与 filename 扩展名不一致必须被拒：%s", w.Body.String())
	assert.Zero(t, mockFS.uploadCalls, "不一致的上传绝不能写进对象存储")
}

// 一致时照常放行（大小写不敏感）。
func TestRobotUploadFile_AllowsMatchingPathExtension(t *testing.T) {
	withRobotUploadPolicy(t, stubRobotPolicySettings{maxKB: 102400})

	w, mockFS := runRobotUploadWithPath(t, "photo.png", "/dir/photo.PNG")
	assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
	assert.Equal(t, 1, mockFS.uploadCalls)
}
