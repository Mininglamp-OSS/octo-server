package file

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/cliv2"
	sts "github.com/tencentyun/qcloud-cos-sts-sdk/go"
)

// registerCLIV2Routes contributes the CLI v2 file upload operations.
func (f *File) registerCLIV2Routes(source *wkhttp.WKHttp) {
	cliv2.Add(source, cliv2.Route{
		Method:      http.MethodPost,
		Path:        "/v2/cli/files",
		Action:      cliv2.ActionFilesWrite,
		User:        CLIV2UploadHandler(f.ctx, f.service),
		Middlewares: []wkhttp.HandlerFunc{rejectCLIV2CallerObjectKey()},
	})
	cliv2.Add(source, cliv2.Route{
		Method:      http.MethodGet,
		Path:        "/v2/cli/uploads/credentials",
		Action:      cliv2.ActionFilesWrite,
		User:        CLIV2CredentialsHandler(f.ctx),
		Middlewares: []wkhttp.HandlerFunc{rejectCLIV2CallerObjectKey()},
	})
	cliv2.Add(source, cliv2.Route{
		Method:      http.MethodGet,
		Path:        "/v2/cli/uploads/_presigned",
		Action:      cliv2.ActionFilesWrite,
		User:        CLIV2PresignedUploadHandler(f.ctx, f.service),
		Middlewares: []wkhttp.HandlerFunc{rejectCLIV2CallerObjectKey()},
	})
}

// CLIV2UploadHandler creates a fresh chat object key and enforces the multipart
// upload limit before storing bytes. It intentionally does not accept a caller
// path because the file subsystem has no object ownership index to authorize
// overwrites against.
func CLIV2UploadHandler(ctx *config.Context, service IService) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if rejectCLIV2ObjectKey(c) {
			return
		}
		if fileType := strings.TrimSpace(c.Query("type")); fileType != "" && fileType != string(TypeChat) {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Only chat file uploads are supported.")
			return
		}

		maxUpload := MaxUploadSize()
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUpload+1024*1024)
		multipartFile, header, err := c.Request.FormFile("file")
		if err != nil {
			if _, tooLarge := err.(*http.MaxBytesError); tooLarge {
				cliv2.Fail(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Uploaded file is too large.")
			} else {
				cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "A file is required.")
			}
			return
		}
		defer multipartFile.Close()
		if header.Size <= 0 {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Uploaded file must not be empty.")
			return
		}
		if header.Size > maxUpload {
			cliv2.Fail(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Uploaded file is too large.")
			return
		}

		filename := sanitizeFilename(filepath.Base(header.Filename))
		ext := strings.ToLower(filepath.Ext(filename))
		if ext == "" || IsBlockedExtension(ext) || !IsAllowedExtension(ext) {
			cliv2.Fail(c, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "This file type is not supported.")
			return
		}
		contentType := mime.TypeByExtension(ext)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		key := fmt.Sprintf("chat/%d/%s/%s%s", time.Now().Unix(), util.GenerUUID(), util.GenerUUID(), ext)
		_, err = service.UploadFile(key, contentType, BuildContentDisposition(filename), func(dst io.Writer) error {
			_, copyErr := io.Copy(dst, multipartFile)
			return copyErr
		})
		if err != nil {
			cliv2.Fail(c, http.StatusInternalServerError, "INTERNAL_ERROR", "File upload failed.")
			return
		}
		downloadURL, err := service.DownloadURL(key, "")
		if err != nil {
			downloadURL = "file/preview/" + key
		}
		cliv2.Respond(c, map[string]any{
			"url":  downloadURL,
			"name": filename,
			"size": header.Size,
		})
	}
}

// CLIV2PresignedUploadHandler signs an exact object key and byte length. OSS
// V1 signing does not enforce Content-Length, and Qiniu/SeaweedFS have no
// compatible presigned-PUT primitive; those backends fail closed.
func CLIV2PresignedUploadHandler(ctx *config.Context, service IService) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if rejectCLIV2ObjectKey(c) {
			return
		}
		filename := strings.TrimSpace(c.Query("filename"))
		if filename == "" {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "A filename is required.")
			return
		}
		filename = sanitizeFilename(filepath.Base(filename))
		ext := strings.ToLower(filepath.Ext(filename))
		if ext == "" || IsBlockedExtension(ext) || !IsAllowedExtension(ext) {
			cliv2.Fail(c, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "This file type is not supported.")
			return
		}
		fileSize, err := strconv.ParseInt(strings.TrimSpace(c.Query("file_size")), 10, 64)
		if err != nil || fileSize <= 0 {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "A positive file_size is required.")
			return
		}
		if fileSize > MaxUploadSize() {
			cliv2.Fail(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Uploaded file is too large.")
			return
		}
		if ctx == nil || ctx.GetConfig() == nil || !supportsBoundedCLIV2Presign(ctx.GetConfig().FileService) {
			cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Bounded direct uploads are unavailable for this storage backend.")
			return
		}

		contentType := mime.TypeByExtension(ext)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		key := fmt.Sprintf("chat/%d/%s/%s%s", time.Now().Unix(), util.GenerUUID(), util.GenerUUID(), ext)
		contentDisposition := BuildContentDisposition(filename)
		expiry := 30 * time.Minute
		uploadURL, downloadURL, err := service.PresignedPutURL(key, contentType, contentDisposition, fileSize, expiry)
		if err != nil {
			cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Bounded direct uploads are unavailable.")
			return
		}
		data := map[string]any{
			"method":        http.MethodPut,
			"upload_url":    uploadURL,
			"download_url":  downloadURL,
			"content_type":  contentType,
			"key":           key,
			"expires_in":    int(expiry.Seconds()),
			"expired_at":    time.Now().Add(expiry).Unix(),
			"max_file_size": fileSize,
		}
		if contentDisposition != "" {
			data["content_disposition"] = contentDisposition
		}
		cliv2.Respond(c, data)
	}
}

