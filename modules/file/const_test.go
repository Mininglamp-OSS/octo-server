package file

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsAllowedExtension(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		want bool
	}{
		// 图片格式
		{"jpg allowed", ".jpg", true},
		{"jpeg allowed", ".jpeg", true},
		{"png allowed", ".png", true},
		{"gif allowed", ".gif", true},
		{"bmp allowed", ".bmp", true},
		{"webp allowed", ".webp", true},
		{"ico allowed", ".ico", true},

		// 文档格式
		{"pdf allowed", ".pdf", true},
		{"doc allowed", ".doc", true},
		{"docx allowed", ".docx", true},
		{"xls allowed", ".xls", true},
		{"xlsx allowed", ".xlsx", true},
		{"ppt allowed", ".ppt", true},
		{"pptx allowed", ".pptx", true},
		{"txt allowed", ".txt", true},
		{"csv allowed", ".csv", true},
		{"md allowed", ".md", true},
		{"html allowed", ".html", true},
		{"htm allowed", ".htm", true},

		// 音频格式
		{"mp3 allowed", ".mp3", true},
		{"wav allowed", ".wav", true},
		{"aac allowed", ".aac", true},
		{"flac allowed", ".flac", true},
		{"amr allowed", ".amr", true},

		// 视频格式
		{"mp4 allowed", ".mp4", true},
		{"avi allowed", ".avi", true},
		{"mov allowed", ".mov", true},
		{"mkv allowed", ".mkv", true},

		// 压缩包
		{"zip allowed", ".zip", true},
		{"rar allowed", ".rar", true},
		{"7z allowed", ".7z", true},
		{"tar allowed", ".tar", true},

		// 其他允许
		{"json allowed", ".json", true},
		{"xml allowed", ".xml", true},

		// 文本/标记语言
		{"md allowed", ".md", true},
		{"html allowed", ".html", true},
		{"htm allowed", ".htm", true},

		// 被禁止的可执行文件 — IsAllowedExtension 应返回 false
		{"exe blocked", ".exe", false},
		{"bat blocked", ".bat", false},
		{"sh blocked", ".sh", false},
		{"cmd blocked", ".cmd", false},
		{"msi blocked", ".msi", false},
		{"dll blocked", ".dll", false},
		{"vbs blocked", ".vbs", false},
		{"ps1 blocked", ".ps1", false},
		{"php blocked", ".php", false},
		{"jsp blocked", ".jsp", false},
		{"py blocked", ".py", false},
		{"rb blocked", ".rb", false},
		{"js blocked", ".js", false},

		// 未知扩展名
		{"unknown ext", ".xyz", false},
		{"unknown ext2", ".abc", false},
		{"empty ext", "", false},
		{"dot only", ".", false},

		// 大小写不敏感
		{"JPG uppercase", ".JPG", true},
		{"Png mixed case", ".Png", true},
		{"PDF uppercase", ".PDF", true},
		{"EXE uppercase blocked", ".EXE", false},
		{"Bat mixed case blocked", ".Bat", false},
		{"PHP uppercase blocked", ".PHP", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAllowedExtension(tt.ext)
			assert.Equal(t, tt.want, got, "IsAllowedExtension(%q)", tt.ext)
		})
	}
}

