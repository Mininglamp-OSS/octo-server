package file

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// fileServiceAwsS3 is the cfg.FileService value that activates the
// awsS3 backend. octo-lib has not yet exported a typed
// FileServiceAwsS3 constant — when it does (tracked in PR follow-ups),
// callers should switch to the typed value and this local const can
// go away. Keeping the literal in one place prevents drift between
// the dispatch site (service.go) and the round-trip URL strip
// (api.go).
const fileServiceAwsS3 = "awsS3"

// fileMagicNumbers 文件魔数签名映射表
// 用于验证文件内容是否与扩展名声称的类型一致
var fileMagicNumbers = map[string][][]byte{
	// 图片
	".jpg":  {{0xFF, 0xD8, 0xFF}},
	".jpeg": {{0xFF, 0xD8, 0xFF}},
	".png":  {{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}},
	".gif":  {{0x47, 0x49, 0x46, 0x38, 0x37, 0x61}, {0x47, 0x49, 0x46, 0x38, 0x39, 0x61}}, // GIF87a, GIF89a
	".bmp":  {{0x42, 0x4D}},
	".webp": {{0x52, 0x49, 0x46, 0x46}}, // RIFF header (need to check WEBP at offset 8)
	".ico":  {{0x00, 0x00, 0x01, 0x00}},
	// 文档
	".pdf": {{0x25, 0x50, 0x44, 0x46}}, // %PDF
	".zip": {{0x50, 0x4B, 0x03, 0x04}, {0x50, 0x4B, 0x05, 0x06}, {0x50, 0x4B, 0x07, 0x08}},
	".rar": {{0x52, 0x61, 0x72, 0x21, 0x1A, 0x07}}, // Rar!
	".7z":  {{0x37, 0x7A, 0xBC, 0xAF, 0x27, 0x1C}},
	".gz":  {{0x1F, 0x8B}},
	".tar": {{0x75, 0x73, 0x74, 0x61, 0x72}}, // ustar at offset 257
	// 音频
	".mp3":  {{0x49, 0x44, 0x33}, {0xFF, 0xFB}, {0xFF, 0xFA}, {0xFF, 0xF3}, {0xFF, 0xF2}}, // ID3, MPEG frames
	".wav":  {{0x52, 0x49, 0x46, 0x46}},                                                   // RIFF
	".flac": {{0x66, 0x4C, 0x61, 0x43}},                                                   // fLaC
	".ogg":  {{0x4F, 0x67, 0x67, 0x53}},                                                   // OggS
	".m4a":  {}, // ftyp container, handled separately
	".aac":  {{0xFF, 0xF1}, {0xFF, 0xF9}},
	// 视频
	".mp4":  {}, // ftyp container, handled separately
	".avi":  {{0x52, 0x49, 0x46, 0x46}},
	".mov":  {}, // ftyp container, handled separately
	".mkv":  {{0x1A, 0x45, 0xDF, 0xA3}},
	".webm": {{0x1A, 0x45, 0xDF, 0xA3}},
	".flv":  {{0x46, 0x4C, 0x56}}, // FLV
	// Office (OOXML: ZIP-based)
	".docx": {{0x50, 0x4B, 0x03, 0x04}},
	".xlsx": {{0x50, 0x4B, 0x03, 0x04}},
	".pptx": {{0x50, 0x4B, 0x03, 0x04}},
	// Office (OLE2)
	".doc": {{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}},
	".xls": {{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}},
	".ppt": {{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}},
}

// ftypExtensions 需要 ftyp 容器格式验证的扩展名
var ftypExtensions = map[string]bool{
	".mp4": true,
	".mov": true,
	".m4a": true,
	".m4v": true,
	".3gp": true,
}

