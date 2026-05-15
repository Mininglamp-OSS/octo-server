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

// minioDefaultRegion is the region the MinIO SDK assumes when the server has
// not been told otherwise. Setting it explicitly avoids an unnecessary
// GetBucketLocation roundtrip on every request.
const minioDefaultRegion = "us-east-1"

// minioDefaultBucket is the bucket used when an object path does not carry a
// known prefix from `allowedMinioBuckets`.
const minioDefaultBucket = "file"

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

	ctx := context.Background()
	minioClient, err := sm.newClient()
	if err != nil {
		sm.Error("创建错误：", zap.Error(err))
		return nil, err
	}

	bucketName, fileName := splitBucketAndObject(filePath, minioDefaultBucket, allowedMinioBuckets)
	exists, err := minioClient.BucketExists(ctx, bucketName)
	if err != nil {
		sm.Error(fmt.Sprintf("检测 %s目录是否存在错误", bucketName))
		return nil, err
	}
	if !exists {
		err = minioClient.MakeBucket(ctx, bucketName, minio.MakeBucketOptions{Region: minioDefaultRegion})
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
	minioClient, err := sm.newClient()
	if err != nil {
		return nil, "", err
	}

	bucketName, objectPath := splitBucketAndObject(ph, minioDefaultBucket, allowedMinioBuckets)

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

// newClient builds a MinIO client pinned to the SDK's default region, which
// is what `mc` and the MinIO server itself ship with. Pinning the region
// here lets the SDK skip a GetBucketLocation pre-flight on every request.
func (sm *ServiceMinio) newClient() (*minio.Client, error) {
	minioConfig := sm.ctx.GetConfig().Minio
	uploadUl, _ := url.Parse(minioConfig.UploadURL)
	endpoint := uploadUl.Host
	useSSL := strings.HasPrefix(uploadUl.Scheme, "https")

	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioConfig.AccessKeyID, minioConfig.SecretAccessKey, ""),
		Secure: useSSL,
		Region: minioDefaultRegion,
	})
}

// rewriteToPublicHost rewrites the scheme/host of a presigned URL produced
// against the server-internal `UploadURL`/`DownloadURL` so that the link
// works from a browser. The internal URL is what the Go process talks to
// (often a Docker service name); the public URL is what end-user browsers
// resolve. When `publicBase` is empty the original URL is returned
// unchanged.
func rewriteToPublicHost(u *url.URL, publicBase string) *url.URL {
	publicBase = strings.TrimSpace(publicBase)
	if publicBase == "" {
		return u
	}
	parsed, err := url.Parse(strings.TrimRight(publicBase, "/"))
	if err != nil || parsed.Host == "" {
		return u
	}
	clone := *u
	clone.Host = parsed.Host
	clone.Scheme = parsed.Scheme
	return &clone
}

// PresignedPutURL generates a presigned PUT URL the browser can use to
// upload directly to MinIO, plus the matching anonymous GET URL for the
// resulting object. Bucket auto-creation is *not* performed here — the
// regular UploadFile path is responsible for ensuring the bucket exists
// before any presigned PUT lands. In a fresh deployment, the first server
// upload through UploadFile primes the bucket; subsequent presigned PUTs
// against that bucket succeed.
func (sm *ServiceMinio) PresignedPutURL(objectPath string, contentType string, contentDisposition string, expires time.Duration) (uploadURL string, downloadURL string, err error) {
	client, err := sm.newClient()
	if err != nil {
		return "", "", err
	}

	bucketName, objectKey := splitBucketAndObject(objectPath, minioDefaultBucket, allowedMinioBuckets)
	if objectKey == "" {
		return "", "", fmt.Errorf("空对象路径，无法生成预签名URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var presigned *url.URL
	if contentDisposition != "" || contentType != "" {
		headers := http.Header{}
		if contentType != "" {
			headers.Set("Content-Type", contentType)
		}
		if contentDisposition != "" {
			headers.Set("Content-Disposition", contentDisposition)
		}
		presigned, err = client.PresignHeader(ctx, http.MethodPut, bucketName, objectKey, expires, nil, headers)
	} else {
		presigned, err = client.PresignedPutObject(ctx, bucketName, objectKey, expires)
	}
	if err != nil {
		return "", "", fmt.Errorf("生成预签名URL失败: %w", err)
	}

	minioConfig := sm.ctx.GetConfig().Minio
	uploadURL = rewriteToPublicHost(presigned, minioConfig.UploadURL).String()

	dl, dlErr := sm.DownloadURL(objectPath, "")
	if dlErr != nil {
		sm.Warn("生成下载URL失败", zap.Error(dlErr))
	}
	return uploadURL, dl, nil
}

// PresignedGetURL generates a presigned GET URL with a Content-Disposition
// override so the browser saves the file under the correct user-facing
// filename. MinIO 默认 bucket 为公共读，但鉴权模式下也通过此方法签发。
func (sm *ServiceMinio) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	client, err := sm.newClient()
	if err != nil {
		return "", err
	}

	bucketName, objectKey := splitBucketAndObject(objectPath, minioDefaultBucket, allowedMinioBuckets)
	if objectKey == "" {
		return "", fmt.Errorf("空对象路径，无法生成预签名URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if disposition != "inline" {
		disposition = "attachment"
	}
	params := url.Values{}
	if filename != "" {
		encodedFilename := "UTF-8''" + rfc5987Encode(filename)
		params.Set("response-content-disposition", fmt.Sprintf("%s; filename*=%s", disposition, encodedFilename))
	} else {
		params.Set("response-content-disposition", disposition)
	}

	presigned, err := client.PresignedGetObject(ctx, bucketName, objectKey, expires, params)
	if err != nil {
		return "", fmt.Errorf("生成预签名GET URL失败: %w", err)
	}

	minioConfig := sm.ctx.GetConfig().Minio
	return rewriteToPublicHost(presigned, minioConfig.DownloadURL).String(), nil
}
