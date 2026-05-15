package space

import (
	"bytes"
	"fmt"
	"html/template"
	"net/url"
	"strings"
)

// emailInviteAcceptPath 后端承接邀请落地页的 API 路径。与 space_join_approve.html
// 走同一模式：后端读 HTML 模板、注入 API_BASE_URL 后返回；JS 在浏览器里完成
// 预览展示和接受动作。
const emailInviteAcceptPath = "/v1/space/email-invite"

// emailInviteAcceptURL 用 External.BaseURL 拼出邀请接受链接。base 为空时返回空串，
// 由调用方决定是否跳过发送（典型场景：本地开发未配置 BaseURL）。
func emailInviteAcceptURL(base, rawToken string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	return fmt.Sprintf("%s%s?token=%s", base, emailInviteAcceptPath, url.QueryEscape(rawToken))
}

// inviterDisplay 收件方看到的邀请人名；为空时回退到通用文案。
func inviterDisplay(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Octo 管理员"
	}
	return name
}

type ownerEmailData struct {
	InviterName string
	PlannedName string
	PlannedDesc string
	AcceptURL   template.URL // 链接已在拼接前 url.QueryEscape；URL 类型避免 html/template 误转义
}

type memberEmailData struct {
	InviterName string
	SpaceName   string
	RoleLabel   string
	AcceptURL   template.URL
}

var (
	ownerEmailTpl = template.Must(template.New("owner_invite").Parse(`<!DOCTYPE html>
<html><body style="font-family:Arial,Helvetica,sans-serif;background:#f8fafc;padding:24px;">
<div style="max-width:520px;margin:0 auto;background:#fff;border:1px solid #e5e7eb;border-radius:10px;padding:32px;">
  <h2 style="color:#7c3aed;margin:0 0 16px;">Octo Space 邀请</h2>
  <p style="color:#111;font-size:15px;line-height:1.6;">
    {{.InviterName}} 邀请你创建并成为团队空间 <strong>{{.PlannedName}}</strong> 的所有者。
  </p>
  {{if .PlannedDesc}}<p style="color:#555;font-size:14px;line-height:1.6;">空间描述：{{.PlannedDesc}}</p>{{end}}
  <p style="text-align:center;margin:28px 0;">
    <a href="{{.AcceptURL}}" style="display:inline-block;background:#7c3aed;color:#fff;text-decoration:none;padding:12px 28px;border-radius:6px;font-weight:600;">接受邀请并创建空间</a>
  </p>
  <p style="color:#888;font-size:12px;line-height:1.6;">
    若按钮无法点击，请复制下方链接到浏览器打开：<br/>
    <span style="word-break:break-all;color:#555;">{{.AcceptURL}}</span>
  </p>
  <p style="color:#aaa;font-size:12px;margin-top:24px;">如果你并未预期此邀请，可忽略此邮件。</p>
</div></body></html>`))

	memberEmailTpl = template.Must(template.New("member_invite").Parse(`<!DOCTYPE html>
<html><body style="font-family:Arial,Helvetica,sans-serif;background:#f8fafc;padding:24px;">
<div style="max-width:520px;margin:0 auto;background:#fff;border:1px solid #e5e7eb;border-radius:10px;padding:32px;">
  <h2 style="color:#7c3aed;margin:0 0 16px;">Octo Space 邀请</h2>
  <p style="color:#111;font-size:15px;line-height:1.6;">
    {{.InviterName}} 邀请你以 <strong>{{.RoleLabel}}</strong> 身份加入团队空间 <strong>{{.SpaceName}}</strong>。
  </p>
  <p style="text-align:center;margin:28px 0;">
    <a href="{{.AcceptURL}}" style="display:inline-block;background:#7c3aed;color:#fff;text-decoration:none;padding:12px 28px;border-radius:6px;font-weight:600;">接受邀请</a>
  </p>
  <p style="color:#888;font-size:12px;line-height:1.6;">
    若按钮无法点击，请复制下方链接到浏览器打开：<br/>
    <span style="word-break:break-all;color:#555;">{{.AcceptURL}}</span>
  </p>
  <p style="color:#aaa;font-size:12px;margin-top:24px;">如果你并未预期此邀请，可忽略此邮件。</p>
</div></body></html>`))
)

// buildOwnerInviteEmail 构造 owner 邀请邮件 (subject, html)。模板使用 html/template
// 自动转义所有用户输入字段，杜绝 XSS 注入。
//
// 模板由 template.Must 编译期校验，且写入 bytes.Buffer 不会失败；当前实现返回
// error 仅是为了未来若引入用户提供的方法/字段可失败时不会被静默吞掉。
func buildOwnerInviteEmail(inv *spaceEmailInviteModel, inviterName, acceptURL string) (string, string, error) {
	data := ownerEmailData{
		InviterName: inviterDisplay(inviterName),
		PlannedName: inv.PlannedName,
		PlannedDesc: inv.PlannedDescription,
		AcceptURL:   template.URL(acceptURL),
	}
	subject := fmt.Sprintf("Octo 邀请你创建团队空间「%s」", inv.PlannedName)
	var buf bytes.Buffer
	if err := ownerEmailTpl.Execute(&buf, data); err != nil {
		return "", "", fmt.Errorf("渲染 owner 邀请邮件失败: %w", err)
	}
	return subject, buf.String(), nil
}

// buildMemberInviteEmail 构造 member 邀请邮件 (subject, html)。
func buildMemberInviteEmail(inv *spaceEmailInviteModel, inviterName, spaceName, acceptURL string) (string, string, error) {
	roleLabel := "成员"
	if inv.Role == EmailInviteRoleAdmin {
		roleLabel = "管理员"
	}
	data := memberEmailData{
		InviterName: inviterDisplay(inviterName),
		SpaceName:   spaceName,
		RoleLabel:   roleLabel,
		AcceptURL:   template.URL(acceptURL),
	}
	subject := fmt.Sprintf("Octo 邀请你加入团队空间「%s」", spaceName)
	var buf bytes.Buffer
	if err := memberEmailTpl.Execute(&buf, data); err != nil {
		return "", "", fmt.Errorf("渲染 member 邀请邮件失败: %w", err)
	}
	return subject, buf.String(), nil
}
