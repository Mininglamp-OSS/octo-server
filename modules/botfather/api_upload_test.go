package botfather

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
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
	lastObjectPath       string
	lastContentDisp      string
	lastContentType      string
	lastUploadPath       string
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

func (m *mockFileServiceForUpload) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	return "https://example.com/download/" + objectPath, nil
}

func (m *mockFileServiceForUpload) PresignedPutURL(objectPath string, contentType string, contentDisposition string, expires time.Duration) (string, string, error) {
	m.lastObjectPath = objectPath
	m.lastContentDisp = contentDisposition
	m.lastContentType = contentType
	return "https://example.com/upload?" + objectPath, "https://example.com/download/" + objectPath, nil
}

func (m *mockFileServiceForUpload) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	return "https://example.com/signed-get/" + objectPath, nil
}

func TestBotUploadPresigned_FilenameSanitization(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name    string
		filename string
		wantExt string
	}{
		{
			name:    "normal filename",
			filename: "test.jpg",
			wantExt: ".jpg",
		},
		{
			name:    "path traversal attack",
			filename: "../../etc/passwd.jpg",
			wantExt: ".jpg",
		},
		{
			name:    "absolute path",
			filename: "/var/log/secret.png",
			wantExt: ".png",
		},
		{
			name:    "nested path",
			filename: "subdir/nested/file.pdf",
			wantExt: ".pdf",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := &mockFileServiceForUpload{}
			bf := &BotFather{
				Log:         log.NewTLog("BotFatherTest"),
				fileService: mockFS,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/bot/upload/presigned?filename="+tt.filename, nil)

			wkCtx := &wkhttp.Context{Context: c}
			bf.botUploadPresigned(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code)

			var resp map[string]interface{}
			err := json.Unmarshal(w.Body.Bytes(), &resp)
			assert.NoError(t, err)

			key, ok := resp["key"].(string)
			assert.True(t, ok, "response should contain 'key' field")

			// UUID-based key should end with the correct extension
			assert.True(t, strings.HasSuffix(key, tt.wantExt),
				"key %q should end with %q", key, tt.wantExt)

			assert.NotContains(t, key, "%2F",
				"key should not contain encoded path separators")
			assert.NotContains(t, key, "..",
				"key should not contain path traversal sequences")
		})
	}
}

func TestBotUploadPresigned_ContentDisposition(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name     string
		filename string
		wantCD   bool
	}{
		{"ascii filename", "report.pdf", true},
		{"chinese filename", "报告.pdf", true},
		{"mixed filename", "report-报告.pdf", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := &mockFileServiceForUpload{}
			bf := &BotFather{
				Log:         log.NewTLog("BotFatherTest"),
				fileService: mockFS,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/bot/upload/presigned?filename="+tt.filename, nil)

			wkCtx := &wkhttp.Context{Context: c}
			bf.botUploadPresigned(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code)

			if tt.wantCD {
				assert.Contains(t, mockFS.lastContentDisp, "inline",
					"contentDisposition should contain 'inline'")
				assert.Contains(t, mockFS.lastContentDisp, "filename*=UTF-8''",
					"contentDisposition should contain RFC 5987 encoded filename")
			}
		})
	}
}

func TestBotUploadFile_UUIDPath_MIME_ContentDisposition(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name            string
		filename        string
		wantExt         string
		wantContentType string
		wantCDContains  string
	}{
		{
			name:            "jpg file gets correct MIME and content-disposition",
			filename:        "photo.jpg",
			wantExt:         ".jpg",
			wantContentType: "image/jpeg",
			wantCDContains:  "photo.jpg",
		},
		{
			name:            "pdf file gets correct MIME",
			filename:        "document.pdf",
			wantExt:         ".pdf",
			wantContentType: "application/pdf",
			wantCDContains:  "document.pdf",
		},
		{
			name:            "chinese filename uses UUID path",
			filename:        "报告.pdf",
			wantExt:         ".pdf",
			wantContentType: "application/pdf",
			wantCDContains:  "UTF-8''",
		},
		{
			name:            "unknown extension falls back to octet-stream",
			filename:        "data.xyz123",
			wantExt:         ".xyz123",
			wantContentType: "application/octet-stream",
			wantCDContains:  "data.xyz123",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockFS := &mockFileServiceForUpload{}
			bf := &BotFather{
				Log:         log.NewTLog("BotFatherTest"),
				fileService: mockFS,
			}

			// Build multipart form
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", tt.filename)
			assert.NoError(t, err)
			part.Write([]byte("fake file content"))
			writer.Close()

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodPost, "/v1/bot/upload/file?type=chat", &body)
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())

			wkCtx := &wkhttp.Context{Context: c}
			bf.botUploadFile(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

			// UUID-based path: should NOT contain the original filename
			assert.NotContains(t, mockFS.lastUploadPath, tt.filename,
				"upload path should use UUID, not original filename")
			assert.True(t, strings.HasSuffix(mockFS.lastUploadPath, tt.wantExt),
				"upload path should end with %s, got: %s", tt.wantExt, mockFS.lastUploadPath)

			// Correct MIME type
			assert.Equal(t, tt.wantContentType, mockFS.lastUploadContentType,
				"content type mismatch")

			// Content-Disposition set
			assert.Contains(t, mockFS.lastUploadContentDisp, "inline",
				"contentDisposition should contain 'inline'")
			assert.Contains(t, mockFS.lastUploadContentDisp, tt.wantCDContains,
				"contentDisposition should reference the original filename")
		})
	}
}
