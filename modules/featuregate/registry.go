package featuregate

import (
	"fmt"
	"regexp"

	"github.com/Mininglamp-OSS/octo-server/modules/common"
)

// ClientFlag 是一条【可下发给客户端】的灰度位注册项。
//
// 为什么需要这份注册表：octo_feature_gate.feature_key 是自由字符串，没有区分
// 「哪些 key 允许暴露给客户端查询」。若端点按客户端传入的 key 查库，任意登录用户
// 就能枚举/探测内部 gate 是否存在及其状态。端点因此【不接受任何 key 参数】，
// 只按这份代码内的清单批量评估——注册表就是客户端无法跨越的那道边界。
//
// FeatureKey 与 ClientKey 刻意分开：
//
//	FeatureKey  运维面标识：octo_feature_gate 主键、管理台展示，且
//	            OCTO_FEATUREGATE_<大写>_KILL 环境变量名由它推导（见 KillSwitchEnv）。
//	ClientKey   wire 契约：GET /v1/featuregate/flags 响应 JSON 的字段名。
//
// 绑成一个会让「运维改 gate 名」这种本该无风险的操作静默破坏三端客户端。反过来
// 的约束是：ClientKey 一旦发布即冻结，与 appconfig 字段同级别（additive-only），
// 要改就是破坏性变更、须与客户端发版协调。
type ClientFlag struct {
	// FeatureKey 是 octo_feature_gate.feature_key。
	FeatureKey string
	// ClientKey 是下发给客户端的 JSON 字段名。
	ClientKey string
	// DeploymentGate 是可选的「部署前置位」。非 nil 时，该 flag 的最终值 =
	// gate 判定 AND DeploymentGate(...)。
	//
	// 用途：某功能既依赖外部服务是否部署（system_setting 里已有的位），又需要
	// 按用户放量时，把两者的 AND 做在【服务端】。绝不能把组合逻辑丢给客户端——
	// 三端各实现一次必然漂移。
	//
	// 多数 flag 不需要它：featuregate 自己的 mode=off 就足以表达「部署没就绪」。
	DeploymentGate func(*common.SystemSettings) bool
}

