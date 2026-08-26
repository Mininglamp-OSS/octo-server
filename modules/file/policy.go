package file

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/Mininglamp-OSS/octo-server/modules/common"
)

// ---------------------------------------------------------------------------
// 上传策略快照（task file-extension-policy-dynamic-config）
//
// 改动前：init() 读 env 后**直接改写包级 allowedExtensions / blockedExtensions
// 两张 map**（const.go 自陈「不可在运行时重复调用，map 无并发写保护」）。改配置
// 要重启全部 pod，且任何运行期热改都是 data race。
//
// 改动后：baseline 两张 map 变成只读，运行期值由 extPolicy **不可变快照**承载，
// 经 atomic.Pointer 原子发布。策略变更 = 换一个新快照，从不原地改 map。
//
// 派生是**声明式**的：
//
//	blocked = baseBlocked ∪ extraBlocked
//	allowed = (baseAllowed ∪ extraAllowed) − blocked
//
// 这一并消掉了改动前 env 逻辑的顺序依赖（先写白名单再 delete 黑名单项）——
// 同一个扩展名同时出现在两边时，结果由集合运算决定而不是由代码执行顺序决定，
// 与改动前「黑名单优先」的结论一致（const_test.go:TestLoadExtensionsFromEnv）。
//
// **baseBlocked 不可撤销**：它在减号右边，配置层无论写什么都进不了 allowed。
// 这是内置黑名单（.exe/.sh/.php/...）的安全保证，由本层而非配置层负责 ——
// modules/common 不能 import modules/file，无从知道 baseline 内容。
// ---------------------------------------------------------------------------

// PolicySettings 是 modules/file 用到的 SystemSettings 子集。定义在使用端
// （同 stickerSystemSettings 的理由）让测试可以注入内存 fake，无需 MySQL/Redis
// 起 test server；生产用 *common.SystemSettings 天然实现。
type PolicySettings interface {
	// FileExtraAllowedExtensions 已在 common 侧完成 DB→env 回退与内置黑名单过滤。
	FileExtraAllowedExtensions() []string
	// FileExtraBlockedExtensions 已在 common 侧完成 env ∪ DB 合并。
	FileExtraBlockedExtensions() []string
	FileMaxSizeKB() int
}

// extPolicy 是一份不可变的策略快照。构造后**不得修改**任何字段，
// 包括两张 map 与两个输入切片 —— 它被多个 goroutine 并发读。
type extPolicy struct {
	allowed map[string]bool
	blocked map[string]bool
	// maxSize 是单文件上限（字节）。
	maxSize int64
	// inAllowed / inBlocked / inMaxKB 是派生输入的副本，用来判断本快照是否
	// 仍然有效（输入不变则直接复用，不为每个上传请求重建两张七十余项的 map）。
	//
	// 用切片逐项比较，**不要**把输入拼成一个字符串指纹：扩展名的清洗规则只拒绝
	// 空 / "." / ".." / 含路径分隔符 / 含连续点的 token，任何其它字符（包括分隔
	// 符本身）都是合法的，所以任意拼接方案都可能碰撞。曾用 "allowed|blocked|kb"
	// 拼接，于是
	//
	//	allowed=[".a"]    blocked=[".b|.pdf"]  →  ".a|.b|.pdf|102400"
	//	allowed=[".a|.b"] blocked=[".pdf"]     →  ".a|.b|.pdf|102400"
	//
	// 两组配置指纹相同：从前者切到后者时缓存命中旧快照，`.pdf` 实际仍可上传、
	// appconfig 也仍下发旧清单，而管理台已经返回 applied=true。紧急封堵在这种
	// 组合下会静默失效。见 TestPolicy_SnapshotKeyDoesNotCollide。
	inAllowed []string
	inBlocked []string
	inMaxKB   int
}

// isAllowed / isBlocked 是访问快照的**唯一推荐方式**。两张 map 是共享只读的，
// 被多个 goroutine 并发读；直接下标写入就是 data race，也正是改动前
// loadExtensionsFromEnv 干的事。Go 挡不住同包内对字段的写入，所以另有一道源码
// 守卫（upload_policy_guard_test.go:TestPolicySnapshotMapsAreNotMutated）。
func (p *extPolicy) isAllowed(ext string) bool { return p.allowed[ext] }
func (p *extPolicy) isBlocked(ext string) bool { return p.blocked[ext] }

