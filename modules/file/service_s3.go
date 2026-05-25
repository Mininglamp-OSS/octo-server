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
//     and supports UsePathStyle as an explicit operator opt-in instead
//     of detecting it from BucketURL shape.
//
// Connection model: cfg.S3.Endpoint always uses HTTPS by design. The
// optional cfg.S3.BucketURL is operator-owned infrastructure and DOES
// honor `http://` for local emulators; see publicEndpoint and the yaml
// example for the rationale.
type ServiceS3 struct {
	log.Log
	ctx *config.Context
}

// NewServiceS3 constructs a ServiceS3 from the active config. Required
// fields (Endpoint, Region, Bucket) are NOT validated here — the
// constructor returns a usable struct even when fields are missing so
// the process can start (matching the existing ServiceMinio / ServiceCOS
// constructors), and per-request errors surface the missing configuration
// at the first call site. Operators should treat
// "S3 配置缺失" log lines as a startup failure.
func NewServiceS3(ctx *config.Context) *ServiceS3 {
	return &ServiceS3{
		Log: log.NewTLog("FileS3"),
		ctx: ctx,
	}
}

// validateConfig fails the request loudly when any of the three required
// fields is missing, rather than letting the SDK surface an opaque
// "endpoint cannot be empty" or signing-with-empty-key error.
func (s *ServiceS3) validateConfig() error {
	cfg := s.ctx.GetConfig().S3
	missing := make([]string, 0, 3)
	if strings.TrimSpace(cfg.Endpoint) == "" {
		missing = append(missing, "s3.endpoint")
	}
	if strings.TrimSpace(cfg.Region) == "" {
		missing = append(missing, "s3.region")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		missing = append(missing, "s3.bucket")
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

// newClient builds a SigV4-signing minio-go client against cfg.S3.Endpoint.
// Region is set explicitly so the SDK skips the GetBucketLocation
// preflight on first use (same rationale as ServiceCOS.newCanonicalPresignClient).
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
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       true,
		Region:       strings.TrimSpace(cfg.Region),
		BucketLookup: s.bucketLookup(),
	})
	if err != nil {
		return nil, fmt.Errorf("创建S3客户端失败: %w", err)
	}
	return client, nil
}

