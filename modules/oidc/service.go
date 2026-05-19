package oidc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 业务错误。callback handler 会把这些错误翻成不同的 HTTP 状态/前端提示。
var (
	// ErrUnknownUser claims 未命中任何已存在的 dmwork 账号,且 AllowNewUser=false。
	ErrUnknownUser = errors.New("oidc: claims do not match any existing dmwork user")
	// ErrConflictNeedManual 同邮箱/手机号在 dmwork 端命中多条用户,需人工合并。
	ErrConflictNeedManual = errors.New("oidc: multiple dmwork users matched, manual link required")
	// ErrEmailNotVerified IdP 返回的邮箱未验证,且配置要求验证后才能自动绑定/创建。
	ErrEmailNotVerified = errors.New("oidc: email not verified by IdP")
)

// ResolveResult ResolveOrLink 的结果(UID + 是否新创建)。
type ResolveResult struct {
	UID   string
	IsNew bool
}

// IssueSessionReq 把 ResolveOrLink 的输出 + 请求级设备信息透给 IssueSession。
//
// 字段命名与 user.ExternalLoginReq 平行,但保持独立类型避免 oidc 直接依赖 user 类型。
type IssueSessionReq struct {
	UID        string
	CreateUser bool
	Name       string
	Email      string
	Phone      string
	Zone       string
	DeviceFlag uint8
	DeviceID   string
	DeviceName string
	DeviceMod  string
	PublicIP   string

	// TrustedSSOCreate 透传到 user.ExternalLoginReq.TrustedSSOCreate,
	// 让可信 IdP 触发的新建用户路径绕过 user 模块的 register.off 全局开关。
	//
	// 仅在 CreateUser=true 时有意义。callback `res.IsNew=true` 与 /bind/create
	// 两条入口显式置 true(代表 IssuerAllowlist 已经过),其他路径(verify→confirm
	// 绑定老用户)留 false —— 与 CreateUser 本身的语义对齐:不建用户的请求不该
	// 表达"信任创建"语义。
	TrustedSSOCreate bool
}

// IssueSessionResp 会话签发结果。LoginRespJSON 直接塞 ThirdAuthcode Redis,
// 前端短码轮询取走;调用方不解析其内容。
type IssueSessionResp struct {
	UID           string
	IsNewUser     bool
	LoginRespJSON string
}

// userLookup oidc service 对 user 模块依赖的最小接口。
//
// 生产环境用 user.IService + oidc.DB 适配器实现;测试用 fakeUserLookup。
// 接口在 service 包内定义,符合 "Accept interfaces, return structs"。
type userLookup interface {
	UIDsByEmail(email string) ([]string, error)
	UIDsByPhone(zone, phone string) ([]string, error)
	IssueSession(ctx context.Context, req IssueSessionReq) (*IssueSessionResp, error)
}

// identityStore oidc service 对 oidc DB 的最小读写接口(仅 ResolveOrLink 用到)。
type identityStore interface {
	Get(issuer, subject string) (*IdentityModel, error)
	Insert(m *IdentityModel) error
	UpdateLogin(id int64, email string, emailVerified int, phone string, phoneVerified int) error
}

// Service OIDC 业务编排层。
//
// 职责:
//  1. ResolveOrLink — 把 IdP claims 解析为 dmwork uid(必要时建账或绑定历史账号)
//  2. IssueSession  — 调 user.IService.LoginByExternalIdentity 签发会话
type Service struct {
	cfg   ProviderConfig
	store identityStore
	users userLookup
	now   func() time.Time
}

// newService 构造 Service,接受小接口 store/users 注入。
//
// 生产路径(NewService in user_adapter.go)和测试路径都走这个构造函数,
// 测试注入 fake store/users,生产注入 DB/userAdapter。
func newService(cfg ProviderConfig, store identityStore, users userLookup) *Service {
	return &Service{
		cfg:   cfg,
		store: store,
		users: users,
		now:   time.Now,
	}
}

