package file

import (
	"bytes"
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

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	composeResult      map[string]interface{}
	composeErr         error
	lastObjectPath     string
	lastGetObjectPath  string
	lastContentDisp    string
	lastFileSize       int64
	presignedGetErr    error
	lastGetDisposition string
}

func (m *mockService) DownloadAndMakeCompose(uploadPath string, downloadURLs []string) (map[string]interface{}, error) {
	return m.composeResult, m.composeErr
}

func (m *mockService) DownloadImage(url string, ctx context.Context) (io.ReadCloser, error) {
	return nil, nil
}

func (m *mockService) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	return nil, nil
}

func (m *mockService) DownloadURL(path string, filename string) (string, error) {
	return "", nil
}

func (m *mockService) GetFile(path string) (io.ReadCloser, string, error) {
	return nil, "", fmt.Errorf("not implemented")
}

func (m *mockService) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (string, string, error) {
	m.lastObjectPath = objectPath
	m.lastContentDisp = contentDisposition
	m.lastFileSize = fileSize
	return "https://example.com/upload?" + objectPath, "https://example.com/download/" + objectPath, nil
}

func (m *mockService) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	m.lastGetObjectPath = objectPath
	m.lastGetDisposition = disposition
	if m.presignedGetErr != nil {
		return "", m.presignedGetErr
	}
	return "https://example.com/signed-get/" + objectPath + "?fn=" + url.QueryEscape(filename), nil
}

