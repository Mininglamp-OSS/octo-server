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
}

// BindService 自助绑定状态机的业务逻辑层。
//
// 不持有 HTTP 上下文 / *wkhttp.Context;handler 层负责 HTTP 解析、CallbackGuard
// IP 限流、审计写入(events 在 model.go 已就位)。
//
// identity / users 字段在 PR3 阶段未注入(nil)—— 仅 Confirm 路径用到,PR4
// callback 接管时由 Init() 传入。其他 5 个方法(Issue/Info/Verify*/SendSMS)
// 不依赖这两个字段,PR3 单测可以不注入。
type BindService struct {
	cfg      BindConfig
	store    BindStore
	auth     BindAuthenticator
	locator  BindLocator
	identity identityStore // PR4: confirm 路径写 user_oidc_identity
	users    userLookup    // PR4: confirm 路径调 IssueSession 签发 dmwork 会话
}

func newBindService(cfg BindConfig, store BindStore, auth BindAuthenticator, locator BindLocator) *BindService {
	return &BindService{cfg: cfg, store: store, auth: auth, locator: locator}
}

// Issue 在 callback ResolveOrLink 失败分支调用:签发 bind_token,持久化
// claims + state_data 快照,返回 jti 供 handler 拼前端跳转 URL。
//
// 不在此处写 audit —— handler 在拿到 jti 后统一写 EventBindIssued
// (handler 持 HTTP 上下文,IP/UA/trace_id 都齐)。
func (s *BindService) Issue(ctx context.Context, claims *IDTokenClaims, sd *StateData) (string, error) {
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
	}
	if err := s.store.Save(ctx, sess, s.cfg.TokenTTL); err != nil {
		return "", fmt.Errorf("oidc bind Issue: save: %w", err)
	}
	return jti, nil
}

// Info 返回脱敏 claims + 可用方法。可用方法 = 配置 Methods ∩ 当前 claims 支持
// 的手段(claims 无 verified phone → 屏蔽 sms_otp,FR-3.3)。
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
	sess, err := s.store.Get(ctx, jti)
	if err != nil {
		return err
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
		return fmt.Errorf("oidc bind VerifyPassword: %w (unknown identifier)", ErrBindAuthRejected)
	}
	matched, reason, aerr := s.auth.VerifyPasswordByUID(ctx, uid, password)
	if aerr != nil {
		// 内部错误(DB/网络):wrap aerr,handler 看不到 ErrBindAuthRejected,
		// metric 落 internal_error / HTTP 500。
		return fmt.Errorf("oidc bind VerifyPassword: auth: %w", aerr)
	}
	if !matched {
		// 业务拒绝:wrap ErrBindAuthRejected,handler 翻 401。
		// reason 只用于 zap.Error / service 层日志,不直接给客户端 / 审计 reason 列。
		return fmt.Errorf("oidc bind VerifyPassword: %w (%s)", ErrBindAuthRejected, reason)
	}
	// 用 *Service.store 直接改 session:CAS issued→verified,顺便回写
	// CandidateUID + VerifiedMethod。memory/redis 两个 store 的 UpdateStatus
	// 只改 status 字段,VerifiedMethod / CandidateUID 需要 Save 整段。
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

// Confirm 自助绑定终态写入。串行步骤:
//
//  1. Get session(不消费 —— 先校验状态,再原子 Consume,避免 cas-then-consume
//     竞态被攻击者反复触发审计/限流计数)
//  2. ConfirmMax counter +1(SR-2.1)
//  3. 校验 Status == verified;否则拒绝
//  4. identity.Insert((uid, issuer, sub)) —— DB uk_issuer_subject + uk_uid_issuer
//     兜底 SR-5;duplicate-key → ErrBindAlreadyBound
//  5. users.IssueSession 签发 dmwork 会话
//  6. Consume session(SR-1 单次消费,即便 5/6 失败也由 caller 决定是否清场)
//
// 并发防护(AC-6):多个并发 confirm 同 jti,只有一个能拿到 status=verified
// 并写入 identity;其他要么撞 status conflict,要么撞 DB unique constraint。
// 都会返回明确错误,不会重复写。
func (s *BindService) Confirm(ctx context.Context, jti string) (*BindConfirmResp, error) {
	if s.identity == nil || s.users == nil {
		return nil, errors.New("oidc bind Confirm: not configured (identity/users nil)")
	}
	sess, err := s.store.Get(ctx, jti)
	if err != nil {
		return nil, err
	}
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

// decodeSDSnapshot 与 decodeClaimsSnapshot 对称。
func decodeSDSnapshot(b []byte) (*StateData, error) {
	var sd StateData
	if err := json.Unmarshal(b, &sd); err != nil {
		return nil, fmt.Errorf("oidc bind: decode sd snapshot: %w", err)
	}
	return &sd, nil
}

// saveVerified 用整段 Save 更新 sess(memory/redis 都覆盖整 key,等价于
// CAS 的"读改写";状态机迁移已经通过 Get → Save 的窗口保证逻辑顺序)。
//
// 真正的 verified→confirmed CAS 在 Confirm 路径用 UpdateStatus 严格做。
// 这里不用 UpdateStatus 是因为还要回写 VerifiedMethod / CandidateUID 等
// 字段,UpdateStatus 接口只改 status,无法承载多字段。
func (s *BindService) saveVerified(ctx context.Context, sess *BindSession) error {
	if err := s.store.Save(ctx, sess, s.cfg.TokenTTL); err != nil {
		return fmt.Errorf("oidc bind: save verified session: %w", err)
	}
	return nil
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
//
// 不区分 +86 与裸号;调用方都来自 claims.PhoneNumber 字段,IdP 写法不固定。
// 长度 < 4 时直接返 *** 兜底,不暴露原值。
func maskPhoneForBind(phone string) string {
	if len(phone) < 4 {
		return "***"
	}
	return "****" + phone[len(phone)-4:]
}

// nowUnix 抽 var 以便未来 BindService 注入 clock 做 deterministic test。
// 当前 P0 用 time.Now,deterministic 需求落到后续 PR 再说。
var nowUnix = func() int64 {
	return time.Now().Unix()
}
