package file

import (
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// File 文件操作
type File struct {
	ctx *config.Context
	log.Log
	service IService
}

// New New
func New(ctx *config.Context) *File {
	return &File{
		ctx:     ctx,
		Log:     log.NewTLog("File"),
		service: NewService(ctx),
	}
}

// Route 路由
func (f *File) Route(r *wkhttp.WKHttp) {
	auth := r.Group("/v1/file", f.ctx.AuthMiddleware(r))
	{
		// 获取文件（需认证，防止未授权访问用户文件）
		auth.GET("/preview/*path", f.getFile)
		//获取上传文件地址
		auth.GET("/upload", f.getFilePath)
		//上传文件
		auth.POST("/upload", f.uploadFile)
		// 预签名上传 URL 签发
		auth.GET("/upload/presigned", f.getUploadCredentials)
		auth.GET("/upload/credentials", f.getUploadCredentials) // 兼容旧路径
	}
}

func (f *File) makeImageCompose(c *wkhttp.Context) {
	var imageURLs []string
	if err := c.BindJSON(&imageURLs); err != nil {
		f.Error("数据格式有误！", zap.Error(err))
		c.ResponseError(errors.New("数据格式有误！"))
		return
	}
	if len(imageURLs) <= 0 {
		c.ResponseError(errors.New("图片不能为空！"))
		return
	}
	if len(imageURLs) > 9 {
		c.ResponseError(errors.New("图片数量不能大于9！"))
		return
	}
	uploadPath := c.Param("path")
	// 下载并组合图片
	resultMap, err := f.service.DownloadAndMakeCompose(uploadPath, imageURLs)
	if err != nil {
		f.Error("组合图片失败！", zap.String("uploadPath", uploadPath), zap.Any("imageURLs", imageURLs), zap.Error(err))
		c.ResponseError(errors.New("组合图片失败！"))
		return
	}
	fid, ok := resultMap["fid"].(string)
	if !ok || fid == "" {
		f.Error("图片合成返回结果异常", zap.Any("resultMap", resultMap))
		c.ResponseError(errors.New("图片合成失败：返回结果异常"))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"path": fid,
	})
}

// 获取上传文件地址
func (f *File) getFilePath(c *wkhttp.Context) {
	loginUID := c.GetLoginUID()
	uploadPath := c.Query("path")
	fileType := c.Query("type")
	err := f.checkReq(Type(fileType), uploadPath)
	if err != nil {
		c.ResponseError(err)
		return
	}
	if uploadPath != "" {
		var sanitizeErr error
		uploadPath, sanitizeErr = sanitizePath(uploadPath)
		if sanitizeErr != nil {
			c.ResponseError(errors.New("无效的文件路径"))
			return
		}
	}
	var path string
	if Type(fileType) == TypeMomentCover {
		// 动态封面
		path = fmt.Sprintf("%s/file/upload?type=%s&path=/%s.png", f.ctx.GetConfig().External.APIBaseURL, fileType, loginUID)
	} else if Type(fileType) == TypeSticker {
		// 自定义表情
		path = fmt.Sprintf("%s/file/upload?type=%s&path=/%s/%s.gif", f.ctx.GetConfig().External.APIBaseURL, fileType, loginUID, util.GenerUUID())
	} else if Type(fileType) == TypeWorkplaceBanner {
		// 工作台横幅
		path = fmt.Sprintf("%s/file/upload?type=%s&path=/workplace/banner/%s", f.ctx.GetConfig().External.APIBaseURL, fileType, path)
	} else if Type(fileType) == TypeWorkplaceAppIcon {
		// 工作台appIcon
		path = fmt.Sprintf("%s/file/upload?type=%s&path=/workplace/appicon/%s", f.ctx.GetConfig().External.APIBaseURL, fileType, path)
	} else {
		path = fmt.Sprintf("%s/file/upload?type=%s&path=%s", f.ctx.GetConfig().External.APIBaseURL, fileType, uploadPath)
	}
	c.Response(map[string]string{
		"url": path,
	})
}