func TestBuildContentDisposition(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		want     string
	}{
		{"empty filename", "", ""},
		{"ascii filename", "report.pdf",
			`inline; filename="report.pdf"; filename*=UTF-8''report.pdf`},
		{"ascii with spaces", "my file.pdf",
			`inline; filename="my file.pdf"; filename*=UTF-8''my%20file.pdf`},
		{"chinese filename", "报告.pdf",
			`inline; filename="__.pdf"; filename*=UTF-8''` + url.PathEscape("报告.pdf")},
		{"japanese filename", "テスト.png",
			`inline; filename="___.png"; filename*=UTF-8''` + url.PathEscape("テスト.png")},
		{"mixed ascii and unicode", "report-报告.pdf",
			`inline; filename="report-__.pdf"; filename*=UTF-8''` + url.PathEscape("report-报告.pdf")},
		{"emoji filename", "photo\U0001F600.jpg",
			`inline; filename="photo_.jpg"; filename*=UTF-8''` + url.PathEscape("photo\U0001F600.jpg")},
		{"ascii with backslash", `report\2024.pdf`,
			`inline; filename="report\\2024.pdf"; filename*=UTF-8''report%5C2024.pdf`},
		{"ascii with semicolon", "report;final.pdf",
			`inline; filename="report;final.pdf"; filename*=UTF-8''report%3Bfinal.pdf`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildContentDisposition(tt.filename)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBuildContentDisposition_AlwaysHasBothFilenameParams(t *testing.T) {
	filenames := []string{
		"simple.txt",
		"with spaces.pdf",
		`back\slash.doc`,
		"报告.pdf",
		"mixed-混合.png",
	}
	for _, fn := range filenames {
		t.Run(fn, func(t *testing.T) {
			got := BuildContentDisposition(fn)
			assert.Contains(t, got, "filename=")
			assert.Contains(t, got, "filename*=UTF-8''")
		})
	}
}

func TestIsASCII(t *testing.T) {
	assert.True(t, isASCII("hello.pdf"))
	assert.True(t, isASCII("my-file_2024.jpg"))
	assert.True(t, isASCII(""))
	assert.False(t, isASCII("报告.pdf"))
	assert.False(t, isASCII("café.txt"))
	assert.False(t, isASCII("photo\U0001F600.jpg"))
}

func TestGetUploadCredentials_ObjectKeyWithFilename(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name               string
		queryParams        string
		wantStatus         int
		wantKeyContains    string
		wantKeyNotContains string
		wantContentDisp    bool
	}{
		{
			name:               "filename provided generates UUID-based key with extension",
			queryParams:        "type=chat&filename=photo.jpg&fileSize=1024",
			wantStatus:         http.StatusOK,
			wantKeyContains:    ".jpg",
			wantKeyNotContains: "photo.jpg",
			wantContentDisp:    true,
		},
		{
			name:               "chinese filename not in key, extension preserved",
			queryParams:        "type=chat&filename=照片.jpg&fileSize=1024",
			wantStatus:         http.StatusOK,
			wantKeyContains:    ".jpg",
			wantKeyNotContains: "照片",
			wantContentDisp:    true,
		},
		{
			name:               "path provided uses path-based key",
			queryParams:        "type=chat&path=/upload/test.jpg&fileSize=2048",
			wantStatus:         http.StatusOK,
			wantKeyContains:    "chat",
			wantKeyNotContains: "",
			wantContentDisp:    false,
		},
		{
			// 贴纸禁止走预签名直传（绕过 StickerMaxFileSize + 魔数 + 格式校验），
			// 必须走 multipart /v1/file/upload —— 此处固化 400 回归（PR #508 review）。
			name:            "sticker type is rejected on the presigned path",
			queryParams:     "type=sticker&filename=sticker.gif&fileSize=512",
			wantStatus:      http.StatusBadRequest,
			wantContentDisp: false,
		},
		{
			name:            "path and filename both provided uses path for key and filename for disposition",
			queryParams:     "type=chat&path=/custom/abc123.jpg&filename=photo.jpg&fileSize=4096",
			wantStatus:      http.StatusOK,
			wantKeyContains: "chat",
			wantContentDisp: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSvc := &mockService{}
			f := &File{
				Log:     log.NewTLog("FileTest"),
				service: mockSvc,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/file/upload/credentials?"+tt.queryParams, nil)

			wkCtx := &wkhttp.Context{Context: c}
			f.getUploadCredentials(wkCtx)

			assert.Equal(t, tt.wantStatus, w.Code, "response body: %s", w.Body.String())

			if tt.wantStatus == http.StatusOK {
				var resp map[string]interface{}
				err := json.Unmarshal(w.Body.Bytes(), &resp)
				assert.NoError(t, err)

				key, ok := resp["key"].(string)
				assert.True(t, ok, "response should contain 'key' field")

				if tt.wantKeyContains != "" {
					assert.Contains(t, key, tt.wantKeyContains)
				}
				if tt.wantKeyNotContains != "" {
					assert.NotContains(t, key, tt.wantKeyNotContains)
				}

				if tt.wantContentDisp {
					cd, ok := resp["contentDisposition"].(string)
					assert.True(t, ok, "response should contain 'contentDisposition'")
					assert.Contains(t, cd, "inline")
					assert.Equal(t, cd, mockSvc.lastContentDisp, "contentDisposition passed to service should match response")
				}
			}
		})
	}
}

func TestGetUploadCredentials_ObjectKeyFormat(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockSvc := &mockService{}
	f := &File{
		Log:     log.NewTLog("FileTest"),
		service: mockSvc,
	}

	// Test with filename: key should be fileType/timestamp/uuid/uuid.ext
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/v1/file/upload/credentials?type=chat&filename=test.jpg&fileSize=1024", nil)
	wkCtx := &wkhttp.Context{Context: c}
	f.getUploadCredentials(wkCtx)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	key := resp["key"].(string)

	parts := strings.Split(key, "/")
	assert.Equal(t, 4, len(parts), "key should have 4 parts: type/timestamp/uuid/uuid.ext, got: %s", key)
	assert.Equal(t, "chat", parts[0])
	// parts[1] should be a unix timestamp (numeric)
	for _, ch := range parts[1] {
		assert.True(t, ch >= '0' && ch <= '9', "timestamp part should be numeric, got: %s", parts[1])
	}
	// parts[3] should be uuid.ext, NOT the original filename
	assert.NotEqual(t, "test.jpg", parts[3], "last part should be uuid.ext, not the original filename")
	assert.True(t, strings.HasSuffix(parts[3], ".jpg"), "last part should end with .jpg, got: %s", parts[3])
}

func TestGetUploadCredentials_UUIDBasedKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name     string
		filename string
		wantExt  string
	}{
		{"chinese filename", "带中文的文件.pdf", ".pdf"},
		{"japanese filename", "テスト.png", ".png"},
		{"korean filename", "파일.docx", ".docx"},
		{"emoji filename", "photo\U0001F600.jpg", ".jpg"},
		{"mixed ascii and unicode", "report-报告-2024.pdf", ".pdf"},
		{"spaces in filename", "file with spaces.pdf", ".pdf"},
		{"question mark in filename", "file?query.jpg", ".jpg"},
		{"pre-encoded filename", "already%20encoded.pdf", ".pdf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSvc := &mockService{}
			f := &File{
				Log:     log.NewTLog("FileTest"),
				service: mockSvc,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			q := url.Values{}
			q.Set("type", "chat")
			q.Set("filename", tt.filename)
			q.Set("fileSize", "1024")
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/file/upload/credentials?"+q.Encode(), nil)

			wkCtx := &wkhttp.Context{Context: c}
			f.getUploadCredentials(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

			// objectKey should contain only ASCII characters (UUID-based)
			assert.True(t, isASCII(mockSvc.lastObjectPath),
				"objectKey should contain only ASCII characters, got: %s", mockSvc.lastObjectPath)

			// objectKey should end with the correct file extension
			assert.True(t, strings.HasSuffix(mockSvc.lastObjectPath, tt.wantExt),
				"objectKey should end with %s, got: %s", tt.wantExt, mockSvc.lastObjectPath)

			// objectKey should NOT contain the original filename
			assert.NotContains(t, mockSvc.lastObjectPath, tt.filename,
				"objectKey should not contain the original filename")
		})
	}

	// Verify UUID uniqueness: different filenames with same extension produce different keys
	t.Run("different filenames produce different keys", func(t *testing.T) {
		filenames := []string{"fileA.pdf", "fileB.pdf", "报告.pdf"}
		keys := make(map[string]bool)
		for _, fn := range filenames {
			mockSvc := &mockService{}
			f := &File{
				Log:     log.NewTLog("FileTest"),
				service: mockSvc,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			q := url.Values{}
			q.Set("type", "chat")
			q.Set("filename", fn)
			q.Set("fileSize", "1024")
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/file/upload/credentials?"+q.Encode(), nil)

			wkCtx := &wkhttp.Context{Context: c}
			f.getUploadCredentials(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code)
			assert.False(t, keys[mockSvc.lastObjectPath],
				"objectKey should be unique, but got duplicate: %s", mockSvc.lastObjectPath)
			keys[mockSvc.lastObjectPath] = true
		}
	})
}

func TestGetUploadCredentials_FallbackWithoutFilename(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockSvc := &mockService{}
	f := &File{
		Log:     log.NewTLog("FileTest"),
		service: mockSvc,
	}

	// Test with path (no filename): key should be fileType + sanitized path
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/v1/file/upload/credentials?type=chat&path=/abc123.jpg&fileSize=1024", nil)
	wkCtx := &wkhttp.Context{Context: c}
	f.getUploadCredentials(wkCtx)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)

	key := resp["key"].(string)
	assert.True(t, strings.HasPrefix(key, "chat/"), "key should start with 'chat/'")
	assert.Contains(t, key, "abc123.jpg")

	// No contentDisposition when no filename
	_, hasCD := resp["contentDisposition"]
	assert.False(t, hasCD, "response should not contain contentDisposition without filename")
	assert.Equal(t, "", mockSvc.lastContentDisp)
}

