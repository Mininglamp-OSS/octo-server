package user

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	commonbase "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	commonsettings "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var managerMFAIntegrationIP uint32

type managerMFASMTPSink struct {
	listener net.Listener
	failAuth bool
	mu       sync.Mutex
	messages [][]byte
}

func newManagerMFASMTPSink(t *testing.T, failAuth bool) *managerMFASMTPSink {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	sink := &managerMFASMTPSink{listener: listener, failAuth: failAuth}
	t.Cleanup(func() { _ = listener.Close() })
	go sink.serve()
	return sink
}

func (s *managerMFASMTPSink) address() string { return s.listener.Addr().String() }

func (s *managerMFASMTPSink) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *managerMFASMTPSink) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	write := func(response string) error {
		_, err := fmt.Fprint(conn, response)
		return err
	}
	if err := write("220 test.smtp ESMTP ready\r\n"); err != nil {
		return
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			if err := write("250-test.smtp\r\n250-AUTH PLAIN\r\n250 OK\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(command, "AUTH"):
			if s.failAuth {
				if err := write("535 5.7.8 authentication failed\r\n"); err != nil {
					return
				}
			} else if err := write("235 2.7.0 authenticated\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(command, "MAIL FROM"):
			if err := write("250 2.1.0 sender ok\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(command, "RCPT TO"):
			if err := write("250 2.1.5 recipient ok\r\n"); err != nil {
				return
			}
		case command == "DATA":
			if err := write("354 3.0.0 end with <CRLF>.<CRLF>\r\n"); err != nil {
				return
			}
			var message strings.Builder
			for {
				dataLine, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				if dataLine == ".\r\n" || dataLine == ".\n" {
					break
				}
				message.WriteString(dataLine)
			}
			s.mu.Lock()
			s.messages = append(s.messages, []byte(message.String()))
			s.mu.Unlock()
			if err := write("250 2.0.0 queued\r\n"); err != nil {
				return
			}
		case command == "QUIT":
			_ = write("221 2.0.0 bye\r\n")
			return
		default:
			if err := write("250 OK\r\n"); err != nil {
				return
			}
		}
	}
}

func newManagerMFAIntegration(t *testing.T, failAuth bool) (*wkhttp.WKHttp, *config.Context, *commonsettings.SystemSettings, string, string) {
	t.Helper()
	server, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := commonsettings.EnsureSystemSettings(ctx)
	smtp := newManagerMFASMTPSink(t, failAuth)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		_ = settings.Load()
	})

	for _, item := range []struct {
		category string
		key      string
		value    string
		kind     string
	}{
		{category: "support", key: "email", value: "mfa-sender@example.com", kind: "string"},
		{category: "support", key: "email_smtp", value: smtp.address(), kind: "string"},
		{category: "support", key: "email_pwd", value: "smtp-password", kind: "string"},
		{category: "login", key: "manager_email_mfa_on", value: "1", kind: "bool"},
	} {
		_, err := ctx.DB().InsertBySql(
			"INSERT INTO system_setting (category, key_name, value, value_type, description) VALUES (?, ?, ?, ?, ?) "+
				"ON DUPLICATE KEY UPDATE value = VALUES(value), value_type = VALUES(value_type)",
			item.category, item.key, item.value, item.kind, "manager MFA integration test",
		).Exec()
		require.NoError(t, err)
	}
	// The manager MFA flow uses the database-backed SMTP provider directly. No
	// startup or reload preflight is required; the real send path is exercised
	// by the handler tests below.
	require.NoError(t, settings.Load())

	password := "manager-mfa-password"
	hash, err := HashPassword(password)
	require.NoError(t, err)
	uid := "mfa-" + util.GenerUUID()[:20]
	recipientEmail := uid + "@example.com"
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID:      uid,
		Username: uid,
		Name:     "MFA Manager",
		Email:    recipientEmail,
		Password: hash,
		Role:     string(wkhttp.SuperAdmin),
		Status:   StatusEnable.Int(),
	}))

	// The manager rate limiter is keyed by client IP. Each test gets a unique
	// private test address so unrelated manager login tests cannot consume its
	// burst budget.
	return server.GetRoute(), ctx, settings, password, recipientEmail
}

func managerMFARequest(t *testing.T, method, path string, body interface{}) *http.Request {
	t.Helper()
	ipOctet := atomic.AddUint32(&managerMFAIntegrationIP, 1)%240 + 10
	return managerMFARequestFromIP(t, fmt.Sprintf("198.51.100.%d", ipOctet), method, path, body)
}

