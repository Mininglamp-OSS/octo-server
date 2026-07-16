package notify

import (
	"context"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

type StandardActionFinalizer struct {
	mutator cardActionMutator
	sender  carddispatch.Sender
}

func NewStandardActionFinalizer(mutator cardActionMutator, sender carddispatch.Sender) (*StandardActionFinalizer, error) {
	if mutator == nil || sender == nil {
		return nil, errors.New("notify: standard action finalizer dependencies are required")
	}
	return &StandardActionFinalizer{mutator: mutator, sender: sender}, nil
}

func (f *StandardActionFinalizer) Finalize(ctx context.Context, event cardactiondispatch.Event, result cardactiondispatch.DecisionResult) error {
	if ctx == nil || event.EventID <= 0 || strings.TrimSpace(event.SenderUID) == "" ||
		strings.TrimSpace(event.MessageID) == "" || event.ChannelType == 0 || strings.TrimSpace(event.ChannelID) == "" ||
		strings.TrimSpace(event.SpaceID) == "" {
		return errors.New("notify: standard action result is missing authoritative context")
	}
	terminal := result.State == cardactiondispatch.StateApproved || result.State == cardactiondispatch.StateDenied
	if terminal && strings.TrimSpace(result.RequesterUID) == "" {
		return errors.New("notify: terminal standard action requires requester_uid")
	}
	labels := standardApprovalLabelsFor(i18n.OutboundLanguage(ctx))
	title := strings.TrimSpace(result.Display["title"])
	if title == "" {
		title, _ = event.Data["title"].(string)
	}
	if strings.TrimSpace(title) == "" {
		title = labels.title
	}
	status, variant := labels.unavailable, "approval.unavailable"
	switch result.State {
	case cardactiondispatch.StateApproved:
		status, variant = labels.approved, "approval.approved"
	case cardactiondispatch.StateDenied:
		status, variant = labels.denied, "approval.denied"
	case cardactiondispatch.StateCancelled:
		status, variant = labels.cancelled, "approval.cancelled"
	}
	document, err := cardtmpl.BuildApprovalResultCard(cardtmpl.ApprovalResultCard{
		Title: title, Status: status, Variant: variant, Source: labels.source,
	})
	if err != nil {
		return err
	}
	contentEdit, err := buildTerminalEnvelope(document, event.SpaceID, event.EventID)
	if err != nil {
		return err
	}
	channelID := event.ChannelID
	if event.ChannelType == common.ChannelTypePerson.Uint8() {
		if strings.TrimSpace(event.OperatorUID) == "" {
			return errors.New("notify: standard DM action is missing operator_uid")
		}
		channelID = event.OperatorUID
	}
	if _, err := f.mutator.Mutate(ctx, carddispatch.CardMutationRequest{
		SenderUID: event.SenderUID, MessageID: event.MessageID, ChannelID: channelID,
		ChannelType: event.ChannelType, ContentEdit: contentEdit,
	}); err != nil {
		return err
	}
	if !terminal {
		return nil
	}
	_, err = f.sender.Send(ctx, carddispatch.Target{
		SpaceID: event.SpaceID, ChannelID: result.RequesterUID, ChannelType: common.ChannelTypePerson.Uint8(),
	}, carddispatch.Card{Profile: cardmsg.ProfileV1, Document: document})
	if err != nil {
		return &actionFinalizeError{category: "applicant_notify_failed", err: err}
	}
	return nil
}

type standardApprovalLabels struct {
	title       string
	approved    string
	denied      string
	cancelled   string
	unavailable string
	source      string
}

func standardApprovalLabelsFor(lang string) standardApprovalLabels {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return standardApprovalLabels{
			title: "审批请求", approved: "申请已允许", denied: "申请已拒绝",
			cancelled: "申请已取消", unavailable: "申请暂不可用", source: "审批",
		}
	}
	return standardApprovalLabels{
		title: "Approval request", approved: "Request approved", denied: "Request denied",
		cancelled: "Request cancelled", unavailable: "Request unavailable", source: "Approval",
	}
}