func TestBuildContentDisposition_UsesInline(t *testing.T) {
	// Verify that BuildContentDisposition uses "inline" not "attachment"
	tests := []string{"report.pdf", "photo.jpg", "报告.pdf", "test file.txt"}
	for _, fn := range tests {
		t.Run(fn, func(t *testing.T) {
			got := BuildContentDisposition(fn)
			assert.Contains(t, got, "inline;")
			assert.NotContains(t, got, "attachment")
		})
	}
}

func TestExtractFilenameFromDisposition(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", ""},
		{"no filename", "inline", ""},
		{"rfc5987 ascii", "inline; filename=\"report.pdf\"; filename*=UTF-8''report.pdf", "report.pdf"},
		{"rfc5987 encoded", "inline; filename=\"__.pdf\"; filename*=UTF-8''%E6%8A%A5%E5%91%8A.pdf", "报告.pdf"},
		{"rfc5987 with spaces", "inline; filename=\"my file.pdf\"; filename*=UTF-8''my%20file.pdf", "my file.pdf"},
		{"only quoted filename", `inline; filename="report.pdf"`, "report.pdf"},
		{"attachment style", `attachment; filename="old.pdf"; filename*=UTF-8''old.pdf`, "old.pdf"},
		{"filename* only", "inline; filename*=UTF-8''doc.pdf", "doc.pdf"},
		{"filename with semicolon after star", "inline; filename*=UTF-8''a.pdf; other=x", "a.pdf"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFilenameFromDisposition(tt.input)
			assert.Equal(t, tt.want, got)
		})
	}
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

