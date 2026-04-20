package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/qiniu/go-sdk/v7/auth"
	"github.com/qiniu/go-sdk/v7/storage"
	"go.uber.org/zap"
)

type ServiceQiniu struct {
	log.Log
	ctx *config.Context
}

// NewServiceQiniu NewServiceQiniu
func NewServiceQiniu(ctx *config.Context) *ServiceQiniu {

	return &ServiceQiniu{
		Log: log.NewTLog("ServiceQiniu"),
		ctx: ctx,
	}
}

// UploadFile 上传文件
func (s *ServiceQiniu) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {

	qiniuCfg := s.ctx.GetConfig().Qiniu

	bucket := qiniuCfg.BucketName
	putPolicy := storage.PutPolicy{
		Scope: fmt.Sprintf("%s:%s", bucket, filePath),
	}
	mac := auth.New(qiniuCfg.AccessKey, qiniuCfg.SecretKey)
	upToken := putPolicy.UploadToken(mac)

	cfg := storage.Config{}
	// 空间对应的机房
	//cfg.Region = &storage.ZoneHuabei
	// 是否使用https域名
	cfg.UseHTTPS = false
	// 上传是否使用CDN上传加速
	cfg.UseCdnDomains = false

	formUploader := storage.NewFormUploader(&cfg)
	ret := storage.PutRet{}
	putExtra := storage.PutExtra{
		Params: map[string]string{},
	}

	if contentDisposition != "" {
		s.Warn("七牛云存储不支持在上传时设置 Content-Disposition 元数据，该值将被忽略",
			zap.String("contentDisposition", contentDisposition))
	}

	data := bytes.NewBuffer(make([]byte, 0))
	err := copyFileWriter(data)
	if err != nil {
		s.Error("复制文件内容失败！", zap.Error(err))
		return nil, err
	}
	dataLen := int64(len(data.Bytes()))

	err = formUploader.Put(context.Background(), &ret, upToken, filePath, bytes.NewReader(data.Bytes()), dataLen, &putExtra)
	if err != nil {
		s.Error("上传失败", zap.Error(err))
	}
	return map[string]interface{}{
		"path": ret.Key,
	}, err
}

func (s *ServiceQiniu) GetFile(path string) (io.ReadCloser, string, error) {
	return nil, "", fmt.Errorf("GetFile not supported for Qiniu, use DownloadURL instead")
}

func (s *ServiceQiniu) DownloadURL(path string, filename string) (string, error) {
	qiniuCfg := s.ctx.GetConfig().Qiniu
	domain := qiniuCfg.URL
	key := strings.TrimPrefix(path, "/")
	publicAccessURL := storage.MakePublicURL(domain, key)
	return publicAccessURL, nil
}
