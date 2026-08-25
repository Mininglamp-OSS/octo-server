// Package featuregate 提供通用的功能灰度评估纯逻辑：给定一条规则、一组白名单
// 条目和一个评估维度，判定某次调用是否放行。
//
// 本包刻意保持「零 IO、零 module 依赖」：不读 env、不查 DB/Redis、不判 kill
// switch（kill switch 由调用方在评估前短路处理）。所有外部状态（规则、白名单）
// 由调用方加载后以参数传入，使 Evaluate 可被表驱动单测完整覆盖。
//
// 持久化、Redis 缓存、env kill、fail-open/closed 兜底等带 IO 的编排逻辑放在
// modules/featuregate 的 Service 中，Service 在准备好 Rule/Scope 后调用本包。
package featuregate

import "hash/crc32"

// Mode 是某个功能 key 的放量模式。
type Mode string

const (
	// ModeOff 全部拒绝。白名单不可穿透（见 Evaluate）。
	ModeOff Mode = "off"
	// ModeOn 全部放行。
	ModeOn Mode = "on"
	// ModeWhitelist 仅白名单命中放行。
	ModeWhitelist Mode = "whitelist"
	// ModePercent 白名单优先，其余按 hash 稳定分桶的百分比放量。
	ModePercent Mode = "percent"
)

// 白名单条目维度类型 / 分桶维度。两者取值域刻意相同：一条 scope 记录的
// scope_type 与一条规则的 bucket_by 都在回答「按哪个维度识别对象」，共用一套
// 词汇表可以让 dimValue 一处解析、避免两套 switch 漂移。
const (
	ScopeTypeGroup = "group"
	ScopeTypeSpace = "space"
	ScopeTypeUser  = "user"
)

// DefaultBucketBy 是 bucket_by 未显式配置时的分桶维度。
//
// 取 group 是为了忠实于本框架初版设计（percent 固定按 group_no 分桶）。它**不是**
// 生产兼容性约束——这套框架从未上线，不存在需要保持行为不变的既有部署；仅仅是
// 一个可预测的缺省。注意它与「只有 UID 可用」的调用方（如客户端灰度位下发端点）
// 天然不匹配，那种场景必须显式声明 bucket_by=user，否则 percentDecision 会因维度
// 缺失而 fail-closed（见 ReasonDimUnavailable）。
const DefaultBucketBy = ScopeTypeGroup

// 决策原因，用于可观测与测试断言。
const (
	ReasonOff           = "off"
	ReasonOn            = "on"
	ReasonWhitelistHit  = "whitelist_hit"
	ReasonWhitelistMiss = "whitelist_miss"
	// ReasonWhitelistDimUnavailable 表示白名单**非空**、但每一条的维度在本次评估
	// 中都取不到值 —— 即这份名单在这个调用点永远不可能命中。它与 whitelist_miss
	// 的区别是「命不中」还是「没法命中」：前者是正常结果，后者是配置与调用点不
	// 匹配。少了这个区分，一条全是 group 条目的 client-visible 白名单会表现得和
	// 「用户确实不在名单里」一模一样，两侧都无声。
	ReasonWhitelistDimUnavailable = "whitelist_dim_unavailable"
	ReasonPercentIn               = "percent_in"
	ReasonPercentOut              = "percent_out"
	ReasonUnknownMode             = "unknown_mode"
	// ReasonDimUnavailable 表示规则要求的分桶维度在本次评估的 Dims 中为空，
	// 判定因此 fail-closed。这是配置与调用点不匹配的信号，调用方应记日志告警。
	ReasonDimUnavailable = "dim_unavailable"
)

// Rule 是某个功能 key 的规则快照（纯数据）。
type Rule struct {
	Key     string
	Mode    Mode
	Percent int // [0,100]，仅 ModePercent 使用
	// BucketBy 是 percent 模式的分桶维度（group/space/user）。空值归一到
	// DefaultBucketBy。让它可配置，是因为同一套框架既要服务「按群放量」的写时
	// 闸门，也要服务「按用户放量」的展示位——固定单一维度无法同时表达。
	BucketBy string
}