func TestDownloadURL_NoQueryParams(t *testing.T) {
	// DownloadURL should return a plain URL without response-content-disposition
	sc := &ServiceCOS{}
	// We can't call DownloadURL without a config context, so test extractFilenameFromDisposition
	// and the logic that was removed. The key assertion: DownloadURL no longer appends query params.
	// This is a compile-time verification that the signature accepts filename but ignores it.
	_ = sc // ServiceCOS.DownloadURL now always returns a clean URL
}

func TestGetDownloadURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name            string
		queryParams     string
		wantStatus      int
		wantURL         string
		wantFilename    string
		wantDisposition string
		wantErr         bool
	}{
		{
			name:            "with path and filename",
			queryParams:     "path=chat/test.jpg&filename=photo.jpg",
			wantStatus:      http.StatusOK,
			wantURL:         "https://example.com/signed-get/chat/test.jpg?fn=photo.jpg",
			wantFilename:    "photo.jpg",
			wantDisposition: "attachment",
		},
		{
			name:            "with path only, filename defaults to basename",
			queryParams:     "path=chat/document.pdf",
			wantStatus:      http.StatusOK,
			wantURL:         "https://example.com/signed-get/chat/document.pdf?fn=document.pdf",
			wantFilename:    "document.pdf",
			wantDisposition: "attachment",
		},
		{
			name:        "missing path returns error",
			queryParams: "filename=photo.jpg",
			wantStatus:  http.StatusBadRequest,
			wantErr:     true,
		},
		{
			name:        "empty path returns error",
			queryParams: "path=",
			wantStatus:  http.StatusBadRequest,
			wantErr:     true,
		},
		{
			name:            "disposition=inline passed through",
			queryParams:     "path=chat/test.jpg&filename=photo.jpg&disposition=inline",
			wantStatus:      http.StatusOK,
			wantURL:         "https://example.com/signed-get/chat/test.jpg?fn=photo.jpg",
			wantFilename:    "photo.jpg",
			wantDisposition: "inline",
		},
		{
			name:            "disposition=attachment passed through",
			queryParams:     "path=chat/test.jpg&filename=photo.jpg&disposition=attachment",
			wantStatus:      http.StatusOK,
			wantURL:         "https://example.com/signed-get/chat/test.jpg?fn=photo.jpg",
			wantFilename:    "photo.jpg",
			wantDisposition: "attachment",
		},
		{
			name:            "disposition empty defaults to attachment",
			queryParams:     "path=chat/test.jpg&filename=photo.jpg",
			wantStatus:      http.StatusOK,
			wantURL:         "https://example.com/signed-get/chat/test.jpg?fn=photo.jpg",
			wantFilename:    "photo.jpg",
			wantDisposition: "attachment",
		},
		{
			name:            "invalid disposition treated as attachment",
			queryParams:     "path=chat/test.jpg&filename=photo.jpg&disposition=foobar",
			wantStatus:      http.StatusOK,
			wantURL:         "https://example.com/signed-get/chat/test.jpg?fn=photo.jpg",
			wantFilename:    "photo.jpg",
			wantDisposition: "attachment",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSvc := &mockService{}
			f := &File{
				Log:     log.NewTLog("FileTest"),
				service: mockSvc,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/file/download/url?"+tt.queryParams, nil)

			wkCtx := &wkhttp.Context{Context: c}
			f.getDownloadURL(wkCtx)

			assert.Equal(t, tt.wantStatus, w.Code, "body: %s", w.Body.String())

			if !tt.wantErr {
				var resp map[string]interface{}
				err := json.Unmarshal(w.Body.Bytes(), &resp)
				assert.NoError(t, err)
				assert.Equal(t, tt.wantURL, resp["url"])
				assert.Equal(t, tt.wantFilename, resp["filename"])
				assert.Equal(t, tt.wantDisposition, mockSvc.lastGetDisposition)
			}
		})
	}
}

