package robot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type mockFileServiceForUpload struct {
	lastObjectPath        string
	lastContentDisp       string
	lastContentType       string
	lastUploadPath        string
	lastUploadContentDisp string
	lastUploadContentType string
}

func (m *mockFileServiceForUpload) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	m.lastUploadPath = filePath
	m.lastUploadContentType = contentType
	m.lastUploadContentDisp = contentDisposition
	return nil, nil
}

func (m *mockFileServiceForUpload) DownloadURL(path string, filename string) (string, error) {
	return fmt.Sprintf("https://example.com/download/%s", path), nil
}

func (m *mockFileServiceForUpload) GetFile(path string) (io.ReadCloser, string, error) {
	return nil, "", nil
}

func (m *mockFileServiceForUpload) DownloadAndMakeCompose(uploadPath string, downloadURLs []string) (map[string]interface{}, error) {
	return nil, nil
}

func (m *mockFileServiceForUpload) DownloadImage(url string, ctx context.Context) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockFileServiceForUpload) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (string, string, error) {
	m.lastObjectPath = objectPath
	m.lastContentDisp = contentDisposition
	m.lastContentType = contentType
	return "https://example.com/upload?" + objectPath, "https://example.com/download/" + objectPath, nil
}

func (m *mockFileServiceForUpload) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	return "https://example.com/signed-get/" + objectPath, nil
}

func TestBotUploadPresigned_ContentDisposition(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name     string
		filename string
	}{
		{"ascii filename", "report.pdf"},
		{"chinese filename", "报告.pdf"},
		{"mixed filename", "report-报告.pdf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := &mockFileServiceForUpload{}
			rb := &Robot{
				Log:         log.NewTLog("RobotTest"),
				fileService: mockFS,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/robot/upload/presigned?fileSize=1024&filename="+tt.filename, nil)

			wkCtx := &wkhttp.Context{Context: c}
			rb.botUploadPresigned(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code)

			var resp map[string]interface{}
			err := json.Unmarshal(w.Body.Bytes(), &resp)
			assert.NoError(t, err)

			// Content-Disposition should be passed to PresignedPutURL
			assert.Contains(t, mockFS.lastContentDisp, "inline",
				"contentDisposition should contain 'inline'")
			assert.Contains(t, mockFS.lastContentDisp, "filename*=UTF-8''",
				"contentDisposition should contain RFC 5987 encoded filename")

			// Key should be UUID-based
			key := resp["key"].(string)
			assert.True(t, strings.HasPrefix(key, "chat/"),
				"key should start with 'chat/'")
			assert.NotContains(t, key, tt.filename,
				"key should not contain original filename")
		})
	}
}

func TestBotUploadPresigned_UUIDBasedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockFS := &mockFileServiceForUpload{}
	rb := &Robot{
		Log:         log.NewTLog("RobotTest"),
		fileService: mockFS,
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/v1/robot/upload/presigned?filename=test.jpg&fileSize=1024", nil)

	wkCtx := &wkhttp.Context{Context: c}
	rb.botUploadPresigned(wkCtx)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	key := resp["key"].(string)

	parts := strings.Split(key, "/")
	assert.Equal(t, 4, len(parts), "key should have 4 parts: chat/timestamp/uuid/uuid.ext, got: %s", key)
	assert.Equal(t, "chat", parts[0])
	assert.True(t, strings.HasSuffix(parts[3], ".jpg"), "last part should end with .jpg")
	assert.NotEqual(t, "test.jpg", parts[3], "last part should be uuid.jpg, not the original filename")
}

// TestBotUploadPresigned_ContentDispositionInResponse asserts the
// /v1/robot/upload/presigned endpoint returns the contentDisposition
// value that was signed into PresignedPutURL — parity with the main
// file endpoint at modules/file/api.go.
//
// Critical: without this echo, browsers cannot construct a PUT that
// passes SigV4. Independently flagged in PR#50 R7 by Jerry-Xin and
// lml2468.
func TestBotUploadPresigned_ContentDispositionInResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name     string
		filename string
	}{
		{"ascii filename with spaces", "annual report 2025.pdf"},
		{"chinese filename", "报告.pdf"},
		{"mixed filename", "report-报告.pdf"},
		{"unicode emoji filename", "🚀-launch.png"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := &mockFileServiceForUpload{}
			rb := &Robot{
				Log:         log.NewTLog("RobotTest"),
				fileService: mockFS,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/robot/upload/presigned?fileSize=1024&filename="+tt.filename, nil)

			wkCtx := &wkhttp.Context{Context: c}
			rb.botUploadPresigned(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

			var payload map[string]interface{}
			err := json.Unmarshal(w.Body.Bytes(), &payload)
			assert.NoError(t, err, "response body must be JSON: %s", w.Body.String())

			cd, ok := payload["contentDisposition"].(string)
			assert.True(t, ok, "response MUST include contentDisposition field; body: %s", w.Body.String())
			assert.NotEmpty(t, cd, "contentDisposition MUST be non-empty so browser can echo signed value")
			assert.Equal(t, mockFS.lastContentDisp, cd,
				"response contentDisposition MUST match value passed to PresignedPutURL")
			assert.Contains(t, cd, "inline",
				"contentDisposition should contain 'inline' disposition")
		})
	}
}