func TestIsBlockedExtension(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		want bool
	}{
		// 可执行文件
		{"exe blocked", ".exe", true},
		{"bat blocked", ".bat", true},
		{"sh blocked", ".sh", true},
		{"cmd blocked", ".cmd", true},
		{"msi blocked", ".msi", true},
		{"dll blocked", ".dll", true},
		{"com blocked", ".com", true},
		{"scr blocked", ".scr", true},
		{"pif blocked", ".pif", true},

		// 脚本文件
		{"vbs blocked", ".vbs", true},
		{"vbe blocked", ".vbe", true},
		{"js blocked", ".js", true},
		{"jse blocked", ".jse", true},
		{"wsf blocked", ".wsf", true},
		{"wsh blocked", ".wsh", true},
		{"ps1 blocked", ".ps1", true},

		// 系统文件
		{"sys blocked", ".sys", true},
		{"cpl blocked", ".cpl", true},
		{"inf blocked", ".inf", true},
		{"reg blocked", ".reg", true},

		// 服务端脚本
		{"php blocked", ".php", true},
		{"jsp blocked", ".jsp", true},
		{"asp blocked", ".asp", true},
		{"aspx blocked", ".aspx", true},
		{"cgi blocked", ".cgi", true},
		{"py blocked", ".py", true},
		{"rb blocked", ".rb", true},
		{"pl blocked", ".pl", true},

		// 不在黑名单中的
		{"jpg not blocked", ".jpg", false},
		{"pdf not blocked", ".pdf", false},
		{"mp4 not blocked", ".mp4", false},
		{"zip not blocked", ".zip", false},
		{"unknown not blocked", ".xyz", false},
		{"empty not blocked", "", false},

		// 大小写不敏感
		{"EXE uppercase", ".EXE", true},
		{"Bat mixed", ".Bat", true},
		{"PHP uppercase", ".PHP", true},
		{"JPG uppercase not blocked", ".JPG", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsBlockedExtension(tt.ext)
			assert.Equal(t, tt.want, got, "IsBlockedExtension(%q)", tt.ext)
		})
	}
}

func TestIsAllowedExtension_BlockedTakesPriority(t *testing.T) {
	// 确保被禁止的扩展名不会因为同时在允许列表中而被放行
	// （当前实现中 blocked 优先检查）
	for ext := range blockedExtensions {
		assert.False(t, IsAllowedExtension(ext),
			"blocked extension %q should not be allowed", ext)
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		// 正常文件名
		{"normal filename", "test.jpg", "test.jpg"},
		{"filename with spaces", "my file.txt", "my file.txt"},
		{"chinese filename", "图片.png", "图片.png"},

		// 路径清理
		{"unix path", "/etc/passwd", "passwd"},
		{"windows path", "C:\\Users\\test.exe", "C:_Users_test.exe"}, // Linux: filepath.Base 不处理反斜杠，反斜杠后被替换为下划线
		{"relative path", "../../../etc/passwd", "passwd"},

		// 危险字符替换
		{"CRLF injection", "file\r\nname.jpg", "file__name.jpg"},
		{"CR only", "file\rname.jpg", "file_name.jpg"},
		{"LF only", "file\nname.jpg", "file_name.jpg"},
		{"null byte", "file\x00.jpg", "file_.jpg"},
		{"double quotes", "file\"name.jpg", "file_name.jpg"},
		{"control chars", "file\x01\x02.jpg", "file__.jpg"},
		{"tab char", "file\t.jpg", "file_.jpg"}, // \t (0x09) < 0x20, replaced by underscore
		{"bell char", "file\a.jpg", "file_.jpg"},

		// 空文件名
		{"empty string", "", "file"},
		{"dot only", ".", "file"},

		// 长文件名截断
		{"exactly 255 chars", strings.Repeat("a", 255), strings.Repeat("a", 255)},
		{"256 chars no ext", strings.Repeat("a", 256), strings.Repeat("a", 255)},
		{"long name with ext", strings.Repeat("a", 260) + ".jpg", strings.Repeat("a", 251) + ".jpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeFilename(tt.input)
			assert.Equal(t, tt.expected, got, "sanitizeFilename(%q)", tt.input)
		})
	}
}

func TestSanitizeFilename_PreservesExtension(t *testing.T) {
	// 验证长文件名截断时保留扩展名
	longName := strings.Repeat("x", 300) + ".pdf"
	result := sanitizeFilename(longName)
	assert.True(t, strings.HasSuffix(result, ".pdf"), "should preserve .pdf extension")
	assert.LessOrEqual(t, len([]rune(result)), 255, "should not exceed 255 runes")
}

func TestSanitizeFilename_ControlCharsReplacedWithUnderscore(t *testing.T) {
	// 所有 0x00-0x1F 的控制字符都应该被替换
	for c := rune(0); c < 0x20; c++ {
		input := "a" + string(c) + "b.txt"
		result := sanitizeFilename(input)
		assert.NotContains(t, result, string(c),
			"control char 0x%02x should be replaced", c)
	}
}

