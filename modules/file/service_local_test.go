package file

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/stretchr/testify/require"
)

func newLocalFileTestContext(t *testing.T) *config.Context {
	t.Helper()
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	cfg := config.New()
	cfg.RootDir = t.TempDir()
	cfg.FileService = config.FileService(fileServiceLocal)
	cfg.External.APIBaseURL = "http://octo.local/v1"
	return config.NewContext(cfg)
}

func TestLocalFileSigningKeyUsesOctoMasterKey(t *testing.T) {
	req := localFileSignedRequest{
		Method:    "GET",
		Path:      "common/documents/readme.txt",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	}

	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	sig, err := signLocalFileRequest(req)
	require.NoError(t, err)
	require.NoError(t, verifyLocalFileRequest(req, sig, time.Now()))

	t.Setenv("OCTO_MASTER_KEY", "abcdef0123456789abcdef0123456789")
	require.Error(t, verifyLocalFileRequest(req, sig, time.Now()))

	t.Setenv("OCTO_MASTER_KEY", "short")
	_, err = signLocalFileRequest(req)
	require.Error(t, err)
}

func TestLocalFileService_UploadReadAndSignDownload(t *testing.T) {
	ctx := newLocalFileTestContext(t)
	svc := NewLocalFileService(ctx)

	_, err := svc.UploadFile("common/documents/readme.txt", "text/plain; charset=utf-8", "", func(w io.Writer) error {
		_, copyErr := io.Copy(w, strings.NewReader("hello local file"))
		return copyErr
	})
	require.NoError(t, err)

	rc, contentType, err := svc.GetFile("common/documents/readme.txt")
	require.NoError(t, err)
	defer rc.Close()

	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "hello local file", string(body))
	require.Equal(t, "text/plain; charset=utf-8", contentType)

	signed, err := svc.PresignedGetURL("common/documents/readme.txt", "需求文档.txt", "inline", 30*time.Minute)
	require.NoError(t, err)

	parsed, err := url.Parse(signed)
	require.NoError(t, err)
	require.Equal(t, "http", parsed.Scheme)
	require.Equal(t, "octo.local", parsed.Host)
	require.Equal(t, "/v1/file/local", parsed.Path)
	require.Equal(t, "common/documents/readme.txt", parsed.Query().Get("path"))
	require.Equal(t, "inline", parsed.Query().Get("disposition"))
	require.NotEmpty(t, parsed.Query().Get("sig"))
}

