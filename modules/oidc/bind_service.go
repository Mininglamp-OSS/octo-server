package oidc

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

// BindAuthenticator OIDC 自助绑定流程对 user 模块的最小依赖接口。
//
// 生产路径下由 user.IService 直接实现(同 verificationUpserter 模式);
// 测试用 fake 注入断言入参。三方法签名与 user.IService 完全一致,这里
// 在 oidc 包内重新声明是为了:
//   - 遵循 "Accept interfaces, return structs":小接口在使用方包内定义;
//   - 让 bind_service_test.go 不必拉起整个 user.Service 就能跑契约测试。
type BindAuthenticator interface {
	VerifyPasswordByUID(ctx context.Context, uid, password string) (matched bool, reason string, err error)
	SendOIDCBindSMS(ctx context.Context, zone, phone string) error
	VerifyOIDCBindSMS(ctx context.Context, zone, phone, code string) error
	// IsBindable Confirm 路径在 identity.Insert 前对 CandidateUID 做最后一次
	// 状态复核。详见 user.IService.IsBindable godoc。
	IsBindable(ctx context.Context, uid string) (bool, error)
}

// BindLocator 把用户输入(username)或 claims phone 解析到 dmwork uid。
//
// 多匹配场景按需扩展(P0 假定 username 全局唯一);UIDsByPhone 返回切片是因为
// dmwork user 表的 (zone, phone) 不是强唯一约束 —— 历史脏数据可能有重复。
// VerifySMS 路径多匹配 → ErrBindConflictNeedManual,走 P1 Admin 兜底。
type BindLocator interface {
	UIDByUsername(username string) (string, error)
	UIDsByPhone(zone, phone string) ([]string, error)
}

// BindInfoResp /info 端点返回给前端的脱敏身份信息(FR-2)。
//
// 不含 sub / issuer / claims 原值,避免社工攻击(SR-7)。
type BindInfoResp struct {
	MaskedEmail    string       `json:"masked_email,omitempty"`
	MaskedPhone    string       `json:"masked_phone,omitempty"`
	Name           string       `json:"name,omitempty"`
	Methods        []BindMethod `json:"methods"`
	SupportContact string       `json:"support_contact,omitempty"`
	AllowCreate    bool         `json:"allow_create"`
	// CreateBlocked 始终序列化(无 omitempty):/bind/info 协议约定该字段恒在,
	// 前端按字符串值分支("" / "disabled" / "claims_incomplete" /
	// "manual_conflict" / "consumed");omitempty 会让 "" 路径整字段消失,
	// 等价于前端要给"字段缺失"和"字段为空字符串"两种状态都写分支。
	CreateBlocked string `json:"create_blocked"`
}

// BindService 自助绑定状态机的业务逻辑层。
//
// 不持有 HTTP 上下文 / *wkhttp.Context;handler 层负责 HTTP 解析、CallbackGuard
// IP 限流、审计写入(events 在 model.go 已就位)。
//
// identity / users 仅 Confirm 路径用到,在 Init() 检测到 Bind.Enabled=true
// 时统一注入(见 api.go:202-203)。其他 5 个方法(Issue/Info/Verify*/SendSMS)
// 不依赖这两个字段,单测可以不注入。
type BindService struct {
	cfg      BindConfig
	store    BindStore
	auth     BindAuthenticator
	locator  BindLocator
	identity identityStore // confirm 路径写 user_oidc_identity
	users    userLookup    // confirm 路径调 IssueSession 签发 dmwork 会话
}

func newBindService(cfg BindConfig, store BindStore, auth BindAuthenticator, locator BindLocator) *BindService {
	return &BindService{cfg: cfg, store: store, auth: auth, locator: locator}
}

// Issue 在 callback ResolveOrLink 失败分支调用:签发 bind_token,持久化
// claims + state_data 快照,返回 jti 供 handler 拼前端跳转 URL。
//
// 等价于 IssueWithReason(ctx, claims, sd, BindReasonUnknownUser) —— 历史调用方
// (单元测试 + ShouldHandle 早期路径)在没有明确区分原因时保留旧签名,语义即
// "claims 未命中已有账号"(可走 /bind/create)。生产 callback 路径应该走
// IssueWithReason 明确传 reason,与 manual_conflict 拒建号路径协同。
//
// 不在此处写 audit —— handler 在拿到 jti 后统一写 EventBindIssued
// (handler 持 HTTP 上下文,IP/UA/trace_id 都齐)。
func (s *BindService) Issue(ctx context.Context, claims *IDTokenClaims, sd *StateData) (string, error) {
	return s.IssueWithReason(ctx, claims, sd, BindReasonUnknownUser)
}

