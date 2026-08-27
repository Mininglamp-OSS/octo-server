package file

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 源码守卫（task file-extension-policy-dynamic-config）。
//
// 这两个不变量靠 code review 守不住 —— 改动前正是它们各自失守了一次：
// bot_api / robot 的 multipart 路径各自写了一份 `const maxSize = 100 * 1024 * 1024`
// 复制品，而 loadExtensionsFromEnv() 在 init() 里原地改写包级 map。

// readSourceWithoutComments 读源文件并剥掉行注释，避免注释里提到被禁的写法
// （本任务的注释里就大量引用了它们）触发误报。
func readSourceWithoutComments(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var clean strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}
	return clean.String()
}

// TestUploadSizeSingleSourceOfTruth 钉死「7 个大小检查点同源」。
//
// 生产路径一律走 MaxUploadSize()。直接引用 MaxFileSize 常量或再写一份
// `100 * 1024 * 1024` 字面量的代码不会跟随 system_setting 的 file.max_size_kb，
// 运营调小上限时那条路径会静默维持 100MB。
func TestUploadSizeSingleSourceOfTruth(t *testing.T) {
	// const.go 是 MaxFileSize 的定义处，故不在列表里。
	files := []string{
		"api.go",
		"../bot_api/file.go",
		"../robot/api.go",
	}
	banned := map[string]string{
		"MaxFileSize":       "请改用 MaxUploadSize()（policy.go）；MaxFileSize 只是代码默认值",
		"100 * 1024 * 1024": "不要再复制一份大小上限字面量；用 MaxUploadSize()",
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			src := readSourceWithoutComments(t, f)
			for pattern, why := range banned {
				if strings.Contains(src, pattern) {
					t.Errorf("%s 含被禁写法 %q：%s", f, pattern, why)
				}
			}
		})
	}
}

// TestExtensionBaselineIsReadOnly 钉死「baseline 两张 map 只读」。
//
// 运行期写入它们就是 data race（多个 HTTP handler 并发读），也正是改动前
// loadExtensionsFromEnv() 自陈「不可在运行时重复调用」的原因。策略变更必须
// 通过 policy.go 换出一份新的不可变快照。
func TestExtensionBaselineIsReadOnly(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	// 匹配 `allowedExtensions[...] = ...` / `delete(blockedExtensions, ...)` /
	// `allowedExtensions = ...`（整体重新赋值）。map 字面量初始化不会命中，
	// 因为那是 `var allowedExtensions = map[string]bool{`，带 var 前缀。
	writes := regexp.MustCompile(`(?m)(^|[^.\w])(allowed|blocked)Extensions\s*\[[^\]]*\]\s*=|delete\(\s*(allowed|blocked)Extensions|(?m)^\s*(allowed|blocked)Extensions\s*=`)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if name == "const.go" {
			// 定义处：`var allowedExtensions = map[string]bool{...}` 是初始化，
			// 不是运行期写入；正则的 `^\s*xxx =` 分支会误报，单独跳过。
			continue
		}
		src := readSourceWithoutComments(t, name)
		if loc := writes.FindString(src); loc != "" {
			t.Errorf("%s 试图写入只读 baseline（%q）；请改用 policy.go 的快照派生", name, strings.TrimSpace(loc))
		}
	}
}

// TestExtensionBaselineHasNoInitMutation 单独盯住 const.go：定义处允许 map
// 字面量，但不允许再出现 init() 里的运行期改写。
func TestExtensionBaselineHasNoInitMutation(t *testing.T) {
	src := readSourceWithoutComments(t, "const.go")
	for _, banned := range []string{
		"func init()",
		"loadExtensionsFromEnv",
		"delete(allowedExtensions",
		"delete(blockedExtensions",
	} {
		if strings.Contains(src, banned) {
			t.Errorf("const.go 含被禁写法 %q：baseline 必须保持只读，运行期策略在 policy.go", banned)
		}
	}
}

