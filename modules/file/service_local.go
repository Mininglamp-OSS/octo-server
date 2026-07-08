package file

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

type LocalFileService struct {
	log.Log
	ctx *config.Context
}

type localFileMetadata struct {
	ContentType        string    `json:"contentType"`
	ContentDisposition string    `json:"contentDisposition,omitempty"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

func NewLocalFileService(ctx *config.Context) *LocalFileService {
	return &LocalFileService{
		Log: log.NewTLog("LocalFileService"),
		ctx: ctx,
	}
}

func (s *LocalFileService) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	objectPath, diskPath, err := s.diskPath(filePath)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(diskPath), 0o755); err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(diskPath), ".upload-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := copyFileWriter(tmp); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpName, diskPath); err != nil {
		return nil, err
	}
	if err := s.writeMetadata(objectPath, localFileMetadata{
		ContentType:        ensureTextCharset(contentType),
		ContentDisposition: contentDisposition,
		UpdatedAt:          time.Now(),
	}); err != nil {
		s.Warn("写入本地文件元数据失败", zap.String("path", objectPath), zap.Error(err))
	}
	return map[string]interface{}{
		"path": objectPath,
	}, nil
}

func (s *LocalFileService) DownloadURL(path string, filename string) (string, error) {
	disposition := "inline"
	if strings.TrimSpace(filename) != "" {
		disposition = "attachment"
	}
	return s.PresignedGetURL(path, filename, disposition, 24*time.Hour)
}

func (s *LocalFileService) GetFile(path string) (io.ReadCloser, string, error) {
	objectPath, diskPath, err := s.diskPath(path)
	if err != nil {
		return nil, "", err
	}
	file, err := os.Open(diskPath)
	if err != nil {
		return nil, "", err
	}
	contentType := s.contentType(objectPath)
	return file, contentType, nil
}

func (s *LocalFileService) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (string, string, error) {
	if fileSize <= 0 {
		return "", "", fmt.Errorf("预签名上传必须提供正向的 fileSize（字节数）")
	}
	if fileSize > MaxFileSize {
		return "", "", fmt.Errorf("文件大小不能超过%dMB", MaxFileSize/1024/1024)
	}
	normalized, _, err := s.diskPath(objectPath)
	if err != nil {
		return "", "", err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	contentType = ensureTextCharset(contentType)
	expiresAt := localFileExpiresAt(expires)
	nonce, err := newLocalFileNonce()
	if err != nil {
		return "", "", err
	}
	uploadURL, err := s.signedURL(localFileSignedRequest{
		Method:             "PUT",
		Path:               normalized,
		ContentType:        contentType,
		ContentDisposition: contentDisposition,
		FileSize:           fileSize,
		ExpiresAt:          expiresAt,
		Nonce:              nonce,
	})
	if err != nil {
		return "", "", err
	}
	downloadURL, err := s.PresignedGetURL(normalized, filepath.Base(normalized), "inline", expires)
	if err != nil {
		return "", "", err
	}
	return uploadURL, downloadURL, nil
}

func (s *LocalFileService) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	normalized, _, err := s.diskPath(objectPath)
	if err != nil {
		return "", err
	}
	if disposition != "inline" {
		disposition = "attachment"
	}
	return s.signedURL(localFileSignedRequest{
		Method:      "GET",
		Path:        normalized,
		Filename:    sanitizeFilename(filename),
		Disposition: disposition,
		ExpiresAt:   localFileExpiresAt(expires),
	})
}

func (s *LocalFileService) signedURL(req localFileSignedRequest) (string, error) {
	base := strings.TrimRight(s.ctx.GetConfig().External.APIBaseURL, "/")
	if base == "" {
		base = "/v1"
	}
	u, err := url.Parse(base + "/file/local")
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("method", req.Method)
	values.Set("path", req.Path)
	values.Set("expires", strconv.FormatInt(req.ExpiresAt, 10))
	if req.Filename != "" {
		values.Set("filename", req.Filename)
	}
	if req.Disposition != "" {
		values.Set("disposition", req.Disposition)
	}
	if req.ContentType != "" {
		values.Set("contentType", req.ContentType)
	}
	if req.ContentDisposition != "" {
		values.Set("contentDispositionB64", base64.RawURLEncoding.EncodeToString([]byte(req.ContentDisposition)))
	}
	if req.FileSize > 0 {
		values.Set("fileSize", strconv.FormatInt(req.FileSize, 10))
	}
	if req.Nonce != "" {
		values.Set("nonce", req.Nonce)
	}
	signature, err := signLocalFileRequest(req)
	if err != nil {
		return "", err
	}
	values.Set("sig", signature)
	u.RawQuery = values.Encode()
	return u.String(), nil
}

func (s *LocalFileService) diskPath(objectPath string) (string, string, error) {
	sanitized, err := sanitizePath(objectPath)
	if err != nil {
		return "", "", err
	}
	normalized := strings.TrimPrefix(filepath.ToSlash(sanitized), "/")
	if normalized == "" || normalized == "." {
		return "", "", fmt.Errorf("文件路径不能为空")
	}
	if strings.HasPrefix(normalized, "../") || strings.Contains(normalized, "/../") {
		return "", "", fmt.Errorf("文件路径不允许包含目录遍历字符")
	}
	root := s.rootDir()
	diskPath := filepath.Join(root, filepath.FromSlash(normalized))
	cleanRoot := filepath.Clean(root)
	cleanDiskPath := filepath.Clean(diskPath)
	if cleanDiskPath != cleanRoot && !strings.HasPrefix(cleanDiskPath, cleanRoot+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("文件路径越界")
	}
	return normalized, cleanDiskPath, nil
}

func (s *LocalFileService) rootDir() string {
	root := strings.TrimSpace(s.ctx.GetConfig().RootDir)
	if root == "" {
		root = "tsdddata"
	}
	return filepath.Join(root, "files")
}

func (s *LocalFileService) metadataPath(objectPath string) string {
	return filepath.Join(s.rootDir(), ".metadata", filepath.FromSlash(objectPath)+".json")
}

func (s *LocalFileService) writeMetadata(objectPath string, meta localFileMetadata) error {
	path := s.metadataPath(objectPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

func (s *LocalFileService) uploadNoncePath(req localFileSignedRequest) string {
	sum := sha256.Sum256([]byte(req.Path + "\n" + req.Nonce))
	return filepath.Join(s.rootDir(), ".upload-nonce", hex.EncodeToString(sum[:])+".used")
}

func (s *LocalFileService) reserveUploadNonce(req localFileSignedRequest) (func(success bool), error) {
	if req.Method != "PUT" || strings.TrimSpace(req.Nonce) == "" {
		return nil, fmt.Errorf("签名上传 nonce 缺失")
	}
	path := s.uploadNoncePath(req)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("签名上传链接已使用")
		}
		return nil, err
	}
	_, writeErr := fmt.Fprintf(file, "%s\n", time.Now().UTC().Format(time.RFC3339Nano))
	closeErr := file.Close()
	if writeErr != nil {
		_ = os.Remove(path)
		return nil, writeErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return nil, closeErr
	}
	return func(success bool) {
		if !success {
			_ = os.Remove(path)
		}
	}, nil
}

func (s *LocalFileService) contentType(objectPath string) string {
	body, err := os.ReadFile(s.metadataPath(objectPath))
	if err == nil {
		var meta localFileMetadata
		if json.Unmarshal(body, &meta) == nil && meta.ContentType != "" {
			return ensureTextCharset(meta.ContentType)
		}
	}
	ext := strings.ToLower(filepath.Ext(objectPath))
	if detected := mime.TypeByExtension(ext); detected != "" {
		return ensureTextCharset(detected)
	}
	if fallback, ok := textExtFallback[ext]; ok {
		return ensureTextCharset(fallback)
	}
	return "application/octet-stream"
}

type localFileSignedRequest struct {
	Method             string
	Path               string
	Filename           string
	Disposition        string
	ContentType        string
	ContentDisposition string
	FileSize           int64
	ExpiresAt          int64
	Nonce              string
}

func localFileExpiresAt(expires time.Duration) int64 {
	if expires <= 0 {
		expires = 30 * time.Minute
	}
	return time.Now().Add(expires).Unix()
}

func signLocalFileRequest(req localFileSignedRequest) (string, error) {
	key, err := localFileSigningKey()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(localFileCanonicalString(req)))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func verifyLocalFileRequest(req localFileSignedRequest, signature string, now time.Time) error {
	if req.ExpiresAt <= 0 || now.Unix() > req.ExpiresAt {
		return fmt.Errorf("文件链接已过期")
	}
	if req.Method == "PUT" && strings.TrimSpace(req.Nonce) == "" {
		return fmt.Errorf("签名上传 nonce 缺失")
	}
	if strings.TrimSpace(signature) == "" {
		return fmt.Errorf("文件链接签名不能为空")
	}
	expected, err := signLocalFileRequest(req)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return fmt.Errorf("文件链接签名无效")
	}
	return nil
}

func localFileCanonicalString(req localFileSignedRequest) string {
	return strings.Join([]string{
		req.Method,
		req.Path,
		req.Filename,
		req.Disposition,
		req.ContentType,
		req.ContentDisposition,
		strconv.FormatInt(req.FileSize, 10),
		strconv.FormatInt(req.ExpiresAt, 10),
		req.Nonce,
	}, "\n")
}

func newLocalFileNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func localFileSigningKey() ([]byte, error) {
	master := os.Getenv("OCTO_MASTER_KEY")
	if len(master) != 32 {
		return nil, fmt.Errorf("OCTO_MASTER_KEY must be exactly 32 bytes for local file signing")
	}
	mac := hmac.New(sha256.New, []byte(master))
	_, _ = mac.Write([]byte("octo/local-file/v1"))
	return mac.Sum(nil), nil
}
