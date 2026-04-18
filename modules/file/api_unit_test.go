package file

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestCheckReq(t *testing.T) {
	f := &File{} // checkReq 不依赖 ctx

	tests := []struct {
		name     string
		fileType Type
		path     string
		wantErr  bool
		errMsg   string
	}{
		// 有效请求
		{"chat with path", TypeChat, "/upload/test.jpg", false, ""},
		{"moment with path", TypeMoment, "/upload/img.png", false, ""},
		{"report with path", TypeReport, "/upload/report.jpg", false, ""},
		{"chatbg with path", TypeChatBg, "/upload/bg.jpg", false, ""},
		{"common with path", TypeCommon, "/upload/file.pdf", false, ""},
		{"download with path", TypeDownload, "/download/file.zip", false, ""},

		// TypeMomentCover 和 TypeSticker 可以没有 path
		{"momentcover no path", TypeMomentCover, "", false, ""},
		{"sticker no path", TypeSticker, "", false, ""},
		{"momentcover with path", TypeMomentCover, "/path", false, ""},
		{"sticker with path", TypeSticker, "/path", false, ""},

		// 空文件类型
		{"empty type", "", "/path", true, "文件类型不能为空"},

		// 空路径（非 momentcover/sticker）
		{"chat no path", TypeChat, "", true, "上传路径不能为空"},
		{"moment no path", TypeMoment, "", true, "上传路径不能为空"},
		{"report no path", TypeReport, "", true, "上传路径不能为空"},

		// 无效文件类型
		{"invalid type", Type("invalid"), "/path", true, "文件类型错误"},
		{"workplace banner type", TypeWorkplaceBanner, "/path", false, ""},
		{"workplace icon type", TypeWorkplaceAppIcon, "/path", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := f.checkReq(tt.fileType, tt.path)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestCheckReq_AllValidTypes(t *testing.T) {
	f := &File{}
	validTypes := []Type{
		TypeChat, TypeMoment, TypeMomentCover,
		TypeSticker, TypeReport, TypeChatBg,
		TypeCommon, TypeDownload,
	}

	for _, ft := range validTypes {
		err := f.checkReq(ft, "/path")
		assert.NoError(t, err, "type %s should be valid", ft)
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"normal path", "/chat/image.jpg", false},
		{"simple traversal", "../etc/passwd", true},
		{"encoded traversal", "%2e%2e%2fetc%2fpasswd", true},
		{"double encoded traversal", "%252e%252e%252fetc%252fpasswd", true},
		{"triple encoded traversal", "%25252e%25252e%25252f", true},
		{"clean path", "/chat/subfolder/file.png", false},
		{"empty path", "", false},
		{"path with spaces", "/chat/my file.jpg", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sanitizePath(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestInferContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		ext         string
		want        string
	}{
		{
			name:        "detect markdown from extension",
			contentType: "application/octet-stream",
			ext:         ".md",
			want:        "text/markdown; charset=utf-8",
		},
		{
			name:        "detect plain text from extension",
			contentType: "application/octet-stream",
			ext:         ".txt",
			want:        "text/plain; charset=utf-8",
		},
		{
			name:        "detect css from extension",
			contentType: "application/octet-stream",
			ext:         ".css",
			want:        "text/css; charset=utf-8",
		},
		{
			name:        "detect html from extension",
			contentType: "application/octet-stream",
			ext:         ".html",
			want:        "text/html; charset=utf-8",
		},
		{
			name:        "detect jpeg keeps binary type",
			contentType: "application/octet-stream",
			ext:         ".jpg",
			want:        "image/jpeg",
		},
		{
			name:        "detect png keeps binary type",
			contentType: "application/octet-stream",
			ext:         ".png",
			want:        "image/png",
		},
		{
			name:        "client-provided text type gets charset",
			contentType: "text/plain",
			ext:         ".txt",
			want:        "text/plain; charset=utf-8",
		},
		{
			name:        "client-provided text type with charset unchanged",
			contentType: "text/plain; charset=utf-8",
			ext:         ".txt",
			want:        "text/plain; charset=utf-8",
		},
		{
			name:        "client-provided non-text type preserved",
			contentType: "application/pdf",
			ext:         ".pdf",
			want:        "application/pdf",
		},
		{
			name:        "unknown extension keeps octet-stream",
			contentType: "application/octet-stream",
			ext:         ".xyz123",
			want:        "application/octet-stream",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferContentType(tt.contentType, tt.ext)
			assert.Equal(t, tt.want, got)
		})
	}
}

// mockService implements IService for testing
type mockService struct {
	composeResult map[string]interface{}
	composeErr    error
}

func (m *mockService) DownloadAndMakeCompose(uploadPath string, downloadURLs []string) (map[string]interface{}, error) {
	return m.composeResult, m.composeErr
}

func (m *mockService) DownloadImage(url string, ctx context.Context) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockService) UploadFile(filePath string, contentType string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	return nil, nil
}

func (m *mockService) DownloadURL(path string, filename string) (string, error) {
	return "", nil
}

func (m *mockService) GetFile(path string) (io.ReadCloser, string, error) {
	return nil, "", fmt.Errorf("not implemented")
}

func (m *mockService) PresignedPutURL(objectPath string, contentType string, expires time.Duration) (string, string, error) {
	return "", "", fmt.Errorf("not implemented")
}

func TestMakeImageCompose_SafeTypeAssertion(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		result         map[string]interface{}
		expectedStatus int
		expectError    bool
	}{
		{
			name:           "valid fid string",
			result:         map[string]interface{}{"fid": "abc123"},
			expectedStatus: http.StatusOK,
			expectError:    false,
		},
		{
			name:           "missing fid key",
			result:         map[string]interface{}{},
			expectedStatus: http.StatusBadRequest,
			expectError:    true,
		},
		{
			name:           "fid is nil",
			result:         map[string]interface{}{"fid": nil},
			expectedStatus: http.StatusBadRequest,
			expectError:    true,
		},
		{
			name:           "fid is wrong type (int)",
			result:         map[string]interface{}{"fid": 12345},
			expectedStatus: http.StatusBadRequest,
			expectError:    true,
		},
		{
			name:           "fid is empty string",
			result:         map[string]interface{}{"fid": ""},
			expectedStatus: http.StatusBadRequest,
			expectError:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &File{
				Log: log.NewTLog("FileTest"),
				service: &mockService{
					composeResult: tt.result,
					composeErr:    nil,
				},
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)

			body, _ := json.Marshal([]string{"http://example.com/img1.jpg", "http://example.com/img2.jpg"})
			c.Request, _ = http.NewRequest(http.MethodPost, "/v1/file/compose/test", bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "path", Value: "/test"}}

			wkContext := &wkhttp.Context{Context: c}
			f.makeImageCompose(wkContext)

			assert.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectError {
				var resp map[string]interface{}
				json.Unmarshal(w.Body.Bytes(), &resp)
				assert.Contains(t, resp, "msg")
			} else {
				var resp map[string]interface{}
				json.Unmarshal(w.Body.Bytes(), &resp)
				assert.Equal(t, tt.result["fid"], resp["path"])
			}
		})
	}
}
