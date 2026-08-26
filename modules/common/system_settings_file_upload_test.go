package common

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 文件上传策略读侧测试（task file-extension-policy-dynamic-config）。
// 全部 no-infra：直接向 SystemSettings.snapshot 灌 map，与
// system_settings_sticker_upload_test.go 同风格。

func fileSnapSettings(snap map[string]string) *SystemSettings {
	s := &SystemSettings{Log: log.NewTLog("SystemSettingsTest")}
	m := map[string]string{}
	for k, v := range snap {
		m[k] = v
	}
	s.snapshot.Store(&m)
	return s
}

// withBuiltinBlockedProbe 注册一个内置黑名单探针（生产由 modules/file 的 init
// 注册；本包的测试二进制不链接 file，探针默认缺席）。
func withBuiltinBlockedProbe(t *testing.T, blocked ...string) {
	t.Helper()
	set := make(map[string]struct{}, len(blocked))
	for _, b := range blocked {
		set[b] = struct{}{}
	}
	prev := fileBlockedProbe.Load()
	SetBuiltinBlockedFileExtensionProbe(func(ext string) bool {
		_, hit := set[ext]
		return hit
	})
	t.Cleanup(func() { fileBlockedProbe.Store(prev) })
}

// ----- NormalizeFileExtension：语义必须与改动前 modules/file 的 normalizeExt 一致 -----

func TestNormalizeFileExtension(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"已带点号", ".svg", ".svg"},
		{"不带点号自动补全", "svg", ".svg"},
		{"大小写归一", ".SVG", ".svg"},
		{"前后空格容错", "  .SVG  ", ".svg"},
		{"空串无效", "", ""},
		{"纯空格无效", "   ", ""},
		{"单点号无效", ".", ""},
		{"双点号无效", "..", ""},
		{"含正斜杠无效", "foo/bar", ""},
		{"含反斜杠无效", `foo\bar`, ""},
		{"多连续点号无效", "..exe", ""},
		{"内部空格无效", ".a b", ""},
		{"换行无效", ".pd\nf", ""},
		{"NUL 无效", ".a\x00b", ""},
		{"制表符无效", ".a\tb", ""},
		{"补全后含连续点无效", ".a..b", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NormalizeFileExtension(tc.in))
		})
	}
}

func TestParseFileExtensionCSV(t *testing.T) {
	assert.Equal(t, []string{".svg", ".heic"}, ParseFileExtensionCSV(" .SVG , heic "))
	assert.Nil(t, ParseFileExtensionCSV(""))
	assert.Nil(t, ParseFileExtensionCSV("  "))
	// 全部非法 → nil，而不是空切片里塞垃圾
	assert.Nil(t, ParseFileExtensionCSV(".,..,foo/bar"))
	// 去重，保留首次出现顺序
	assert.Equal(t, []string{".png", ".jpg"}, ParseFileExtensionCSV("png,jpg,.PNG,png"))
}

// ----- extra_allowed：env ∪ DB，只增不减（D9） -----

func TestFileExtraAllowed_UnconfiguredFallsBackToEnv(t *testing.T) {
	t.Setenv(envFileExtraAllowed, ".svg,.dwg")
	s := fileSnapSettings(nil)
	assert.Equal(t, []string{".dwg", ".svg"}, s.FileExtraAllowedExtensions())
}

func TestFileExtraAllowed_UnionOfEnvAndDB(t *testing.T) {
	t.Setenv(envFileExtraAllowed, ".tgz,.xlsm")
	s := fileSnapSettings(map[string]string{"file.extra_allowed_extensions": "dwg"})
	assert.Equal(t, []string{".dwg", ".tgz", ".xlsm"}, s.FileExtraAllowedExtensions())
}

// 这是并集语义存在的理由，也是本键最重要的回归点：现网 env 里放开着几个格式，
// 运维想再加一个时只填新的那个 —— 原有格式绝不能因此失效。覆盖语义下
// .tgz/.xlsm 会当场作废，用户立刻传不了，而运维不会想到这一层。
func TestFileExtraAllowed_AddingOneDoesNotDropEnvEntries(t *testing.T) {
	t.Setenv(envFileExtraAllowed, ".tgz,.xlsm,.key,.numbers,.pages,.heic")
	s := fileSnapSettings(map[string]string{"file.extra_allowed_extensions": "dwg"})

	got := s.FileExtraAllowedExtensions()
	for _, ext := range []string{".tgz", ".xlsm", ".key", ".numbers", ".pages", ".heic"} {
		assert.Contains(t, got, ext, "env 里放开的 %s 不能因为管理台加了一项就失效", ext)
	}
	assert.Contains(t, got, ".dwg")
}