// Scope 是一条白名单条目。
type Scope struct {
	Type string // ScopeTypeGroup / ScopeTypeSpace / ScopeTypeUser
	ID   string // group_no / space_id / uid
}

// Dims 是一次评估的维度取值。调用点能提供哪几个维度取决于它自身的上下文：
// 写时闸门通常三者齐备，而用户级灰度位下发端点只有 UID。
type Dims struct {
	SpaceID string
	GroupNo string
	UID     string
}

// Decision 是评估结果。Reason 取上面的 Reason* 常量之一。
type Decision struct {
	Allow  bool
	Reason string
	// DimensionUnusable 标记「这条规则引用的某个维度在本次评估中取不到值」，
	// **独立于 Allow**。
	//
	// 之所以不并进 Reason：percent 的 0%/100% 短路在维度不可用时**决策依然正确**
	// （0=谁都不放、100=所有人都放，与维度无关），把它们改成 fail-closed 会为了
	// 一个不影响结果的错配去打挂一条正在全量的规则。但错配必须可见，否则运维会在
	// 把 100 调到 50 的那一刻突然全员掉线。于是决策照旧、另开一个位回报。
	DimensionUnusable bool
}

// Evaluate 是纯函数：按 rule.Mode 判定是否放行。不读 env、不判 kill switch。
// 未知 mode 一律保守拒绝（fail-safe），避免规则写坏时意外放量。
//
// 白名单的作用域：它在 whitelist 与 percent 两个 mode 下都生效（percent 下优先于
// 分桶，即「永久豁免名单 + 其余人群按比例」），在 off 下**无条件失效**，在 on 下
// 无意义。
//
// off 不可被白名单穿透是硬边界：off 是回滚/止血语义的一部分，若白名单能穿透，
// 关停就失去了确定性。
//
// 白名单跨 mode 生效是本框架初版的一处修正。初版的 percent 分支完全不读 scopes，
// 于是标准放量路径 off → whitelist（内测）→ percent 会在切档瞬间把内测人员整批
// 甩掉（只剩恰好分桶命中的那部分），而且 percent 阶段新加白名单条目是静默失败——
// 写入返回成功、读路径根本不看。
func Evaluate(rule Rule, scopes []Scope, dims Dims) Decision {
	switch rule.Mode {
	case ModeOff:
		return Decision{Allow: false, Reason: ReasonOff}
	case ModeOn:
		return Decision{Allow: true, Reason: ReasonOn}
	case ModeWhitelist:
		hit, allUnusable := whitelistHit(scopes, dims)
		if hit {
			return Decision{Allow: true, Reason: ReasonWhitelistHit}
		}
		if allUnusable {
			return Decision{Allow: false, Reason: ReasonWhitelistDimUnavailable, DimensionUnusable: true}
		}
		return Decision{Allow: false, Reason: ReasonWhitelistMiss}
	case ModePercent:
		hit, allUnusable := whitelistHit(scopes, dims)
		if hit {
			return Decision{Allow: true, Reason: ReasonWhitelistHit}
		}
		d := percentDecision(rule, dims)
		// 白名单用不上也是错配，即便分桶本身判得出结果。
		if allUnusable {
			d.DimensionUnusable = true
		}
		return d
	default:
		return Decision{Allow: false, Reason: ReasonUnknownMode}
	}
}

