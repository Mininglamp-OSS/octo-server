package bot_api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

// mockFileServiceForPresigned is a minimal IService satisfying the
// `fileService` field for botUploadPresigned. It captures what the
// handler passed to PresignedPutURL so the test can assert the
// browser-facing response echoes the same Content-Disposition.
type mockFileServiceForPresigned struct {
	lastObjectPath  string
	lastContentDisp string
	lastContentType string
}

func (m *mockFileServiceForPresigned) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	return nil, nil
}

func (m *mockFileServiceForPresigned) DownloadURL(path string, filename string) (string, error) {
	return fmt.Sprintf("https://example.com/download/%s", path), nil
}

func (m *mockFileServiceForPresigned) GetFile(path string) (io.ReadCloser, string, error) {
	return nil, "", nil
}

func (m *mockFileServiceForPresigned) DownloadAndMakeCompose(uploadPath string, downloadURLs []string) (map[string]interface{}, error) {
	return nil, nil
}

func (m *mockFileServiceForPresigned) DownloadImage(u string, ctx context.Context) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockFileServiceForPresigned) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	return "https://example.com/signed-get/" + objectPath, nil
}

func (m *mockFileServiceForPresigned) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (string, string, error) {
	m.lastObjectPath = objectPath
	m.lastContentType = contentType
	m.lastContentDisp = contentDisposition
	return "https://example.com/upload?" + objectPath, "https://example.com/download/" + objectPath, nil
}

// TestBotAPIUploadPresigned_ContentDispositionInResponse asserts that the
// /v1/bot/upload/presigned endpoint returns the contentDisposition value
// the server signed (parity with the main file endpoint at
// modules/file/api.go). Without it, browsers cannot construct a PUT that
// passes SigV4 — every upload of a non-ASCII / spaced filename returns
// 403 SignatureDoesNotMatch.
//
// Independently flagged as Critical by Jerry-Xin and lml2468 in PR#50 R7.
func TestBotAPIUploadPresigned_ContentDispositionInResponse(t *testing.T) {
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
			mockFS := &mockFileServiceForPresigned{}
			ba := &BotAPI{
				fileService: mockFS,
				Log:         log.NewTLog("BotAPI-test"),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			q := url.Values{}
			q.Set("fileSize", "1024")
			q.Set("filename", tt.filename)
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/bot/upload/presigned?"+q.Encode(), nil)

			wkCtx := &wkhttp.Context{Context: c}
			ba.botUploadPresigned(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

			var payload map[string]interface{}
			err := json.Unmarshal(w.Body.Bytes(), &payload)
			assert.NoError(t, err, "response body must be JSON: %s", w.Body.String())

			cd, ok := payload["contentDisposition"].(string)
			assert.True(t, ok, "response MUST include contentDisposition field for non-ASCII / spaced filenames; body: %s", w.Body.String())
			assert.NotEmpty(t, cd, "contentDisposition MUST be non-empty so browser can echo signed value")
			assert.Equal(t, mockFS.lastContentDisp, cd,
				"response contentDisposition MUST match what was signed into PresignedPutURL")
			assert.True(t, strings.Contains(cd, "inline"),
				"contentDisposition should include the 'inline' disposition")
		})
	}
}
