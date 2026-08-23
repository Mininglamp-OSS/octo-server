package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	commonapi "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	appauth "github.com/Mininglamp-OSS/octo-server/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	twoFactorTestPassword = "Adm1n-Passw0rd"
	twoFactorTestIP       = "198.51.100.77"
)

// fakeManagerEmailSender stands in for SMTP. Delivery is not what these tests
// are about, and a real send would make every case depend on a mail server.
type fakeManagerEmailSender struct {
	sends    int
	lastTo   string
	lastHTML string
	lastText string
	failWith error
}

func (f *fakeManagerEmailSender) SendVerifyCode(context.Context, string, commonapi.CodeType, string) error {
	panic("manager 2FA must not route through the shared email-code service")
}

func (f *fakeManagerEmailSender) Verify(context.Context, string, string, commonapi.CodeType) error {
	panic("manager 2FA must not route through the shared email-code service")
}

func (f *fakeManagerEmailSender) SendHTMLEmail(context.Context, string, string, string) error {
	return errors.New("manager 2FA must use SendTransactionalHTML")
}

func (f *fakeManagerEmailSender) SendTransactionalHTML(_ context.Context, to, _, htmlBody, plainBody string) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.sends++
	f.lastTo = to
	f.lastHTML = htmlBody
	f.lastText = plainBody
	return nil
}

type twoFactorHarness struct {
	t      *testing.T
	ctx    *config.Context
	route  *wkhttp.WKHttp
	mgr    *Manager
	mailer *fakeManagerEmailSender
	uid    string
	user   string
	email  string
}

// newTwoFactorHarness boots the manager routes with the second factor in the
// requested state, plus one console account carrying the given address.
func newTwoFactorHarness(t *testing.T, enabled bool, email string) *twoFactorHarness {
	t.Helper()
	return newTwoFactorHarnessWithRole(t, enabled, email, string(wkhttp.Admin))
}

func newTwoFactorHarnessWithRole(t *testing.T, enabled bool, email, role string) *twoFactorHarness {
	t.Helper()
	route, ctx, m := newManagerRouteOnly(t)
	if enabled {
		setSystemSettingForUserTest(t, ctx, "login", "manager_2fa_on", "1", "bool")
	}
	// SystemSettings is a process-wide singleton with a snapshot refreshed on
	// Reload, so the Manager built above sees this write.
	require.NoError(t, commonsettings.EnsureSystemSettings(ctx).Reload())

	mailer := &fakeManagerEmailSender{}
	m.twoFactor.emailSvc = mailer

	hashed, err := HashPassword(twoFactorTestPassword)
	require.NoError(t, err)
	uid := util.GenerUUID()
	username := "admin-" + uid[:8]
	require.NoError(t, m.userDB.Insert(&Model{
		UID:      uid,
		Username: username,
		Name:     "二次认证管理员",
		Password: hashed,
		Email:    email,
		Role:     role,
		Status:   1,
		ShortNo:  uid[:8],
	}))

	h := &twoFactorHarness{
		t: t, ctx: ctx, route: route, mgr: m,
		mailer: mailer, uid: uid, user: username, email: email,
	}
	t.Cleanup(h.cleanupRedis)
	return h
}

func (h *twoFactorHarness) cleanupRedis() {
	redis := h.ctx.GetRedisConn()
	for _, key := range []string{
		manager2FACodeKeyPrefix + h.uid,
		manager2FACooldownKeyPrefix + h.uid,
		manager2FAFailKeyPrefix + h.uid,
		manager2FALockKeyPrefix + h.uid,
	} {
		_ = redis.Del(key)
	}
}

// resetRateLimit clears the strict per-IP bucket. It persists in Redis across
// requests and is NOT cleared by CleanAllTables, so a loop of requests from one
// address would otherwise trip the limiter instead of the guard under test.
func (h *twoFactorHarness) resetRateLimit(tags ...string) {
	for _, tag := range tags {
		_ = h.ctx.GetRedisConn().Del("ratelimit:strict:" + tag + ":" + twoFactorTestIP)
	}
}

