package user

import (
	"context"
	"fmt"

	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
)

// oidcBindGuardPrefix loginGuard 在 OIDC 自助绑定路径上的独立 keyspace 前缀。
//
// 与 username/email 登录路径(直接用 account 做 key)分开:
//   - uid 维度而非 account 维度,匹配需求 SR-2.2 "每个被尝试绑定的 dmwork uid:
//     当日 ≤ 10 次失败验证";
//   - 独立 prefix 避免与登录失败计数互相累计,防止"用户刚登错过一次密码,
//     立马进 OIDC 绑定就被锁定"的体验问题。
const oidcBindGuardPrefix = "oidc-bind:"

// VerifyPasswordByUID 详见 IService 注释。
//
// 失败维度按 uid 计:配合 SR-2.2 "每 uid 当日 ≤ 10 次失败"。当前用与登录共用
// 的 loginGuard 默认阈值(5 次/15min),P0 上线后视实际反爆破效果再调
// oidcBindGuardPrefix 维度的独立阈值。
//
// 已注销/封号账号视为不可用,与 api_usernamelogin.go 行为对齐 ——
// 攻击者用 OIDC bind 探测一个已注销账号的密码与登录路径同样无意义。
func (u *User) VerifyPasswordByUID(_ context.Context, uid, password string) (bool, string, error) {
	if uid == "" {
		// caller contract 违反归 err 维度,与 IService 文档一致 ——
		// reason 是给前端的业务文案,不该出现"程序员忘传参数"这种值。
		return false, "", fmt.Errorf("oidc bind: uid required")
	}
	guardKey := oidcBindGuardPrefix + uid
	if err := u.loginGuard.Check(guardKey); err != nil {
		// loginGuard.Check 返 ErrLoginLocked 时不计失败:本次根本没走密码比对,
		// 再 +1 等于把窗口续期到永远。
		return false, BindReasonRateLimited, nil
	}
	m, err := u.db.QueryByUID(uid)
	if err != nil {
		// DB 异常:不计失败(避免基础设施抖动把所有 uid 误锁),让调用方按 err 兜底。
		return false, "", fmt.Errorf("oidc bind: query user by uid: %w", err)
	}
	if m == nil {
		// "uid 不存在"是探测攻击的必到信号 —— 计入失败计数。reason 仍区分原因
		// 是为审计/告警,前端文案应统一兜底"账号或密码错误"避免账号枚举。
		u.loginGuard.RecordFailureLogged(guardKey)
		return false, BindReasonUserNotFound, nil
	}
	if m.IsDestroy == IsDestroyDone || m.Status == 0 {
		u.loginGuard.RecordFailureLogged(guardKey)
		return false, BindReasonUserUnavailable, nil
	}
	matched, needsMigration := CheckPassword(password, m.Password)
	if !matched {
		u.loginGuard.RecordFailureLogged(guardKey)
		return false, BindReasonPasswordMismatch, nil
	}
	u.loginGuard.ResetLogged(guardKey)
	// 与 api_usernamelogin.go 同款自动迁移:MD5 旧 hash 比对成功后写回 bcrypt。
	// 写失败不阻塞绑定流程(下次登录还会再试)。
	if needsMigration {
		if newHash, hashErr := HashPassword(password); hashErr == nil {
			_ = u.db.updatePassword(newHash, uid)
		}
	}
	return true, "", nil
}