// whitelistHit 逐条比对白名单。条目按自己的 scope_type 取对应维度值；该维度在本次
// 评估中为空时跳过该条目——空值绝不匹配，否则一条 scope_id="" 的脏数据会命中所有
// 缺维度的调用。
//
// 第二个返回值 allDimsUnusable 报告「名单非空，但每一条都因维度取不到值而被跳过」。
// 调用方据此把「没命中」和「没法命中」分开：空名单不算（那是合法状态），有可用维度
// 的条目只是没匹配上也不算。
func whitelistHit(scopes []Scope, dims Dims) (hit bool, allDimsUnusable bool) {
	usable := 0
	for _, s := range scopes {
		id := dimValue(s.Type, dims)
		if id == "" {
			continue
		}
		usable++
		if s.ID == id {
			return true, false
		}
	}
	return false, len(scopes) > 0 && usable == 0
}

// percentDecision 判定 percent 模式。0（及负数）全拒、>=100 全放，这两档与维度
// 无关，故先短路——否则一条 100% 的规则会因维度缺失而反直觉地全拒。
//
// 只有真正需要分桶时才要求维度可用：维度为空时**绝不按空串照算**。空串会让所有
// 对象落进同一个桶（Bucket(key,"") 是该 key 的一个常数），使配置的 50% 静默变成
// 全体开或全体关，且管理台仍显示 50%——一个无任何报错的错配。
func percentDecision(rule Rule, dims Dims) Decision {
	// 短路分支的决策与维度无关，但错配仍要回报（见 Decision.DimensionUnusable）。
	unusable := dimValue(BucketDimension(rule), dims) == ""
	if rule.Percent <= 0 {
		return Decision{Allow: false, Reason: ReasonPercentOut, DimensionUnusable: unusable}
	}
	if rule.Percent >= 100 {
		return Decision{Allow: true, Reason: ReasonPercentIn, DimensionUnusable: unusable}
	}
	if unusable {
		return Decision{Allow: false, Reason: ReasonDimUnavailable, DimensionUnusable: true}
	}
	dimID := dimValue(BucketDimension(rule), dims)
	if Bucket(rule.Key, dimID) < rule.Percent {
		return Decision{Allow: true, Reason: ReasonPercentIn}
	}
	return Decision{Allow: false, Reason: ReasonPercentOut}
}

// dimValue 把维度名解析为本次评估里的取值。未知维度返回空串，交由调用点按
// 「维度不可用」处理，而不是静默回退到某个别的维度。
func dimValue(dim string, dims Dims) string {
	switch dim {
	case ScopeTypeGroup:
		return dims.GroupNo
	case ScopeTypeSpace:
		return dims.SpaceID
	case ScopeTypeUser:
		return dims.UID
	default:
		return ""
	}
}

// BucketDimension 返回规则实际使用的分桶维度，空值归一到 DefaultBucketBy。
// 导出是为了让写侧校验与可观测日志能拿到与判定完全一致的口径。
func BucketDimension(rule Rule) string {
	if rule.BucketBy == "" {
		return DefaultBucketBy
	}
	return rule.BucketBy
}

// ValidDimension 报告 dim 是否是合法的维度名（同时用于 scope_type 与 bucket_by
// 的写侧校验）。空串在此视为非法：调用方若要表达「未配置」，应在传入前判空并走
// DefaultBucketBy，而不是把空串当合法值存进库。
func ValidDimension(dim string) bool {
	switch dim {
	case ScopeTypeGroup, ScopeTypeSpace, ScopeTypeUser:
		return true
	}
	return false
}

// Bucket 把 (key, dimID) 稳定映射到 [0,100)。同一对入参结果恒定，保证放量曲线
// 单调（percent 调高只会纳入更多桶，已命中的不会掉出）；按 key 加盐，使不同功能
// 的分桶相互独立、不会同涨同落。
//
// 注意分桶维度本身不参与加盐。切换 bucket_by 会整体改变入参，因此是一次人群
// 重新洗牌而非渐进操作——单调性保证只在同一维度内成立。
func Bucket(key, dimID string) int {
	h := crc32.ChecksumIEEE([]byte(key + ":" + dimID))
	return int(h % 100)
}