// ResolveOrLink Issue #1120 的历史账号绑定矩阵。
//
// 规则(按顺序短路):
//
//   - 1. (issuer, sub) 命中已绑定 identity 行 → 返回原 UID(场景 3:重复登录)
//
//   - 2. AutoLinkByEmail=true && claims.email_verified:
//     a. user.email 命中 1 条 → 写绑定 → 返回该 UID(场景 3:首次绑历史账号)
//     b. user.email 命中多条 → ErrConflictNeedManual(场景 4:脏数据冲突)
//     c. 未命中 → 走 step 3
//
//   - 3. user.phone 同上(命中 1 / 多条 / 未命中)
//
//   - 4. AllowNewUser=true → 不写本地 user 表,只返回 IsNew=true 让 IssueSession 创建
//     (避免 service 层直接持 user.IService 写权限,职责更清)
//
//   - 5. AllowNewUser=false → ErrUnknownUser
//
// 返回 IsNew=true 时 UID 为空,由 IssueSession 通过 user.IService 创建并回填。
func (s *Service) ResolveOrLink(ctx context.Context, claims *IDTokenClaims) (*ResolveResult, error) {
	if claims == nil || claims.Issuer == "" || claims.Subject == "" {
		return nil, fmt.Errorf("oidc: ResolveOrLink: claims iss/sub required")
	}

	// 1. 已绑定
	if existing, err := s.store.Get(claims.Issuer, claims.Subject); err != nil {
		return nil, fmt.Errorf("oidc: ResolveOrLink: query identity: %w", err)
	} else if existing != nil {
		return &ResolveResult{UID: existing.UID, IsNew: false}, nil
	}

	// 2. 邮箱自动绑定
	//
	// RequireEmailVerified=true 且 email 未验证时,只跳过邮箱绑定分支——
	// 不 return,让 step 3(phone)和 step 4(AllowNewUser)继续有机会命中。
	// 之前直接 return ErrEmailNotVerified 会短路整条矩阵,导致"邮箱未验证但
	// 手机可绑"和"邮箱未验证但允许新建用户"两个合法路径全被堵死。
	emailLinkable := s.cfg.AutoLinkByEmail && claims.Email != "" &&
		(!s.cfg.RequireEmailVerified || claims.EmailVerified)
	if emailLinkable {
		uids, err := s.users.UIDsByEmail(claims.Email)
		if err != nil {
			return nil, fmt.Errorf("oidc: ResolveOrLink: lookup email: %w", err)
		}
		if uid, err := s.linkSingleMatch(uids, claims); err != nil || uid != "" {
			if err != nil {
				return nil, err
			}
			return &ResolveResult{UID: uid, IsNew: false}, nil
		}
	}

	// 3. 手机号自动绑定。
	//
	// PhoneVerified 一律强制 true(没设 RequirePhoneVerified 配置):手机号
	// 在国内是强账号载体,被劫持/未验证就绑可能直接接管账户,默认收紧。
	if s.cfg.AutoLinkByPhone && claims.PhoneNumber != "" && claims.PhoneVerified {
		uids, err := s.users.UIDsByPhone(extractZone(claims.PhoneNumber), extractPhone(claims.PhoneNumber))
		if err != nil {
			return nil, fmt.Errorf("oidc: ResolveOrLink: lookup phone: %w", err)
		}
		if uid, err := s.linkSingleMatch(uids, claims); err != nil || uid != "" {
			if err != nil {
				return nil, err
			}
			return &ResolveResult{UID: uid, IsNew: false}, nil
		}
	}

	// 4/5. 走新建 or 拒绝
	if !s.cfg.AllowNewUser {
		return nil, ErrUnknownUser
	}
	return &ResolveResult{IsNew: true}, nil
}

// IssueSession 委托给 user.IService.LoginByExternalIdentity 签发会话。
//
// 只是个薄壳:负责把 oidc IssueSessionReq 透传到 userLookup,真正逻辑(token /
// IM token / 创建用户)在 user 模块。返回的 LoginRespJSON 由 callback 直接落
// ThirdAuthcode Redis,前端短码轮询取走。
func (s *Service) IssueSession(ctx context.Context, req IssueSessionReq) (*IssueSessionResp, error) {
	if req.UID == "" && !req.CreateUser {
		return nil, fmt.Errorf("oidc: IssueSession: UID required when CreateUser=false")
	}
	resp, err := s.users.IssueSession(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("oidc: issue session: %w", err)
	}
	return resp, nil
}

// linkSingleMatch 把唯一匹配的 uid 写一行 identity 绑定;多匹配返回冲突;无匹配返回("", nil)走下一规则。
func (s *Service) linkSingleMatch(uids []string, claims *IDTokenClaims) (string, error) {
	switch len(uids) {
	case 0:
		return "", nil
	case 1:
		uid := uids[0]
		if err := s.store.Insert(&IdentityModel{
			UID:           uid,
			Issuer:        claims.Issuer,
			Subject:       claims.Subject,
			Email:         claims.Email,
			EmailVerified: boolToInt(claims.EmailVerified),
			Phone:         claims.PhoneNumber,
			PhoneVerified: boolToInt(claims.PhoneVerified),
			LinkedAt:      s.now(),
		}); err != nil {
			return "", fmt.Errorf("oidc: link identity: %w", err)
		}
		return uid, nil
	default:
		return "", ErrConflictNeedManual
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// extractZone 从 E.164(+8613...) 提取国家码;格式不符合返回空(交给上层不绑定)。
//
// 简化处理:仅识别 +86;其他号段交给后续扩展,避免引入复杂依赖。
func extractZone(phone string) string {
	if strings.HasPrefix(phone, "+86") {
		return "0086"
	}
	return ""
}

// extractPhone 去掉 +86 前缀返回纯号码。
func extractPhone(phone string) string {
	if strings.HasPrefix(phone, "+86") {
		return phone[3:]
	}
	return ""
}