// clientKeyPattern / featureKeyPattern 约束两个 key 的字面量。
//
// 两者都限定在 [a-z0-9_]，**刻意不允许连字符**。原因在 feature_key 这一侧：
// 它要推导 env 杀开关名（见 KillSwitchEnv）。若同时允许 `-` 和 `_`，就得在推导时
// 把 `-` 折成 `_`，于是 `docs-beta` 与 `docs_beta` 这两条**不同**的规则（唯一性
// 校验都能过）会映射到**同一个** OCTO_FEATUREGATE_DOCS_BETA_KILL ——
// 想停其中一个会把另一个
// 一起停掉。禁掉连字符使 key → env 名成为单射，这一整类碰撞在结构上就不存在了。
//
// ClientKey 用同样的 snake_case，与 appconfig 现有字段（docs_on、tracking_enabled）
// 风格一致。
var (
	clientKeyPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	featureKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// maxFeatureKeyLen 对齐 octo_feature_gate.feature_key 的 VARCHAR(64)。在注册期
// 拦住超长值，好过等到写库时才失败。
const maxFeatureKeyLen = 64

// validFeatureKey 是 feature_key 的**唯一**校验口径，注册表与管理端写路径共用。
//
// 管理端一度只校验长度不校验字符集，后果不是脏数据而是**丢掉急停能力**：
// feature_key = "my gate" 推导出的 OCTO_FEATUREGATE_MY GATE_KILL 含空格，
// 常规手段设不进去，
// 这条 gate 就永久失去了 env 级紧急停止 —— 而那正是「DB/Redis 全挂时仍能一键停」
// 的最后一条路径（主回滚 mode=off 要读 DB，恰恰在故障时不可用）。
func validFeatureKey(key string) bool {
	return len(key) <= maxFeatureKeyLen && featureKeyPattern.MatchString(key)
}

// clientRegistry 是校验过的注册表。构造后不可变，读侧无锁。
type clientRegistry struct {
	flags []ClientFlag
	// needsSettings 缓存「是否有任一 flag 声明了 DeploymentGate」，让 API 层
	// 只在真的需要时才去取 SystemSettings 单例。
	needsSettings bool
}

// newClientRegistry 校验并构造注册表。
//
// 两个 key 都必须唯一。ClientKey 重复尤其致命且**静默**：响应是 map[string]bool，
// 两项声明同一个 ClientKey 时后写的直接覆盖先写的，map 不会报任何错，线上表现为
// 「某个 flag 的值莫名其妙跟着另一个功能走」。
func newClientRegistry(flags []ClientFlag) (*clientRegistry, error) {
	seenFeature := make(map[string]struct{}, len(flags))
	seenClient := make(map[string]struct{}, len(flags))
	needsSettings := false
	for i, f := range flags {
		if !featureKeyPattern.MatchString(f.FeatureKey) {
			return nil, fmt.Errorf("featuregate registry[%d]: invalid FeatureKey %q (want %s)", i, f.FeatureKey, featureKeyPattern)
		}
		if len(f.FeatureKey) > maxFeatureKeyLen {
			return nil, fmt.Errorf("featuregate registry[%d]: FeatureKey %q exceeds %d bytes", i, f.FeatureKey, maxFeatureKeyLen)
		}
		if !clientKeyPattern.MatchString(f.ClientKey) {
			return nil, fmt.Errorf("featuregate registry[%d]: invalid ClientKey %q (want %s)", i, f.ClientKey, clientKeyPattern)
		}
		if _, dup := seenFeature[f.FeatureKey]; dup {
			return nil, fmt.Errorf("featuregate registry: duplicate FeatureKey %q", f.FeatureKey)
		}
		if _, dup := seenClient[f.ClientKey]; dup {
			return nil, fmt.Errorf("featuregate registry: duplicate ClientKey %q", f.ClientKey)
		}
		seenFeature[f.FeatureKey] = struct{}{}
		seenClient[f.ClientKey] = struct{}{}
		if f.DeploymentGate != nil {
			needsSettings = true
		}
	}
	// 拷贝一份再持有：直接存调用方的 slice 会让构造后对其元素的原地修改
	// （如 clientFlagList[0].ClientKey = "x"）穿透进来，绕过上面刚做的唯一性与
	// 命名校验。注册表构造后必须是不可变的。
	owned := make([]ClientFlag, len(flags))
	copy(owned, flags)
	return &clientRegistry{flags: owned, needsSettings: needsSettings}, nil
}

// mustNewClientRegistry 是 newClientRegistry 的启动期变体：注册表写错直接 panic，
// 而不是带着一个会静默串值的表上线。同 pkg/i18n 的 codes 注册与
// mustLookupSharedCode 的取向——注册表错误属于部署事故，越早越响越好。
func mustNewClientRegistry(flags []ClientFlag) *clientRegistry {
	r, err := newClientRegistry(flags)
	if err != nil {
		panic(err)
	}
	return r
}

// clientKeys 返回全部对外 key，供「答案整体不可知」时一次性列进 unavailable。
func (r *clientRegistry) clientKeys() []string {
	keys := make([]string, 0, len(r.flags))
	for _, f := range r.flags {
		keys = append(keys, f.ClientKey)
	}
	return keys
}

// isClientVisible 报告某 feature_key 是否会被下发给客户端。管理端写侧用它决定
// 要不要施加「维度必须是调用点能提供的」这条额外约束。
func (r *clientRegistry) isClientVisible(featureKey string) bool {
	for _, f := range r.flags {
		if f.FeatureKey == featureKey {
			return true
		}
	}
	return false
}

// clientFlagList 是**可下发给客户端的灰度位清单**。
//
// 当前为空 —— 本任务交付的是机制，不是具体业务开关。某个功能要用户级灰度时，在此
// 追加一行即可，客户端无需为「发现新 flag」发版（它读到的就是这份清单的全集）。
//
// 追加时注意：
//   - ClientKey 一经发布即冻结（wire 契约），命名用 snake_case；
//   - 归属唯一——已经在 appconfig 有展示位的功能【不要】再建 featuregate key，
//     老客户端只读 appconfig，双挂会让灰度当场失效；
//   - 规则必须配 bucket_by=user / scope_type=user，因为本端点只有 UID 维度
//     （管理端写侧会拦，读侧也会 fail-closed）。
var clientFlagList = []ClientFlag{}

// defaultClientRegistry 是进程内的注册表实例。包初始化期完成校验。
var defaultClientRegistry = mustNewClientRegistry(clientFlagList)
