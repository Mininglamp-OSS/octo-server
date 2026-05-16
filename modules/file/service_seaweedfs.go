package file

import (
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

type SeaweedFS struct {
	log.Log
	ctx *config.Context
}

func NewSeaweedFS(ctx *config.Context) *SeaweedFS {
	return &SeaweedFS{
		Log: log.NewTLog("SeaweedFS"),
		ctx: ctx,
	}
}

// UploadFile 上传文件
func (s *SeaweedFS) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	if contentDisposition != "" {
		s.Warn("SeaweedFS 不支持在上传时设置 Content-Disposition 元数据，该值将被忽略",
			zap.String("contentDisposition", contentDisposition))
	}
	fileDir, fileName := filepath.Split(filePath)
	s.Debug("filePath->", zap.String("filePath", filePath), zap.String("fileDir", fileDir), zap.String("fileName", fileName))
	newFileDir := fileDir
	if !filepath.IsAbs(fileDir) {
		newFileDir = fmt.Sprintf("/%s", newFileDir)
	}
	seaweedConfig := s.ctx.GetConfig().Seaweed
	resultMap, err := uploadFile(fmt.Sprintf("%s%s", seaweedConfig.URL, newFileDir), fileName, copyFileWriter)
	return resultMap, err
}

func (s *SeaweedFS) GetFile(path string) (io.ReadCloser, string, error) {
	return nil, "", fmt.Errorf("GetFile not supported for SeaweedFS, use DownloadURL instead")
}

func (s *SeaweedFS) DownloadURL(path string, filename string) (string, error) {
	seaweedConfig := s.ctx.GetConfig().Seaweed
	rpath, _ := url.JoinPath(seaweedConfig.URL, path)
	return rpath, nil
}

// PresignedPutURL is intentionally not implemented for SeaweedFS.
//
// The standard SeaweedFS Filer/Volume HTTP API does not expose a presigned
// upload primitive: uploads go through the multipart-form helper used by
// UploadFile above, and the volume server authenticates by IP / network
// segmentation rather than by signed URL. Returning a clear error keeps
// the IService surface uniform; deployments needing browser-direct upload
// should sit a presign-capable proxy (or the SeaweedFS S3 gateway, when
// configured) in front of SeaweedFS, or fall back to server-side upload
// via UploadFile.
func (s *SeaweedFS) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (string, string, error) {
	return "", "", fmt.Errorf("SeaweedFS 后端暂不支持预签名上传：SeaweedFS does not expose a presigned PUT primitive; falling back to server-side upload via UploadFile")
}

// PresignedGetURL is intentionally not implemented for SeaweedFS.
//
// Public SeaweedFS reads are unsigned — the volume URL itself is the
// download URL. Callers that want a signed download against SeaweedFS
// should use the regular DownloadURL path; the IService surface keeps the
// hook here so the type assertion in service.go succeeds and we fail
// loudly only when a signed URL is genuinely required.
func (s *SeaweedFS) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	return "", fmt.Errorf("SeaweedFS 后端暂不支持预签名下载：use DownloadURL for unsigned public access")
}