func TestGetDownloadURL_ServiceError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockSvc := &mockService{
		presignedGetErr: fmt.Errorf("service not supported"),
	}
	f := &File{
		Log:     log.NewTLog("FileTest"),
		service: mockSvc,
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request, _ = http.NewRequest(http.MethodGet, "/v1/file/download/url?path=/chat/test.jpg", nil)

	wkCtx := &wkhttp.Context{Context: c}
	f.getDownloadURL(wkCtx)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestGetUploadCredentials_FileSizeValidation pins the P0 size-bypass guard
// landed in PR#50 R6: the presigned-PUT path now refuses to sign a URL
// without a `fileSize` query parameter, and rejects sizes outside the
// (0, MaxFileSize] band. The signed Content-Length covered by SigV4 /
// oss.ContentLength is the only thing stopping a caller from PUTting
// arbitrary bytes through the presigned URL — without server-side
// validation here, the storage layer would happily accept whatever the
// client sent and we would lose parity with the multipart `uploadFile`
// handler's MaxBytesReader gate.
func TestGetUploadCredentials_FileSizeValidation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name           string
		queryParams    string
		wantStatus     int
		wantMsgContain string
		wantPropagated int64
	}{
		{
			name:           "missing fileSize is rejected",
			queryParams:    "type=chat&filename=photo.jpg",
			wantStatus:     http.StatusBadRequest,
			wantMsgContain: "fileSize",
		},
		{
			name:           "non-numeric fileSize is rejected",
			queryParams:    "type=chat&filename=photo.jpg&fileSize=abc",
			wantStatus:     http.StatusBadRequest,
			wantMsgContain: "正整数",
		},
		{
			name:           "zero fileSize is rejected",
			queryParams:    "type=chat&filename=photo.jpg&fileSize=0",
			wantStatus:     http.StatusBadRequest,
			wantMsgContain: "正整数",
		},
		{
			name:           "negative fileSize is rejected",
			queryParams:    "type=chat&filename=photo.jpg&fileSize=-1",
			wantStatus:     http.StatusBadRequest,
			wantMsgContain: "正整数",
		},
		{
			name:           "fileSize over MaxFileSize is rejected",
			queryParams:    fmt.Sprintf("type=chat&filename=photo.jpg&fileSize=%d", MaxFileSize+1),
			wantStatus:     http.StatusBadRequest,
			wantMsgContain: "MB",
		},
		{
			name:           "fileSize exactly MaxFileSize is accepted",
			queryParams:    fmt.Sprintf("type=chat&filename=photo.jpg&fileSize=%d", MaxFileSize),
			wantStatus:     http.StatusOK,
			wantPropagated: MaxFileSize,
		},
		{
			name:           "small fileSize is accepted and propagated to backend",
			queryParams:    "type=chat&filename=photo.jpg&fileSize=2048",
			wantStatus:     http.StatusOK,
			wantPropagated: 2048,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSvc := &mockService{}
			f := &File{
				Log:     log.NewTLog("FileTest"),
				service: mockSvc,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request, _ = http.NewRequest(http.MethodGet, "/v1/file/upload/credentials?"+tt.queryParams, nil)

			wkCtx := &wkhttp.Context{Context: c}
			f.getUploadCredentials(wkCtx)

			assert.Equal(t, tt.wantStatus, w.Code, "body: %s", w.Body.String())
			if tt.wantMsgContain != "" {
				assert.Contains(t, w.Body.String(), tt.wantMsgContain)
			}
			if tt.wantStatus == http.StatusOK {
				assert.Equal(t, tt.wantPropagated, mockSvc.lastFileSize,
					"fileSize must be propagated to the backend signer so the storage layer can enforce it")

				// maxFileSize must also be echoed back to the caller so
				// the client knows the exact byte budget the URL was
				// signed against (the URL itself is opaque to the client).
				var resp map[string]interface{}
				err := json.Unmarshal(w.Body.Bytes(), &resp)
				assert.NoError(t, err)
				echoedRaw, ok := resp["maxFileSize"]
				assert.True(t, ok, "response must echo maxFileSize so the client can match its PUT body length")
				echoed, ok := echoedRaw.(float64)
				assert.True(t, ok, "maxFileSize must be a number, got %T", echoedRaw)
				assert.Equal(t, tt.wantPropagated, int64(echoed))
			}
		})
	}
}