// 上传文件
func (f *File) uploadFile(c *wkhttp.Context) {
	uploadPath := c.Query("path")
	fileType := c.Query("type")
	signature := c.Query("signature") // 是否返回签名
	var signatureInt int64 = 0
	if signature != "" {
		signatureInt, _ = strconv.ParseInt(signature, 10, 64)
	}
	contentType := c.DefaultPostForm("contenttype", "application/octet-stream")
	err := f.checkReq(Type(fileType), uploadPath)
	if err != nil {
		c.ResponseError(err)
		return
	}
	if uploadPath != "" {
		var sanitizeErr error
		uploadPath, sanitizeErr = sanitizePath(uploadPath)
		if sanitizeErr != nil {
			c.ResponseError(errors.New("无效的文件路径"))
			return
		}
	}

	// 限制请求体大小，防止大文件 DoS
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxFileSize+1024*1024)

	file, fileHeader, err := c.Request.FormFile("file")
	if err != nil {
		f.Error("读取文件失败！", zap.Error(err))
		c.ResponseError(errors.New("读取文件失败！"))
		return
	}
	defer file.Close()

	// 文件大小检查
	if fileHeader.Size > MaxFileSize {
		f.Warn("文件大小超出限制", zap.Int64("size", fileHeader.Size), zap.Int64("max", MaxFileSize))
		c.ResponseError(fmt.Errorf("文件大小不能超过%dMB", MaxFileSize/1024/1024))
		return
	}

	// 文件扩展名检查
	fileName := sanitizeFilename(fileHeader.Filename)
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == "" {
		f.Warn("上传的文件没有扩展名", zap.String("filename", fileName))
		c.ResponseError(errors.New("文件必须包含扩展名"))
		return
	}
	if IsBlockedExtension(ext) {
		f.Warn("上传了禁止的文件类型", zap.String("filename", fileName), zap.String("ext", ext))
		c.ResponseError(fmt.Errorf("禁止上传%s类型的文件", ext))
		return
	}
	if !IsAllowedExtension(ext) {
		f.Warn("上传了不支持的文件类型", zap.String("filename", fileName), zap.String("ext", ext))
		c.ResponseError(fmt.Errorf("不支持上传%s类型的文件", ext))
		return
	}

	// If contentType is the default octet-stream, try to infer from file extension
	if contentType == "application/octet-stream" {
		if detected := mime.TypeByExtension(ext); detected != "" {
			contentType = detected
		} else if fallback, ok := extMIMEFallback[ext]; ok {
			contentType = fallback
		}
	}
	// Ensure text content types include charset=utf-8
	contentType = ensureTextCharset(contentType)

	// 读取文件头部用于魔数验证（最多读取 16 字节）
	magicHeader := make([]byte, 16)
	n, err := file.Read(magicHeader)
	if err != nil && err.Error() != "EOF" {
		f.Error("读取文件头部失败", zap.Error(err))
		c.ResponseError(errors.New("读取文件失败"))
		return
	}
	magicHeader = magicHeader[:n]

	// 验证文件魔数是否与扩展名匹配
	if !ValidateMagicNumber(ext, magicHeader) {
		f.Warn("文件内容与扩展名不匹配", zap.String("filename", fileName), zap.String("ext", ext))
		c.ResponseError(errors.New("文件内容与扩展名不匹配"))
		return
	}

	// 重置文件指针到开头
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		f.Error("重置文件指针失败", zap.Error(err))
		c.ResponseError(errors.New("文件处理失败"))
		return
	}

	contentType = inferContentType(contentType, ext)

	path := uploadPath
	if !strings.HasPrefix(path, "/") {
		path = fmt.Sprintf("/%s", path)
	}
	// 修复客户端上传路径缺少扩展名的问题
	pathExt := strings.ToLower(filepath.Ext(path))
	if ext != "" && pathExt == "" {
		// 路径完全没有扩展名（如 /HASH），根据文件名追加（→ /HASH.jpg）
		if strings.HasSuffix(strings.ToLower(path), ext[1:]) {
			// 有扩展名文本但缺点号（如 HASHpdf → HASH.pdf）
			path = path[:len(path)-len(ext)+1] + ext
		} else {
			// 完全没有扩展名（如纯HASH），直接追加
			path = path + ext
		}
	}
	var sign []byte
	if signatureInt == 1 {
		h := sha512.New()
		_, err := io.Copy(h, file)
		if err != nil {
			f.Error("签名复制文件错误", zap.Error(err))
			c.ResponseError(errors.New("签名复制文件错误"))
			return
		}
		sign = h.Sum(nil)
	}
	_, err = f.service.UploadFile(fmt.Sprintf("%s%s", fileType, path), contentType, func(w io.Writer) error {
		_, err := file.Seek(0, io.SeekStart)
		if err != nil {
			f.Error("设置文件偏移量错误", zap.Error(err))
			return err
		}
		_, err = io.Copy(w, file)
		return err
	})
	if err != nil {
		f.Error("上传文件失败！", zap.Error(err))
		c.ResponseError(errors.New("上传文件失败！"))
		return
	}

	storagePath := fmt.Sprintf("%s%s", fileType, path)
	fullURL, err := f.service.DownloadURL(storagePath, "")
	if err != nil {
		f.Warn("生成下载URL失败，回退到相对路径", zap.Error(err))
		fullURL = fmt.Sprintf("file/preview/%s%s", fileType, path)
	}
	resp := map[string]interface{}{
		"path": fullURL,
		"name": fileName,
		"size": fileHeader.Size,
		"ext":  ext,
	}
	if signatureInt == 1 {
		encoded := base64.StdEncoding.EncodeToString(sign[:])
		resp["sha512"] = encoded
	}
	c.Response(resp)
}