// IssueWithReason 同 Issue,但额外把 reason 固化到 BindSession.IssueReason 字段。
// callback 按 ResolveOrLink 返回的 err 类型选择 reason:ErrUnknownUser →
// BindReasonUnknownUser(可建号);ErrConflictNeedManual → BindReasonManualConflict
// (Create 路径拒绝)。详见 IssueReason godoc。
func (s *BindService) IssueWithReason(ctx context.Context, claims *IDTokenClaims, sd *StateData, reason IssueReason) (string, error) {
	if claims == nil || claims.Issuer == "" || claims.Subject == "" {
		return "", fmt.Errorf("oidc bind Issue: claims iss/sub required")
	}
	if sd == nil {
		return "", fmt.Errorf("oidc bind Issue: state data required")
	}
	jti, err := newBindJTI()
	if err != nil {
		return "", fmt.Errorf("oidc bind Issue: jti: %w", err)
	}
	claimsRaw, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("oidc bind Issue: marshal claims: %w", err)
	}
	sdRaw, err := json.Marshal(sd)
	if err != nil {
		return "", fmt.Errorf("oidc bind Issue: marshal sd: %w", err)
	}
	sess := &BindSession{
		JTI:            jti,
		Issuer:         claims.Issuer,
		Subject:        claims.Subject,
		Status:         BindStatusIssued,
		ClaimsSnapshot: claimsRaw,
		SDSnapshot:     sdRaw,
		OriginIP:       sd.IP,
		OriginUA:       sd.UserAgent,
		CreatedAt:      nowUnix(),
		IssueReason:    reason,
	}
	if err := s.store.Save(ctx, sess, s.cfg.TokenTTL); err != nil {
		return "", fmt.Errorf("oidc bind Issue: save: %w", err)
	}
	return jti, nil
}

// Info 返回脱敏 claims + 可用方法 + create 能力状态。
// 可用方法 = 配置 Methods ∩ 当前 claims 支持的手段(claims 无 verified phone → 屏蔽 sms_otp,FR-3.3)。
func (s *BindService) Info(ctx context.Context, jti string) (*BindInfoResp, error) {
	sess, err := s.store.Get(ctx, jti)
	if err != nil {
		return nil, err
	}
	claims, err := decodeClaimsSnapshot(sess.ClaimsSnapshot)
	if err != nil {
		return nil, err
	}
	resp := &BindInfoResp{
		MaskedEmail:    maskEmailForBind(claims.Email),
		MaskedPhone:    maskPhoneForBind(claims.PhoneNumber),
		Name:           claims.Name,
		SupportContact: s.cfg.SupportContact,
	}
	resp.Methods = s.availableMethods(claims)
	resp.AllowCreate = s.cfg.AllowCreate
	// 优先级 disabled > claims_incomplete > manual_conflict > consumed。
	// disabled 是配置层面的"运维关闭",最高优先;claims_incomplete 是
	// claims 自身欠缺,继续往下走也注定建不出;manual_conflict 是 token 来源
	// 的策略拒绝(reviewer 强调 P2-1);consumed 是 token 已被推进出 issued
	// 状态(verify/create 已发生),前端最多展示"重发起 OIDC 登录"提示。
	switch {
	case !s.cfg.AllowCreate:
		resp.CreateBlocked = "disabled"
	case s.checkClaimsForCreate(claims) != nil:
		resp.CreateBlocked = "claims_incomplete"
	case sess.IssueReason == BindReasonManualConflict:
		resp.CreateBlocked = "manual_conflict"
	case sess.Status != BindStatusIssued:
		// token 已被推进(verified / creating / refused) —— 二次 /bind/info 仍可读,
		// 但 /bind/create 已经不可能走通(状态机 CAS 必败)。提前告诉前端别让用户
		// 误点"自助建号"按钮反复 429。
		resp.CreateBlocked = "consumed"
	}
	return resp, nil
}