func (h *twoFactorHarness) post(path string, body map[string]interface{}) *httptest.ResponseRecorder {
	h.t.Helper()
	h.resetRateLimit(managerLoginRateLimitTag, manager2FAVerifyRateLimitTag, manager2FAResendRateLimitTag)
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(util.ToJson(body))))
	require.NoError(h.t, err)
	req.Header.Set("Content-Type", "application/json")
	setPublicIPForUserTest(req, twoFactorTestIP)
	h.route.ServeHTTP(w, req)
	return w
}

func (h *twoFactorHarness) login() *httptest.ResponseRecorder {
	return h.post("/v1/manager/login", map[string]interface{}{
		"username": h.user,
		"password": twoFactorTestPassword,
	})
}

// startHandshake performs step one and returns the handshake token.
func (h *twoFactorHarness) startHandshake() string {
	h.t.Helper()
	w := h.login()
	require.Equal(h.t, http.StatusOK, w.Code, w.Body.String())
	var resp managerLoginTwoFactorResp
	require.NoError(h.t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.True(h.t, resp.TwoFactorRequired)
	require.NotEmpty(h.t, resp.TwoFactorToken)
	return resp.TwoFactorToken
}

// issuedCode reads the code straight out of its uid-keyed bucket, which is both
// deterministic and a direct assertion that the bucket is the uid-keyed one.
func (h *twoFactorHarness) issuedCode() string {
	h.t.Helper()
	code, err := h.ctx.GetRedisConn().GetString(manager2FACodeKeyPrefix + h.uid)
	require.NoError(h.t, err)
	require.NotEmpty(h.t, code, "no code stored under the uid-keyed bucket")
	return code
}

func (h *twoFactorHarness) pending(token string) *manager2FAPending {
	h.t.Helper()
	raw, err := h.ctx.GetRedisConn().GetString(manager2FAPendingKeyPrefix + token)
	require.NoError(h.t, err)
	if raw == "" {
		return nil
	}
	var p manager2FAPending
	require.NoError(h.t, json.Unmarshal([]byte(raw), &p))
	return &p
}

func (h *twoFactorHarness) loginLogStatuses() map[int]int {
	h.t.Helper()
	var rows []*LoginLogModel
	_, err := h.ctx.DB().Select("*").From("login_log").Load(&rows)
	require.NoError(h.t, err)
	counts := map[int]int{}
	for _, row := range rows {
		counts[row.Status]++
	}
	return counts
}

// TestManagerLoginTwoFactorOffIsUnchanged pins the compatibility contract: with
// the switch off the endpoint still returns a session in one step.
func TestManagerLoginTwoFactorOffIsUnchanged(t *testing.T) {
	h := newTwoFactorHarness(t, false, "admin@example.com")

	w := h.login()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp managerLoginResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, h.uid, resp.UID)
	assert.NotEmpty(t, resp.Token)
	assert.Equal(t, 0, h.mailer.sends, "no code may be sent while the switch is off")
}

