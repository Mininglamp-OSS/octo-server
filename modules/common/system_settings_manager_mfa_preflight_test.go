package common

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newManagerMFAPreflightSMTP(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				if _, err := fmt.Fprint(conn, "220 preflight.smtp ready\r\n"); err != nil {
					return
				}
				for {
					line, readErr := reader.ReadString('\n')
					if readErr != nil {
						return
					}
					command := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(command, "EHLO"), strings.HasPrefix(command, "HELO"):
						_, _ = fmt.Fprint(conn, "250-preflight.smtp\r\n250-AUTH PLAIN\r\n250 OK\r\n")
					case strings.HasPrefix(command, "AUTH"):
						_, _ = fmt.Fprint(conn, "235 authenticated\r\n")
					case strings.HasPrefix(command, "MAIL FROM"), strings.HasPrefix(command, "RCPT TO"):
						_, _ = fmt.Fprint(conn, "250 OK\r\n")
					case command == "DATA":
						_, _ = fmt.Fprint(conn, "354 continue\r\n")
						for {
							dataLine, dataErr := reader.ReadString('\n')
							if dataErr != nil {
								return
							}
							if dataLine == ".\r\n" || dataLine == ".\n" {
								break
							}
						}
						_, _ = fmt.Fprint(conn, "250 queued\r\n")
					case command == "QUIT":
						_, _ = fmt.Fprint(conn, "221 bye\r\n")
						return
					default:
						_, _ = fmt.Fprint(conn, "250 OK\r\n")
					}
				}
			}(conn)
		}
	}()
	return listener.Addr().String()
}

func TestManagerEmailMFAPreflightUsesRealSMTPPath(t *testing.T) {
	settings := newTestSystemSettings(t, nil)
	settings.ctx.GetConfig().Support.Email = "mfa-preflight@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = newManagerMFAPreflightSMTP(t)
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))
	require.NoError(t, settings.Load())

	preflightCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, settings.PreflightManagerEmailMFA(preflightCtx))
	require.True(t, settings.ManagerEmailMFAReady())
}

func TestManagerEmailMFAReloadReprobesChangedConfiguration(t *testing.T) {
	settings := newTestSystemSettings(t, nil)
	settings.ctx.GetConfig().Support.Email = "mfa-reload@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = newManagerMFAPreflightSMTP(t)
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))

	// Reload is the compatibility path for a direct DB write. It must publish
	// the new snapshot fail-closed, then perform one real preflight for it.
	require.NoError(t, settings.Reload())
	require.Eventually(t, func() bool {
		return settings.ManagerEmailMFAReady()
	}, 3*time.Second, 10*time.Millisecond)
}

func TestManagerEmailMFAAutoReloadReprobesChangedConfiguration(t *testing.T) {
	settings := newTestSystemSettings(t, nil)
	settings.ctx.GetConfig().Support.Email = "mfa-auto-reload@example.com"
	settings.ctx.GetConfig().Support.EmailSmtp = newManagerMFAPreflightSMTP(t)
	settings.ctx.GetConfig().Support.EmailPwd = "smtp-password"

	reloadCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	settings.startAutoReload(reloadCtx, 10*time.Millisecond)
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "1", settingTypeBool, ""))

	require.Eventually(t, func() bool {
		return settings.ManagerEmailMFAReady()
	}, 3*time.Second, 10*time.Millisecond)
}
