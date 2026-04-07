package backup

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"
)

// COSStorage 腾讯云 COS 存储（通过 S3 兼容协议）
type COSStorage struct {
	log.Log
	client *minio.Client
	config *StorageConfig
}

// NewCOSStorage 创建 COS 存储实例
func NewCOSStorage(cfg *StorageConfig) (*COSStorage, error) {
	if cfg.Region == "" {
		return nil, fmt.Errorf("COS region is required")
	}

	// 根据 region 构建 endpoint
	endpoint := fmt.Sprintf("cos.%s.myqcloud.com", cfg.Region)

	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure:       true,
		BucketLookup: minio.BucketLookupDNS, // COS 要求 virtual-hosted-style
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create COS client: %w", err)
	}

	return &COSStorage{
		Log:    log.NewTLog("COSStorage"),
		client: client,
		config: cfg,
	}, nil
}

// Upload 上传文件到 COS
func (s *COSStorage) Upload(ctx context.Context, localPath, remotePath string) error {
	fullRemotePath := path.Join(s.config.Prefix, remotePath)
	s.Info("uploading file to COS", zap.String("local", localPath), zap.String("remote", fullRemotePath))

	_, err := s.client.FPutObject(ctx, s.config.Bucket, fullRemotePath, localPath, minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to upload file: %w", err)
	}

	s.Info("file uploaded successfully", zap.String("path", fullRemotePath))
	return nil
}

// Delete 删除 COS 上的文件
func (s *COSStorage) Delete(ctx context.Context, remotePath string) error {
	fullRemotePath := path.Join(s.config.Prefix, remotePath)
	err := s.client.RemoveObject(ctx, s.config.Bucket, fullRemotePath, minio.RemoveObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete object: %w", err)
	}
	s.Info("file deleted", zap.String("path", fullRemotePath))
	return nil
}

// GetPresignedURL 获取预签名下载 URL
func (s *COSStorage) GetPresignedURL(ctx context.Context, remotePath string, expires time.Duration) (string, error) {
	fullRemotePath := path.Join(s.config.Prefix, remotePath)
	reqParams := make(url.Values)
	presignedURL, err := s.client.PresignedGetObject(ctx, s.config.Bucket, fullRemotePath, expires, reqParams)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}
	return presignedURL.String(), nil
}

// TestConnection 测试 COS 连接
func (s *COSStorage) TestConnection(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.config.Bucket)
	if err != nil {
		return fmt.Errorf("failed to check bucket existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("bucket %s does not exist", s.config.Bucket)
	}
	return nil
}

// List 列出前缀下的文件
func (s *COSStorage) List(ctx context.Context, prefix string) ([]string, error) {
	fullPrefix := path.Join(s.config.Prefix, prefix)
	var objects []string

	objectCh := s.client.ListObjects(ctx, s.config.Bucket, minio.ListObjectsOptions{
		Prefix:    fullPrefix,
		Recursive: false,
	})

	for object := range objectCh {
		if object.Err != nil {
			return nil, fmt.Errorf("failed to list objects: %w", object.Err)
		}
		objects = append(objects, object.Key)
	}

	return objects, nil
}
