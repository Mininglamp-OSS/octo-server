package file

import (
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetPolicyForTest 把策略层恢复到「未挂载 settings」状态并清空快照缓存，
// 让用例回落到 env + baseline。测试结束还原原有 provider，避免用例间串扰
// （集成测试里 New(ctx) 会挂上真的 SystemSettings）。
func resetPolicyForTest(t *testing.T) {
	t.Helper()
	prev := provider.Load()
	provider.Store(nil)
	cached.Store(nil)
	t.Cleanup(func() {
		provider.Store(prev)
		cached.Store(nil)
	})
}

// fakePolicySettings 是 PolicySettings 的内存实现，值语义与 common 侧 getter
// 的输出契约一致（已完成 DB→env 回退、候选集过滤、env∪DB 合并）。
type fakePolicySettings struct {
	allowed []string
	blocked []string
	maxKB   int
}

func (f fakePolicySettings) FileExtraAllowedExtensions() []string { return f.allowed }
func (f fakePolicySettings) FileExtraBlockedExtensions() []string { return f.blocked }
func (f fakePolicySettings) FileMaxSizeKB() int                   { return f.maxKB }

func useSettings(t *testing.T, s PolicySettings) {
	t.Helper()
	resetPolicyForTest(t)
	SetPolicySettings(s)
	cached.Store(nil)
}

// ---------------------------------------------------------------------------
// 等价性：DB 与 env 都未配置时，判定必须与本次改动前逐字节相同。
// ---------------------------------------------------------------------------

func TestPolicy_UnconfiguredMatchesBaseline(t *testing.T) {
	resetPolicyForTest(t)
	t.Setenv("DM_FILE_EXTRA_ALLOWED", "")
	t.Setenv("DM_FILE_EXTRA_BLOCKED", "")

	for ext := range allowedExtensions {
		assert.True(t, IsAllowedExtension(ext), "baseline 允许的 %s 必须仍被允许", ext)
		assert.False(t, IsBlockedExtension(ext), "baseline 允许的 %s 不该被判为 blocked", ext)
	}
	for ext := range blockedExtensions {
		assert.True(t, IsBlockedExtension(ext), "baseline 禁止的 %s 必须仍被禁止", ext)
		assert.False(t, IsAllowedExtension(ext), "baseline 禁止的 %s 不该被允许", ext)
	}
	// 未知扩展名既不在白名单也不在黑名单 → 不允许上传。
	assert.False(t, IsAllowedExtension(".nosuchext"))
	assert.False(t, IsBlockedExtension(".nosuchext"))
}

func TestPolicy_MaxUploadSizeDefaultMatchesLegacyConstant(t *testing.T) {
	resetPolicyForTest(t)
	assert.Equal(t, MaxFileSize, MaxUploadSize(),
		"未配置时 MaxUploadSize() 必须等于改动前的 MaxFileSize 常量")
}

// ---------------------------------------------------------------------------
// env 兼容层。
//
// 取代改动前的 const_test.go:TestLoadExtensionsFromEnv —— 那个函数直接调
// loadExtensionsFromEnv() 并断言包级 map 的内容，现在 map 是只读 baseline。
// **11 条子用例的名称与语义逐条保留**，断言改走公开 API，方便与改动前对照。
// ---------------------------------------------------------------------------

func TestExtensionPolicy_EnvCompatibility(t *testing.T) {
	t.Run("DM_FILE_EXTRA_ALLOWED 追加白名单", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", ".svg,.heic")

		assert.True(t, IsAllowedExtension(".svg"), ".svg 应被允许")
		assert.True(t, IsAllowedExtension(".heic"), ".heic 应被允许")
		assert.True(t, IsAllowedExtension(".jpg"), "原有 .jpg 应保持允许")
	})

	t.Run("DM_FILE_EXTRA_BLOCKED 追加黑名单", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_BLOCKED", ".xyz,.abc")

		assert.True(t, IsBlockedExtension(".xyz"), ".xyz 应被禁止")
		assert.True(t, IsBlockedExtension(".abc"), ".abc 应被禁止")
		assert.False(t, IsAllowedExtension(".xyz"), "黑名单优先，.xyz 不应被允许")
		assert.True(t, IsBlockedExtension(".exe"), "原有 .exe 应保持禁止")
	})

	t.Run("大小写与空格容错", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", " .SVG , .HEIC ")

		assert.True(t, IsAllowedExtension(".SVG"), "大写 .SVG 应被允许")
		assert.True(t, IsAllowedExtension(".svg"), "小写 .svg 应被允许")
	})

	t.Run("不带点号自动补全", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", "tiff,avif")
		t.Setenv("DM_FILE_EXTRA_BLOCKED", "bin")

		assert.True(t, IsAllowedExtension(".tiff"), "不带点号的 tiff 应被自动补全并允许")
		assert.True(t, IsAllowedExtension(".avif"), "不带点号的 avif 应被自动补全并允许")
		assert.True(t, IsBlockedExtension(".bin"), "不带点号的 bin 应被自动补全并禁止")
	})

	t.Run("空环境变量不影响现有配置", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", "")
		t.Setenv("DM_FILE_EXTRA_BLOCKED", "")

		assert.True(t, IsAllowedExtension(".jpg"), ".jpg 应保持允许")
		assert.True(t, IsBlockedExtension(".exe"), ".exe 应保持禁止")
	})

	t.Run("黑名单中的扩展名加入白名单时被忽略", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", ".exe,.php")

		assert.False(t, IsAllowedExtension(".exe"), ".exe 在黑名单中，白名单设置应被忽略")
		assert.False(t, IsAllowedExtension(".php"), ".php 在黑名单中，白名单设置应被忽略")
	})

	t.Run("纯点号输入被忽略", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", ".,..,  ")

		assert.False(t, IsAllowedExtension("."), `"." 不应被加入白名单`)
		assert.False(t, IsAllowedExtension(".."), `".." 不应被加入白名单`)
		assert.NotContains(t, EffectiveAllowedExtensions(), ".")
		assert.NotContains(t, EffectiveAllowedExtensions(), "..")
	})

	t.Run("含路径分隔符的输入被忽略", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", "foo/bar,.svg")

		assert.False(t, IsAllowedExtension(".foo/bar"), "含路径分隔符的扩展名不应被加入白名单")
		assert.True(t, IsAllowedExtension(".svg"), "合法扩展名 .svg 应被允许")
	})

	t.Run("同一扩展名同时出现在两个 env var 中以黑名单为准", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", ".danger")
		t.Setenv("DM_FILE_EXTRA_BLOCKED", ".danger")

		assert.False(t, IsAllowedExtension(".danger"), "黑名单优先，.danger 不应被允许")
		assert.True(t, IsBlockedExtension(".danger"), ".danger 应被禁止")
		assert.NotContains(t, EffectiveAllowedExtensions(), ".danger",
			".danger 不应出现在有效白名单中")
	})

	t.Run("将已有白名单扩展名加入黑名单", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_BLOCKED", ".jpg")

		assert.True(t, IsBlockedExtension(".jpg"), ".jpg 应被禁止")
		assert.False(t, IsAllowedExtension(".jpg"), ".jpg 不应再被允许")
		assert.NotContains(t, EffectiveAllowedExtensions(), ".jpg",
			".jpg 应从有效白名单中移除")
	})

	t.Run("多连续点号的畸形输入被忽略", func(t *testing.T) {
		resetPolicyForTest(t)
		t.Setenv("DM_FILE_EXTRA_ALLOWED", "..exe,..svg")

		assert.False(t, IsAllowedExtension("..exe"), `"..exe" 不应被加入白名单`)
		assert.False(t, IsAllowedExtension("..svg"), `"..svg" 不应被加入白名单`)
	})
}