// TestManagerLoginTwoFactorIssuesSessionOnlyAfterCode walks the whole handshake.
func TestManagerLoginTwoFactorIssuesSessionOnlyAfterCode(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")

	w := h.login()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	// The step-one body must not carry a session in any shape.
	assert.NotContains(t, w.Body.String(), `"token"`)
	assert.NotContains(t, w.Body.String(), `"uid"`)
	assert.NotContains(t, w.Body.String(), `"role"`)

	var pendingResp managerLoginTwoFactorResp
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &pendingResp))
	assert.True(t, pendingResp.TwoFactorRequired)
	assert.Equal(t, "a***n@example.com", pendingResp.EmailMasked)
	assert.Equal(t, int(manager2FAPendingTTL.Seconds()), pendingResp.ExpiresIn)
	require.Equal(t, 1, h.mailer.sends)
	assert.Equal(t, "admin@example.com", h.mailer.lastTo)

	code := h.issuedCode()
	assert.Contains(t, h.mailer.lastText, code, "the delivered email must carry the issued code")
	assert.Contains(t, h.mailer.lastHTML, twoFactorTestIP, "the email must name the source IP so an unexpected sign-in is recognisable")

	verify := h.post("/v1/manager/login/verify", map[string]interface{}{
		"two_factor_token": pendingResp.TwoFactorToken,
		"code":             code,
	})
	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
	var session managerLoginResp
	require.NoError(t, json.Unmarshal(verify.Body.Bytes(), &session))
	assert.Equal(t, h.uid, session.UID)
	assert.NotEmpty(t, session.Token)

	assert.Nil(t, h.pending(pendingResp.TwoFactorToken), "handshake must be single-use")
	stored, err := h.ctx.GetRedisConn().GetString(manager2FACodeKeyPrefix + h.uid)
	require.NoError(t, err)
	assert.Empty(t, stored, "code must be destroyed once redeemed")

	counts := h.loginLogStatuses()
	assert.Equal(t, 1, counts[loginStatusPendingSecondFactor], "password-ok-awaiting-code must be auditable")
	assert.Equal(t, 1, counts[loginStatusSuccess])

	// The returned string must be a materialised session, not just a token
	// shaped like one: the store must know it as this account's web session.
	//
	// Asserted through the store rather than by calling an authenticated route:
	// this harness mounts octo-lib's stock AuthMiddleware, whereas main.go wires
	// the session-aware CacheTokenParser, so an authed call here would 401 for a
	// plain single-step sign-in too.
	issued, err := h.mgr.sessionStore.DeviceToken(context.Background(), h.uid, int(config.Web))
	require.NoError(t, err)
	assert.Equal(t, session.Token, issued)
}

// TestManagerLoginTwoFactorCoversEveryConsoleRole pins that the second factor
// applies to every role /v1/manager/login admits — not just admin.
//
// The enable guard and the sign-in path must agree on that set: a role the guard
// ignores but sign-in enforces would be locked out the moment the switch is
// turned on, which is exactly what the guard exists to prevent.
func TestManagerLoginTwoFactorCoversEveryConsoleRole(t *testing.T) {
	for _, role := range appauth.ManagerConsoleRoles {
		t.Run(role, func(t *testing.T) {
			h := newTwoFactorHarnessWithRole(t, true, "admin@example.com", role)
			token := h.startHandshake()
			w := h.post("/v1/manager/login/verify", map[string]interface{}{
				"two_factor_token": token,
				"code":             h.issuedCode(),
			})
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var session managerLoginResp
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &session))
			assert.Equal(t, role, session.Role)
			assert.NotEmpty(t, session.Token)
		})
	}
}

// TestManagerLoginTwoFactorSendFailureLeavesNothingBehind pins the cleanup path:
// a code nobody received must not stay redeemable, and no handshake may be handed
// out for a message that never went anywhere.
func TestManagerLoginTwoFactorSendFailureLeavesNothingBehind(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	h.mailer.failWith = errors.New("smtp unreachable")

	w := h.login()
	assert.Contains(t, w.Body.String(), "err.server.user.email_send_failed")

	code, err := h.ctx.GetRedisConn().GetString(manager2FACodeKeyPrefix + h.uid)
	require.NoError(t, err)
	assert.Empty(t, code, "an undelivered code must not stay redeemable")
	cooldown, err := h.ctx.GetRedisConn().GetString(manager2FACooldownKeyPrefix + h.uid)
	require.NoError(t, err)
	assert.Empty(t, cooldown, "a failed send must not burn the resend cooldown")

	// The operator can retry immediately once the mail path recovers.
	h.mailer.failWith = nil
	h.startHandshake()
}

// TestManagerLoginTwoFactorRejectsAccountWithoutEmail pins fail-closed: no
// address means no session, never a silent single-step sign-in.
func TestManagerLoginTwoFactorRejectsAccountWithoutEmail(t *testing.T) {
	h := newTwoFactorHarness(t, true, "")

	w := h.login()
	assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_email_missing")
	assert.NotContains(t, w.Body.String(), `"token"`)
	assert.Equal(t, 0, h.mailer.sends)
}

