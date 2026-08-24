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

func TestTrackedManagerCodeRequiresSMTPAndSentStatus(t *testing.T) {
	server := newTrackedSMTPServer(t, false)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "mfa-recipient@example.com"
	codeKey := common.EmailCodeKey(email, common.CodeTypeManagerLogin)
	statusKey := common.EmailCodeStatusKey(email, common.CodeTypeManagerLogin)

	require.NoError(t, service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, common.CodeTypeManagerLogin, "zh-CN", "attempt-1",
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

	err := service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, common.CodeTypeManagerLogin, "zh-CN", "attempt-failed",
	)
	assert.Error(t, err)
	code, getErr := ctx.GetRedisConn().GetString(common.EmailCodeKey(email, common.CodeTypeManagerLogin))
	require.NoError(t, getErr)
	status, getErr := ctx.GetRedisConn().GetString(common.EmailCodeStatusKey(email, common.CodeTypeManagerLogin))
	require.NoError(t, getErr)
	assert.Empty(t, code)
	assert.Empty(t, status)
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
