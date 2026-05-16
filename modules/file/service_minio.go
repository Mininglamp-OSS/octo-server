package file

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
// itself initiates the request. Browser-facing presigned URLs must instead
// be issued by `newPublicClient` so the SigV4 signature is valid for the
// host the browser actually resolves.
func (sm *ServiceMinio) newClient() (*minio.Client, error) {
	minioConfig := sm.ctx.GetConfig().Minio
	return sm.newClientForEndpoint(minioConfig.UploadURL)
}

// newPublicClient builds a MinIO client signing against the browser-facing
// endpoint resolved by `publicEndpoint`. Presigned PUT/GET URLs MUST be
// issued from this client: SigV4 includes `host` in the signed headers, so
// any post-sign host rewrite invalidates the signature. Signing once with
// the public host means the URL the browser receives is the URL the
// signature is valid for, no rewriting needed.
//
// The public endpoint is interpreted as `scheme://host:port` only —
// reverse-proxy path prefixes are not supported here (see `publicEndpoint`
// for the rationale and the validation that enforces it).
func (sm *ServiceMinio) newPublicClient() (*minio.Client, error) {
	return sm.newClientForEndpoint(sm.publicEndpoint())
}

// newClientForEndpoint builds a MinIO client against an arbitrary base URL.
// Endpoint scheme drives TLS; an empty or unparseable base URL surfaces as
// the SDK's "endpoint cannot be empty" error rather than producing a client
// silently bound to the wrong host. Only `parsed.Host` is consumed — any
// path component on the URL is ignored at this layer; callers that hand in
// the public endpoint are expected to validate up front that no path
// prefix is configured.
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
//     Operators running behind a reverse proxy or with split internal /
//     external hosts SHOULD set this. The value MUST be a `scheme://host:port`
//     URL with no path component; see `validatePublicDownloadURL` for the
//     rationale.
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

// validatePublicDownloadURL enforces the host:port-only contract on
// `cfg.Minio.DownloadURL`. SigV4 covers `host` and the canonical URI in
// the signed headers; any reverse-proxy path-strip between the browser
// and the MinIO server will rewrite the canonical URI mid-flight and
// produce SignatureDoesNotMatch on every request. There is no clean way
// to keep a path prefix working under standard nginx `proxy_pass <upstream>/`
// semantics (which is the documented and idiomatic shape for path-routed
// deployments), so we explicitly reject it at sign time rather than
// silently produce broken URLs.
//
// Accepted shapes:
//   - empty string (caller falls back to UploadURL / URL)
//   - "scheme://host[:port]" with no path
//   - "scheme://host[:port]/" — single trailing slash, treated as no path
//
// Rejected: any URL whose path is something other than "" or "/", e.g.
// "https://octo.example.com/minio". For path-proxied deployments the
// fix is to expose the MinIO API directly (subdomain + DNS, or a
// dedicated host port) rather than route it through a path prefix.
func validatePublicDownloadURL(rawURL string) error {
	v := strings.TrimSpace(rawURL)
	if v == "" {
		return nil
	}
	parsed, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("minio.downloadURL 不是合法 URL: %q: %w", rawURL, err)
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return fmt.Errorf(
			"minio.downloadURL must be a host:port URL without a path prefix; "+
				"for path-proxied deployments, expose the MinIO API directly instead "+
				"(got %q)", rawURL)
	}
	return nil
}

