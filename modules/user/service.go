package user

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Mininglamp-OSS/octo-server/modules/source"
	"github.com/Mininglamp-OSS/octo-server/modules/space"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"go.uber.org/zap"
)

var ErrorUserNotExist = errors.New("用户不存在！")

// IService 用户服务接口
type IService interface {
	//获取用户
	GetUser(uid string) (*Resp, error)
	// 获取用户详情（包括与loginUID的关系等等）
	GetUserDetail(uid string, loginUID string) (*UserDetailResp, error)
	// 批量获取用户详情
	GetUserDetails(uids []string, loginUID string) ([]*UserDetailResp, error)
	// 通过用户名获取用户
	GetUserWithUsername(username string) (*Resp, error)
	// 通过用户名获取用户uid集合
	GetUserUIDWithUsernames(usernames []string) ([]string, error)
	// 批量获取用户信息
	GetUsers(uids []string) ([]*Resp, error)
	// 通过APPID获取用户
	GetUsersWithAppID(appID string) ([]*Resp, error)
	// 获取用户集合
	GetUsersWithCategory(category Category) ([]*Resp, error)
	// 获取指定类别的用户列表
	GetUsersWithCategories(categories []string) ([]*Resp, error)
	//查询某个人好友
	GetFriendsWithToUIDs(uid string, toUIDs []string) ([]*FriendResp, error)
	//查询某个用户的所有好友
	GetFriends(uid string) ([]*FriendResp, error)
	//添加一个好友
	AddFriend(uid string, friend *FriendReq) error
	//添加一个用户
	AddUser(user *AddUserReq) error
	// 通过qrvercode获取用户信息
	GetUserWithQRVercode(qrVercode string) (*Resp, error)
	// 获取总用户数量
	GetAllUserCount() (int64, error)
	// 查询某天注册用户数
	GetRegisterWithDate(date string) (int64, error)
	// 获取某个时间区间的注册数量
	GetRegisterCountWithDateSpace(startDate, endDate string) (map[string]int64, error)
	// IsFriend 查询两个用户是否为好友关系
	IsFriend(uid string, toUID string) (bool, error)
	// 获取在线用户
	GetUserOnlineStatus([]string) ([]*OnLineUserResp, error)
	// 更新用户信息
	UpdateUser(req UserUpdateReq) error
	// 获取所有用户
	GetAllUsers() ([]*Resp, error)
	// 更新登录密码
	UpdateLoginPassword(req UpdateLoginPasswordReq) error

	// GetUserSettings 获取用户的配置
	GetUserSettings(uids []string, loginUID string) ([]*SettingResp, error)

	// GetOnetimePrekeyCount 获取用户一次性signal key的数量(决定是否可以开启加密通讯)
	GetOnetimePrekeyCount(uid string) (int, error)

	// 获取设备在线状态
	GetDeviceOnline(uid string, deviceFlag config.DeviceFlag) (*config.OnlinestatusResp, error)
	// 查询在线用户总数量
	GetOnlineCount() (int64, error)
	// 存在黑明单
	ExistBlacklist(uid string, toUID string) (bool, error)
	// 更新用户消息过期时长
	UpdateUserMsgExpireSecond(uid string, msgExpireSecond int64) error
	// 搜索好友
	SearchFriendsWithKeyword(uid string, keyword string) ([]*FriendResp, error)

	// LoginByExternalIdentity 给外部 IdP（OIDC / OAuth）登录流程签发 DMWork 会话。
	//
	// 内部委托给 *User 实现,需要在模块初始化时通过 (*Service).SetExternalLoginHandler 注入。
	// 未注入或调用方为 *Service 以外的 IService 实现时,返回 ErrExternalLoginNotConfigured。
	LoginByExternalIdentity(ctx context.Context, req ExternalLoginReq) (*ExternalLoginResp, error)

	// UpsertVerificationFromOIDC 基于 Aegis OIDC identity_verification scope
	// 返回的 claims 写入 user_verification 表(YUJ-382 / Aegis OIDC Phase 1 直切)。
	//
	// 自 2026-05-10 起替代原先的 verify-service HMAC 回调链路;权威写入口从
	// /v1/internal/verification/complete 转移到 oidc callback(登录时即写)。
	//
	// 语义:幂等 upsert,冲突按 user_id 主键全字段覆盖。调用方(oidc callback)
	// 负责判断 IsVerified / LegalName 非空 — 本方法再做一次防御式校验,空则 no-op。
	// verifiedProvider 必须在 allowlist 白名单内(cas/wecom/feishu),否则返错不写。
	UpsertVerificationFromOIDC(ctx context.Context, uid string, claims OIDCVerificationClaims) error

	// VerifyPasswordByUID 给 OIDC 自助绑定流程做账号密码二次验证(需求 FR-3.1)。
	//
	// 与 username 登录路径同款的 bcrypt/MD5 兼容 + loginGuard 反爆破,但走独立
	// 计数维度 ("oidc-bind:"+uid),避免与登录失败计数互相串扰。已注销/封号账号
	// 视为不可用。
	//
	// 三种返回组合(任一其它组合都是 bug,调用方可断言):
	//   - (true,  "",        nil)   密码正确且账号可用 → 推进绑定流程
	//   - (false, BindReason*, nil) 业务拒绝 → 计入审计,前端按 reason 显示文案
	//                                **(false, "", nil) 视为非法返回**,调用方应当
	//                                以基础设施异常处理(等同 err != nil)
	//   - (false, "",        err)   基础设施异常 / 调用方 contract 违反(如空 uid)
	//                                → 前端走兜底页 + 运维告警,不计审计失败计数
	//
	// reason 取值见 BindReason* 常量。
	//
	// 内部委托给 *User 实现,需通过 (*Service).SetOIDCBindHandler 注入;
	// 未注入时返回 ErrOIDCBindNotConfigured。
	VerifyPasswordByUID(ctx context.Context, uid, password string) (matched bool, reason string, err error)

	// SendOIDCBindSMS 向给定手机号发送 OIDC 绑定 OTP(需求 FR-3.3)。
	//
	// **调用方(oidc 模块)负责保证 zone/phone 来自 OIDC claims 的
	// phone_number/phone_number_verified,不接受用户输入** —— 否则攻击者
	// 可用自己手机绑别人 sub。本方法仅做信道分发,不做来源校验。
	//
	// keyspace 隔离边界(踩坑勘误):
	//   - 验证码本身 (CacheKeySMSCode) 按 codeType 分桶 —— OIDC bind 流程的
	//     OTP 不会被其他流程的验证码覆盖,也不会覆盖别人;
	//   - **但底层 SMSService 的"sms_rate_limit:zone@phone"(1min 发送频率)、
	//     "sms_verify_lock:zone@phone"(10min 失败锁定)、"sms_verify_fail:..."
	//     三个 key 都不带 codeType,跨流程共享**。后果:
	//       a. 用户刚走过 register/forget-pwd SMS 流程,1min 内进 OIDC bind
	//          会被 "发送过于频繁" 挡住;
	//       b. OIDC bind 路径连续输错 3 次,该手机号其他 SMS 流程一并被
	//          锁 10min。
	//   - bind_token 维度的"OTP 发送 ≤ 3 次"(SR-2.1)由 oidc 模块的
	//     BindStore.IncrAndCheck 单独兜底,与本层无关。
	SendOIDCBindSMS(ctx context.Context, zone, phone string) error

	// VerifyOIDCBindSMS 与 SendOIDCBindSMS 对称,复用底层 SMSService.Verify 的
	// 锁定/重试限制。**注意 lock/failCount key 同样不带 codeType,跨流程共享**,
	// 详见 SendOIDCBindSMS 注释。
	VerifyOIDCBindSMS(ctx context.Context, zone, phone, code string) error

	// IsBindable 给 oidc bind Confirm 路径在 identity.Insert 之前再校验一次
	// uid 仍可绑定。
	//
	// 必要性:locator/VerifyPasswordByUID 都只在 verify 阶段过滤
	// (is_destroy + status<>0)。verify→confirm 之间有 5min 用户交互窗口,
	// 期间运维可能 disable / 用户可能自助 destroy。若 confirm 不复核就 Insert,
	// 会给一个已不可绑定的账号写入 user_oidc_identity 行;残留脏数据让该
	// 用户后续 OIDC 登录走 (issuer, sub) autolink 命中 → IssueSession 拒绝 →
	// 死循环登录失败,需要人工 DB 清理。
	//
	// 实务上 TOCTOU 窗口仍 ~毫秒级(本方法返 true 后到 identity.Insert 之间),
	// DB 层 uk_uid_issuer + 登录路径的 status 检查兜底,但本方法把窗口从
	// "用户交互级"压缩到"DB 单次 round-trip 级",符合纵深防御。
	//
	// 返回:
	//   - (true, nil):账号可绑定
	//   - (false, nil):账号不可绑定(已 destroy / 已停用 / 不存在),调用方按业务拒绝处理
	//   - (false, err):基础设施错误,调用方按 internal_error 兜底
	IsBindable(ctx context.Context, uid string) (bool, error)
}

