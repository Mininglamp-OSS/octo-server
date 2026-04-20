package botfather

import (
	"context"
	"encoding/json"
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
	lastObjectPath string
}

func (m *mockFileServiceForUpload) UploadFile(filePath string, contentType string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	return nil, nil
}

func (m *mockFileServiceForUpload) DownloadURL(path string, filename string) (string, error) {
	return "", nil
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

func (m *mockFileServiceForUpload) PresignedPutURL(objectPath string, contentType string, expires time.Duration) (string, string, error) {
	m.lastObjectPath = objectPath
	return "https://example.com/upload?" + objectPath, "https://example.com/download/" + objectPath, nil
}

func TestBotUploadPresigned_FilenameSanitization(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name            string
		filename        string
		wantKeyContains string
	}{
		{
			name:            "normal filename",
			filename:        "test.jpg",
			wantKeyContains: "test.jpg",
		},
		{
			name:            "path traversal attack",
			filename:        "../../etc/passwd.jpg",
			wantKeyContains: "passwd.jpg",
		},
		{
			name:            "absolute path",
			filename:        "/var/log/secret.png",
			wantKeyContains: "secret.png",
		},
		{
			name:            "nested path",
			filename:        "subdir/nested/file.pdf",
			wantKeyContains: "file.pdf",
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

			assert.True(t, strings.HasSuffix(key, tt.wantKeyContains),
				"key %q should end with %q", key, tt.wantKeyContains)

			assert.NotContains(t, key, "%2F",
				"key should not contain encoded path separators")
			assert.NotContains(t, key, "..",
				"key should not contain path traversal sequences")
		})
	}
}