// publicEndpoint resolves the browser-facing host used to sign presigned
// PUT/GET URLs, and reports the bucket-addressing style the SDK should
// use to reach it. Mirrors ServiceCOS.publicEndpoint — SigV4 covers
// `host` and the canonical URI, so the host we sign against must
// equal the host the browser will hit. Any post-sign rewrite breaks
// the signature.
//
// Resolution rules:
//
//  1. cfg.S3.BucketURL is empty — fall back to cfg.S3.Endpoint with
//     the lookup style chosen by cfg.S3.UsePathStyle.
//
//  2. BucketURL host begins with "<bucket>." (canonical bucket-subdomain
//     shape, e.g. `https://my-bucket.s3.us-west-2.amazonaws.com` or
//     `https://my-bucket.cdn.example.com`). The bucket already lives
//     in the host, so we strip "<bucket>." before handing the parent
//     host to the SDK and use BucketLookupDNS; the SDK re-attaches
//     "<bucket>." and the resulting signed URL equals BucketURL+key.
//
//  3. BucketURL host has no "<bucket>." prefix (CDN alias / accelerator,
//     e.g. `https://files.example.com`). The CDN routes to the bucket
//     by some out-of-band rule (origin alias, path mapping, ...).
//     The SDK signs against the host as-is with BucketLookupPath and
//     emits `<host>/<bucket>/<key>`. This deliberately produces a URL
//     shape that differs from publicURL (which returns
//     `<BucketURL>/<key>` without a bucket segment) — operators
//     fronting the bucket with a CDN must route both shapes; see the
//     yaml docs for the deployment recipe.
//
// `host` is the bare host[:port] suitable for minio.New (no scheme,
// no path). `secure` reflects the URL scheme — http:// flips it false
// for the rare local-emulator-on-CDN case (the file-level "HTTPS only"
// framing applies to cfg.S3.Endpoint; BucketURL is operator-owned
// infrastructure and treated as such). `lookup` is the SDK addressing
// style: DNS for shapes 1 & 2, Path for shape 3.
func (s *ServiceS3) publicEndpoint() (host string, secure bool, lookup minio.BucketLookupType, err error) {
	cfg := s.ctx.GetConfig().S3
	base := strings.TrimSpace(cfg.BucketURL)
	if base == "" {
		ep := strings.TrimSpace(cfg.Endpoint)
		ep = strings.TrimPrefix(ep, "https://")
		ep = strings.TrimPrefix(ep, "http://")
		ep = strings.TrimRight(ep, "/")
		return ep, true, s.bucketLookup(), nil
	}
	parsed, parseErr := url.Parse(strings.TrimRight(base, "/"))
	if parseErr != nil || parsed == nil || parsed.Host == "" {
		s.Warn("s3.bucketURL 解析失败，预签名URL退回到 s3.endpoint",
			zap.String("bucketURL", base))
		ep := strings.TrimSpace(cfg.Endpoint)
		ep = strings.TrimPrefix(ep, "https://")
		ep = strings.TrimPrefix(ep, "http://")
		ep = strings.TrimRight(ep, "/")
		return ep, true, s.bucketLookup(), nil
	}
	if parsed.Path != "" && parsed.Path != "/" {
		// SigV4 covers the canonical URI; a path component on BucketURL
		// would be stripped before signing and produce
		// SignatureDoesNotMatch at the browser. Reject loudly rather
		// than silently dropping the path.
		return "", false, 0, fmt.Errorf("s3.bucketURL 不可包含路径前缀（%q），仅支持 scheme://host[:port] 形式", base)
	}
	secure = !strings.EqualFold(parsed.Scheme, "http")
	h := parsed.Host
	if cfg.Bucket != "" {
		bucketPrefix := cfg.Bucket + "."
		if strings.HasPrefix(h, bucketPrefix) {
			// Bucket-subdomain shape: strip "<bucket>." so the SDK with
			// BucketLookupDNS can re-attach it without producing
			// "<bucket>.<bucket>.<parent>".
			parent := strings.TrimPrefix(h, bucketPrefix)
			if parent == "" {
				s.Warn("s3.bucketURL 仅包含 bucket 子域，无父域可签名，回退到 s3.endpoint",
					zap.String("bucketURL", base))
				ep := strings.TrimSpace(cfg.Endpoint)
				ep = strings.TrimPrefix(ep, "https://")
				ep = strings.TrimPrefix(ep, "http://")
				ep = strings.TrimRight(ep, "/")
				return ep, true, s.bucketLookup(), nil
			}
			return parent, secure, minio.BucketLookupDNS, nil
		}
	}
	// CDN alias / accelerator: no "<bucket>." prefix. Sign against the
	// host directly with path-style addressing.
	return h, secure, minio.BucketLookupPath, nil
}

// newPublicClient builds the client used for presigned URL generation.
// Routes through publicEndpoint so the signed URL's host matches the
// host the browser will hit; otherwise SigV4 rejects at validation.
func (s *ServiceS3) newPublicClient() (*minio.Client, error) {
	cfg := s.ctx.GetConfig().S3
	host, secure, lookup, err := s.publicEndpoint()
	if err != nil {
		return nil, err
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       secure,
		Region:       strings.TrimSpace(cfg.Region),
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("创建S3公网客户端失败: %w", err)
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

// publicURL constructs the public-shape URL for an object. If BucketURL
// is set it wins (typical CDN front-door scenario). Otherwise we build
// a virtual-hosted URL against cfg.S3.Endpoint — works for AWS S3 and
// any provider whose endpoint accepts <bucket>.<endpoint> DNS.
func (s *ServiceS3) publicURL(objectPath string) string {
	cfg := s.ctx.GetConfig().S3
	key := s.withPrefix(objectPath)

	base := strings.TrimRight(strings.TrimSpace(cfg.BucketURL), "/")
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
func (s *ServiceS3) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (uploadURL string, downloadURL string, err error) {
	if fileSize <= 0 {
		return "", "", errors.New("预签名上传必须提供正向的 fileSize（字节数），用于在签名中固定 Content-Length")
	}
	if err := s.validateConfig(); err != nil {
		return "", "", err
	}

	client, err := s.newPublicClient()
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
// the raw object key.
func (s *ServiceS3) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	if err := s.validateConfig(); err != nil {
		return "", err
	}
	client, err := s.newPublicClient()
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
