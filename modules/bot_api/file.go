package bot_api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/file"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	pkgutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/gin-gonic/gin"
	sts "github.com/tencentyun/qcloud-cos-sts-sdk/go"
	"go.uber.org/zap"
)

// botProxyFile handles GET /v1/botfile/*path — 302 redirect to presigned URL.
func (ba *BotAPI) botProxyFile(c *wkhttp.Context) {
	ph := c.Param("path")
	if ph == "" {
		respondBotAPIRequestInvalid(c, "path")
		return
	}
	ph = strings.TrimPrefix(ph, "/")
	ph = strings.TrimPrefix(ph, "file/")

	cleaned := filepath.Clean(ph)
	if strings.Contains(cleaned, "..") || strings.ContainsAny(cleaned, "\x00") {
		respondBotAPIRequestInvalid(c, "path")
		return
	}
	ph = cleaned

	filename := c.Query("filename")
	if filename == "" {
		filename = pkgutil.ExtractFilenameFromPath(ph)
	}

	downloadURL, err := ba.fileService.DownloadURL(ph, filename)
	if err != nil {
		ba.Error("获取文件下载URL失败", zap.Error(err), zap.String("path", ph))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		return
	}
	c.Redirect(http.StatusFound, downloadURL)
}

// botUploadFile handles POST /v1/bot/file/upload.
func (ba *BotAPI) botUploadFile(c *wkhttp.Context) {
	fileType := c.DefaultQuery("type", "chat")
	uploadPath := c.Query("path")

	multipartFile, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		respondBotAPIRequestInvalid(c, "file")
		return
	}
	defer multipartFile.Close()

	const maxSize int64 = 100 * 1024 * 1024
	if fileHeader.Size > maxSize {
		respondBotAPIFileTooLarge(c, maxSize/1024/1024)
		return
	}

	fileName := fileHeader.Filename
	filePath := uploadPath
	if filePath == "" {
		filePath = fmt.Sprintf("/%d/%s%s", time.Now().Unix(), util.GenerUUID(), filepath.Ext(fileName))
	}
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}

	storagePath := fmt.Sprintf("%s%s", fileType, filePath)
	contentType := mime.TypeByExtension(filepath.Ext(fileName))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	contentDisposition := file.BuildContentDisposition(fileName)
	_, err = ba.fileService.UploadFile(storagePath, contentType, contentDisposition, func(w io.Writer) error {
		_, err := io.Copy(w, multipartFile)
		return err
	})
	if err != nil {
		ba.Error("上传文件失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIUploadFailed, nil, nil)
		return
	}

	fullURL, err := ba.fileService.DownloadURL(storagePath, "")
	if err != nil {
		ba.Warn("生成下载URL失败，回退到相对路径", zap.Error(err))
		fullURL = fmt.Sprintf("file/preview/%s%s", fileType, filePath)
	}
	c.Response(gin.H{
		"url":  fullURL,
		"name": fileName,
		"size": fileHeader.Size,
	})
}

// botFileDownload handles GET /v1/bot/file/download/*path.
func (ba *BotAPI) botFileDownload(c *wkhttp.Context) {
	ph := c.Param("path")
	if ph == "" {
		respondBotAPIRequestInvalid(c, "path")
		return
	}
	ph = strings.TrimPrefix(ph, "/")

	ph, err := sanitizeBotFilePath(ph)
	if err != nil {
		respondBotAPIRequestInvalid(c, "path")
		return
	}

	filename := c.Query("filename")
	if filename == "" {
		filename = pkgutil.ExtractFilenameFromPath(ph)
	}

	downloadURL, err := ba.fileService.DownloadURL(ph, filename)
	if err != nil {
		ba.Error("获取文件下载URL失败", zap.Error(err), zap.String("path", ph))
		httperr.ResponseErrorLWithStatus(c, errcode.ErrSharedNotFound, nil, nil)
		return
	}
	c.Redirect(http.StatusFound, downloadURL)
}

// sanitizeBotFilePath normalizes file path and prevents traversal attacks.
func sanitizeBotFilePath(p string) (string, error) {
	decoded := p
	for i := 0; i < 3; i++ {
		next, err := url.QueryUnescape(decoded)
		if err != nil {
			return "", errors.New("路径包含无效字符")
		}
		if next == decoded {
			break
		}
		decoded = next
	}
	cleaned := filepath.Clean(decoded)
	if strings.Contains(cleaned, "..") {
		return "", errors.New("路径不允许包含目录遍历字符")
	}
	return cleaned, nil
}