// textExtFallback covers common text extensions that may not exist in the
// system MIME database (e.g. .md on macOS).
var textExtFallback = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".yml":      "text/yaml",
	".yaml":     "text/yaml",
	".log":      "text/plain",
	".ini":      "text/plain",
	".cfg":      "text/plain",
	".conf":     "text/plain",
}

// inferContentType detects the content type from file extension when the
// client-provided contentType is the default "application/octet-stream",
// and ensures text/* types include charset=utf-8.
func inferContentType(contentType string, ext string) string {
	if contentType == "application/octet-stream" {
		if detected := mime.TypeByExtension(ext); detected != "" {
			contentType = detected
		} else if fallback, ok := textExtFallback[ext]; ok {
			contentType = fallback
		}
	}
	if strings.HasPrefix(contentType, "text/") && !strings.Contains(contentType, "charset") {
		contentType = contentType + "; charset=utf-8"
	}
	return contentType
}

// 获取文件
func (f *File) getFile(c *wkhttp.Context) {
	ph, err := sanitizePath(c.Param("path"))
	if err != nil {
		c.ResponseError(err)
		return
	}
	if ph == "" {
		c.Response(errors.New("访问路径不能为空"))
		return
	}
	filename := c.Query("filename")
	if filename == "" {
		paths := strings.Split(ph, "/")
		if len(paths) > 0 {
			filename = paths[len(paths)-1]
		}
	}
	// 清洗文件名，防止 CRLF 注入和路径穿越
	filename = sanitizeFilename(filename)

	// 设置 Content-Type，未知扩展名默认为 application/octet-stream
	ext := strings.ToLower(filepath.Ext(filename))
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Header("Content-Type", contentType)

	// 对未知扩展名强制 attachment（防止浏览器解析恶意内容）
	disposition := c.Query("disposition")
	if mime.TypeByExtension(ext) == "" {
		disposition = "attachment"
	}
	// 构造安全的 Content-Disposition，使用 RFC 5987 编码处理非 ASCII 文件名
	escapedFilename := url.PathEscape(filename)
	if disposition == "attachment" {
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename*=UTF-8''%s", escapedFilename))
	} else {
		c.Header("Content-Disposition", fmt.Sprintf("inline; filename*=UTF-8''%s", escapedFilename))
	}

	dlFilename := filename
	if disposition != "attachment" {
		dlFilename = "" // inline显示不带content-disposition
	}
	downloadURL, err := f.service.DownloadURL(ph, dlFilename)
	if err != nil {
		c.ResponseError(err)
		return
	}
	c.Redirect(http.StatusFound, downloadURL)
}

