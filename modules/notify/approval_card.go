package notify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

func (n *Notify) deliverApprovalCardNotification(req *NotifyReq, capability cardactiondispatch.NotifyCapability) (*NotifyResp, error) {
	if req == nil || req.ApprovalCard == nil || n.actionService == nil ||
		!n.actionService.CanNotify(capability, req.ApprovalCard.ActionType) {
		return nil, errNotifyCardNotAllowed
	}
	card := req.ApprovalCard
	if strings.TrimSpace(req.SpaceID) == "" || len(req.Targets) == 0 || len(req.Targets) > 200 ||
		strings.TrimSpace(card.Title) == "" {
		return nil, errNotifyCardInvalid
	}
	sender := n.actionSenders[capability]
	if sender == nil || !cardmsg.Enabled() {
		return nil, errors.New("notify: action card producer unavailable")
	}
	targets := dedupTargets(req.Targets)
	if req.ActorUID != "" {
		filtered := make([]string, 0, len(targets))
		for _, uid := range targets {
			if uid != req.ActorUID {
				filtered = append(filtered, uid)
			}
		}
		targets = filtered
	}
	members, filteredMap, err := n.memberCache.verify(n.db, req.SpaceID, targets)
	if err != nil {
		return nil, fmt.Errorf("member verification failed: %w", err)
	}
	if len(members) == 0 {
		return &NotifyResp{Delivered: []string{}, Filtered: filteredMap}, nil
	}
	n.ensureNotifyBotReady()
	if !n.botOK.Load() {
		return nil, errors.New("notification bot unavailable")
	}
	document, err := n.buildApprovalRequestCard(card, capability, i18n.OutboundLanguage(context.Background()))
	if err != nil {
		return nil, errNotifyCardInvalid
	}

	type sendResult struct {
		uid    string
		reason string
	}
	resultCh := make(chan sendResult, len(members))
	sem := make(chan struct{}, 20)
	for _, targetUID := range members {
		sem <- struct{}{}
		go func(uid string) {
			defer func() { <-sem }()
			reason := ""
			if _, sendErr := sender.Send(context.Background(), carddispatch.Target{
				SpaceID: req.SpaceID, ChannelID: uid, ChannelType: common.ChannelTypePerson.Uint8(),
			}, carddispatch.Card{Profile: cardmsg.ProfileV2, Document: document}); sendErr != nil {
				reason = string(carddispatch.CategoryOf(sendErr))
				n.Warn("deliver action approval card failed", zap.String("owner", capability.Owner),
					zap.String("action_type", card.ActionType), zap.String("target", uid), zap.Error(sendErr))
			}
			resultCh <- sendResult{uid: uid, reason: reason}
		}(targetUID)
	}
	delivered := make([]string, 0, len(members))
	for range members {
		result := <-resultCh
		if result.reason == "" {
			delivered = append(delivered, result.uid)
		} else {
			filteredMap[result.uid] = result.reason
		}
	}
	return &NotifyResp{Delivered: delivered, Filtered: filteredMap}, nil
}

func (n *Notify) buildApprovalRequestCard(card *ApprovalCardFields, capability cardactiondispatch.NotifyCapability, lang string) ([]byte, error) {
	if card == nil {
		return nil, errNotifyCardInvalid
	}
	tmpl := cardtmpl.ApprovalRequestCard{
		Title: card.Title, Description: card.Description, Owner: capability.Owner,
		ActionType: card.ActionType, Data: card.Data,
	}
	// nil = caller omitted the field → server-owned localized approve/deny.
	// Non-nil (including explicit []) enters the custom path so cardtmpl can
	// reject the empty slice as a caller bug instead of silently falling back.
	if card.Actions == nil {
		labels := approvalActionLabelsFor(lang)
		tmpl.ApproveTitle = labels.approve
		tmpl.DenyTitle = labels.deny
	} else {
		actions := make([]cardtmpl.ApprovalRequestAction, 0, len(card.Actions))
		for _, a := range card.Actions {
			actions = append(actions, cardtmpl.ApprovalRequestAction{
				Decision: a.Decision, Title: a.Title,
			})
		}
		tmpl.Actions = actions
	}
	return cardtmpl.BuildApprovalRequestCard(tmpl)
}

type approvalActionLabels struct {
	approve string
	deny    string
}

func approvalActionLabelsFor(lang string) approvalActionLabels {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return approvalActionLabels{approve: "允许", deny: "拒绝"}
	}
	return approvalActionLabels{approve: "Allow", deny: "Deny"}
}