// SendOIDCBindSMS 详见 IService 注释(含跨流程 keyspace 共享勘误)。
//
// **安全契约(必须读)**:本方法不做"phone 来自 OIDC claims 而非用户输入"的来源校验,
// 因为 oidc 模块在 callback 阶段已经拿着可信 claims 转发过来。**如果未来
// 增加新的调用方,必须在该调用方内部确保 zone/phone 不可被用户控制**,否则:
//   - 攻击者可用自己手机号 + 任意 OIDC sub 发起绑定流程,把别人 sub 绑到
//     自己 dmwork 账号(违反 FR-3.3 / SR-4 反伪造手机号)
//   - 攻击者可批量请求发短信制造账号枚举 / 短信轰炸
//
// 实务防护(分层互补):
//   - **本层**:commonapi.SMSService 内置 1min 发送频率 + 10min 失败锁定,
//     但锁定 key 不带 codeType(详见 IService 注释),与 register/forget-pwd
//     等其他 SMS 流程共享 -> 提供"全局手机号粒度"的反滥用;
//   - **oidc 模块 BindService 层**:OCTO_OIDC_BIND_ISSUER_ALLOWLIST 收口 +
//     bind_token 单次消费 + Redis 5min TTL + BindStore.IncrAndCheck
//     (bind_token 维度的 OTP 发送/校验/确认 counter, 对应 SR-2.1)
//     -> 提供"OIDC bind 流程粒度"的反爆破;
//   - 两层叠加,phone 输入来源校验是调用方契约,不在本层兜底。
func (u *User) SendOIDCBindSMS(ctx context.Context, zone, phone string) error {
	if zone == "" || phone == "" {
		return fmt.Errorf("oidc bind sms: zone and phone required")
	}
	if err := u.smsServie.SendVerifyCode(ctx, zone, phone, commonapi.CodeTypeOIDCBind); err != nil {
		return fmt.Errorf("oidc bind sms send: %w", err)
	}
	return nil
}

// VerifyOIDCBindSMS 详见 IService 注释(含跨流程 keyspace 共享勘误)。
//
// 反爆破隔离边界:
//   - 与 loginGuard 的密码反爆破计数器独立(后者用 "oidc-bind:"+uid 前缀,
//     SMSService 用 zone@phone 前缀)—— 用户即便密码尝试已被限流,仍可走
//     短信路径,反之亦然;
//   - 但底层 SMSService 的"3 次失败锁定 10 分钟"行为 lock key 不带 codeType,
//     与 register / forget-pwd / login_check_phone / destroy / email_login
//     等所有 SMS 流程共享 —— OIDC bind 路径错 3 次也会把该手机号其他 SMS
//     流程一并锁 10min(详见 IService.SendOIDCBindSMS 注释)。如需 codeType
//     维度的独立锁,P0 之后单独评估改造 commonapi.SMSService(影响 5 个调
//     用方,需 ops 决策)。
func (u *User) VerifyOIDCBindSMS(ctx context.Context, zone, phone, code string) error {
	if zone == "" || phone == "" || code == "" {
		return fmt.Errorf("oidc bind sms verify: zone/phone/code required")
	}
	if err := u.smsServie.Verify(ctx, zone, phone, code, commonapi.CodeTypeOIDCBind); err != nil {
		return fmt.Errorf("oidc bind sms verify: %w", err)
	}
	return nil
}

// IsBindable 详见 IService 注释。
//
// 可绑定条件比 VerifyPasswordByUID 更严:locator (oidc.dbBindLocator + DB
// QueryUIDsByEmail/Phone) 已经用 `is_destroy=0 AND status<>0` 过滤,本方法
// 必须与 locator 完全一致 —— 否则"verify 时被 locator 拒,改用别的入口
// 绕到 confirm"或"verify 通过后进入冷静期"两种场景就会出现 verify/confirm
// 不一致。
//
// 比 VerifyPasswordByUID 的 `IsDestroyDone || Status == 0` 多排除一个状态:
// is_destroy=1(冷静期内可撤销注销)。理由:5min bind 窗口内被用户主动发起
// 注销的账号,不该允许新 OIDC 身份绑定上去。
//
// 不计入 loginGuard 失败计数:Confirm 阶段调用,uid 已在 verify 阶段通过
// 密码/OTP 校验,这里只是状态二次确认。如果计失败会让"管理员在用户绑定
// 中途 disable"演变成"该 uid 被错锁 15min",体验差且无安全收益。
func (u *User) IsBindable(_ context.Context, uid string) (bool, error) {
	if uid == "" {
		return false, fmt.Errorf("oidc bind IsBindable: uid required")
	}
	m, err := u.db.QueryByUID(uid)
	if err != nil {
		return false, fmt.Errorf("oidc bind IsBindable: query user by uid: %w", err)
	}
	if m == nil {
		return false, nil
	}
	if m.IsDestroy != 0 || m.Status == 0 {
		return false, nil
	}
	return true, nil
}