func TestFileTypeConstants(t *testing.T) {
	// 验证文件类型常量值
	assert.Equal(t, Type("chat"), TypeChat)
	assert.Equal(t, Type("moment"), TypeMoment)
	assert.Equal(t, Type("momentcover"), TypeMomentCover)
	assert.Equal(t, Type("sticker"), TypeSticker)
	assert.Equal(t, Type("report"), TypeReport)
	assert.Equal(t, Type("common"), TypeCommon)
	assert.Equal(t, Type("chatbg"), TypeChatBg)
	assert.Equal(t, Type("workplacebanner"), TypeWorkplaceBanner)
	assert.Equal(t, Type("workplaceappicon"), TypeWorkplaceAppIcon)
}

func TestMaxFileSize(t *testing.T) {
	assert.Equal(t, int64(100*1024*1024), MaxFileSize, "MaxFileSize should be 100MB")
}

func TestAllowedExtensionsCompleteness(t *testing.T) {
	// 确保关键文件类型都在允许列表中
	criticalTypes := []string{
		".jpg", ".jpeg", ".png", ".gif",
		".pdf", ".doc", ".docx",
		".mp3", ".mp4",
		".zip",
	}
	for _, ext := range criticalTypes {
		assert.True(t, IsAllowedExtension(ext), "%s should be allowed", ext)
	}
}

func TestBlockedExtensionsCompleteness(t *testing.T) {
	// 确保所有危险的可执行文件类型都在禁止列表中
	dangerousTypes := []string{
		".exe", ".bat", ".sh", ".cmd", ".msi", ".dll",
		".vbs", ".ps1", ".php", ".jsp", ".asp",
	}
	for _, ext := range dangerousTypes {
		assert.True(t, IsBlockedExtension(ext), "%s should be blocked", ext)
	}
}

func TestIsAllowedExtension_DoubleExtensions(t *testing.T) {
	// 双扩展名 — filepath.Ext 只取最后一个扩展名，这里测试最终扩展名的判定
	tests := []struct {
		name string
		ext  string
		want bool
	}{
		{"final ext is jpg", ".jpg", true},
		{"final ext is pdf", ".pdf", true},
		{"final ext is exe", ".exe", false},
		{"final ext is php", ".php", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsAllowedExtension(tt.ext)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsBlockedExtension_AllEntries(t *testing.T) {
	allBlocked := []string{
		".exe", ".bat", ".sh", ".cmd", ".msi", ".dll", ".com", ".scr", ".pif",
		".vbs", ".vbe", ".js", ".jse", ".wsf", ".wsh", ".ps1",
		".sys", ".cpl", ".inf", ".reg",
		".apk", ".ipa",
		".php", ".jsp", ".asp", ".aspx", ".cgi", ".py", ".rb", ".pl",
	}
	for _, ext := range allBlocked {
		assert.True(t, IsBlockedExtension(ext), "%s should be blocked", ext)
		assert.False(t, IsAllowedExtension(ext), "%s should not be allowed", ext)
	}
}

func TestIsAllowedExtension_AllEntries(t *testing.T) {
	allAllowed := []string{
		".jpg", ".jpeg", ".png", ".gif", ".bmp", ".webp", ".ico",
		".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx",
		".txt", ".csv", ".rtf", ".odt", ".ods", ".md", ".html", ".htm",
		".mp3", ".wav", ".aac", ".flac", ".ogg", ".wma", ".m4a", ".amr",
		".mp4", ".avi", ".mov", ".wmv", ".flv", ".mkv", ".webm", ".m4v",
		".zip", ".rar", ".7z", ".tar", ".gz", ".bz2", ".xz",
		".json", ".xml", ".yaml", ".yml",
		".md", ".html", ".htm",
	}
	for _, ext := range allAllowed {
		assert.True(t, IsAllowedExtension(ext), "%s should be allowed", ext)
		assert.False(t, IsBlockedExtension(ext), "%s should not be blocked", ext)
	}
}

func TestSanitizeFilename_UTF8EdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"emoji filename", "🎉🎊🎉.jpg"},
		{"mixed unicode", "résumé_文件.pdf"},
		{"just extension", ".jpg"},
		{"japanese", "画像ファイル.png"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeFilename(tt.input)
			assert.NotEmpty(t, result)
			assert.LessOrEqual(t, len([]rune(result)), 255)
		})
	}
}

