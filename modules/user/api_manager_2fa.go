package user

import (
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"go.uber.org/zap"
)

// manager2FALoginType tags the login_log rows produced by the second step, so
// "wrong password" and "right password, wrong code" stay separable in the audit
// table. Fits login_log.login_type VARCHAR(20).
const manager2FALoginType = "manager_2fa"

// managerLoginTwoFactorResp is the /v1/manager/login body when the second factor
// is on. It deliberately shares no field with managerLoginResp: a client that
// ignores two_factor_required must not find an empty token and mistake this for
// a completed sign-in.
type managerLoginTwoFactorResp struct {
	TwoFactorRequired bool   `json:"two_factor_required"`
	TwoFactorToken    string `json:"two_factor_token"`
	EmailMasked       string `json:"email_masked"`
	ExpiresIn         int    `json:"expires_in"`
}

type managerLoginVerifyReq struct {
	TwoFactorToken string `json:"two_factor_token"`
	Code           string `json:"code"`
}

type managerLoginResendReq struct {
	TwoFactorToken string `json:"two_factor_token"`
}

// beginTwoFactor is the tail of step one: credentials are already verified, but
// no session exists yet and no session fence has been opened. It sends the code
// and hands back an opaque handshake token.
func (m *Manager) beginTwoFactor(c *wkhttp.Context, account *managerLoginModel, publicIP string) {
	if strings.TrimSpace(account.Email) == "" {
		// Unreachable for accounts that existed when the switch was turned on
		// (the write path refuses that), reachable for one created without an
		// address afterwards. Fail closed and say so: silently skipping the
		// second factor would make the switch a lie.
		m.Warn("管理台账号未配置邮箱，无法进行二次认证",
			zap.String("uid", account.UID), zap.String("username", account.Username))
		respondUserError(c, errcode.ErrUserManager2FAEmailMissing)
		return
	}
	lang := managerOutboundLanguage(c.Request.Context(), account)
	token, err := m.twoFactor.start(c.Request.Context(), account, publicIP, lang)
	if err != nil {
		m.respondTwoFactorDeliveryError(c, account.UID, err)
		return
	}
	m.loginLog.recordPendingSecondFactor(account.UID, account.Username, publicIP, manager2FALoginType)
	c.Response(&managerLoginTwoFactorResp{
		TwoFactorRequired: true,
		TwoFactorToken:    token,
		EmailMasked:       maskManagerEmail(account.Email),
		ExpiresIn:         int(manager2FAPendingTTL.Seconds()),
	})
}

// loginVerify is step two: redeem the code and mint the session.
//
// Unauthenticated by design — it IS part of the pre-auth sign-in handshake, so
// there is no session to authenticate against. The handshake token plus the
// emailed code are the credential; abuse is bounded by the per-IP limiter on the
// route and the per-uid wrong-code lockout.
func (m *Manager) loginVerify(c *wkhttp.Context) {
	var req managerLoginVerifyReq
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	req.TwoFactorToken = strings.TrimSpace(req.TwoFactorToken)
	req.Code = strings.TrimSpace(req.Code)
	if req.TwoFactorToken == "" {
		respondUserRequestInvalid(c, "two_factor_token")
		return
	}
	if req.Code == "" {
		respondUserRequestInvalid(c, "code")
		return
	}
	publicIP := wkhttp.ClientIP(c.Request)
	pending, err := m.twoFactor.verify(req.TwoFactorToken, req.Code)
	if err != nil {
		if errors.Is(err, errManager2FAInvalid) {
			// Only a resolvable handshake produces an audit row. An unknown or
			// forged token has no account to attribute and would let an
			// unauthenticated caller write rows at will.
			if pending != nil {
				m.loginLog.recordFailure(pending.Username, publicIP, manager2FALoginType)
			}
			respondUserError(c, errcode.ErrUserManager2FACodeInvalid)
			return
		}
		m.Error("管理台二次认证校验失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserTokenCacheFailed)
		return
	}
	m.logTwoFactorIPDrift(pending, publicIP)
	// Re-reads the account and re-checks state, role and credential before
	// issuing: the handshake may have outlived a ban, a destroy request, a role
	// change — or the password rotation that should invalidate it.
	m.issueManagerSession(c, pending.Username, pending.UID, publicIP, func(fenced *managerLoginModel) bool {
		return manager2FAPasswordFingerprint(fenced.Password) == pending.PasswordFingerprint
	})
}

// loginResend re-sends the code for a pending handshake.
//
// It exists so the second step never has to ask for the password again: making
// the client re-post credentials would force the sign-in page to keep the
// plaintext password in memory across the whole handshake, and would mint a new
// handshake on every retry. Unauthenticated for the same reason as loginVerify.
func (m *Manager) loginResend(c *wkhttp.Context) {
	var req managerLoginResendReq
	if err := c.BindJSON(&req); err != nil {
		respondUserRequestInvalid(c, "")
		return
	}
	req.TwoFactorToken = strings.TrimSpace(req.TwoFactorToken)
	if req.TwoFactorToken == "" {
		respondUserRequestInvalid(c, "two_factor_token")
		return
	}
	publicIP := wkhttp.ClientIP(c.Request)
	pending, err := m.twoFactor.resend(c.Request.Context(), req.TwoFactorToken, publicIP)
	if err != nil {
		if errors.Is(err, errManager2FAInvalid) {
			respondUserError(c, errcode.ErrUserManager2FACodeInvalid)
			return
		}
		uid := ""
		if pending != nil {
			uid = pending.UID
		}
		m.respondTwoFactorDeliveryError(c, uid, err)
		return
	}
	m.logTwoFactorIPDrift(pending, publicIP)
	c.Response(&managerLoginTwoFactorResp{
		TwoFactorRequired: true,
		TwoFactorToken:    req.TwoFactorToken,
		EmailMasked:       maskManagerEmail(pending.Email),
		// The deadline is the one set when the handshake was created; resending
		// never extends it.
		ExpiresIn: int(pending.remaining().Seconds()),
	})
}

// respondTwoFactorDeliveryError maps a send-path failure onto the wire. The two
// client-actionable cases (wait a minute / start over) keep their own codes; the
// rest are infrastructure and collapse onto the internal send-failure code with
// the cause logged.
func (m *Manager) respondTwoFactorDeliveryError(c *wkhttp.Context, uid string, err error) {
	switch {
	case errors.Is(err, errManager2FAResendCooldown):
		respondUserError(c, errcode.ErrUserEmailRateLimited)
	case errors.Is(err, errManager2FAResendsExhausted):
		respondUserError(c, errcode.ErrUserManager2FAResendExhausted)
	default:
		m.Error("发送管理台二次认证验证码失败", zap.String("uid", uid), zap.Error(err))
		respondUserError(c, errcode.ErrUserEmailSendFailed)
	}
}

// logTwoFactorIPDrift notes, without blocking, that the two halves of a handshake
// arrived from different addresses.
//
// Deliberately not an enforcement point: a phone dropping from Wi-Fi to cellular
// mid-sign-in changes address routinely, and rejecting that would lock out
// legitimate operators far more often than it would stop an attacker who, by
// this point, already holds both the password and the mailbox.
func (m *Manager) logTwoFactorIPDrift(pending *manager2FAPending, publicIP string) {
	if pending == nil || pending.IP == "" || pending.IP == publicIP {
		return
	}
	m.Warn("管理台二次认证前后请求 IP 不一致",
		zap.String("uid", pending.UID),
		zap.String("start_ip", pending.IP),
		zap.String("verify_ip", publicIP))
}
