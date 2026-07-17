package messages_search

import (
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

// Principal 抽象化「当前搜索主体」（决策十）。在引入 bot 主体之前，messages_search
// 全程直接取 c.GetLoginUID()（真人）；本抽象让 handler 只依赖接口，真人 / as-bot /
// as-user(OBO) / as-user(uk) 四态各自提供身份、Space、限流键、黑名单策略与审计字段。
//
// 本层（YUJ-48 / Stage 1）只落「载体 + 可读谓词接口 + 单测」，不改任何鉴权语义：
//   - 真人（user）实现行为与现状完全一致（SubjectUID==GetLoginUID、SpaceID==GetSpaceID）。
//   - user_bot / obo / uk 三套实现提供 Authenticate + BlacklistPolicy（uk 为完整实现，
//     非桩；obo 的 grant/scope/TOCTOU 校验在 #F 落，本层只组装载体）。
//   - bot 主体的可读谓词逻辑（IsFriend / ExistMemberActive）在 #C/#D/#E 落，本层
//     只定义 canReadChannel / enumerateReadableChannels 的接口与 dispatch（见 predicate.go）。

// principalKind 标识请求背后的凭证类型。
type principalKind uint8

const (
	// principalKindUser 真人 web/登录态（context key "uid"，现存路径）。
	principalKindUser principalKind = iota
	// principalKindUserBot as-bot：以 User Bot 自身身份搜索（context key "robot_id"）。
	principalKindUserBot
	// principalKindOBO as-user：经 OBO 以 grantor 身份搜索（主体 = grantorUID）。
	principalKindOBO
	// principalKindUK as-user：经 uk_ User API Key 的直接真人身份（主体 = key UID）。
	principalKindUK
)

func (k principalKind) String() string {
	switch k {
	case principalKindUser:
		return "user"
	case principalKindUserBot:
		return "user_bot"
	case principalKindOBO:
		return "obo"
	case principalKindUK:
		return "uk"
	default:
		return "unknown"
	}
}

// blacklistPolicy 决定是否对主体套用真人双向黑名单门。
type blacklistPolicy uint8

const (
	// blacklistNone user_bot 显式短路：bot 无法有意义地拉黑他人 / 被拉黑
	//（addBlacklist 是真人会话专属；对方→bot 方向已决策不取，见 #C）。
	blacklistNone blacklistPolicy = iota
	// blacklistRealUserBidirectional user / obo / uk 复用同一份真人双向逻辑，
	// 仅主体 uid 来源不同（obo=grantor、uk=key UID）。
	blacklistRealUserBidirectional
)

func (p blacklistPolicy) String() string {
	switch p {
	case blacklistNone:
		return "none"
	case blacklistRealUserBidirectional:
		return "real-user-bidirectional"
	default:
		return "unknown"
	}
}

// context key 常量，镜像 bot_api/auth.go（CtxKeyRobotID/CtxKeyBotKind）与
// botfather/api_user.go（api_key_uid / api_key_space_id）。本层刻意本地定义、不
// 导入 bot_api/botfather，让搜索模块在 #B 接线路由之前保持解耦；值必须与来源一致。
const (
	ctxKeyLoginUID      = "uid"
	ctxKeyRobotID       = "robot_id"
	ctxKeyBotKind       = "bot_kind"
	ctxKeyAPIKeyUID     = "api_key_uid"
	ctxKeyAPIKeySpaceID = "api_key_space_id"

	botKindUser = "user"
	botKindApp  = "app"
)

// principalCtxKey 存放被路由显式解析出的主体（bot / uk / obo 在 #B 写入）。真人路由
// 不写入，靠 Handler.principal 的惰性默认构造。
const principalCtxKey = "messages_search.principal"

var (
	// errPrincipalUnauthenticated 上游鉴权中间件未在 context 落主体身份。
	errPrincipalUnauthenticated = errors.New("messages_search: principal not authenticated")
	// errPrincipalAppBotDenied App Bot 不支持搜索（一期整体不做 App Bot，#F 兜底显式拒绝）。
	errPrincipalAppBotDenied = errors.New("messages_search: app bot is not supported for search")
)

// Principal 是搜索主体的策略接口。handler 只依赖此接口取身份 / Space / 策略；
// 归一化可读谓词（canReadChannel / enumerateReadableChannels）以 Handler 方法承载，
// 按 Kind 分派（见 predicate.go）。
type Principal interface {
	// Kind 凭证类型。
	Kind() principalKind
	// SubjectUID 是「可达频道集」所依据的身份：真人登录 uid、bot 自身（as-bot）、
	// grantor（obo）或 key 拥有者（uk）。
	SubjectUID() string
	// SpaceID 请求所属 Space；bot 路由不挂 SpaceMiddleware 故为空，uk 取 api_key_space_id。
	SpaceID() string
	// RequiresSpaceScope 报告「空 Space 是否必须 fail-close」（决策十 / YUJ-57）。
	// 真人语义且实际驻留在某个 Space 的主体（user / uk）依赖 spaceId 段收窄跨 Space
	// DM 泄露，空 Space 触发必填门 fail-close；而**天然无 Space** 的主体（user_bot /
	// obo）根本不属于任何 Space，其可读频道集由 per-principal 谓词
	//（IsFriend / grantor allowlist）枚举，DM 可见性无需 spaceId 段兜底，故空 Space
	// 合法、**不**被必填门提前挡下（否则无 space 的 bot 永远搜不到任何结果）。
	RequiresSpaceScope() bool
	// BlacklistPolicy 报告是否对该主体套用真人双向黑名单门。
	BlacklistPolicy() blacklistPolicy
	// RateLimitKey 限流令牌桶键：as-bot / obo 都按 botUID 计（防单 bot 打爆），
	// uk 按 key UID，user 按自身 uid。
	RateLimitKey() string
	// AuditBotUID / AuditGrantorUID 供审计追溯（as-user 同时记 botUID + grantorUID）；
	// 不适用时返回 ""。
	AuditBotUID() string
	AuditGrantorUID() string
}

// userPrincipal 真人登录态。行为与现状完全一致。
type userPrincipal struct {
	uid     string
	spaceID string
}

func (p userPrincipal) Kind() principalKind              { return principalKindUser }
func (p userPrincipal) SubjectUID() string               { return p.uid }
func (p userPrincipal) SpaceID() string                  { return p.spaceID }
func (p userPrincipal) RequiresSpaceScope() bool         { return true }
func (p userPrincipal) BlacklistPolicy() blacklistPolicy { return blacklistRealUserBidirectional }
func (p userPrincipal) RateLimitKey() string             { return p.uid }
func (p userPrincipal) AuditBotUID() string              { return "" }
func (p userPrincipal) AuditGrantorUID() string          { return "" }

// userBotPrincipal as-bot：主体 = botUID，Space 空，黑名单不查，限流/审计按 botUID。
type userBotPrincipal struct {
	botUID string
}

func (p userBotPrincipal) Kind() principalKind              { return principalKindUserBot }
func (p userBotPrincipal) SubjectUID() string               { return p.botUID }
func (p userBotPrincipal) SpaceID() string                  { return "" }
func (p userBotPrincipal) RequiresSpaceScope() bool         { return false }
func (p userBotPrincipal) BlacklistPolicy() blacklistPolicy { return blacklistNone }
func (p userBotPrincipal) RateLimitKey() string             { return p.botUID }
func (p userBotPrincipal) AuditBotUID() string              { return p.botUID }
func (p userBotPrincipal) AuditGrantorUID() string          { return "" }

// oboPrincipal as-user(OBO)：主体 = grantorUID（走真人分支），黑名单复用真人双向，
// 限流/审计按 botUID，审计并记 grantorUID 以追溯。
type oboPrincipal struct {
	botUID     string
	grantorUID string
	spaceID    string
}

func (p oboPrincipal) Kind() principalKind              { return principalKindOBO }
func (p oboPrincipal) SubjectUID() string               { return p.grantorUID }
func (p oboPrincipal) SpaceID() string                  { return p.spaceID }
func (p oboPrincipal) RequiresSpaceScope() bool         { return false }
func (p oboPrincipal) BlacklistPolicy() blacklistPolicy { return blacklistRealUserBidirectional }
func (p oboPrincipal) RateLimitKey() string             { return p.botUID }
func (p oboPrincipal) AuditBotUID() string              { return p.botUID }
func (p oboPrincipal) AuditGrantorUID() string          { return p.grantorUID }

// ukPrincipal as-user(uk)：主体 = keyModel.UID（直接真人身份，不做 OBO scope 收窄），
// 黑名单复用真人双向，Space 取 api_key_space_id，限流/审计按 key UID。
type ukPrincipal struct {
	keyUID  string
	spaceID string
}

func (p ukPrincipal) Kind() principalKind              { return principalKindUK }
func (p ukPrincipal) SubjectUID() string               { return p.keyUID }
func (p ukPrincipal) SpaceID() string                  { return p.spaceID }
func (p ukPrincipal) RequiresSpaceScope() bool         { return true }
func (p ukPrincipal) BlacklistPolicy() blacklistPolicy { return blacklistRealUserBidirectional }
func (p ukPrincipal) RateLimitKey() string             { return p.keyUID }
func (p ukPrincipal) AuditBotUID() string              { return "" }
func (p ukPrincipal) AuditGrantorUID() string          { return "" }

// setPrincipal 把显式解析出的主体写入 context（bot / uk / obo 路由在 #B 使用）。
// 真人路由不调用，依赖 Handler.principal 的惰性默认。
func setPrincipal(c *wkhttp.Context, p Principal) {
	c.Set(principalCtxKey, p)
}

// principal 返回本请求的搜索主体。若路由显式写入（bot / uk / obo，#B），返回该值；
// 否则从鉴权 context（"uid" + SpaceMiddleware）构造真人默认。
//
// 这是搜索路径读取 GetLoginUID() / GetSpaceID() 的【唯一】站点——handler 不再直接
// 调用它们（决策十验收）。
func (h *Handler) principal(c *wkhttp.Context) Principal {
	if v, ok := c.Get(principalCtxKey); ok {
		if p, ok := v.(Principal); ok && p != nil {
			return p
		}
	}
	return authenticateUser(c)
}

// authenticateUser 从鉴权 context 构造真人主体（现存路径的 Authenticate）。
func authenticateUser(c *wkhttp.Context) Principal {
	return userPrincipal{
		uid:     c.GetLoginUID(),
		spaceID: strings.TrimSpace(spacepkg.GetSpaceID(c)),
	}
}

// authenticateUserBot 解析 as-bot 主体：subjectUID = botUID（authBot() 落的 "robot_id"），
// Space 空（/v1/bot 不挂 SpaceMiddleware）。App Bot 显式拒绝（一期不做，#F 兜底）。
func authenticateUserBot(c *wkhttp.Context) (Principal, error) {
	botUID := ctxString(c, ctxKeyRobotID)
	if botUID == "" {
		return nil, errPrincipalUnauthenticated
	}
	if ctxString(c, ctxKeyBotKind) == botKindApp {
		return nil, errPrincipalAppBotDenied
	}
	return userBotPrincipal{botUID: botUID}, nil
}

// authenticateOBO 组装 as-user(OBO) 主体：subjectUID = grantorUID（以 grantor 身份搜、
// 走真人分支），限流/审计仍按 botUID。grant + scope + TOCTOU 的实时权限校验在搜索热路径
// per-channel 进行（YUJ-53 / #F，见 obo.go 的 oboCanReadChannel），因为 checkOBO 需 channel
// 维度；本构造器只从已解析入参组装载体。
func authenticateOBO(botUID, grantorUID, spaceID string) (Principal, error) {
	if botUID == "" || grantorUID == "" {
		return nil, errPrincipalUnauthenticated
	}
	if botUID == grantorUID {
		// 与 checkOBO 一致的 fail-closed：主体不能自授。
		return nil, errPrincipalUnauthenticated
	}
	return oboPrincipal{
		botUID:     botUID,
		grantorUID: grantorUID,
		spaceID:    strings.TrimSpace(spaceID),
	}, nil
}

// authenticateOBOFromContext 从 authBot() 落的 context 组装 as-user(OBO) 主体（#B 在解析
// 请求体 on_behalf_of 后调用，grantorUID 由其传入）。相较 authenticateOBO 多两道 context
// 侧的 fail-closed 门：
//   - botUID 取自 "robot_id"（authBot 落）；缺失 → errPrincipalUnauthenticated。
//   - App Bot 显式拒绝（YUJ-53 / #F 验收）：一期整体不做 App Bot，且 App Bot 拿不到 OBO
//     grant（grant 只查 robot 表，App Bot 在 app_bot 表）。此处在主体构造阶段就 fail-closed
//     显式拒绝，而非放行后靠 checkOBO 静默返回空——避免 App Bot 搜索被误当作"无结果"。
//
// spaceID 走 grantor 的 Space（spacepkg.GetSpaceID 读 space_id / X-Space-Id）；bot 路由不挂
// SpaceMiddleware 时通常为空，P2P 分支即回退到 grantor 好友判定（安全，grantor=创建者）。
func authenticateOBOFromContext(c *wkhttp.Context, grantorUID string) (Principal, error) {
	botUID := ctxString(c, ctxKeyRobotID)
	if botUID == "" {
		return nil, errPrincipalUnauthenticated
	}
	if ctxString(c, ctxKeyBotKind) == botKindApp {
		return nil, errPrincipalAppBotDenied
	}
	return authenticateOBO(botUID, grantorUID, spacepkg.GetSpaceID(c))
}

// authenticateUK 解析 uk_ 主体（一期正式接线，完整实现，非桩）：subjectUID = keyModel.UID，
// Space = api_key_space_id。botfather 的 authUserAPIKey() 中间件已校验 key
//（status/ClientID/active-user）并在此之前落 context。
func authenticateUK(c *wkhttp.Context) (Principal, error) {
	keyUID := ctxString(c, ctxKeyAPIKeyUID)
	if keyUID == "" {
		return nil, errPrincipalUnauthenticated
	}
	return ukPrincipal{
		keyUID:  keyUID,
		spaceID: strings.TrimSpace(ctxString(c, ctxKeyAPIKeySpaceID)),
	}, nil
}

// ctxString 读取一个 string context 值，缺失或类型不符返回 ""。
func ctxString(c *wkhttp.Context, key string) string {
	v, ok := c.Get(key)
	if !ok || v == nil {
		return ""
	}
	s, _ := v.(string)
	return s
}
