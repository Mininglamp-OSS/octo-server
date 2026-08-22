package common

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/modules/base/common/emailtmpl"
	octoredis "github.com/Mininglamp-OSS/octo-server/pkg/redis"
	rd "github.com/go-redis/redis"
	"go.uber.org/zap"
)

const CacheKeyEmailCode = "emailcode:"

const (
	emailCodeStatusPending = "pending"
	emailCodeStatusSent    = "sent"
	emailCodeStatusFailed  = "failed"
	emailCodeTTL           = 5 * time.Minute
)

// The key helpers are shared by the manager MFA flow and the ordinary email
// login flow. CodeType is part of every control key so a public-user code
// cannot consume or lock a manager-console code for the same mailbox.
func EmailCodeKey(email string, codeType CodeType) string {
	return fmt.Sprintf("%s%d@%s", CacheKeyEmailCode, codeType, email)
}

func EmailCodeStatusKey(email string, codeType CodeType) string {
	return fmt.Sprintf("emailcode-status:%d@%s", codeType, email)
}

func EmailRateLimitKey(email string, codeType CodeType) string {
	return fmt.Sprintf("email_rate_limit:%d:%s", codeType, email)
}

func EmailVerifyFailKey(email string, codeType CodeType) string {
	return fmt.Sprintf("email_verify_fail:%d:%s", codeType, email)
}

func EmailVerifyLockKey(email string, codeType CodeType) string {
	return fmt.Sprintf("email_verify_lock:%d:%s", codeType, email)
}

func emailCodeRequiresSentStatus(codeType CodeType) bool {
	return codeType == CodeTypeManagerLogin
}

// IEmailService 邮件服务接口
type IEmailService interface {
	// 发送验证码。lang 为收件人内容语言（BCP-47），用于渲染主题与正文；
	// 调用方通常传 i18n.OutboundLanguage(ctx)，其会兜底到 OCTO_DEFAULT_LANGUAGE。
	SendVerifyCode(ctx context.Context, email string, codeType CodeType, lang string) error
	// 验证验证码(销毁缓存)
	Verify(ctx context.Context, email, code string, codeType CodeType) error
	// SendHTMLEmail 发送一封 HTML 邮件（不走频率限制 / 验证码缓存，由调用方自己控制）
	SendHTMLEmail(ctx context.Context, to, subject, htmlBody string) error
	// SendTransactionalHTML 发送一封带 plaintext 兜底 + 标准事务邮件 header 的邮件。
	// 收件方反垃圾过滤对极简 HTML-only 事务邮件常常静默丢弃；这条路径包成
	// multipart/alternative,补上 Date / Message-ID / Auto-Submitted /
	// List-Unsubscribe 等 header,显著降低被丢的概率。
	SendTransactionalHTML(ctx context.Context, to, subject, htmlBody, plainBody string) error
}

// SMTPSettingsProvider exposes the admin-tunable SMTP config to EmailService
// without creating an import dependency on modules/common (which itself
// imports modules/base/common — a cycle if we depended back). Any type that
// implements these three getters can drive the email sender; the production
// implementation lives in modules/common.SystemSettings.
type SMTPSettingsProvider interface {
	SupportEmail() string
	SupportEmailSmtp() string
	SupportEmailPwd() string
}

// EmailService 邮件服务
type EmailService struct {
	ctx      *config.Context
	settings SMTPSettingsProvider
	log.Log
}

// rawRedisClients is only used for the manager-code atomic consume path.
// The legacy pkg/redis Conn intentionally does not expose Eval; cache one
// instrumented go-redis client per process Context rather than creating a
// connection pool per HTTP request.
var rawRedisClients sync.Map // map[*config.Context]*rd.Client