// ErrExternalLoginNotConfigured 外部登录未注入 handler（通常是单测中未走 user.New 完整初始化）
var ErrExternalLoginNotConfigured = errors.New("user: external login handler not configured")

// ErrOIDCBindNotConfigured OIDC 自助绑定 handler 未注入(同 ErrExternalLoginNotConfigured
// 的故障模式:测试或部署时未完整走 user.New 初始化)。
var ErrOIDCBindNotConfigured = errors.New("user: oidc bind handler not configured")

// VerifyPasswordByUID 返回的 reason 枚举。typed const 而非 magic string,既能
// 让调用方 switch-case 时被 govet 检出拼写错误,也作为"matched=false 时
// reason 必非空"不变式的可枚举值集合。
const (
	BindReasonRateLimited      = "rate_limited"      // loginGuard 阈值已到,本次未走密码比对
	BindReasonUserNotFound     = "user_not_found"    // uid 不存在,前端应统一兜底"账号或密码错误"避免枚举
	BindReasonUserUnavailable  = "user_unavailable"  // 账号已注销或被封禁
	BindReasonPasswordMismatch = "password_mismatch" // 密码错误
)

// externalLoginHandler 由 *User 实现,Service 通过 SetExternalLoginHandler 后注入。
//
// 之所以走注入而非直接 import:execLogin / createUserWithRespAndTx 是 *User 的私有方法,
// *User 的构造已依赖 IService（u.userService = NewService(ctx)）,反向再依赖会成环。
type externalLoginHandler interface {
	LoginByExternalIdentity(ctx context.Context, req ExternalLoginReq) (*ExternalLoginResp, error)
}

// oidcBindHandler OIDC 自助绑定流程所需的 *User 内部能力子集:
// 密码二次验证 + 短信 OTP 发送/校验。复用 *User 已持有的 loginGuard / smsServie。
//
// 反向依赖注入(同 externalLoginHandler 模式):*User.New 构造时已经持 IService,
// 想从 Service 调到 *User 的私有字段必须靠注入,直接 import 会成环。
type oidcBindHandler interface {
	VerifyPasswordByUID(ctx context.Context, uid, password string) (matched bool, reason string, err error)
	SendOIDCBindSMS(ctx context.Context, zone, phone string) error
	VerifyOIDCBindSMS(ctx context.Context, zone, phone, code string) error
	IsBindable(ctx context.Context, uid string) (bool, error)
}

// Service Service
type Service struct {
	ctx *config.Context
	db  *DB
	log.Log
	friendDB         *friendDB
	onlineDB         *onlineDB
	settingDB        *SettingDB
	onetimePrekeysDB *onetimePrekeysDB
	onlineService    *OnlineService
	extLogin         externalLoginHandler
	bindHandler      oidcBindHandler
	verificationDB   *verificationDB
}

// SetExternalLoginHandler 注入外部 IdP 登录 handler（在 user.New 内部调用,生产路径下保证非空）
func (s *Service) SetExternalLoginHandler(h externalLoginHandler) {
	s.extLogin = h
}

// SetOIDCBindHandler 注入 OIDC 自助绑定 handler(在 user.New 内部调用,
// 生产路径下保证非空)。未注入时三个 OIDC 绑定方法返回 ErrOIDCBindNotConfigured。
func (s *Service) SetOIDCBindHandler(h oidcBindHandler) {
	s.bindHandler = h
}

// LoginByExternalIdentity 委托给已注入的 handler。未注入时返回 ErrExternalLoginNotConfigured。
func (s *Service) LoginByExternalIdentity(ctx context.Context, req ExternalLoginReq) (*ExternalLoginResp, error) {
	if s.extLogin == nil {
		return nil, ErrExternalLoginNotConfigured
	}
	return s.extLogin.LoginByExternalIdentity(ctx, req)
}

// VerifyPasswordByUID 详见 IService 注释。
func (s *Service) VerifyPasswordByUID(ctx context.Context, uid, password string) (bool, string, error) {
	if s.bindHandler == nil {
		return false, "", ErrOIDCBindNotConfigured
	}
	return s.bindHandler.VerifyPasswordByUID(ctx, uid, password)
}

// SendOIDCBindSMS 详见 IService 注释。
func (s *Service) SendOIDCBindSMS(ctx context.Context, zone, phone string) error {
	if s.bindHandler == nil {
		return ErrOIDCBindNotConfigured
	}
	return s.bindHandler.SendOIDCBindSMS(ctx, zone, phone)
}

// VerifyOIDCBindSMS 详见 IService 注释。
func (s *Service) VerifyOIDCBindSMS(ctx context.Context, zone, phone, code string) error {
	if s.bindHandler == nil {
		return ErrOIDCBindNotConfigured
	}
	return s.bindHandler.VerifyOIDCBindSMS(ctx, zone, phone, code)
}

// IsBindable 详见 IService 注释。
func (s *Service) IsBindable(ctx context.Context, uid string) (bool, error) {
	if s.bindHandler == nil {
		return false, ErrOIDCBindNotConfigured
	}
	return s.bindHandler.IsBindable(ctx, uid)
}

// OIDCVerificationClaims 从 OIDC id_token / userinfo claims 里摘出的实名字段子集。
//
// 在 user 包定义(而不是直接引用 oidc.IDTokenClaims),避免 user → oidc 反向 import
// 环 —— oidc 模块已经 import user。字段命名与 oidc 层一致,方便调用方直接拷贝。
type OIDCVerificationClaims struct {
	// Subject OIDC sub,作为 user_verification.source_sub 写入。
	Subject string
	// VerifiedProvider Aegis 返回的 provider 全名,如 "cas.example.com"。
	// 存库前 strip 到一级(cas),不在 allowlist 内会被拒写。
	VerifiedProvider string
	// VerifiedAt Unix 秒,0 视为"未提供实名完成时间",Upsert 拒绝写入。
	VerifiedAt int64
	// LegalName 实名姓名。空字符串视为未实名,Upsert 拒绝写入。
	LegalName string
	// LegalEmail 实名邮箱,允许空。
	LegalEmail string
}

