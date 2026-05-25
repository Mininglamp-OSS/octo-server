package file

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"
)

// ServiceS3 implements a single-bucket S3 / S3-compatible storage backend.
//
// Targets AWS S3 and any vendor that speaks the S3 protocol with SigV4
// (Cloudflare R2, Backblaze B2, Wasabi, MinIO with a custom endpoint, ...).
// Configuration comes from cfg.S3 — see octo-lib config.S3Config for the
// field semantics and Viper bindings.
//
// Design contrast with ServiceMinio / ServiceCOS:
//
//   - ServiceMinio routes by path prefix into N buckets and pins
//     Region: us-east-1. ServiceS3 uses a single bucket from cfg.S3.Bucket
//     and the explicit cfg.S3.Region; the upload type segment ("chat/",
//     "moment/", ...) becomes part of the object key inside that one bucket.
//   - ServiceCOS bakes in cos.<region>.myqcloud.com endpoint templating
//     plus a CDN-alias-vs-canonical client switch for path-style CDN
//     fronts. ServiceS3 takes the endpoint verbatim from cfg.S3.Endpoint
//     and supports UsePathStyle as an explicit operator opt-in.
//
// Signing model: presigned PUT/GET URLs always sign against cfg.S3.Endpoint.
// cfg.S3.DownloadURL is the browser-facing prefix for UNSIGNED URLs only
// (preview redirects, upload response path) — it never participates in
// SigV4. This deliberate separation removes the previous host-shape
// detection on BucketURL and aligns with the octo-lib S3Config contract
// (see config.S3Config.DownloadURL godoc).
type ServiceS3 struct {
	log.Log
	ctx *config.Context
}

// NewServiceS3 constructs a ServiceS3 from the active config. Required
// fields (Endpoint, Region, Bucket, AccessKeyID, SecretAccessKey) are
// NOT validated here — the constructor returns a usable struct even when
// fields are missing so the process can start (matching the existing
// ServiceMinio / ServiceCOS constructors), and per-request errors
// surface the missing configuration at the first call site. Operators
// should treat "S3 配置缺失" log lines as a startup failure.
func NewServiceS3(ctx *config.Context) *ServiceS3 {
	return &ServiceS3{
		Log: log.NewTLog("FileS3"),
		ctx: ctx,
	}
}

// validateConfig fails the request loudly when any required field is
// missing, rather than letting the SDK surface an opaque
// "endpoint cannot be empty" or signing-with-empty-key error.
func (s *ServiceS3) validateConfig() error {
	cfg := s.ctx.GetConfig().S3
	missing := make([]string, 0, 5)
	if strings.TrimSpace(cfg.Endpoint) == "" {
		missing = append(missing, "s3.endpoint")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		missing = append(missing, "s3.region")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		missing = append(missing, "s3.bucket")
	}
	if strings.TrimSpace(cfg.AccessKeyID) == "" {
		missing = append(missing, "s3.accessKeyID")
	}
	if strings.TrimSpace(cfg.SecretAccessKey) == "" {
		missing = append(missing, "s3.secretAccessKey")
	}
	if len(missing) > 0 {
		return fmt.Errorf("S3 配置缺失: %s 必须设置", strings.Join(missing, ", "))
	}
	return nil
}

// withPrefix joins cfg.S3.Prefix in front of objectPath when set. Mirrors
// ServiceCOS.withPrefix so the multi-environment-shared-bucket layout
// works identically across S3 and COS deployments.
func (s *ServiceS3) withPrefix(objectPath string) string {
	prefix := strings.TrimSpace(s.ctx.GetConfig().S3.Prefix)
	if prefix == "" {
		return objectPath
	}
	return path.Join(prefix, objectPath)
}

// bucketLookup translates cfg.S3.UsePathStyle to a minio-go addressing
// style. Default is BucketLookupDNS (virtual-hosted, the S3 modern
// default that works on AWS S3, R2, B2, Wasabi); UsePathStyle=true
// forces path-style for gateways that only accept that shape.
//
// We pick BucketLookupDNS instead of BucketLookupAuto so the signed
// URL host matches what the operator typed in cfg.S3.Endpoint
// verbatim — BucketLookupAuto rewrites AWS hostnames to their
// dual-stack equivalent (s3.dualstack.<region>.amazonaws.com) which
// surprises operators who configured a strict allow-list against the
// non-dual-stack hostname.
func (s *ServiceS3) bucketLookup() minio.BucketLookupType {
	if s.ctx.GetConfig().S3.UsePathStyle {
		return minio.BucketLookupPath
	}
	return minio.BucketLookupDNS
}