// getUploadCredentials 返回预签名 PUT URL，供客户端直接上传文件，无需后端中转
func (f *File) getUploadCredentials(c *wkhttp.Context) {
	fileType := c.Query("type")
	uploadPath := c.Query("path")
	filename := c.Query("filename")
	contentType := c.Query("contentType")

	if err := f.checkReq(Type(fileType), uploadPath); err != nil {
		c.ResponseError(err)
		return
	}

	ext := ""
	if filename != "" {
		ext = strings.ToLower(filepath.Ext(filepath.Base(filename)))
	} else if uploadPath != "" {
		ext = strings.ToLower(filepath.Ext(uploadPath))
	}
	if ext == "" || IsBlockedExtension(ext) || !IsAllowedExtension(ext) {
		c.ResponseError(errors.New("不支持的文件类型"))
		return
	}

	if ext != "" {
		inferred := mime.TypeByExtension(ext)
		if inferred != "" {
			contentType = inferred
		}
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	var objectKey string
	if uploadPath != "" {
		sanitized, err := sanitizePath(uploadPath)
		if err != nil {
			c.ResponseError(errors.New("无效的文件路径"))
			return
		}
		if !strings.HasPrefix(sanitized, "/") {
			sanitized = "/" + sanitized
		}
		objectKey = fileType + sanitized
	} else {
		objectKey = fmt.Sprintf("%s/%s%s", fileType, util.GenerUUID(), ext)
	}

	expiry := 30 * time.Minute
	uploadURL, downloadURL, err := f.service.PresignedPutURL(objectKey, contentType, expiry)
	if err != nil {
		f.Error("生成预签名URL失败", zap.Error(err))
		c.ResponseError(errors.New("生成预签名上传 URL 失败"))
		return
	}

	c.Response(map[string]interface{}{
		"method":      "PUT",
		"uploadUrl":   uploadURL,
		"downloadUrl": downloadURL,
		"contentType": contentType,
		"key":         objectKey,
		"expiresIn":   int(expiry.Seconds()),
		"expiredTime": time.Now().Add(expiry).Unix(),
	})
}

// sanitizePath 规范化上传路径，防止路径遍历攻击（包括双重编码）
func sanitizePath(p string) (string, error) {
	// 循环解码防止双重/多重 URL 编码绕过
	decoded := p
	for i := 0; i < 3; i++ {
		next, err := url.QueryUnescape(decoded)
		if err != nil {
			return "", errors.New("路径包含无效字符")
		}
		if next == decoded {
			break // 没有更多编码层
		}
		decoded = next
	}
	// 过滤空字节及其他控制字符
	for _, r := range decoded {
		if r == 0 || r == 0x7F || r < 0x20 {
			return "", errors.New("path contains invalid control characters")
		}
	}
	// 禁止包含 .. 的路径遍历
	cleaned := filepath.Clean(decoded)
	if strings.Contains(cleaned, "..") {
		return "", errors.New("路径不允许包含目录遍历字符")
	}
	return cleaned, nil
}

// extMIMEFallback covers extensions that may be missing from the OS mime
// database (e.g. .md on macOS).
var extMIMEFallback = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".yaml":     "text/yaml",
	".yml":      "text/yaml",
}

// ensureTextCharset appends "; charset=utf-8" to text/* content types that
// don't already specify a charset. This prevents garbled text when browsers
// render files served from object storage without explicit encoding metadata.
func ensureTextCharset(contentType string) string {
	if strings.HasPrefix(contentType, "text/") && !strings.Contains(strings.ToLower(contentType), "charset") {
		return contentType + "; charset=utf-8"
	}
	return contentType
}

func (f *File) checkReq(fileType Type, path string) error {
	if fileType == "" {
		return errors.New("文件类型不能为空")
	}
	if path == "" && fileType != TypeMomentCover && fileType != TypeSticker {
		return errors.New("上传路径不能为空")
	}
	if path != "" {
		if _, err := sanitizePath(path); err != nil {
			return err
		}
	}
	if fileType != TypeChat && fileType != TypeMoment && fileType != TypeMomentCover && fileType != TypeSticker && fileType != TypeReport && fileType != TypeChatBg && fileType != TypeCommon && fileType != TypeDownload && fileType != TypeWorkplaceBanner && fileType != TypeWorkplaceAppIcon {
		return errors.New("文件类型错误")
	}
	return nil
}