// oidcVerificationProviderAllowlist 限定可写入 user_verification.source 的 provider 一级名。
// 与 dmwork-verify-service / user_verification 表历史契约保持一致。
//
// Aegis 侧返回的 VerifiedProvider 是完整域名(如 "cas.example.com"),
// strip 到首段后必须命中本集合;未知值(文档外的 IdP / 拼写错误 / 恶意篡改)
// 直接 return error,不写脏数据到生产表。
var oidcVerificationProviderAllowlist = map[string]struct{}{
	"cas":    {},
	"wecom":  {},
	"feishu": {},
}

// UpsertVerificationFromOIDC 详见 IService 注释。
//
// 语义检查顺序:
//  1. uid / VerifiedAt / LegalName 基本必填(防 oidc callback 上游误调)
//  2. VerifiedProvider strip domain → allowlist 白名单
//  3. 复用 verificationDB.Upsert(与 verify-service 写入同一 SQL 路径)
func (s *Service) UpsertVerificationFromOIDC(ctx context.Context, uid string, claims OIDCVerificationClaims) error {
	if uid == "" {
		return errors.New("user: UpsertVerificationFromOIDC: uid required")
	}
	if claims.LegalName == "" {
		return errors.New("user: UpsertVerificationFromOIDC: legal_name empty")
	}
	if claims.VerifiedAt <= 0 {
		return errors.New("user: UpsertVerificationFromOIDC: verified_at invalid")
	}
	// VerifiedProvider 允许为空兜底,但仅当 Aegis 不返回 provider 字段时 ——
	// 目前规范里是必填的,这里空 → 直接拒,防 source 列落 "" 这种脏值。
	source := stripOIDCVerifiedProvider(claims.VerifiedProvider)
	if source == "" {
		return fmt.Errorf("user: UpsertVerificationFromOIDC: verified_provider empty")
	}
	if _, ok := oidcVerificationProviderAllowlist[source]; !ok {
		return fmt.Errorf("user: UpsertVerificationFromOIDC: provider %q not in allowlist", source)
	}

	verifiedAt := time.Unix(claims.VerifiedAt, 0).UTC()
	// source_sub = OIDC sub:跨 Aegis 重签发后可保证同一实名记录被覆盖而非分裂。
	// 空 sub 兜底成空字符串,Upsert SQL 的 source_sub 列是 NOT NULL VARCHAR,空串合法。
	sub := claims.Subject

	m := &verificationModel{
		UserID:     uid,
		RealName:   claims.LegalName,
		Source:     source,
		SourceSub:  sub,
		Email:      nullableVerificationString(claims.LegalEmail),
		VerifiedAt: verifiedAt,
	}
	if err := s.verificationDB.Upsert(m); err != nil {
		return fmt.Errorf("user: UpsertVerificationFromOIDC: db upsert: %w", err)
	}
	return nil
}

// stripOIDCVerifiedProvider 把 Aegis 返回的 verified_provider(如 "cas.example.com")
// 截到首段("cas"),再做 allowlist 白名单校验。
//
// 采用 strings.SplitN(..., 2) 而非 Split(..., ".") 是为了保留"provider 名里本身含点"
// 这种未来扩展可能性(届时只改 allowlist,不改 strip 逻辑)。空串 / 仅 "." 前缀
// 等畸形输入直接返回空串,调用方拒写。
func stripOIDCVerifiedProvider(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	// SplitN(p, ".", 2) 对 "cas" 返回 ["cas"],对 "cas.example.com" 返回
	// ["cas", "example.com"] —— 取 [0] 即目标一级名。
	first := strings.SplitN(p, ".", 2)[0]
	return strings.ToLower(strings.TrimSpace(first))
}

// NewService NewService
func NewService(ctx *config.Context) IService {
	return &Service{
		ctx:              ctx,
		db:               NewDB(ctx),
		friendDB:         newFriendDB(ctx),
		settingDB:        NewSettingDB(ctx.DB()),
		onetimePrekeysDB: newOnetimePrekeysDB(ctx),
		onlineDB:         newOnlineDB(ctx),
		Log:              log.NewTLog("userService"),
		onlineService:    NewOnlineService(ctx),
		verificationDB:   newVerificationDB(ctx),
	}
}