func managerMFARequestFromIP(t *testing.T, ip, method, path string, body interface{}) *http.Request {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.RemoteAddr = ip + ":1234"
	return req
}

func serveManagerMFARequest(t *testing.T, route *wkhttp.WKHttp, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	route.ServeHTTP(recorder, req)
	return recorder
}

func TestManagerConsoleMFAHandlerFullFlowAndReplay(t *testing.T) {
	route, ctx, _, password, recipientEmail := newManagerMFAIntegration(t, false)
	// The generated manager username is discoverable from the only management
	// user in this isolated database.
	var user Model
	_, err := ctx.DB().Select("uid,username").From("user").Where("role=?", string(wkhttp.SuperAdmin)).Load(&user)
	require.NoError(t, err)
	login := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login", map[string]string{
		"username": user.Username,
		"password": password,
	}))
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var challengeResp managerLoginResp
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &challengeResp))
	assert.True(t, challengeResp.MFARequired)
	assert.NotEmpty(t, challengeResp.ChallengeID)
	assert.Equal(t, maskManagerEmail(recipientEmail), challengeResp.Email)
	assert.Empty(t, challengeResp.Token)

	send := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/send", map[string]string{
		"challenge_id": challengeResp.ChallengeID,
	}))
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	var sendResp managerMFAChallengeResponse
	require.NoError(t, json.Unmarshal(send.Body.Bytes(), &sendResp))
	assert.Equal(t, challengeResp.ChallengeID, sendResp.ChallengeID)
	assert.Equal(t, maskManagerEmail(recipientEmail), sendResp.Email)
	assert.Greater(t, sendResp.ExpiresIn, int64(0))
	assert.True(t, sendResp.CodeSent)
	assert.Equal(t, int64(managerMFASendCooldown.Seconds()), sendResp.ResendAfter)
	code, err := ctx.GetRedisConn().GetString(commonbase.EmailCodeKey(recipientEmail, commonbase.CodeTypeManagerLogin))
	require.NoError(t, err)
	require.Regexp(t, `^[0-9]{6}$`, code)
	status, err := ctx.GetRedisConn().GetString(commonbase.EmailCodeStatusKey(recipientEmail, commonbase.CodeTypeManagerLogin))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(status, "sent:"), "manager code status must carry sent attempt ownership: %q", status)

	verify := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/verify", map[string]string{
		"challenge_id": challengeResp.ChallengeID,
		"code":         code,
	}))
	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
	var tokenResp managerLoginResp
	require.NoError(t, json.Unmarshal(verify.Body.Bytes(), &tokenResp))
	assert.NotEmpty(t, tokenResp.Token)
	assert.Equal(t, maskManagerEmail(recipientEmail), tokenResp.Email)

	replay := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/verify", map[string]string{
		"challenge_id": challengeResp.ChallengeID,
		"code":         code,
	}))
	assert.NotEqual(t, http.StatusOK, replay.Code)
	assert.NotContains(t, replay.Body.String(), `"token":"`+tokenResp.Token+`"`)

	resendAfterSuccess := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/resend", map[string]string{
		"challenge_id": challengeResp.ChallengeID,
	}))
	assert.Equal(t, http.StatusBadRequest, resendAfterSuccess.Code, resendAfterSuccess.Body.String())
}

func TestManagerConsoleMFAHappyPathDoesNotHitIPRateLimit(t *testing.T) {
	route, ctx, _, password, recipientEmail := newManagerMFAIntegration(t, false)
	var user Model
	_, err := ctx.DB().Select("uid,username").From("user").Where("role=?", string(wkhttp.SuperAdmin)).Load(&user)
	require.NoError(t, err)

	clientIP := fmt.Sprintf("198.51.100.%d", atomic.AddUint32(&managerMFAIntegrationIP, 1)%240+10)
	request := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return serveManagerMFARequest(t, route, managerMFARequestFromIP(t, clientIP, method, path, body))
	}

	login := request(http.MethodPost, "/v1/manager/login", map[string]string{
		"username": user.Username,
		"password": password,
	})
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var challenge managerLoginResp
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &challenge))

	// These rejected requests consume only the MFA bucket. With the old shared
	// bucket, the subsequent real /send would already be rejected by IP rate
	// limiting after the initial /login plus five MFA requests.
	for i := 0; i < managerLoginRateLimitBurst; i++ {
		invalid := request(http.MethodPost, "/v1/manager/login/send", map[string]string{
			"challenge_id": "invalid-" + fmt.Sprint(i),
		})
		require.NotEqual(t, http.StatusTooManyRequests, invalid.Code,
			"MFA request %d unexpectedly hit the IP limiter: %s", i+1, invalid.Body.String())
	}

	send := request(http.MethodPost, "/v1/manager/login/send", map[string]string{
		"challenge_id": challenge.ChallengeID,
	})
	require.NotEqual(t, http.StatusTooManyRequests, send.Code, send.Body.String())
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())

	code, err := ctx.GetRedisConn().GetString(commonbase.EmailCodeKey(recipientEmail, commonbase.CodeTypeManagerLogin))
	require.NoError(t, err)
	verify := request(http.MethodPost, "/v1/manager/login/verify", map[string]string{
		"challenge_id": challenge.ChallengeID,
		"code":         code,
	})
	require.NotEqual(t, http.StatusTooManyRequests, verify.Code, verify.Body.String())
	require.Equal(t, http.StatusOK, verify.Code, verify.Body.String())
}