// ValidateMagicNumber 验证文件内容的魔数是否与扩展名匹配
// 返回 true 表示验证通过（内容与扩展名一致或该扩展名无需验证）
func ValidateMagicNumber(ext string, header []byte) bool {
	ext = strings.ToLower(ext)

	// 对于 ftyp 容器格式（mp4, mov, m4a 等），验证 offset 4-7 是否为 "ftyp"
	if ftypExtensions[ext] {
		if len(header) >= 8 && string(header[4:8]) == "ftyp" {
			return true
		}
		return false
	}

	signatures, exists := fileMagicNumbers[ext]
	if !exists {
		// 该扩展名没有魔数定义，跳过验证（如 .txt, .json 等文本文件）
		return true
	}
	if len(header) == 0 {
		return false
	}
	for _, sig := range signatures {
		if len(header) >= len(sig) && bytes.HasPrefix(header, sig) {
			return true
		}
	}
	return false
}

// Type 文件类型
type Type string

const (
	// TypeChat 聊天文件
	TypeChat Type = "chat"
	// TypeMoment 动态文件
	TypeMoment Type = "moment"
	// TypeMomentCover 动态封面
	TypeMomentCover Type = "momentcover"
	// TypeSticker 表情
	TypeSticker Type = "sticker"
	// TypeReport 举报
	TypeReport Type = "report"
	// TypeCommon 通用
	TypeCommon Type = "common"
	// TypeChatBg 聊天背景
	TypeChatBg Type = "chatbg"
	// TypeDownload 下载文件目录
	TypeDownload = "download"
	// TypeWorkplaceBanner
	TypeWorkplaceBanner Type = "workplacebanner"
	// TypeWorkplaceAppIcon
	TypeWorkplaceAppIcon Type = "workplaceappicon"
)

// MaxFileSize 最大文件大小（100MB）
const MaxFileSize int64 = 100 * 1024 * 1024

// allowedExtensions 允许上传的文件扩展名
var allowedExtensions = map[string]bool{
	// 图片
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".bmp": true, ".webp": true, ".ico": true,
	// 文档
	".pdf": true, ".doc": true, ".docx": true, ".xls": true,
	".xlsx": true, ".ppt": true, ".pptx": true, ".txt": true,
	".csv": true, ".rtf": true, ".odt": true, ".ods": true,
	".md": true, ".html": true, ".htm": true,
	// 音频
	".mp3": true, ".wav": true, ".aac": true, ".flac": true,
	".ogg": true, ".wma": true, ".m4a": true, ".amr": true,
	// 视频
	".mp4": true, ".avi": true, ".mov": true, ".wmv": true,
	".flv": true, ".mkv": true, ".webm": true, ".m4v": true,
	// 压缩包
	".zip": true, ".rar": true, ".7z": true, ".tar": true,
	".gz": true, ".bz2": true, ".xz": true,
	// 其他
	".json": true, ".xml": true, ".yaml": true, ".yml": true,
	// 安装包
	".dmg": true, ".pkg": true, ".deb": true, ".rpm": true, ".appimage": true,
}

// blockedExtensions 禁止上传的文件扩展名（可执行文件）
var blockedExtensions = map[string]bool{
	".exe": true, ".bat": true, ".sh": true, ".cmd": true,
	".msi": true, ".dll": true, ".com": true, ".scr": true,
	".pif": true, ".vbs": true, ".vbe": true, ".js": true,
	".jse": true, ".wsf": true, ".wsh": true, ".ps1": true,
	".sys": true, ".cpl": true, ".inf": true, ".reg": true,
	".apk": true, ".ipa": true,
	".php": true, ".jsp": true, ".asp": true, ".aspx": true,
	".cgi": true, ".py": true, ".rb": true, ".pl": true,
}

func init() {
	loadExtensionsFromEnv()
}