// newClient builds the single SigV4-signing minio-go client used for all
// I/O against this backend: server-side uploads/downloads AND presigned
// PUT/GET signing. The client always targets cfg.S3.Endpoint; cfg.S3.
// DownloadURL is deliberately out of scope here (see file-level doc).
//
// SessionToken is forwarded when present, enabling STS / IRSA / IMDSv2 /
// EKS Pod Identity workflows that issue rotating credentials. The token
// is opaque to ServiceS3 — the deployment pipeline owns refreshing it
// before STS expiry (see octo-lib S3Config.SessionToken godoc).
//
// Region is set explicitly so the SDK skips the GetBucketLocation
// preflight on first use.
//
// HTTPS is hard-coded — see the file-level comment for the rationale.
func (s *ServiceS3) newClient() (*minio.Client, error) {
	cfg := s.ctx.GetConfig().S3
	endpoint := strings.TrimSpace(cfg.Endpoint)
	// Strip an accidental scheme so operators who paste a full URL into
	// the yaml don't get a confusing "endpoint cannot contain scheme"
	// error from the SDK. The S3Config godoc says "hostname, without
	// scheme" but tolerating both shapes is cheap and matches operator
	// muscle memory from the minio/COS blocks.
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimRight(endpoint, "/")
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken),
		Secure:       true,
		Region:       strings.TrimSpace(cfg.Region),
		BucketLookup: s.bucketLookup(),
	})
	if err != nil {
		return nil, fmt.Errorf("创建S3客户端失败: %w", err)
	}
	return client, nil
}

// UploadFile streams a single object into the configured S3 bucket.
// The full filePath (e.g. "chat/2026/foo.jpg") becomes the object key
// after the optional cfg.S3.Prefix is prepended; the single-bucket
// model means the file-type segment lives in the key, not the bucket.
func (s *ServiceS3) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	if err := s.validateConfig(); err != nil {
		return nil, err
	}
	buff := bytes.NewBuffer(make([]byte, 0))
	if err := copyFileWriter(buff); err != nil {
		s.Error("复制文件内容失败！", zap.Error(err))
		return nil, err
	}

	cfg := s.ctx.GetConfig().S3
	client, err := s.newClient()
	if err != nil {
		return nil, err
	}

	key := s.withPrefix(filePath)
	opts := minio.PutObjectOptions{
		ContentType: contentType,
		PartSize:    10 * 1024 * 1024,
	}
	if contentDisposition != "" {
		opts.ContentDisposition = contentDisposition
	}

	ctx := context.Background()
	info, err := client.PutObject(ctx, cfg.Bucket, key, buff, int64(buff.Len()), opts)
	if err != nil {
		s.Error("上传文件到S3失败", zap.Error(err))
		return map[string]interface{}{"path": ""}, err
	}
	return map[string]interface{}{"path": info.Key}, nil
}