// ---------------------------------------------------------------------------
// system_setting 层
// ---------------------------------------------------------------------------

func TestPolicy_SettingsExtraBlockedTakesEffect(t *testing.T) {
	useSettings(t, fakePolicySettings{blocked: []string{".pdf"}, maxKB: common.DefaultFileMaxSizeKB})

	assert.True(t, IsBlockedExtension(".pdf"), "运营封堵的 .pdf 必须立即被拒")
	assert.False(t, IsAllowedExtension(".pdf"))
	assert.True(t, IsAllowedExtension(".png"), "未被封堵的扩展名不受影响")
}

func TestPolicy_SettingsExtraAllowedTakesEffect(t *testing.T) {
	useSettings(t, fakePolicySettings{allowed: []string{".dwg"}, maxKB: common.DefaultFileMaxSizeKB})

	assert.True(t, IsAllowedExtension(".dwg"))
	assert.Contains(t, EffectiveAllowedExtensions(), ".dwg")
}

// baseline 黑名单不可撤销：配置层无论写什么都进不了 allowed。这是本任务最重要
// 的安全不变量 —— 一个超管账号或一条直改库的 SQL 都不该能放开 .exe / .php。
func TestPolicy_BaselineBlocklistCannotBeOverridden(t *testing.T) {
	dangerous := []string{".exe", ".php", ".sh", ".bat", ".js", ".apk"}
	useSettings(t, fakePolicySettings{allowed: dangerous, maxKB: common.DefaultFileMaxSizeKB})

	for _, ext := range dangerous {
		assert.True(t, IsBlockedExtension(ext), "%s 必须仍被禁止", ext)
		assert.False(t, IsAllowedExtension(ext), "%s 绝不能被配置放开", ext)
		assert.NotContains(t, EffectiveAllowedExtensions(), ext,
			"%s 不得出现在下发给客户端的有效白名单里", ext)
	}
}

