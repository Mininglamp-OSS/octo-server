package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"
)

// ServiceCOS 腾讯云COS文件上传（通过S3兼容协议）
type ServiceCOS struct {
	log.Log
	ctx            *config.Context
	downloadClient *http.Client
}

// NewServiceCOS NewServiceCOS
func NewServiceCOS(ctx *config.Context) *ServiceCOS {
	return &ServiceCOS{
		Log: log.NewTLog("FileCOS"),
		ctx: ctx,
		downloadClient: &http.Client{
			Timeout: time.Second * 30,
		},
	}
}

// withPrefix 拼接环境前缀到对象路径（多环境共用 bucket 时隔离路径）
func (sc *ServiceCOS) withPrefix(objectPath string) string {
	prefix := strings.TrimSpace(sc.ctx.GetConfig().COS.Prefix)
	if prefix == "" {
		return objectPath
	}
	return path.Join(prefix, objectPath)
}

func (sc *ServiceCOS) getClient() (*minio.Client, error) {
	cosConfig := sc.ctx.GetConfig().COS
	endpoint := fmt.Sprintf("cos.%s.myqcloud.com", cosConfig.Region)

	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cosConfig.SecretID, cosConfig.SecretKey, ""),
		Secure:       true,
		BucketLookup: minio.BucketLookupDNS, // COS 要求 virtual-hosted-style: <bucket>.cos.<region>.myqcloud.com
	})
	if err != nil {
		return nil, fmt.Errorf("创建COS客户端失败: %w", err)
	}
	return client, nil
}

// UploadFile 上传文件到腾讯云COS
func (sc *ServiceCOS) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	buff := bytes.NewBuffer(make([]byte, 0))
	err := copyFileWriter(buff)
	if err != nil {
		sc.Error("复制文件内容失败！", zap.Error(err))
		return nil, err
	}

	cosConfig := sc.ctx.GetConfig().COS
	client, err := sc.getClient()
	if err != nil {
		return nil, err
	}

	bucketName := cosConfig.Bucket
	// COS 单 bucket 模式：保留完整路径（含 chat/ 等原始 bucket 名），用 prefix 区分环境
	fileName := sc.withPrefix(filePath)

	opts := minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    10 * 1024 * 1024,
	}
	if contentDisposition != "" {
		opts.ContentDisposition = contentDisposition
	}

	ctx := context.Background()
	n, err := client.PutObject(ctx, bucketName, fileName, buff, int64(buff.Len()), opts)
	if err != nil {
		sc.Error("上传文件到COS失败", zap.Error(err))
		return map[string]interface{}{
			"path": "",
		}, err
	}

	return map[string]interface{}{
		"path": n.Key,
	}, nil
}

// DownloadURL 获取COS文件下载地址
func (sc *ServiceCOS) GetFile(ph string) (io.ReadCloser, string, error) {
	client, err := sc.getClient()
	if err != nil {
		return nil, "", err
	}

	cosConfig := sc.ctx.GetConfig().COS
	bucketName := cosConfig.Bucket
	// COS 单 bucket 模式：保留完整路径，用 prefix 区分环境
	objectPath := sc.withPrefix(ph)

	obj, err := client.GetObject(context.Background(), bucketName, objectPath, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", err
	}
	stat, err := obj.Stat()
	if err != nil {
		obj.Close()
		return nil, "", err
	}
	return obj, stat.ContentType, nil
}

// PresignedPutURL 生成预签名 PUT URL，用于客户端直传 COS。
// 如果配置了 BucketURL（自定义域名），会将预签名 URL 的 Host 替换为自定义域名，
// 避免客户端网络屏蔽云厂商域名。
func (sc *ServiceCOS) PresignedPutURL(objectPath string, contentType string, contentDisposition string, expires time.Duration) (uploadURL string, downloadURL string, err error) {
	cosConfig := sc.ctx.GetConfig().COS
	client, err := sc.getClient()
	if err != nil {
		return "", "", err
	}

	key := sc.withPrefix(objectPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var presigned *url.URL
	if contentDisposition != "" {
		headers := http.Header{}
		headers.Set("Content-Disposition", contentDisposition)
		presigned, err = client.PresignHeader(ctx, http.MethodPut, cosConfig.Bucket, key, expires, nil, headers)
	} else {
		presigned, err = client.PresignedPutObject(ctx, cosConfig.Bucket, key, expires)
	}
	if err != nil {
		return "", "", fmt.Errorf("生成预签名URL失败: %w", err)
	}

	if customBase := strings.TrimSpace(cosConfig.BucketURL); customBase != "" {
		parsed, parseErr := url.Parse(strings.TrimRight(customBase, "/"))
		if parseErr == nil {
			presigned.Host = parsed.Host
			presigned.Scheme = parsed.Scheme
		}
	}

	uploadURL = presigned.String()

	downloadURL, dlErr := sc.DownloadURL(objectPath, "")
	if dlErr != nil {
		sc.Warn("生成下载URL失败", zap.Error(dlErr))
	}
	return uploadURL, downloadURL, nil
}

func (sc *ServiceCOS) DownloadURL(ph string, filename string) (string, error) {
	cosConfig := sc.ctx.GetConfig().COS

	downloadBase := cosConfig.BucketURL
	if strings.TrimSpace(downloadBase) == "" {
		downloadBase = fmt.Sprintf("https://%s.cos.%s.myqcloud.com", cosConfig.Bucket, cosConfig.Region)
	}

	ph = sc.withPrefix(ph)
	result, _ := url.JoinPath(downloadBase, ph)
	if strings.TrimSpace(filename) == "" {
		return result, nil
	}
	vals := url.Values{}
	encodedFilename := "UTF-8''" + url.QueryEscape(filename)
	vals.Set("response-content-disposition", fmt.Sprintf("attachment; filename*=%s", encodedFilename))
	return fmt.Sprintf("%s?%s", result, vals.Encode()), nil
}
