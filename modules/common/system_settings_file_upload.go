package common

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
)

// ---------------------------------------------------------------------------
// 文件上传策略（task file-extension-policy-dynamic-config）
//
// 三个键把「扩展名白/黑名单」与「单文件大小上限」从 modules/file 的进程级
// init()+包级 map / 散落三处的硬编码常量搬到 system_setting，让运维能在不重启
// pod 的前提下紧急封堵一个扩展名。
//
// 本文件是**配置层**：负责 DB 快照读、env 兼容回退、语法清洗与硬上限 clamp。最终的策略语义（扣除内置黑名单、与 baseline 求并）在
// modules/file/policy.go —— 内置黑名单是 file 包的知识，且 common 不能 import
// file（file 已 import common）。
//
// 两个扩展名键**都是 env ∪ DB 的并集**（brief D1/D9）：
//
//	extra_blocked : env ∪ DB —— 封堵 A 不会静默解封 env 里已封的 B。
//	extra_allowed : env ∪ DB —— 放开 A 不会静默作废 env 里已放开的 B。
//
// extra_allowed 早期用的是「DB 覆盖 env」，理由是覆盖能提供「从管理台撤销 env
// 放开项」的能力。落地核对现网配置时否掉了：生产 env 里放开着几个格式，运维
// 想再加一个时若在管理台只填新的那个，原有格式**当场失效**、用户立刻传不了，
// 而他不会想到这一层。加一个格式是高频操作，作废一批格式是事故。
//
// 撤销能力没有丢：黑名单优先级最高，要收回某个 env 放开项，把它写进
// extra_blocked 即可。规矩因此统一成一句话 ——
// **「允许」栏只管加，「禁止」栏只管减**，两栏都不会误伤对方已有的配置。
// ---------------------------------------------------------------------------

const (
	// envFileExtraAllowed / envFileExtraBlocked 是迁移前就在生产使用的 env，
	// 保留为兼容回退层。DB 未配置时解析结果必须与迁移前逐字节相同 —— 同
	// threadAutoArchiveDaysFromEnv 的约束：env 兼容层不得重新解释既有部署的
	// 配置语义。
	envFileExtraAllowed = "DM_FILE_EXTRA_ALLOWED"
	envFileExtraBlocked = "DM_FILE_EXTRA_BLOCKED"

	// DefaultFileMaxSizeKB 是单文件上限的代码默认值，等于迁移前 modules/file
	// 的 MaxFileSize 常量（100MB）。这个键**没有 env 层**：迁移前它就是硬编码
	// 常量，没有对应 env，所以回退链只有 DB → code default 两层。
	DefaultFileMaxSizeKB = 100 * 1024

	// envFileMaxSizeKBHardCap 让部署自己决定单文件上限的**天花板**。
	// 新增 env 一律走 OCTO_ 前缀；既有的 DM_FILE_EXTRA_* 保持原名不动，
	// 改名会让现网 configmap 里的配置当场失效。
	envFileMaxSizeKBHardCap = "OCTO_FILE_MAX_SIZE_KB_HARD_CAP"

	// defaultFileMaxSizeKBHardCap 是未配置 env 时的天花板（512MB）。
	defaultFileMaxSizeKBHardCap = 512 * 1024

	// fileMaxSizeKBAbsoluteMax 只防溢出，不表达策略：maxKB 会以
	// int64(maxKB)*1024 转成字节，一个荒谬的 env 值会让它绕回负数，
	// 而负的上限意味着**所有上传都被拒**。100GB 远超任何真实附件场景，
	// 又离溢出边界足够远。
	fileMaxSizeKBAbsoluteMax = 100 * 1024 * 1024
)

// ---------------------------------------------------------------------------
// 为什么放开方向没有「候选集」白名单
//
// 早期设计里 extra_allowed 只能命中一张代码内候选集，理由是「放开一个扩展名 =
// 同时跳过内容校验」(ValidateMagicNumber 对没有魔数定义的扩展名直接返回 true)。
// 落地评审时否掉了：
//
//  1. 那道保险防不住主要风险 —— 浏览器可渲染类型里最危险的 .html / .htm
//     **本来就在内置允许集里**(modules/file/const.go)，口子今天就是敞的，
//     不是动态化引入的。
//  2. 真正的安全边界是**内置黑名单不可撤销**，那是另一层(modules/file/policy.go
//     的派生把 baseBlocked 放在减号右边)，与候选集无关。去掉候选集，
//     .exe / .php / .sh 照样开不了。
//  3. 需求本身就是「不重启放开一个文件格式」。候选集把它变成「仍要发一次版」，
//     等于没解决问题。
//
// 取而代之的两道约束：
//   - 读侧：内置黑名单永远压过放开（modules/file/policy.go）。
//   - 写侧：命中内置黑名单的 token 直接拒绝写入（见 fileBlockedExtensionProbe），
//     让运维当场看到错误，而不是写进去后发现不生效。
// ---------------------------------------------------------------------------

