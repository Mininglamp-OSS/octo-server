package common

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// managerMFAProbeSMTP is a deliberately small SMTP sink for the manager
// settings handler. It speaks the transaction used by PreflightSMTP and keeps
// the commands visible so the test can prove the blocking probe was reached.
type managerMFAProbeSMTP struct {
	listener net.Listener

	mu       sync.Mutex
	commands []string
}

func newManagerMFAProbeSMTP(t *testing.T) *managerMFAProbeSMTP {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &managerMFAProbeSMTP{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go server.serve()
	return server
}

func (s *managerMFAProbeSMTP) address() string {
	return s.listener.Addr().String()
}

func (s *managerMFAProbeSMTP) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *managerMFAProbeSMTP) record(command string) {
	s.mu.Lock()
	s.commands = append(s.commands, command)
	s.mu.Unlock()
}

func (s *managerMFAProbeSMTP) hasCommandPrefix(prefix string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, command := range s.commands {
		if strings.HasPrefix(command, prefix) {
			return true
		}
	}
	return false
}

func (s *managerMFAProbeSMTP) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	write := func(response string) error {
		_, err := fmt.Fprint(conn, response)
		return err
	}
	if err := write("220 manager-mfa-probe.test ESMTP ready\r\n"); err != nil {
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
			if err := write("250-manager-mfa-probe.test\r\n250-AUTH PLAIN\r\n250 OK\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(command, "AUTH"):
			if err := write("235 2.7.0 authenticated\r\n"); err != nil {
				return
			}
		case strings.HasPrefix(command, "MAIL FROM"), strings.HasPrefix(command, "RCPT TO"):
			if err := write("250 2.0.0 accepted\r\n"); err != nil {
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
			if err := write("250 2.0.0 accepted\r\n"); err != nil {
				return
			}
		}
	}
}

func newManagerSystemSettingTest(t *testing.T) (*wkhttp.WKHttp, *SystemSettings) {
	t.Helper()
	server, ctx := testutil.NewTestServer()
	require.NoError(t, testutil.CleanAllTables(ctx))
	settings := EnsureSystemSettings(ctx)
	t.Cleanup(func() {
		_ = testutil.CleanAllTables(ctx)
		_ = settings.Reload()
	})
	require.NoError(t, ctx.Cache().Set(
		ctx.GetConfig().Cache.TokenCachePrefix+testutil.Token,
		testutil.UID+"@test@"+string(wkhttp.SuperAdmin),
	))
	return server.GetRoute(), settings
}

func TestManagerSystemSettingChangeRunsRealSMTPPreflight(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	route, settings := newManagerSystemSettingTest(t)

	password, err := encryptKey("smtp-password")
	require.NoError(t, err)
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "0", settingTypeBool, ""))
	require.NoError(t, settings.db.upsert("support", "email", "mfa-sender@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", "127.0.0.1:1", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_pwd", password, settingTypeEncrypted, ""))
	require.NoError(t, settings.Load())

	probe := newManagerMFAProbeSMTP(t)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting", bytes.NewBufferString(
		`{"items":[{"category":"support","key":"email_smtp","value":"`+probe.address()+`"}]}`,
	))
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.True(t, probe.hasCommandPrefix("AUTH"), "the settings write must run SMTP AUTH")
	assert.True(t, probe.hasCommandPrefix("MAIL FROM"), "the settings write must run the SMTP transaction")
	assert.Equal(t, probe.address(), settings.ManagerEmailMFASMTPSettings().SupportEmailSmtp())
}

func TestManagerSystemSetting_AllowsClearingSMTPWhenManagerMFAIsOff(t *testing.T) {
	t.Setenv(masterKeyEnv, "0123456789abcdef0123456789abcdef")
	route, settings := newManagerSystemSettingTest(t)
	password, err := encryptKey("smtp-password")
	require.NoError(t, err)
	require.NoError(t, settings.db.upsert("login", "manager_email_mfa_on", "0", settingTypeBool, ""))
	require.NoError(t, settings.db.upsert("support", "email", "mfa-sender@example.com", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_smtp", "smtp.example.com:587", settingTypeString, ""))
	require.NoError(t, settings.db.upsert("support", "email_pwd", password, settingTypeEncrypted, ""))
	require.NoError(t, settings.Load())

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/manager/common/system_setting", bytes.NewBufferString(
		`{"items":[{"category":"support","key":"email","value":""}]}`,
	))
	req.Header.Set("token", testutil.Token)
	route.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Empty(t, settings.ManagerEmailMFASMTPSettings().SupportEmail())
	assert.Equal(t, "smtp.example.com:587", settings.ManagerEmailMFASMTPSettings().SupportEmailSmtp(),
		"clearing the sender while MFA is off must not clear unrelated SMTP fields")
}