func TestSanitizeFilename_BackslashHandling(t *testing.T) {
	result := sanitizeFilename("dir\\subdir\\file.txt")
	assert.NotContains(t, result, "\\")
	assert.Contains(t, result, "file.txt")
}

func TestMaxFileSize_Value(t *testing.T) {
	assert.Equal(t, int64(104857600), MaxFileSize)
	assert.Greater(t, MaxFileSize, int64(10*1024*1024))
	assert.LessOrEqual(t, MaxFileSize, int64(1024*1024*1024))
}

func TestSanitizeFilename_MultipleDotsInName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"multiple dots", "file.backup.2024.tar.gz", "file.backup.2024.tar.gz"},
		{"dot at start", ".hidden.txt", ".hidden.txt"},
		{"dots only", "...", "..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeFilename(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestValidateMagicNumber(t *testing.T) {
	tests := []struct {
		name   string
		ext    string
		header []byte
		want   bool
	}{
		// PNG
		{"valid png", ".png", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, true},
		{"invalid png header", ".png", []byte{0xFF, 0xD8, 0xFF}, false},
		{"png short header", ".png", []byte{0x89, 0x50}, false},

		// JPEG
		{"valid jpg", ".jpg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}, true},
		{"valid jpeg", ".jpeg", []byte{0xFF, 0xD8, 0xFF}, true},
		{"invalid jpg header", ".jpg", []byte{0x89, 0x50, 0x4E, 0x47}, false},

		// GIF
		{"valid gif87a", ".gif", []byte{0x47, 0x49, 0x46, 0x38, 0x37, 0x61}, true},
		{"valid gif89a", ".gif", []byte{0x47, 0x49, 0x46, 0x38, 0x39, 0x61}, true},
		{"invalid gif", ".gif", []byte{0xFF, 0xD8, 0xFF}, false},

		// WebP (RIFF container — must also carry the "WEBP" fourCC at bytes 8-11,
		// else WAV/AVI RIFF streams renamed .webp would pass; PR#508 review).
		{"valid webp", ".webp", []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P'}, true},
		{"webp riff but wav fourcc", ".webp", []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00, 'W', 'A', 'V', 'E'}, false},
		{"webp riff but avi fourcc", ".webp", []byte{'R', 'I', 'F', 'F', 0x10, 0x00, 0x00, 0x00, 'A', 'V', 'I', 0x20}, false},
		{"webp riff prefix only too short", ".webp", []byte{'R', 'I', 'F', 'F'}, false},
		{"webp non-riff", ".webp", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, false},

		// PDF
		{"valid pdf", ".pdf", []byte{0x25, 0x50, 0x44, 0x46, 0x2D}, true},
		{"invalid pdf", ".pdf", []byte{0x50, 0x4B, 0x03, 0x04}, false},

		// MP4 (ftyp container format)
		{"valid mp4 ftyp", ".mp4", []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, true},
		{"valid mp4 ftyp mp42", ".mp4", []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}, true},
		{"invalid mp4 no ftyp", ".mp4", []byte{0x00, 0x00, 0x00, 0x18, 'm', 'o', 'o', 'v'}, false},
		{"invalid mp4 too short", ".mp4", []byte{0x00, 0x00, 0x00}, false},
		{"invalid mp4 random bytes", ".mp4", []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}, false},

		// MOV (ftyp container format)
		{"valid mov ftyp", ".mov", []byte{0x00, 0x00, 0x00, 0x14, 'f', 't', 'y', 'p', 'q', 't', ' ', ' '}, true},
		{"invalid mov no ftyp", ".mov", []byte{0x00, 0x00, 0x00, 0x14, 'm', 'o', 'o', 'v'}, false},

		// M4A (ftyp container format)
		{"valid m4a ftyp", ".m4a", []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '}, true},
		{"invalid m4a no ftyp", ".m4a", []byte{0x00, 0x00, 0x00, 0x20, 'f', 'r', 'e', 'e'}, false},

		// Text files (no magic number defined, should pass)
		{"txt no magic needed", ".txt", []byte("Hello, World!"), true},
		{"json no magic needed", ".json", []byte("{\"key\": \"value\"}"), true},

		// Unknown extension (no magic number defined, should pass)
		{"unknown ext", ".xyz", []byte{0xFF, 0xFF, 0xFF}, true},

		// Empty header
		{"empty header png", ".png", []byte{}, false},
		{"empty header mp4", ".mp4", []byte{}, false},

		// Case insensitive extension
		{"uppercase PNG", ".PNG", []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}, true},
		{"mixed case Mp4", ".Mp4", []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p'}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateMagicNumber(tt.ext, tt.header)
			assert.Equal(t, tt.want, got, "ValidateMagicNumber(%q, header)", tt.ext)
		})
	}
}

