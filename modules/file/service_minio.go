package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/minio/minio-go/v7/pkg/signer"
	"go.uber.org/zap"
)

// minioDefaultRegion is the region the MinIO SDK assumes when the server has
// not been told otherwise. Setting it explicitly avoids an unnecessary
// GetBucketLocation roundtrip on every request.
const minioDefaultRegion = "us-east-1"

// minioDefaultBucket is the bucket used when an object path does not carry a
// known prefix from `allowedMinioBuckets`.
const minioDefaultBucket = "file"

// minioBucketAlreadyOwnedByYou is the S3 error code returned when MakeBucket
// races with another caller that already created the same bucket under the
// same credentials. Treated as a benign no-op by `ensureBucket`.
const minioBucketAlreadyOwnedByYou = "BucketAlreadyOwnedByYou"

// readOnlyAnonymousPolicy is the bucket policy applied to every auto-created
// bucket: anonymous principals can GET objects, but uploads and deletes
// remain authenticated. Identical to the policy that the legacy `UploadFile`
// path used to inline.
const readOnlyAnonymousPolicy = `{
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

// ServiceMinio 文件上传
type ServiceMinio struct {
	log.Log
	ctx            *config.Context
	downloadClient *http.Client

	// bucketLocks serializes ensureBucket calls per bucket so concurrent
	// uploads to a fresh bucket cannot double-create or race the
	// SetBucketPolicy step. The map is keyed by bucket name; values are
	// `*sync.Mutex` lazily inserted on first use via LoadOrStore. The map
	// itself is never deleted from — bucket count is bounded by the
	// allow-list, so growth is O(allowed buckets).
	bucketLocks sync.Map
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

// ensureBucket guarantees that `bucket` exists on the MinIO server and has
// the read-only anonymous GET policy applied. Safe to call concurrently for
// the same bucket — a per-bucket mutex serializes the BucketExists / MakeBucket
// / SetBucketPolicy sequence so two parallel callers never double-create or
// race the policy update. The `BucketAlreadyOwnedByYou` S3 response is
// swallowed as a benign no-op for the case where another process (or another
// node sharing these credentials) won the create race.
func (sm *ServiceMinio) ensureBucket(ctx context.Context, client *minio.Client, bucket string) error {
	mtxIface, _ := sm.bucketLocks.LoadOrStore(bucket, &sync.Mutex{})
	mtx := mtxIface.(*sync.Mutex)
	mtx.Lock()
	defer mtx.Unlock()

	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		sm.Error(fmt.Sprintf("检测 %s目录是否存在错误", bucket), zap.Error(err))
		return err
	}
	if exists {
		return nil
	}

	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: minioDefaultRegion}); err != nil {
		// Another caller (different process / different node sharing the
		// same credentials) may have created the bucket between our
		// BucketExists call and our MakeBucket call. Treat that specific
		// S3 response as a no-op rather than a hard failure.
		if minio.ToErrorResponse(err).Code == minioBucketAlreadyOwnedByYou {
			sm.Info("bucket already owned by us, skipping create", zap.String("bucket", bucket))
		} else {
			sm.Error(fmt.Sprintf("创建 %s目录失败", bucket), zap.Error(err))
			return err
		}
	}

	// Read-only public policy: allow anonymous download only. Upload and
	// delete go through authenticated server-side credentials.
	if err := client.SetBucketPolicy(ctx, bucket, fmt.Sprintf(readOnlyAnonymousPolicy, bucket)); err != nil {
		sm.Error("设置minio文件读写权限错误", zap.Error(err))
		return err
	}
	return nil
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
	if err := sm.ensureBucket(ctx, minioClient, bucketName); err != nil {
		return nil, err
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
//
// This client targets the *server-internal* `UploadURL` (typically a
// container service name like `minio:9000`). It is used by UploadFile,
// GetFile, and the bucket-bootstrap path — i.e. anywhere the Go process
// itself initiates the request. Browser-facing presigned URLs are instead
// produced by `presignPublicURL`, which signs with the public host and
// any reverse-proxy path prefix baked into the canonical URI from the
// start. Server-internal endpoints never need a path prefix, so this
// client only consumes the parsed `Host`.
func (sm *ServiceMinio) newClient() (*minio.Client, error) {
	minioConfig := sm.ctx.GetConfig().Minio
	return sm.newClientForEndpoint(minioConfig.UploadURL)
}

// newClientForEndpoint builds a MinIO client against an arbitrary
// server-internal base URL. Endpoint scheme drives TLS; an empty or
// unparseable base URL surfaces as the SDK's "endpoint cannot be empty"
// error rather than producing a client silently bound to the wrong host.
//
// Note: only `parsed.Host` is consumed here. Reverse-proxy path prefixes
// (e.g. `https://octo.example.com/minio`) belong to the browser-facing
// endpoint, never the server-internal one — server-internal traffic
// hits MinIO directly. The presign path handles path-prefix preservation
// in `presignPublicURL`.
func (sm *ServiceMinio) newClientForEndpoint(baseURL string) (*minio.Client, error) {
	minioConfig := sm.ctx.GetConfig().Minio
	parsed, _ := url.Parse(strings.TrimRight(baseURL, "/"))
	endpoint := ""
	useSSL := false
	if parsed != nil {
		endpoint = parsed.Host
		useSSL = strings.HasPrefix(parsed.Scheme, "https")
	}
	return minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(minioConfig.AccessKeyID, minioConfig.SecretAccessKey, ""),
		Secure: useSSL,
		Region: minioDefaultRegion,
	})
}