// loadExtensionsFromEnv 读取环境变量，追加扩展名到白名单/黑名单。
// 仅在 init() 中调用，不可在运行时重复调用（map 无并发写保护）。
// DM_FILE_EXTRA_ALLOWED: 逗号分隔的额外允许扩展名，如 ".svg,.heic" 或 "svg,heic"
// DM_FILE_EXTRA_BLOCKED: 逗号分隔的额外禁止扩展名，如 ".xyz,.abc"
// 注：init() 阶段 zap logger 尚未初始化，此处使用 stdlib log。
func loadExtensionsFromEnv() {
	normalizeExt := func(raw string) string {
		ext := strings.ToLower(strings.TrimSpace(raw))
		if ext == "" || ext == "." || ext == ".." {
			return ""
		}
		if strings.ContainsAny(ext, `/\`) {
			return ""
		}
		if !strings.HasPrefix(ext, ".") {
			ext = "." + ext
		}
		// 过滤 "..exe" 这类多连续点号的畸形输入：补全后仍含 ".." 则无效
		if strings.Contains(ext, "..") {
			return ""
		}
		return ext
	}

	var addedAllowed, addedBlocked []string

	if val := os.Getenv("DM_FILE_EXTRA_ALLOWED"); val != "" {
		for _, raw := range strings.Split(val, ",") {
			ext := normalizeExt(raw)
			if ext == "" {
				continue
			}
			if blockedExtensions[ext] {
				log.Printf("[file] DM_FILE_EXTRA_ALLOWED: %q 已在黑名单中，将被忽略", ext)
				continue
			}
			allowedExtensions[ext] = true
			addedAllowed = append(addedAllowed, ext)
		}
	}
	if val := os.Getenv("DM_FILE_EXTRA_BLOCKED"); val != "" {
		for _, raw := range strings.Split(val, ",") {
			ext := normalizeExt(raw)
			if ext == "" {
				continue
			}
			blockedExtensions[ext] = true
			addedBlocked = append(addedBlocked, ext)
			// 若同一扩展名也出现在 EXTRA_ALLOWED 中，从白名单清除保持状态一致
			delete(allowedExtensions, ext)
		}
	}

	if len(addedAllowed) > 0 {
		log.Printf("[file] DM_FILE_EXTRA_ALLOWED 已加载: %v", addedAllowed)
	}
	if len(addedBlocked) > 0 {
		log.Printf("[file] DM_FILE_EXTRA_BLOCKED 已加载: %v", addedBlocked)
	}
}

// IsAllowedExtension 检查文件扩展名是否允许上传
func IsAllowedExtension(ext string) bool {
	ext = strings.ToLower(ext)
	if blockedExtensions[ext] {
		return false
	}
	return allowedExtensions[ext]
}

// IsBlockedExtension 检查文件扩展名是否被禁止
func IsBlockedExtension(ext string) bool {
	return blockedExtensions[strings.ToLower(ext)]
}

// sanitizeFilename 清洗文件名，去除路径分隔符、CRLF、控制字符等危险字符
func sanitizeFilename(name string) string {
	// 去除路径前缀，只保留文件名部分
	name = filepath.Base(name)
	// 替换 Windows 路径分隔符残留
	name = strings.ReplaceAll(name, "\\", "_")

	// 过滤危险字符：CRLF、控制字符、双引号
	var b strings.Builder
	for _, r := range name {
		if r == '\r' || r == '\n' || r == '"' || r == '\x00' {
			b.WriteRune('_')
		} else if r < 0x20 { // 其他控制字符
			b.WriteRune('_')
		} else {
			b.WriteRune(r)
		}
	}
	name = b.String()

	// 限制长度为 255 字符（按 UTF-8 字符数）
	if utf8.RuneCountInString(name) > 255 {
		runes := []rune(name)
		ext := filepath.Ext(name)
		extRunes := []rune(ext)
		// 保留扩展名，截断文件名主体
		if len(extRunes) < 255 {
			name = string(runes[:255-len(extRunes)]) + ext
		} else {
			name = string(runes[:255])
		}
	}

	if name == "" || name == "." {
		name = "file"
	}

	return name
}
