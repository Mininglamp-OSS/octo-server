package common

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"go.uber.org/zap"
)

const CacheKeyEmailCode = "emailcode:"

// IEmailService 邮件服务接口
type IEmailService interface {
	// 发送验证码
	SendVerifyCode(ctx context.Context, email string, codeType CodeType) error
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
var ErrEmailSendRateLimited = errors.New("发送过于频繁，请1分钟后再试")

// SendVerifyCode 发送验证码
func (s *EmailService) SendVerifyCode(ctx context.Context, email string, codeType CodeType) error {
	// 检查发送频率限制
	rateLimitKey := fmt.Sprintf("email_rate_limit:%s", email)
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
		s.Error("生成验证码失败", zap.Error(err))
		return errors.New("系统错误，请稍后重试")
	}
	s.Info("发送邮箱验证码", zap.String("email", email))

	cacheKey := fmt.Sprintf("%s%d@%s", CacheKeyEmailCode, codeType, email)
	err = s.ctx.GetRedisConn().SetAndExpire(cacheKey, code, time.Minute*5)
	if err != nil {
		return err
	}

	// 设置发送频率限制（1分钟）
	err = s.ctx.GetRedisConn().SetAndExpire(rateLimitKey, "1", time.Minute)
	if err != nil {
		return err
	}

	subject := "Octo 验证码"
	body := fmt.Sprintf(`<div style="max-width:400px;margin:20px auto;font-family:Arial,sans-serif;padding:20px;border:1px solid #e0e0e0;border-radius:8px;">
<h2 style="color:#7c3aed;margin:0 0 16px;">Octo</h2>
<p style="color:#333;">您的验证码为：</p>
<div style="background:#f5f3ff;padding:16px;border-radius:6px;text-align:center;margin:12px 0;">
<span style="font-size:32px;font-weight:bold;letter-spacing:8px;color:#7c3aed;">%s</span>
</div>
<p style="color:#666;font-size:13px;">验证码 5 分钟内有效，请勿泄露给他人。</p>
</div>`, code)
	return s.sendEmail(ctx, email, subject, body)
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
		return errors.New("收件人不能为空")
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
		return errors.New("收件人不能为空")
	}
	smtpAddr, fromAddr, pwd := s.resolveSMTP()
	if smtpAddr == "" || fromAddr == "" || pwd == "" {
		return errors.New("邮件服务未配置，请联系管理员")
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

	if smtpAddr == "" || fromAddr == "" || pwd == "" {
		return errors.New("邮件服务未配置，请联系管理员")
	}

	toSan, fromSan, subjectSan := sanitizeHeader(to), sanitizeHeader(fromAddr), sanitizeHeader(subject)

	msg := "From: " + fromSan + "\r\n" +
		"To: " + toSan + "\r\n" +
		"Subject: " + subjectSan + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=UTF-8\r\n" +
		"\r\n" +
		body + "\r\n"

	return s.dispatchSMTP(ctx, smtpAddr, fromSan, pwd, toSan, []byte(msg))
}

// sanitizeHeader 清除 \r / \n,防止 CRLF 注入攻击者构造 "Bcc: hacker@evil.com"
// 等额外 header。所有用作 SMTP header 字段或 envelope (MAIL FROM / RCPT TO) 的
// 字符串都必须先过这里。
func sanitizeHeader(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
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
		"Subject: " + subjectSan,
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
	lockKey := fmt.Sprintf("email_verify_lock:%s", email)
	locked, err := s.ctx.GetRedisConn().GetString(lockKey)
	if err != nil {
		return err
	}
	if locked != "" {
		return errors.New("验证失败次数过多，请10分钟后再试")
	}

	// 支持测试验证码（仅限非 release 模式；release 下即便配置了 SMSCode 也不会匹配）
	if MatchTestCode(s.ctx.GetConfig(), code) {
		log.Warn("email verify passed via test SMSCode", zap.String("email", maskEmail(email)))
		return nil
	}

	cacheKey := fmt.Sprintf("%s%d@%s", CacheKeyEmailCode, codeType, email)
	sysCode, err := s.ctx.GetRedisConn().GetString(cacheKey)
	if err != nil {
		return err
	}
	if sysCode != "" && subtle.ConstantTimeCompare([]byte(sysCode), []byte(code)) == 1 {
		s.ctx.GetRedisConn().Del(cacheKey)
		// 验证成功，清除失败计数
		failCountKey := fmt.Sprintf("email_verify_fail:%s", email)
		s.ctx.GetRedisConn().Del(failCountKey)
		s.ctx.GetRedisConn().Del(lockKey)
		return nil
	}

	// 验证失败，增加失败计数
	failCountKey := fmt.Sprintf("email_verify_fail:%s", email)
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
		return errors.New("验证失败次数过多，已锁定10分钟")
	}
	s.ctx.GetRedisConn().SetAndExpire(failCountKey, fmt.Sprintf("%d", failCount), time.Minute*10)

	s.Info("邮箱验证码错误", zap.String("email", email))
	return errors.New("验证码无效！")
}