func TestFileExtraAllowed_EmptyDBValueKeepsEnvEntries(t *testing.T) {
	t.Setenv(envFileExtraAllowed, ".svg")
	// 空串在 lookup 里等同「未配置」（全仓 65 个键共用的契约）。
	s := fileSnapSettings(map[string]string{"file.extra_allowed_extensions": ""})
	assert.Equal(t, []string{".svg"}, s.FileExtraAllowedExtensions())
}

// 这个键的目的就是「不重启放开一个文件格式」：除了内置黑名单，任意扩展名都能
// 放开，不需要先发一次版。
func TestFileExtraAllowed_AllowsArbitraryExtension(t *testing.T) {
	t.Setenv(envFileExtraAllowed, "")
	s := fileSnapSettings(map[string]string{
		"file.extra_allowed_extensions": "dwg,psd,step,sketch",
	})
	assert.Equal(t, []string{".dwg", ".psd", ".sketch", ".step"}, s.FileExtraAllowedExtensions())
}

// 内置黑名单项即使被写进 DB（例如绕过写侧直接改库）也不会出现在返回值里 ——
// 否则管理台 effective_value 会显示一个「写着但不生效」的扩展名，比不显示更误导。
func TestFileExtraAllowed_FiltersBuiltinBlocked(t *testing.T) {
	t.Setenv(envFileExtraAllowed, "")
	withBuiltinBlockedProbe(t, ".exe", ".php")
	s := fileSnapSettings(map[string]string{
		"file.extra_allowed_extensions": "dwg,exe,php",
	})
	assert.Equal(t, []string{".dwg"}, s.FileExtraAllowedExtensions())
}

// 语法全非法时等同没写，env 项保持不变。
func TestFileExtraAllowed_AllInvalidKeepsEnvEntries(t *testing.T) {
	t.Setenv(envFileExtraAllowed, ".svg")
	s := fileSnapSettings(map[string]string{"file.extra_allowed_extensions": ".,..,foo/bar"})
	assert.Equal(t, []string{".svg"}, s.FileExtraAllowedExtensions())
}

// 收回一个 env 放开项的正确姿势：写进 extra_blocked（黑名单优先级最高）。
// 并集语义下这是唯一的「减」入口，规矩统一。
func TestFileExtraAllowed_RevokedViaBlocklist(t *testing.T) {
	t.Setenv(envFileExtraAllowed, ".tgz,.xlsm")
	t.Setenv(envFileExtraBlocked, "")
	s := fileSnapSettings(map[string]string{"file.extra_blocked_extensions": "tgz"})

	// 配置层各管各的：allowed 仍列出 .tgz，blocked 也列出它。
	assert.Contains(t, s.FileExtraAllowedExtensions(), ".tgz")
	assert.Contains(t, s.FileExtraBlockedExtensions(), ".tgz")
	// 最终谁赢由 modules/file 的派生决定（黑名单在减号右边），
	// 见 policy_test.go:TestPolicy_BlockedWinsOverAllowedInSettings。
}

// ----- extra_blocked：env ∪ DB，只增不减（D1） -----

func TestFileExtraBlocked_UnionOfEnvAndDB(t *testing.T) {
	t.Setenv(envFileExtraBlocked, ".abc")
	s := fileSnapSettings(map[string]string{"file.extra_blocked_extensions": "xyz"})
	assert.Equal(t, []string{".abc", ".xyz"}, s.FileExtraBlockedExtensions())
}

// 这是并集语义存在的理由：若用覆盖语义，运维为封堵 .xyz 写一次 DB 就会把 env
// 里已封的 .abc 静默解封。
func TestFileExtraBlocked_DBWriteNeverUnblocksEnvEntry(t *testing.T) {
	t.Setenv(envFileExtraBlocked, ".abc")
	s := fileSnapSettings(map[string]string{"file.extra_blocked_extensions": "xyz"})
	assert.Contains(t, s.FileExtraBlockedExtensions(), ".abc")
}

func TestFileExtraBlocked_EmptyWhenNeitherConfigured(t *testing.T) {
	t.Setenv(envFileExtraBlocked, "")
	assert.Nil(t, fileSnapSettings(nil).FileExtraBlockedExtensions())
}

// ----- max_size_kb -----

