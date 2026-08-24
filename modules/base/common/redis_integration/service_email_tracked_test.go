package common_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	common "github.com/Mininglamp-OSS/octo-server/modules/base/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// trackedSMTPServer is deliberately small: it implements only the SMTP
// conversation used by EmailService. The test therefore exercises the real
// dial/auth/MAIL/RCPT/DATA/QUIT path without depending on an external MTA.
type trackedSMTPServer struct {
	listener net.Listener
	failAuth bool

	mu       sync.Mutex
	messages [][]byte
}

func newTrackedSMTPServer(t *testing.T, failAuth bool) *trackedSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &trackedSMTPServer{listener: listener, failAuth: failAuth}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func (s *trackedSMTPServer) address() string { return s.listener.Addr().String() }

func (s *trackedSMTPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *trackedSMTPServer) handle(conn net.Conn) {
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

func newTrackedEmailService(t *testing.T, smtpAddr string) (*common.EmailService, *config.Context) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.Support.Email = "mfa-sender@example.com"
	cfg.Support.EmailPwd = "smtp-password"
	cfg.Support.EmailSmtp = smtpAddr
	ctx := config.NewContext(cfg)
	if _, err := ctx.GetRedisConn().Ping(); err != nil {
		t.Skipf("Redis unavailable: %v", err)
	}
	service := common.NewEmailService(ctx, nil)
	t.Cleanup(func() {
		for _, key := range []string{
			common.EmailCodeKey("mfa-recipient@example.com", common.CodeTypeManagerLogin),
			common.EmailCodeStatusKey("mfa-recipient@example.com", common.CodeTypeManagerLogin),
			common.EmailRateLimitKey("mfa-recipient@example.com", common.CodeTypeManagerLogin),
			common.EmailCodeKey("ordinary@example.com", common.CodeTypeEmailLogin),
			common.EmailCodeStatusKey("ordinary@example.com", common.CodeTypeEmailLogin),
		} {
			_ = ctx.GetRedisConn().Del(key)
		}
	})
	return service, ctx
}

const trackedEmailCodeTTL = 5 * time.Minute

func prepareTrackedAttempt(t *testing.T, ctx *config.Context, attemptID string) string {
	t.Helper()
	key := "manager:mfa:test:send-state:" + attemptID
	require.NoError(t, ctx.GetRedisConn().Hset(key, "attempt_id", attemptID))
	require.NoError(t, ctx.GetRedisConn().Hset(key, "status", "pending"))
	require.NoError(t, ctx.GetRedisConn().Expire(key, trackedEmailCodeTTL))
	t.Cleanup(func() { _ = ctx.GetRedisConn().Del(key) })
	return key
}

