package docsaccessrequest

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

// pendingLabelSet 是 pending 视图按语言本地化的文案集。硬编码 zh-CN / en 两档,
// 与 modules/notify 现有 docsLabelsFor + approvalActionLabelsFor 语义一致;
// 未来若接入 i18n locales 再迁移。
type pendingLabelSet struct {
	headerLabel   string
	statusLabel   string
	bannerSuffix  string
	roleLabel     string
	reasonLabel   string
	approveTitle  string
	denyTitle     string
	fallbackNamed string // format: "<actor> ... <docTitle>"
	fallbackAnon  string // format: "<docTitle>"
}

// pendingLabels 选择语言;permission.roleLabel 若非空,套用到 banner 里替代 "查看者" 默认值。
func pendingLabels(lang string, pf pendingFields) pendingLabelSet {
	roleLabel := strings.TrimSpace(pf.Permission.RoleLabel)
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		if roleLabel == "" {
			roleLabel = "协作者"
		}
		return pendingLabelSet{
			headerLabel:   "文档申请",
			statusLabel:   "待你处理",
			bannerSuffix:  "申请成为此文档的" + roleLabel + "。",
			roleLabel:     "申请人",
			reasonLabel:   "申请原因",
			approveTitle:  "允许",
			denyTitle:     "拒绝",
			fallbackNamed: "%s 申请查看文档「%s」,请前往处理。",
			fallbackAnon:  "有新的文档访问申请「%s」,请前往处理。",
		}
	}
	if roleLabel == "" {
		roleLabel = "collaborator"
	}
	return pendingLabelSet{
		headerLabel:   "Document access",
		statusLabel:   "Pending",
		bannerSuffix:  "is requesting " + roleLabel + " access to this document.",
		roleLabel:     "Requester",
		reasonLabel:   "Reason",
		approveTitle:  "Allow",
		denyTitle:     "Deny",
		fallbackNamed: "%s requested access to \"%s\", please review.",
		fallbackAnon:  "New access request for \"%s\", please review.",
	}
}

// docsApprovalContentFromFields 把契约字段填进 cardtmpl.DocsApprovalContent。
// 服务端负责补 HeaderLabel / StatusLabel / BannerSuffix / RoleLabel / ReasonLabel 等
// L1 契约里标为 server-filled 的文案(handoff data.schema.json 中同名字段并未 required)。
func docsApprovalContentFromFields(pf pendingFields, labels pendingLabelSet) cardtmpl.DocsApprovalContent {
	return cardtmpl.DocsApprovalContent{
		Title:        pf.Document.Title,
		Actor:        pf.Requester.Name,
		ActorAvatar:  pf.Requester.AvatarURL,
		Timestamp:    pf.RequestedAtDisplay,
		Reason:       pf.RequestReason,
		Variant:      Variant,
		HeaderLabel:  labels.headerLabel,
		StatusLabel:  labels.statusLabel,
		BannerSuffix: labels.bannerSuffix,
		RoleLabel:    labels.roleLabel,
		ReasonLabel:  labels.reasonLabel,
		// Source 走 Registry.Render 的 Meta.Source 注入,不在 content 里带
	}
}

// cardtmplApprovalActions 组装 approve/deny 按钮文案入参。
func cardtmplApprovalActions(approveTitle, denyTitle string) cardtmpl.ApprovalActions {
	return cardtmpl.ApprovalActions{
		ApproveTitle: approveTitle,
		DenyTitle:    denyTitle,
	}
}
