package file

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"go.uber.org/zap"
)

type ServiceOSS struct {
	log.Log
	ctx *config.Context
}

// NewServiceOSS NewServiceOSS
func NewServiceOSS(ctx *config.Context) *ServiceOSS {

	return &ServiceOSS{
		Log: log.NewTLog("ServiceOSS"),
		ctx: ctx,
	}
}

// newClient builds an OSS client. The Aliyun SDK derives the region from the
// configured Endpoint string (e.g. `oss-cn-hangzhou.aliyuncs.com`), so we do
// not have to set a separate Region option on the client — we just pass the
// SDK its own safe default by leaving the option list empty.
func (s *ServiceOSS) newClient() (*oss.Client, error) {
	ossCfg := s.ctx.GetConfig().OSS
	return oss.New(ossCfg.Endpoint, ossCfg.AccessKeyID, ossCfg.AccessKeySecret)
}

// UploadFile 上传文件
func (s *ServiceOSS) UploadFile(filePath string, contentType string, contentDisposition string, copyFileWriter func(io.Writer) error) (map[string]interface{}, error) {
	client, err := s.newClient()
	if err != nil {
		return nil, err
	}
	bucketName := s.ctx.GetConfig().OSS.BucketName

	bucket, err := client.Bucket(bucketName)
	if err != nil {
		return nil, err
	}
	if bucket == nil {
		err = client.CreateBucket(bucketName, oss.ACL(oss.ACLPublicRead))
		if err != nil {
			return nil, err
		}
		bucket, err = client.Bucket(bucketName)
		if err != nil {
			return nil, err
		}
	}
	buff := bytes.NewBuffer(make([]byte, 0))
	err = copyFileWriter(buff)
	if err != nil {
		s.Error("复制文件内容失败！", zap.Error(err))
		return nil, err
	}
	putOptions := []oss.Option{oss.ContentType(contentType), oss.ContentLength(int64(len(buff.Bytes())))}
	if contentDisposition != "" {
		putOptions = append(putOptions, oss.ContentDisposition(contentDisposition))
	}
	// Use the shared key normalizer so server-side and presigned uploads land
	// at the SAME object key for the same logical input — see
	// `normalizeOSSObjectKey` for the rationale (bucket-name-equals-prefix
	// asymmetry, PR#50 R5 codex finding 2.4).
	objectKey := s.normalizeOSSObjectKey(filePath)
	err = bucket.PutObject(objectKey, buff, putOptions...)
	if err != nil {
		return nil, err
	}

	return map[string]interface{}{}, nil
}

func (s *ServiceOSS) GetFile(path string) (io.ReadCloser, string, error) {
	return nil, "", fmt.Errorf("GetFile not supported for OSS, use DownloadURL instead")
}

// DownloadURL returns the public anonymous-GET URL for an object stored in
// the configured OSS bucket. The input `path` is the same shape that the
// file API at `modules/file/api.go` produces — `<fileType>/<...>` (e.g.
// `chat/2025/x.png`) — possibly with a leading slash.
//
// The path is routed through `normalizeOSSObjectKey` before being joined
// with `BucketURL` so that the resulting URL points at the same OSS object
// the upload paths actually wrote: `UploadFile` and `PresignedPutURL` both
// strip a leading `<BucketName>/` segment to avoid double-bucketing the
// stored key. Without that normalization step here, the asymmetric case
// where the deployer's bucket name happens to equal a `fileType` prefix
// (e.g. `OSS.BucketName == "chat"`, `path == "chat/2025/x.png"` → object
// stored as `2025/x.png`) would emit a URL like
// `<BucketURL>/chat/2025/x.png` and 404. PR#50 R5 codex finding 2.4 closed
// this asymmetry on the upload side; lml2468 surfaced the surviving
// download-side mismatch in PR#50 R6.
//
// The `filename` argument is preserved for API symmetry with other
// backends but is not used for OSS V1 download URLs (operators wanting
// per-request filename overrides should call `PresignedGetURL` instead,
// which embeds `response-content-disposition` in the signed URL).
func (s *ServiceOSS) DownloadURL(path string, filename string) (string, error) {
	ossCfg := s.ctx.GetConfig().OSS

	key := s.normalizeOSSObjectKey(path)
	rpath, _ := url.JoinPath(ossCfg.BucketURL, key)
	return rpath, nil
}

// normalizeOSSObjectKey turns an `objectPath` from the file API into the
// canonical OSS object key. OSS only takes the object key in SignURL /
// PutObject — passing a path that starts with the bucket would sign /
// store under `/<bucket>/<bucket>/<key>` and 404 at the gateway.
//
// `UploadFile` (server-side) and `PresignedPutURL` / `PresignedGetURL`
// (browser-direct) both call this helper so the two upload paths land
// at the SAME key for the same logical input. The previous code had
// `UploadFile` use `filePath` raw while presigned URLs stripped a
// leading `<BucketName>/` prefix; when a deployer's bucket name happened
// to match a `fileType` prefix produced by `modules/file/api.go`
// (e.g. bucket=`chat`), the two paths landed on different keys for the
// same input — fixed in PR#50 R5 codex finding 2.4.
//
// Normalization rules:
//   - strip a single leading `/` (file API may emit either form)
//   - strip a leading `<BucketName>/` segment when present
//
// The pure form (`ossNormalizeObjectKey` in helpers.go) is exposed for
// unit testing without a config context.
func (s *ServiceOSS) normalizeOSSObjectKey(objectPath string) string {
	return ossNormalizeObjectKey(s.ctx.GetConfig().OSS.BucketName, objectPath)
}