// NewEmailService 创建邮件服务。
//
// settings 为 nil 时退化到读取 cfg.Support.*（yaml 静态值）。生产路径
// 应传入 common.EnsureSystemSettings(ctx) 以启用 admin 覆盖。参数显式
// 强制每个 call site 在 nil（yaml-only）和实际注入之间做出选择，避免
// 静默漏掉 admin 配置入口。
func NewEmailService(ctx *config.Context, settings SMTPSettingsProvider) *EmailService {
	return &EmailService{
		ctx:      ctx,
		settings: settings,
		Log:      log.NewTLog("EmailService"),
	}
}

// ErrEmailSendRateLimited is returned by SendVerifyCode when the per-address
// 1-minute resend cooldown is still active. It is a client-actionable condition
// (HTTP 429), not an internal failure — callers should branch on it with
// errors.Is rather than collapsing it onto a generic send-failure code.
var ErrEmailSendRateLimited = errors.New("email resend cooldown active, retry in 1 minute")

var (
	ErrManagerCodeInvalid = errors.New("invalid manager verification code")
	ErrManagerCodeLocked  = errors.New("too many failed attempts, locked for 10 minutes")
)

// SendVerifyCode 发送验证码。
//
// 主题/正文由 emailtmpl 按 lang 渲染（外置 per-lang 模板，issue #221）；走
// SendTransactionalHTML 而非极简 sendEmail —— 验证码是高价值事务邮件，带
// plaintext 兜底 + 标准事务邮件 header 可显著降低被反垃圾静默丢弃的概率。
func (s *EmailService) SendVerifyCode(ctx context.Context, email string, codeType CodeType, lang string) error {
	return s.sendVerifyCode(ctx, email, codeType, lang, false, "")
}

// SendVerifyCodeTracked is the delivery-confirmed variant used by manager MFA.
// It writes a pending status before SMTP, changes it to sent only after the
// SMTP transaction returns nil, and removes the code on every failure path.
// Ordinary user flows continue through SendVerifyCode and retain the legacy
// missing-status compatibility behavior.
func (s *EmailService) SendVerifyCodeTracked(ctx context.Context, email string, codeType CodeType, lang string) error {
	return s.sendVerifyCode(ctx, email, codeType, lang, true, "")
}

// SendVerifyCodeTrackedWithAttempt is the manager-console variant whose Redis
// status is bound to a caller-owned attempt ID. A slow SMTP request that
// returns after its 120s send lock has been replaced cannot mark a newer
// attempt as sent, nor can its cleanup delete the newer code.
func (s *EmailService) SendVerifyCodeTrackedWithAttempt(ctx context.Context, email string, codeType CodeType, lang, attemptID string) error {
	if strings.TrimSpace(attemptID) == "" {
		return errors.New("send attempt id must not be empty")
	}
	return s.sendVerifyCode(ctx, email, codeType, lang, true, attemptID)
}