// matches 报告本快照是否由这组输入派生而来。
func (p *extPolicy) matches(extraAllowed, extraBlocked []string, maxKB int) bool {
	return p.inMaxKB == maxKB &&
		slices.Equal(p.inAllowed, extraAllowed) &&
		slices.Equal(p.inBlocked, extraBlocked)
}

type policyProvider struct{ settings PolicySettings }

var (
	// provider 承载 SystemSettings。生产路径经 New(ctx) 挂载；未挂载时
	// currentPolicy 回落到 env + baseline，与改动前逐字节等价（老 unit test
	// 直接构造 &File{} 不走 New，同 stickerLimits() 的 nil-safe 范式）。
	provider atomic.Pointer[policyProvider]
	// cached 是最近一次派生结果。仅当输入指纹变化时才重建。
	cached atomic.Pointer[extPolicy]
)

// SetPolicySettings 挂载配置源。由 New(ctx) 在模块初始化时调用一次。
func SetPolicySettings(s PolicySettings) {
	provider.Store(&policyProvider{settings: s})
}

// policyInputs 收集本次派生的输入。settings 未挂载时读 env —— env 解析复用
// modules/common 的导出实现，不在本包镜像一份，避免两边漂移。
func policyInputs() (extraAllowed, extraBlocked []string, maxKB int) {
	if p := provider.Load(); p != nil && p.settings != nil {
		return p.settings.FileExtraAllowedExtensions(),
			p.settings.FileExtraBlockedExtensions(),
			p.settings.FileMaxSizeKB()
	}
	return common.FileExtraAllowedFromEnv(),
		common.FileExtraBlockedFromEnv(),
		common.DefaultFileMaxSizeKB
}

// currentPolicy 返回当前生效的策略快照。
func currentPolicy() *extPolicy {
	extraAllowed, extraBlocked, maxKB := policyInputs()
	if p := cached.Load(); p != nil && p.matches(extraAllowed, extraBlocked, maxKB) {
		return p
	}
	next := derivePolicy(extraAllowed, extraBlocked, maxKB)
	// 竞态下多个 goroutine 可能同时派生同一份快照：结果逐字节相同，最后一个
	// Store 胜出即可，无需加锁串行化。
	cached.Store(next)
	return next
}

// derivePolicy 执行集合运算。maxKB 由 common 侧 clamp 过；这里再挡一次 ≤0，
// 因为未挂载 settings 的路径（老 unit test）不经过那层 clamp。
func derivePolicy(extraAllowed, extraBlocked []string, maxKB int) *extPolicy {
	blocked := make(map[string]bool, len(blockedExtensions)+len(extraBlocked))
	for ext := range blockedExtensions {
		blocked[ext] = true
	}
	for _, ext := range extraBlocked {
		blocked[ext] = true
	}

	allowed := make(map[string]bool, len(allowedExtensions)+len(extraAllowed))
	for ext := range allowedExtensions {
		if blocked[ext] {
			continue
		}
		allowed[ext] = true
	}
	for _, ext := range extraAllowed {
		// 内置/额外黑名单永远压过放开 —— 与改动前 loadExtensionsFromEnv 的
		// 「DM_FILE_EXTRA_ALLOWED 里命中黑名单的项被忽略」行为一致。
		if blocked[ext] {
			continue
		}
		allowed[ext] = true
	}

	if maxKB <= 0 || maxKB > common.FileMaxSizeKBHardCap {
		maxKB = common.DefaultFileMaxSizeKB
	}
	return &extPolicy{
		allowed: allowed,
		blocked: blocked,
		maxSize: int64(maxKB) * 1024,
		// 存副本：调用方（common 的 getter）每次返回新切片，但快照是共享只读的，
		// 不该持有外部可能改写的底层数组。
		inAllowed: slices.Clone(extraAllowed),
		inBlocked: slices.Clone(extraBlocked),
		inMaxKB:   maxKB,
	}
}