// 同一扩展名同时出现在配置的两端时，黑名单胜出 —— 与 env 层结论一致，
// 且因为派生是声明式集合运算，结果不依赖处理顺序。
func TestPolicy_BlockedWinsOverAllowedInSettings(t *testing.T) {
	useSettings(t, fakePolicySettings{
		allowed: []string{".dwg"},
		blocked: []string{".dwg"},
		maxKB:   common.DefaultFileMaxSizeKB,
	})
	assert.False(t, IsAllowedExtension(".dwg"))
	assert.True(t, IsBlockedExtension(".dwg"))
}

func TestPolicy_MaxUploadSizeFromSettings(t *testing.T) {
	useSettings(t, fakePolicySettings{maxKB: 2048})
	assert.Equal(t, int64(2048*1024), MaxUploadSize())
}

// 未挂 settings 的路径不经过 common 侧 clamp，这里再挡一次。
func TestPolicy_MaxUploadSizeRejectsOutOfRange(t *testing.T) {
	for _, bad := range []int{0, -1, common.FileMaxSizeKBHardCap + 1} {
		useSettings(t, fakePolicySettings{maxKB: bad})
		assert.Equal(t, MaxFileSize, MaxUploadSize(), "maxKB=%d 应回退默认值", bad)
	}
}

func TestPolicy_EffectiveAllowedExtensionsIsSortedAndExcludesBlocked(t *testing.T) {
	useSettings(t, fakePolicySettings{blocked: []string{".pdf"}, maxKB: common.DefaultFileMaxSizeKB})

	got := EffectiveAllowedExtensions()
	require.NotEmpty(t, got)
	assert.NotContains(t, got, ".pdf")
	for i := 1; i < len(got); i++ {
		assert.Less(t, got[i-1], got[i], "必须按字典序，保证下发内容稳定可比较")
	}
}

// ---------------------------------------------------------------------------
// 快照语义：复用与并发
// ---------------------------------------------------------------------------

// 输入不变时复用同一份快照，不为每个上传请求重建两张七十余项的 map。
func TestPolicy_SnapshotIsReusedWhenInputsUnchanged(t *testing.T) {
	useSettings(t, fakePolicySettings{maxKB: common.DefaultFileMaxSizeKB})
	assert.Same(t, currentPolicy(), currentPolicy())
}