// 获取所有用户
func (s *Service) GetAllUsers() ([]*Resp, error) {
	models, err := s.db.queryAll()
	if err != nil {
		s.Error("查询所有用户错误", zap.Error(err))
		return nil, err
	}
	list := make([]*Resp, 0, len(models))
	for _, user := range models {
		list = append(list, &Resp{
			UID:   user.UID,
			Name:  user.Name,
			Zone:  user.Zone,
			Phone: user.Phone,
		})
	}
	return list, nil
}
func (s *Service) GetUserDetail(uid string, loginUID string) (*UserDetailResp, error) {
	model, err := s.db.QueryDetailByUID(uid, loginUID)
	if err != nil {
		s.Error("查询用户信息失败！", zap.Error(err), zap.String("uid", uid))
		return nil, err
	}
	if model == nil {
		return nil, errors.New("用户信息不存在！")
	}
	onlineM, err := s.onlineDB.queryLastOnlineDeviceWithUID(uid)
	if err != nil {
		s.Error("查询用户在线状态失败", zap.Error(err))
		return nil, err
	}
	var online int
	var lastOffline int
	var deviceFlag config.DeviceFlag
	if onlineM != nil {
		online = onlineM.Online
		lastOffline = onlineM.LastOffline
		deviceFlag = config.DeviceFlag(onlineM.DeviceFlag)
	}
	//查询用户设置
	blacklist := 1
	userSettings, err := s.settingDB.QueryTwoUserSettingModel(uid, loginUID)
	if err != nil {
		s.Error("查询用户设置错误", zap.Error(err))
		return nil, err
	}
	var userSetting *SettingModel
	var toUserSetting *SettingModel
	if len(userSettings) > 0 {
		for _, userSett := range userSettings {
			if userSett.UID == loginUID {
				userSetting = userSett
			} else if userSett.UID == uid {
				toUserSetting = userSett
			}
		}
	}

	if userSetting != nil && userSetting.Blacklist == 1 {
		blacklist = 2
	}
	// 默认打开撤回通知/截屏通知
	if userSetting == nil {
		model.RevokeRemind = 1
		model.Screenshot = 1
		model.Receipt = 1
	}

	friends, err := s.friendDB.queryTwoWithUID(loginUID, uid)
	// isFriend, err := u.friendDB.IsFriend(loginUID, uid)
	if err != nil {
		s.Error("查询是否为好友关系失败", zap.Error(err))
		return nil, err
	}
	var friend *FriendModel
	var toFriend *FriendModel
	if len(friends) > 0 {
		for _, f := range friends {
			if f.UID == loginUID {
				friend = f
			} else if f.UID == uid {
				toFriend = f
			}
		}
	}

	var follow int
	var sourceFrom string
	var remark string
	var beDeleted int
	var beBlacklist int
	var vercode string
	if friend != nil && friend.IsDeleted == 0 {
		follow = 1
		//查询加好友来源
		sourceFrom = source.GetSoruce(friend.SourceVercode)
		if friend.Initiator == 0 && sourceFrom != "" {
			sourceFrom = fmt.Sprintf("对方%s", sourceFrom)
		}

		if toFriend != nil {
			beDeleted = toFriend.IsDeleted

		} else {
			beDeleted = 1
		}
		vercode = friend.Vercode
	}
	if userSetting != nil {
		remark = userSetting.Remark
	}

	if toUserSetting != nil {
		beBlacklist = toUserSetting.Blacklist
	}
	if follow == 0 && model.Robot == 0 {
		if commonSpaceID := space.GetCommonSpaceID(s.ctx, loginUID, uid); commonSpaceID != "" {
			follow = 1
		}
	}
	resp := NewUserDetailResp(model, remark, loginUID, sourceFrom, online, lastOffline, deviceFlag, follow, blacklist, beDeleted, beBlacklist, userSetting, vercode)

	// OCTO 实名认证（YUJ-354）：查询 user_verification，命中即标 verified 并回填 real_name。
	// 失败仅 log 不阻断主响应 —— 实名是增强信息，不能因查询抖动让 profile 不可用。
	//
	// YUJ-398 注记:这里读的是 user_verification **cache**(不是 source of truth,
	// Aegis IdP 才是权威源 — 详见 db_verification.go 文件头)。返回的 RealnameVerified
	// 可能 stale,最大滞后取决于 OIDC callback / OIDC SyncWorker 触发频率;
	// 典型场景 "用户刚在 Aegis 完成实名别人查他 profile" 要等对方下次 OIDC 登录
	// 或 SyncWorker 下轮 tick(生产默认 15min)才会刷新。
	// 不要在此处直接同步拉 Aegis —— profile 热路径 QPS 很高,同步 admin API
	// 会把 Aegis 打挂。
	if vr, vErr := s.verificationDB.QueryByUID(uid); vErr != nil {
		s.Warn("查询实名认证记录失败", zap.Error(vErr), zap.String("uid", uid))
	} else if vr != nil {
		resp.RealnameVerified = true
		resp.RealName = vr.RealName
		if !vr.VerifiedAt.IsZero() {
			resp.RealnameVerifiedAt = vr.VerifiedAt.Unix()
		}
	}

	// 为机器人用户填充bot_commands + 详情
	if model.Robot == 1 {
		var botDetails []struct {
			BotCommands   string `db:"bot_commands"`
			Description   string `db:"description"`
			CreatorUID    string `db:"creator_uid"`
			AutoApprove   int    `db:"auto_approve"`
			AgentPlatform string `db:"agent_platform"`
			AgentVersion  string `db:"agent_version"`
			PluginVersion string `db:"plugin_version"`
		}
		_, err = s.ctx.DB().SelectBySql(
			"SELECT IFNULL(bot_commands,'') as bot_commands, IFNULL(description,'') as description, IFNULL(creator_uid,'') as creator_uid, IFNULL(auto_approve,0) as auto_approve, IFNULL(agent_platform,'') as agent_platform, IFNULL(agent_version,'') as agent_version, IFNULL(plugin_version,'') as plugin_version FROM robot WHERE robot_id = ? AND status=1", uid,
		).Load(&botDetails)
		if err != nil {
			s.Error("查询机器人详情失败", zap.Error(err))
		} else if len(botDetails) > 0 {
			if botDetails[0].BotCommands != "" {
				resp.BotCommands = botDetails[0].BotCommands
			}
			resp.BotDescription = botDetails[0].Description
			resp.BotCreatorUID = botDetails[0].CreatorUID
			resp.BotAutoApprove = botDetails[0].AutoApprove
			resp.BotAgentPlatform = botDetails[0].AgentPlatform
			resp.BotAgentVersion = botDetails[0].AgentVersion
			resp.BotPluginVersion = botDetails[0].PluginVersion
			// Bot 创建者可上传 Bot 头像，对其展示上传入口
			if botDetails[0].CreatorUID == loginUID {
				resp.IsUploadAvatar = 1
			}
			// 查创建者昵称
			if botDetails[0].CreatorUID != "" {
				var creatorName string
				err2 := s.ctx.DB().Select("IFNULL(name,'')").From("user").Where("uid=?", botDetails[0].CreatorUID).LoadOne(&creatorName)
				if err2 == nil {
					resp.BotCreatorName = creatorName
				}
			}
		}
	}

	return resp, nil
}