// availableMethods 配置 ∩ 当前 claims 支持。phone 未验证就剔 sms_otp,
// 让前端不会展示一个"发不出短信"的按钮。
func (s *BindService) availableMethods(claims *IDTokenClaims) []BindMethod {
	out := make([]BindMethod, 0, len(s.cfg.Methods))
	for _, m := range s.cfg.Methods {
		if m == BindMethodSMSOTP && (claims.PhoneNumber == "" || !claims.PhoneVerified) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// VerifyPassword 用户输入 (jti, identifier, password) → locator 解析 uid →
// auth.VerifyPasswordByUID → 推进状态机到 verified。
//
// 限流:每 jti 维度的"verify 尝试"counter +1(SR-2.1, VerifyMax 阈值),超返
// ErrBindRateLimited。密码 / 短信共用同一 counter —— 需求文档 SR-2.1 说
// "验证尝试 ≤ 5 次",不区分手段。
func (s *BindService) VerifyPassword(ctx context.Context, jti, identifier, password string) error {
	if !s.methodEnabled(BindMethodPassword) {
		return ErrBindMethodDisabled
	}
	sess, err := s.store.Get(ctx, jti)
	if err != nil {
		return err
	}
	// 状态机守卫:只有 issued 才能进 verify,verified/confirmed 都拒绝(409)。
	// 防身份接管:攻击者拿到 victim bind_token(已 verified)再调一次 verify
	// 用自己账号 → 覆盖 CandidateUID → confirm 时绑到攻击者 uid。SR-1/SR-5。
	if sess.Status != BindStatusIssued {
		return ErrBindStatusConflict
	}
	if _, err := s.store.IncrAndCheck(ctx,
		"bind:verify:"+jti, s.cfg.VerifyMax, s.cfg.TokenTTL); err != nil {
		return err
	}
	uid, lerr := s.locator.UIDByUsername(identifier)
	if lerr != nil {
		return fmt.Errorf("oidc bind VerifyPassword: locate uid: %w", lerr)
	}
	if uid == "" {
		// 不暴露"用户存在 vs 密码错"差异(SR-6 反账号枚举)。上层统一兜底文案。
		// wrap ErrBindAuthRejected → handler 翻 401(与密码错路径一致),
		// 避免归到 internal_error metric 污染告警。
		//
		// 仍然消费 uid-fail 限流配额(用 identifier 哈希作为 dimension),
		// 否则"未知用户路径免费"会形成副信道:攻击者可以从响应延迟之外的
		// 维度(自己计数请求是否被限)区分"用户存在"与"用户不存在",绕过 SR-6。
		// 用 subHash(identifier) 而非原文,避免攻击者通过 Redis 监控反查 identifier。
		if _, lerr := s.store.IncrAndCheck(ctx,
			bindEnumFailKey(identifierKeyHash(identifier)), s.cfg.UIDFailPerDay, uidFailWindow); lerr != nil {
			return lerr
		}
		return fmt.Errorf("oidc bind VerifyPassword: %w (unknown identifier)", ErrBindAuthRejected)
	}
	matched, reason, aerr := s.auth.VerifyPasswordByUID(ctx, uid, password)
	if aerr != nil {
		// 内部错误(DB/网络):wrap aerr,handler 看不到 ErrBindAuthRejected,
		// metric 落 internal_error / HTTP 500。
		return fmt.Errorf("oidc bind VerifyPassword: auth: %w", aerr)
	}
	if !matched {
		// user 模块 loginGuard 已经把当前 uid 锁定 —— 把 reason 透传成限流,
		// 让 handler 翻 429 + metric 落 rate_limited 而不是 401/unauthorized。
		// 否则同一份"账号被锁"语义在 dashboard 上会被 unauthorized 桶吃掉,
		// 与 PR 一直强调的"sentinel/HTTP 状态/metric label 三方对齐"违背。
		// 不再消耗 SR-2.2 uid-fail 配额(配额是给"密码错"用的,loginGuard 锁定
		// 期间根本没走密码比对,再 +1 会延长锁定窗口)。
		if reason == userBindReasonRateLimited {
			return fmt.Errorf("oidc bind VerifyPassword: %w (user loginGuard locked uid)", ErrBindRateLimited)
		}
		// SR-2.2 uid 维度防爆破:同 uid 跨 token 的密码失败累计达 UIDFailPerDay
		// 直接限流(优先于 ErrBindAuthRejected 返回,让攻击者无法用换 token 的
		// 方式绕过 per-token 的 VerifyMax)。24h 窗口固定 —— 与 TokenTTL 解耦,
		// SR-2.2 字面"每 uid 每日失败 ≤ 10 次"。
		if _, lerr := s.store.IncrAndCheck(ctx,
			bindUIDFailKey(uid), s.cfg.UIDFailPerDay, uidFailWindow); lerr != nil {
			return lerr
		}
		// 业务拒绝:wrap ErrBindAuthRejected,handler 翻 401。
		// reason 只用于 zap.Error / service 层日志,不直接给客户端 / 审计 reason 列。
		return fmt.Errorf("oidc bind VerifyPassword: %w (%s)", ErrBindAuthRejected, reason)
	}
	// 在内存 sess 上写齐 verified 三件套(Status + CandidateUID + VerifiedMethod),
	// 然后通过 CASSave 一次性原子提交;状态机迁移 + 业务字段写入合并成单次
	// Redis 操作,避免 plain Save 在并发场景下被覆盖。
	sess.CandidateUID = uid
	sess.VerifiedMethod = BindMethodPassword
	sess.Status = BindStatusVerified
	return s.saveVerified(ctx, sess)
}

// SendSMS 走短信路径,zone/phone 从 claims snapshot 取(FR-3.3)。
//
// 限流:每 jti 维度的"OTP 发送"counter +1(SR-2.1, OTPSendMax 阈值,默认 3 次),
// 与底层 commonapi.SMSService 的 1min/手机号全局节流互补 ——
// 后者不带 codeType 跨流程串扰(详见 user.SendOIDCBindSMS godoc),所以
// 这里的 counter 是 bind_token 维度的真正反爆破。
func (s *BindService) SendSMS(ctx context.Context, jti string) error {
	if !s.methodEnabled(BindMethodSMSOTP) {
		return ErrBindMethodDisabled
	}
	sess, err := s.store.Get(ctx, jti)
	if err != nil {
		return err
	}
	claims, err := decodeClaimsSnapshot(sess.ClaimsSnapshot)
	if err != nil {
		return err
	}
	zone, phone := extractZone(claims.PhoneNumber), extractPhone(claims.PhoneNumber)
	if !claims.PhoneVerified || phone == "" {
		return ErrBindNoPhone
	}
	if _, err := s.store.IncrAndCheck(ctx,
		"bind:otpsend:"+jti, s.cfg.OTPSendMax, s.cfg.TokenTTL); err != nil {
		return err
	}
	if err := s.auth.SendOIDCBindSMS(ctx, zone, phone); err != nil {
		return fmt.Errorf("oidc bind SendSMS: %w", err)
	}
	return nil
}

// VerifySMS 与 VerifyPassword 共用 verify counter。短信验证通过后推进到 verified,
// VerifiedMethod=sms_otp。CandidateUID 在短信路径上**留空** —— 短信不
// 直接确认 uid,confirm 阶段由 claims.phone → user 查询定位(P0 用 phone-only
// 路径时由 service.UIDsByPhone 走);多匹配场景走 P1 Admin。
func (s *BindService) VerifySMS(ctx context.Context, jti, code string) error {
	if !s.methodEnabled(BindMethodSMSOTP) {
		return ErrBindMethodDisabled
	}
	sess, err := s.store.Get(ctx, jti)
	if err != nil {
		return err
	}
	// 状态机守卫:同 VerifyPassword,只允许 issued → verified 的单向迁移。
	if sess.Status != BindStatusIssued {
		return ErrBindStatusConflict
	}
	claims, err := decodeClaimsSnapshot(sess.ClaimsSnapshot)
	if err != nil {
		return err
	}
	zone, phone := extractZone(claims.PhoneNumber), extractPhone(claims.PhoneNumber)
	if !claims.PhoneVerified || phone == "" {
		return ErrBindNoPhone
	}
	if _, err := s.store.IncrAndCheck(ctx,
		"bind:verify:"+jti, s.cfg.VerifyMax, s.cfg.TokenTTL); err != nil {
		return err
	}
	if err := s.auth.VerifyOIDCBindSMS(ctx, zone, phone, code); err != nil {
		// commonapi.SMSService.Verify 把"验证码错误"和"未发送/已过期"都包成
		// errors.New 字符串错误,无法精确判别。统一按"业务拒绝"处理 → 401 / unauthorized。
		return fmt.Errorf("oidc bind VerifySMS: %w (%v)", ErrBindAuthRejected, err)
	}
	// 用 claims phone 在 dmwork user 表找候选 uid:
	//   - 单匹配 → fill CandidateUID,confirm 阶段直接用;
	//   - 多匹配 → ErrBindConflictNeedManual,走 P1 Admin 兜底;
	//   - 0 匹配 → 拒绝(confirm 没有目标可绑)。
	// 与 service.go ResolveOrLink 的 phone autolink 行为对齐,语义可预测。
	uids, lerr := s.locator.UIDsByPhone(zone, phone)
	if lerr != nil {
		return fmt.Errorf("oidc bind VerifySMS: locate phone: %w", lerr)
	}
	switch len(uids) {
	case 0:
		// 老用户没有匹配的 dmwork phone 记录(脏数据/历史未补全)。
		// 业务可预期场景,wrap ErrBindAuthRejected → handler 翻 401 + 通用文案,
		// 不归 internal_error。引导走 FR-7 "联系管理员"兜底。
		return fmt.Errorf("oidc bind VerifySMS: %w (no dmwork user matches claims phone)", ErrBindAuthRejected)
	case 1:
		sess.CandidateUID = uids[0]
	default:
		return ErrBindConflictNeedManual
	}
	sess.VerifiedMethod = BindMethodSMSOTP
	sess.Status = BindStatusVerified
	return s.saveVerified(ctx, sess)
}

// BindConfirmResp Confirm 返回给 handler 的完整快照。
// handler 拿 IssueResp.LoginRespJSON 写 ThirdAuthcode,SD 用来回填原发起设备
// 的 authcode key(FR-6.3 跨设备流转)。
type BindConfirmResp struct {
	IssueResp *IssueSessionResp
	SD        *StateData
	UID       string
	Issuer    string
}

// Confirm 自助绑定终态写入。串行步骤(与下方代码一一对应):
//
//  1. Get session(不消费)
//  2. ConfirmMax counter +1(SR-2.1)—— 故意放在 status 检查之前
//     (asymmetry note,与 VerifyPassword/VerifySMS 顺序相反)
//  3. 校验 Status == verified;否则拒绝
//  4. 解出 claims/sd snapshot
//  5. identity.Insert((uid, issuer, sub)) —— DB uk_issuer_subject + uk_uid_issuer
//     兜底 SR-5;duplicate-key → ErrBindAlreadyBound
//  6. users.IssueSession 签发 dmwork 会话
//  7. Consume session(SR-1 单次消费;Insert 已成功 + IssueSession 失败时**不**
//     Consume,留给客户端重试,二次 Insert 撞唯一约束 → 409 引导走 OIDC 登录)
//
// 并发防护(AC-6):多个并发 confirm 同 jti,只有一个能拿到 status=verified
// 并写入 identity;其他要么撞 status conflict,要么撞 DB unique constraint。
// 都会返回明确错误,不会重复写。
//
// 计数器顺序的 asymmetry 说明:VerifyPassword/VerifySMS 是 status→counter
// (合法用户的 status 已经被推进就不该被攻击者再消耗 verify 配额);Confirm
// 是 counter→status(任何形式的"反复 confirm"都该烧 budget,包括 status 不对
// 的探测,否则攻击者可以零成本探测状态机位置)。
func (s *BindService) Confirm(ctx context.Context, jti string) (*BindConfirmResp, error) {
	if s.identity == nil || s.users == nil {
		return nil, errors.New("oidc bind Confirm: not configured (identity/users nil)")
	}
	sess, err := s.store.Get(ctx, jti)
	if err != nil {
		return nil, err
	}
	// counter→status (见上方 asymmetry 说明)
	if _, err := s.store.IncrAndCheck(ctx,
		"bind:confirm:"+jti, s.cfg.ConfirmMax, s.cfg.TokenTTL); err != nil {
		return nil, err
	}
	if sess.Status != BindStatusVerified {
		return nil, ErrBindStatusConflict
	}
	if sess.CandidateUID == "" {
		// VerifySMS 单匹配会 fill, VerifyPassword 也 fill;走到这里说明
		// 状态机被异常推进了,拒绝写脏数据。
		return nil, errors.New("oidc bind Confirm: candidate_uid empty")
	}
	claims, err := decodeClaimsSnapshot(sess.ClaimsSnapshot)
	if err != nil {
		return nil, err
	}
	sd, err := decodeSDSnapshot(sess.SDSnapshot)
	if err != nil {
		return nil, err
	}
	// TOCTOU 复核:locator + VerifyPasswordByUID 都只在
	// verify 阶段过滤 is_destroy/status,verify→confirm 之间有 5min 用户交互
	// 窗口,运维 disable / 用户自助 destroy 都能让 CandidateUID 变成不可绑定状态。
	// 此处再查一次,把残留 user_oidc_identity 脏数据导致用户后续 OIDC 登录
	// dead-loop 的窗口从"用户交互级"压到"DB 单次 round-trip 级"。
	// DB 层 uk_uid_issuer + 登录路径的 status 检查仍然是终态兜底。
	bindable, berr := s.auth.IsBindable(ctx, sess.CandidateUID)
	if berr != nil {
		return nil, fmt.Errorf("oidc bind Confirm: check uid bindable: %w", berr)
	}
	if !bindable {
		// 与 verify 阶段拒绝停用账号同语义,wrap ErrBindAuthRejected 让 handler
		// 翻 401 + 通用文案(反账号枚举 SR-6,不暴露"账号被运维停用"vs"不存在")。
		return nil, fmt.Errorf("oidc bind Confirm: %w (candidate uid not bindable)", ErrBindAuthRejected)
	}
	// 写 identity binding —— uk_uid_issuer / uk_issuer_subject 兜底竞态。
	if err := s.identity.Insert(&IdentityModel{
		UID:           sess.CandidateUID,
		Issuer:        claims.Issuer,
		Subject:       claims.Subject,
		Email:         claims.Email,
		EmailVerified: boolToInt(claims.EmailVerified),
		Phone:         claims.PhoneNumber,
		PhoneVerified: boolToInt(claims.PhoneVerified),
		LinkedAt:      time.Now(),
	}); err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrBindAlreadyBound
		}
		return nil, fmt.Errorf("oidc bind Confirm: insert identity: %w", err)
	}
	// 签发 dmwork 会话。沿用 callback 的 IssueSessionReq 形状,从 SD snapshot
	// 取设备 flag / IP,从 claims 取 name/email/phone/zone。
	zone, phone := extractZone(claims.PhoneNumber), extractPhone(claims.PhoneNumber)
	issueReq := IssueSessionReq{
		UID:        sess.CandidateUID,
		CreateUser: false, // 绑定路径都是老用户,绝不在 confirm 阶段建用户
		Name:       claims.Name,
		Email:      claims.Email,
		Phone:      phone,
		Zone:       zone,
		DeviceFlag: sd.DeviceFlag,
		PublicIP:   sd.IP,
	}
	resp, err := s.users.IssueSession(ctx, issueReq)
	if err != nil {
		// identity 已写但 session 签发失败:不消费 token,让客户端可以重试
		// 拿同一 token 再 confirm 一次。第二次 identity.Insert 会撞唯一约束
		// → ErrBindAlreadyBound,handler 翻成 409 提示"已绑定,直接登录"。
		// 比"identity 写了但用户不知道"的丢失更可控。
		return nil, fmt.Errorf("oidc bind Confirm: issue session: %w", err)
	}
	// 单次消费:成功才删 session,避免回放。
	if _, cerr := s.store.Consume(ctx, jti); cerr != nil {
		// Consume 失败不致命:session TTL 自己会到期。注意:这里**绝不能**
		// 回滚 identity / session —— 用户已经登录成功了。
		// log 让运维察觉 Redis 抖动(否则 5min 后 TTL 静默清理,问题消失无痕)。
		log.Warn("OIDC bind Confirm: consume session failed (non-fatal, will TTL out)",
			zap.String("jti_hash", subHash(jti)), zap.Error(cerr))
	}
	return &BindConfirmResp{
		IssueResp: resp,
		SD:        sd,
		UID:       sess.CandidateUID,
		Issuer:    claims.Issuer,
	}, nil
}