// CLIV2CredentialsHandler issues a COS credential scoped to one server-generated
// object key, matching the legacy Bot upload policy without accepting a caller
// key or requiring a byte-size parameter.
func CLIV2CredentialsHandler(ctx *config.Context) wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if rejectCLIV2ObjectKey(c) {
			return
		}
		filename := c.Query("filename")
		if strings.TrimSpace(filename) == "" {
			cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "A filename is required.")
			return
		}
		filename = filepath.Base(filename)
		ext := strings.ToLower(filepath.Ext(filename))
		if ext == "" || IsBlockedExtension(ext) || !IsAllowedExtension(ext) {
			cliv2.Fail(c, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "This file type is not supported.")
			return
		}
		if ctx == nil || ctx.GetConfig() == nil {
			cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Temporary upload credentials are unavailable.")
			return
		}

		cosConfig := ctx.GetConfig().COS
		if cosConfig.SecretID == "" || cosConfig.SecretKey == "" || cosConfig.Bucket == "" {
			cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Temporary upload credentials are unavailable.")
			return
		}

		objectPath := fmt.Sprintf("chat/%d/%s/%s%s", time.Now().Unix(), util.GenerUUID(), util.GenerUUID(), ext)
		key := objectPath
		if prefix := strings.TrimSpace(cosConfig.Prefix); prefix != "" {
			key = path.Join(prefix, objectPath)
		}
		appID := ""
		if index := strings.LastIndex(cosConfig.Bucket, "-"); index > 0 {
			appID = cosConfig.Bucket[index+1:]
		}
		if appID == "" {
			cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Temporary upload credentials are unavailable.")
			return
		}

		client := sts.NewClient(cosConfig.SecretID, cosConfig.SecretKey, nil)
		result, err := client.GetCredential(cliv2COSCredentialOptions(cosConfig.Region, appID, cosConfig.Bucket, key))
		if err != nil {
			cliv2.Fail(c, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", "Temporary upload credentials are unavailable.")
			return
		}

		cliv2.Respond(c, map[string]any{
			"bucket": cosConfig.Bucket,
			"region": cosConfig.Region,
			"key":    key,
			"credentials": map[string]string{
				"temporary_secret_id":  result.Credentials.TmpSecretID,
				"temporary_secret_key": result.Credentials.TmpSecretKey,
				"session_token":        result.Credentials.SessionToken,
			},
			"start_at":     result.StartTime,
			"expires_at":   result.ExpiredTime,
			"cdn_base_url": cosConfig.BucketURL,
		})
	}
}

func cliv2COSCredentialOptions(region, appID, bucket, key string) *sts.CredentialOptions {
	return &sts.CredentialOptions{
		DurationSeconds: 1800,
		Region:          region,
		Policy: &sts.CredentialPolicy{
			Statement: []sts.CredentialPolicyStatement{{
				Action:   []string{"cos:PutObject"},
				Effect:   "allow",
				Resource: []string{fmt.Sprintf("qcs::cos:%s:uid/%s:%s/%s", region, appID, bucket, key)},
			}},
		},
	}
}

func rejectCLIV2CallerObjectKey() wkhttp.HandlerFunc {
	return func(c *wkhttp.Context) {
		if rejectCLIV2ObjectKey(c) {
			c.Abort()
			return
		}
		c.Next()
	}
}

func rejectCLIV2ObjectKey(c *wkhttp.Context) bool {
	if c.Query(uploadPathField) == "" {
		return false
	}
	cliv2.Fail(c, http.StatusBadRequest, "VALIDATION_ERROR", "Caller-provided object keys are not supported.")
	return true
}

func supportsBoundedCLIV2Presign(backend config.FileService) bool {
	return backend == config.FileServiceMinio || backend == config.FileServiceTencentCOS || backend == fileServiceAwsS3
}