// TestManagerLoginTwoFactorFailuresAreIndistinguishable is the anti-enumeration
// contract: a wrong code, an unknown token and an expired handshake must be one
// response, so an attacker cannot learn which half they got right.
func TestManagerLoginTwoFactorFailuresAreIndistinguishable(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	token := h.startHandshake()

	expiredToken := util.GenerUUID()
	expired, err := json.Marshal(&manager2FAPending{
		UID: h.uid, Username: h.user, Email: h.email,
		ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	})
	require.NoError(t, err)
	require.NoError(t, h.ctx.GetRedisConn().SetAndExpire(
		manager2FAPendingKeyPrefix+expiredToken, string(expired), time.Minute))

	bodies := make([]string, 0, 3)
	for _, tc := range []struct {
		name  string
		token string
		code  string
	}{
		{"wrong code", token, "000000"},
		{"unknown token", util.GenerUUID(), "123456"},
		{"expired handshake", expiredToken, "123456"},
	} {
		w := h.post("/v1/manager/login/verify", map[string]interface{}{
			"two_factor_token": tc.token,
			"code":             tc.code,
		})
		assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_code_invalid", tc.name)
		bodies = append(bodies, w.Body.String())
	}
	for i := 1; i < len(bodies); i++ {
		assert.Equal(t, bodies[0], bodies[i], "failure responses must be byte-identical")
	}
}

// TestManagerLoginTwoFactorWrongCodeBudget pins that a handshake is destroyed
// once the wrong-code budget is spent, so a token cannot be brute-forced code by
// code — and that the genuine code cannot revive it afterwards.
func TestManagerLoginTwoFactorWrongCodeBudget(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	token := h.startHandshake()
	code := h.issuedCode()

	for i := 0; i < manager2FAMaxCodeFailures; i++ {
		w := h.post("/v1/manager/login/verify", map[string]interface{}{
			"two_factor_token": token,
			"code":             "000000",
		})
		assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_code_invalid", "attempt %d", i+1)
	}
	assert.Nil(t, h.pending(token), "handshake must be destroyed once the wrong-code budget is spent")

	w := h.post("/v1/manager/login/verify", map[string]interface{}{
		"two_factor_token": token,
		"code":             code,
	})
	assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_code_invalid")
}

// TestManagerTwoFactorBudgetSurvivesNewHandshake pins that the wrong-code budget
// is counted per account, not per handshake: an attacker holding the password
// can mint handshakes at will, so a per-handshake counter would be free to reset.
func TestManagerTwoFactorBudgetSurvivesNewHandshake(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	first := h.startHandshake()
	code := h.issuedCode()

	for i := 0; i < manager2FAMaxCodeFailures-1; i++ {
		h.post("/v1/manager/login/verify", map[string]interface{}{
			"two_factor_token": first,
			"code":             "000000",
		})
	}

	// Start over: a fresh handshake, same account.
	second := h.startHandshake()
	h.post("/v1/manager/login/verify", map[string]interface{}{
		"two_factor_token": second,
		"code":             "000000",
	})

	locked, err := h.ctx.GetRedisConn().GetString(manager2FALockKeyPrefix + h.uid)
	require.NoError(t, err)
	assert.NotEmpty(t, locked, "restarting the sign-in must not reset the wrong-code budget")

	w := h.post("/v1/manager/login/verify", map[string]interface{}{
		"two_factor_token": second,
		"code":             code,
	})
	assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_code_invalid")
}

// TestManagerLoginTwoFactorResendBudget pins both resend guarantees: a bounded
// number of sends, and a deadline that resending never extends.
func TestManagerLoginTwoFactorResendBudget(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	token := h.startHandshake()
	deadline := h.pending(token).ExpiresAt

	for i := 0; i < manager2FAMaxResends; i++ {
		// Skip the per-uid cooldown, which is about pacing, not budget.
		require.NoError(t, h.ctx.GetRedisConn().Del(manager2FACooldownKeyPrefix+h.uid))
		w := h.post("/v1/manager/login/resend", map[string]interface{}{"two_factor_token": token})
		require.Equal(t, http.StatusOK, w.Code, "resend %d: %s", i+1, w.Body.String())
		assert.Equal(t, deadline, h.pending(token).ExpiresAt, "resend must not extend the handshake deadline")
	}

	require.NoError(t, h.ctx.GetRedisConn().Del(manager2FACooldownKeyPrefix+h.uid))
	w := h.post("/v1/manager/login/resend", map[string]interface{}{"two_factor_token": token})
	assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_resend_exhausted")
	assert.Equal(t, 1+manager2FAMaxResends, h.mailer.sends)
}

