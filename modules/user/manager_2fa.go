package user

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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
	manager2FASendQuotaPrefix   = "mgr2fa:sends:"
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
	// How much longer than its code a handshake lives. If the two expired
	// together, a code submitted in its final seconds could fail because the
	// handshake went first — reported through the same anti-enumeration error as
	// a wrong code, which is unexplainable to the operator and to whoever reads
	// the ticket. The deadline is always derived from the code's own expiry
	// (see pendingDeadline) rather than from a second independent clock, so the
	// gap holds even when a handshake is minted for a code issued earlier.
	manager2FAPendingGrace = time.Minute
	// Per-uid resend cooldown, mirroring the shared email service's 1 minute.
	manager2FAResendCooldown = time.Minute
	// Wrong-code budget, then a lockout. Counted per UID rather than per
	// handshake: a per-handshake counter would be reset simply by signing in
	// again, which an attacker holding the password can do at will. This one
	// survives starting over, so it is the real brute-force bound.
	manager2FAMaxCodeFailures = 3
	manager2FACodeLockTTL     = 10 * time.Minute
	// Resend budget for a single handshake.
	manager2FAMaxResends = 3
	// Codes per account per hour, across every handshake. The per-handshake
	// resend budget cannot bound this on its own: signing in again mints a fresh
	// handshake with a fresh budget, so an actor holding the password could
	// otherwise mail the administrator a code every minute indefinitely.
	manager2FAMaxSendsPerHour = 10
	manager2FASendQuotaWindow = time.Hour
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
	errManager2FASendQuota        = errors.New("manager 2fa: hourly send quota exhausted")
	errManager2FAAddressMissing   = errors.New("manager 2fa: account has no delivery address")
)

