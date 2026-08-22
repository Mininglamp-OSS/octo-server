package common

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
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

func newTrackedEmailService(t *testing.T, smtpAddr string) (*EmailService, *config.Context) {
	t.Helper()
	cfg := config.New()
	cfg.Test = true
	cfg.Support.Email = "mfa-sender@example.com"
	cfg.Support.EmailPwd = "smtp-password"
	cfg.Support.EmailSmtp = smtpAddr
	ctx := config.NewContext(cfg)
	service := NewEmailService(ctx, nil)
	t.Cleanup(func() {
		for _, key := range []string{
			EmailCodeKey("mfa-recipient@example.com", CodeTypeManagerLogin),
			EmailCodeStatusKey("mfa-recipient@example.com", CodeTypeManagerLogin),
			EmailRateLimitKey("mfa-recipient@example.com", CodeTypeManagerLogin),
			EmailCodeKey("ordinary@example.com", CodeTypeEmailLogin),
			EmailCodeStatusKey("ordinary@example.com", CodeTypeEmailLogin),
		} {
			_ = ctx.GetRedisConn().Del(key)
		}
	})
	return service, ctx
}

func TestTrackedManagerCodeRequiresSMTPAndSentStatus(t *testing.T) {
	server := newTrackedSMTPServer(t, false)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "mfa-recipient@example.com"
	codeKey := EmailCodeKey(email, CodeTypeManagerLogin)
	statusKey := EmailCodeStatusKey(email, CodeTypeManagerLogin)

	require.NoError(t, service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, CodeTypeManagerLogin, "zh-CN", "attempt-1",
	))
	code, err := ctx.GetRedisConn().GetString(codeKey)
	require.NoError(t, err)
	status, err := ctx.GetRedisConn().GetString(statusKey)
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9]{6}$`, code)
	assert.Equal(t, "sent:attempt-1", status)

	// A manager code with a missing status is not accepted even if the code key
	// itself is present. This is the delivery-confirmation boundary.
	require.NoError(t, ctx.GetRedisConn().SetAndExpire(codeKey, code, emailCodeTTL))
	require.NoError(t, ctx.GetRedisConn().Del(statusKey))
	err = service.Verify(context.Background(), email, code, CodeTypeManagerLogin)
	assert.Error(t, err)
}

func TestTrackedManagerCodeRemovesCodeWhenSMTPFails(t *testing.T) {
	server := newTrackedSMTPServer(t, true)
	service, ctx := newTrackedEmailService(t, server.address())
	email := "mfa-recipient@example.com"

	err := service.SendVerifyCodeTrackedWithAttempt(
		context.Background(), email, CodeTypeManagerLogin, "zh-CN", "attempt-failed",
	)
	assert.Error(t, err)
	code, getErr := ctx.GetRedisConn().GetString(EmailCodeKey(email, CodeTypeManagerLogin))
	require.NoError(t, getErr)
	status, getErr := ctx.GetRedisConn().GetString(EmailCodeStatusKey(email, CodeTypeManagerLogin))
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
		EmailCodeKey(email, CodeTypeEmailLogin), code, emailCodeTTL,
	))
	require.NoError(t, ctx.GetRedisConn().Del(EmailCodeStatusKey(email, CodeTypeEmailLogin)))
	assert.NoError(t, service.Verify(context.Background(), email, code, CodeTypeEmailLogin))
}