// 配置变化后必须换出新快照，而不是原地改上一份（那正是改动前的 data race）。
func TestPolicy_SnapshotRebuildsWhenInputsChange(t *testing.T) {
	useSettings(t, fakePolicySettings{maxKB: common.DefaultFileMaxSizeKB})
	first := currentPolicy()

	SetPolicySettings(fakePolicySettings{blocked: []string{".pdf"}, maxKB: common.DefaultFileMaxSizeKB})
	second := currentPolicy()

	assert.NotSame(t, first, second)
	assert.False(t, first.blocked[".pdf"], "旧快照必须保持不可变")
	assert.True(t, second.blocked[".pdf"])
}

// 改动前 loadExtensionsFromEnv 原地写包级 map，任何运行期热改都是 data race；
// 这个用例在 -race 下守住新实现的并发安全。
func TestPolicy_ConcurrentReadsAndConfigChanges(t *testing.T) {
	useSettings(t, fakePolicySettings{maxKB: common.DefaultFileMaxSizeKB})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				IsAllowedExtension(".png")
				IsBlockedExtension(".exe")
				MaxUploadSize()
				EffectiveAllowedExtensions()
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 200; j++ {
			if j%2 == 0 {
				SetPolicySettings(fakePolicySettings{blocked: []string{".pdf"}, maxKB: 2048})
			} else {
				SetPolicySettings(fakePolicySettings{maxKB: common.DefaultFileMaxSizeKB})
			}
		}
	}()
	wg.Wait()
}

// IsBaselineBlockedExtension 只看**内置**黑名单，不含运营封堵项 —— 管理台写侧
// 用它判断「这个扩展名注定放不开」，而运营自己封的可以由运营自己解封。
func TestIsBaselineBlockedExtension(t *testing.T) {
	useSettings(t, fakePolicySettings{blocked: []string{".dwg"}, maxKB: common.DefaultFileMaxSizeKB})

	assert.True(t, IsBaselineBlockedExtension(".exe"))
	assert.True(t, IsBaselineBlockedExtension(".PHP"), "大小写不敏感")
	assert.False(t, IsBaselineBlockedExtension(".jpg"))
	assert.False(t, IsBaselineBlockedExtension(".dwg"),
		"运营封堵项不属于内置黑名单，运营可以自己解封")
	assert.True(t, IsBlockedExtension(".dwg"), "但当前生效的黑名单里有它")
}

// 放开任意扩展名（去掉候选集后的核心能力）：不发版、不重启。
func TestPolicy_AllowsArbitraryExtensionWithoutRedeploy(t *testing.T) {
	useSettings(t, fakePolicySettings{
		allowed: []string{".dwg", ".psd", ".step"},
		maxKB:   common.DefaultFileMaxSizeKB,
	})
	for _, ext := range []string{".dwg", ".psd", ".step"} {
		assert.True(t, IsAllowedExtension(ext), "%s 应可由配置放开", ext)
		assert.Contains(t, EffectiveAllowedExtensions(), ext)
	}
}

