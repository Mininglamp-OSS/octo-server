package file

import (
	"bytes"
	"context"
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

// getClient builds a COS client targeted at the *server-internal* default
// endpoint (`cos.<region>.myqcloud.com`). It is used by UploadFile / GetFile
// — i.e. anywhere the Go process itself initiates the request — where the
// canonical SDK endpoint is the right thing to hit.
//
// Browser-facing presigned URLs MUST instead be issued by `newPublicClient`
// so the SigV4 signature is valid for the host the browser actually
// resolves. SigV4 covers `host` in the signed headers, so any post-sign
// host change would invalidate the signature — this is the same hazard
// MinIO closed at PR#50 R3+, mirrored here for COS.
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

// publicEndpoint resolves the browser-facing parent domain used to issue
// presigned URLs, and reports the addressing style (DNS vs path) the
// minio SDK should use to reach it.
//
// COS canonically uses virtual-hosted-style addressing
// (`<bucket>.<host>/<key>`), but operators front the bucket with a
// custom CDN / accelerator that exposes the bucket *path-style*
// (`<host>/<bucket>/<key>`). Both shapes are supported:
//
//  1. Bucket-subdomain BucketURL (e.g.
//     `https://my-bucket-12345678.cos.example.com`) — the documented
//     shape. The `<bucket>.` prefix is stripped here so what we hand
//     the SDK is the parent domain (`cos.example.com`); the SDK's
//     `BucketLookupDNS` then re-attaches `<bucket>.` and the signed
//     URL host matches BucketURL exactly.
//
//  2. Path-style BucketURL (e.g. `https://cdn.example.com`,
//     `https://files.example.com`) — typical of a CDN that fronts the
//     bucket without bucket-as-subdomain DNS. Detection key: the host
//     does NOT start with `<bucket>.`. We hand the host to the SDK
//     verbatim and request `BucketLookupPath`, so the SDK signs and
//     produces `https://cdn.example.com/<bucket>/<key>` — the host
//     the browser actually resolves.
//
//  3. BucketURL empty — fall back to the SDK canonical endpoint
//     `cos.<region>.myqcloud.com` with `BucketLookupDNS`. This is the
//     "no custom domain" deployment shape.
//
// In every case the URL is signed against the same host the browser
// will hit. SigV4 covers `host` in the signed headers, so any post-sign
// host rewrite would invalidate the signature — see the R6→R7 fix
// history in this file.
//
// Returned `host` is the bare host[:port] suitable for `minio.New` (no
// scheme, no path). `secure` reflects the URL scheme — `http://` flips
// it to false so HTTP-only deployments (e.g. local emulators) do not
// get silently upgraded to HTTPS. `lookup` is the SDK addressing style
// to pair with `host` — DNS for bucket-subdomain BucketURL, Path for
// path-style BucketURL.
//
// Hotfix history: the path-style branch is YUJ-846 (PR#50 R7→R8
// follow-up). Before this fix the function returned only `(host,
// secure)` and `newPublicClient` hardcoded `BucketLookupDNS`, so a
// path-style BucketURL like `https://cdn.example.com` was silently
// rewritten by the SDK into `https://<bucket>.cdn.example.com` — a
// hostname that did not exist in DNS, producing
// `net::ERR_NAME_NOT_RESOLVED` on every browser PUT.
func (sc *ServiceCOS) publicEndpoint() (host string, secure bool, lookup minio.BucketLookupType) {
	cosConfig := sc.ctx.GetConfig().COS
	defaultHost := fmt.Sprintf("cos.%s.myqcloud.com", cosConfig.Region)

	base := strings.TrimSpace(cosConfig.BucketURL)
	if base == "" {
		return defaultHost, true, minio.BucketLookupDNS
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil || parsed == nil || parsed.Host == "" {
		sc.Warn("cos.bucketURL 解析失败，回退到默认 COS 域名", zap.String("bucketURL", base))
		return defaultHost, true, minio.BucketLookupDNS
	}

	secure = !strings.EqualFold(parsed.Scheme, "http")
	h := parsed.Host

	if cosConfig.Bucket != "" {
		bucketPrefix := cosConfig.Bucket + "."
		if strings.HasPrefix(h, bucketPrefix) {
			// Bucket-subdomain shape: strip the `<bucket>.` prefix so
			// that BucketLookupDNS can re-attach it without producing
			// `<bucket>.<bucket>.cos...`.
			h = strings.TrimPrefix(h, bucketPrefix)
			if h == "" {
				// Bucket-name-only host (no parent domain) is degenerate
				// and not a valid endpoint. Fall back to the default.
				sc.Warn("cos.bucketURL 仅包含 bucket 子域，无父域可用作签名 endpoint，回退到默认 COS 域名",
					zap.String("bucketURL", base))
				return defaultHost, true, minio.BucketLookupDNS
			}
			return h, secure, minio.BucketLookupDNS
		}
	}

	// Path-style: BucketURL host has no `<bucket>.` prefix, so the
	// operator clearly intends the host to be reached as-is and the
	// bucket to live in the URL path. SDK signs against `host`
	// directly and emits `<host>/<bucket>/<key>`.
	return h, secure, minio.BucketLookupPath
}

// newPublicClient builds a COS client signing against the browser-facing
// endpoint resolved by `publicEndpoint`. Presigned PUT/GET URLs MUST be
// issued from this client: SigV4 covers `host` in the signed headers, so
// any post-sign host rewrite invalidates the signature. Signing once with
// the public host means the URL the browser receives is the URL the
// signature is valid for — no rewriting needed. Same hazard MinIO closed
// at PR#50 R3+; this is the COS-side mirror.
//
// The bucket addressing style (`BucketLookupDNS` vs `BucketLookupPath`)
// is decided by `publicEndpoint` from the BucketURL shape — see that
// function for the rules. We propagate the chosen style through to the
// SDK so signing matches the URL we will hand the browser:
// virtual-hosted (`<bucket>.<host>/<key>`) for bucket-subdomain
// BucketURL, path-style (`<host>/<bucket>/<key>`) for CDN / accelerator
// BucketURL without a `<bucket>.` subdomain.
//
// `Region` is set explicitly so the SDK skips a GetBucketLocation
// preflight on first use — that preflight is the wrong thing to do for
// pure URL signing (it is network I/O against a host the test
// environment cannot resolve), and once skipped the presign path
// becomes deterministic / offline.
func (sc *ServiceCOS) newPublicClient() (*minio.Client, error) {
	cosConfig := sc.ctx.GetConfig().COS
	host, secure, lookup := sc.publicEndpoint()
	region := strings.TrimSpace(cosConfig.Region)
	if region == "" {
		// SDK default; only reached if operator left region blank.
		region = "us-east-1"
	}
	client, err := minio.New(host, &minio.Options{
		Creds:        credentials.NewStaticV4(cosConfig.SecretID, cosConfig.SecretKey, ""),
		Secure:       secure,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("创建COS公网客户端失败: %w", err)
	}
	return client, nil
}

// newCanonicalPresignClient builds a COS client for presigned URL generation
// against the canonical COS endpoint (`<bucket>.cos.<region>.myqcloud.com`).
//
// This is used when BucketURL is a CDN alias (no `<bucket>.` subdomain):
// CDN domains are bucket aliases — the CDN origin routes to the bucket
// implicitly. The SDK's path-style `/<bucket>/<key>` URL shape has no
// corresponding route on the CDN, so presigned URLs must go directly to
// COS. The CDN is only used for non-presigned download URLs via
// `publicURL`.
//
// Unlike `getClient` (which is for server-side I/O and does not set
// Region), this sets Region explicitly so the SDK skips the
// GetBucketLocation preflight — same rationale as `newPublicClient`.
func (sc *ServiceCOS) newCanonicalPresignClient() (*minio.Client, error) {
	cosConfig := sc.ctx.GetConfig().COS
	endpoint := fmt.Sprintf("cos.%s.myqcloud.com", cosConfig.Region)
	region := strings.TrimSpace(cosConfig.Region)
	if region == "" {
		region = "us-east-1"
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cosConfig.SecretID, cosConfig.SecretKey, ""),
		Secure:       true,
		Region:       region,
		BucketLookup: minio.BucketLookupDNS,
	})
	if err != nil {
		return nil, fmt.Errorf("创建COS签名客户端失败: %w", err)
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
//
// Client selection depends on the BucketURL shape:
//
//   - Bucket-subdomain BucketURL (e.g. `https://<bucket>.cos.example.com`)
//     or empty BucketURL: sign against the browser-facing endpoint via
//     `newPublicClient`. SigV4 covers `host`, so the browser hits the
//     same host the signature covers.
//
//   - CDN alias BucketURL (e.g. `https://cdn.deepminer.com.cn`): sign
//     against the canonical COS endpoint via `getClient` (virtual-hosted
//     `<bucket>.cos.<region>.myqcloud.com`). CDN domains are bucket
//     aliases — the CDN origin routes to the bucket implicitly, so the
//     SDK's path-style `/<bucket>/<key>` URL shape has no corresponding
//     route on the CDN. Signing against the real COS endpoint means the
//     browser uploads/downloads directly to COS, bypassing the CDN.
//     `publicURL` uses the CDN for non-presigned download URLs only.
//     (YUJ-877 / GH#57 fix)
//
// fileSize is signed into the canonical-headers section as
// `Content-Length`. The browser MUST echo the same value (browsers
// compute it automatically from the request body length); any
// mismatch is rejected by COS as 403 SignatureDoesNotMatch — same
// enforcement model as the MinIO backend, see service_minio.go for
// the rationale.
func (sc *ServiceCOS) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (uploadURL string, downloadURL string, err error) {
	if fileSize <= 0 {
		return "", "", fmt.Errorf("预签名上传必须提供正向的 fileSize（字节数），用于在签名中固定 Content-Length")
	}
	cosConfig := sc.ctx.GetConfig().COS

	// Pick signing client based on BucketURL shape.
	_, _, lookup := sc.publicEndpoint()
	var client *minio.Client
	if lookup == minio.BucketLookupPath {
		// CDN alias: sign against canonical COS endpoint (DNS-style).
		client, err = sc.newCanonicalPresignClient()
	} else {
		// Bucket-subdomain or empty: sign against the public endpoint.
		client, err = sc.newPublicClient()
	}
	if err != nil {
		return "", "", err
	}

	key := sc.withPrefix(objectPath)

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
	presigned, err := client.PresignHeader(ctx, http.MethodPut, cosConfig.Bucket, key, expires, nil, headers)
	if err != nil {
		return "", "", fmt.Errorf("生成预签名URL失败: %w", err)
	}

	// No post-sign URL mutation — the public client above already signed
	// against `BucketURL`'s host, so what the SDK returns is exactly what
	// the browser must hit for the signature to validate.
	uploadURL = presigned.String()

	downloadURL, dlErr := sc.DownloadURL(objectPath, "")
	if dlErr != nil {
		sc.Warn("生成下载URL失败", zap.Error(dlErr))
	}
	return uploadURL, downloadURL, nil
}

// extractFilenameFromDisposition 从 Content-Disposition 头中提取文件名。
// 优先解析 RFC 5987 的 filename*=UTF-8”xxx 格式，其次解析 filename="xxx" 格式。
func extractFilenameFromDisposition(cd string) string {
	if cd == "" {
		return ""
	}

	// 优先匹配 filename*=UTF-8''xxx
	if idx := strings.Index(cd, "filename*=UTF-8''"); idx >= 0 {
		val := cd[idx+len("filename*=UTF-8''"):]
		// 截取到分号或末尾
		if semi := strings.Index(val, ";"); semi >= 0 {
			val = val[:semi]
		}
		val = strings.TrimSpace(val)
		if decoded, err := url.PathUnescape(val); err == nil && decoded != "" {
			return decoded
		}
	}

	// 回退：匹配 filename="xxx"
	if idx := strings.Index(cd, "filename=\""); idx >= 0 {
		val := cd[idx+len("filename=\""):]
		if end := strings.Index(val, "\""); end >= 0 {
			return val[:end]
		}
	}

	return ""
}

// DownloadURL builds a browser-facing object URL that respects the
// addressing style chosen by `publicEndpoint`. The result MUST land on
// the same host (and path shape) as the presigned GET URL emitted by
// `PresignedGetURL`, otherwise an upload-then-download flow returns
// 404 even when the PUT succeeded.
//
// Hotfix history: this function previously concatenated `BucketURL`
// with the object key directly, which silently dropped the bucket
// segment for path-style CDN deployments (BucketURL=`https://cdn.example.com`):
//
//   - PresignedPutURL → `https://cdn.example.com/<bucket>/<prefix>/<key>` ✅
//     (signed by `newPublicClient` with `BucketLookupPath`)
//   - DownloadURL     → `https://cdn.example.com/<prefix>/<key>`           ❌
//     (missing bucket segment → next browser GET = 404)
//
// `PresignedPutURL` calls `DownloadURL` to populate the
// `downloadUrl` field returned by `/v1/file/upload-credentials`, so
// the mismatch shipped to every browser client. This is the YUJ-848
// follow-up to the YUJ-846 path-style fix in PR#56 — the sibling
// `PresignedPutURL` / `PresignedGetURL` paths got `BucketLookupPath`
// in PR#56, and this function now matches.
func (sc *ServiceCOS) DownloadURL(ph string, filename string) (string, error) {
	return sc.publicURL(ph), nil
}

// publicURL constructs a browser-facing object URL for `objectPath`.
//
// Branches:
//
//  1. BucketURL empty — fall back to the SDK canonical endpoint
//     `https://<bucket>.cos.<region>.myqcloud.com/<key>`. This mirrors
//     `publicEndpoint` returning `BucketLookupDNS` against the default
//     host; the bucket lives in the subdomain so we just append the key.
//
//  2. BucketURL is bucket-subdomain shape (host begins with `<bucket>.`) —
//     `publicEndpoint` returns `BucketLookupDNS`. The bucket is already
//     in the host, so `<BucketURL>/<key>` is the correct browser URL.
//
//  3. BucketURL is a CDN / accelerator alias (no `<bucket>.` subdomain,
//     e.g. `https://cdn.deepminer.com.cn`) — the CDN origin routes to
//     the bucket implicitly. The browser URL is `<BucketURL>/<key>`
//     *without* a bucket segment. Inserting the bucket here was the
//     YUJ-877 (GH#57) 404 regression: the CDN has no route for
//     `/<bucket>/…` in the URL path.
//
// In all three branches `withPrefix` is applied so the env-prefix
// routing keeps working (multi-env shared bucket layout).
func (sc *ServiceCOS) publicURL(objectPath string) string {
	cosConfig := sc.ctx.GetConfig().COS
	key := sc.withPrefix(objectPath)

	base := strings.TrimRight(strings.TrimSpace(cosConfig.BucketURL), "/")
	if base == "" {
		// BucketURL empty: canonical bucket-as-subdomain shape.
		base = fmt.Sprintf("https://%s.cos.%s.myqcloud.com", cosConfig.Bucket, cosConfig.Region)
		result, _ := url.JoinPath(base, key)
		return result
	}

	// Both DNS-style (bucket-subdomain) and CDN-alias (no bucket
	// subdomain) produce `<BucketURL>/<key>` — the bucket is either
	// already embedded in the host or handled by the CDN origin rule.
	// Neither case needs a `/<bucket>/` segment in the URL path.
	result, _ := url.JoinPath(base, key)
	return result
}

// PresignedGetURL 生成预签名 GET URL，带 response-content-disposition 用于下载。
//
// Client selection follows the same logic as PresignedPutURL:
// bucket-subdomain / empty BucketURL → sign via `newPublicClient`;
// CDN alias BucketURL → sign via `getClient` (canonical COS endpoint)
// because the CDN domain has no route for path-style `/<bucket>/<key>`.
// (YUJ-877 / GH#57 fix — see PresignedPutURL for the full rationale.)
func (sc *ServiceCOS) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	cosConfig := sc.ctx.GetConfig().COS

	// Pick signing client based on BucketURL shape.
	_, _, lookup := sc.publicEndpoint()
	var client *minio.Client
	var err error
	if lookup == minio.BucketLookupPath {
		// CDN alias: sign against canonical COS endpoint (DNS-style).
		client, err = sc.newCanonicalPresignClient()
	} else {
		// Bucket-subdomain or empty: sign against the public endpoint.
		client, err = sc.newPublicClient()
	}
	if err != nil {
		return "", err
	}

	key := sc.withPrefix(objectPath)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if disposition != "inline" {
		disposition = "attachment"
	}
	encodedFilename := "UTF-8''" + rfc5987Encode(filename)
	params := url.Values{}
	params.Set("response-content-disposition", fmt.Sprintf("%s; filename*=%s", disposition, encodedFilename))

	presigned, err := client.PresignHeader(ctx, http.MethodGet, cosConfig.Bucket, key, expires, params, nil)
	if err != nil {
		return "", fmt.Errorf("生成预签名GET URL失败: %w", err)
	}

	// No post-sign URL mutation — see PresignedPutURL for the rationale.
	return presigned.String(), nil
}