// NormalizeFileExtension 清洗单个扩展名 token，非法输入返回 ""。
//
// 语义与迁移前 modules/file/const.go:loadExtensionsFromEnv 内联的 normalizeExt
// **逐字节一致**（去空格、转小写、补前导点、丢弃 "." / ".." / 含路径分隔符 /
// 补全后仍含 ".." 的畸形输入），因为 env 层必须保持既有部署的解析结果不变。
// 导出是为了让 modules/file 直接复用同一份实现，而不是镜像一份 —— 镜像会漂移。
func NormalizeFileExtension(raw string) string {
	ext := strings.ToLower(strings.TrimSpace(raw))
	if ext == "" || ext == "." || ext == ".." {
		return ""
	}
	if strings.ContainsAny(ext, `/\`) {
		return ""
	}
	// 控制字符与内部空白一律拒绝。这比迁移前的清洗**更严**（原实现只挡路径
	// 分隔符与连续点），刻意如此：这类 token 永远匹配不上真实上传 —— 上传侧的
	// 扩展名取自 filepath.Ext(sanitizeFilename(...))，而 sanitizeFilename 已把
	// 控制字符替换成 "_" —— 但它们会原样进入 /v1/common/appconfig 下发给客户端
	// 的清单。死配置不该出现在对外契约里。
	//
	// 收紧 env 兼容层需要确认现网不受影响：部署环境实测的两组值
	// （.tgz,.xlsm,.key,.numbers,.pages,.heic 与 .tgz,.xlsm）均不含此类字符，
	// 见 modules/file:TestExtensionPolicy_DeployedEnvValuesRemainEffective。
	for _, r := range ext {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return ""
		}
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	// 过滤 "..exe" 这类多连续点号的畸形输入：补全后仍含 ".." 则无效。
	if strings.Contains(ext, "..") {
		return ""
	}
	return ext
}

// ParseFileExtensionCSV 把逗号分隔的扩展名列表清洗成去重后的有序切片。
// 非法 token 静默丢弃（调用方按需记日志）；返回值顺序为输入顺序，稳定可比较。
func ParseFileExtensionCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		ext := NormalizeFileExtension(p)
		if ext == "" {
			continue
		}
		if _, dup := seen[ext]; dup {
			continue
		}
		seen[ext] = struct{}{}
		out = append(out, ext)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FileMaxSizeKBHardCap 返回单文件上限的天花板（KB）。
//
// 这是**部署容量决策**，不是产品语义边界，所以它跟着部署配置走而不是编译进
// 二进制：能传多大取决于 ingress 的 body 限制、临时文件的磁盘、带宽和对象
// 存储后端，每个部署都不一样。对比 sticker 那组硬上限——「贴纸该多大」在任何
// 部署下都一样，那种才适合写死。
//
// 为什么是 env 而不是管理台：天花板若也能从管理台改，超管就能先抬天花板、
// 再抬实际值，这道闸等于不存在。env 要重启 Pod，这个成本本身就是防线。
//
// 职责因此分成三层：
//
//	开发   代码默认值（512MB），未配置时兜底
//	运维   OCTO_FILE_MAX_SIZE_KB_HARD_CAP 定天花板（改 configmap + 重启）
//	超管   file.max_size_kb 调实际值（管理台，≤60s 生效，超不过天花板）
//
// 非法值（非数字 / ≤0 / 超过防溢出上界）回落代码默认值，而不是被原样服务。
func FileMaxSizeKBHardCap() int {
	raw := strings.TrimSpace(os.Getenv(envFileMaxSizeKBHardCap))
	if raw == "" {
		return defaultFileMaxSizeKBHardCap
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 || v > fileMaxSizeKBAbsoluteMax {
		return defaultFileMaxSizeKBHardCap
	}
	return v
}

// FileExtraAllowedFromEnv 解析 DM_FILE_EXTRA_ALLOWED。
// 供 SystemSettings 的回退层与 modules/file 的 nil-settings 回落路径共用。
func FileExtraAllowedFromEnv() []string {
	return ParseFileExtensionCSV(os.Getenv(envFileExtraAllowed))
}

// FileExtraBlockedFromEnv 解析 DM_FILE_EXTRA_BLOCKED。
func FileExtraBlockedFromEnv() []string {
	return ParseFileExtensionCSV(os.Getenv(envFileExtraBlocked))
}

// FileExtraAllowedExtensions 返回**额外放开**的扩展名（每项含前导点、小写、
// 字典序）。语义是 env ∪ DB（D9）：只增不减，管理台写入不会作废 env 里已放开
// 的格式。要收回某一项，把它写进 extra_blocked（黑名单优先级最高）。
//
// 返回值里会过滤掉内置黑名单项：正常写入路径已在写侧拦下它们，这里是给直改库
// 的旁路兜底，同时保证管理台 effective_value 不会显示一个「写着但不生效」的
// 扩展名 —— 那比不显示更误导。
//
// 本键是**追加**语义，空值 = 不追加 = baseline 全集，天然不会 dark-close
// （对比 StickerUploadAllowedFormats：那个键**替换**整个允许集，全部非法时必须
// 回退默认集才不至于把功能暗关）。
func (s *SystemSettings) FileExtraAllowedExtensions() []string {
	merged := make(map[string]struct{})
	for _, ext := range FileExtraAllowedFromEnv() {
		merged[ext] = struct{}{}
	}
	if raw, ok := s.lookup("file", "extra_allowed_extensions"); ok {
		for _, ext := range ParseFileExtensionCSV(raw) {
			merged[ext] = struct{}{}
		}
	}
	out := make([]string, 0, len(merged))
	for ext := range merged {
		if IsBuiltinBlockedFileExtension(ext) {
			continue
		}
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// FileExtraBlockedExtensions 返回**额外封堵**的扩展名，语义是 env ∪ DB（D1）。
// 只增不减：DB 写入不会解封 env 里已封的项。返回值已去重并按字典序排序，
// 让 effective_value 回显稳定。
func (s *SystemSettings) FileExtraBlockedExtensions() []string {
	merged := make(map[string]struct{})
	for _, ext := range FileExtraBlockedFromEnv() {
		merged[ext] = struct{}{}
	}
	if raw, ok := s.lookup("file", "extra_blocked_extensions"); ok {
		for _, ext := range ParseFileExtensionCSV(raw) {
			merged[ext] = struct{}{}
		}
	}
	if len(merged) == 0 {
		return nil
	}
	out := make([]string, 0, len(merged))
	for ext := range merged {
		out = append(out, ext)
	}
	sort.Strings(out)
	return out
}

// FileMaxSizeKB 返回单文件上传上限（KB）。回退链只有 DB → code default 两层
// （见 DefaultFileMaxSizeKB 的说明）。
//
// 越界处理与 sticker 那组键**共用同一个钳位器**：≤0 视为未配置、回落默认值；
// 超过硬上限则钳到硬上限，而不是回落默认值 —— 后者会让运维填 600000（想要
// ~586MB）反而拿到 100MB，比编辑前还小、比键上写明的 512MB 还小，且写侧
// Positive:true 跳过了范围检查，唯一的信号是 effective_value 悄悄读回 102400。
// 那正是本任务在 extra_allowed_extensions 上明确拒绝的「存下了但不生效」。
//
// 越界 Warn 由钳位器按 (key, value) 去重：本 getter 在 policyInputs() 里，
// 每次 currentPolicy() 都会走 —— 包括每个未认证的 /v1/common/appconfig 请求。
//
// 未配置的那一格刻意不走钳位器：天花板可以低于代码默认值
// （OCTO_FILE_MAX_SIZE_KB_HARD_CAP < 100MB），把默认值喂进去会报
// configured=102400，而 system_setting 表里一行都没有 —— 那是在诬告一次没有
// 发生过的变更，且每个 pod 都会打一条。回落值本身仍然要过天花板。
func (s *SystemSettings) FileMaxSizeKB() int {
	hardCap := FileMaxSizeKBHardCap()
	v, configured := s.getIntOK("file", "max_size_kb")
	if !configured {
		return min(DefaultFileMaxSizeKB, hardCap)
	}
	return s.clampIntUpper("file.max_size_kb", v, DefaultFileMaxSizeKB, hardCap)
}

// ---------------------------------------------------------------------------
// 跨键组合约束：file.max_size_kb vs sticker.upload_max_size_kb（brief D6）
//
// 上传校验里全局大小门在前、贴纸门在后（modules/file/api.go）。所以把
// file.max_size_kb 调到低于 sticker.upload_max_size_kb 时，贴纸会**永远传不
// 上去**，而两个键各自看都完全合法。这类「各自合法、组合致命」的配置，本仓
// 已有先例（thread.auto_archive_days vs sidebar.recent_filter_thread_days），
// 处理方式同样是写侧 merge-then-validate：校验 merge(当前快照, 入参)，而不是
// 任一半。
// ---------------------------------------------------------------------------

// FileStickerSizeOrdering 是参与组合校验的两个上限值（KB）。
type FileStickerSizeOrdering struct {
	FileMaxSizeKB    int
	StickerMaxSizeKB int
}

// FileStickerSizeOrdering 读当前快照的组合值，供写侧守卫比较。
//
// 贴纸侧取的是 stickerUploadMaxSizeKBOwnBound()（只受贴纸自身硬上限约束），
// **不是**收敛后的 StickerUploadMaxSizeKB()：后者已经 min 过全局上限，两侧
// 拿去比永远相等，守卫等于死代码。写侧要回答的问题是「运营配置的贴纸上限能
// 不能被兑现」，那要拿配置意图去比生效的全局上限。
//
// 注意这里是**两次**独立的 snapshot.Load()（两个 getter 各自 load 一次），
// 不是一次。中间落一次 Reload() 会拼出一个跨代组合。后果有界：影响的只是
// 一次写侧校验判定，读侧不受影响，与既有的 thread.auto_archive_days 守卫同形
// 态。要真正收口需要给 SystemSettings 加一个「一次快照读 N 个键」的原语，
// 那是共享配置机件的改动，留作独立任务。
func (s *SystemSettings) FileStickerSizeOrdering() FileStickerSizeOrdering {
	return FileStickerSizeOrdering{
		FileMaxSizeKB:    s.FileMaxSizeKB(),
		StickerMaxSizeKB: s.stickerUploadMaxSizeKBOwnBound(),
	}
}

// ApplyFileStickerSizeOverlay 把一批待写入的值叠加到当前组合上，得到「若本次
// 写入提交后」的 prospective 组合。key 形如 "file.max_size_kb"。
// 无法解析的值保持原值 —— 类型校验在调用方已经跑过。
//
// 叠加后**必须按各自 getter 的上界钳位**，否则守卫比较的两侧不同质：cur 来自
// 钳位过的 getter，incoming 却是原样值，于是它校验了一对运行时永远不会执行的
// 组合。天花板是常量 524288 时这只会过度拒绝（fail-closed，clamp 的结果恒高于
// 贴纸上限）；天花板可配到贴纸上限之下后，它会**漏拒**：
//
//	OCTO_FILE_MAX_SIZE_KB_HARD_CAP=512，贴纸取默认 1024
//	超管写 file.max_size_kb=4096
//	  改动前 prospective={4096,1024} → 合法 → 200 {"applied":true}
//	  运行时钳位后 {512,1024} → 贴纸永远传不上去，且没有任何报错
//
// 贴纸侧只钳到贴纸自身的硬上限，**不钳全局上限** —— 钳了两侧就恒相等，守卫
// 永远不触发。
func ApplyFileStickerSizeOverlay(cur FileStickerSizeOrdering, incoming map[string]string) FileStickerSizeOrdering {
	out := cur
	if v, ok := incoming["file.max_size_kb"]; ok {
		out.FileMaxSizeKB = clampUpper(
			overlayPositiveInt(v, DefaultFileMaxSizeKB, cur.FileMaxSizeKB),
			FileMaxSizeKBHardCap())
	}
	if v, ok := incoming["sticker.upload_max_size_kb"]; ok {
		out.StickerMaxSizeKB = clampUpper(
			overlayPositiveInt(v, defaultStickerUploadMaxSizeKB, cur.StickerMaxSizeKB),
			stickerUploadMaxSizeKBHardCap)
	}
	return out
}

// clampUpper 是 SystemSettings.clampIntUpper 的无接收者、无告警版本，供
// overlay 这类纯函数复用同一道上界。overlayPositiveInt 已保证入参 > 0，
// 所以这里不需要 ≤0 分支。
func clampUpper(v, hardCap int) int {
	return min(v, hardCap)
}

// overlayPositiveInt 解析 overlay 值：空串 = 清除该 key，回到 code default
// （getter 把空快照条目当未配置）；非法值保持原值。
func overlayPositiveInt(raw string, codeDefault, current int) int {
	if strings.TrimSpace(raw) == "" {
		return codeDefault
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return current
	}
	return n
}

// ViolatesFileStickerSizeOrdering 报告该组合是否会让贴纸永远传不上去。
func ViolatesFileStickerSizeOrdering(o FileStickerSizeOrdering) bool {
	if o.FileMaxSizeKB <= 0 || o.StickerMaxSizeKB <= 0 {
		return false
	}
	return o.FileMaxSizeKB < o.StickerMaxSizeKB
}

// ---------------------------------------------------------------------------
// appconfig 下发（brief D7）
//
// /v1/common/appconfig 需要下发**有效**的允许扩展名，但有效值 =
// (baseline ∪ extra) − blocked，而 baseline 是 modules/file 的知识，且
// modules/common 不能 import modules/file（file 已 import common）。
//
// 依赖倒置：file 包在 init() 里注册一个计算闭包，这里只负责调用。
// ---------------------------------------------------------------------------

// fileUploadLimitsFn 包一层，让接口值能进 atomic.Pointer。
type fileUploadLimitsFn struct {
	fn func() (maxSizeKB int, allowedExtensions []string)
}

var fileUploadLimitsProvider atomic.Pointer[fileUploadLimitsFn]

// SetFileUploadLimitsProvider 由 modules/file 的 init() 调用一次。
func SetFileUploadLimitsProvider(fn func() (maxSizeKB int, allowedExtensions []string)) {
	fileUploadLimitsProvider.Store(&fileUploadLimitsFn{fn: fn})
}

// FileUploadLimits 返回当前生效的上传限制。ok=false 表示 provider 未注册
// （modules/file 未链接进本次构建），调用方应当**整个字段不下发**，而不是
// 下发一个空数组 —— 空 allowed_extensions 会被客户端读成「什么都不能传」。
func FileUploadLimits() (maxSizeKB int, allowedExtensions []string, ok bool) {
	p := fileUploadLimitsProvider.Load()
	if p == nil || p.fn == nil {
		return 0, nil, false
	}
	kb, exts := p.fn()
	if exts == nil {
		exts = []string{}
	}
	return kb, exts, true
}

// ---------------------------------------------------------------------------
// 内置黑名单探针
//
// 「哪些扩展名永远不可放开」是 modules/file 的知识（那张 baseline map 在
// const.go），而 common 不能 import file。同 appconfig provider 的依赖倒置：
// file 在 init() 里注册判定函数，这里只负责调用。
//
// 用途有二：写侧当场拒绝把黑名单项写进 extra_allowed（否则运维会写进去、
// 看到 effective_value 显示它、然后发现上传还是被拒）；以及给直改库的旁路
// 在读侧兜底。
// ---------------------------------------------------------------------------

type fileBlockedExtensionProbe struct {
	fn func(ext string) bool
}

var fileBlockedProbe atomic.Pointer[fileBlockedExtensionProbe]

// SetBuiltinBlockedFileExtensionProbe 由 modules/file 的 init() 调用一次。
func SetBuiltinBlockedFileExtensionProbe(fn func(ext string) bool) {
	fileBlockedProbe.Store(&fileBlockedExtensionProbe{fn: fn})
}

// IsBuiltinBlockedFileExtension 报告扩展名是否在内置黑名单里（不可通过任何
// 配置撤销）。probe 未注册时返回 false —— 降级为「不额外拦截」，真正的安全
// 保证在 modules/file 的派生里，那一层不依赖本探针。
func IsBuiltinBlockedFileExtension(ext string) bool {
	p := fileBlockedProbe.Load()
	if p == nil || p.fn == nil {
		return false
	}
	return p.fn(ext)
}