// TestExtensionPolicy_DeployedEnvValuesRemainEffective 用**部署环境实测到的**
// DM_FILE_EXTRA_ALLOWED 值做等价性验证（2026-08-26 取自各环境配置）。
//
// 等价性测试用 baseline 全集覆盖是「理论上不变」，这条是「按现网真实配置不变」。
// 迁移的承诺是 DB 未配置时行为逐字节相同，这两组值就是那个承诺的具体对象。
func TestExtensionPolicy_DeployedEnvValuesRemainEffective(t *testing.T) {
	cases := []struct {
		env string
		raw string
		// viaEnv 是**只有靠 env 才被允许**的扩展名：baseline 里没有它们，
		// 一旦 env 兼容层出问题，现网用户会立刻传不了这些文件。
		viaEnv []string
		// redundant 在 baseline 里本来就有，env 配置是历史冗余；即使 env 层
		// 完全失效它们也照样能传。
		redundant []string
	}{
		{
			env:       "A",
			raw:       ".tgz,.xlsm,.key,.numbers,.pages,.heic",
			viaEnv:    []string{".tgz", ".xlsm"},
			redundant: []string{".key", ".numbers", ".pages", ".heic"},
		},
		{
			env:    "B",
			raw:    ".tgz,.xlsm",
			viaEnv: []string{".tgz", ".xlsm"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			resetPolicyForTest(t)
			t.Setenv("DM_FILE_EXTRA_ALLOWED", tc.raw)
			t.Setenv("DM_FILE_EXTRA_BLOCKED", "")

			for _, ext := range tc.viaEnv {
				assert.True(t, IsAllowedExtension(ext),
					"%s 只靠 env 放开，迁移后必须仍被允许", ext)
				assert.False(t, allowedExtensions[ext],
					"%s 若已进 baseline，请更新本用例的分类", ext)
			}
			for _, ext := range tc.redundant {
				assert.True(t, IsAllowedExtension(ext), "%s 必须仍被允许", ext)
				assert.True(t, allowedExtensions[ext],
					"%s 应在 baseline 里；若被移出，它就变成只靠 env 撑着了", ext)
			}
			// 现网配置里没有任何一项命中内置黑名单 —— 若将来有人往 env 里加了
			// 黑名单项，它会被静默忽略（与改动前行为一致），这条守住那个前提。
			for _, ext := range append(append([]string{}, tc.viaEnv...), tc.redundant...) {
				assert.False(t, IsBaselineBlockedExtension(ext),
					"%s 不应命中内置黑名单，否则现网配置有一项从未生效过", ext)
			}
		})
	}
}

// 端到端版的「加一个格式不会丢掉原有的」（D9）：模拟现网 env 已放开若干格式、
// 运维在管理台又加了一项的场景，确认上传门对两批都放行。
func TestPolicy_AddingExtensionKeepsEnvOnesUploadable(t *testing.T) {
	resetPolicyForTest(t)
	// 配置层已完成 env ∪ DB 合并，这里给的就是合并后的结果。
	SetPolicySettings(fakePolicySettings{
		allowed: []string{".tgz", ".xlsm", ".dwg"},
		maxKB:   common.DefaultFileMaxSizeKB,
	})
	cached.Store(nil)

	for _, ext := range []string{".tgz", ".xlsm", ".dwg"} {
		assert.True(t, IsAllowedExtension(ext), "%s 必须可上传", ext)
	}
}

// 快照有效性判定不能靠「把输入拼成字符串指纹」：扩展名清洗只拒绝空 / "." /
// ".." / 含路径分隔符 / 含连续点的 token，其余字符（含分隔符本身）都合法，
// 所以任意拼接方案都可能碰撞。
//
// 这两组配置在旧的 "allowed|blocked|kb" 指纹下同为 ".a|.b|.pdf|102400"：
// 从前者切到后者时缓存命中旧快照，.pdf 实际仍可上传、appconfig 也仍下发旧清单，
// 而管理台已经返回 applied=true —— 紧急封堵静默失效。
func TestPolicy_SnapshotKeyDoesNotCollide(t *testing.T) {
	resetPolicyForTest(t)

	SetPolicySettings(fakePolicySettings{
		allowed: []string{".a"},
		blocked: []string{".b|.pdf"},
		maxKB:   common.DefaultFileMaxSizeKB,
	})
	cached.Store(nil)
	require.True(t, IsAllowedExtension(".pdf"), "前置条件：此时 .pdf 尚未被封")
	first := currentPolicy()

	// 切到第二组：.pdf 被真正封堵。
	SetPolicySettings(fakePolicySettings{
		allowed: []string{".a|.b"},
		blocked: []string{".pdf"},
		maxKB:   common.DefaultFileMaxSizeKB,
	})

	assert.NotSame(t, first, currentPolicy(), "输入变了就必须换出新快照")
	assert.True(t, IsBlockedExtension(".pdf"), "封堵必须生效，不得命中碰撞的旧快照")
	assert.False(t, IsAllowedExtension(".pdf"))
	assert.NotContains(t, EffectiveAllowedExtensions(), ".pdf",
		"appconfig 下发清单同样不得停留在旧快照")
}