// BindCreateResp Create 返给 handler 的完整快照,与 BindConfirmResp 对齐。
type BindCreateResp struct {
	IssueResp *IssueSessionResp
	SD        *StateData
	UID       string
	Issuer    string
}

// Create 在 status=issued 时用 bind_token 里的 claims 建号 + 写 identity + 签发会话。
//
// 顺序很关键(防 ghost user):
//  1. Get session
//  2. IncrAndCheck("bind:create:"+jti, bindCreateMax) — counter→status 与 Confirm 对齐,
//     任何对 create 的探测都消耗配额
//  3. decode claims + 校验(纯只读,无副作用,失败保持 token 在 issued 让用户重试)
//  4. CASSave issued→creating — 唯一的"锁":只有拿到锁的 goroutine 才能进入
//     产生副作用的 #5/#6/#7。并发 verify/create 都会撞 CAS 失败 → ErrBindStatusConflict,
//     不会出现"副作用已发生但终态锁不上"的 ghost-user 窗口。
//  5. IssueSession({CreateUser:true, ...}) — 副作用 #1:dmwork user 入库
//  6. identity.Insert((uid, issuer, sub)) — 副作用 #2:user_oidc_identity 入库
//  7. Consume(token) — 终态:token 整条删除,后续 Get 返 ErrBindNotFound
//
// 中途失败语义:
//   - #5/#6 失败:token 卡在 creating,5min TTL 后自然消失。**不会**被同一 token
//     的二次 create 重复触发(CAS issued→creating 第二次必败)—— 避免重试产生
//     第二个 ghost user。用户体感:需要重走 OIDC 登录拿新 bind_token。
//   - #6 撞 uk_issuer_subject(并发不同 token 同 issuer+sub):返 ErrBindAlreadyBound,
//     handler 引导用户重发起 OIDC 登录拾取赢家会话(与 plan §4 一致)。
//   - #7 失败:用户已建,会话已发,token 5min TTL 自清,只 log warn。
func (s *BindService) Create(ctx context.Context, jti string) (*BindCreateResp, error) {
	if s.identity == nil || s.users == nil {
		return nil, errors.New("oidc bind Create: not configured (identity/users nil)")
	}
	sess, err := s.store.Get(ctx, jti)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.IncrAndCheck(ctx,
		"bind:create:"+jti, bindCreateMax, s.cfg.TokenTTL); err != nil {
		return nil, err
	}
	// manual_conflict 来源的 token 不允许走自助建号:用户在 dmwork 已经有(多条)
	// 账号,/bind/create 会再造账号加剧脏数据 —— 走 P1 Admin 人工合并兜底。
	// 空字符串视同 unknown_user(灰度兼容旧 token),与 IssueReason godoc 一致。
	if sess.IssueReason == BindReasonManualConflict {
		return nil, ErrBindCreateConflictNeedManual
	}
	claims, err := decodeClaimsSnapshot(sess.ClaimsSnapshot)
	if err != nil {
		return nil, err
	}
	if err := s.checkClaimsForCreate(claims); err != nil {
		return nil, err
	}
	// D. IssuerAllowlist 防御性复核:ShouldHandle 在 callback 阶段已挡过一遍,
	// 这里在 Create 入口再校验一次,防止运维在 token 5min TTL 内把某 issuer
	// 从 allowlist 移走、但 token 已经签发的窗口里继续被滥用。
	// 仅在 Create 路径加(与 reviewer 描述一致):verify/confirm 路径的 token
	// 是用户主动二次验证,IdP 信任由原 callback 时已校验,风险低。
	if !s.issuerAllowedForCreate(claims.Issuer) {
		return nil, fmt.Errorf("oidc bind Create: %w (issuer no longer in allowlist)", ErrBindAuthRejected)
	}
	sd, err := decodeSDSnapshot(sess.SDSnapshot)
	if err != nil {
		return nil, err
	}
	// CAS issued→creating:把后续所有副作用包在锁里。
	// 失败原因:(a) 别的 goroutine 已推进了 status(verify / 另一个 create),
	// (b) token TTL 已到。两种都返 ErrBindStatusConflict / ErrBindNotFound,无副作用。
	if err := s.casLockForCreate(ctx, sess); err != nil {
		return nil, err
	}
	zone, phone := extractZone(claims.PhoneNumber), extractPhone(claims.PhoneNumber)
	issueReq := IssueSessionReq{
		CreateUser: true,
		Name:       claims.Name,
		Email:      claims.Email,
		Phone:      phone,
		Zone:       zone,
		DeviceFlag: sd.DeviceFlag,
		PublicIP:   sd.IP,
		// 进入本方法前已经过 ShouldHandle 的 IssuerAllowlist 校验(callback 阶段),
		// 加上 bind_token 自身的 5min 单次性 + 上面的 checkClaimsForCreate / issuerAllowed
		// 兜底,运维要授权的就是这条"OIDC 自助建号"路径 —— 告诉 user 模块绕过
		// register.off 全局开关,与 callback `res.IsNew` 分支语义对称。
		TrustedSSOCreate: true,
	}
	resp, err := s.users.IssueSession(ctx, issueReq)
	if err != nil {
		return nil, fmt.Errorf("oidc bind Create: issue session: %w", err)
	}
	uid := resp.UID
	if err := s.identity.Insert(&IdentityModel{
		UID:           uid,
		Issuer:        claims.Issuer,
		Subject:       claims.Subject,
		Email:         claims.Email,
		EmailVerified: boolToInt(claims.EmailVerified),
		Phone:         claims.PhoneNumber,
		PhoneVerified: boolToInt(claims.PhoneVerified),
		LinkedAt:      time.Now(),
	}); err != nil {
		if isDuplicateKeyError(err) {
			return nil, ErrBindAlreadyBound
		}
		return nil, fmt.Errorf("oidc bind Create: insert identity: %w", err)
	}
	if _, cerr := s.store.Consume(ctx, jti); cerr != nil {
		log.Warn("OIDC bind Create: consume session failed (non-fatal, will TTL out)",
			zap.String("jti_hash", subHash(jti)), zap.Error(cerr))
	}
	return &BindCreateResp{
		IssueResp: resp,
		SD:        sd,
		UID:       uid,
		Issuer:    claims.Issuer,
	}, nil
}