// IsAllowedExtension 检查文件扩展名是否允许上传。
// 签名保持不变：modules/bot_api 与 modules/robot 的调用点无需改动。
func IsAllowedExtension(ext string) bool {
	p := currentPolicy()
	ext = strings.ToLower(ext)
	if p.isBlocked(ext) {
		return false
	}
	return p.isAllowed(ext)
}

// IsBlockedExtension 检查文件扩展名是否被禁止。
func IsBlockedExtension(ext string) bool {
	return currentPolicy().isBlocked(strings.ToLower(ext))
}

// MaxUploadSize 返回当前生效的单文件上限（字节）。
//
// **这是全部 7 个大小检查点的唯一真源**：multipart 校验、multipart body reader、
// 预签名签发，以及 bot_api / robot 各自的 multipart 与预签名路径。改动前那三条
// 路径分别读 file.MaxFileSize 和两份本地 `const maxSize`，配置调小时 bot / robot
// 的 multipart 上传不会跟着收紧。
func MaxUploadSize() int64 {
	return currentPolicy().maxSize
}

// EffectiveAllowedExtensions 返回当前生效的允许扩展名（含前导点、小写、字典序）。
// 供 /v1/common/appconfig 下发给客户端做上传前预校验。
//
// 只下发 allowed，不下发 blocked：客户端只需要知道能传什么；下发黑名单等于让
// 任何未认证调用方对比 baseline 就看出本部署额外封了哪些扩展名。
func EffectiveAllowedExtensions() []string {
	p := currentPolicy()
	out := make([]string, 0, len(p.allowed))
	for ext := range p.allowed {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// IsBaselineBlockedExtension 报告扩展名是否在**内置**黑名单里。
//
// 与 IsBlockedExtension 的区别：后者是当前生效的黑名单（内置 ∪ 运营封堵），
// 会随配置变化；本函数只看不可撤销的那部分。管理台写侧用它拒绝「把内置黑名单
// 项写进 extra_allowed」这种注定不生效的配置。
func IsBaselineBlockedExtension(ext string) bool {
	return blockedExtensions[strings.ToLower(ext)]
}

func init() {
	// 把有效上传限制注册给 modules/common 的 appconfig。依赖倒置：有效值的
	// 计算需要 baseline（file 包的知识），而 common 不能 import file。
	// 放在 init() 而不是 New(ctx) —— 只要 file 包被链接进来就一定可用，
	// appconfig 不会因为模块构造顺序而拿到空值。
	common.SetFileUploadLimitsProvider(func() (int, []string) {
		p := currentPolicy()
		return int(p.maxSize / 1024), EffectiveAllowedExtensions()
	})
	// 同上，把「哪些扩展名永远不可放开」告诉配置层，让管理台写侧能当场拒绝。
	common.SetBuiltinBlockedFileExtensionProbe(IsBaselineBlockedExtension)
}

// FormatSizeLimit 把字节上限渲染成给人看的字符串（"100 MB" / "1.5 MB" / "512 KB"）。
//
// 存在的理由：file.max_size_kb 接受任意 KB 值，而错误提示此前一律按
// `bytes/1024/1024` 整除成 MB —— 配成 1536KB 时服务端实际放行 1.5MB，
// 提示却说「不能超过 1MB」，客户端据此告诉用户的上限是错的。
//
// 规则：不足 1MB 显示 KB；整数 MB 不带小数；否则保留一位小数。
func FormatSizeLimit(bytes int64) string {
	const kb = 1024
	const mb = 1024 * kb
	if bytes < mb {
		return fmt.Sprintf("%d KB", bytes/kb)
	}
	if bytes%mb == 0 {
		return fmt.Sprintf("%d MB", bytes/mb)
	}
	return fmt.Sprintf("%.1f MB", float64(bytes)/float64(mb))
}

// SizeLimitDetails 是上限相关错误的结构化详情。
//
// max_size_kb 是**精确**值，客户端应当用它做判断；max_mb 是历史字段，按整除
// 截断，只为兼容已经在读它的客户端而保留 —— 新代码不要用它做判断。
func SizeLimitDetails(bytes int64) (maxSizeKB int64, maxMB int64) {
	return bytes / 1024, bytes / 1024 / 1024
}
