package common

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// passwordlessSMTPServer implements the unauthenticated relay path used by
// SendTransactionalHTML. It intentionally does not advertise AUTH; a client
// that tries smtp.Client.Auth would fail before MAIL FROM. The positive path
// advertises STARTTLS so passwordless does not imply plaintext transport.
type passwordlessSMTPServer struct {
	listener          net.Listener
	advertiseStartTLS bool
	tlsConfig         *tls.Config

	mu       sync.Mutex
	commands []string
}

func newPasswordlessSMTPServer(t *testing.T) *passwordlessSMTPServer {
	return newPasswordlessSMTPServerWithStartTLS(t, true)
}

func newPasswordlessSMTPServerWithStartTLS(t *testing.T, advertiseStartTLS bool) *passwordlessSMTPServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	server := &passwordlessSMTPServer{
		listener:          listener,
		advertiseStartTLS: advertiseStartTLS,
	}
	if advertiseStartTLS {
		server.tlsConfig = newSMTPTestTLSConfig(t)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func newSMTPTestTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	require.NoError(t, err)
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
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
	defer func() { _ = conn.Close() }()
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
			response := "250-relay.test\r\n"
			if s.advertiseStartTLS {
				response += "250-STARTTLS\r\n"
			}
			response += "250 OK\r\n"
			if err := write(response); err != nil {
				return
			}
		case command == "STARTTLS" && s.advertiseStartTLS:
			if err := write("220 2.0.0 Ready to start TLS\r\n"); err != nil {
				return
			}
			tlsConn := tls.Server(conn, s.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			reader = bufio.NewReader(conn)
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
	service.tlsConfig = func(host string) *tls.Config {
		return &tls.Config{
			ServerName:         host,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // test server uses a per-test self-signed certificate
		}
	}

	err := service.SendTransactionalHTML(
		context.Background(),
		"recipient@example.com",
		"SMTP relay test",
		"<p>hello</p>",
		"hello",
	)
	require.NoError(t, err)
	assert.True(t, server.hasCommandPrefix("MAIL FROM"), "relay must receive the message without AUTH")
	assert.True(t, server.hasCommandPrefix("STARTTLS"), "passwordless relay must negotiate TLS")
	assert.False(t, server.hasCommandPrefix("AUTH"), "passwordless relay must not receive an AUTH command")
}

func TestSendTransactionalHTMLWithoutSMTPPasswordRejectsPlaintextRelay(t *testing.T) {
	server := newPasswordlessSMTPServerWithStartTLS(t, false)
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
	require.Error(t, err)
	assert.Contains(t, err.Error(), "STARTTLS")
	assert.False(t, server.hasCommandPrefix("MAIL FROM"), "plaintext relay must not receive the message")
}