// 反向：同样的碰撞组合，从「已封堵」切到「未封堵」也必须换快照。
func TestPolicy_SnapshotKeyDoesNotCollideReversed(t *testing.T) {
	resetPolicyForTest(t)

	SetPolicySettings(fakePolicySettings{
		allowed: []string{".a|.b"},
		blocked: []string{".pdf"},
		maxKB:   common.DefaultFileMaxSizeKB,
	})
	cached.Store(nil)
	require.True(t, IsBlockedExtension(".pdf"))

	SetPolicySettings(fakePolicySettings{
		allowed: []string{".a"},
		blocked: []string{".b|.pdf"},
		maxKB:   common.DefaultFileMaxSizeKB,
	})

	assert.False(t, IsBlockedExtension(".pdf"), "解封同样不得命中碰撞的旧快照")
	assert.True(t, IsAllowedExtension(".pdf"))
}

// 输入顺序不同视为不同输入（宁可多重建一次，也不要误判为命中）。
func TestPolicy_SnapshotDistinguishesOrder(t *testing.T) {
	resetPolicyForTest(t)

	SetPolicySettings(fakePolicySettings{allowed: []string{".a", ".b"}, maxKB: common.DefaultFileMaxSizeKB})
	cached.Store(nil)
	first := currentPolicy()

	SetPolicySettings(fakePolicySettings{allowed: []string{".b", ".a"}, maxKB: common.DefaultFileMaxSizeKB})
	assert.NotSame(t, first, currentPolicy())
}

// ---------------------------------------------------------------------------
// 上限展示精度（review P2）
//
// file.max_size_kb 接受任意 KB 值，而错误提示此前一律 bytes/1024/1024 整除成
// MB：配成 1536KB 时服务端实际放行 1.5MB，提示却说「不能超过 1MB」——
// 向客户端报告了一个服务端并不执行的上限。
// ---------------------------------------------------------------------------

func TestFormatSizeLimit(t *testing.T) {
	cases := []struct {
		name  string
		bytes int64
		want  string
	}{
		{"整数 MB 不带小数", 100 * 1024 * 1024, "100 MB"},
		{"非整数 MB 保留一位小数", 1536 * 1024, "1.5 MB"},
		{"1.2MB", 1229 * 1024, "1.2 MB"},
		{"不足 1MB 用 KB", 512 * 1024, "512 KB"},
		{"恰好 1MB", 1024 * 1024, "1 MB"},
		{"硬上限 512MB", 512 * 1024 * 1024, "512 MB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, FormatSizeLimit(tc.bytes))
		})
	}
}

// 这条是 P2 的直接回归：1536KB 的上限**不能**被展示成 1MB。
func TestFormatSizeLimit_DoesNotTruncateToWrongCap(t *testing.T) {
	const cap1536KB = 1536 * 1024
	assert.NotEqual(t, "1 MB", FormatSizeLimit(cap1536KB),
		"整除会报出一个服务端并不执行的上限")
	assert.Equal(t, "1.5 MB", FormatSizeLimit(cap1536KB))
}

func TestSizeLimitDetails(t *testing.T) {
	kb, mb := SizeLimitDetails(1536 * 1024)
	assert.EqualValues(t, 1536, kb, "max_size_kb 必须精确")
	assert.EqualValues(t, 1, mb, "max_mb 是历史字段，保持整除截断以兼容老客户端")

	kb, mb = SizeLimitDetails(100 * 1024 * 1024)
	assert.EqualValues(t, 102400, kb)
	assert.EqualValues(t, 100, mb)
}

// 配置任意 KB 值时，MaxUploadSize 与展示值必须一致对得上。
func TestPolicy_NonIntegralMBCapIsReportedExactly(t *testing.T) {
	useSettings(t, fakePolicySettings{maxKB: 1536})

	assert.Equal(t, int64(1536*1024), MaxUploadSize())
	assert.Equal(t, "1.5 MB", FormatSizeLimit(MaxUploadSize()))
	kb, _ := SizeLimitDetails(MaxUploadSize())
	assert.EqualValues(t, 1536, kb)
}
