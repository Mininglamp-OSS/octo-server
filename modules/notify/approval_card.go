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

// prepareApprovalCard runs every ApprovalCard check that does NOT depend on the
// recipient set, and returns the rendered card document.
//
// It exists so those checks can run BEFORE role-targeted recipient resolution
// (sendNotify) as well as during delivery, from one definition. Splitting them
// out matters because resolveRoleTargets short-circuits a zero-admin Space as a
// 200; if validation lived only downstream of that, a malformed card would be
// rejected or accepted depending on the membership of the Space it names.
//
// The rendered document doubles as the schema check — cardtmpl.Build… is where
// an over-long action list or an empty custom-actions slice is caught — which is
// why the build happens here rather than after memberCache.verify as it used to.
// That ordering is the same C1 policy the summary and docs card paths already
// follow (docs/platform-card-base.md §10): a caller contract violation must be a
// 400 with zero delivery, and must not be silently skipped by a request whose
// recipients all turn out to be non-members.
//
// Deliberately NOT checked here: len(req.Targets) == 0. On the role path the
// slice is still empty at pre-validation time (resolution has not run yet), so
// the empty check belongs to the caller that knows whether resolution has
// happened. The upper bound IS checked, because exceeding it is a caller error
// on the explicit-targets path and unreachable on the role path (resolution
// truncates at the same constant).
func (n *Notify) prepareApprovalCard(req *NotifyReq, capability cardactiondispatch.NotifyCapability) ([]byte, error) {
	if req == nil || req.ApprovalCard == nil || n.actionService == nil ||
		!n.actionService.CanNotify(capability, req.ApprovalCard.ActionType) {
		return nil, errNotifyCardNotAllowed
	}
	card := req.ApprovalCard
	if !spaceIDAcceptable(req.SpaceID) || len(req.Targets) > maxNotifyTargets ||
		strings.TrimSpace(card.Title) == "" {
		return nil, errNotifyCardInvalid
	}
	if n.actionSenders[capability] == nil || !cardmsg.Enabled() {
		return nil, errors.New("notify: action card producer unavailable")
	}
	document, err := n.buildApprovalRequestCard(card, capability, i18n.OutboundLanguage(context.Background()))
	if err != nil {
		return nil, errNotifyCardInvalid
	}
	return document, nil
}

func (n *Notify) deliverApprovalCardNotification(req *NotifyReq, capability cardactiondispatch.NotifyCapability) (*NotifyResp, error) {
	document, err := n.prepareApprovalCard(req, capability)
	if err != nil {
		return nil, err
	}
	card := req.ApprovalCard
	if len(req.Targets) == 0 {
		return nil, errNotifyCardInvalid
	}
	sender := n.actionSenders[capability]
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