func (s *Service) GetUserDetails(uids []string, loginUID string) ([]*UserDetailResp, error) {

	userDetails, err := s.db.QueryDetailByUIDs(uids, loginUID)
	if err != nil {
		s.Error("查询用户详情失败！")
		return nil, err
	}
	if userDetails == nil {
		return nil, nil
	}
	onlineStatusResults, err := s.onlineDB.queryUserLastNewOnlines(uids)
	if err != nil {
		s.Error("查询用户在线状态失败", zap.Error(err))
		return nil, err
	}
	onlineStatusResultMap := map[string]*onlineStatusWeightModel{}
	if len(onlineStatusResults) > 0 {
		for _, onlineStatusResult := range onlineStatusResults {
			onlineStatusResultMap[onlineStatusResult.UID] = onlineStatusResult
		}
	}
	// 查询loginUID用户对uids的设置
	settings, err := s.settingDB.QueryUserSettings(uids, loginUID)
	if err != nil {
		return nil, err
	}
	settingMap := map[string]*SettingModel{}
	if len(settings) > 0 {
		for _, setting := range settings {
			settingMap[setting.ToUID] = setting
		}
	}

	// 查询uids对loginUID的设置
	toSettings, err := s.settingDB.QueryWithUidsAndToUID(uids, loginUID)
	if err != nil {
		return nil, err
	}
	toSettingMap := map[string]*SettingModel{}
	if len(toSettings) > 0 {
		for _, toSetting := range toSettings {
			toSettingMap[toSetting.UID] = toSetting
		}
	}
	// 查询loginUID与uids的好友
	friends, err := s.friendDB.queryWithToUIDsAndUID(uids, loginUID)
	if err != nil {
		return nil, err
	}
	friendMap := map[string]*FriendModel{}
	if len(friends) > 0 {
		for _, friend := range friends {
			friendMap[friend.ToUID] = friend
		}
	}
	// 好友来源
	friendVSourceVercodes := make([]string, 0, len(friends))
	for _, friend := range friends {
		friendVSourceVercodes = append(friendVSourceVercodes, friend.SourceVercode)
	}
	friendVercodeSourceMap := map[string]string{}
	if len(friendVSourceVercodes) > 0 {
		friendVercodeSourceMap, err = source.GetSources(friendVSourceVercodes)
		if err != nil {
			return nil, err
		}
		if friendVercodeSourceMap == nil {
			friendVercodeSourceMap = map[string]string{}
		}
	}

	// 查询uids内与loginUID的好友 （查询uids与loginUID的好友）
	toFriends, err := s.friendDB.queryWithToUIDAndUIDs(loginUID, uids)
	if err != nil {
		return nil, err
	}
	toFriendMap := map[string]*FriendModel{}
	if len(friends) > 0 {
		for _, toFriend := range toFriends {
			toFriendMap[toFriend.UID] = toFriend
		}
	}

	userDetailResps := make([]*UserDetailResp, 0)

	for _, userDetail := range userDetails {
		uid := userDetail.UID
		online := 0
		lastOffline := 0
		var deviceFlag config.DeviceFlag
		onlineStatus := onlineStatusResultMap[uid]
		if onlineStatus != nil {
			online = onlineStatus.Online
			lastOffline = onlineStatus.LastOffline
			deviceFlag = config.DeviceFlag(onlineStatus.DeviceFlag)
		}
		follow := 0
		nameRemark := ""
		sourceFrom := ""
		vercode := ""
		friend := friendMap[uid]
		if friend != nil && friend.IsDeleted == 0 {
			follow = 1
			sourceFrom = friendVercodeSourceMap[friend.SourceVercode]
			vercode = friend.Vercode
		}
		if follow == 0 && userDetail.Robot == 0 {
			if commonSpaceID := space.GetCommonSpaceID(s.ctx, loginUID, uid); commonSpaceID != "" {
				follow = 1
			}
		}

		status := 1
		setting := settingMap[uid] // loginUID用户对对方的设置
		if setting != nil {
			if setting.Blacklist == 1 {
				status = 2 // 拉黑
			}
			nameRemark = setting.Remark

		}

		beBlacklist := 0
		toSetting := toSettingMap[uid] // 对方对loginUID用户的设置
		if toSetting != nil {
			if toSetting.Blacklist == 1 {
				beBlacklist = 1
			}
		}

		beDeleted := 0
		toFriend := toFriendMap[uid]
		if toFriend != nil {
			beDeleted = toFriend.IsDeleted
		} else {
			beDeleted = 1
		}
		userDetailResps = append(userDetailResps, NewUserDetailResp(userDetail, nameRemark, loginUID, sourceFrom, online, lastOffline, deviceFlag, follow, status, beDeleted, beBlacklist, setting, vercode))
	}

	// OCTO 实名认证（YUJ-354）：批量查实名记录，回填 verified 与 real_name。失败仅 log。
	if verifiMap, vErr := s.verificationDB.QueryByUIDs(uids); vErr != nil {
		s.Warn("批量查询实名认证记录失败", zap.Error(vErr))
	} else {
		for _, resp := range userDetailResps {
			if vr, ok := verifiMap[resp.UID]; ok && vr != nil {
				resp.RealnameVerified = true
				resp.RealName = vr.RealName
				if !vr.VerifiedAt.IsZero() {
					resp.RealnameVerifiedAt = vr.VerifiedAt.Unix()
				}
			}
		}
	}

	// 为机器人用户填充bot_commands
	robotUIDs := make([]string, 0)
	for _, resp := range userDetailResps {
		if resp.Robot == 1 {
			robotUIDs = append(robotUIDs, resp.UID)
		}
	}
	if len(robotUIDs) > 0 {
		var botDetails []struct {
			RobotID       string `db:"robot_id"`
			BotCommands   string `db:"bot_commands"`
			Description   string `db:"description"`
			CreatorUID    string `db:"creator_uid"`
			AutoApprove   int    `db:"auto_approve"`
			AgentPlatform string `db:"agent_platform"`
			AgentVersion  string `db:"agent_version"`
			PluginVersion string `db:"plugin_version"`
		}
		_, err = s.ctx.DB().SelectBySql(
			"SELECT robot_id, IFNULL(bot_commands,'') as bot_commands, IFNULL(description,'') as description, IFNULL(creator_uid,'') as creator_uid, IFNULL(auto_approve,0) as auto_approve, IFNULL(agent_platform,'') as agent_platform, IFNULL(agent_version,'') as agent_version, IFNULL(plugin_version,'') as plugin_version FROM robot WHERE robot_id in ? AND status=1",
			robotUIDs,
		).Load(&botDetails)
		if err != nil {
			s.Error("查询机器人详情失败", zap.Error(err))
		} else {
			botMap := make(map[string]*struct {
				BotCommands   string
				Description   string
				CreatorUID    string
				AutoApprove   int
				AgentPlatform string
				AgentVersion  string
				PluginVersion string
			}, len(botDetails))
			for i := range botDetails {
				botMap[botDetails[i].RobotID] = &struct {
					BotCommands   string
					Description   string
					CreatorUID    string
					AutoApprove   int
					AgentPlatform string
					AgentVersion  string
					PluginVersion string
				}{
					BotCommands:   botDetails[i].BotCommands,
					Description:   botDetails[i].Description,
					CreatorUID:    botDetails[i].CreatorUID,
					AutoApprove:   botDetails[i].AutoApprove,
					AgentPlatform: botDetails[i].AgentPlatform,
					AgentVersion:  botDetails[i].AgentVersion,
					PluginVersion: botDetails[i].PluginVersion,
				}
			}
			// 批量查创建者昵称
			creatorUIDs := make([]string, 0)
			for _, d := range botDetails {
				if d.CreatorUID != "" {
					creatorUIDs = append(creatorUIDs, d.CreatorUID)
				}
			}
			creatorNameMap := make(map[string]string)
			if len(creatorUIDs) > 0 {
				var nameResults []struct {
					UID  string `db:"uid"`
					Name string `db:"name"`
				}
				_, _ = s.ctx.DB().Select("uid", "IFNULL(name,'') as name").From("user").Where("uid in ?", creatorUIDs).Load(&nameResults)
				for _, n := range nameResults {
					creatorNameMap[n.UID] = n.Name
				}
			}
			for _, resp := range userDetailResps {
				if resp.Robot == 1 {
					if d, ok := botMap[resp.UID]; ok {
						if d.BotCommands != "" {
							resp.BotCommands = d.BotCommands
						}
						resp.BotDescription = d.Description
						resp.BotCreatorUID = d.CreatorUID
						resp.BotCreatorName = creatorNameMap[d.CreatorUID]
						resp.BotAutoApprove = d.AutoApprove
						resp.BotAgentPlatform = d.AgentPlatform
						resp.BotAgentVersion = d.AgentVersion
						resp.BotPluginVersion = d.PluginVersion
						// Bot 创建者可上传 Bot 头像，对其展示上传入口
						if d.CreatorUID == loginUID {
							resp.IsUploadAvatar = 1
						}
					}
				}
			}
		}
	}

	return userDetailResps, nil
}

// GetUserOnlineStatus 查询在线用户
func (s *Service) GetUserOnlineStatus(uids []string) ([]*OnLineUserResp, error) {
	result, err := s.onlineService.GetUserLastOnlineStatus(uids)
	if err != nil {
		s.Error("查询在线用户信息错误", zap.Error(err))
		return nil, err
	}
	if len(result) == 0 {
		return nil, nil
	}
	list := make([]*OnLineUserResp, 0)
	for _, user := range result {
		list = append(list, &OnLineUserResp{
			UID:         user.UID,
			LastOffline: user.LastOffline,
			Online:      user.Online,
			DeviceFlag:  user.DeviceFlag,
		})
	}
	return list, nil
}

// AddUser AddUser
func (s *Service) AddUser(user *AddUserReq) error {
	uid := user.UID
	if strings.TrimSpace(uid) == "" {
		uid = util.GenerUUID()
	}
	username := user.Username
	if strings.TrimSpace(username) == "" {
		username = fmt.Sprintf("%s%s", user.Zone, user.Phone)
	}
	shortNo := user.ShortNo
	if strings.TrimSpace(shortNo) == "" {
		shortNo = util.Ten2Hex(time.Now().UnixNano())
	}
	userM := &Model{
		Name:      user.Name,
		UID:       uid,
		Zone:      user.Zone,
		Phone:     user.Phone,
		Username:  username,
		Email:     user.Email,
		ShortNo:   shortNo,
		Vercode:   fmt.Sprintf("%s@%d", util.GenerUUID(), common.User),
		QRVercode: fmt.Sprintf("%s@%d", util.GenerUUID(), common.QRCode),
		Status:    1,
		Robot:     user.Robot,
	}
	if user.Password != "" {
		hashedPwd, hashErr := HashPassword(user.Password)
		if hashErr != nil {
			return hashErr
		}
		userM.Password = hashedPwd
	}

	err := s.db.Insert(userM)
	if err != nil {
		s.Error("添加用户失败", zap.Error(err))
		return err
	}
	return nil
}

