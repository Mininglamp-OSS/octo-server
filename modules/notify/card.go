package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

// deliverCardNotification is the summary-notify card path. It mirrors
// deliverNotification (dedup → actor exclusion → live member verification →
// bounded fan-out) but delivers an octo/v1 card through the producer-bound
// carddispatch.Sender. If a card cannot be built (feature disabled, sender
// unavailable, or template/config error such as a non-https deep-link origin),
// the whole request degrades once to the plain-text DM so a summary
// notification is never silently lost. Per-recipient dispatch errors
// (busy/dispatch_failed/target_denied) surface in Filtered so the caller's
// existing retry/dedup state machine handles them, exactly as the text path.
func (n *Notify) deliverCardNotification(req *NotifyReq) (*NotifyResp, error) {
	if req == nil || req.Card == nil {
		return nil, errNotifyCardInvalid
	}
	card := req.Card
	if strings.TrimSpace(req.SpaceID) == "" || len(req.Targets) == 0 || len(req.Targets) > 200 {
		return nil, errNotifyCardInvalid
	}
	if strings.TrimSpace(card.TaskNo) == "" || strings.TrimSpace(card.Title) == "" {
		return nil, errNotifyCardInvalid
	}
	if card.Kind != SummaryCardKindCompleted && card.Kind != SummaryCardKindFailed {
		return nil, errNotifyCardInvalid
	}

	// Dedup + actor exclusion (same as the text path).
	targets := dedupTargets(req.Targets)
	if req.ActorUID != "" {
		tmp := make([]string, 0, len(targets))
		for _, uid := range targets {
			if uid != req.ActorUID {
				tmp = append(tmp, uid)
			}
		}
		targets = tmp
	}

	// Live membership verification. The dispatcher independently re-verifies
	// Space/target (Decision 3); this pre-filter mirrors the text path so a
	// non-member never reaches transport and is reported in Filtered.
	members, filteredMap, err := n.memberCache.verify(n.db, req.SpaceID, targets)
	if err != nil {
		return nil, fmt.Errorf("member verification failed: %w", err)
	}
	if len(members) == 0 {
		return &NotifyResp{Delivered: []string{}, Filtered: filteredMap}, nil
	}

	// The card and its text fallback reuse the existing notification bot so the
	// user sees one system DM conversation. Retry if provisioning is not ready.
	n.ensureNotifyBotReady()
	if !n.botOK.Load() {
		return nil, errors.New("notification bot unavailable")
	}

	// Deployment default outbound language (no per-request negotiation on the
	// internal ingress); mirrors the email/botfather outbound discipline.
	lang := i18n.OutboundLanguage(context.Background())

	// Decide card vs text once, up front. A build failure degrades the entire
	// request to text rather than dropping the notification.
	canCard := n.cardSender != nil && cardmsg.Enabled()
	var document json.RawMessage
	if canCard {
		doc, buildErr := n.buildSummaryCard(context.Background(), req.SpaceID, card, lang)
		if buildErr != nil {
			n.Warn("build summary card failed, degrading to text",
				zap.Error(buildErr), zap.String("space_id", req.SpaceID), zap.String("task_no", card.TaskNo))
			canCard = false
		} else {
			document = doc
		}
	}
	fallbackText := ""
	if !canCard {
		fallbackText = buildSummaryFallbackText(card, lang)
	}

	type sendResult struct {
		uid    string
		reason string // empty => delivered
	}
	resultCh := make(chan sendResult, len(members))
	sem := make(chan struct{}, 20)

	for _, targetUID := range members {
		sem <- struct{}{}
		go func(uid string) {
			defer func() { <-sem }()
			reason := ""
			if canCard {
				_, sendErr := n.cardSender.Send(
					context.Background(),
					carddispatch.Target{
						SpaceID:     req.SpaceID,
						ChannelID:   uid,
						ChannelType: common.ChannelTypePerson.Uint8(),
					},
					carddispatch.Card{Profile: cardmsg.ProfileV1, Document: document},
				)
				if sendErr != nil {
					reason = string(carddispatch.CategoryOf(sendErr))
					n.Warn("投递总结卡片失败",
						zap.String("target", uid), zap.String("space_id", req.SpaceID),
						zap.String("category", reason), zap.Error(sendErr))
				}
			} else if txtErr := n.sendSummaryText(uid, req.SpaceID, fallbackText); txtErr != nil {
				reason = "send_failed"
				n.Warn("投递总结文本失败",
					zap.String("target", uid), zap.String("space_id", req.SpaceID), zap.Error(txtErr))
			}
			resultCh <- sendResult{uid: uid, reason: reason}
		}(targetUID)
	}

	delivered := make([]string, 0, len(members))
	for range members {
		r := <-resultCh
		if r.reason == "" {
			delivered = append(delivered, r.uid)
		} else {
			filteredMap[r.uid] = r.reason
		}
	}

	n.Info("notify_card_delivered",
		zap.String("service", req.Service),
		zap.String("space_id", req.SpaceID),
		zap.String("task_no", card.TaskNo),
		zap.String("kind", card.Kind),
		zap.Bool("as_card", canCard),
		zap.Int("targets", len(req.Targets)),
		zap.Int("delivered", len(delivered)),
		zap.Int("filtered", len(filteredMap)),
	)

	return &NotifyResp{Delivered: delivered, Filtered: filteredMap}, nil
}