func TestValidateMagicNumber_FtypFormats(t *testing.T) {
	// Test that ftyp container formats require proper ftyp magic at offset 4
	ftypFormats := []string{".mp4", ".mov", ".m4a", ".m4v", ".3gp"}

	// Valid ftyp header - any value for first 4 bytes, but "ftyp" at bytes 4-7
	validFtypHeader := []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}

	// Invalid header - just zeros (should fail because no "ftyp" at offset 4)
	invalidHeader := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

	for _, ext := range ftypFormats {
		t.Run("valid "+ext, func(t *testing.T) {
			assert.True(t, ValidateMagicNumber(ext, validFtypHeader),
				"%s with valid ftyp header should pass", ext)
		})
		t.Run("invalid "+ext, func(t *testing.T) {
			assert.False(t, ValidateMagicNumber(ext, invalidHeader),
				"%s with invalid header should fail", ext)
		})
	}
}

func TestValidateMagicNumber_MP4Variations(t *testing.T) {
	// Test various real-world MP4/MOV ftyp variations
	tests := []struct {
		name   string
		header []byte
		want   bool
	}{
		{
			name:   "isom brand",
			header: []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'},
			want:   true,
		},
		{
			name:   "mp42 brand",
			header: []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'},
			want:   true,
		},
		{
			name:   "M4A brand",
			header: []byte{0x00, 0x00, 0x00, 0x20, 'f', 't', 'y', 'p', 'M', '4', 'A', ' '},
			want:   true,
		},
		{
			name:   "qt brand (QuickTime)",
			header: []byte{0x00, 0x00, 0x00, 0x14, 'f', 't', 'y', 'p', 'q', 't', ' ', ' '},
			want:   true,
		},
		{
			name:   "moov atom instead of ftyp",
			header: []byte{0x00, 0x00, 0x00, 0x14, 'm', 'o', 'o', 'v'},
			want:   false,
		},
		{
			name:   "free atom instead of ftyp",
			header: []byte{0x00, 0x00, 0x00, 0x08, 'f', 'r', 'e', 'e'},
			want:   false,
		},
		{
			name:   "short header",
			header: []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y'},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidateMagicNumber(".mp4", tt.header)
			assert.Equal(t, tt.want, got)
		})
	}
}