func TestManagerConsoleMFAHandlerSMTPFailureCannotVerify(t *testing.T) {
	route, ctx, _, password, recipientEmail := newManagerMFAIntegration(t, true)
	var user Model
	_, err := ctx.DB().Select("uid,username").From("user").Where("role=?", string(wkhttp.SuperAdmin)).Load(&user)
	require.NoError(t, err)

	login := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login", map[string]string{
		"username": user.Username,
		"password": password,
	}))
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var challengeResp managerLoginResp
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &challengeResp))

	send := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/send", map[string]string{
		"challenge_id": challengeResp.ChallengeID,
	}))
	assert.Equal(t, http.StatusServiceUnavailable, send.Code, send.Body.String())
	code, err := ctx.GetRedisConn().GetString(commonbase.EmailCodeKey(recipientEmail, commonbase.CodeTypeManagerLogin))
	require.NoError(t, err)
	assert.Empty(t, code, "SMTP failure must not leave a verifiable OTP")
}

func TestManagerConsoleMFAVerificationLockHasDistinct429Contract(t *testing.T) {
	route, ctx, _, password, recipientEmail := newManagerMFAIntegration(t, false)
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	var user Model
	_, err := ctx.DB().Select("uid,username").From("user").Where("role=?", string(wkhttp.SuperAdmin)).Load(&user)
	require.NoError(t, err)

	login := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login", map[string]string{
		"username": user.Username,
		"password": password,
	}))
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var challenge managerLoginResp
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &challenge))

	send := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/send", map[string]string{
		"challenge_id": challenge.ChallengeID,
	}))
	require.Equal(t, http.StatusOK, send.Code, send.Body.String())
	code, err := ctx.GetRedisConn().GetString(commonbase.EmailCodeKey(recipientEmail, commonbase.CodeTypeManagerLogin))
	require.NoError(t, err)
	wrongCode := "000000"
	if wrongCode == code {
		wrongCode = "111111"
	}

	for attempt := 1; attempt <= 2; attempt++ {
		invalid := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/verify", map[string]string{
			"challenge_id": challenge.ChallengeID,
			"code":         wrongCode,
		}))
		require.Equal(t, http.StatusBadRequest, invalid.Code, invalid.Body.String())
		assert.Contains(t, invalid.Body.String(), "err.server.user.manager_mfa_code_invalid")
	}

	locked := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/verify", map[string]string{
		"challenge_id": challenge.ChallengeID,
		"code":         wrongCode,
	}))
	require.Equal(t, http.StatusTooManyRequests, locked.Code, locked.Body.String())
	assert.Contains(t, locked.Body.String(), "err.server.user.manager_mfa_verification_locked")
	assert.Contains(t, locked.Body.String(), "retry_after")
	assert.Contains(t, locked.Body.String(), "错误次数过多")
	assert.NotContains(t, locked.Body.String(), "发送过于频繁")
}