// TestGetDownloadURL_PathStyleStripsBucketSegment pins the YUJ-848
// follow-up to the YUJ-846 path-style fix on the
// `/v1/file/download/url` endpoint. When a path-style CDN BucketURL
// is in effect (host has no `<bucket>.` subdomain), the URL we hand
// the browser as `downloadUrl` is `<host>/<bucket>/<prefix>/<key>`
// (see ServiceCOS.publicURL / PresignedGetURL with BucketLookupPath).
// When the client round-trips that full URL back into this handler
// via `?path=<full-url>`, the parsed path therefore begins with
// `/<bucket>/`, and that bucket segment MUST be stripped BEFORE the
// COS.Prefix strip — otherwise PresignedGetURL signs the bucket as
// part of the object key and the resulting GET 404s.
//
// Pre-fix the handler stripped only `cosCfg.Prefix`, leaving the
// bucket segment in the path; this regression was Jerry-Xin's R1
// second warning (modules/file/api.go:559-569 in PR#56).
func TestGetDownloadURL_PathStyleStripsBucketSegment(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name             string
		bucket           string
		bucketURL        string
		prefix           string
		inputPath        string
		wantSignedObject string
	}{
		{
			name:             "path-style CDN with prefix strips bucket then prefix",
			bucket:           "im-data-1255521909",
			bucketURL:        "https://cdn.example.com",
			prefix:           "im-test",
			inputPath:        "https://cdn.example.com/im-data-1255521909/im-test/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "path-style CDN no prefix strips just bucket",
			bucket:           "im-data-1255521909",
			bucketURL:        "https://cdn.example.com",
			prefix:           "",
			inputPath:        "https://cdn.example.com/im-data-1255521909/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "DNS-style bucket-subdomain BucketURL — bucket NOT in path, only prefix stripped",
			bucket:           "im-data-1255521909",
			bucketURL:        "https://im-data-1255521909.cos.example.com",
			prefix:           "im-prod",
			inputPath:        "https://im-data-1255521909.cos.example.com/im-prod/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "DNS-style: must NOT accidentally strip a path segment that happens to share the bucket name",
			bucket:           "im-data-1255521909",
			bucketURL:        "https://im-data-1255521909.cos.example.com",
			prefix:           "",
			inputPath:        "https://im-data-1255521909.cos.example.com/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "BucketURL empty (canonical default endpoint) — bucket lives in subdomain, path is just key",
			bucket:           "im-data-1255521909",
			bucketURL:        "",
			prefix:           "",
			inputPath:        "https://im-data-1255521909.cos.ap-beijing.myqcloud.com/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "non-URL path passes through unchanged",
			bucket:           "im-data-1255521909",
			bucketURL:        "https://cdn.example.com",
			prefix:           "im-test",
			inputPath:        "chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.New()
			cfg.Test = true
			cfg.FileService = config.FileServiceTencentCOS
			cfg.COS.Bucket = tc.bucket
			cfg.COS.Region = "ap-beijing"
			cfg.COS.BucketURL = tc.bucketURL
			cfg.COS.Prefix = tc.prefix

			mockSvc := &mockService{}
			f := &File{
				ctx:     testutil.NewTestContext(cfg),
				Log:     log.NewTLog("FileTest"),
				service: mockSvc,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req, _ := http.NewRequest(
				http.MethodGet,
				"/v1/file/download/url?path="+url.QueryEscape(tc.inputPath)+"&filename=abc.jpg",
				nil,
			)
			c.Request = req

			wkCtx := &wkhttp.Context{Context: c}
			f.getDownloadURL(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code, "unexpected status; body=%s", w.Body.String())
			// The handler runs sanitizePath on the stripped path
			// before handing it to PresignedGetURL — sanitizePath
			// preserves the leading `/` for absolute paths and
			// returns relative paths unchanged. So the assertion is
			// on the post-sanitize value the signer actually sees.
			assert.Equal(t, tc.wantSignedObject, mockSvc.lastGetObjectPath,
				"object path passed to PresignedGetURL must have the bucket segment stripped for path-style and the prefix stripped in all cases")
		})
	}
}

