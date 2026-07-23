package docsaccessrequest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const (
	HandoffRootV3     = "handoff/docs.access-request@0.3.0"
	TemplateVersionV3 = "0.3.0"

	StateApproved cardtmpl.State = "approved"
	StateRejected cardtmpl.State = "rejected"

	VariantApproved = "docs.access_approved"
	VariantRejected = "docs.access_denied"
)

type TemplateV3 struct {
	meta cardtmpl.TemplateMeta
}

func NewV3() *TemplateV3 { return &TemplateV3{} }

func (t *TemplateV3) SetMeta(meta cardtmpl.TemplateMeta) { t.meta = meta }

func (t *TemplateV3) Meta() cardtmpl.TemplateMeta { return t.meta.Clone() }

type v3Fields struct {
	RequestID string `json:"requestId"`
	State     string `json:"state"`
	Document  struct {
		DocID      string `json:"docId"`
		Title      string `json:"title"`
		URL        string `json:"url"`
		SourceName string `json:"sourceName"`
	} `json:"document"`
	Requester struct {
		Name      string `json:"name"`
		AvatarURL string `json:"avatarUrl"`
	} `json:"requester"`
	Permission struct {
		Label     string `json:"label"`
		RoleLabel string `json:"roleLabel"`
	} `json:"permission"`
	RequestReason      string `json:"requestReason"`
	RequestedAtDisplay string `json:"requestedAtDisplay"`
	MessageTimeDisplay string `json:"messageTimeDisplay"`
	Decision           struct {
		OperatorName     string `json:"operatorName"`
		DecidedAtDisplay string `json:"decidedAtDisplay"`
		RejectionReason  string `json:"rejectionReason"`
	} `json:"decision"`
}

func (t *TemplateV3) Build(ctx context.Context, state cardtmpl.State, fields json.RawMessage, env cardtmpl.BuildEnv) (cardtmpl.BuildResult, error) {
	if err := ctx.Err(); err != nil {
		return cardtmpl.BuildResult{}, err
	}
	if state == StatePending {
		var input pendingFields
		if err := json.Unmarshal(fields, &input); err != nil {
			return cardtmpl.BuildResult{}, fmt.Errorf("docs.access-request@0.3.0: unmarshal pending fields: %w", err)
		}
		labels := pendingLabels(env.Lang, input)
		docID := strings.TrimSpace(input.Document.DocID)
		if docID == "" {
			docID = input.RequestID
		}
		body, deepLink, err := cardtmpl.BuildDocsAccessRequestV3BodyWithLang(
			env.Lang, env.WebLoginURL, docID, input.RequestID, env.SpaceID,
			cardtmpl.DocsAccessRequestV3Content{
				DocsApprovalContent: docsApprovalContentFromFields(input, labels),
				SourceName:          input.Document.SourceName,
				PermissionLabel:     input.Permission.Label,
				PermissionRoleLabel: input.Permission.RoleLabel,
				MessageTimeDisplay:  input.MessageTimeDisplay,
			},
			cardtmplApprovalActions(labels.approveTitle, labels.denyTitle),
		)
		if err != nil {
			return cardtmpl.BuildResult{}, err
		}
		source := sourceForLang(env.Lang)
		return cardtmpl.BuildResult{
			Body: body, Variant: Variant, DeepLink: deepLink, Source: &source,
		}, nil
	}
	if state != StateApproved && state != StateRejected {
		return cardtmpl.BuildResult{}, fmt.Errorf("docs.access-request@0.3.0: unsupported state %q", state)
	}
	var input v3Fields
	if err := json.Unmarshal(fields, &input); err != nil {
		return cardtmpl.BuildResult{}, fmt.Errorf("docs.access-request@0.3.0: unmarshal fields: %w", err)
	}
	docID := strings.TrimSpace(input.Document.DocID)
	if docID == "" {
		docID = input.RequestID
	}
	labels := resultLabels(env.Lang, state == StateRejected)
	variant := VariantApproved
	if state == StateRejected {
		variant = VariantRejected
	}
	body, deepLink, err := cardtmpl.BuildDocsApprovalOutcomeV3BodyWithLang(env.Lang, env.WebLoginURL, docID, env.SpaceID, cardtmpl.DocsOutcomeContent{
		Title: input.Document.Title, Variant: variant, Denied: state == StateRejected,
		HeaderLabel: labels.header, SourceName: input.Document.SourceName,
		StatusLabel: labels.status, PermissionLabel: input.Permission.Label, ResultText: labels.result,
		ReasonLabel: labels.reason, Reason: input.Decision.RejectionReason,
		Actor: input.Requester.Name, ActorAvatar: input.Requester.AvatarURL,
		RequestedAtDisplay: input.RequestedAtDisplay, RequestReason: input.RequestReason,
		BannerSuffix: labels.banner(input.Permission.RoleLabel), RoleLabel: labels.role,
		RequestReasonLabel: labels.requestReason, MessageTimeLabel: labels.processedAt,
		MessageTimeDisplay: input.MessageTimeDisplay,
		DecisionSummary:    strings.Trim(strings.TrimSpace(input.Decision.OperatorName)+" · "+strings.TrimSpace(input.Decision.DecidedAtDisplay), " ·"),
	})
	if err != nil {
		return cardtmpl.BuildResult{}, err
	}
	source := sourceForLang(env.Lang)
	return cardtmpl.BuildResult{Body: body, Variant: variant, DeepLink: deepLink, Source: &source}, nil
}

func (t *TemplateV3) FallbackText(state cardtmpl.State, fields json.RawMessage, lang string) (string, error) {
	if state == StatePending {
		return New().FallbackText(state, fields, lang)
	}
	if state != StateApproved && state != StateRejected {
		return "", fmt.Errorf("docs.access-request@0.3.0: unsupported fallback state %q", state)
	}
	var input v3Fields
	if err := json.Unmarshal(fields, &input); err != nil {
		return "", err
	}
	labels := resultLabels(lang, state == StateRejected)
	return strings.TrimSpace(input.Document.Title) + " - " + labels.status, nil
}

type resultLabelSet struct {
	header        string
	status        string
	result        string
	reason        string
	role          string
	requestReason string
	processedAt   string
	zh            bool
}

func (l resultLabelSet) banner(role string) string {
	role = strings.TrimSpace(role)
	if l.zh {
		if role == "" {
			role = "查看者"
		}
		return "申请成为此文档的" + role + "。"
	}
	if role == "" {
		role = "viewer"
	}
	return "requested " + role + " access to this document."
}

func resultLabels(lang string, denied bool) resultLabelSet {
	zh := strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh")
	if zh {
		if denied {
			return resultLabelSet{header: "文档申请", status: "已拒绝", result: "本次访问申请已拒绝，权限状态已更新。", reason: "拒绝原因", role: "申请人", requestReason: "申请原因", processedAt: "处理于", zh: true}
		}
		return resultLabelSet{header: "文档申请", status: "已允许", result: "申请人已获得所申请的文档权限。", reason: "拒绝原因", role: "申请人", requestReason: "申请原因", processedAt: "处理于", zh: true}
	}
	if denied {
		return resultLabelSet{header: "Document access", status: "Denied", result: "The access request was denied.", reason: "Reason", role: "Requester", requestReason: "Reason", processedAt: "Processed at"}
	}
	return resultLabelSet{header: "Document access", status: "Approved", result: "The requester now has access to the document.", reason: "Reason", role: "Requester", requestReason: "Reason", processedAt: "Processed at"}
}
