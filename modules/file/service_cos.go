package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
func (sc *ServiceCOS) UploadFile(filePath string, contentType string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
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
	fileName := filePath
	// 如果路径包含 bucket 前缀，分离出来
	strs := strings.Split(filePath, "/")
	if len(strs) > 1 {
		fileName = strings.TrimPrefix(filePath, fmt.Sprintf("%s/", strs[0]))
	}

	ctx := context.Background()
	n, err := client.PutObject(ctx, bucketName, fileName, buff, int64(buff.Len()), minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    10 * 1024 * 1024,
	})
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
	objectPath := ph
	strs := strings.Split(ph, "/")
	if len(strs) > 1 {
		allowedBuckets := map[string]bool{
			"file": true, "chat": true, "moment": true, "sticker": true,
			"report": true, "chatbg": true, "common": true, "download": true,
			"group": true, "avatar": true,
		}
		if allowedBuckets[strs[0]] {
			objectPath = strings.TrimPrefix(ph, strs[0]+"/")
		}
	}

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

func (sc *ServiceCOS) DownloadURL(ph string, filename string) (string, error) {
	cosConfig := sc.ctx.GetConfig().COS

	downloadBase := cosConfig.BucketURL
	if strings.TrimSpace(downloadBase) == "" {
		downloadBase = fmt.Sprintf("https://%s.cos.%s.myqcloud.com", cosConfig.Bucket, cosConfig.Region)
	}

	vals := url.Values{}
	encodedFilename := "UTF-8''" + url.QueryEscape(filename)
	vals.Set("response-content-disposition", fmt.Sprintf("attachment; filename*=%s", encodedFilename))
	result, _ := url.JoinPath(downloadBase, ph)
	return fmt.Sprintf("%s?%s", result, vals.Encode()), nil
}