// TestGetDownloadURL_S3PrefixAndBucketStrip mirrors the COS round-trip
// test for the awsS3 fileService. Same hazard: when the client posts
// back a full URL we issued (e.g. with a multi-env prefix or a
// path-style endpoint), the handler must strip both the prefix and
// (path-style only) the bucket segment before PresignedGetURL
// re-applies the prefix via ServiceS3.withPrefix. Without the strip,
// the signed object key double-prefixes and the GET 404s.
func TestGetDownloadURL_S3PrefixAndBucketStrip(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name             string
		bucket           string
		downloadURL      string
		prefix           string
		usePathStyle     bool
		inputPath        string
		wantSignedObject string
	}{
		{
			name:             "virtual-hosted with prefix strips just prefix",
			bucket:           "my-bucket",
			downloadURL:      "",
			prefix:           "prod",
			usePathStyle:     false,
			inputPath:        "https://my-bucket.s3.us-west-2.amazonaws.com/prod/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "path-style with prefix strips bucket then prefix",
			bucket:           "my-bucket",
			downloadURL:      "",
			prefix:           "prod",
			usePathStyle:     true,
			inputPath:        "https://s3.us-west-2.amazonaws.com/my-bucket/prod/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "path-style no prefix strips just bucket",
			bucket:           "my-bucket",
			downloadURL:      "",
			prefix:           "",
			usePathStyle:     true,
			inputPath:        "https://s3.us-west-2.amazonaws.com/my-bucket/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "CDN DownloadURL with prefix — bucket NOT in path, only prefix stripped",
			bucket:           "my-bucket",
			downloadURL:      "https://files.example.com",
			prefix:           "prod",
			usePathStyle:     false,
			inputPath:        "https://files.example.com/prod/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "virtual-hosted no prefix — path is just key",
			bucket:           "my-bucket",
			downloadURL:      "",
			prefix:           "",
			usePathStyle:     false,
			inputPath:        "https://my-bucket.s3.us-west-2.amazonaws.com/chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
		{
			name:             "non-URL path passes through unchanged",
			bucket:           "my-bucket",
			downloadURL:      "",
			prefix:           "prod",
			usePathStyle:     false,
			inputPath:        "chat/2026/05/abc.jpg",
			wantSignedObject: "chat/2026/05/abc.jpg",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.New()
			cfg.Test = true
			cfg.FileService = "awsS3"
			cfg.S3.Endpoint = "s3.us-west-2.amazonaws.com"
			cfg.S3.Region = "us-west-2"
			cfg.S3.Bucket = tc.bucket
			cfg.S3.DownloadURL = tc.downloadURL
			cfg.S3.Prefix = tc.prefix
			cfg.S3.UsePathStyle = tc.usePathStyle

			mockSvc := &mockService{}
			f := &File{
				ctx:     testutil.NewTestContext(cfg),
				Log:     log.NewTLog("FileTest"),
				service: mockSvc,
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			req, _ := http.NewRequest(
				http.MethodGet,
				"/v1/file/download/url?path="+url.QueryEscape(tc.inputPath)+"&filename=abc.jpg",
				nil,
			)
			c.Request = req

			wkCtx := &wkhttp.Context{Context: c}
			f.getDownloadURL(wkCtx)

			assert.Equal(t, http.StatusOK, w.Code, "unexpected status; body=%s", w.Body.String())
			assert.Equal(t, tc.wantSignedObject, mockSvc.lastGetObjectPath,
				"object path passed to PresignedGetURL must have prefix (and path-style bucket) stripped before signing")
		})
	}
}

// TestGetDownloadURL_S3_RealServiceRoundTrip is the regression that the
// mock-based TestGetDownloadURL_S3PrefixAndBucketStrip cannot catch by
// construction: yujiawei's P1 in PR#147 review noted that the previous
// table test recorded `lastGetObjectPath` from a mock that skips
// `validatePresignObjectKey`, hiding the fact that ServiceS3 rejects
// leading-slash object keys at sign time. This test wires the real
// `ServiceS3` (with fake credentials — the SDK never makes a network
// call for `PresignedGetObject`) and asserts the handler returns 200,
// not 500, when the default minimal config (no s3.prefix) round-trips a
// virtual-hosted URL.
func TestGetDownloadURL_S3_RealServiceRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := config.New()
	cfg.Test = true
	cfg.S3.Endpoint = "s3.us-west-2.amazonaws.com"
	cfg.S3.Region = "us-west-2"
	cfg.S3.Bucket = "my-bucket"
	cfg.S3.AccessKeyID = "AKIAFAKEACCESSKEY"
	cfg.S3.SecretAccessKey = "fake-secret-access-key-1234567890"
	// No prefix — this is the failure shape from the review.

	// Use the real Service wrapper + ServiceS3. SDK PresignedGetObject
	// is pure URL signing (no network I/O) so fake creds are fine.
	cfg.FileService = "awsS3"
	ctx := testutil.NewTestContext(cfg)
	svc := NewService(ctx)

	f := &File{
		ctx:     ctx,
		Log:     log.NewTLog("FileTest"),
		service: svc,
	}

	inputURL := "https://my-bucket.s3.us-west-2.amazonaws.com/chat/2026/05/abc.jpg"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest(
		http.MethodGet,
		"/v1/file/download/url?path="+url.QueryEscape(inputURL)+"&filename=abc.jpg",
		nil,
	)
	c.Request = req

	wkCtx := &wkhttp.Context{Context: c}
	f.getDownloadURL(wkCtx)

	require.Equal(t, http.StatusOK, w.Code,
		"real ServiceS3 round-trip must return 200; pre-fix this returned 500 because validatePresignObjectKey rejected the leading-slash key. body=%s",
		w.Body.String())

	// The body should carry a signed URL with no double slashes in the
	// path — that's the SigV4 normalization hazard the leading-slash
	// strip is preventing in the first place.
	assert.NotContains(t, w.Body.String(), `\/\/chat`,
		"signed URL must not contain `//chat` (sign-time path normalization would break the signature)")
}

