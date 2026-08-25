package featuregate

import (
	"context"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	fg "github.com/Mininglamp-OSS/octo-server/pkg/featuregate"
	"go.uber.org/zap"
)

// flagsResp 是 GET /v1/featuregate/flags 的响应体。
//
// Flags 是 map[string]bool（动态 key），且**不得**加 omitempty —— 值为 false 的
// key 必须出现在 JSON 里。固定字段 struct 或 omitempty 都会让「确定性的关」丢失。
//
// 值只有布尔：不下发 mode/percent/scope 等内部细节，避免客户端反推灰度策略。
// 排障所需的「为什么是这个值」只进服务端日志。
//
// Unavailable 列出**判定不可得**的 key（存储故障），客户端应对这些 key 保留上次
// 值。早先的设计是把它们从 Flags 里省略、用「缺席」来表达这层含义，语义上等价，
// 但**任何 schema 语言都描述不了「缺席携带含义」**：OpenAPI / JSON Schema 只能说
// 某属性可选，没有词汇表达「不出现 ≠ 零值」。于是 codegen 出来的客户端反序列化进
// map 之后，那个区分在任何业务代码跑起来之前就已经丢了，语言的零值规则会把缺席读
// 成 false —— 恰好是这套设计要防的那个失败。改成显式字段后契约完全可描述，
// 客户端不可能"默认做错"，而且它只说"没算出来"、不说为什么，不泄露灰度策略。
type flagsResp struct {
	Flags map[string]bool `json:"flags"`
	// 恒为非 nil：空时序列化成 []，避免弱类型客户端在 null 上解引用炸掉。
	Unavailable []string `json:"unavailable"`
}

// displayEvaluator 是 flagsAPI 需要的最小能力面，在使用处定义（而不是在
// Service 侧）。抽这一层不是为了将来换实现，而是为了让「存储故障 → 进 unavailable」
// 这条路径可被确定性地测到：真去制造一次 DB/Redis 故障要么污染同包其它测试
// （drop 表），要么依赖时序，两者都比一个 stub 差。
type displayEvaluator interface {
	// AllowDisplay 返回 (allow, ok)，语义见 Service.AllowDisplay。
	AllowDisplay(ctx context.Context, key string, dims fg.Dims) (bool, bool)
}

// flagsAPI 承载用户侧只读端点。
type flagsAPI struct {
	svc      displayEvaluator
	registry *clientRegistry
	// systemSettings 仅在注册表里存在声明了 DeploymentGate 的 flag 时才解析。
	//
	// 必须在**构造期**取一次并持有：common.EnsureSystemSettings 每次调用都取一个
	// 进程级 mutex，而 SystemSettings 的设计前提是读侧走 atomic.Pointer、永不取锁。
	// 在每请求路径上调它等于把全局锁加到热路径上。
	systemSettings *common.SystemSettings
	log.Log
}

func newFlagsAPI(ctx *config.Context, svc displayEvaluator, registry *clientRegistry) *flagsAPI {
	a := &flagsAPI{
		svc:      svc,
		registry: registry,
		Log:      log.NewTLog("featureGateFlags"),
	}
	// 没有任何 flag 需要部署前置位时不去触碰 SystemSettings 单例——构造它会做一次
	// DB Load 并起一个后台 goroutine，没人用就不该付这个代价。注册表里一旦出现
	// 声明了 DeploymentGate 的 flag，这里会自动开始解析，无需另行改动。
	if registry.needsSettings {
		a.systemSettings = common.EnsureSystemSettings(ctx)
	}
	return a
}

// get 返回当前登录用户的全部灰度位。
//
// 契约要点：
//   - 请求**不接受任何 key 参数**。响应恒为注册表全集，客户端因此无法用一个精心
//     构造的 key 去探测某个内部 gate 是否存在——注册表就是调用方跨不过去的边界。
//   - 单个 key 遇到**存储故障**时进 unavailable 列表（客户端保留上次值），其余 key
//     照常返回，整个请求仍然 200。一次 Redis 抖动不该让全体用户丢功能。
//   - 「规则不存在」是确定性的关，进 flags 且值为 false。两者混淆会让「未配置」
//     变成「保留旧值」，灰度就永远关不掉。
func (a *flagsAPI) get(c *wkhttp.Context) {
	// 结果因人而异，而 URL 对所有用户逐字节相同——区分调用者的只有鉴权头。
	// 任何按 URL 缓存的共享代理都会把用户 A 的判定回给用户 B。
	// private 禁共享缓存，no-store 连私有副本也不留（开关变更要即时可见）。
	c.Header("Cache-Control", "private, no-store")
	// Vary 必须点名**本仓实际使用的**鉴权头。octo-lib 的 AuthMiddleware 读的是
	// `token`（不是 Authorization —— 那是 bot API 的用法）。写错头名比不写更糟：
	// 一个所有请求都为空的头没有任何区分度，对某些「认 Vary 但对 no-store 处理不
	// 到位」的中间层来说，等于宣告"这些响应可以互换"。
	c.Header("Vary", "token")

	uid := c.GetLoginUID()
	if uid == "" {
		// 不可达：本路由挂在 AuthMiddleware 之后，uid 必然已注入。真出现说明鉴权
		// 链被改动了。判定本身仍会 fail-closed（白名单不匹配空值、按 user 分桶走
		// dim_unavailable），但那样只会表现为"功能全没了"，排查会绕远路；显式记一条
		// Error 让根因一眼可见。
		a.Error("flags endpoint reached without a login uid; auth chain may be misconfigured")
	}

	// 本端点只有 UID 维度：不接收也不推断空间/群上下文。一个用户可属多个空间，
	// "当前空间"在这条请求里没有确定答案，因此 flags 是**账号级**的，不构成跨空间
	// 读取面。凡是要下发到客户端的规则都必须按 user 维度配置（管理端写侧已拦，
	// AllowDisplay 读侧还会再兜一次）。
	dims := fg.Dims{UID: uid}

	flags := make(map[string]bool, len(a.registry.flags))
	unavailable := make([]string, 0)
	for _, f := range a.registry.flags {
		allow, ok := a.resolve(c.Request.Context(), f, dims)
		if !ok {
			// 存储故障：判定不可得，显式列出让客户端保留上次值。
			// 已在 AllowDisplay 内打过 Warn。
			unavailable = append(unavailable, f.ClientKey)
			continue
		}
		flags[f.ClientKey] = allow
	}

	c.Response(flagsResp{Flags: flags, Unavailable: unavailable})
}

// resolve 求出单个 flag 对本次调用者的最终值。
//
// 返回 (allow, ok)：ok=false 表示存储故障、判定不可得，调用方应把该 key 列进 unavailable。
func (a *flagsAPI) resolve(ctx context.Context, f ClientFlag, dims fg.Dims) (bool, bool) {
	allow, ok := a.svc.AllowDisplay(ctx, f.FeatureKey, dims)
	if !ok {
		return false, false
	}
	if !allow || f.DeploymentGate == nil {
		return allow, true
	}
	// 部署前置位：在服务端完成 AND，客户端只看到最终布尔。
	// 组合逻辑绝不下放给客户端——三端各实现一次必然漂移。
	if a.systemSettings == nil {
		// 不可达：needsSettings 由同一份注册表推导。真出现说明注册表与构造期解析
		// 脱节了，此时宁可关掉也不要按 gate 单独放行。
		a.Error("deployment gate declared but SystemSettings unresolved; failing closed",
			zap.String("feature_key", f.FeatureKey))
		return false, true
	}
	return f.DeploymentGate(a.systemSettings), true
}