// checkClaimsForCreate 校验 claims 至少有一条强标识(email 或 phone)。
//
// phone 可用性必须用 extractPhone 判定,不能只看 PhoneNumber != "":
// dmwork 当前仅支持 +86 号段(service.go:extractPhone),非 +86 verified phone
// 会被 IssueSession 默默丢成空 Phone/Zone,落库后用户没有可用手机号锚点。
// 用 extractPhone 做能用性检查,把"格式不支持"提前到 422 而非建出残缺账号。
//
// email_verified / phone_verified 必须为 true,否则后续客服 / 找回流程没有可信锚点
// (与 autolink 准入条件一致)。
func (s *BindService) checkClaimsForCreate(claims *IDTokenClaims) error {
	hasEmail := claims.Email != "" && claims.EmailVerified
	hasPhone := claims.PhoneVerified && extractPhone(claims.PhoneNumber) != ""
	if !hasEmail && !hasPhone {
		return ErrBindCreateClaimsIncomplete
	}
	return nil
}

// decodeSDSnapshot 与 decodeClaimsSnapshot 对称。
func decodeSDSnapshot(b []byte) (*StateData, error) {
	var sd StateData
	if err := json.Unmarshal(b, &sd); err != nil {
		return nil, fmt.Errorf("oidc bind: decode sd snapshot: %w", err)
	}
	return &sd, nil
}