// GetFile fetches the object body for server-side handlers that need to
// proxy the file (e.g. legacy /v1/file/preview path). Buckets with
// Block Public Access turned on are still readable here because the SDK
// signs each request.
func (s *ServiceS3) GetFile(ph string) (io.ReadCloser, string, error) {
	if err := s.validateConfig(); err != nil {
		return nil, "", err
	}
	client, err := s.newClient()
	if err != nil {
		return nil, "", err
	}

	cfg := s.ctx.GetConfig().S3
	key := s.withPrefix(ph)
	obj, err := client.GetObject(context.Background(), cfg.Bucket, key, minio.GetObjectOptions{})
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

// DownloadURL returns a browser-facing URL for the object. This is the
// *unsigned* URL — useful only when the bucket allows anonymous GET, or
// when fronted by a CDN that handles auth out-of-band. For private
// buckets (the AWS default with Block Public Access on), callers
// should use PresignedGetURL instead; modules/file/api.go's getFile
// handler redirects through DownloadURL but the public API also
// exposes /v1/file/download/url that goes through PresignedGetURL.
func (s *ServiceS3) DownloadURL(ph string, filename string) (string, error) {
	if err := s.validateConfig(); err != nil {
		return "", err
	}
	return s.publicURL(ph), nil
}

// publicURL constructs the unsigned, browser-facing URL for an object.
// cfg.S3.DownloadURL wins when set (typical CloudFront / CDN front-door
// or custom-domain scenario). Otherwise we build a virtual-hosted URL
// against cfg.S3.Endpoint — works for AWS S3 and any provider whose
// endpoint accepts <bucket>.<endpoint> DNS; with UsePathStyle the URL
// flips to <endpoint>/<bucket>/<key>.
//
// cfg.S3.DownloadURL is NEVER used for SigV4 signing — that responsibility
// belongs to newClient against cfg.S3.Endpoint.
func (s *ServiceS3) publicURL(objectPath string) string {
	cfg := s.ctx.GetConfig().S3
	key := s.withPrefix(objectPath)

	base := strings.TrimRight(strings.TrimSpace(cfg.DownloadURL), "/")
	if base != "" {
		result, _ := url.JoinPath(base, key)
		return result
	}

	endpoint := strings.TrimSpace(cfg.Endpoint)
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	endpoint = strings.TrimRight(endpoint, "/")

	if cfg.UsePathStyle {
		result, _ := url.JoinPath(fmt.Sprintf("https://%s", endpoint), cfg.Bucket, key)
		return result
	}
	// Virtual-hosted style: <bucket>.<endpoint>/<key>
	result, _ := url.JoinPath(fmt.Sprintf("https://%s.%s", cfg.Bucket, endpoint), key)
	return result
}

// PresignedPutURL signs a direct-to-storage PUT URL. fileSize is signed
// into the canonical-headers section as Content-Length; any deviation
// the browser sends is rejected by the gateway with
// 403 SignatureDoesNotMatch. This is the server-side enforcement of
// MaxFileSize on the presigned path — same model as ServiceMinio /
// ServiceCOS, see modules/file/api.go getUploadCredentials for the full
// signed-header contract returned to clients.
//
// Signing host is always cfg.S3.Endpoint; cfg.S3.DownloadURL has no
// influence on the signed URL. The returned downloadURL (unsigned) is
// constructed via publicURL and may carry the DownloadURL prefix.
func (s *ServiceS3) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (uploadURL string, downloadURL string, err error) {
	if fileSize <= 0 {
		return "", "", errors.New("预签名上传必须提供正向的 fileSize（字节数），用于在签名中固定 Content-Length")
	}
	if err := s.validateConfig(); err != nil {
		return "", "", err
	}

	client, err := s.newClient()
	if err != nil {
		return "", "", err
	}

	cfg := s.ctx.GetConfig().S3
	key := s.withPrefix(objectPath)
	if err := validatePresignObjectKey(key); err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	headers := http.Header{}
	headers.Set("Content-Length", strconv.FormatInt(fileSize, 10))
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	if contentDisposition != "" {
		headers.Set("Content-Disposition", contentDisposition)
	}
	presigned, err := client.PresignHeader(ctx, http.MethodPut, cfg.Bucket, key, expires, nil, headers)
	if err != nil {
		return "", "", fmt.Errorf("生成预签名URL失败: %w", err)
	}
	uploadURL = presigned.String()

	dl, dlErr := s.DownloadURL(objectPath, "")
	if dlErr != nil {
		s.Warn("生成下载URL失败", zap.Error(dlErr))
	}
	return uploadURL, dl, nil
}

// PresignedGetURL signs a GET URL with a Content-Disposition override so
// the browser downloads with the operator-supplied filename instead of
// the raw object key. Signing host is always cfg.S3.Endpoint (see
// file-level doc).
func (s *ServiceS3) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	if err := s.validateConfig(); err != nil {
		return "", err
	}
	client, err := s.newClient()
	if err != nil {
		return "", err
	}

	cfg := s.ctx.GetConfig().S3
	key := s.withPrefix(objectPath)
	if err := validatePresignObjectKey(key); err != nil {
		return "", err
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
	}

	presigned, err := client.PresignHeader(ctx, http.MethodGet, cfg.Bucket, key, expires, params, nil)
	if err != nil {
		return "", fmt.Errorf("生成预签名GET URL失败: %w", err)
	}
	return presigned.String(), nil
}