// TestManagerLoginTwoFactorResendCooldown pins that back-to-back resends are
// paced, so the endpoint cannot be used to bomb an administrator's mailbox.
func TestManagerLoginTwoFactorResendCooldown(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	token := h.startHandshake()

	w := h.post("/v1/manager/login/resend", map[string]interface{}{"two_factor_token": token})
	assert.Contains(t, w.Body.String(), "err.server.user.email_rate_limited")
	assert.Equal(t, 1, h.mailer.sends, "cooldown must suppress the send itself, not just the response")
}

// TestManagerLoginDuringCooldownReissuesHandshake pins that a reload or a closed
// tab does not strand an operator who already holds a valid code.
//
// The handshake token lives only in the browser. Without this behaviour, signing
// in again inside the send cooldown would fail — leaving a perfectly good code in
// the mailbox with nothing to redeem it against for up to a minute.
func TestManagerLoginDuringCooldownReissuesHandshake(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	first := h.startHandshake()
	code := h.issuedCode()

	second := h.startHandshake()
	assert.NotEqual(t, first, second, "a repeat sign-in must mint a fresh handshake")
	assert.Equal(t, 1, h.mailer.sends, "the cooldown must still suppress a second email")
	assert.Equal(t, code, h.issuedCode(), "the live code must be left intact for reuse")

	w := h.post("/v1/manager/login/verify", map[string]interface{}{
		"two_factor_token": second,
		"code":             code,
	})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// TestManagerLoginDuringCooldownWithoutLiveCode pins the other half: once the
// code itself is gone there is nothing to re-issue, so the operator is told to
// wait rather than handed a handshake that can never be satisfied.
func TestManagerLoginDuringCooldownWithoutLiveCode(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	h.startHandshake()
	require.NoError(t, h.ctx.GetRedisConn().Del(manager2FACodeKeyPrefix+h.uid))

	w := h.login()
	assert.Contains(t, w.Body.String(), "err.server.user.email_rate_limited")
}

// TestManagerTwoFactorHandshakeDiesWithPasswordRotation pins that a password
// change invalidates an in-flight second-factor sign-in.
//
// Without it the handshake would be a window in which a captured password keeps
// working after the victim has rotated it — the single-step path closes that
// window with a post-fence password re-check, and the two paths must not
// disagree about it.
func TestManagerTwoFactorHandshakeDiesWithPasswordRotation(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	token := h.startHandshake()
	code := h.issuedCode()

	rotated, err := HashPassword("An0ther-Passw0rd")
	require.NoError(t, err)
	require.NoError(t, h.mgr.userDB.UpdateUsersWithField("password", rotated, h.uid))

	w := h.post("/v1/manager/login/verify", map[string]interface{}{
		"two_factor_token": token,
		"code":             code,
	})
	assert.Contains(t, w.Body.String(), "err.server.user.invalid_credentials")
	assert.NotContains(t, w.Body.String(), `"token"`)
}

// TestManagerTwoFactorIsolatedFromPublicEmailKeyspace is the regression test for
// the reason this flow does not reuse EmailService.SendVerifyCode / Verify.
//
// Those helpers key their cooldown and lockout by email address alone, and the
// public unauthenticated endpoints /v1/user/email/sendcode and
// /v1/user/emaillogin write exactly those keys. If manager 2FA shared them,
// anyone who could guess an administrator's address could jam the console:
// one request a minute keeps the cooldown alive so no code is ever sent, and
// three bad codes trip a ten-minute lock so no code can ever be redeemed.
//
// The test asserts the sign-in survives both keys being fully hostile.
func TestManagerTwoFactorIsolatedFromPublicEmailKeyspace(t *testing.T) {
	const email = "admin@example.com"
	h := newTwoFactorHarness(t, true, email)

	redis := h.ctx.GetRedisConn()
	jammed := []string{
		fmt.Sprintf("email_rate_limit:%s", email),
		fmt.Sprintf("email_verify_lock:%s", email),
		fmt.Sprintf("email_verify_fail:%s", email),
	}
	for _, key := range jammed {
		require.NoError(t, redis.SetAndExpire(key, "1", 10*time.Minute))
	}
	t.Cleanup(func() {
		for _, key := range jammed {
			_ = redis.Del(key)
		}
	})

	token := h.startHandshake()
	require.Equal(t, 1, h.mailer.sends, "a jammed shared cooldown must not block the manager code")

	verify := h.post("/v1/manager/login/verify", map[string]interface{}{
		"two_factor_token": token,
		"code":             h.issuedCode(),
	})
	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
	var session managerLoginResp
	require.NoError(t, json.Unmarshal(verify.Body.Bytes(), &session))
	assert.NotEmpty(t, session.Token, "a jammed shared lock must not block redeeming the manager code")
}

// TestManagerTwoFactorDoesNotWritePublicEmailKeys is the other direction: a
// failed console sign-in must not lock the same address out of the ordinary
// user-facing email flows.
func TestManagerTwoFactorDoesNotWritePublicEmailKeys(t *testing.T) {
	const email = "admin@example.com"
	h := newTwoFactorHarness(t, true, email)
	token := h.startHandshake()

	for i := 0; i < manager2FAMaxCodeFailures; i++ {
		h.post("/v1/manager/login/verify", map[string]interface{}{
			"two_factor_token": token,
			"code":             "000000",
		})
	}

	redis := h.ctx.GetRedisConn()
	for _, key := range []string{
		fmt.Sprintf("email_rate_limit:%s", email),
		fmt.Sprintf("email_verify_lock:%s", email),
		fmt.Sprintf("email_verify_fail:%s", email),
	} {
		value, err := redis.GetString(key)
		require.NoError(t, err)
		assert.Empty(t, value, "manager 2FA must not touch the shared key %s", key)
	}
}

// TestManagerTwoFactorLockout pins the per-uid lock, which unlike the
// per-handshake cap survives starting a fresh sign-in.
func TestManagerTwoFactorLockout(t *testing.T) {
	h := newTwoFactorHarness(t, true, "admin@example.com")
	token := h.startHandshake()

	for i := 0; i < manager2FAMaxCodeFailures; i++ {
		h.post("/v1/manager/login/verify", map[string]interface{}{
			"two_factor_token": token,
			"code":             "000000",
		})
	}
	locked, err := h.ctx.GetRedisConn().GetString(manager2FALockKeyPrefix + h.uid)
	require.NoError(t, err)
	require.NotEmpty(t, locked, "repeated wrong codes must lock the account's second factor")

	// The correct code is refused while the lock stands.
	w := h.post("/v1/manager/login/verify", map[string]interface{}{
		"two_factor_token": token,
		"code":             h.issuedCode(),
	})
	assert.Contains(t, w.Body.String(), "err.server.user.manager_2fa_code_invalid")
}

func TestMaskManagerEmail(t *testing.T) {
	cases := map[string]string{
		"admin@example.com": "a***n@example.com",
		"ab@example.com":    "**@example.com",
		"a@example.com":     "*@example.com",
		"not-an-email":      "***",
		"":                  "***",
	}
	for input, want := range cases {
		assert.Equal(t, want, maskManagerEmail(input), "input=%q", input)
	}
}

// TestManagerTwoFactorPendingOutlivesCode pins the deliberate one-minute gap: if
// the handshake expired first, a code submitted in its final seconds would fail
// through the anti-enumeration error, which is unexplainable to the operator.
func TestManagerTwoFactorPendingOutlivesCode(t *testing.T) {
	assert.Greater(t, manager2FAPendingTTL, manager2FACodeTTL)
}