// saveVerified 把已经在 sess 上设好的 (Status=verified, CandidateUID,
// VerifiedMethod) 通过 CASSave 原子写入 —— 要求 Redis 当前 status 仍是 issued。
//
// 必须 CAS 不能 plain Save:两个并发 verify 即便都通过了 Get → status
// 检查(都基于 issued 快照),只有一个能把 status 推进到 verified;
// 另一个 CASSave 看到 status 已经是 verified,返 ErrBindStatusConflict。
// 防止"victim 的 verified token + attacker 用自己账号再 verify 一次 →
// CandidateUID 被覆盖 → confirm 绑到 attacker"的身份接管路径。
//
// TTL 使用 absolute expiry:传 remainingTTL 而非
// 完整 cfg.TokenTTL,否则在 token 快过期时 verify 会把 TTL 续到 issue 后的
// "TokenTTL × 2",与文档承诺的 5min 不符,也让 URL 泄漏窗口翻倍。
// remaining<=0 时返 ErrBindNotFound 让调用方按"已过期"处理。
func (s *BindService) saveVerified(ctx context.Context, sess *BindSession) error {
	remaining := s.remainingTTL(sess)
	if remaining <= 0 {
		return ErrBindNotFound
	}
	if err := s.store.CASSave(ctx, sess, BindStatusIssued, remaining); err != nil {
		return fmt.Errorf("oidc bind: cas save verified session: %w", err)
	}
	return nil
}