// PresignedPutURL signs an OSS PUT URL the browser can use directly.
// Aliyun OSS does NOT accept a separate Content-Disposition signature on PUT
// the way S3 does — disposition has to be embedded as object metadata at
// upload time. We therefore include it in the signed headers so the client
// echoes the same value, and the OSS gateway records it on the resulting
// object.
//
// Caveat — Content-Length on OSS V1: the OSS V1 canonical-string algorithm
// does NOT cover Content-Length. Although we pass `oss.ContentLength` into
// SignURL the resulting signature is computed over Date / Content-Type /
// Canonicalized OSSHeaders / Resource — the size is advisory only and
// the OSS gateway accepts a PUT of any byte length under the signed URL.
// This is the OSS-side deviation from the SigV4 backends (MinIO/COS),
// where Content-Length IS folded into `X-Amz-SignedHeaders` and a wrong
// or missing value at PUT time returns 403 SignatureDoesNotMatch. To
// enforce a hard size budget on OSS, operators must rely on bucket /
// account-level policies (e.g. lifecycle quotas, RAM/STS policies that
// cap object size) — this server's signed URL alone cannot. We still
// require a positive `fileSize` so the request is well-formed and the
// API contract (`maxFileSize` echo) stays consistent across backends.
//
// Caveat — Content-Disposition on OSS V1: same root cause. The OSS V1
// canonical-string algorithm also does NOT include Content-Disposition,
// so a deviating value at PUT time does NOT produce SignatureDoesNotMatch
// the way it does on MinIO/COS. The browser-supplied Content-Disposition
// (or absence of one) is silently persisted. Operators who need strict
// disposition or size enforcement should migrate to a SigV4 backend
// (MinIO/COS), or run a post-upload validator. See `getUploadCredentials`
// docstring for the full per-header deviation matrix.
//
// Roadmap — OSS V4 signing covers Content-Length canonically; switching
// the SDK call to `oss.WithSignVersion(oss.SignVersionV4)` would close
// this gap. Tracked separately so this PR can ship the customer-facing
// SigV4 fix without dragging the OSS SDK uplift along.
func (s *ServiceOSS) PresignedPutURL(objectPath string, contentType string, contentDisposition string, fileSize int64, expires time.Duration) (uploadURL string, downloadURL string, err error) {
	if fileSize <= 0 {
		return "", "", fmt.Errorf("预签名上传必须提供正向的 fileSize（字节数），用于在签名中固定 Content-Length")
	}
	client, err := s.newClient()
	if err != nil {
		return "", "", err
	}
	ossCfg := s.ctx.GetConfig().OSS
	bucket, err := client.Bucket(ossCfg.BucketName)
	if err != nil {
		return "", "", err
	}

	key := s.normalizeOSSObjectKey(objectPath)
	if key == "" {
		return "", "", fmt.Errorf("空对象路径，无法生成预签名URL")
	}

	opts := []oss.Option{oss.ContentLength(fileSize)}
	if contentType != "" {
		opts = append(opts, oss.ContentType(contentType))
	}
	if contentDisposition != "" {
		opts = append(opts, oss.ContentDisposition(contentDisposition))
	}

	signed, err := bucket.SignURL(key, oss.HTTPPut, int64(expires.Seconds()), opts...)
	if err != nil {
		return "", "", fmt.Errorf("生成预签名URL失败: %w", err)
	}

	dl, dlErr := s.DownloadURL(objectPath, "")
	if dlErr != nil {
		s.Warn("生成下载URL失败", zap.Error(dlErr))
	}
	return signed, dl, nil
}

// PresignedGetURL signs an OSS GET URL with a `response-content-disposition`
// override so the browser saves the file under the user-facing filename.
func (s *ServiceOSS) PresignedGetURL(objectPath string, filename string, disposition string, expires time.Duration) (string, error) {
	client, err := s.newClient()
	if err != nil {
		return "", err
	}
	ossCfg := s.ctx.GetConfig().OSS
	bucket, err := client.Bucket(ossCfg.BucketName)
	if err != nil {
		return "", err
	}

	key := s.normalizeOSSObjectKey(objectPath)
	if key == "" {
		return "", fmt.Errorf("空对象路径，无法生成预签名URL")
	}

	if disposition != "inline" {
		disposition = "attachment"
	}

	opts := []oss.Option{}
	if filename != "" {
		encoded := "UTF-8''" + rfc5987Encode(filename)
		opts = append(opts, oss.ResponseContentDisposition(fmt.Sprintf("%s; filename*=%s", disposition, encoded)))
	} else {
		opts = append(opts, oss.ResponseContentDisposition(disposition))
	}

	signed, err := bucket.SignURL(key, oss.HTTPGet, int64(expires.Seconds()), opts...)
	if err != nil {
		return "", fmt.Errorf("生成预签名GET URL失败: %w", err)
	}
	return signed, nil
}
