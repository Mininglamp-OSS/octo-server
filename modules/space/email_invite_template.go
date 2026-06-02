package space

import (
	"fmt"
	htmltemplate "html/template"
	"net/url"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/modules/base/common/emailtmpl"
)

// emailInviteAcceptPath 后端承接邀请落地页的 API 路径。与 space_join_approve.html
// 走同一模式：后端读 HTML 模板、注入 API_BASE_URL 后返回；JS 在浏览器里完成
// 预览展示和接受动作。
const emailInviteAcceptPath = "/v1/space/email-invite"

// emailInviteAcceptURL 用 External.BaseURL 拼出邀请接受链接。base 为空时返回空串，
// 由调用方决定是否跳过发送（典型场景：本地开发未配置 BaseURL）。
//
// lang 非空时追加 &lang=<lang>，作为前向兼容把语言带给落地页。注意：当前落地页
// assets/web/space_email_invite.html 仍固定中文、尚未消费该参数，落地页本地化是
// 独立后续工作；本期邀请语言恒为部署默认（issue #221 决策 A），掩盖了该 gap。
func emailInviteAcceptURL(base, rawToken, lang string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	base = strings.TrimRight(base, "/")
	link := fmt.Sprintf("%s%s?token=%s", base, emailInviteAcceptPath, url.QueryEscape(rawToken))
	if lang != "" {
		link += "&lang=" + url.QueryEscape(lang)
	}
	return link
}

// buildOwnerInviteEmail 构造 owner 邀请邮件。文案外置于 emailtmpl 的 per-lang
// 模板，html/template 自动转义用户输入字段（InviterName/PlannedName/...）杜绝
// XSS；空 InviterName 的兜底文案由模板按语言提供，不再在 Go 里硬编码中文。
func buildOwnerInviteEmail(inv *spaceEmailInviteModel, inviterName, lang, acceptURL string) (emailtmpl.Rendered, error) {
	return emailtmpl.Render(emailtmpl.KeySpaceInviteOwner, lang, emailtmpl.SpaceInviteOwnerData{
		InviterName: strings.TrimSpace(inviterName),
		PlannedName: inv.PlannedName,
		PlannedDesc: inv.PlannedDescription,
		AcceptURL:   htmltemplate.URL(acceptURL),
	})
}

// buildMemberInviteEmail 构造 member 邀请邮件。角色标签（成员/管理员）由模板按
// IsAdmin 分支本地化，不再在 Go 里硬编码中文标签。
func buildMemberInviteEmail(inv *spaceEmailInviteModel, inviterName, spaceName, lang, acceptURL string) (emailtmpl.Rendered, error) {
	return emailtmpl.Render(emailtmpl.KeySpaceInviteMember, lang, emailtmpl.SpaceInviteMemberData{
		InviterName: strings.TrimSpace(inviterName),
		SpaceName:   spaceName,
		IsAdmin:     inv.Role == EmailInviteRoleAdmin,
		AcceptURL:   htmltemplate.URL(acceptURL),
	})
}
