package common

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildTransactionalMessage_StructureAndHeaders 验证 SendTransactionalHTML
// 内部拼出来的报文符合 RFC 2046 multipart/alternative 结构,且带齐"事务邮件"
// 反垃圾过滤期望看到的 header。
//
// 这条 case 是这次 follow-up PR 的核心 —— 经验证收件方反垃圾对极简 HTML 单一
// part 邮件会静默丢弃,只要 multipart + 标准 header 齐全就能正常入箱。任何
// 未来调整 buildTransactionalMessage 时,本测试就是 contract guard。
func TestBuildTransactionalMessage_StructureAndHeaders(t *testing.T) {
	msg, err := buildTransactionalMessage(
		"contact@xming.ai",
		"guobin.a@mininglamp.com",
		"[Octo] SMTP 自检",
		"<p>hello</p>",
		"hello",
	)
	require.NoError(t, err)
	s := string(msg)

	// ---- header block ----
	assert.Contains(t, s, "From: contact@xming.ai\r\n")
	assert.Contains(t, s, "To: guobin.a@mininglamp.com\r\n")
	assert.Contains(t, s, "Subject: [Octo] SMTP 自检\r\n")
	assert.Regexp(t, regexp.MustCompile(`(?m)^Date: .+\r$`), s, "Date header must be present")
	assert.Regexp(t, regexp.MustCompile(`(?m)^Message-ID: <[0-9a-f]+@xming\.ai>\r$`), s,
		"Message-ID domain should follow From's domain so SPF/DKIM alignment is sane")
	assert.Contains(t, s, "MIME-Version: 1.0\r\n")
	assert.Contains(t, s, `Content-Type: multipart/alternative; boundary="octo_`,
		"Top-level Content-Type must declare multipart/alternative with a deterministic boundary prefix")
	assert.Contains(t, s, "List-Unsubscribe: <mailto:contact@xming.ai?subject=unsubscribe>\r\n",
		"List-Unsubscribe (RFC 2369) is one of the strongest transactional-email signals for Gmail/Outlook")
	assert.Contains(t, s, "Auto-Submitted: auto-generated\r\n",
		"Auto-Submitted advertises this as a machine-generated message; suppresses OOO replies without suppressing DSN")
	assert.Contains(t, s, "X-Mailer: Octo Transactional Mailer\r\n")

	// 这两条 header 故意 *不* 发(详见 buildTransactionalMessage 中的注释):
	//   - "List-Unsubscribe-Post: List-Unsubscribe=One-Click" 跟 mailto 配错 (RFC 8058 misuse)
	//   - "Precedence: bulk" 可能在某些 MTA 上抑制退信,跟 /test_email 诊断意图相反
	// 这里做 negative assertion 防止有人后续 hardening 时悄悄加回来。
	assert.NotContains(t, s, "List-Unsubscribe-Post:",
		"One-Click without an HTTPS POST endpoint is RFC 8058 misuse")
	assert.NotContains(t, s, "Precedence: bulk",
		"Precedence: bulk can suppress DSN, defeating the /test_email diagnostic purpose")

	// ---- multipart body ----
	plainPos := strings.Index(s, "Content-Type: text/plain")
	htmlPos := strings.Index(s, "Content-Type: text/html")
	require.Positive(t, plainPos, "plaintext part must exist")
	require.Positive(t, htmlPos, "html part must exist")
	assert.Less(t, plainPos, htmlPos,
		"RFC 2046: alternative parts go simplest-first; plaintext must precede html "+
			"so naive clients render plaintext fallback instead of raw markup")

	// 报文必须以 boundary close 收尾,缺了就成 broken multipart,部分严格邮件
	// 网关会拒收。
	assert.True(t, strings.HasSuffix(s, "--\r\n"), "envelope must end with a closing boundary line")
}

// TestBuildTransactionalMessage_FromMissingAtSign 验证 Message-ID 的 domain
// fallback 路径 —— from 没有 @ 时不能 panic,也不能产生空 domain。
func TestBuildTransactionalMessage_FromMissingAtSign(t *testing.T) {
	msg, err := buildTransactionalMessage(
		"contact", // 没有 @
		"u@example.com",
		"subj",
		"<p>h</p>",
		"h",
	)
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`(?m)^Message-ID: <[0-9a-f]+@octo\.local>\r$`), string(msg),
		"degraded From should fall back to a safe domain stub, never produce <id@>")
}

// TestSanitizeHeader_StripsCRLF 验证 CRLF 消毒。攻击者控制的 to / subject
// 不能注入额外 header(比如 \r\nBcc: hacker@evil.com)。
func TestSanitizeHeader_StripsCRLF(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "guobin.a@example.com", "guobin.a@example.com"},
		{"lf injection", "x@e.com\nBcc: h@evil.com", "x@e.comBcc: h@evil.com"},
		{"crlf injection", "x@e.com\r\nBcc: h@evil.com", "x@e.comBcc: h@evil.com"},
		{"cr only", "x@e.com\rsubject hijack", "x@e.comsubject hijack"},
		{"mixed", "a\r\nb\rc\nd", "abcd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeHeader(tt.in))
		})
	}
}