// publicEndpoint returns the browser-facing MinIO base URL used to issue
// presigned URLs. Resolution order:
//
//  1. `cfg.Minio.DownloadURL` — the documented browser-facing endpoint.
//     Operators behind nginx or running with split internal / external
//     hosts SHOULD set this.
//  2. `cfg.Minio.UploadURL` — fallback when DownloadURL is empty. Logged
//     as a warning because in any non-trivial deployment this is the
//     server-internal hostname (e.g. a Docker service name) which the
//     browser cannot resolve, and the resulting presigned URL will fail.
//  3. `cfg.Minio.URL` — last-resort fallback, same caveat.
//
// Note: octo-lib's MinioConfig auto-fills DownloadURL from URL when both
// are blank, so reaching the UploadURL fallback here in practice means
// the operator explicitly configured separate URL/UploadURL/DownloadURL
// values and zeroed DownloadURL — typically a misconfiguration. A
// future octo-lib release may rename this field to `PublicEndpoint` and
// deprecate `DownloadURL` to make the role explicit; this resolver is
// the single point at which that rename would land in octo-server.
func (sm *ServiceMinio) publicEndpoint() string {
	minioConfig := sm.ctx.GetConfig().Minio
	if v := strings.TrimSpace(minioConfig.DownloadURL); v != "" {
		return v
	}
	if v := strings.TrimSpace(minioConfig.UploadURL); v != "" {
		sm.Warn("minio.DownloadURL 未设置，预签名URL将退回到 UploadURL；浏览器可能无法解析此主机",
			zap.String("uploadURL", v))
		return v
	}
	sm.Warn("minio.DownloadURL 与 UploadURL 都未设置，预签名URL退回到 minio.URL")
	return strings.TrimSpace(minioConfig.URL)
}