// manager2FACode is the live code plus its own deadline. The deadline is stored
// rather than inferred from the Redis TTL because a handshake minted for an
// already-delivered code has to inherit it — see pendingDeadline.
type manager2FACode struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`
}

// manager2FAPending is the server-side half of the handshake. It is stored under
// an opaque token and never returned to the client.
//
// ExpiresAt is carried inside the record, not inferred from the Redis TTL,
// because every rewrite (attempt counter, resend counter) would otherwise reset
// that TTL and silently extend the window. Rewrites compute the remaining time
// from this field, so the deadline set at issue time is the real one.
type manager2FAPending struct {
	UID      string `json:"uid"`
	Username string `json:"username"`
	// Address is where this handshake's codes go: the account's
	// manager_two_factor_email, never its email identity.
	Address  string `json:"address"`
	IP       string `json:"ip"`
	Language string `json:"language"`
	// PasswordFingerprint is a digest of the stored password hash as it was when
	// the password was accepted. Redeeming the code re-derives it from the
	// current row and refuses on a mismatch, so a password rotated mid-handshake
	// invalidates the sign-in it authorised — the same guarantee the post-fence
	// password re-check gives the single-step path.
	//
	// A digest of the hash, never the password or the hash itself: this record
	// lives in Redis with a 6-minute TTL and only ever needs to answer "is this
	// still the same credential", which equality of digests answers without
	// storing anything usable.
	PasswordFingerprint string `json:"password_fingerprint"`
	Resends             int    `json:"resends"`
	ExpiresAt           int64  `json:"expires_at"`
}

// manager2FAPasswordFingerprint digests a stored password hash for the staleness
// check above.
func manager2FAPasswordFingerprint(passwordHash string) string {
	sum := sha256.Sum256([]byte(passwordHash))
	return hex.EncodeToString(sum[:])
}

func (p *manager2FAPending) remaining() time.Duration {
	return time.Until(time.Unix(p.ExpiresAt, 0))
}

// manager2FA owns the second-factor handshake: code lifecycle, pending store and
// delivery.
//
// It holds a DB handle for one reason only — resending re-reads the delivery
// address, so a SuperAdmin who corrects a typo'd address takes effect on the
// next resend instead of only after the operator restarts the whole sign-in.
type manager2FA struct {
	ctx      *config.Context
	db       *managerDB
	emailSvc commonapi.IEmailService
	log.Log
}

func newManager2FA(ctx *config.Context) *manager2FA {
	return &manager2FA{
		ctx:      ctx,
		db:       newManagerDB(ctx),
		emailSvc: commonapi.NewEmailService(ctx, common.EnsureSystemSettings(ctx)),
		Log:      log.NewTLog("managerTwoFactor"),
	}
}

// start issues a code, delivers it, and returns the opaque handshake token.
//
// Delivery happens BEFORE the code is stored. The reverse order would overwrite
// a live code with one the mail server then refused to deliver, stranding an
// operator who is holding a perfectly good code in their inbox; storing only
// after a successful send leaves the previous code intact when delivery fails.
// The narrow cost is a delivered code that a Redis failure prevented storing —
// the caller sees an error and can retry.
func (s *manager2FA) start(ctx context.Context, account *managerLoginModel, clientIP, lang string) (token string, err error) {
	// A second sign-in while the send cooldown is still active does NOT send
	// another code — but it must still hand back a usable handshake. The
	// handshake token lives only in the browser, so a closed tab or a reload
	// would otherwise strand the operator: the code sitting in their mailbox has
	// nothing left to redeem it against, and they would be locked out for the
	// remainder of the cooldown despite holding a valid code.
	//
	// Re-issuing costs an attacker nothing they did not already have: the
	// wrong-code budget is counted per UID, so starting over does not reset it.
	if err := s.checkCooldown(account.UID); err != nil {
		if !errors.Is(err, errManager2FAResendCooldown) {
			return "", err
		}
		live, liveErr := s.liveCode(account.UID)
		if liveErr != nil {
			return "", liveErr
		}
		if live == nil {
			// Cooling down with no code left to reuse: nothing better to offer
			// than asking the operator to wait.
			return "", err
		}
		return s.issuePending(account, clientIP, lang, live.ExpiresAt)
	}
	if err := s.chargeSendQuota(account.UID); err != nil {
		return "", err
	}
	code, err := commonapi.GenerateVerifyCode(manager2FACodeLength)
	if err != nil {
		return "", fmt.Errorf("manager 2fa: generate code: %w", err)
	}
	if err := s.deliver(ctx, account, account.ManagerTwoFactorEmail, code, clientIP, lang); err != nil {
		return "", err
	}
	expiresAt, err := s.storeCode(account.UID, code)
	if err != nil {
		return "", err
	}
	if err := s.ctx.GetRedisConn().SetAndExpire(
		manager2FACooldownKeyPrefix+account.UID, "1", manager2FAResendCooldown); err != nil {
		return "", fmt.Errorf("manager 2fa: persist cooldown: %w", err)
	}
	return s.issuePending(account, clientIP, lang, expiresAt)
}

// issuePending mints a handshake for an already-delivered code.
//
// codeExpiresAt is the code's own deadline; the handshake outlives it by exactly
// manager2FAPendingGrace, whether the code was just sent or is being reused.
func (s *manager2FA) issuePending(account *managerLoginModel, clientIP, lang string, codeExpiresAt int64) (string, error) {
	pending := &manager2FAPending{
		UID:                 account.UID,
		Username:            account.Username,
		Address:             account.ManagerTwoFactorEmail,
		IP:                  clientIP,
		Language:            lang,
		PasswordFingerprint: manager2FAPasswordFingerprint(account.Password),
		ExpiresAt:           pendingDeadline(codeExpiresAt),
	}
	token := util.GenerUUID()
	ttl := time.Until(time.Unix(pending.ExpiresAt, 0))
	if ttl <= 0 {
		return "", errManager2FAInvalid
	}
	if err := s.savePending(token, pending, ttl); err != nil {
		return "", err
	}
	return token, nil
}

// pendingDeadline keeps the handshake alive slightly past its code.
func pendingDeadline(codeExpiresAt int64) int64 {
	return time.Unix(codeExpiresAt, 0).Add(manager2FAPendingGrace).Unix()
}

// storeCode writes the live code and returns the deadline it was written with.
func (s *manager2FA) storeCode(uid, code string) (int64, error) {
	record := manager2FACode{Code: code, ExpiresAt: time.Now().Add(manager2FACodeTTL).Unix()}
	encoded, err := json.Marshal(record)
	if err != nil {
		return 0, fmt.Errorf("manager 2fa: encode code: %w", err)
	}
	if err := s.ctx.GetRedisConn().SetAndExpire(
		manager2FACodeKeyPrefix+uid, string(encoded), manager2FACodeTTL); err != nil {
		return 0, fmt.Errorf("manager 2fa: persist code: %w", err)
	}
	return record.ExpiresAt, nil
}

// liveCode returns the account's current code, or nil when there is none.
func (s *manager2FA) liveCode(uid string) (*manager2FACode, error) {
	raw, err := s.ctx.GetRedisConn().GetString(manager2FACodeKeyPrefix + uid)
	if err != nil {
		return nil, fmt.Errorf("manager 2fa: read code: %w", err)
	}
	if raw == "" {
		return nil, nil
	}
	var record manager2FACode
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		s.Error("解析管理台二次认证验证码记录失败", zap.Error(err))
		return nil, nil
	}
	if record.Code == "" || time.Now().After(time.Unix(record.ExpiresAt, 0)) {
		return nil, nil
	}
	return &record, nil
}

// chargeSendQuota bounds how many codes one account can be mailed per hour,
// across every handshake it starts.
func (s *manager2FA) chargeSendQuota(uid string) error {
	redis := s.ctx.GetRedisConn()
	key := manager2FASendQuotaPrefix + uid
	sends, err := redis.Incr(key)
	if err != nil {
		return fmt.Errorf("manager 2fa: charge send quota: %w", err)
	}
	if sends == 1 {
		// INCR alone leaves the key immortal; the window starts at the first send.
		if err := redis.Expire(key, manager2FASendQuotaWindow); err != nil {
			s.Error("设置管理台二次认证发送配额过期失败", zap.Error(err))
		}
	}
	if sends > manager2FAMaxSendsPerHour {
		return errManager2FASendQuota
	}
	return nil
}

// resend re-issues a code for an existing handshake. It never extends the
// handshake deadline: a caller cannot keep a pending sign-in alive indefinitely
// by asking for more codes.
//
// The delivery address is re-read from the account rather than taken from the
// handshake, so a SuperAdmin who corrects a typo'd address takes effect on the
// next resend instead of only after the operator restarts the whole sign-in.
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
	account, err := s.db.queryUserInfoWithNameAndPwd(pending.Username)
	if err != nil {
		return nil, fmt.Errorf("manager 2fa: reload account: %w", err)
	}
	if account == nil || account.UID != pending.UID {
		return nil, errManager2FAInvalid
	}
	if strings.TrimSpace(account.ManagerTwoFactorEmail) == "" {
		return nil, errManager2FAAddressMissing
	}
	pending.Address = account.ManagerTwoFactorEmail
	if err := s.chargeSendQuota(pending.UID); err != nil {
		return pending, err
	}
	code, err := commonapi.GenerateVerifyCode(manager2FACodeLength)
	if err != nil {
		return pending, fmt.Errorf("manager 2fa: generate code: %w", err)
	}
	if err := s.deliver(ctx, account, pending.Address, code, clientIP, pending.Language); err != nil {
		return pending, err
	}
	if _, err := s.storeCode(pending.UID, code); err != nil {
		return pending, err
	}
	if err := s.ctx.GetRedisConn().SetAndExpire(
		manager2FACooldownKeyPrefix+pending.UID, "1", manager2FAResendCooldown); err != nil {
		return pending, fmt.Errorf("manager 2fa: persist cooldown: %w", err)
	}
	pending.Resends++
	remaining := pending.remaining()
	if remaining <= 0 {
		return pending, errManager2FAInvalid
	}
	if err := s.savePending(token, pending, remaining); err != nil {
		return pending, err
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
		// A locked account can never redeem this handshake; leaving it in Redis
		// would only invite pointless retries against a dead token.
		s.Warn("管理台二次认证已锁定", zap.String("uid", pending.UID))
		s.dropPending(token)
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
	live, err := s.liveCode(pending.UID)
	if err != nil {
		return nil, err
	}
	if live == nil {
		// No code to be wrong about — expired, already redeemed, or never stored
		// because delivery failed. Charging the wrong-code budget here would let
		// an operator retyping a stale code lock their own account without a
		// single guess having been made.
		s.Warn("管理台二次认证无可用验证码", zap.String("uid", pending.UID))
		return pending, errManager2FAInvalid
	}
	if subtle.ConstantTimeCompare([]byte(live.Code), []byte(code)) == 1 {
		s.consume(token, pending.UID)
		return pending, nil
	}
	s.recordFailure(token, pending)
	return pending, errManager2FAInvalid
}

// deliver renders and sends the second-factor email under a bounded deadline.
func (s *manager2FA) deliver(ctx context.Context, account *managerLoginModel, address, code, clientIP, lang string) error {
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
	if err := s.emailSvc.SendTransactionalHTML(sendCtx, address, rendered.Subject, rendered.HTML, rendered.Text); err != nil {
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

// recordFailure charges a wrong code against the per-uid budget and, once that
// budget is spent, locks the second factor and drops the handshake.
func (s *manager2FA) recordFailure(token string, pending *manager2FAPending) {
	redis := s.ctx.GetRedisConn()
	failKey := manager2FAFailKeyPrefix + pending.UID
	failures, err := redis.Incr(failKey)
	if err != nil {
		s.Error("累计管理台二次认证失败次数失败", zap.Error(err))
		return
	}
	if failures == 1 {
		// INCR alone leaves the key immortal; the window starts at the first
		// failure so a slow trickle of wrong codes cannot accumulate forever.
		if err := redis.Expire(failKey, manager2FACodeLockTTL); err != nil {
			s.Error("设置管理台二次认证失败计数过期失败", zap.Error(err))
		}
	}
	if failures >= manager2FAMaxCodeFailures {
		if err := redis.SetAndExpire(manager2FALockKeyPrefix+pending.UID, "1", manager2FACodeLockTTL); err != nil {
			s.Error("锁定管理台二次认证失败", zap.Error(err))
		}
		s.dropPending(token)
	}
}

// dropPending destroys a handshake that can no longer succeed.
func (s *manager2FA) dropPending(token string) {
	if err := s.ctx.GetRedisConn().Del(manager2FAPendingKeyPrefix + token); err != nil {
		s.Error("删除管理台二次认证待验证记录失败", zap.Error(err))
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
//
// Rune-based, not byte-based: a non-ASCII local part sliced by bytes would be cut
// mid-rune and render as replacement characters — exactly where the operator
// needs to recognise their own address.
func maskManagerEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "***"
	}
	local, domain := []rune(email[:at]), email[at:]
	if len(local) <= 2 {
		return strings.Repeat("*", len(local)) + domain
	}
	return string(local[0]) + strings.Repeat("*", len(local)-2) + string(local[len(local)-1]) + domain
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