// AddFriend 添加一个好友（若已存在则恢复为有效状态）
func (s *Service) AddFriend(uid string, friend *FriendReq) error {
	err := s.friendDB.InsertOrUpdate(&FriendModel{
		UID:   friend.UID,
		ToUID: friend.ToUID,
	})
	if err != nil {
		s.Error("添加好友失败", zap.Error(err))
		return err
	}
	return nil
}

// GetFriendsWithToUIDs 查询一批好友
func (s *Service) GetFriendsWithToUIDs(uid string, toUIDs []string) ([]*FriendResp, error) {
	friends, err := s.friendDB.QueryFriendsWithUIDs(uid, toUIDs)
	if err != nil {
		s.Error("批量查询用户失败", zap.Error(err))
		return nil, err
	}
	list := make([]*FriendResp, 0)
	for _, friend := range friends {
		list = append(list, &FriendResp{
			Name: friend.ToName,
			UID:  friend.ToUID,
		})
	}
	return list, nil
}

// GetFriends 查询某个用户的所有好友
func (s *Service) GetFriends(uid string) ([]*FriendResp, error) {
	if uid == "" {
		return nil, nil
	}
	friends, err := s.friendDB.QueryFriends(uid)
	if err != nil {
		s.Error("批量查询用户失败", zap.Error(err))
		return nil, err
	}
	list := make([]*FriendResp, 0)
	for _, friend := range friends {
		list = append(list, &FriendResp{
			Name:    friend.ToName,
			UID:     friend.ToUID,
			IsAlone: friend.IsAlone,
		})
	}
	return list, nil
}

// GetUser 获取用户
func (s *Service) GetUser(uid string) (*Resp, error) {
	// App Bot fast path: check app_bot Registry
	if appBotResp := s.resolveAppBot(uid); appBotResp != nil {
		return appBotResp, nil
	}

	userM, err := s.db.QueryByUID(uid)
	if err != nil {
		return nil, err
	}
	if userM == nil {
		return nil, errors.New("用户不存在！")
	}
	if userM.Status != StatusEnable.Int() {
		return nil, errors.New("用户不可用！")
	}

	return newResp(userM), nil
}

// resolveAppBot checks if UID is an App Bot and returns a synthetic Resp.
// Returns nil if not an App Bot or resolver not registered.
// Design: registry is the source of truth for published App Bot identity.
// Consistency with user table is maintained by updateBot (dual-write to registry + user table).
// If registry returns empty name (deleted/unpublished), falls through to nil → DB lookup.
func (s *Service) resolveAppBot(uid string) *Resp {
	if !strings.HasPrefix(uid, "app_") || !strings.HasSuffix(uid, "_bot") {
		return nil
	}
	v := appBotResolverValue.Load()
	if v == nil {
		return nil
	}
	fn := v.(AppBotResolverFunc)
	name := fn(uid)
	if name == "" {
		return nil
	}
	return &Resp{
		UID:   uid,
		Name:  name,
		Robot: 1,
	}
}

// AppBotResolverFunc resolves an App Bot UID to display name. Returns empty if not found.
type AppBotResolverFunc func(uid string) string

// appBotResolverValue stores AppBotResolverFunc, set by the app_bot module to break circular imports.
var appBotResolverValue atomic.Value

// SetAppBotResolver registers the App Bot identity resolver.
func SetAppBotResolver(fn AppBotResolverFunc) {
	appBotResolverValue.Store(fn)
}

// GetUserWithUsername 获取用户
func (s *Service) GetUserWithUsername(username string) (*Resp, error) {
	userM, err := s.db.QueryByUsername(username)
	if err != nil {
		return nil, err
	}
	if userM == nil {
		return nil, nil
	}
	if userM.Status != StatusEnable.Int() {
		return nil, errors.New("用户不可用！")
	}

	return newResp(userM), nil
}

// GetUserUIDWithUsernames 获取用户uid集合
func (s *Service) GetUserUIDWithUsernames(usernames []string) ([]string, error) {
	if len(usernames) == 0 {
		return nil, nil
	}
	return s.db.QueryUIDsByUsernames(usernames)
}

// GetUsers 批量获取用户
func (s *Service) GetUsers(uids []string) ([]*Resp, error) {
	if len(uids) <= 0 {
		return nil, nil
	}
	userModels, err := s.db.queryByUIDs(uids)
	if err != nil {
		return nil, err
	}
	resps := make([]*Resp, 0, len(userModels))
	if len(userModels) > 0 {
		for _, userModel := range userModels {
			resps = append(resps, newResp(userModel))
		}
	}
	return resps, nil
}

// GetUsersWithAppID 通过appID获取用户集合
func (s *Service) GetUsersWithAppID(appID string) ([]*Resp, error) {
	userModels, err := s.db.QueryWithAppID(appID)
	if err != nil {
		return nil, err
	}
	resps := make([]*Resp, 0, len(userModels))
	if len(userModels) > 0 {
		for _, userModel := range userModels {
			resps = append(resps, newResp(userModel))
		}
	}
	return resps, nil
}

// GetUsersWithCategory 获取用户列表
func (s *Service) GetUsersWithCategory(category Category) ([]*Resp, error) {
	userModels, err := s.db.QueryByCategory(string(category))
	if err != nil {
		return nil, err
	}
	resps := make([]*Resp, 0, len(userModels))
	for _, userM := range userModels {
		resps = append(resps, newResp(userM))
	}
	return resps, nil
}

// GetUsersWithCategories 获取指定类别的用户列表
func (s *Service) GetUsersWithCategories(categories []string) ([]*Resp, error) {
	userModels, err := s.db.queryWithCategories(categories)
	if err != nil {
		s.Error("查询用户列表错误", zap.Error(err))
		return nil, err
	}
	resps := make([]*Resp, 0, len(userModels))
	for _, userM := range userModels {
		resps = append(resps, newResp(userM))
	}
	s.Debug("查询用户列表", zap.Strings("categories", categories), zap.Int("count", len(resps)))
	return resps, nil
}

// GetUserWithQRVercode 通过qrvercode获取用户信息
func (s *Service) GetUserWithQRVercode(qrVercode string) (*Resp, error) {
	userModel, err := s.db.queryByQRVerCode(qrVercode)
	if err != nil {
		return nil, err
	}
	if userModel != nil {
		return newResp(userModel), nil
	}
	return nil, nil
}

// GetAllUserCount 获取总用户数量
func (s *Service) GetAllUserCount() (int64, error) {
	count, err := s.db.queryUserCount()
	return count, err
}

// GetRegisterWithDate 查询某天的注册量
func (s *Service) GetRegisterWithDate(date string) (int64, error) {
	count, err := s.db.queryRegisterCountWithDate(date)
	return count, err
}

// GetRegisterCountWithDateSpace 获取某个时间区间的注册数量
func (s *Service) GetRegisterCountWithDateSpace(startDate, endDate string) (map[string]int64, error) {
	list, err := s.db.queryRegisterCountWithDateSpace(startDate, endDate)
	if err != nil {
		s.Error("查询注册用户数据错误", zap.Error(err))
		return nil, err
	}
	result := make(map[string]int64, 0)
	if len(list) > 0 {
		for _, model := range list {
			key := util.Toyyyy_MM_dd(time.Time(model.CreatedAt))
			if _, ok := result[key]; ok {
				//存在某个
				result[key]++
			} else {
				result[key] = 1
			}
		}
	}
	return result, nil
}