// presignPublicURL builds a SigV4 presigned URL aimed at the browser-facing
// endpoint resolved by `publicEndpoint`. The endpoint's path component
// (e.g. "/minio" for an nginx-proxied deployment that routes
// `https://octo.example.com/minio/*` to the MinIO service) is included in
// the canonical URI from the start, so the signature is valid for the URL
// the browser will actually hit. No post-sign URL rewriting is performed
// — that would invalidate the signature, since SigV4 covers `host` and the
// canonical URI.
//
// We bypass minio-go's high-level URL machinery here because `minio.New`
// only consumes `host:port` from the endpoint and offers no hook to inject
// a reverse-proxy path prefix. The request shape we build mirrors what
// minio-go's `makeTargetURL` produces for path-style buckets, plus the
// configured prefix.
//
// Bucket bootstrap (`ensureBucket`) still runs against the internal
// `UploadURL` client — only the URL handed to the browser is signed with
// the public path baked in.
func (sm *ServiceMinio) presignPublicURL(method, bucketName, objectKey string, expires time.Duration, query url.Values, extraHeaders http.Header) (string, error) {
	cfg := sm.ctx.GetConfig().Minio
	base := strings.TrimRight(sm.publicEndpoint(), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed == nil || parsed.Host == "" {
		return "", fmt.Errorf("浏览器可达的MinIO公共端点无效或未配置: %q", base)
	}
	// `parsed.Path` carries the reverse-proxy prefix (e.g. "/minio"); empty
	// for direct host:port deployments. Trim any trailing slash so we don't
	// emit "//bucket/object" when the operator wrote "https://host/minio/".
	pathPrefix := strings.TrimSuffix(parsed.Path, "/")

	// Match minio-go's path-style URL construction (see api.go
	// `makeTargetURL`) so the canonical URI we sign is byte-for-byte the
	// path the browser will request. `s3utils.EncodePath` is the same
	// encoder PreSignV4 uses to build the canonical request, which keeps
	// the signed canonical URI consistent with `req.URL.String()` for
	// arbitrary UTF-8 object keys.
	urlStr := parsed.Scheme + "://" + parsed.Host + pathPrefix + "/" + bucketName + "/" + s3utils.EncodePath(objectKey)
	if len(query) > 0 {
		urlStr += "?" + s3utils.QueryEncode(query)
	}
	target, err := url.Parse(urlStr)
	if err != nil {
		return "", fmt.Errorf("解析预签名目标URL失败: %w", err)
	}

	req := http.Request{
		Method: method,
		URL:    target,
		Host:   target.Host,
		Header: http.Header{},
	}
	for k, vs := range extraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	expSecs := int64(expires / time.Second)
	if expSecs <= 0 {
		expSecs = 1
	}
	signed := signer.PreSignV4(req, cfg.AccessKeyID, cfg.SecretAccessKey, "", minioDefaultRegion, expSecs)
	return signed.URL.String(), nil
}

// validatePresignObjectKey rejects object keys that would produce a
// nonsense S3 path: empty keys, trailing slash (no actual object), and
// any embedded `//` (a directory marker that the gateway would normalize
// away, breaking signature validation). Matches the empty-key error
// style used throughout the file module.
func validatePresignObjectKey(objectKey string) error {
	if objectKey == "" {
		return fmt.Errorf("空对象路径，无法生成预签名URL")
	}
	if strings.HasSuffix(objectKey, "/") {
		return fmt.Errorf("对象路径不可以斜杠结尾，无法生成预签名URL: %q", objectKey)
	}
	if strings.Contains(objectKey, "//") {
		return fmt.Errorf("对象路径包含连续斜杠，无法生成预签名URL: %q", objectKey)
	}
	return nil
}

// PresignedPutURL generates a presigned PUT URL the browser can use to
// upload directly to MinIO, plus the matching anonymous GET URL for the
// resulting object. The target bucket is bootstrapped on first use via
// `ensureBucket` so a presigned PUT against a fresh deployment never lands
// on a NoSuchBucket response.
//
// The returned URL is signed against the *browser-facing* endpoint
// (`publicEndpoint`), not the server-internal one. SigV4 includes `host`
// and the canonical URI in the signed headers, so any post-sign host or
// path change would invalidate the signature; signing with the public
// host (and any reverse-proxy path prefix) up front is the only way for
// the resulting URL to be valid as-is from a browser. Bucket bootstrap
// still runs against the internal client because it needs network
// reachability, not signature validity for the browser.
func (sm *ServiceMinio) PresignedPutURL(objectPath string, contentType string, contentDisposition string, expires time.Duration) (uploadURL string, downloadURL string, err error) {
	internalClient, err := sm.newClient()
	if err != nil {
		return "", "", err
	}

	bucketName, objectKey := splitBucketAndObject(objectPath, minioDefaultBucket, allowedMinioBuckets)
	if err := validatePresignObjectKey(objectKey); err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := sm.ensureBucket(ctx, internalClient, bucketName); err != nil {
		return "", "", fmt.Errorf("预签名上传前的目录引导失败: %w", err)
	}

	// Mirror minio-go's PresignHeader behaviour: when the caller pins
	// Content-Type / Content-Disposition, fold them into the signed
	// headers so the browser must replay them exactly to authenticate.
	// Otherwise sign only the implicit `host` header, matching
	// PresignedPutObject.
	var extraHeaders http.Header
	if contentType != "" || contentDisposition != "" {
		extraHeaders = http.Header{}
		if contentType != "" {
			extraHeaders.Set("Content-Type", contentType)
		}
		if contentDisposition != "" {
			extraHeaders.Set("Content-Disposition", contentDisposition)
		}
	}

	uploadURL, err = sm.presignPublicURL(http.MethodPut, bucketName, objectKey, expires, nil, extraHeaders)
	if err != nil {
		return "", "", fmt.Errorf("生成预签名URL失败: %w", err)
	}

	dl, dlErr := sm.DownloadURL(objectPath, "")
	if dlErr != nil {
		sm.Warn("生成下载URL失败", zap.Error(dlErr))
	}
	return uploadURL, dl, nil
}

// PresignedGetURL generates a presigned GET URL with a Content-Disposition
// override so the browser saves the file under the correct user-facing
// filename. The URL is signed against the browser-facing endpoint
// (`publicEndpoint`), with any reverse-proxy path prefix baked into the
// canonical URI; no post-sign host or path rewriting is performed. MinIO
// 默认 bucket 为公共读，但鉴权模式下也通过此方法签发。
func (sm *ServiceMinio) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	bucketName, objectKey := splitBucketAndObject(objectPath, minioDefaultBucket, allowedMinioBuckets)
	if err := validatePresignObjectKey(objectKey); err != nil {
		return "", err
	}

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

	signed, err := sm.presignPublicURL(http.MethodGet, bucketName, objectKey, expires, params, nil)
	if err != nil {
		return "", fmt.Errorf("生成预签名GET URL失败: %w", err)
	}
	return signed, nil
}