func (s *EmailService) sendVerifyCode(ctx context.Context, email string, codeType CodeType, lang string, tracked bool, attemptID string) error {
	// 检查发送频率限制。额度按邮箱 + CodeType 隔离。
	rateLimitKey := EmailRateLimitKey(email, codeType)
	exists, err := s.ctx.GetRedisConn().GetString(rateLimitKey)
	if err != nil {
		return err
	}
	if exists != "" {
		return ErrEmailSendRateLimited
	}

	// 生成6位验证码
	code, err := generateSecureVerifyCode(6)
	if err != nil {
		s.Error("generate verify code", zap.Error(err))
		return errors.New("internal error, please retry")
	}
	s.Info("发送邮箱验证码", zap.String("email", email))

	rendered, err := emailtmpl.Render(emailtmpl.KeyVerifyCode, lang, emailtmpl.VerifyCodeData{Code: code})
	if err != nil {
		// 渲染失败属于配置/构建问题（模板缺失/损坏），不应把验证码写进缓存后
		// 却发不出邮件 —— 在写缓存与限速之前先 fail。
		s.Error("render verify-code email", zap.String("lang", lang), zap.Error(err))
		return errors.New("internal error, please retry")
	}

	cacheKey := EmailCodeKey(email, codeType)
	statusKey := EmailCodeStatusKey(email, codeType)
	statusPending := emailCodeStatusPending
	if attemptID != "" {
		statusPending += ":" + attemptID
	}
	if tracked {
		// A new attempt invalidates an older code before any SMTP I/O starts.
		if err = s.ctx.GetRedisConn().Del(cacheKey); err != nil {
			return err
		}
		if err = s.ctx.GetRedisConn().SetAndExpire(statusKey, statusPending, emailCodeTTL); err != nil {
			return err
		}
	}
	err = s.ctx.GetRedisConn().SetAndExpire(cacheKey, code, emailCodeTTL)
	if err != nil {
		if tracked {
			_ = s.ctx.GetRedisConn().Del(statusKey)
		}
		return err
	}

	// 设置发送频率限制（1分钟）
	err = s.ctx.GetRedisConn().SetAndExpire(rateLimitKey, "1", time.Minute)
	if err != nil {
		if tracked {
			_ = s.ctx.GetRedisConn().Del(cacheKey)
			_ = s.ctx.GetRedisConn().Del(statusKey)
		}
		return err
	}

	if err := s.SendTransactionalHTML(ctx, email, rendered.Subject, rendered.HTML, rendered.Text); err != nil {
		if tracked {
			if attemptID != "" {
				_, _ = s.clearTrackedCode(ctx, email, codeType, statusPending)
			} else {
				_ = s.ctx.GetRedisConn().Del(cacheKey)
				_ = s.ctx.GetRedisConn().SetAndExpire(statusKey, emailCodeStatusFailed, time.Minute)
			}
		}
		return err
	}
	if tracked {
		if attemptID != "" {
			committed, err := s.commitTrackedCode(ctx, statusKey, statusPending, emailCodeStatusSent+":"+attemptID)
			if err != nil {
				_, _ = s.clearTrackedCode(ctx, email, codeType, statusPending)
				return err
			}
			if !committed {
				_, _ = s.clearTrackedCode(ctx, email, codeType, statusPending)
				return errors.New("email verification status was superseded")
			}
		} else if err := s.ctx.GetRedisConn().SetAndExpire(statusKey, emailCodeStatusSent, emailCodeTTL); err != nil {
			_ = s.ctx.GetRedisConn().Del(cacheKey)
			_ = s.ctx.GetRedisConn().SetAndExpire(statusKey, emailCodeStatusFailed, time.Minute)
			return err
		}
	}
	return nil
}