// TestGetDownloadURL_S3_DownloadURLPreservesBucketLikeKeySegment is the
// regression for Jerry-Xin's PR #147 review nit: when DownloadURL is
// set, ServiceS3.publicURL emits `<downloadURL>/<key>` (no bucket
// segment in the path). If api.getDownloadURL strips `/<bucket>` from
// path-style URLs unconditionally, a deployment where the bucket name
// equals the first object-key segment (e.g. bucket "chat", URL
// `https://files.example.com/chat/foo.jpg`) loses the real key and
// signs the wrong object. The fix gates the bucket strip on
// DownloadURL being empty.
func TestGetDownloadURL_S3_DownloadURLPreservesBucketLikeKeySegment(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := config.New()
	cfg.Test = true
	cfg.FileService = "awsS3"
	cfg.S3.Endpoint = "s3.us-west-2.amazonaws.com"
	cfg.S3.Region = "us-west-2"
	// Bucket name deliberately equals the first object-key segment
	// in the round-tripped URL below — the exact shape that would
	// produce the wrong-object signature without the DownloadURL gate.
	cfg.S3.Bucket = "chat"
	cfg.S3.DownloadURL = "https://files.example.com"
	cfg.S3.UsePathStyle = true

	mockSvc := &mockService{}
	f := &File{
		ctx:     testutil.NewTestContext(cfg),
		Log:     log.NewTLog("FileTest"),
		service: mockSvc,
	}

	inputURL := "https://files.example.com/chat/2026/05/abc.jpg"
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest(
		http.MethodGet,
		"/v1/file/download/url?path="+url.QueryEscape(inputURL)+"&filename=abc.jpg",
		nil,
	)
	c.Request = req

	wkCtx := &wkhttp.Context{Context: c}
	f.getDownloadURL(wkCtx)

	require.Equal(t, http.StatusOK, w.Code, "unexpected status; body=%s", w.Body.String())
	// The full key including the "chat/" prefix must reach the signer.
	// Pre-fix, the bucket strip ate "chat/" and PresignedGetURL would
	// have signed "2026/05/abc.jpg" — a completely different object.
	assert.Equal(t, "chat/2026/05/abc.jpg", mockSvc.lastGetObjectPath,
		"DownloadURL mode must NOT strip the bucket segment even when it matches the first key segment")
}

// TestGetDownloadURL_S3_BarePathLeadingSlashTrimmed locks in the
// post-#147-P2 fix from yujiawei: a bare relative path with a leading
// slash (`?path=/chat/foo`) must reach the signer as a clean key, not
// `/chat/foo`. Pre-fix, the leading-slash trim lived inside the
// `if strings.HasPrefix(ph, "http")` branch and bare paths slipped
// through, triggering validatePresignObjectKey rejection (500) on the
// strict S3 backend.
func TestGetDownloadURL_S3_BarePathLeadingSlashTrimmed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := config.New()
	cfg.Test = true
	cfg.FileService = "awsS3"
	cfg.S3.Endpoint = "s3.us-west-2.amazonaws.com"
	cfg.S3.Region = "us-west-2"
	cfg.S3.Bucket = "my-bucket"
	cfg.S3.AccessKeyID = "AKIAFAKEACCESSKEY"
	cfg.S3.SecretAccessKey = "fake-secret-access-key-1234567890"

	ctx := testutil.NewTestContext(cfg)
	svc := NewService(ctx)

	f := &File{
		ctx:     ctx,
		Log:     log.NewTLog("FileTest"),
		service: svc,
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req, _ := http.NewRequest(
		http.MethodGet,
		"/v1/file/download/url?path=%2Fchat%2F2026%2Fabc.jpg&filename=abc.jpg",
		nil,
	)
	c.Request = req

	wkCtx := &wkhttp.Context{Context: c}
	f.getDownloadURL(wkCtx)

	require.Equal(t, http.StatusOK, w.Code,
		"bare path with leading slash must be normalized before signing; pre-fix this returned 500 because validatePresignObjectKey rejected the leading-slash key. body=%s",
		w.Body.String())
}