// saveCreated 把 sess.Status 从 issued CAS 到 created,使用绝对剩余 TTL。
// 对称 saveVerified:CAS expected=issued,防并发 create 竞态。
// casLockForCreate 把 issued 锁到 creating —— Create 路径下唯一的状态写入。
//
// 不复用 saveVerified 的"写整条 session"是因为 Create 没有 verify/sms 那种业务字段
// 要落地(CandidateUID/VerifiedMethod),只需推进 status。CAS 用 BindStatusIssued
// 作为期望值,保证并发 verify(已把 status 推到 verified)或并发 create(已推到
// creating)都拒绝在此处,不进入下游副作用阶段。
//
// TTL 用 remainingTTL 而非完整 cfg.TokenTTL:防止 Create 把 token 续命到 issue 后
// 的 "TokenTTL × 2",与文档承诺的 5min 不符。remaining<=0 时返 ErrBindNotFound 让
// 上层按"已过期"处理。
func (s *BindService) casLockForCreate(ctx context.Context, sess *BindSession) error {
	remaining := s.remainingTTL(sess)
	if remaining <= 0 {
		return ErrBindNotFound
	}
	sess.Status = BindStatusCreating
	if err := s.store.CASSave(ctx, sess, BindStatusIssued, remaining); err != nil {
		return fmt.Errorf("oidc bind: cas lock creating: %w", err)
	}
	return nil
}

// remainingTTL 计算 sess 距离绝对过期(Issue 时刻 + TokenTTL)还剩多久。
// 时间基准与 Issue 一致(nowUnix=time.Now().Unix(),time.Until 用 time.Now()),
// 无 deterministic-clock 注入需求时可直接拿来用。
func (s *BindService) remainingTTL(sess *BindSession) time.Duration {
	deadline := time.Unix(sess.CreatedAt, 0).Add(s.cfg.TokenTTL)
	return time.Until(deadline)
}