// TestExtensionGateParityAcrossModules 是 handler 行为测试的**补充**，不是替代。
//
// 真正证明「每个上传入口都过同一道门」的是各模块的行为测试：
//   - modules/bot_api/file_extension_gate_test.go
//   - modules/robot/file_extension_gate_test.go
//   - modules/file/policy_integration_test.go（管理台写入 → 上传门变化）
//
// 它们直驱 handler 并断言被拒时 UploadFile **没有被调用** —— 拦住的是字节落进
// 对象存储，不是状态码。
//
// 本用例只做一件源码扫描做得到而行为测试做不到的事：**盯住入口数量**。
// 新增一条上传路径时行为测试不会自动失败（没人给它写用例），而这里的计数会。
// 初版守卫只有计数、没有行为测试，结果 bot_api / robot 的 multipart 路径压根
// 没有扩展名门也照样 PASS —— 计数验证的是「调用点没变少」，不是「每个入口都有门」。
func TestExtensionGateParityAcrossModules(t *testing.T) {
	entries := []struct {
		path      string
		mustCall  []string
		wantGates int
		gateNote  string
	}{
		// multipart 上传 + 预签名签发。
		{path: "api.go", mustCall: []string{"IsAllowedExtension(", "IsBlockedExtension("},
			wantGates: 2, gateNote: "uploadFile / getUploadCredentials"},
		// multipart 上传 + STS credentials + 预签名签发。
		{path: "../bot_api/file.go", mustCall: []string{"file.IsAllowedExtension(", "file.IsBlockedExtension("},
			wantGates: 3, gateNote: "botUploadFile / botUploadCredentials / botUploadPresigned"},
		{path: "../robot/api.go", mustCall: []string{"file.IsAllowedExtension(", "file.IsBlockedExtension("},
			wantGates: 3, gateNote: "botUploadFile / botUploadCredentials / botUploadPresigned"},
	}
	for _, e := range entries {
		t.Run(e.path, func(t *testing.T) {
			src := readSourceWithoutComments(t, e.path)
			for _, call := range e.mustCall {
				if !strings.Contains(src, call) {
					t.Errorf("%s 必须调用 %s —— 扩展名门不得在各模块自行实现", e.path, call)
				}
			}
			got := strings.Count(src, e.mustCall[0])
			if got != e.wantGates {
				t.Errorf("%s 的 %s 调用点数量变了：want %d (%s), got %d。"+
					"新增上传入口时请一并更新本守卫，并给新入口补一条 handler 行为测试"+
					"（断言被拒时 UploadFile 未被调用）",
					e.path, e.mustCall[0], e.wantGates, e.gateNote, got)
			}
			for _, banned := range []string{`map[string]bool{".`, `[]string{".jpg"`} {
				if strings.Contains(src, banned) {
					t.Errorf("%s 疑似自建扩展名清单（%q）；请统一走 modules/file 的策略快照", e.path, banned)
				}
			}
		})
	}
}

// TestNewMountsPolicySettingsSource 是 TestNewMountsPolicySettings 的零 infra
// 兜底：装配那一行断了，动态配置整体失效且没有任何报错，值得两道守卫。
func TestNewMountsPolicySettingsSource(t *testing.T) {
	src := readSourceWithoutComments(t, "api.go")
	idx := strings.Index(src, "func New(ctx *config.Context) *File {")
	if idx < 0 {
		t.Fatal("找不到 File.New")
	}
	end := strings.Index(src[idx:], "\n}\n")
	if end < 0 {
		t.Fatal("无法界定 File.New 函数体")
	}
	if !strings.Contains(src[idx:idx+end], "SetPolicySettings(") {
		t.Error("File.New 必须调用 SetPolicySettings(settings)，" +
			"否则 policy.go 一直走「未挂载」分支，管理台写入 file.* 对任何上传入口都不生效")
	}
}

// TestSizeLimitIsNotTruncatedForDisplay 钉死「不要再把字节上限整除成 MB 报给用户」。
//
// file.max_size_kb 接受任意 KB 值；`bytes/1024/1024` 会把 1536KB 报成 1MB ——
// 一个服务端并不执行的上限。展示走 FormatSizeLimit，结构化详情走
// SizeLimitDetails（max_size_kb 精确，max_mb 仅兼容）。
func TestSizeLimitIsNotTruncatedForDisplay(t *testing.T) {
	files := []string{"api.go", "../bot_api/file.go", "../robot/api.go", "../bot_api/api_i18n.go", "../robot/api_i18n.go"}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			src := readSourceWithoutComments(t, f)
			for _, banned := range []string{"/1024/1024", "/ 1024 / 1024"} {
				if strings.Contains(src, banned) {
					t.Errorf("%s 仍在把字节上限整除成 MB（%q）；"+
						"展示用 file.FormatSizeLimit，详情用 file.SizeLimitDetails", f, banned)
				}
			}
		})
	}
}

// TestPolicySnapshotMapsAreNotMutated 钉死「快照的两张 map 只读」。
//
// extPolicy 被多个 goroutine 并发读，任何下标写入都是 data race —— 那正是
// 改动前 loadExtensionsFromEnv 干的事。Go 挡不住同包内对结构体字段的写入，
// 所以用源码扫描补这一刀；读请一律走 isAllowed / isBlocked。
func TestPolicySnapshotMapsAreNotMutated(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	// 匹配 `X.allowed[...] = ` / `delete(X.blocked, ...)` / `X.allowed = `。
	writes := regexp.MustCompile(`\.(allowed|blocked)\s*\[[^\]]*\]\s*=|delete\(\s*\w+\.(allowed|blocked)|\.(allowed|blocked)\s*=[^=]`)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src := readSourceWithoutComments(t, name)
		// derivePolicy 构造快照时给字段赋值是合法的（此时还没发布出去）。
		if name == "policy.go" {
			if idx := strings.Index(src, "func derivePolicy"); idx >= 0 {
				if end := strings.Index(src[idx:], "\n}\n"); end >= 0 {
					src = src[:idx] + src[idx+end:]
				}
			}
		}
		if loc := writes.FindString(src); loc != "" {
			t.Errorf("%s 试图写入已发布的快照 map（%q）；快照不可变，读走 isAllowed/isBlocked",
				name, strings.TrimSpace(loc))
		}
	}
}