func TestManagerConsoleMFAMailboxCooldownReturns429WithRetryAfter(t *testing.T) {
	route, ctx, _, password, recipientEmail := newManagerMFAIntegration(t, false)
	route.SetErrorRenderer(i18n.NewErrorRenderer(i18n.NewLocalizer(i18n.DefaultLanguage)))
	var user Model
	_, err := ctx.DB().Select("uid,username").From("user").Where("role=?", string(wkhttp.SuperAdmin)).Load(&user)
	require.NoError(t, err)

	login := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login", map[string]string{
		"username": user.Username,
		"password": password,
	}))
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var firstChallenge managerLoginResp
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &firstChallenge))
	firstSend := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/send", map[string]string{
		"challenge_id": firstChallenge.ChallengeID,
	}))
	require.Equal(t, http.StatusOK, firstSend.Code, firstSend.Body.String())

	// A new login invalidates the previous OTP but deliberately leaves the
	// mailbox-level cooldown in place. The second send must expose that
	// cooldown as a client-actionable manager MFA 429, not SMTP 503.
	secondLogin := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login", map[string]string{
		"username": user.Username,
		"password": password,
	}))
	require.Equal(t, http.StatusOK, secondLogin.Code, secondLogin.Body.String())
	var secondChallenge managerLoginResp
	require.NoError(t, json.Unmarshal(secondLogin.Body.Bytes(), &secondChallenge))
	secondSend := serveManagerMFARequest(t, route, managerMFARequest(t, http.MethodPost, "/v1/manager/login/send", map[string]string{
		"challenge_id": secondChallenge.ChallengeID,
	}))
	require.Equal(t, http.StatusTooManyRequests, secondSend.Code, secondSend.Body.String())
	assert.Contains(t, secondSend.Body.String(), "err.server.user.manager_mfa_rate_limited")
	assert.Contains(t, secondSend.Body.String(), "retry_after")

	// Keep this test isolated if the shared Redis database is reused before the
	// one-minute mailbox key naturally expires.
	t.Cleanup(func() {
		_ = ctx.GetRedisConn().Del(commonbase.EmailRateLimitKey(recipientEmail, commonbase.CodeTypeManagerLogin))
	})
}

func TestManagerConsoleMFAOffKeepsDirectTokenPath(t *testing.T) {
	server, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		_ = settings.Reload()
	})
	require.NoError(t, settings.Reload())

	password := "manager-mfa-off-password"
	hash, err := HashPassword(password)
	require.NoError(t, err)
	uid := "mfaoff-" + util.GenerUUID()[:20]
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: uid, Username: uid, Name: "MFA Off Manager", Password: hash,
		Role: string(wkhttp.Admin), Email: "mfa-off@example.com", Status: StatusEnable.Int(),
	}))
	var user Model
	_, err = ctx.DB().Select("uid,username").From("user").Where("uid=?", uid).Load(&user)
	require.NoError(t, err)

	login := serveManagerMFARequest(t, server.GetRoute(), managerMFARequest(t, http.MethodPost, "/v1/manager/login", map[string]string{
		"username": user.Username,
		"password": password,
	}))
	require.Equal(t, http.StatusOK, login.Code, login.Body.String())
	var response managerLoginResp
	require.NoError(t, json.Unmarshal(login.Body.Bytes(), &response))
	assert.NotEmpty(t, response.Token)
	assert.False(t, response.MFARequired)
	assert.Equal(t, "mxxxxf@example.com", response.Email)
}

func TestManagerAdminEmailMaintenanceRequiresValidEmailAndInvalidatesChallenge(t *testing.T) {
	server, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		_ = settings.Reload()
	})
	callerToken := "manager-email-maintainer"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"maintainer@root@"+string(wkhttp.SuperAdmin),
	))
	targetUID := "email-target"
	oldEmail := "old-admin@example.com"
	newEmail := "new-admin@example.com"
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: targetUID, Username: "email-target", Name: "Email Target",
		Role: string(wkhttp.Admin), Email: oldEmail, Status: StatusEnable.Int(),
	}))
	require.NoError(t, ctx.Cache().Set(managerMFAActiveKey(targetUID), "old-challenge"))
	require.NoError(t, ctx.Cache().Set(managerMFAChallengeKey("old-challenge"), "old"))
	require.NoError(t, ctx.Cache().Set(commonbase.EmailCodeKey(oldEmail, commonbase.CodeTypeManagerLogin), "123456"))

	req := managerMFARequest(t, http.MethodPut, "/v1/manager/user/admin/email", map[string]string{
		"uid": targetUID, "email": newEmail,
	})
	req.Header.Set("token", callerToken)
	response := serveManagerMFARequest(t, server.GetRoute(), req)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var target Model
	_, err := ctx.DB().Select("uid,email").From("user").Where("uid=?", targetUID).Load(&target)
	require.NoError(t, err)
	assert.Equal(t, newEmail, target.Email)
	active, err := ctx.GetRedisConn().GetString(managerMFAActiveKey(targetUID))
	require.NoError(t, err)
	assert.Empty(t, active)
	code, err := ctx.GetRedisConn().GetString(commonbase.EmailCodeKey(oldEmail, commonbase.CodeTypeManagerLogin))
	require.NoError(t, err)
	assert.Empty(t, code)

	// Re-submitting the already-stored address is an idempotent maintenance
	// operation. MySQL reports zero changed rows for this no-op update, but the
	// endpoint must not turn a valid, authorized request into an internal error.
	repeatReq := managerMFARequest(t, http.MethodPut, "/v1/manager/user/admin/email", map[string]string{
		"uid": targetUID, "email": newEmail,
	})
	repeatReq.Header.Set("token", callerToken)
	repeatResponse := serveManagerMFARequest(t, server.GetRoute(), repeatReq)
	require.Equal(t, http.StatusOK, repeatResponse.Code, repeatResponse.Body.String())
}

