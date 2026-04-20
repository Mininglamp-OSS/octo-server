package file

import (
	"fmt"
	"io"
	"net/url"
	"path/filepath"

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