// IsFriend 查询两个用户是否为好友关系
func (s *Service) IsFriend(uid string, toUID string) (bool, error) {
	if uid == "" || toUID == "" {
		return false, errors.New("用户ID不能为空")
	}
	model, err := s.friendDB.queryWithUID(uid, toUID)
	if err != nil {
		s.Error("查询好友关系错误", zap.Error(err))
		return false, errors.New("查询好友关系错误")
	}
	isFriend := true
	if model == nil || model.UID == "" || model.IsDeleted == 1 {
		isFriend = false
	}
	return isFriend, nil
}

func (s *Service) UpdateUser(req UserUpdateReq) error {
	updateMap := map[string]interface{}{}
	if req.Name != nil {
		updateMap["name"] = req.Name
	}
	err := s.db.updateUser(updateMap, req.UID)
	if err != nil {
		return err
	}
	return nil
}

func (s *Service) UpdateLoginPassword(req UpdateLoginPasswordReq) error {
	if req.UID == "" {
		return errors.New("uid不能为空！")
	}
	if req.Password == "" {
		return errors.New("原密码不能为空！")
	}
	userM, err := s.db.QueryByUID(req.UID)
	if err != nil {
		return err
	}
	if userM == nil {
		return errors.New("用户不存在！")
	}
	matched, _ := CheckPassword(req.Password, userM.Password)
	if !matched {
		return errors.New("原密码不正确！")
	}

	newHash, hashErr := HashPassword(req.NewPassword)
	if hashErr != nil {
		return hashErr
	}
	err = s.db.updatePassword(newHash, req.UID)
	if err != nil {
		return errors.New("更新密码失败！")
	}

	return nil
}

func (s *Service) GetUserSettings(uids []string, loginUID string) ([]*SettingResp, error) {
	if len(uids) == 0 || loginUID == "" {
		return nil, nil
	}
	settingModels, err := s.settingDB.QueryUserSettings(uids, loginUID)
	if err != nil {
		return nil, err
	}
	settingResps := make([]*SettingResp, 0)
	if len(settingModels) > 0 {
		for _, settingM := range settingModels {
			settingResps = append(settingResps, toSettingResp(settingM))
		}
	}
	return settingResps, nil
}

func (s *Service) GetOnetimePrekeyCount(uid string) (int, error) {
	cn, err := s.onetimePrekeysDB.queryCount(uid)
	return cn, err
}

func (s *Service) GetDeviceOnline(uid string, deviceFlag config.DeviceFlag) (*config.OnlinestatusResp, error) {
	onlineM, err := s.onlineDB.queryOnlineDevice(uid, deviceFlag)
	if err != nil {
		return nil, err
	}
	if onlineM == nil {
		return nil, nil
	}

	return &config.OnlinestatusResp{
		UID:         onlineM.UID,
		DeviceFlag:  onlineM.DeviceFlag,
		LastOffline: onlineM.LastOffline,
		Online:      onlineM.Online,
	}, nil
}