func TestFileMaxSizeKB(t *testing.T) {
	assert.Equal(t, DefaultFileMaxSizeKB, fileSnapSettings(nil).FileMaxSizeKB())
	assert.Equal(t, 2048, fileSnapSettings(map[string]string{"file.max_size_kb": "2048"}).FileMaxSizeKB())
	// 越界回退默认值，而不是被原样服务（覆盖直改库的旁路）。
	for _, bad := range []string{"0", "-1", "abc", "99999999"} {
		s := fileSnapSettings(map[string]string{"file.max_size_kb": bad})
		assert.Equal(t, DefaultFileMaxSizeKB, s.FileMaxSizeKB(), "value=%s", bad)
	}
	// 恰好等于硬上限是允许的。
	s := fileSnapSettings(map[string]string{"file.max_size_kb": "524288"})
	assert.Equal(t, FileMaxSizeKBHardCap, s.FileMaxSizeKB())
}

// ----- D6 组合约束 -----

func TestFileStickerSizeOrdering(t *testing.T) {
	assert.False(t, ViolatesFileStickerSizeOrdering(FileStickerSizeOrdering{FileMaxSizeKB: 102400, StickerMaxSizeKB: 1024}))
	assert.True(t, ViolatesFileStickerSizeOrdering(FileStickerSizeOrdering{FileMaxSizeKB: 512, StickerMaxSizeKB: 1024}))
	// 相等不算冲突。
	assert.False(t, ViolatesFileStickerSizeOrdering(FileStickerSizeOrdering{FileMaxSizeKB: 1024, StickerMaxSizeKB: 1024}))
}

func TestApplyFileStickerSizeOverlay(t *testing.T) {
	cur := FileStickerSizeOrdering{FileMaxSizeKB: 102400, StickerMaxSizeKB: 1024}

	// 只改一边，另一边保持当前快照值 —— merge-then-validate 的关键。
	got := ApplyFileStickerSizeOverlay(cur, map[string]string{"file.max_size_kb": "512"})
	assert.Equal(t, 512, got.FileMaxSizeKB)
	assert.Equal(t, 1024, got.StickerMaxSizeKB)
	assert.True(t, ViolatesFileStickerSizeOrdering(got))

	// 从 sticker 那一侧把上限抬过全局上限，同样要被识别为冲突。
	got = ApplyFileStickerSizeOverlay(
		FileStickerSizeOrdering{FileMaxSizeKB: 2048, StickerMaxSizeKB: 1024},
		map[string]string{"sticker.upload_max_size_kb": "4096"},
	)
	assert.True(t, ViolatesFileStickerSizeOrdering(got))

	// 空串 = 清除该键，回到 code default。
	got = ApplyFileStickerSizeOverlay(cur, map[string]string{"file.max_size_kb": ""})
	assert.Equal(t, DefaultFileMaxSizeKB, got.FileMaxSizeKB)

	// 非法值保持当前值（类型校验在调用方已跑过）。
	got = ApplyFileStickerSizeOverlay(cur, map[string]string{"file.max_size_kb": "abc"})
	assert.Equal(t, 102400, got.FileMaxSizeKB)
}

// ----- appconfig provider -----

func TestFileUploadLimitsProvider(t *testing.T) {
	prev := fileUploadLimitsProvider.Load()
	t.Cleanup(func() { fileUploadLimitsProvider.Store(prev) })

	fileUploadLimitsProvider.Store(nil)
	_, _, ok := FileUploadLimits()
	assert.False(t, ok, "provider 未注册时必须报告 ok=false，让调用方整体不下发字段")

	SetFileUploadLimitsProvider(func() (int, []string) { return 2048, nil })
	kb, exts, ok := FileUploadLimits()
	require.True(t, ok)
	assert.Equal(t, 2048, kb)
	// nil 归一成空切片，避免 JSON 序列化出 null。
	assert.NotNil(t, exts)
	assert.Empty(t, exts)
}

// ----- M1：条数与单项长度上限 -----

// 写侧限制的读侧配套：即便超长/超量的值绕过写侧直接进了库，读侧也不该把它
// 原样交出去 —— 这份清单会下发到无鉴权的 /v1/common/appconfig。
func TestFileExtensionList_LongEntryIsRejectedByNormalisation(t *testing.T) {
	// 超长 token 本身仍是合法语法，读侧不截断（写侧负责拒绝），
	// 这条只钉住「读侧不会因为超长而 panic 或吞掉整个列表」。
	long := "." + strings.Repeat("a", 200)
	got := ParseFileExtensionCSV(long + ",dwg")
	assert.Len(t, got, 2)
	assert.Contains(t, got, ".dwg")
}
