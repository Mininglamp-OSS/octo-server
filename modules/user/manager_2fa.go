package user

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	"github.com/Mininglamp-OSS/octo-server/modules/base/common/emailtmpl"
	common "github.com/Mininglamp-OSS/octo-server/modules/common"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

// Manager-console second factor: an emailed one-time code, required after the
// password check when login.manager_2fa_on is on.
//
// # Why this does not reuse EmailService.SendVerifyCode / Verify
//
// That shared implementation keys its cooldown, failure counter and lockout by
// EMAIL ADDRESS ALONE — `email_rate_limit:{email}`, `email_verify_fail:{email}`,
// `email_verify_lock:{email}` — with no code-type in the key. Those same keys
// are written by the public, unauthenticated endpoints /v1/user/email/sendcode,
// /v1/user/emaillogin and /v1/user/email/forgetpwd.
//
// Reusing them here would hand anyone who can guess an administrator's address
// (admin@$company is not a secret) two cheap remote locks on the console:
//
//   - one request per minute to /v1/user/email/sendcode keeps the shared
//     cooldown key alive, so the administrator's code can never be sent;
//   - three bad codes on /v1/user/emaillogin trips the shared 10-minute lock,
//     so a correctly delivered code can never be redeemed.
//
// A second factor that an anonymous attacker can jam is worse than no second
// factor: it converts a hardening feature into an availability hole on the
// emergency entry point that /v1/manager/login deliberately is. So the whole
// bucket set below is keyed by UID, which is only knowable after the password
// check has already passed. The public email flows and this one cannot reach
// each other's keys in either direction.
//
// These hand-written Redis keys are the per-resource-cooldown exception in
// .octospec/rules/rate-limit.md, not a bypass of it: they express a per-account
// verification-code lifecycle (one live code, a send pace, a failure budget)
// that the IP and UID buckets cannot represent. HTTP request-frequency limiting
// for these endpoints is still the shared StrictIPRateLimitMiddleware, mounted
// in Manager.Route.
const (
	manager2FACodeKeyPrefix     = "mgr2fa:code:"
	manager2FACooldownKeyPrefix = "mgr2fa:cooldown:"
	manager2FAFailKeyPrefix     = "mgr2fa:fail:"
	manager2FALockKeyPrefix     = "mgr2fa:lock:"
	// The pending handshake is keyed by its own opaque token rather than by uid:
	// the client holds the token between the two requests, and it must not be
	// guessable from anything the client already knows.
	manager2FAPendingKeyPrefix = "manager_login_2fa:"
)

const (
	manager2FACodeLength = 6
	// Code lifetime. Five minutes is long enough to absorb SMTP queueing and
	// anti-spam scanning, short enough that a code sitting in a mailbox is not a
	// standing key. Kept in sync with the TTL rendered into the email body.
	manager2FACodeTTL = 5 * time.Minute
	// Pending-handshake lifetime, deliberately ONE MINUTE LONGER than the code.
	// If the two were equal, a code submitted in its final seconds could fail
	// because the handshake expired first — reported through the same
	// anti-enumeration error as a wrong code, which is unexplainable to the
	// operator and to whoever reads the ticket.
	manager2FAPendingTTL = 6 * time.Minute
	// Per-uid resend cooldown, mirroring the shared email service's 1 minute.
	manager2FAResendCooldown = time.Minute
	// Wrong-code budget per uid, then a lockout. Separate from the per-handshake
	// attempt cap: this one survives restarting the sign-in, so an attacker
	// cannot reset it by asking for a fresh handshake.
	manager2FAMaxCodeFailures = 3
	manager2FACodeLockTTL     = 10 * time.Minute
	// Attempt / resend budget for a single handshake.
	manager2FAMaxAttempts = 5
	manager2FAMaxResends  = 3
	// Bound the SMTP round-trip. The transport's own defaults are 15s dial +
	// 60s IO and the shared call sites pass context.Background(), which would
	// let a sick mail server hold a sign-in request open for over a minute.
	manager2FASendTimeout = 8 * time.Second
)

// errManager2FAInvalid is the single sentinel every second-step failure maps to:
// unknown / expired / forged token, wrong code, attempt cap, lockout. Callers
// translate it to exactly one error code so the wire cannot distinguish "valid
// handshake, wrong code" from "no such handshake".
var (
	errManager2FAInvalid          = errors.New("manager 2fa: invalid or expired verification")
	errManager2FAResendCooldown   = errors.New("manager 2fa: resend cooldown active")
	errManager2FAResendsExhausted = errors.New("manager 2fa: resend budget exhausted")
)