func TestTrackedManagerCodeRequiresSMTPAndSentStatus(t *testing.T) {
	server := newTrackedSMTPServer(t, false)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "mfa-recipient@example.com"
	sendStateKey := prepareTrackedAttempt(t, ctx, "attempt-1")
	codeKey := common.EmailCodeKey(email, common.CodeTypeManagerLogin)
	statusKey := common.EmailCodeStatusKey(email, common.CodeTypeManagerLogin)

	require.NoError(t, service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, common.CodeTypeManagerLogin, "zh-CN", "attempt-1", sendStateKey,
	))
	code, err := ctx.GetRedisConn().GetString(codeKey)
	require.NoError(t, err)
	status, err := ctx.GetRedisConn().GetString(statusKey)
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9]{6}$`, code)
	assert.Equal(t, "sent:attempt-1", status)

	// A manager code with a missing status is not accepted even if the code key
	// itself is present. This is the delivery-confirmation boundary.
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(codeKey, code, trackedEmailCodeTTL))
	require.NoError(t, ctx.GetRedisConn().Del(statusKey))
	err = service.Verify(context.Background(), email, code, common.CodeTypeManagerLogin)
	assert.Error(t, err)
}

func TestTrackedManagerCodeRemovesCodeWhenSMTPFails(t *testing.T) {
	server := newTrackedSMTPServer(t, true)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "mfa-recipient@example.com"
	sendStateKey := prepareTrackedAttempt(t, ctx, "attempt-failed")

	err := service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, common.CodeTypeManagerLogin, "zh-CN", "attempt-failed", sendStateKey,
	)
	assert.Error(t, err)
	code, getErr := ctx.GetRedisConn().GetString(common.EmailCodeKey(email, common.CodeTypeManagerLogin))
	require.NoError(t, getErr)
	status, getErr := ctx.GetRedisConn().GetString(common.EmailCodeStatusKey(email, common.CodeTypeManagerLogin))
	require.NoError(t, getErr)
	assert.Empty(t, code)
	assert.Empty(t, status)
}

func TestTrackedManagerCodeCannotOverwriteNewerAttempt(t *testing.T) {
	server := newTrackedSMTPServer(t, false)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "mfa-recipient@example.com"
	sendStateKey := prepareTrackedAttempt(t, ctx, "attempt-a")
	codeKey := common.EmailCodeKey(email, common.CodeTypeManagerLogin)
	statusKey := common.EmailCodeStatusKey(email, common.CodeTypeManagerLogin)

	// Attempt B has already taken ownership of the mailbox. Attempt A must
	// fail before SMTP and must not overwrite B's delivered code or status.
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(codeKey, "654321", trackedEmailCodeTTL))
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(statusKey, "sent:attempt-b", trackedEmailCodeTTL))

	err := service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, common.CodeTypeManagerLogin, "zh-CN", "attempt-a", sendStateKey,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "superseded")

	code, getErr := ctx.GetRedisConn().GetString(codeKey)
	require.NoError(t, getErr)
	assert.Equal(t, "654321", code)
	status, getErr := ctx.GetRedisConn().GetString(statusKey)
	require.NoError(t, getErr)
	assert.Equal(t, "sent:attempt-b", status)

	server.mu.Lock()
	assert.Empty(t, server.messages, "a superseded attempt must not send SMTP")
	server.mu.Unlock()
}

func TestTrackedManagerCodeRejectsStaleAttemptAndAllowsNewerAttempt(t *testing.T) {
	server := newTrackedSMTPServer(t, false)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "mfa-recipient@example.com"
	oldStateKey := "manager:mfa:test:send-state:stale-attempt"
	newStateKey := prepareTrackedAttempt(t, ctx, "current-attempt")

	// Challenge replacement removes the old send-state and the shared code
	// pair before the new attempt claims its own send-state. The stale sender
	// must fail ownership validation, while the current sender must still be
	// able to initialize and deliver its code.
	require.NoError(t, ctx.GetRedisConn().Del(oldStateKey))
	err := service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, common.CodeTypeManagerLogin, "zh-CN", "stale-attempt", oldStateKey,
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "superseded")

	require.NoError(t, service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, common.CodeTypeManagerLogin, "zh-CN", "current-attempt", newStateKey,
	))
	status, getErr := ctx.GetRedisConn().GetString(common.EmailCodeStatusKey(email, common.CodeTypeManagerLogin))
	require.NoError(t, getErr)
	assert.Equal(t, "sent:current-attempt", status)

	server.mu.Lock()
	assert.Len(t, server.messages, 1, "only the current attempt may send SMTP")
	server.mu.Unlock()
}

func TestTrackedCodeWithoutAttemptStillReplacesExistingCode(t *testing.T) {
	server := newTrackedSMTPServer(t, false)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "mfa-recipient@example.com"
	codeKey := common.EmailCodeKey(email, common.CodeTypeManagerLogin)
	statusKey := common.EmailCodeStatusKey(email, common.CodeTypeManagerLogin)
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(codeKey, "654321", trackedEmailCodeTTL))
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(statusKey, "sent:previous", trackedEmailCodeTTL))

	require.NoError(t, service.SendVerifyCodeTracked(
		context.Background(), email, common.CodeTypeManagerLogin, "zh-CN",
	))
	status, getErr := ctx.GetRedisConn().GetString(statusKey)
	require.NoError(t, getErr)
	assert.Equal(t, "sent", status)
}

func TestOrdinaryEmailCodeMissingStatusRetainsCompatibility(t *testing.T) {
	server := newTrackedSMTPServer(t, false)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "ordinary@example.com"
	code := "123456"
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(
		common.EmailCodeKey(email, common.CodeTypeEmailLogin), code, trackedEmailCodeTTL,
	))
	require.NoError(t, ctx.GetRedisConn().Del(common.EmailCodeStatusKey(email, common.CodeTypeEmailLogin)))
	assert.NoError(t, service.Verify(context.Background(), email, code, common.CodeTypeEmailLogin))
}
