package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

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
	// 空间对应的机房 — leave Region unset so the SDK queries the upload host
	// at runtime via its configured probe path. Setting an explicit MinIO-
	// style region here would not match Qiniu's zone names.
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

// PresignedPutURL is intentionally not implemented for Qiniu.
//
// Qiniu's direct-upload contract is fundamentally different from the S3
// presigned-PUT model that the rest of the IService surface assumes: a
// browser uploads to a fixed upload host (e.g. `up.qiniup.com`) using a
// multipart form whose `token` field carries an `UploadToken` derived from
// a `PutPolicy`. There is no single signed URL the browser can `PUT` to.
//
// Surfacing that asymmetry as a clear error here keeps the IService
// signatures uniform across backends; deployments that need browser-direct
// upload to Qiniu should instead use the standard server-side UploadFile
// path or migrate to the COS / OSS / MinIO backends. The
// `configs/octo-server.yaml` support matrix documents this fallback.
func (s *ServiceQiniu) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (string, string, error) {
	return "", "", fmt.Errorf("七牛云后端暂不支持预签名上传：Qiniu uses an UploadToken+form-post upload model that does not map to a single PUT URL; falling back to server-side upload via UploadFile")
}

// PresignedGetURL signs a private-bucket download URL for Qiniu, with the
// `attname` query parameter set so the browser saves under the user-facing
// filename.
func (s *ServiceQiniu) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	qiniuCfg := s.ctx.GetConfig().Qiniu
	mac := auth.New(qiniuCfg.AccessKey, qiniuCfg.SecretKey)

	domain := qiniuCfg.URL
	key := strings.TrimPrefix(objectPath, "/")
	if key == "" {
		return "", fmt.Errorf("空对象路径，无法生成预签名URL")
	}

	deadline := time.Now().Add(expires).Unix()

	if filename == "" {
		return storage.MakePrivateURLv2(mac, domain, key, deadline), nil
	}

	query := url.Values{}
	// Qiniu uses `attname` for the response file name; it expects the raw
	// (URL-decoded) filename and does its own quoting internally.
	query.Set("attname", filename)
	return storage.MakePrivateURLv2WithQuery(mac, domain, key, query, deadline), nil
}