// manager2FAPending is the server-side half of the handshake. It is stored under
// an opaque token and never returned to the client.
//
// ExpiresAt is carried inside the record, not inferred from the Redis TTL,
// because every rewrite (attempt counter, resend counter) would otherwise reset
// that TTL and silently extend the window. Rewrites compute the remaining time
// from this field, so the deadline set at issue time is the real one.
type manager2FAPending struct {
	UID       string `json:"uid"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	IP        string `json:"ip"`
	Language  string `json:"language"`
	Attempts  int    `json:"attempts"`
	Resends   int    `json:"resends"`
	ExpiresAt int64  `json:"expires_at"`
}

func (p *manager2FAPending) remaining() time.Duration {
	return time.Until(time.Unix(p.ExpiresAt, 0))
}

// manager2FA owns the second-factor handshake: code lifecycle, pending store and
// delivery. It deliberately holds no DB handle — the caller has already resolved
// and re-validated the account before anything here runs.
type manager2FA struct {
	ctx      *config.Context
	emailSvc commonapi.IEmailService
	log.Log
}

func newManager2FA(ctx *config.Context) *manager2FA {
	return &manager2FA{
		ctx:      ctx,
		emailSvc: commonapi.NewEmailService(ctx, common.EnsureSystemSettings(ctx)),
		Log:      log.NewTLog("managerTwoFactor"),
	}
}

// start issues a code, delivers it, and returns the opaque handshake token.
//
// Order matters: the code is persisted BEFORE the email goes out (a delivered
// code that cannot be redeemed is worse than an undelivered one), and the
// pending record is written only after delivery succeeds, so a failed send
// leaves nothing behind for the client to retry against.
func (s *manager2FA) start(ctx context.Context, account *managerLoginModel, clientIP, lang string) (token string, err error) {
	// A second sign-in while the send cooldown is still active does NOT send
	// another code — but it must still hand back a usable handshake. The
	// handshake token lives only in the browser, so a closed tab or a reload
	// would otherwise strand the operator: the code sitting in their mailbox has
	// nothing left to redeem it against, and they would be locked out for the
	// remainder of the cooldown despite holding a valid code.
	//
	// Re-issuing the handshake resets the per-handshake attempt counter, which is
	// why that counter is not the real brute-force bound: the per-uid failure
	// lock is, and it deliberately survives starting over.
	if err := s.checkCooldown(account.UID); err != nil {
		if !errors.Is(err, errManager2FAResendCooldown) {
			return "", err
		}
		live, liveErr := s.ctx.GetRedisConn().GetString(manager2FACodeKeyPrefix + account.UID)
		if liveErr != nil {
			return "", fmt.Errorf("manager 2fa: read live code: %w", liveErr)
		}
		if live == "" {
			// Cooling down with no code left to reuse: nothing better to offer
			// than asking the operator to wait.
			return "", err
		}
		return s.issuePending(account, clientIP, lang)
	}
	code, err := commonapi.GenerateVerifyCode(manager2FACodeLength)
	if err != nil {
		return "", fmt.Errorf("manager 2fa: generate code: %w", err)
	}
	redis := s.ctx.GetRedisConn()
	if err := redis.SetAndExpire(manager2FACodeKeyPrefix+account.UID, code, manager2FACodeTTL); err != nil {
		return "", fmt.Errorf("manager 2fa: persist code: %w", err)
	}
	if err := s.deliver(ctx, account, code, clientIP, lang); err != nil {
		// Do not leave a redeemable code behind for a message nobody received.
		_ = redis.Del(manager2FACodeKeyPrefix + account.UID)
		return "", err
	}
	if err := redis.SetAndExpire(manager2FACooldownKeyPrefix+account.UID, "1", manager2FAResendCooldown); err != nil {
		return "", fmt.Errorf("manager 2fa: persist cooldown: %w", err)
	}
	return s.issuePending(account, clientIP, lang)
}

// issuePending mints a fresh handshake for an already-delivered code.
func (s *manager2FA) issuePending(account *managerLoginModel, clientIP, lang string) (string, error) {
	pending := &manager2FAPending{
		UID:       account.UID,
		Username:  account.Username,
		Email:     account.Email,
		IP:        clientIP,
		Language:  lang,
		ExpiresAt: time.Now().Add(manager2FAPendingTTL).Unix(),
	}
	token := util.GenerUUID()
	if err := s.savePending(token, pending, manager2FAPendingTTL); err != nil {
		return "", err
	}
	return token, nil
}

// resend re-issues a code for an existing handshake. It never extends the
// handshake deadline: a caller cannot keep a pending sign-in alive indefinitely
// by asking for more codes.
func (s *manager2FA) resend(ctx context.Context, token, clientIP string) (*manager2FAPending, error) {
	pending, err := s.loadPending(token)
	if err != nil {
		return nil, err
	}
	if pending.Resends >= manager2FAMaxResends {
		return nil, errManager2FAResendsExhausted
	}
	if err := s.checkCooldown(pending.UID); err != nil {
		return nil, err
	}
	code, err := commonapi.GenerateVerifyCode(manager2FACodeLength)
	if err != nil {
		return nil, fmt.Errorf("manager 2fa: generate code: %w", err)
	}
	redis := s.ctx.GetRedisConn()
	if err := redis.SetAndExpire(manager2FACodeKeyPrefix+pending.UID, code, manager2FACodeTTL); err != nil {
		return nil, fmt.Errorf("manager 2fa: persist code: %w", err)
	}
	account := &managerLoginModel{UID: pending.UID, Username: pending.Username, Email: pending.Email}
	if err := s.deliver(ctx, account, code, clientIP, pending.Language); err != nil {
		_ = redis.Del(manager2FACodeKeyPrefix + pending.UID)
		return nil, err
	}
	if err := redis.SetAndExpire(manager2FACooldownKeyPrefix+pending.UID, "1", manager2FAResendCooldown); err != nil {
		return nil, fmt.Errorf("manager 2fa: persist cooldown: %w", err)
	}
	pending.Resends++
	remaining := pending.remaining()
	if remaining <= 0 {
		return nil, errManager2FAInvalid
	}
	if err := s.savePending(token, pending, remaining); err != nil {
		return nil, err
	}
	return pending, nil
}

// verify consumes the handshake. On success the pending record and the code are
// both destroyed, so a token is single-use even if it is replayed immediately.
//
// Every failure path returns errManager2FAInvalid; the reason is logged, never
// returned. The pending record is returned alongside the error when one was
// resolvable, so the caller can attribute the audit row to an account — the HTTP
// response stays identical either way.
func (s *manager2FA) verify(token, code string) (*manager2FAPending, error) {
	pending, err := s.loadPending(token)
	if err != nil {
		return nil, err
	}
	redis := s.ctx.GetRedisConn()
	locked, err := redis.GetString(manager2FALockKeyPrefix + pending.UID)
	if err != nil {
		return nil, fmt.Errorf("manager 2fa: read lock: %w", err)
	}
	if locked != "" {
		s.Warn("管理台二次认证已锁定", zap.String("uid", pending.UID))
		return pending, errManager2FAInvalid
	}
	// Non-release test bypass, same gate as every other verification path in the
	// codebase: IsTestCodeEnabled is hard-false in release mode, so a production
	// deployment cannot be talked into accepting the fixture code.
	if commonapi.MatchTestCode(s.ctx.GetConfig(), code) {
		s.Warn("管理台二次认证通过测试验证码放行", zap.String("uid", pending.UID))
		s.consume(token, pending.UID)
		return pending, nil
	}
	stored, err := redis.GetString(manager2FACodeKeyPrefix + pending.UID)
	if err != nil {
		return nil, fmt.Errorf("manager 2fa: read code: %w", err)
	}
	if stored != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(code)) == 1 {
		s.consume(token, pending.UID)
		return pending, nil
	}
	s.recordFailure(token, pending)
	return pending, errManager2FAInvalid
}

// deliver renders and sends the second-factor email under a bounded deadline.
func (s *manager2FA) deliver(ctx context.Context, account *managerLoginModel, code, clientIP, lang string) error {
	rendered, err := emailtmpl.Render(emailtmpl.KeyManagerLoginCode, lang, emailtmpl.ManagerLoginCodeData{
		Code:     code,
		Username: account.Username,
		// Header-derived and therefore caller-influenced. It reaches only the
		// html/template body (auto-escaped) and the plaintext part — never the
		// subject line, which is text/template and feeds an SMTP header.
		ClientIP:    clientIP,
		RequestedAt: time.Now().Format("2006-01-02 15:04:05 MST"),
		TTLMinutes:  int(manager2FACodeTTL / time.Minute),
	})
	if err != nil {
		return fmt.Errorf("manager 2fa: render email: %w", err)
	}
	sendCtx, cancel := context.WithTimeout(ctx, manager2FASendTimeout)
	defer cancel()
	if err := s.emailSvc.SendTransactionalHTML(sendCtx, account.Email, rendered.Subject, rendered.HTML, rendered.Text); err != nil {
		return fmt.Errorf("manager 2fa: send email: %w", err)
	}
	return nil
}

func (s *manager2FA) checkCooldown(uid string) error {
	active, err := s.ctx.GetRedisConn().GetString(manager2FACooldownKeyPrefix + uid)
	if err != nil {
		return fmt.Errorf("manager 2fa: read cooldown: %w", err)
	}
	if active != "" {
		return errManager2FAResendCooldown
	}
	return nil
}

// recordFailure charges a wrong code against both budgets: the per-handshake
// attempt counter (cheap to reset — just start over) and the per-uid failure
// counter (survives a restart, and trips a lockout).
func (s *manager2FA) recordFailure(token string, pending *manager2FAPending) {
	redis := s.ctx.GetRedisConn()
	pending.Attempts++
	if pending.Attempts >= manager2FAMaxAttempts {
		if err := redis.Del(manager2FAPendingKeyPrefix + token); err != nil {
			s.Error("删除管理台二次认证待验证记录失败", zap.Error(err))
		}
	} else if remaining := pending.remaining(); remaining > 0 {
		if err := s.savePending(token, pending, remaining); err != nil {
			s.Error("更新管理台二次认证尝试次数失败", zap.Error(err))
		}
	}
	failKey := manager2FAFailKeyPrefix + pending.UID
	failures, err := redis.Incr(failKey)
	if err != nil {
		s.Error("累计管理台二次认证失败次数失败", zap.Error(err))
		return
	}
	if failures == 1 {
		if err := redis.Expire(failKey, manager2FACodeLockTTL); err != nil {
			s.Error("设置管理台二次认证失败计数过期失败", zap.Error(err))
		}
	}
	if failures >= manager2FAMaxCodeFailures {
		if err := redis.SetAndExpire(manager2FALockKeyPrefix+pending.UID, "1", manager2FACodeLockTTL); err != nil {
			s.Error("锁定管理台二次认证失败", zap.Error(err))
		}
	}
}

// consume destroys everything the handshake owned. Best-effort: the session has
// already been earned at this point, so a Redis hiccup must not fail the login —
// the code key carries its own TTL as a backstop.
func (s *manager2FA) consume(token, uid string) {
	redis := s.ctx.GetRedisConn()
	for _, key := range []string{
		manager2FAPendingKeyPrefix + token,
		manager2FACodeKeyPrefix + uid,
		manager2FAFailKeyPrefix + uid,
		manager2FALockKeyPrefix + uid,
	} {
		if err := redis.Del(key); err != nil {
			s.Error("清理管理台二次认证缓存失败", zap.String("key", key), zap.Error(err))
		}
	}
}

func (s *manager2FA) savePending(token string, pending *manager2FAPending, ttl time.Duration) error {
	encoded, err := json.Marshal(pending)
	if err != nil {
		return fmt.Errorf("manager 2fa: encode pending: %w", err)
	}
	if err := s.ctx.GetRedisConn().SetAndExpire(manager2FAPendingKeyPrefix+token, string(encoded), ttl); err != nil {
		return fmt.Errorf("manager 2fa: persist pending: %w", err)
	}
	return nil
}

// loadPending resolves a client-supplied token. A malformed, unknown or expired
// token is indistinguishable from a wrong code on the wire.
func (s *manager2FA) loadPending(token string) (*manager2FAPending, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errManager2FAInvalid
	}
	raw, err := s.ctx.GetRedisConn().GetString(manager2FAPendingKeyPrefix + token)
	if err != nil {
		return nil, fmt.Errorf("manager 2fa: read pending: %w", err)
	}
	if raw == "" {
		return nil, errManager2FAInvalid
	}
	var pending manager2FAPending
	if err := json.Unmarshal([]byte(raw), &pending); err != nil {
		s.Error("解析管理台二次认证待验证记录失败", zap.Error(err))
		return nil, errManager2FAInvalid
	}
	if pending.UID == "" || pending.remaining() <= 0 {
		return nil, errManager2FAInvalid
	}
	return &pending, nil
}

// maskManagerEmail renders an address for the sign-in screen: enough to tell the
// operator which mailbox to open, not enough to disclose an address they did not
// already know.
func maskManagerEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	local, domain := email[:at], email[at:]
	if len(local) <= 2 {
		return strings.Repeat("*", len(local)) + domain
	}
	return local[:1] + strings.Repeat("*", len(local)-2) + local[len(local)-1:] + domain
}

// managerOutboundLanguage picks the language for the second-factor email: the
// account's own preference when it has one, otherwise the request's negotiated
// language (falling back to OCTO_DEFAULT_LANGUAGE inside OutboundLanguage).
func managerOutboundLanguage(ctx context.Context, account *managerLoginModel) string {
	if lang := strings.TrimSpace(account.Language); lang != "" {
		return lang
	}
	return octoi18n.OutboundLanguage(ctx)
}