// 查询在线总数量
func (s *Service) GetOnlineCount() (int64, error) {
	count, err := s.onlineService.GetOnlineCount()
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (s *Service) ExistBlacklist(uid string, toUID string) (bool, error) {
	return s.friendDB.existBlacklist(uid, toUID)
}

func (s *Service) UpdateUserMsgExpireSecond(uid string, msgExpireSecond int64) error {
	return s.db.updateUserMsgExpireSecond(uid, msgExpireSecond)
}

// 搜索好友
func (s *Service) SearchFriendsWithKeyword(uid string, keyword string) ([]*FriendResp, error) {
	friends, err := s.friendDB.QueryFriendsWithKeyword(uid, keyword)
	if err != nil {
		s.Error("查询好友数据失败！", zap.Error(err))
		return nil, errors.New("查询好友数据失败！")
	}
	list := make([]*FriendResp, 0)
	if len(friends) > 0 {
		for _, friend := range friends {
			list = append(list, &FriendResp{
				UID:    friend.ToUID,
				Name:   friend.ToName,
				Remark: friend.Remark,
			})
		}
	}
	return list, nil
}

// Resp 用户返回
type Resp struct {
	UID             string
	Name            string
	Zone            string
	Phone           string
	Email           string
	Status         int // 用户状态 1 正常 2:黑名单 0 禁用
	IsUploadAvatar int
	NewMsgNotice    int
	MsgShowDetail   int //显示消息通知详情0.否1.是
	MsgExpireSecond int64
	CreatedAt       int64 // 注册时间 10位时间戳
	IsDestroy       int   // 是否注销
	Robot           int   // 机器人0.否1.是
}

func newResp(m *Model) *Resp {
	return &Resp{
		UID:             m.UID,
		Name:            m.Name,
		Zone:            m.Zone,
		Phone:           m.Phone,
		Email:           m.Email,
		Status:         m.Status,
		IsUploadAvatar: m.IsUploadAvatar,
		NewMsgNotice:    m.NewMsgNotice,
		MsgShowDetail:   m.MsgShowDetail,
		MsgExpireSecond: m.MsgExpireSecond,
		IsDestroy:       m.IsDestroy,
		CreatedAt:       time.Time(m.CreatedAt).Unix(),
		Robot:           m.Robot,
	}
}

// FriendResp 用户好友
type FriendResp struct {
	Name    string
	UID     string
	IsAlone int // 是否为单项好友
	Remark  string
}

// FriendReq FriendReq
type FriendReq struct {
	UID     string
	ToUID   string
	Flag    int
	Version int64
}

// AddUserReq  AddUserReq
type AddUserReq struct {
	Name     string
	UID      string // 如果无值，则随机生成
	Username string
	ShortNo  string // 如果无值，则自动生成
	Zone     string
	Phone    string
	Email    string
	Password string
	Robot    int    // 机器人 0.否 1.是
}

type UserUpdateReq struct {
	UID  string
	Name *string
}

type UpdateLoginPasswordReq struct {
	UID         string // 用户uid
	Password    string // 用户旧密码
	NewPassword string // 用户新密码
}

type SettingResp struct {
	ToUID        string // 用户UID
	UID          string // 用户UID
	Mute         int    // 免打扰
	Top          int    // 置顶
	ChatPwdOn    int    // 是否开启聊天密码
	Screenshot   int    //截屏通知
	RevokeRemind int    //撤回提醒
	Blacklist    int    //黑名单
	Receipt      int    //消息是否回执
	Version      int64  // 版本
}

type OnLineUserResp struct {
	UID         string
	LastOffline int
	Online      int
	DeviceFlag  uint8
}

func toSettingResp(m *SettingModel) *SettingResp {

	return &SettingResp{
		ToUID:        m.ToUID,
		UID:          m.UID,
		Mute:         m.Mute,
		Top:          m.Top,
		ChatPwdOn:    m.ChatPwdOn,
		Screenshot:   m.Screenshot,
		RevokeRemind: m.RevokeRemind,
		Blacklist:    m.Blacklist,
		Receipt:      m.Receipt,
		Version:      m.Version,
	}
}

type UserDetailResp struct {
	UID                 string            `json:"uid"`
	Name                string            `json:"name"`
	Username            string            `json:"username"`
	Email               string            `json:"email,omitempty"`        // email（仅自己能看）
	Zone                string            `json:"zone,omitempty"`         // 手机区号（仅自己能看）
	Phone               string            `json:"phone,omitempty"`        // 手机号（仅自己能看）
	Mute                int               `json:"mute"`                   // 免打扰
	Top                 int               `json:"top"`                    // 置顶
	Sex                 int               `json:"sex"`                    //性别1:男
	Category            string            `json:"category"`               //用户分类 '客服'
	ShortNo             string            `json:"short_no"`               // 用户唯一短编号
	ChatPwdOn           int               `json:"chat_pwd_on"`            //是否开启聊天密码
	Screenshot          int               `json:"screenshot"`             //截屏通知
	RevokeRemind        int               `json:"revoke_remind"`          //撤回提醒
	Receipt             int               `json:"receipt"`                //消息是否回执
	Online              int               `json:"online"`                 //是否在线
	LastOffline         int               `json:"last_offline"`           //最后一次离线时间
	DeviceFlag          config.DeviceFlag `json:"device_flag"`            // 在线设备标记
	Follow              int               `json:"follow"`                 //是否是好友
	BeDeleted           int               `json:"be_deleted"`             // 被删除
	BeBlacklist         int               `json:"be_blacklist"`           // 被拉黑
	Code                string            `json:"code"`                   //加好友所需vercode TODO: code不再使用 请使用Vercode
	Vercode             string            `json:"vercode"`                //
	SourceDesc          string            `json:"source_desc"`            // 好友来源
	Remark              string            `json:"remark"`                 //好友备注
	IsUploadAvatar      int               `json:"is_upload_avatar"`       // 是否上传头像
	Status              int               `json:"status"`                 //用户状态 1 正常 2:黑名单
	Robot               int               `json:"robot"`                  // 机器人0.否1.是
	BotCommands         string            `json:"bot_commands,omitempty"`    // 机器人命令列表JSON
	BotDescription      string            `json:"bot_description,omitempty"` // Bot 简介
	BotCreatorUID       string            `json:"bot_creator_uid,omitempty"` // Bot 创建者 UID
	BotCreatorName      string            `json:"bot_creator_name,omitempty"` // Bot 创建者昵称
	BotAutoApprove      int               `json:"bot_auto_approve,omitempty"` // 是否自动通过好友 0:否 1:是
	BotAgentPlatform    string            `json:"bot_agent_platform,omitempty"` // Agent 平台名称
	BotAgentVersion     string            `json:"bot_agent_version,omitempty"`  // Agent 平台版本号
	BotPluginVersion    string            `json:"bot_plugin_version,omitempty"` // DMWork 插件版本号
	IsDestroy           int               `json:"is_destroy"`             // 注销状态 0.正常 1.注销申请中（冷静期，仍可正常通信） 2.已注销
	Flame               int               `json:"flame"`                  // 是否开启阅后即焚
	FlameSecond         int               `json:"flame_second"`           // 阅后即焚秒数
	JoinGroupInviteUID  string            `json:"join_group_invite_uid"`  // 加入群聊邀请人UID
	JoinGroupInviteName string            `json:"join_group_invite_name"` // 加入群聊邀请人名称
	JoinGroupTime       string            `json:"join_group_time"`        // 加入群聊时间
	GroupMember         *GroupMemberResp  `json:"group_member,omitempty"` // 群成员信息
	// OCTO 实名认证（YUJ-354 / GH#1300）
	// RealnameVerified：该用户是否已完成 OCTO 实名认证（user_verification 有记录）。
	// RealName：已认证时返回实名姓名；未认证留空。
	// 目前对外可见（含非自己），因为实名结果会进入前端 displayName() 展示；
	// 若后续产品要求"仅自己可见"，改为仅当 loginUID == m.UID 时填充即可。
	RealnameVerified bool   `json:"realname_verified"`
	RealName         string `json:"real_name,omitempty"`
	// RealnameVerifiedAt：实名认证完成时间 (Unix 秒)。未认证时为 0 被 omitempty 剥离。
	// 三端客户端（Web/Android/iOS）在 Custom Tabs 实名回跳后读此字段做
	// 「已认证 · YYYY-MM」展示；同时与 login / GET /v1/user/current 下发的
	// 字段名保持一致（YUJ-413）。
	RealnameVerifiedAt int64 `json:"realname_verified_at,omitempty"`
}

type GroupMemberResp struct {
	UID                string `json:"uid"`                  // 成员uid
	GroupNo            string `json:"group_no"`             // 群唯一编号
	Name               string `json:"name"`                 // 群成员名称
	Remark             string `json:"remark"`               // 成员备注
	Role               int    `json:"role"`                 // 成员角色
	IsDeleted          int    `json:"is_deleted"`           // 是否删除
	Status             int    `json:"status"`               //成员状态0:正常，2:黑名单
	Vercode            string `json:"vercode"`              // 验证码
	InviteUID          string `json:"invite_uid"`           // 邀请人
	Robot              int    `json:"robot"`                // 机器人
	ForbiddenExpirTime int64  `json:"forbidden_expir_time"` // 禁言时长
	// YUJ-206：透传群成员的外部来源/归属 Space 字段，命名与 /groups/{no}/members
	// 的 memberDetailResp 保持一致，供 Web/Android/iOS UserInfo 判定"同 Space 非
	// 好友 → 直接发消息" vs "跨 Space 外部成员 → 仅可在群内交流"。
	// 后端保留 IsExternal / SourceSpaceID / SourceSpaceName 的绝对语义不变；
	// HomeSpaceID / HomeSpaceName 是对齐企微相对视角的视图字段：
	//   外部成员 (is_external == 1) → home_space_id = source_space_id
	//   内部成员                     → home_space_id = group.space_id
	IsExternal      int    `json:"is_external"`       // 是否外部成员 0/1
	SourceSpaceID   string `json:"source_space_id"`   // 来源 Space ID（外部成员使用）
	SourceSpaceName string `json:"source_space_name"` // 来源 Space 名称
	HomeSpaceID     string `json:"home_space_id"`     // 成员归属 Space ID（相对视角）
	HomeSpaceName   string `json:"home_space_name"`   // 成员归属 Space 名称
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

func NewUserDetailResp(m *Detail, remark, loginUID string, sourceFrom string, onLine int, lastOffline int, deviceFlag config.DeviceFlag, follow int, status int, beDeleted int, beBlacklist int, setting *SettingModel, vercode string) *UserDetailResp {
	self := loginUID == m.UID

	email := ""
	phone := ""
	zone := ""
	username := ""
	if self {
		email = m.Email
		phone = m.Phone
		zone = m.Zone
	}
	if m.Robot == 1 {
		username = m.Username
	}
	var flame int
	var flameSecond int
	if setting != nil {
		flame = setting.Flame
		flameSecond = setting.FlameSecond

	}

	return &UserDetailResp{
		UID:            m.UID,
		Name:           m.Name,
		Email:          email,
		Zone:           zone,
		Phone:          phone,
		Mute:           m.Mute,
		Top:            m.Top,
		Sex:            m.Sex,
		ChatPwdOn:      m.ChatPwdOn,
		Category:       m.Category,
		ShortNo:        m.ShortNo,
		Screenshot:     m.Screenshot,
		RevokeRemind:   m.RevokeRemind,
		Receipt:        m.Receipt,
		Online:         onLine,
		LastOffline:    lastOffline,
		DeviceFlag:     deviceFlag,
		Follow:         follow,
		SourceDesc:     sourceFrom,
		Remark:         remark,
		IsUploadAvatar: m.IsUploadAvatar,
		Status:         status,
		Robot:          m.Robot,
		Username:       username,
		BeDeleted:      beDeleted,
		BeBlacklist:    beBlacklist,
		IsDestroy:      m.IsDestroy,
		Flame:          flame,
		FlameSecond:    flameSecond,
		Vercode:        vercode,
	}
}