// issuerAllowedForCreate D. defense-in-depth 检查:Create 入口在
// checkClaimsForCreate 后做最后一道 issuer allowlist 兜底。空 allowlist =
// deny-all(灰度安全默认值),与 ShouldHandle 同语义。
func (s *BindService) issuerAllowedForCreate(issuer string) bool {
	for _, allowed := range s.cfg.IssuerAllowlist {
		if allowed == issuer {
			return true
		}
	}
	return false
}

// ShouldHandle 给 callback 失败分支用的接管判定。任何一条不满足就走旧路径,
// 行为完全保留(NFR-6 可回滚)。
func (s *BindService) ShouldHandle(err error, claims *IDTokenClaims) bool {
	if s == nil || !s.cfg.Enabled {
		return false
	}
	if claims == nil {
		return false
	}
	if !errors.Is(err, ErrUnknownUser) && !errors.Is(err, ErrConflictNeedManual) {
		return false
	}
	// 空 allowlist = deny-all(灰度安全默认值)
	for _, allowed := range s.cfg.IssuerAllowlist {
		if allowed == claims.Issuer {
			return true
		}
	}
	return false
}

// ---- helpers ----

// newBindJTI 32 字节随机 base64 url-safe。与 stateStore 同款熵源 + 编码,
// 便于在 URL query / HTTP header 中安全携带。
func newBindJTI() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func decodeClaimsSnapshot(b []byte) (*IDTokenClaims, error) {
	var c IDTokenClaims
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("oidc bind: decode claims snapshot: %w", err)
	}
	return &c, nil
}

// maskEmailForBind alice@example.com → a***@example.com (FR-2.1)。
// 与 maskEmail 略不同:那个用于审计日志压缩;这个面向终端用户。
func maskEmailForBind(email string) string {
	at := strings.Index(email, "@")
	if at <= 0 {
		return ""
	}
	if at == 1 {
		// 单字符 local 部分,直接 ***@domain
		return "***" + email[at:]
	}
	return email[:1] + "***" + email[at:]
}

// maskPhoneForBind 保留后 4 位,其余替 *。例:
//
//	"+8613912345678" → "****5678"
//	"13912345678"    → "****5678"
//	""               → ""        // 空字符串透传,让 BindInfoResp.MaskedPhone 的
//	                                json:"omitempty" 生效;否则返 *** 会让前端
//	                                把"无手机号"误显示成"有手机号已脱敏"
//
// 不区分 +86 与裸号;调用方都来自 claims.PhoneNumber 字段,IdP 写法不固定。
// 0<长度<4 时仍返 *** 兜底,不暴露原值(异常短手机号字段)。
func maskPhoneForBind(phone string) string {
	if phone == "" {
		return ""
	}
	if len(phone) < 4 {
		return "***"
	}
	return "****" + phone[len(phone)-4:]
}

// userBindReasonRateLimited 镜像 user.BindReasonRateLimited 字符串字面值。
//
// 不直接 import user 包是因为:user.IService 通过 BindAuthenticator 接口反向
// 依赖 oidc 包不会触发,但 string 常量是契约层面的"魔法字符串"。这里复制
// 一份并在测试里固定串到一起(见 bind_service_test.go 同名常量断言),保证
// user 侧改动会被本包测试发现而不是 silently degrade 成 401/unauthorized。
const userBindReasonRateLimited = "rate_limited"

// uidFailWindow SR-2.2 uid 维度防爆破窗口。固定 24h 与 TokenTTL 解耦 ——
// 后者按 token 生命周期(5min),前者按"每日 ≤ 10 次失败"语义。变量(非 const)
// 以便单测可临时缩小窗口验证滚动行为(当前未用,保留扩展位)。
var uidFailWindow = 24 * time.Hour

// bindUIDFailKey 拼装 SR-2.2 真实 uid 维度计数器的 Redis key。集中拼装避免漂移。
func bindUIDFailKey(uid string) string { return "bind:uidfail:" + uid }

// bindEnumFailKey 反枚举(SR-6)维度计数器的 Redis key。**独立 keyspace**
// (`bind:enumfail:` 而非 `bind:uidfail:unknown:`),避免万一某个 IdP 发出
// 字面以 `unknown:` 开头的 uid 时与真实账号计数串扰 —— 真用户被攻击者用
// 不存在的 identifier 反复探测而锁死。两个 keyspace 物理隔离更稳。
func bindEnumFailKey(identifierHash string) string { return "bind:enumfail:" + identifierHash }

// methodEnabled 检查 cfg.Methods 是否启用了 m。配置必须是真实策略:
// 运维通过 OCTO_OIDC_BIND_METHODS 关一种方法后,客户端硬调端点也不能绕过 ——
// 否则配置就退化成 UI 提示,无安全意义。
func (s *BindService) methodEnabled(m BindMethod) bool {
	for _, x := range s.cfg.Methods {
		if x == m {
			return true
		}
	}
	return false
}

// nowUnix 抽 var 以便未来 BindService 注入 clock 做 deterministic test。
// 当前 P0 用 time.Now,deterministic 需求落到后续 PR 再说。
var nowUnix = func() int64 {
	return time.Now().Unix()
}
