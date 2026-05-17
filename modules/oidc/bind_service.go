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

// BindLocator 把用户输入(目前只支持 username)解析到 dmwork uid。
//
// 仅在密码路径使用 —— 短信路径 zone/phone 直接从 claims snapshot 取(FR-3.3)。
// 多匹配场景按需扩展(P0 假定 username 全局唯一,有冲突单独走 P1 Admin 兜底)。
type BindLocator interface {
	UIDByUsername(username string) (string, error)
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
type BindService struct {
	cfg     BindConfig
	store   BindStore
	auth    BindAuthenticator
	locator BindLocator
}

func newBindService(cfg BindConfig, store BindStore, auth BindAuthenticator, locator BindLocator) *BindService {
	return &BindService{cfg: cfg, store: store, auth: auth, locator: locator}
}

// Issue 在 callback ResolveOrLink 失败分支调用:签发 bind_token,持久化
// claims + state_data 快照,返回 jti 供 handler 拼前端跳转 URL。
//
// 不在此处写 audit —— handler 在拿到 jti 后统一写 EventBindIssued
// (handler 持 HTTP 上下文,IP/UA/trace_id 都齐)。
func (s *BindService) Issue(_ context.Context, claims *IDTokenClaims, sd *StateData) (string, error) {
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
	if err := s.store.Save(context.Background(), sess, s.cfg.TokenTTL); err != nil {
		return "", fmt.Errorf("oidc bind Issue: save: %w", err)
	}
	return jti, nil
}

// Info 返回脱敏 claims + 可用方法。可用方法 = 配置 Methods ∩ 当前 claims 支持
// 的手段(claims 无 verified phone → 屏蔽 sms_otp,FR-3.3)。
func (s *BindService) Info(_ context.Context, jti string) (*BindInfoResp, error) {
	sess, err := s.store.Get(context.Background(), jti)
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
		return errors.New("oidc bind VerifyPassword: account or password invalid")
	}
	matched, reason, aerr := s.auth.VerifyPasswordByUID(ctx, uid, password)
	if aerr != nil {
		return fmt.Errorf("oidc bind VerifyPassword: auth: %w", aerr)
	}
	if !matched {
		return fmt.Errorf("oidc bind VerifyPassword: rejected: %s", reason)
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
		return errors.New("oidc bind SendSMS: claims has no verified phone")
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
		return errors.New("oidc bind VerifySMS: claims has no verified phone")
	}
	if _, err := s.store.IncrAndCheck(ctx,
		"bind:verify:"+jti, s.cfg.VerifyMax, s.cfg.TokenTTL); err != nil {
		return err
	}
	if err := s.auth.VerifyOIDCBindSMS(ctx, zone, phone, code); err != nil {
		return fmt.Errorf("oidc bind VerifySMS: auth: %w", err)
	}
	sess.VerifiedMethod = BindMethodSMSOTP
	sess.Status = BindStatusVerified
	return s.saveVerified(ctx, sess)
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
