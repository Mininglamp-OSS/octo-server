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

// ServiceMinio 文件上传
type ServiceMinio struct {
	log.Log
	ctx            *config.Context
	downloadClient *http.Client
}

// NewServiceMinio NewServiceMinio
func NewServiceMinio(ctx *config.Context) *ServiceMinio {
	return &ServiceMinio{
		Log: log.NewTLog("File"),
		ctx: ctx,
		downloadClient: &http.Client{
			Timeout: time.Second * 30,
		},
	}
}

// UploadFile 上传文件
func (sm *ServiceMinio) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	buff := bytes.NewBuffer(make([]byte, 0))
	err := copyFileWriter(buff)
	if err != nil {
		sm.Error("复制文件内容失败！", zap.Error(err))
		return nil, err
	}

	minioConfig := sm.ctx.GetConfig().Minio

	ctx := context.Background()
	uploadUl, _ := url.Parse(minioConfig.UploadURL)
	endpoint := uploadUl.Host
	accessKeyID := minioConfig.AccessKeyID
	secretAccessKey := minioConfig.SecretAccessKey
	useSSL := false

	if strings.HasPrefix(uploadUl.Scheme, "https") {
		useSSL = true
	}
	// 初使化minio client对象。
	minioClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		sm.Error("创建错误：", zap.Error(err))
		return nil, err
	}
	// Bucket name whitelist to prevent arbitrary bucket creation
	allowedBuckets := map[string]bool{
		"file": true, "chat": true, "moment": true, "sticker": true,
		"report": true, "chatbg": true, "common": true, "download": true,
		"group": true, "avatar": true,
	}
	bucketName := "file"
	strs := strings.Split(filePath, "/")
	if len(strs) > 0 && allowedBuckets[strs[0]] {
		bucketName = strs[0]
	}
	exists, err := minioClient.BucketExists(ctx, bucketName)
	if err != nil {
		sm.Error(fmt.Sprintf("检测 %s目录是否存在错误", bucketName))
		return nil, err
	}
	if !exists {
		err = minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{})
		if err != nil {
			sm.Error(fmt.Sprintf("创建 %s目录失败", bucketName))
			return nil, err
		}
		// Read-only public policy: allow anonymous download only.
		// Upload and delete go through authenticated server-side credentials.
		policy := `{
			"Version": "2012-10-17",
			"Statement": [{
				"Effect": "Allow",
				"Principal": {
					"AWS": ["*"]
				},
				"Action": ["s3:GetObject"],
				"Resource": ["arn:aws:s3:::%s/*"]
			}]
		}`
		err = minioClient.SetBucketPolicy(context.Background(), bucketName, fmt.Sprintf(policy, bucketName))
		if err != nil {
			sm.Error("设置minio文件读写权限错误", zap.Error(err))
			return nil, err
		}
	}

	fileName := strings.TrimPrefix(filePath, fmt.Sprintf("%s/", bucketName))
	opts := minio.PutObjectOptions{ContentType: contentType, PartSize: 10 * 1024 * 1024}
	if contentDisposition != "" {
		opts.ContentDisposition = contentDisposition
	}
	n, err := minioClient.PutObject(ctx, bucketName, fileName, buff, int64(len(buff.Bytes())), opts)
	if err != nil {
		sm.Error("上传文件失败：", zap.Error(err))
		return map[string]interface{}{
			"path": "",
		}, err
	}
	return map[string]interface{}{
		"path": n.Key,
	}, err
}

func (sm *ServiceMinio) GetFile(ph string) (io.ReadCloser, string, error) {
	minioConfig := sm.ctx.GetConfig().Minio
	uploadUl, _ := url.Parse(minioConfig.UploadURL)
	endpoint := uploadUl.Host
	useSSL := strings.HasPrefix(uploadUl.Scheme, "https")

	minioClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioConfig.AccessKeyID, minioConfig.SecretAccessKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, "", err
	}

	bucketName := "file"
	objectPath := ph
	strs := strings.Split(ph, "/")
	if len(strs) > 1 {
		allowedBuckets := map[string]bool{
			"file": true, "chat": true, "moment": true, "sticker": true,
			"report": true, "chatbg": true, "common": true, "download": true,
			"group": true, "avatar": true,
		}
		if allowedBuckets[strs[0]] {
			bucketName = strs[0]
			objectPath = strings.TrimPrefix(ph, bucketName+"/")
		}
	}

	obj, err := minioClient.GetObject(context.Background(), bucketName, objectPath, minio.GetObjectOptions{})
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

func (sm *ServiceMinio) DownloadURL(ph string, filename string) (string, error) {
	minioConfig := sm.ctx.GetConfig().Minio
	result, _ := url.JoinPath(minioConfig.DownloadURL, ph)
	if strings.TrimSpace(filename) == "" {
		return result, nil
	}
	vals := url.Values{}
	encodedFilename := "UTF-8''" + url.QueryEscape(filename)
	vals.Set("response-content-disposition", fmt.Sprintf("attachment; filename*=%s", encodedFilename))
	return fmt.Sprintf("%s?%s", result, vals.Encode()), nil
}