func TestFile_LocalSignedRouteServesContentAndRejectsTampering(t *testing.T) {
	ctx := newLocalFileTestContext(t)
	f := New(ctx)
	svc, ok := f.service.(*Service).uploadService.(*LocalFileService)
	require.True(t, ok)
	_, err := svc.UploadFile("common/documents/readme.txt", "text/plain; charset=utf-8", "", func(w io.Writer) error {
		_, copyErr := io.Copy(w, strings.NewReader("hello route"))
		return copyErr
	})
	require.NoError(t, err)

	r := wkhttp.New()
	f.Route(r)

	signed, err := svc.PresignedGetURL("common/documents/readme.txt", "readme.txt", "attachment", 30*time.Minute)
	require.NoError(t, err)
	parsed, err := url.Parse(signed)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, parsed.RequestURI(), nil)
	req.Header.Set("Origin", "http://localhost:3001")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "hello route", rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))

	req = httptest.NewRequest(http.MethodHead, parsed.RequestURI(), nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")

	req = httptest.NewRequest(http.MethodOptions, parsed.RequestURI(), nil)
	req.Header.Set("Origin", "http://localhost:3001")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))

	query := parsed.Query()
	query.Set("path", "common/documents/other.txt")
	parsed.RawQuery = query.Encode()
	req = httptest.NewRequest(http.MethodGet, parsed.RequestURI(), nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestFile_LocalSignedRouteAcceptsPresignedPutRoundtrip(t *testing.T) {
	ctx := newLocalFileTestContext(t)
	f := New(ctx)
	svc, ok := f.service.(*Service).uploadService.(*LocalFileService)
	require.True(t, ok)

	payload := "hello signed put"
	contentType := "text/markdown; charset=utf-8"
	contentDisposition := `inline; filename="notes.md"; filename*=UTF-8''notes.md`
	signed, _, err := svc.PresignedPutURL(
		"chat/2/test-local-put.md",
		contentType,
		contentDisposition,
		int64(len(payload)),
		30*time.Minute,
	)
	require.NoError(t, err)
	parsed, err := url.Parse(signed)
	require.NoError(t, err)
	require.NotEmpty(t, parsed.Query().Get("nonce"))
	require.Empty(t, parsed.Query().Get("contentDisposition"))
	require.NotEmpty(t, parsed.Query().Get("contentDispositionB64"))

	r := wkhttp.New()
	f.Route(r)

	req := httptest.NewRequest(http.MethodPut, parsed.RequestURI(), strings.NewReader(payload))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Disposition", contentDisposition)
	req.Header.Set("Origin", "http://localhost:3001")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))

	rc, storedContentType, err := svc.GetFile("chat/2/test-local-put.md")
	require.NoError(t, err)
	defer rc.Close()
	body, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, payload, string(body))
	require.Equal(t, contentType, storedContentType)

	_, diskPath, err := svc.diskPath("chat/2/test-local-put.md")
	require.NoError(t, err)
	require.NoError(t, os.Remove(diskPath))

	req = httptest.NewRequest(http.MethodPut, parsed.RequestURI(), strings.NewReader(payload))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Disposition", contentDisposition)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestFile_LocalSignedPutRejectsBoundaryViolations(t *testing.T) {
	ctx := newLocalFileTestContext(t)
	f := New(ctx)
	svc, ok := f.service.(*Service).uploadService.(*LocalFileService)
	require.True(t, ok)

	r := wkhttp.New()
	f.Route(r)

	t.Run("content_length_mismatch", func(t *testing.T) {
		payload := "short"
		signed, _, err := svc.PresignedPutURL(
			"chat/2/mismatch.txt",
			"text/plain; charset=utf-8",
			"",
			int64(len(payload)+1),
			30*time.Minute,
		)
		require.NoError(t, err)
		rec := performLocalSignedPut(t, r, signed, payload, "text/plain; charset=utf-8", "")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("file_size_over_limit", func(t *testing.T) {
		signed, err := svc.signedURL(localFileSignedRequest{
			Method:      http.MethodPut,
			Path:        "chat/2/oversize.txt",
			ContentType: "text/plain; charset=utf-8",
			FileSize:    MaxFileSize + 1,
			ExpiresAt:   time.Now().Add(30 * time.Minute).Unix(),
			Nonce:       "oversize-nonce",
		})
		require.NoError(t, err)
		rec := performLocalSignedPut(t, r, signed, "tiny", "text/plain; charset=utf-8", "")
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	})

	t.Run("blocked_extension", func(t *testing.T) {
		payload := "binary"
		signed, _, err := svc.PresignedPutURL(
			"chat/2/malware.exe",
			"application/octet-stream",
			"",
			int64(len(payload)),
			30*time.Minute,
		)
		require.NoError(t, err)
		rec := performLocalSignedPut(t, r, signed, payload, "application/octet-stream", "")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("path_exists", func(t *testing.T) {
		path := "chat/2/existing.txt"
		_, err := svc.UploadFile(path, "text/plain; charset=utf-8", "", func(w io.Writer) error {
			_, copyErr := io.Copy(w, strings.NewReader("already here"))
			return copyErr
		})
		require.NoError(t, err)

		payload := "replacement"
		signed, _, err := svc.PresignedPutURL(
			path,
			"text/plain; charset=utf-8",
			"",
			int64(len(payload)),
			30*time.Minute,
		)
		require.NoError(t, err)
		rec := performLocalSignedPut(t, r, signed, payload, "text/plain; charset=utf-8", "")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
}

func TestFile_LocalSignedGetDowngradesUnsafeInlineContent(t *testing.T) {
	ctx := newLocalFileTestContext(t)
	f := New(ctx)
	svc, ok := f.service.(*Service).uploadService.(*LocalFileService)
	require.True(t, ok)
	_, err := svc.UploadFile("common/page.html", "text/html; charset=utf-8", "", func(w io.Writer) error {
		_, copyErr := io.Copy(w, strings.NewReader("<html>unsafe</html>"))
		return copyErr
	})
	require.NoError(t, err)

	signed, err := svc.PresignedGetURL("common/page.html", "page.html", "inline", 30*time.Minute)
	require.NoError(t, err)
	parsed, err := url.Parse(signed)
	require.NoError(t, err)

	r := wkhttp.New()
	f.Route(r)
	req := httptest.NewRequest(http.MethodGet, parsed.RequestURI(), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
}

func performLocalSignedPut(t *testing.T, r *wkhttp.WKHttp, signedURL string, payload string, contentType string, contentDisposition string) *httptest.ResponseRecorder {
	t.Helper()
	parsed, err := url.Parse(signedURL)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPut, parsed.RequestURI(), strings.NewReader(payload))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if contentDisposition != "" {
		req.Header.Set("Content-Disposition", contentDisposition)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}
