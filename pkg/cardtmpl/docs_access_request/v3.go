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
	// StateCancelled is a request withdrawn or expired rather than decided, so
	// it carries no `decision` block — the data contract requires one only for
	// approved/rejected. It renders through the result view like the other two
	// because a card that has been marked cannot be replaced by a frame built
	// outside the Registry: the markers assert a template identity, and only a
	// Registry render produces the metadata.octo.template they must agree with.
	StateCancelled cardtmpl.State = "cancelled"
	// StateUnavailable is the outcome for an action that reached no decision at
	// all — the finalizer's catch-all, which previously rendered a plain
	// resource card. It exists for the same reason StateCancelled does: the
	// replacement for a marked card has to be a Registry render.
	StateUnavailable cardtmpl.State = "unavailable"

	VariantApproved    = "docs.access_approved"
	VariantRejected    = "docs.access_denied"
	VariantCancelled   = "docs.access_cancelled"
	VariantUnavailable = "docs.access_unavailable"
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
		Name            string `json:"name"`
		AvatarURL       string `json:"avatarUrl"`
		SourceSpaceName string `json:"sourceSpaceName"`
	} `json:"requester"`
	RequestedBotNames []string `json:"requestedBotNames"`
	Permission        struct {
		Label     string `json:"label"`
		RoleLabel string `json:"roleLabel"`
	} `json:"permission"`
	RequestReason      string `json:"requestReason"`
	RequestedAtDisplay string `json:"requestedAtDisplay"`
	MessageTimeDisplay string `json:"messageTimeDisplay"`
	Decision           struct {
		OperatorName      string `json:"operatorName"`
		OperatorSpaceName string `json:"operatorSpaceName"`
		DecidedAtDisplay  string `json:"decidedAtDisplay"`
		RejectionReason   string `json:"rejectionReason"`
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
			env.Lang, env.WebLoginURL, docID, input.RequestID,
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
	if state != StateApproved && state != StateRejected &&
		state != StateCancelled && state != StateUnavailable {
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
	decidedAtDisplay := strings.TrimSpace(input.Decision.DecidedAtDisplay)
	if decidedAtDisplay == "" {
		decidedAtDisplay = input.MessageTimeDisplay // compatibility for older callers
	}
	variant := VariantApproved
	switch state {
	case StateRejected:
		variant = VariantRejected
	case StateCancelled:
		labels, variant = cancelledLabels(env.Lang), VariantCancelled
	case StateUnavailable:
		labels, variant = unavailableLabels(env.Lang), VariantUnavailable
	}
	body, deepLink, err := cardtmpl.BuildDocsApprovalOutcomeV3BodyWithLang(env.Lang, env.WebLoginURL, docID, cardtmpl.DocsOutcomeContent{
		// Denied selects the L0 "not granted" treatment rather than the green
		// approved one. Cancelled is not a denial, but the outcome builder
		// carries a two-way flag and the honest half of it is that access was
		// not granted; the wording comes from the labels above. Giving the base
		// card a third treatment would change a shared L0 contract for a
		// display nuance, which does not belong in this fix.
		Title: input.Document.Title, Variant: variant, Denied: state != StateApproved,
		HeaderLabel: labels.header, SourceName: input.Document.SourceName,
		StatusLabel: labels.status, PermissionLabel: input.Permission.Label,
		PermissionRoleLabel: input.Permission.RoleLabel,
		ReasonLabel:         labels.reason, Reason: input.Decision.RejectionReason,
		Actor: input.Requester.Name, ActorAvatar: input.Requester.AvatarURL,
		RequesterSpaceName: input.Requester.SourceSpaceName, RequestedBotNames: input.RequestedBotNames,
		RequestedAtDisplay: input.RequestedAtDisplay, RequestReason: input.RequestReason,
		BannerSuffix: labels.banner(input.Permission.RoleLabel), RoleLabel: labels.role,
		RequestReasonLabel:        labels.requestReason,
		MessageTimeDisplay:        decidedAtDisplay,
		DecisionOperatorName:      input.Decision.OperatorName,
		DecisionOperatorSpaceName: input.Decision.OperatorSpaceName,
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
	if state != StateApproved && state != StateRejected &&
		state != StateCancelled && state != StateUnavailable {
		return "", fmt.Errorf("docs.access-request@0.3.0: unsupported fallback state %q", state)
	}
	var input v3Fields
	if err := json.Unmarshal(fields, &input); err != nil {
		return "", err
	}
	labels := resultLabels(lang, state == StateRejected)
	switch state {
	case StateCancelled:
		labels = cancelledLabels(lang)
	case StateUnavailable:
		labels = unavailableLabels(lang)
	}
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

// unavailableLabels describes an action that reached no decision at all — the
// finalizer's catch-all when a route answers something other than a decision.
func unavailableLabels(lang string) resultLabelSet {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return resultLabelSet{
			header: "文档申请", status: "暂不可用", result: "本次访问申请暂时无法处理，权限未发生变更。",
			reason: "拒绝原因", role: "申请人", requestReason: "申请原因", processedAt: "处理于", zh: true,
		}
	}
	return resultLabelSet{
		header: "Document access", status: "Unavailable", result: "The access request could not be processed; no permission changed.",
		reason: "Reason", role: "Requester", requestReason: "Reason", processedAt: "Processed at",
	}
}

// cancelledLabels describes a request that ended without a decision. It reuses
// resultLabelSet so the outcome body needs no new shape; the reason row stays
// empty because a cancelled request has no operator and no rejection reason.
func cancelledLabels(lang string) resultLabelSet {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return resultLabelSet{
			header: "文档申请", status: "已取消", result: "本次访问申请已取消，权限未发生变更。",
			reason: "拒绝原因", role: "申请人", requestReason: "申请原因", processedAt: "处理于", zh: true,
		}
	}
	return resultLabelSet{
		header: "Document access", status: "Cancelled", result: "The access request was cancelled; no permission changed.",
		reason: "Reason", role: "Requester", requestReason: "Reason", processedAt: "Processed at",
	}
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
