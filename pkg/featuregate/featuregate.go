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
	// ModeOff 全部拒绝。
	ModeOff Mode = "off"
	// ModeOn 全部放行。
	ModeOn Mode = "on"
	// ModeWhitelist 仅白名单命中放行。
	ModeWhitelist Mode = "whitelist"
	// ModePercent 按 hash 稳定分桶的百分比放量。
	ModePercent Mode = "percent"
)

// 白名单条目维度类型。当前 Evaluate 只消费 group；space 字段先落库、暂不参与
// 命中判定（扩面阶段再启用），见 Evaluate 的 whitelist 分支。
const (
	ScopeTypeGroup = "group"
	ScopeTypeSpace = "space"
)

// 决策原因，用于可观测与测试断言。
const (
	ReasonOff           = "off"
	ReasonOn            = "on"
	ReasonWhitelistHit  = "whitelist_hit"
	ReasonWhitelistMiss = "whitelist_miss"
	ReasonPercentIn     = "percent_in"
	ReasonPercentOut    = "percent_out"
	ReasonUnknownMode   = "unknown_mode"
)

// Rule 是某个功能 key 的规则快照（纯数据）。
type Rule struct {
	Key     string
	Mode    Mode
	Percent int // [0,100]，仅 ModePercent 使用
}

// Scope 是一条白名单条目。
type Scope struct {
	Type string // ScopeTypeGroup / ScopeTypeSpace
	ID   string // group_no 或 space_id
}

// Dims 是一次评估的维度取值。
type Dims struct {
	SpaceID string
	GroupNo string
	UID     string
}

// Decision 是评估结果。Reason 取上面的 Reason* 常量之一。
type Decision struct {
	Allow  bool
	Reason string
}

// Evaluate 是纯函数：按 rule.Mode 判定是否放行。不读 env、不判 kill switch。
// 未知 mode 一律保守拒绝（fail-safe），避免规则写坏时意外放量。
func Evaluate(rule Rule, scopes []Scope, dims Dims) Decision {
	switch rule.Mode {
	case ModeOff:
		return Decision{Allow: false, Reason: ReasonOff}
	case ModeOn:
		return Decision{Allow: true, Reason: ReasonOn}
	case ModeWhitelist:
		// 当前仅按 group 维度命中；空 GroupNo 不应匹配到任何条目。
		if dims.GroupNo != "" {
			for _, s := range scopes {
				if s.Type == ScopeTypeGroup && s.ID == dims.GroupNo {
					return Decision{Allow: true, Reason: ReasonWhitelistHit}
				}
			}
		}
		return Decision{Allow: false, Reason: ReasonWhitelistMiss}
	case ModePercent:
		if percentIn(rule, dims) {
			return Decision{Allow: true, Reason: ReasonPercentIn}
		}
		return Decision{Allow: false, Reason: ReasonPercentOut}
	default:
		return Decision{Allow: false, Reason: ReasonUnknownMode}
	}
}

// percentIn 判定 percent 模式是否命中。0（及负数）全拒、>=100 全放，中间值按
// 稳定分桶比较；空 GroupNo 落到桶 0，仍受 percent 约束。
func percentIn(rule Rule, dims Dims) bool {
	if rule.Percent <= 0 {
		return false
	}
	if rule.Percent >= 100 {
		return true
	}
	return Bucket(rule.Key, dims.GroupNo) < rule.Percent
}

// Bucket 把 (key, dimID) 稳定映射到 [0,100)。同一对入参结果恒定，保证放量曲线
// 单调（percent 调高只会纳入更多桶，已命中的不会掉出）；按 key 加盐，使不同功能
// 的分桶相互独立、不会同涨同落。
func Bucket(key, dimID string) int {
	h := crc32.ChecksumIEEE([]byte(key + ":" + dimID))
	return int(h % 100)
}