// buildSummaryCard renders the octo/v1 ResourceCard document for a summary
// notification. Labels/layout/deep-link are owned here (per contract); the deep
// link origin comes from External.WebLoginURL and must be https (cardtmpl
// enforces it — a non-https origin returns an error and triggers text fallback).
func (n *Notify) buildSummaryCard(ctx context.Context, spaceID string, card *SummaryCardFields, lang string) (json.RawMessage, error) {
	labels := summaryLabelsFor(lang)
	facts := make([]cardtmpl.Fact, 0, 4)
	if tr := strings.TrimSpace(card.TimeRange); tr != "" {
		facts = append(facts, cardtmpl.Fact{Title: labels.timeRange, Value: tr})
	}
	if card.Members > 0 {
		facts = append(facts, cardtmpl.Fact{Title: labels.members, Value: fmt.Sprintf(labels.membersValue, card.Members)})
	}
	if card.MsgCount > 0 {
		facts = append(facts, cardtmpl.Fact{Title: labels.msgCount, Value: fmt.Sprintf(labels.msgCountValue, card.MsgCount)})
	}
	if gen := strings.TrimSpace(card.GeneratedAt); gen != "" {
		facts = append(facts, cardtmpl.Fact{Title: labels.generatedAt, Value: gen})
	}

	attribution := labels.completedBanner
	excerpt := ""
	if card.Kind == SummaryCardKindFailed {
		attribution = labels.failedBanner
		if reason := strings.TrimSpace(card.Reason); reason != "" {
			excerpt = labels.failedPrefix + reason
		}
	}

	webLoginURL := n.ctx.GetConfig().External.WebLoginURL
	return cardtmpl.BuildSummaryResourceCard(ctx, webLoginURL, card.TaskNo, spaceID, cardtmpl.ResourceCard{
		Title:       card.Title,
		Attribution: attribution,
		Excerpt:     excerpt,
		Facts:       facts,
	})
}

// sendSummaryText delivers the plain-text fallback DM from the notification bot,
// reusing the same PERSONAL builder + space_id injection as the text path.
func (n *Notify) sendSummaryText(uid, spaceID, text string) error {
	payload := map[string]interface{}{"type": 1, "content": text}
	return n.ctx.SendMessage(config.NewPersonalMsgSendReq(
		uid,
		NotifyBotUIDValue,
		payload,
		spaceID,
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}},
	))
}

// buildSummaryFallbackText composes the plain-text DM used when a card cannot be
// built. It mirrors the fields the card would carry so no information is lost.
func buildSummaryFallbackText(card *SummaryCardFields, lang string) string {
	labels := summaryLabelsFor(lang)
	var b strings.Builder
	title := strings.TrimSpace(card.Title)
	if card.Kind == SummaryCardKindFailed {
		fmt.Fprintf(&b, labels.failedHeadline, title)
		if reason := strings.TrimSpace(card.Reason); reason != "" {
			fmt.Fprintf(&b, "\n%s%s", labels.failedPrefix, reason)
		}
		return b.String()
	}
	fmt.Fprintf(&b, labels.completedHeadline, title)
	if tr := strings.TrimSpace(card.TimeRange); tr != "" {
		fmt.Fprintf(&b, "\n%s%s", labels.timeRange+labels.kvSep, tr)
	}
	if card.Members > 0 {
		fmt.Fprintf(&b, "\n%s"+labels.membersValue, labels.members+labels.kvSep, card.Members)
	}
	if card.MsgCount > 0 {
		fmt.Fprintf(&b, "\n%s"+labels.msgCountValue, labels.msgCount+labels.kvSep, card.MsgCount)
	}
	if gen := strings.TrimSpace(card.GeneratedAt); gen != "" {
		fmt.Fprintf(&b, "\n%s%s", labels.generatedAt+labels.kvSep, gen)
	}
	return b.String()
}

type summaryLabels struct {
	completedBanner   string
	failedBanner      string
	timeRange         string
	members           string
	membersValue      string // fmt verb for the count, e.g. "%d 人"
	msgCount          string
	msgCountValue     string // e.g. "%d 条"
	generatedAt       string
	failedPrefix      string
	kvSep             string
	completedHeadline string // fmt verb for the title, used by the text fallback
	failedHeadline    string
}

func summaryLabelsFor(lang string) summaryLabels {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return summaryLabels{
			completedBanner:   "总结已生成完成",
			failedBanner:      "总结生成失败",
			timeRange:         "时间范围",
			members:           "参与成员",
			membersValue:      "%d 人",
			msgCount:          "消息数量",
			msgCountValue:     "%d 条",
			generatedAt:       "生成时间",
			failedPrefix:      "失败原因：",
			kvSep:             "：",
			completedHeadline: "你的总结「%s」已生成完成。",
			failedHeadline:    "你的总结「%s」生成失败。",
		}
	}
	return summaryLabels{
		completedBanner:   "Summary ready",
		failedBanner:      "Summary failed",
		timeRange:         "Time range",
		members:           "Participants",
		membersValue:      "%d",
		msgCount:          "Messages",
		msgCountValue:     "%d",
		generatedAt:       "Generated at",
		failedPrefix:      "Reason: ",
		kvSep:             ": ",
		completedHeadline: "Your summary \"%s\" is ready.",
		failedHeadline:    "Your summary \"%s\" failed to generate.",
	}
}