func TestManagerAdminEmailMaintenanceRejectsEmailUsedByAnotherUser(t *testing.T) {
	server, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(server)
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		_ = settings.Reload()
	})

	callerToken := "manager-email-conflict-maintainer"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"maintainer@root@"+string(wkhttp.SuperAdmin),
	))
	targetUID := "email-conflict-target"
	conflictEmail := "existing-user@example.com"
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: targetUID, Username: "email-conflict-target", Name: "Email Conflict Target",
		ShortNo: "email-conflict-target-short", Role: string(wkhttp.Admin),
		Email: "old-admin@example.com", Status: StatusEnable.Int(),
	}))
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: "email-conflict-user", Username: "email-conflict-user", Name: "Existing User",
		ShortNo: "email-conflict-user-short", Role: "user", Email: conflictEmail,
		Status: StatusEnable.Int(),
	}))

	req := managerMFARequest(t, http.MethodPut, "/v1/manager/user/admin/email", map[string]string{
		"uid": targetUID, "email": conflictEmail,
	})
	req.Header.Set("token", callerToken)
	response := serveManagerMFARequest(t, server.GetRoute(), req)
	assert.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "err.server.user.already_exists")

	var target Model
	_, err := ctx.DB().Select("uid,email").From("user").Where("uid=?", targetUID).Load(&target)
	require.NoError(t, err)
	assert.Equal(t, "old-admin@example.com", target.Email)
}

func TestManagerAddAdminRejectsMissingEmail(t *testing.T) {
	server, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		_ = settings.Reload()
	})
	callerToken := "manager-admin-creator"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"creator@root@"+string(wkhttp.SuperAdmin),
	))
	loginName := "missing-email-admin"
	req := managerMFARequest(t, http.MethodPost, "/v1/manager/user/admin", map[string]string{
		"login_name": loginName,
		"name":       "Missing Email Admin",
		"password":   "Strong-admin-password-123!",
	})
	req.Header.Set("token", callerToken)
	response := serveManagerMFARequest(t, server.GetRoute(), req)
	assert.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
	var count int64
	_, err := ctx.DB().Select("count(*)").From("user").Where("username=?", loginName).Load(&count)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestManagerAddAdminRejectsEmailUsedByAnotherUser(t *testing.T) {
	server, ctx := testutil.NewTestServer()
	wireI18nRendererForUserTest(server)
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := commonsettings.EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		_ = settings.Reload()
	})

	callerToken := "manager-admin-email-conflict-creator"
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+callerToken,
		"creator@root@"+string(wkhttp.SuperAdmin),
	))
	conflictEmail := "existing-user-for-admin@example.com"
	require.NoError(t, NewDB(ctx).Insert(&Model{
		UID: "email-conflict-admin-user", Username: "email-conflict-admin-user", Name: "Existing User",
		ShortNo: "email-conflict-admin-user-short", Role: "user", Email: conflictEmail,
		Status: StatusEnable.Int(),
	}))

	loginName := "email-conflict-admin"
	req := managerMFARequest(t, http.MethodPost, "/v1/manager/user/admin", map[string]string{
		"login_name": loginName,
		"name":       "Email Conflict Admin",
		"password":   "Strong-admin-password-123!",
		"email":      conflictEmail,
	})
	req.Header.Set("token", callerToken)
	response := serveManagerMFARequest(t, server.GetRoute(), req)
	assert.NotEqual(t, http.StatusOK, response.Code, response.Body.String())
	assert.Contains(t, response.Body.String(), "err.server.user.already_exists")

	var count int64
	_, err := ctx.DB().Select("count(*)").From("user").Where("username=?", loginName).Load(&count)
	require.NoError(t, err)
	assert.Zero(t, count)
}