// commitTrackedCode changes pending:<attempt> to sent:<attempt> only when the
// status still belongs to that attempt. The Lua compare-and-set is necessary
// because SMTP can complete after a newer challenge has already started.
func (s *EmailService) commitTrackedCode(ctx context.Context, statusKey, pending, sent string) (bool, error) {
	result, err := s.verifyRedis().WithContext(ctx).Eval(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('SET', KEYS[1], ARGV[2], 'EX', ARGV[3])
return 1
`, []string{statusKey}, pending, sent, int(emailCodeTTL.Seconds())).Result()
	if err != nil {
		return false, err
	}
	value, ok := result.(int64)
	return ok && value == 1, nil
}

// clearTrackedCode removes the code and its status only if the status still
// belongs to the pending attempt. It prevents a late SMTP failure from
// deleting a code created by a newer attempt for the same mailbox.
func (s *EmailService) clearTrackedCode(ctx context.Context, email string, codeType CodeType, pending string) (bool, error) {
	result, err := s.verifyRedis().WithContext(ctx).Eval(`
if redis.call('GET', KEYS[2]) ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[1], KEYS[2])
return 1
`, []string{EmailCodeKey(email, codeType), EmailCodeStatusKey(email, codeType)}, pending).Result()
	if err != nil {
		return false, err
	}
	value, ok := result.(int64)
	return ok && value == 1, nil
}

// InvalidateManagerCode atomically removes the shared manager-code/status
// pair. Challenge creation and send claiming use this to make an older code
// unusable before any new SMTP operation begins.
func (s *EmailService) InvalidateManagerCode(ctx context.Context, email string) error {
	return s.verifyRedis().WithContext(ctx).Del(
		EmailCodeKey(email, CodeTypeManagerLogin),
		EmailCodeStatusKey(email, CodeTypeManagerLogin),
	).Err()
}

// ClearManagerCodeIfAttempt removes a code only when its status still carries
// the supplied attempt ID. It is used when an asynchronous SMTP operation
// loses ownership of its challenge state.
func (s *EmailService) ClearManagerCodeIfAttempt(ctx context.Context, email, attemptID string) error {
	_, err := s.clearTrackedCode(ctx, email, CodeTypeManagerLogin, emailCodeStatusSent+":"+attemptID)
	if err != nil {
		return err
	}
	return nil
}

// SendHTMLEmail 直接发送一封 HTML 邮件。subject/body 由调用方负责，本方法
// 不写 Redis、不限速；速率控制由调用方根据业务场景自行处理。
//
// ctx 的 deadline 会传递到 SMTP 层（dial / 投递阶段）；调用方设的 ctx 比
// SMTP 默认超时（dial 15s + IO 60s）更紧时，会真正生效。
//
// 内容仅含 text/html 单一部分,header 也只补 From/To/Subject/MIME-Version/
// Content-Type。短 HTML 事务邮件容易被收件方反垃圾静默丢弃 —— 自检/状态类
// 邮件请改用 SendTransactionalHTML,带 plaintext 兜底和完整事务邮件 header。
func (s *EmailService) SendHTMLEmail(ctx context.Context, to, subject, htmlBody string) error {
	if to == "" {
		return errors.New("recipient must not be empty")
	}
	return s.sendEmail(ctx, to, subject, htmlBody)
}

// SendTransactionalHTML 发送带 plaintext 兜底 + 标准事务邮件 header 的邮件。
//
// 与 SendHTMLEmail 的区别:
//   - 包成 multipart/alternative,plaintext + HTML 双版本
//   - 补 Date / Message-ID / Auto-Submitted / List-Unsubscribe 等反垃圾过滤
//     期望看到的事务邮件特征(故意不发 Precedence: bulk,详见 buildTransactional
//     Message 中的注释 —— 部分 MTA 会把它解释成"不要生成退信",跟本路径用于
//     诊断的目的相反)
//
// 经验验证:阿里云 SMTP → mininglamp.com 这条链路,只发极简 HTML 单一部分
// (~300 字节)的测试邮件会被收件方静默丢弃,既不入收件箱也不入垃圾夹也
// 不退信;同样的链路、同样的凭据,改成 multipart/alternative + 标准 header
// (~2KB) 后能正常入箱。该方法把这套包装内化,所有"系统自检 / 邀请 / 通知"
// 类事务邮件应该走这条路径。
//
// plainBody 必须由调用方提供(避免依赖脆弱的"从 HTML strip 标签"启发式)。
// htmlBody 为空时,plaintext 仍会被发出(降级体验,但通路工作)。
func (s *EmailService) SendTransactionalHTML(ctx context.Context, to, subject, htmlBody, plainBody string) error {
	if to == "" {
		return errors.New("recipient must not be empty")
	}
	smtpAddr, fromAddr, pwd := s.resolveSMTP()
	if err := ValidateSMTPConfiguration(smtpAddr, fromAddr, pwd); err != nil {
		return err
	}
	toSan, fromSan, subjectSan := sanitizeHeader(to), sanitizeHeader(fromAddr), sanitizeHeader(subject)
	msg, err := buildTransactionalMessage(fromSan, toSan, subjectSan, htmlBody, plainBody)
	if err != nil {
		return err
	}
	return s.dispatchSMTP(ctx, smtpAddr, fromSan, pwd, toSan, msg)
}

// sendEmail 通过SMTP发送简单的 text/html 单一部分邮件。
// 用于验证码 / 兼容旧调用方;新通知类邮件请用 SendTransactionalHTML。
func (s *EmailService) sendEmail(ctx context.Context, to, subject, body string) error {
	smtpAddr, fromAddr, pwd := s.resolveSMTP()

	if err := ValidateSMTPConfiguration(smtpAddr, fromAddr, pwd); err != nil {
		return err
	}

	toSan, fromSan, subjectSan := sanitizeHeader(to), sanitizeHeader(fromAddr), sanitizeHeader(subject)

	msg := "From: " + fromSan + "\r\n" +
		"To: " + toSan + "\r\n" +
		"Subject: " + encodeSubject(subjectSan) + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		body + "\r\n"

	return s.dispatchSMTP(ctx, smtpAddr, fromSan, pwd, toSan, []byte(msg))
}

// ValidateSMTPConfiguration validates the complete effective configuration
// before any network operation. In particular, accepting a syntactically
// non-empty but invalid MAIL FROM would let an SMTP server reject MFA mail
// after the policy had already been enabled.
func ValidateSMTPConfiguration(smtpAddr, fromAddr, password string) error {
	smtpAddr = strings.TrimSpace(smtpAddr)
	fromAddr = strings.TrimSpace(fromAddr)
	if smtpAddr == "" || fromAddr == "" || password == "" {
		return errors.New("email service not configured")
	}
	if err := ValidateEmailAddress(fromAddr); err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(smtpAddr)
	if err != nil || strings.TrimSpace(host) == "" {
		return errors.New("smtp address format is invalid")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return errors.New("smtp port is invalid")
	}
	return nil
}

// ValidateEmailAddress accepts only a bare RFC-style mailbox. Display-name
// forms are valid in a message header but are not a safe SMTP envelope sender
// for this configuration field.
func ValidateEmailAddress(email string) error {
	email = strings.TrimSpace(email)
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email || parsed.Name != "" {
		return errors.New("email address is invalid")
	}
	return nil
}

// PreflightSMTP exercises the same SMTP transaction used for real mail. The
// sender address is used as the recipient because no new deployment setting
// is introduced for a probe mailbox. This deliberately sends a real probe;
// callers invoke it only when enabling/validating manager MFA or during the
// startup warning check.
func (s *EmailService) PreflightSMTP(ctx context.Context) error {
	smtpAddr, fromAddr, pwd := s.resolveSMTP()
	if err := ValidateSMTPConfiguration(smtpAddr, fromAddr, pwd); err != nil {
		return err
	}
	return s.SendTransactionalHTML(ctx, fromAddr,
		"[Octo] 管理控制台 MFA SMTP 自检",
		"<p>Octo 管理控制台 MFA SMTP configuration preflight.</p>",
		"Octo 管理控制台 MFA SMTP configuration preflight.")
}

// sanitizeHeader 清除 \r / \n,防止 CRLF 注入攻击者构造 "Bcc: hacker@evil.com"
// 等额外 header。所有用作 SMTP header 字段或 envelope (MAIL FROM / RCPT TO) 的
// 字符串都必须先过这里。
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

// encodeSubject RFC 2047 word-encodes a Subject so non-ASCII content (localized
// subjects like "Octo 验证码" / "Octo 邀请你加入团队空间「…」", or user-controlled
// space names) survives strict MTAs and clients instead of risking mojibake.
// ASCII input is returned unchanged. The encoder only ever emits ASCII, so it
// cannot reintroduce CRLF header injection; callers still pass CRLF-sanitized
// input for defense in depth.
func encodeSubject(s string) string {
	return mime.QEncoding.Encode("utf-8", s)
}

// buildTransactionalMessage 拼一份 multipart/alternative 报文,内含两段标准
// 事务邮件特征 header。boundary 用随机串避免与 body 字面冲突。
//
// 设计取舍:把模板字符串内联放在这里而不是模板文件,因为这是 SMTP 层基础
// 设施,完全不参与产品 UI;调用方传入的 htmlBody / plainBody 才是内容。
func buildTransactionalMessage(fromSan, toSan, subjectSan, htmlBody, plainBody string) ([]byte, error) {
	boundaryBytes := make([]byte, 8)
	if _, err := rand.Read(boundaryBytes); err != nil {
		return nil, fmt.Errorf("生成 multipart boundary 失败: %w", err)
	}
	boundary := "octo_" + hex.EncodeToString(boundaryBytes)

	msgIDBytes := make([]byte, 12)
	if _, err := rand.Read(msgIDBytes); err != nil {
		return nil, fmt.Errorf("生成 Message-ID 失败: %w", err)
	}
	// Message-ID 的 domain 部分用 From 地址的域名,跟 SPF 校验对齐。
	domain := "octo.local"
	if at := strings.LastIndex(fromSan, "@"); at >= 0 && at < len(fromSan)-1 {
		domain = fromSan[at+1:]
	}
	messageID := fmt.Sprintf("<%s@%s>", hex.EncodeToString(msgIDBytes), domain)

	headers := []string{
		"From: " + fromSan,
		"To: " + toSan,
		"Subject: " + encodeSubject(subjectSan),
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"Message-ID: " + messageID,
		"MIME-Version: 1.0",
		`Content-Type: multipart/alternative; boundary="` + boundary + `"`,
		// List-Unsubscribe 单独保留 mailto 形态(RFC 2369),作为 transactional
		// 信号给 Gmail/Outlook 打分用。
		// 不再发 "List-Unsubscribe-Post: One-Click":RFC 8058 要求 One-Click 必
		// 须配 HTTPS POST endpoint,跟 mailto 配是 misuse,部分打分引擎会判 weak
		// signal。等真有 HTTPS 退订入口再加回来。
		"List-Unsubscribe: <mailto:" + fromSan + "?subject=unsubscribe>",
		"X-Mailer: Octo Transactional Mailer",
		// Auto-Submitted 让收件方知道这是机器生成,顺便压制 out-of-office
		// 自动回复。不发 "Precedence: bulk":部分 MTA 把它解释为"不要生成 DSN
		// (退信)",而本 endpoint 的诊断价值正是依赖退信,跟意图相反。
		"Auto-Submitted: auto-generated",
	}

	var b strings.Builder
	for _, h := range headers {
		b.WriteString(h)
		b.WriteString("\r\n")
	}
	b.WriteString("\r\n")
	// plaintext part (RFC 2046: 多个 alternative 时,*先*放最简单的格式,*后*放最丰富的;
	// 兼容性最差的客户端只会渲染第一个能识别的)。
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(plainBody)
	b.WriteString("\r\n")
	// html part
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(htmlBody)
	b.WriteString("\r\n")
	b.WriteString("--" + boundary + "--\r\n")
	return []byte(b.String()), nil
}

// dispatchSMTP 跑完一次 SMTP 投递:dial → (STARTTLS) → AUTH → MAIL → RCPT →
// DATA → QUIT。fromSan / toSan 必须已经过 sanitizeHeader。
func (s *EmailService) dispatchSMTP(ctx context.Context, smtpAddr, fromSan, pwd, toSan string, msg []byte) error {
	host, port, err := net.SplitHostPort(smtpAddr)
	if err != nil {
		return fmt.Errorf("smtp地址格式错误: %w", err)
	}
	auth := smtp.PlainAuth("", fromSan, pwd, host)

	dialer := &net.Dialer{Timeout: smtpDialTimeout}
	var conn net.Conn
	if port == "465" {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: host}}
		conn, err = tlsDialer.DialContext(ctx, "tcp", smtpAddr)
		if err != nil {
			return fmt.Errorf("TLS连接失败: %w", err)
		}
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", smtpAddr)
		if err != nil {
			return fmt.Errorf("SMTP 连接失败: %w", err)
		}
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	} else {
		_ = conn.SetDeadline(time.Now().Add(smtpIOTimeout))
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("创建SMTP客户端失败: %w", err)
	}
	defer client.Close()

	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err = client.StartTLS(&tls.Config{ServerName: host}); err != nil {
				return fmt.Errorf("STARTTLS 失败: %w", err)
			}
		}
	}

	return runSMTPTransaction(client, auth, fromSan, toSan, msg)
}

// runSMTPTransaction 跑完一次 SMTP 投递：Auth → Mail → Rcpt → Data → Quit。
// 抽出来是为了 465 / 587 路径不用复制 7 行序列；同时确保两条路径都发 QUIT
// （旧实现用 smtp.SendMail，stdlib 末尾就是 c.Quit()——本 PR 重写时漏发，
// 部分严格邮件网关在缺 QUIT 时会丢弃消息。defer client.Close 仅用于异常兜底）。
func runSMTPTransaction(client *smtp.Client, auth smtp.Auth, from, to string, msg []byte) error {
	if err := client.Auth(auth); err != nil {
		return fmt.Errorf("SMTP认证失败: %w", err)
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write(msg); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

const (
	smtpDialTimeout = 15 * time.Second
	smtpIOTimeout   = 60 * time.Second
)

// resolveSMTP returns the effective SMTP config: admin-tunable values from
// the injected provider win over yaml; a missing provider (legacy callers
// and unit tests) falls back to cfg.Support.* directly.
func (s *EmailService) resolveSMTP() (smtpAddr, from, pwd string) {
	if s.settings != nil {
		smtpAddr = s.settings.SupportEmailSmtp()
		from = s.settings.SupportEmail()
		pwd = s.settings.SupportEmailPwd()
		return
	}
	cfg := s.ctx.GetConfig()
	smtpAddr = cfg.Support.EmailSmtp
	from = cfg.Support.Email
	pwd = cfg.Support.EmailPwd
	return
}

// Verify 验证验证码（验证成功后销毁缓存）
func (s *EmailService) Verify(ctx context.Context, email, code string, codeType CodeType) error {
	// 检查是否被锁定
	lockKey := EmailVerifyLockKey(email, codeType)
	locked, err := s.ctx.GetRedisConn().GetString(lockKey)
	if err != nil {
		return err
	}
	if locked != "" {
		return errors.New("too many failed attempts, locked for 10 minutes")
	}

	// 支持测试验证码（仅限非 release 模式；release 下即便配置了 SMSCode 也不会匹配）
	if MatchTestCode(s.ctx.GetConfig(), code) {
		log.Warn("email verify passed via test SMSCode", zap.String("email", maskEmail(email)))
		return nil
	}

	cacheKey := EmailCodeKey(email, codeType)
	statusKey := EmailCodeStatusKey(email, codeType)
	sysCode, err := s.ctx.GetRedisConn().GetString(cacheKey)
	if err != nil {
		return err
	}
	status := ""
	if emailCodeRequiresSentStatus(codeType) {
		status, err = s.ctx.GetRedisConn().GetString(statusKey)
		if err != nil {
			return err
		}
	}
	if sysCode != "" && subtle.ConstantTimeCompare([]byte(sysCode), []byte(code)) == 1 &&
		(!emailCodeRequiresSentStatus(codeType) || status == emailCodeStatusSent) {
		s.ctx.GetRedisConn().Del(cacheKey)
		s.ctx.GetRedisConn().Del(statusKey)
		// 验证成功，清除失败计数
		failCountKey := EmailVerifyFailKey(email, codeType)
		s.ctx.GetRedisConn().Del(failCountKey)
		s.ctx.GetRedisConn().Del(lockKey)
		return nil
	}

	// 验证失败，增加失败计数
	failCountKey := EmailVerifyFailKey(email, codeType)
	failCountStr, _ := s.ctx.GetRedisConn().GetString(failCountKey)
	failCount := 0
	if failCountStr != "" {
		if count, err := strconv.Atoi(failCountStr); err == nil {
			failCount = count
		}
	}
	failCount++

	if failCount >= 3 {
		s.ctx.GetRedisConn().SetAndExpire(lockKey, "1", time.Minute*10)
		return errors.New("too many failed attempts, locked for 10 minutes")
	}
	s.ctx.GetRedisConn().SetAndExpire(failCountKey, fmt.Sprintf("%d", failCount), time.Minute*10)

	s.Info("邮箱验证码错误", zap.String("email", email))
	return errors.New("invalid verification code")
}

// verifyRedis returns the raw client used for the manager-only atomic Lua
// consume. It is intentionally kept private so ordinary email callers keep
// their established Conn behavior.
func (s *EmailService) verifyRedis() *rd.Client {
	if client, ok := rawRedisClients.Load(s.ctx); ok {
		return client.(*rd.Client)
	}
	client := octoredis.NewInstrumentedClient(s.ctx.GetConfig(), func(o *rd.Options) {
		o.MaxRetries = 1
		o.PoolSize = 10
	})
	actual, loaded := rawRedisClients.LoadOrStore(s.ctx, client)
	if loaded {
		_ = client.Close()
		return actual.(*rd.Client)
	}
	return client
}

// VerifyManagerCodeAtomically verifies and consumes a manager OTP in one Redis
// script. The script also binds the email status to the active challenge's
// sent attempt, so a code from an older challenge cannot be reused by a newer
// challenge for the same mailbox. A successful consume also removes the
// challenge and UID active index before the caller proceeds to token issuance;
// this makes the password-validated challenge single-use even if token
// issuance later fails.
func (s *EmailService) VerifyManagerCodeAtomically(ctx context.Context, email, code, challengeID string, activeKey, sendStateKey, challengeKey string) error {
	const script = `
local lock = redis.call('GET', KEYS[4])
if lock then return 2 end
if redis.call('GET', KEYS[5]) ~= ARGV[2] then return 0 end
local expected = redis.call('GET', KEYS[1])
local status = redis.call('GET', KEYS[2])
local sendStatus = redis.call('HGET', KEYS[6], 'status')
local attemptID = redis.call('HGET', KEYS[6], 'attempt_id')
if expected and sendStatus == 'sent' and attemptID and status == ('sent:' .. attemptID) and expected == ARGV[1] then
	redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[6])
	if redis.call('GET', KEYS[5]) == ARGV[2] then
	  redis.call('DEL', KEYS[5], KEYS[7])
	end
	return 1
end
local count = tonumber(redis.call('GET', KEYS[3]) or '0') + 1
if count >= 3 then
  redis.call('SET', KEYS[4], '1', 'EX', ARGV[3])
  redis.call('DEL', KEYS[3])
  return 2
end
redis.call('SET', KEYS[3], tostring(count), 'EX', ARGV[4])
return 0
`
	result, err := s.verifyRedis().WithContext(ctx).Eval(script, []string{
		EmailCodeKey(email, CodeTypeManagerLogin),
		EmailCodeStatusKey(email, CodeTypeManagerLogin),
		EmailVerifyFailKey(email, CodeTypeManagerLogin),
		EmailVerifyLockKey(email, CodeTypeManagerLogin),
		activeKey,
		sendStateKey,
		challengeKey,
	}, code, challengeID, int((10 * time.Minute).Seconds()), int((10 * time.Minute).Seconds())).Result()
	if err != nil {
		return err
	}
	value, ok := result.(int64)
	if !ok {
		return errors.New("invalid manager verification result")
	}
	switch value {
	case 1:
		return nil
	case 2:
		return ErrManagerCodeLocked
	default:
		return ErrManagerCodeInvalid
	}
}