func snapshotExtMap(m map[string]bool) map[string]bool {
	cp := make(map[string]bool, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

// TestLoadExtensionsFromEnv 直接修改 package-level map，子测试不可并行执行。
func TestLoadExtensionsFromEnv(t *testing.T) {
	withCleanMaps := func(t *testing.T, fn func(t *testing.T)) {
		t.Helper()
		origAllowed := snapshotExtMap(allowedExtensions)
		origBlocked := snapshotExtMap(blockedExtensions)
		t.Cleanup(func() {
			allowedExtensions = origAllowed
			blockedExtensions = origBlocked
		})
		fn(t)
	}

	t.Run("DM_FILE_EXTRA_ALLOWED 追加白名单", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", ".svg,.heic")
			loadExtensionsFromEnv()

			assert.True(t, IsAllowedExtension(".svg"), ".svg 应被允许")
			assert.True(t, IsAllowedExtension(".heic"), ".heic 应被允许")
			assert.True(t, IsAllowedExtension(".jpg"), "原有 .jpg 应保持允许")
		})
	})

	t.Run("DM_FILE_EXTRA_BLOCKED 追加黑名单", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_BLOCKED", ".xyz,.abc")
			loadExtensionsFromEnv()

			assert.True(t, IsBlockedExtension(".xyz"), ".xyz 应被禁止")
			assert.True(t, IsBlockedExtension(".abc"), ".abc 应被禁止")
			assert.False(t, IsAllowedExtension(".xyz"), "黑名单优先，.xyz 不应被允许")
			assert.True(t, IsBlockedExtension(".exe"), "原有 .exe 应保持禁止")
		})
	})

	t.Run("大小写与空格容错", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", " .SVG , .HEIC ")
			loadExtensionsFromEnv()

			assert.True(t, IsAllowedExtension(".SVG"), "大写 .SVG 应被允许")
			assert.True(t, IsAllowedExtension(".svg"), "小写 .svg 应被允许")
		})
	})

	t.Run("不带点号自动补全", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", "tiff,avif")
			t.Setenv("DM_FILE_EXTRA_BLOCKED", "bin")
			loadExtensionsFromEnv()

			assert.True(t, IsAllowedExtension(".tiff"), "不带点号的 tiff 应被自动补全并允许")
			assert.True(t, IsAllowedExtension(".avif"), "不带点号的 avif 应被自动补全并允许")
			assert.True(t, IsBlockedExtension(".bin"), "不带点号的 bin 应被自动补全并禁止")
		})
	})

	t.Run("空环境变量不影响现有配置", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", "")
			t.Setenv("DM_FILE_EXTRA_BLOCKED", "")
			loadExtensionsFromEnv()

			assert.True(t, IsAllowedExtension(".jpg"), ".jpg 应保持允许")
			assert.True(t, IsBlockedExtension(".exe"), ".exe 应保持禁止")
		})
	})

	t.Run("黑名单中的扩展名加入白名单时被忽略", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", ".exe,.php")
			loadExtensionsFromEnv()

			assert.False(t, IsAllowedExtension(".exe"), ".exe 在黑名单中，白名单设置应被忽略")
			assert.False(t, IsAllowedExtension(".php"), ".php 在黑名单中，白名单设置应被忽略")
		})
	})

	t.Run("纯点号输入被忽略", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", ".,..,  ")
			loadExtensionsFromEnv()

			assert.False(t, allowedExtensions["."], `"." 不应被加入白名单`)
			assert.False(t, allowedExtensions[".."], `".." 不应被加入白名单`)
		})
	})

	t.Run("含路径分隔符的输入被忽略", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", "foo/bar,.svg")
			loadExtensionsFromEnv()

			assert.False(t, allowedExtensions[".foo/bar"], "含路径分隔符的扩展名不应被加入白名单")
			assert.True(t, IsAllowedExtension(".svg"), "合法扩展名 .svg 应被允许")
		})
	})

	t.Run("同一扩展名同时出现在两个 env var 中以黑名单为准", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", ".danger")
			t.Setenv("DM_FILE_EXTRA_BLOCKED", ".danger")
			loadExtensionsFromEnv()

			assert.False(t, IsAllowedExtension(".danger"), "黑名单优先，.danger 不应被允许")
			assert.True(t, IsBlockedExtension(".danger"), ".danger 应被禁止")
			assert.False(t, allowedExtensions[".danger"], ".danger 不应残留在 allowedExtensions map 中")
		})
	})

	t.Run("将已有白名单扩展名加入黑名单", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_BLOCKED", ".jpg")
			loadExtensionsFromEnv()

			assert.True(t, IsBlockedExtension(".jpg"), ".jpg 应被禁止")
			assert.False(t, IsAllowedExtension(".jpg"), ".jpg 不应再被允许")
			assert.False(t, allowedExtensions[".jpg"], ".jpg 应从 allowedExtensions 中移除")
		})
	})

	t.Run("多连续点号的畸形输入被忽略", func(t *testing.T) {
		withCleanMaps(t, func(t *testing.T) {
			t.Setenv("DM_FILE_EXTRA_ALLOWED", "..exe,..svg")
			loadExtensionsFromEnv()

			assert.False(t, allowedExtensions["..exe"], `"..exe" 不应被加入白名单`)
			assert.False(t, allowedExtensions["..svg"], `"..svg" 不应被加入白名单`)
		})
	})
}