// validatePresignObjectKey rejects object keys that would produce a
// nonsense S3 path: empty keys, leading slash (which becomes `//` after
// the bucket separator and trips the same gateway-normalization hazard
// as the embedded-`//` case below), trailing slash (no actual object),
// and any embedded `//` (a directory marker that the gateway would
// normalize away, breaking signature validation). Matches the empty-key
// error style used throughout the file module.
func validatePresignObjectKey(objectKey string) error {
	if objectKey == "" {
		return fmt.Errorf("空对象路径，无法生成预签名URL")
	}
	if strings.HasPrefix(objectKey, "/") {
		// `splitBucketAndObject` splits on the first `/`, so an input like
		// `chat//foo.png` yields bucket=`chat`, objectKey=`/foo.png`. The
		// signed canonical URI then becomes `/chat//foo.png` and gets
		// path-normalized to `/chat/foo.png` by HTTP intermediaries —
		// same SignatureDoesNotMatch failure mode as the embedded-`//`
		// rejection below, just routed through the bucket-separator
		// boundary. Reject up front for parity.
		return fmt.Errorf("对象路径不可以斜杠开头，无法生成预签名URL: %q", objectKey)
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
// (`publicEndpoint`), not the server-internal one. SigV4 includes `host` in
// the signed headers, so any post-sign host change would invalidate the
// signature; signing with the public host up front is the only way for the
// resulting URL to be valid as-is from a browser. Bucket bootstrap still
// runs against the internal client because it needs network reachability,
// not signature validity for the browser.
//
// `fileSize` is signed into the canonical-headers section as
// `Content-Length`. The browser MUST echo the same Content-Length on its
// PUT (browsers compute this automatically from the request body length),
// and the storage gateway rejects any mismatch with
// SignatureDoesNotMatch. This is the server-side enforcement of the
// upload size cap on the presigned path: a caller cannot exceed the
// signed budget without invalidating the URL — closes the size-bypass
// security gap that the multipart `uploadFile` handler closes via
// `http.MaxBytesReader` + `MaxFileSize` on the request body. Pass a
// non-positive `fileSize` and the function returns an error rather than
// silently producing an unbounded URL.
func (sm *ServiceMinio) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (uploadURL string, downloadURL string, err error) {
	if fileSize <= 0 {
		return "", "", fmt.Errorf("预签名上传必须提供正向的 fileSize（字节数），用于在签名中固定 Content-Length")
	}
	if err := validatePublicDownloadURL(sm.ctx.GetConfig().Minio.DownloadURL); err != nil {
		return "", "", err
	}
	internalClient, err := sm.newClient()
	if err != nil {
		return "", "", err
	}
	publicClient, err := sm.newPublicClient()
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

	// Always go through PresignHeader so Content-Length lands in the
	// signed headers — without that the gateway would happily accept a
	// PUT of any size against the URL. Content-Type / Content-Disposition
	// piggy-back on the same path so the per-header behaviour stays
	// consistent with the rest of the file module.
	headers := http.Header{}
	headers.Set("Content-Length", strconv.FormatInt(fileSize, 10))
	if contentType != "" {
		headers.Set("Content-Type", contentType)
	}
	if contentDisposition != "" {
		headers.Set("Content-Disposition", contentDisposition)
	}
	presigned, err := publicClient.PresignHeader(ctx, http.MethodPut, bucketName, objectKey, expires, nil, headers)
	if err != nil {
		return "", "", fmt.Errorf("生成预签名URL失败: %w", err)
	}

	uploadURL = presigned.String()

	dl, dlErr := sm.DownloadURL(objectPath, "")
	if dlErr != nil {
		sm.Warn("生成下载URL失败", zap.Error(dlErr))
	}
	return uploadURL, dl, nil
}

// PresignedGetURL generates a presigned GET URL with a Content-Disposition
// override so the browser saves the file under the correct user-facing
// filename. The URL is signed against the browser-facing endpoint
// (`publicEndpoint`); no post-sign host rewriting is performed. MinIO 默认
// bucket 为公共读，但鉴权模式下也通过此方法签发。
func (sm *ServiceMinio) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	if err := validatePublicDownloadURL(sm.ctx.GetConfig().Minio.DownloadURL); err != nil {
		return "", err
	}
	client, err := sm.newPublicClient()
	if err != nil {
		return "", err
	}

	bucketName, objectKey := splitBucketAndObject(objectPath, minioDefaultBucket, allowedMinioBuckets)
	if err := validatePresignObjectKey(objectKey); err != nil {
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
	} else {
		params.Set("response-content-disposition", disposition)
	}

	presigned, err := client.PresignedGetObject(ctx, bucketName, objectKey, expires, params)
	if err != nil {
		return "", fmt.Errorf("生成预签名GET URL失败: %w", err)
	}
	return presigned.String(), nil
}
