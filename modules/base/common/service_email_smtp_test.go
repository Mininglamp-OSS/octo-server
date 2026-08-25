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

// passwordlessSMTPServer implements the unauthenticated relay path used by
// SendTransactionalHTML. It intentionally does not advertise AUTH; a client
// that tries smtp.Client.Auth would fail before MAIL FROM.
type passwordlessSMTPServer struct {
	listener net.Listener

	mu       sync.Mutex
	commands []string
}

func newPasswordlessSMTPServer(t *testing.T) *passwordlessSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &passwordlessSMTPServer{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func (s *passwordlessSMTPServer) address() string {
	return s.listener.Addr().String()
}

func (s *passwordlessSMTPServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *passwordlessSMTPServer) record(command string) {
	s.mu.Lock()
	s.commands = append(s.commands, command)
	s.mu.Unlock()
}

func (s *passwordlessSMTPServer) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	write := func(response string) error {
		_, err := fmt.Fprint(conn, response)
		return err
	}
	if err := write("220 relay.test ESMTP ready\r\n"); err != nil {
		return
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		command := strings.ToUpper(strings.TrimSpace(line))
		s.record(command)
		switch {
		case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
			if err := write("250-relay.test\r\n250 OK\r\n"); err != nil {
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
			for {
				dataLine, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				if dataLine == ".\r\n" || dataLine == ".\n" {
					break
				}
			}
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

func (s *passwordlessSMTPServer) hasCommandPrefix(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, command := range s.commands {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	return false
}

func TestSendTransactionalHTMLWithoutSMTPPasswordUsesRelay(t *testing.T) {
	server := newPasswordlessSMTPServer(t)
	cfg := config.New()
	cfg.Support.Email = "relay@example.com"
	cfg.Support.EmailSmtp = server.address()
	cfg.Support.EmailPwd = ""
	ctx := config.NewContext(cfg)
	service := NewEmailService(ctx, nil)

	err := service.SendTransactionalHTML(
		context.Background(),
		"recipient@example.com",
		"SMTP relay test",
		"<p>hello</p>",
		"hello",
	)
	require.NoError(t, err)
	assert.True(t, server.hasCommandPrefix("MAIL FROM"), "relay must receive the message without AUTH")
	assert.False(t, server.hasCommandPrefix("AUTH"), "passwordless relay must not receive an AUTH command")
}