// botUploadCredentials handles GET /v1/bot/upload/credentials — STS temp key.
func (ba *BotAPI) botUploadCredentials(c *wkhttp.Context) {
	filename := c.Query("filename")
	if strings.TrimSpace(filename) == "" {
		respondBotAPIRequestInvalid(c, "filename")
		return
	}
	filename = filepath.Base(filename)

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || file.IsBlockedExtension(ext) || !file.IsAllowedExtension(ext) {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIFileTypeUnsupported, nil, nil)
		return
	}

	cosConfig := ba.ctx.GetConfig().COS
	if cosConfig.SecretID == "" || cosConfig.SecretKey == "" || cosConfig.Bucket == "" {
		ba.Error("COS 配置不完整")
		httperr.ResponseErrorL(c, errcode.ErrBotAPIUploadFailed, nil, nil)
		return
	}

	prefix := strings.TrimSpace(cosConfig.Prefix)
	fnExt := strings.ToLower(filepath.Ext(filename))
	objectPath := fmt.Sprintf("chat/%d/%s/%s%s", time.Now().Unix(), util.GenerUUID(), util.GenerUUID(), fnExt)
	var key string
	if prefix != "" {
		key = path.Join(prefix, objectPath)
	} else {
		key = objectPath
	}

	bucket := cosConfig.Bucket
	region := cosConfig.Region

	appId := ""
	if idx := strings.LastIndex(bucket, "-"); idx > 0 {
		appId = bucket[idx+1:]
	}
	if appId == "" {
		ba.Error("无法从 bucket 名称中提取 appId", zap.String("bucket", bucket))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIUploadFailed, nil, nil)
		return
	}

	client := sts.NewClient(cosConfig.SecretID, cosConfig.SecretKey, nil)
	opt := &sts.CredentialOptions{
		DurationSeconds: 1800,
		Region:          region,
		Policy: &sts.CredentialPolicy{
			Statement: []sts.CredentialPolicyStatement{
				{
					Action:   []string{"cos:PutObject"},
					Effect:   "allow",
					Resource: []string{fmt.Sprintf("qcs::cos:%s:uid/%s:%s/%s", region, appId, bucket, key)},
				},
			},
		},
	}

	res, err := client.GetCredential(opt)
	if err != nil {
		ba.Error("获取 STS 临时密钥失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIUploadFailed, nil, nil)
		return
	}

	c.Response(gin.H{
		"bucket": bucket,
		"region": region,
		"key":    key,
		"credentials": gin.H{
			"tmpSecretId":  res.Credentials.TmpSecretID,
			"tmpSecretKey": res.Credentials.TmpSecretKey,
			"sessionToken": res.Credentials.SessionToken,
		},
		"startTime":   res.StartTime,
		"expiredTime": res.ExpiredTime,
		"cdnBaseUrl":  cosConfig.BucketURL,
	})
}

// botUploadPresigned handles GET /v1/bot/upload/presigned — presigned PUT URL.
func (ba *BotAPI) botUploadPresigned(c *wkhttp.Context) {
	filename := c.Query("filename")
	if strings.TrimSpace(filename) == "" {
		respondBotAPIRequestInvalid(c, "filename")
		return
	}
	filename = filepath.Base(filename)

	// fileSize is REQUIRED so the storage layer can sign Content-Length and
	// reject any PUT that exceeds the byte budget — same P0 size-bypass
	// guard the public file API enforces (see modules/file/api.go).
	fileSizeRaw := strings.TrimSpace(c.Query("fileSize"))
	if fileSizeRaw == "" {
		respondBotAPIRequestInvalid(c, "fileSize")
		return
	}
	fileSize, parseErr := strconv.ParseInt(fileSizeRaw, 10, 64)
	if parseErr != nil || fileSize <= 0 {
		respondBotAPIRequestInvalid(c, "fileSize")
		return
	}
	if fileSize > file.MaxFileSize {
		ba.Warn("预签名上传 fileSize 超出限制",
			zap.Int64("size", fileSize), zap.Int64("max", file.MaxFileSize))
		respondBotAPIFileTooLarge(c, file.MaxFileSize/1024/1024)
		return
	}

	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || file.IsBlockedExtension(ext) || !file.IsAllowedExtension(ext) {
		httperr.ResponseErrorL(c, errcode.ErrBotAPIFileTypeUnsupported, nil, nil)
		return
	}

	objectPath := fmt.Sprintf("chat/%d/%s/%s%s", time.Now().Unix(), util.GenerUUID(), util.GenerUUID(), ext)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	contentDisposition := file.BuildContentDisposition(filename)
	expiry := 30 * time.Minute
	uploadURL, downloadURL, err := ba.fileService.PresignedPutURL(objectPath, contentType, contentDisposition, fileSize, expiry)
	if err != nil {
		ba.Error("生成预签名上传URL失败", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrBotAPIUploadFailed, nil, nil)
		return
	}

	resp := gin.H{
		"method":      "PUT",
		"uploadUrl":   uploadURL,
		"downloadUrl": downloadURL,
		"contentType": contentType,
		"key":         objectPath,
		"expiresIn":   int(expiry.Seconds()),
		"expiredTime": time.Now().Add(expiry).Unix(),
		"maxFileSize": fileSize,
	}
	// Content-Disposition is signed into the canonical headers on
	// SigV4 backends (MinIO/COS), so the browser MUST echo this exact
	// value at PUT time or the gateway returns 403 SignatureDoesNotMatch.
	// Mirror the main file endpoint at modules/file/api.go.
	if contentDisposition != "" {
		resp["contentDisposition"] = contentDisposition
	}
	c.Response(resp)
}
