package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docscommented "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_commented"
	docsshared "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_shared"
	summarycompleted "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_completed"
	summaryfailed "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_failed"
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
// Ordinary card-build failures retain the legacy text fallback; runtime-catalog
// safety rejections (blocked/disabled/auth/integrity/unavailable) fail closed.
// spaceIDAcceptable rejects a Space the card boundary would later refuse.
//
// Blank was already rejected here; untrimmed was not, and after PR-C D3 that
// difference is visible to callers. The marker-authoring boundary in
// internal/carddispatch validates the Space before stamping it and fails the
// send, so an untrimmed value used to fan out one goroutine per target and
// come back as HTTP 200 with every target failed as `card_invalid`, N
// identical warn lines, and nothing naming the cause. Catching it here turns
// that into one accurate 400.
//
// This is validation, not normalisation: the value is still never rewritten.
// Trimming it silently would put the caller's card in a Space they did not
// name, which is the failure the strict reader exists to prevent.
func spaceIDAcceptable(spaceID string) bool {
	return spaceID != "" && strings.TrimSpace(spaceID) == spaceID
}

func (n *Notify) deliverCardNotification(ctx context.Context, req *NotifyReq) (*NotifyResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil || req.Card == nil {
		return nil, errNotifyCardInvalid
	}
	card := req.Card
	if !spaceIDAcceptable(req.SpaceID) || len(req.Targets) == 0 || len(req.Targets) > 200 {
		return nil, errNotifyCardInvalid
	}
	if strings.TrimSpace(card.TaskNo) == "" || strings.TrimSpace(card.Title) == "" {
		return nil, errNotifyCardInvalid
	}
	if card.Kind != SummaryCardKindCompleted && card.Kind != SummaryCardKindFailed {
		return nil, errNotifyCardInvalid
	}

	// C1 policy (docs/platform-card-base.md §10):
	// Schema-level field errors are caller contract violations — they must reject
	// the whole request with 400 zero delivery, and be observable at ingress.
	// Running schema check here (before memberCache, before cardSender gate,
	// before cardmsg.Enabled) guarantees the check cannot be silently skipped by:
	//   (a) an empty-member request (200 with delivered=[]),
	//   (b) cardSender==nil / cardmsg.Enabled()==false (text degradation).
	// Preflight mirrors the docs-notify migration (roadmap C PR-1); same F7
	// fail-close for wiring bugs (Registry unwired → 500, not silent text).
	var summaryTid cardtmpl.ID
	if card.Kind == SummaryCardKindCompleted {
		summaryTid = summarycompleted.TemplateID
	} else {
		summaryTid = summaryfailed.TemplateID
	}
	if err := preflightSummarySchema(ctx, req.SpaceID, card, summaryTid); err != nil {
		if failure, ok := classifyNotifyCatalogRuntimeFailure(err); ok {
			n.Error("summary card preflight rejected by runtime catalog",
				zap.String("catalog_result", failure.result), zap.Error(err),
				zap.String("space_id", req.SpaceID), zap.String("task_no", card.TaskNo),
				zap.String("kind", card.Kind))
			return nil, err
		}
		if errors.Is(err, errCardTmplUnavailable) {
			n.Error("summary card preflight: cardtmpl unavailable (composition bug, 500)",
				zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("task_no", card.TaskNo),
				zap.String("kind", card.Kind))
			return nil, err // api 层兜底走 500
		}
		n.Warn("summary card rejected at preflight: schema (400)",
			zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("task_no", card.TaskNo),
			zap.String("kind", card.Kind))
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
	//
	// F7 (roadmap C PR-2): Registry unwired = composition bug = 500, no legacy
	// fallback. schema 违规 (typed cardtmpl.ErrFieldsInvalid) 已在 preflight 上游
	// 提前挡回 400 零投递,这里 buildErr non-typed 才走 render_error 降级为文本 DM。
	canCard := n.cardSender != nil && cardmsg.Enabled()
	renderProfile := ""
	var document json.RawMessage
	if canCard {
		var (
			doc      json.RawMessage
			buildErr error
		)
		if card.Kind == SummaryCardKindCompleted {
			doc, renderProfile, buildErr = n.buildSummaryCardViaRegistry(
				ctx, req.SpaceID, card, lang,
				summarycompleted.TemplateID, summarycompleted.StateShown)
		} else {
			doc, renderProfile, buildErr = n.buildSummaryCardViaRegistry(
				ctx, req.SpaceID, card, lang,
				summaryfailed.TemplateID, summaryfailed.StateShown)
		}
		if buildErr != nil {
			if failure, ok := classifyNotifyCatalogRuntimeFailure(buildErr); ok {
				n.Error("summary card rejected by runtime catalog",
					zap.String("catalog_result", failure.result), zap.Error(buildErr),
					zap.String("space_id", req.SpaceID), zap.String("task_no", card.TaskNo),
					zap.String("kind", card.Kind))
				return nil, buildErr
			}
			// C1: schema 违规理论上已被 preflight 拦掉,这里再 typed 断言一次
			// 兜底(避免 Build 内部新增校验后绕过 preflight)。
			if errors.Is(buildErr, cardtmpl.ErrFieldsInvalid) {
				n.Warn("summary card rejected: fields did not pass schema (400)",
					zap.Error(buildErr), zap.String("space_id", req.SpaceID), zap.String("task_no", card.TaskNo),
					zap.String("kind", card.Kind))
				return nil, errNotifyCardInvalid
			}
			if errors.Is(buildErr, errCardTmplUnavailable) {
				n.Error("summary card: cardtmpl unavailable (composition bug, 500)",
					zap.Error(buildErr), zap.String("space_id", req.SpaceID), zap.String("task_no", card.TaskNo),
					zap.String("kind", card.Kind))
				return nil, buildErr
			}
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
					carddispatch.Card{Profile: cardmsg.ProfileV1, RenderProfile: renderProfile, Document: document},
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
	variant := "summary.completed"
	if card.Kind == SummaryCardKindFailed {
		attribution = labels.failedBanner
		variant = "summary.failed"
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
		Variant:     variant,
		Source:      cardtmpl.Source{Label: labels.sourceLabel},
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

// sanitizeLine 委托 pkg/cardtmpl.SanitizeLine —— 与 L2a Template FallbackText
// 共用单一实现,消除词表漂移(G5, roadmap C)。保留本包 wrapper 避免大范围改
// 调用点。
func sanitizeLine(s string) string {
	return cardtmpl.SanitizeLine(s)
}

// buildSummaryFallbackText composes the plain-text DM used when a card cannot be
// built. It mirrors the fields the card would carry so no information is lost.
// Caller fields are sanitizeLine'd so a newline can't inject a spoofed line.
func buildSummaryFallbackText(card *SummaryCardFields, lang string) string {
	labels := summaryLabelsFor(lang)
	var b strings.Builder
	title := sanitizeLine(card.Title)
	if card.Kind == SummaryCardKindFailed {
		fmt.Fprintf(&b, labels.failedHeadline, title)
		if reason := sanitizeLine(card.Reason); reason != "" {
			fmt.Fprintf(&b, "\n%s%s", labels.failedPrefix, reason)
		}
		return b.String()
	}
	fmt.Fprintf(&b, labels.completedHeadline, title)
	if tr := sanitizeLine(card.TimeRange); tr != "" {
		fmt.Fprintf(&b, "\n%s%s", labels.timeRange+labels.kvSep, tr)
	}
	if card.Members > 0 {
		fmt.Fprintf(&b, "\n%s"+labels.membersValue, labels.members+labels.kvSep, card.Members)
	}
	if card.MsgCount > 0 {
		fmt.Fprintf(&b, "\n%s"+labels.msgCountValue, labels.msgCount+labels.kvSep, card.MsgCount)
	}
	if gen := sanitizeLine(card.GeneratedAt); gen != "" {
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
	sourceLabel       string // ResourceCard.Source.Label — "智能总结" / "Smart Summary"
}

// deliverDocsCardNotification is the docs-notify card path. Structurally it
// mirrors deliverCardNotification (dedup -> actor exclusion -> live member
// verification -> bounded fan-out) but binds to the docs-notify producer and
// uses BuildDocsResourceCard for the /d/{doc_id}?sp={space_id} deep link. A
// build failure degrades the whole request to a plain-text DM so a docs
// notification is never silently lost. Runtime-catalog safety rejections are
// excluded from that fallback and fail closed so an emergency block cannot be
// converted into a successful text delivery.
func (n *Notify) deliverDocsCardNotification(ctx context.Context, req *NotifyReq) (*NotifyResp, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil || req.DocsCard == nil {
		return nil, errNotifyCardInvalid
	}
	card := req.DocsCard
	if !spaceIDAcceptable(req.SpaceID) || len(req.Targets) == 0 || len(req.Targets) > 200 {
		return nil, errNotifyCardInvalid
	}
	if strings.TrimSpace(card.DocID) == "" || strings.TrimSpace(card.Title) == "" {
		return nil, errNotifyCardInvalid
	}
	if card.Kind != DocsCardKindShared && card.Kind != DocsCardKindCommented &&
		card.Kind != DocsCardKindAccessRequested && card.Kind != DocsCardKindAccessGranted &&
		card.Kind != DocsCardKindAccessDenied {
		return nil, errNotifyCardInvalid
	}
	if card.Kind == DocsCardKindAccessRequested && docsApprovalCardsEnabled() && strings.TrimSpace(card.RequestID) == "" {
		return nil, errNotifyCardInvalid
	}

	// C1 policy (docs/platform-card-base.md §10):
	// Schema-level field errors are caller contract violations — they must reject
	// the whole request with 400 zero delivery, and be observable at ingress.
	// Running schema check here (before memberCache, before docsSender gate,
	// before cardmsg.Enabled) guarantees the check cannot be silently skipped by:
	//   (a) an empty-member request (200 with delivered=[]),
	//   (b) docsSender==nil / cardmsg.Enabled()==false (text degradation).
	// Only the access_requested + gate-on branch flows through Registry.Render;
	// other kinds keep the historical validate-in-builder behavior.
	if card.Kind == DocsCardKindAccessRequested && docsApprovalCardsEnabled() {
		if err := preflightDocsAccessRequestSchema(ctx, req.SpaceID, card); err != nil {
			if failure, ok := classifyNotifyCatalogRuntimeFailure(err); ok {
				n.Error("docs access-request card preflight rejected by runtime catalog",
					zap.String("catalog_result", failure.result), zap.Error(err),
					zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID))
				return nil, err
			}
			// R2-2: preflight 现在能返 typed errCardTmplUnavailable (Registry/Template
			// 未注入,composition bug) —— 与 schema 400 分开处理,让 500 不降级。
			if errors.Is(err, errCardTmplUnavailable) {
				n.Error("docs access-request card preflight: cardtmpl unavailable (composition bug, 500)",
					zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID))
				return nil, err // api.go 兜底走 500
			}
			n.Warn("docs access-request card rejected at preflight: schema (400)",
				zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID))
			return nil, errNotifyCardInvalid
		}
	}
	// roadmap C:commented/shared 迁到 Registry 后同享 C1 preflight,避免 memberCache/
	// docsSender/gate 之后才发现 schema 错(空 members 会 200 delivered=[] 掩盖违规)。
	if card.Kind == DocsCardKindCommented || card.Kind == DocsCardKindShared {
		var tid cardtmpl.ID
		if card.Kind == DocsCardKindCommented {
			tid = docscommented.TemplateID
		} else {
			tid = docsshared.TemplateID
		}
		if err := preflightDocsDisplaySchema(ctx, req.SpaceID, card, tid); err != nil {
			if failure, ok := classifyNotifyCatalogRuntimeFailure(err); ok {
				n.Error("docs display card preflight rejected by runtime catalog",
					zap.String("catalog_result", failure.result), zap.Error(err),
					zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID),
					zap.String("kind", card.Kind))
				return nil, err
			}
			if errors.Is(err, errCardTmplUnavailable) {
				n.Error("docs display card preflight: cardtmpl unavailable (composition bug, 500)",
					zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID),
					zap.String("kind", card.Kind))
				return nil, err
			}
			n.Warn("docs display card rejected at preflight: schema (400)",
				zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID),
				zap.String("kind", card.Kind))
			return nil, errNotifyCardInvalid
		}
	}

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

	lang := i18n.OutboundLanguage(context.Background())

	// Fill the actor display name server-side when the producer sent only the
	// uid (empty ActorName + non-empty ActorUID). Best-effort; see hydrateActorName.
	n.hydrateActorName(card)

	canCard := n.docsSender != nil && cardmsg.Enabled()
	profile := cardmsg.ProfileV1
	renderProfile := ""
	var document json.RawMessage
	if canCard {
		var (
			doc      json.RawMessage
			buildErr error
		)
		if card.Kind == DocsCardKindAccessRequested && docsApprovalCardsEnabled() {
			profile = cardmsg.ProfileV2
			// F7: 唯一入口。Registry 未注入 = composition bug,不再 fallback 到 legacy
			// (那样会遮蔽 wiring 漏洞);普通 render_error 才降级文本,catalog safety
			// rejection 由下方分类器直接拒绝。
			doc, renderProfile, buildErr = n.buildDocsAccessRequestCardViaRegistry(ctx, req.SpaceID, card, lang)
		} else if card.Kind == DocsCardKindCommented {
			// roadmap C:docs.commented@0.1.0 走 Registry.Render(v1 展示卡)。
			// 与 access_requested 同 policy —— preflight 早跑 C1、Registry 未注入
			// 直接 500 不降级(F7)。字节等价 legacy buildDocsCard 由 body 共享
			// assembleResourceCardBody 保证。
			doc, renderProfile, buildErr = n.buildDocsDisplayCardViaRegistry(
				ctx, req.SpaceID, card, lang,
				docscommented.TemplateID, docscommented.StateShown)
		} else if card.Kind == DocsCardKindShared {
			// roadmap C:docs.shared@0.1.0 走 Registry.Render(与 commented 对称)。
			doc, renderProfile, buildErr = n.buildDocsDisplayCardViaRegistry(
				ctx, req.SpaceID, card, lang,
				docsshared.TemplateID, docsshared.StateShown)
		} else {
			doc, buildErr = n.buildDocsCard(context.Background(), req.SpaceID, card, lang)
		}
		if buildErr != nil {
			if failure, ok := classifyNotifyCatalogRuntimeFailure(buildErr); ok {
				n.Error("docs card rejected by runtime catalog",
					zap.String("catalog_result", failure.result), zap.Error(buildErr),
					zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID),
					zap.String("kind", card.Kind))
				return nil, buildErr
			}
			// C1 policy (docs/platform-card-base.md §10):
			// - schema-level field errors (typed cardtmpl.ErrFieldsInvalid) → 400,
			//   zero delivery. Reject the whole request instead of silently
			//   masking a caller contract violation as a plain-text DM.
			// - Registry/Template unavailable (typed errCardTmplUnavailable,
			//   composition bug) → propagate as internal error (500 from api.go
			//   layer),不降级 —— 静默走 text 会遮蔽 wiring 漏洞 (R2-2 / A14)。
			// - other build errors (render failure / marshal / dependency) →
			//   degrade to plain-text so the notification still lands.
			if errors.Is(buildErr, cardtmpl.ErrFieldsInvalid) {
				n.Warn("docs card rejected: fields did not pass schema (400)",
					zap.Error(buildErr), zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID),
					zap.String("kind", card.Kind))
				return nil, errNotifyCardInvalid
			}
			if errors.Is(buildErr, errCardTmplUnavailable) {
				n.Error("docs card: cardtmpl unavailable (composition bug, 500)",
					zap.Error(buildErr), zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID),
					zap.String("kind", card.Kind))
				return nil, buildErr // 不 wrap 为 errNotifyCardInvalid,api.go 兜底走 500
			}
			n.Warn("build docs card failed, degrading to text",
				zap.Error(buildErr), zap.String("space_id", req.SpaceID), zap.String("doc_id", card.DocID))
			canCard = false
		} else {
			document = doc
		}
	}
	fallbackText := ""
	if !canCard {
		fallbackText = buildDocsFallbackText(card, lang, req.SpaceID)
	}

	type sendResult struct {
		uid         string
		reason      string
		card        bool  // true when this send delivered a mutatable card (not text fallback)
		messageID   int64 // IM message id, only meaningful when card && reason == ""
		clientMsgNo string
	}
	resultCh := make(chan sendResult, len(members))
	sem := make(chan struct{}, 20)

	for _, targetUID := range members {
		sem <- struct{}{}
		go func(uid string) {
			defer func() { <-sem }()
			reason := ""
			var sent *carddispatch.Result
			if canCard {
				res, sendErr := n.docsSender.Send(
					context.Background(),
					carddispatch.Target{
						SpaceID:     req.SpaceID,
						ChannelID:   uid,
						ChannelType: common.ChannelTypePerson.Uint8(),
					},
					carddispatch.Card{Profile: profile, RenderProfile: renderProfile, Document: document},
				)
				if sendErr != nil {
					reason = string(carddispatch.CategoryOf(sendErr))
					n.Warn("投递文档卡片失败",
						zap.String("target", uid), zap.String("space_id", req.SpaceID),
						zap.String("category", reason), zap.Error(sendErr))
				} else {
					sent = res
				}
			} else if txtErr := n.sendSummaryText(uid, req.SpaceID, fallbackText); txtErr != nil {
				reason = "send_failed"
				n.Warn("投递文档文本失败",
					zap.String("target", uid), zap.String("space_id", req.SpaceID), zap.Error(txtErr))
			}
			sr := sendResult{uid: uid, reason: reason, card: canCard}
			if sent != nil {
				sr.messageID = sent.MessageID
				sr.clientMsgNo = sent.ClientMsgNo
			}
			resultCh <- sr
		}(targetUID)
	}

	delivered := make([]string, 0, len(members))
	// deliveredCards holds locators only for successfully delivered CARDS (the
	// text-fallback path has nothing to mutate later). Left empty/omitted when no
	// card was sent, keeping the response identical for legacy/text flows.
	deliveredCards := make([]DeliveredCard, 0, len(members))
	for range members {
		r := <-resultCh
		if r.reason == "" {
			delivered = append(delivered, r.uid)
			if r.card && r.messageID != 0 {
				deliveredCards = append(deliveredCards, DeliveredCard{
					UID:         r.uid,
					ChannelID:   r.uid,
					ChannelType: common.ChannelTypePerson.Uint8(),
					MessageID:   strconv.FormatInt(r.messageID, 10),
					ClientMsgNo: r.clientMsgNo,
				})
			}
		} else {
			filteredMap[r.uid] = r.reason
		}
	}

	n.Info("notify_docs_card_delivered",
		zap.String("service", req.Service),
		zap.String("space_id", req.SpaceID),
		zap.String("doc_id", card.DocID),
		zap.String("kind", card.Kind),
		zap.Bool("as_card", canCard),
		zap.Int("targets", len(req.Targets)),
		zap.Int("delivered", len(delivered)),
		zap.Int("filtered", len(filteredMap)),
	)

	return &NotifyResp{Delivered: delivered, Filtered: filteredMap, DeliveredCards: deliveredCards}, nil
}

// hydrateActorName fills card.ActorName from card.ActorUID via the user service
// when the producer sent only the uid (empty name). octo-server is the identity
// authority, so this removes the producer's need for a user-lookup credential
// (e.g. docs-backend's short-lived session token). Best-effort and idempotent:
// a pre-resolved name is left untouched, and any lookup miss/error leaves the
// name empty so the card degrades to the anonymous banner exactly as before.
func (n *Notify) hydrateActorName(card *DocsCardFields) {
	if card == nil || strings.TrimSpace(card.ActorName) != "" {
		return
	}
	actorUID := strings.TrimSpace(card.ActorUID)
	if actorUID == "" || n.userService == nil {
		return
	}
	if resp, err := n.userService.GetUser(actorUID); err == nil && resp != nil {
		card.ActorName = resp.Name
	}
}

// hydrateActorName fills card.ActorName from card.ActorUID via the user service
// when the producer sent only the uid (empty name). octo-server is the identity
// authority, so this removes the producer's need for a user-lookup credential
// (e.g. docs-backend's short-lived session token). Best-effort and idempotent:
// a pre-resolved name is left untouched, and any lookup miss/error leaves the
// name empty so the card degrades to the anonymous banner exactly as before.
func (n *Notify) hydrateActorName(card *DocsCardFields) {
	if card == nil || strings.TrimSpace(card.ActorName) != "" {
		return
	}
	actorUID := strings.TrimSpace(card.ActorUID)
	if actorUID == "" || n.userService == nil {
		return
	}
	if resp, err := n.userService.GetUser(actorUID); err == nil && resp != nil {
		card.ActorName = resp.Name
	}
}

// buildDocsCard renders the octo/v1 ResourceCard for a docs-notify
// notification. Kind maps to Variant / Attribution deterministically; ActorName
// and UpdatedAt render as optional FactSet rows. Excerpt is the free-form
// preview / comment / access reason bounded by cardtmpl.MaxExcerptRunes.
func (n *Notify) buildDocsCard(ctx context.Context, spaceID string, card *DocsCardFields, lang string) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	facts := make([]cardtmpl.Fact, 0, 2)
	isOutcome := card.Kind == DocsCardKindAccessGranted || card.Kind == DocsCardKindAccessDenied
	if actor := strings.TrimSpace(card.ActorName); actor != "" && !isOutcome {
		facts = append(facts, cardtmpl.Fact{Title: labels.actor, Value: actor})
	}
	if ts := strings.TrimSpace(card.UpdatedAt); ts != "" {
		facts = append(facts, cardtmpl.Fact{Title: labels.updatedAt, Value: ts})
	}

	attribution, variant := docsAttributionAndVariant(card.Kind, card.ActorName, labels)

	webLoginURL := n.ctx.GetConfig().External.WebLoginURL
	return cardtmpl.BuildDocsResourceCard(ctx, webLoginURL, card.DocID, spaceID, cardtmpl.ResourceCard{
		Title:       card.Title,
		Attribution: attribution,
		Excerpt:     docsSafeExcerpt(card),
		Facts:       facts,
		Variant:     variant,
		Source:      cardtmpl.Source{Label: labels.sourceLabel},
	})
}

// buildDocsAccessRequestCard 是**迁移前的 legacy 构造路径,已不在生产链路上**:
// deliverDocsCardNotification 的 access_requested + gate 分支走
// buildDocsAccessRequestCardViaRegistry(Registry.Render 唯一入口)。本方法仅存留作
// 迁移基线 —— card_via_registry_baseline_test.go 用它产出 pre-migration 输出,
// 断言与 Registry.Render 字节等价;card_action_test.go 也引用它。请勿把它误当成
// 第二条活的生产渲染路径;真要改 access-request 卡的生产行为,改 pilot Template
// (pkg/cardtmpl/docs_access_request) 或 Registry.Render。
func (n *Notify) buildDocsAccessRequestCard(ctx context.Context, spaceID string, card *DocsCardFields, lang string) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	actor := strings.TrimSpace(card.ActorName)
	bannerSuffix := labels.requestBannerSuffix
	if actor == "" {
		bannerSuffix = labels.requestBannerAnon
	}
	return cardtmpl.BuildDocsAccessRequestCard(
		ctx,
		n.ctx.GetConfig().External.WebLoginURL,
		card.DocID,
		card.RequestID,
		spaceID,
		cardtmpl.DocsApprovalContent{
			Title:        card.Title,
			Actor:        actor,
			ActorAvatar:  strings.TrimSpace(card.ActorAvatarURL),
			Timestamp:    strings.TrimSpace(card.UpdatedAt),
			Reason:       strings.TrimSpace(card.Excerpt),
			Variant:      "docs.access_requested",
			Source:       cardtmpl.Source{Label: labels.sourceLabel},
			HeaderLabel:  labels.approvalHeader,
			StatusLabel:  labels.pendingStatus,
			BannerSuffix: bannerSuffix,
			RoleLabel:    labels.roleRequester,
			ReasonLabel:  labels.reasonLabel,
		},
		cardtmpl.ApprovalActions{ApproveTitle: labels.approve, DenyTitle: labels.deny},
	)
}

func docsApprovalCardsEnabled() bool {
	enabled, err := strconv.ParseBool(os.Getenv("OCTO_DOCS_APPROVAL_CARD_ENABLED"))
	return err == nil && enabled
}

// DocsApprovalCardsEnabled exposes the rollout decision to the composition
// root so producer capability and callback routing fail closed together.
func DocsApprovalCardsEnabled() bool { return docsApprovalCardsEnabled() }

func docsSafeExcerpt(card *DocsCardFields) string {
	if card == nil || card.Kind == DocsCardKindAccessGranted || card.Kind == DocsCardKindAccessDenied {
		return ""
	}
	return strings.TrimSpace(card.Excerpt)
}

// docsAttributionAndVariant maps DocsCard.Kind to a (localized attribution
// line, Variant string). When ActorName is present the attribution embeds it;
// otherwise a subject-less form is used ("Shared a document" / "文档已分享").
func docsAttributionAndVariant(kind, actorName string, labels docsLabels) (string, string) {
	actor := strings.TrimSpace(actorName)
	switch kind {
	case DocsCardKindAccessGranted:
		return labels.accessGrantedBanner, "docs.access_granted"
	case DocsCardKindAccessDenied:
		return labels.accessDeniedBanner, "docs.access_denied"
	case DocsCardKindCommented:
		if actor != "" {
			return fmt.Sprintf(labels.commentedBanner, actor), "docs.commented"
		}
		return labels.commentedBannerAnon, "docs.commented"
	case DocsCardKindAccessRequested:
		if actor != "" {
			return fmt.Sprintf(labels.accessRequestedBanner, actor), "docs.access_requested"
		}
		return labels.accessRequestedBannerAnon, "docs.access_requested"
	default: // DocsCardKindShared
		if actor != "" {
			return fmt.Sprintf(labels.sharedBanner, actor), "docs.shared"
		}
		return labels.sharedBannerAnon, "docs.shared"
	}
}

// buildDocsFallbackText mirrors buildSummaryFallbackText: composes the plain
// DM used when a card cannot be built (feature disabled, sender missing, or
// template/config error). No information is lost — every field that would
// appear on the card is emitted as a text line.
//
// F6: for the access_requested + gate-on + Registry-wired branch, delegate to
// pilot Template.FallbackText — that is the L0 authoritative fallback for the
// pilot Template. Other kinds keep the historical multi-line composition.
// If Template.FallbackText is unavailable (Registry unwired / mapping fails),
// fall back to the historical composition so callers never lose the text path.
// spaceID is the Space the accompanying send is authorized against. It is not
// decoration: template resolution became Space-aware in Slice 3, so resolving
// the fallback with an empty Space would authorize a different principal than
// the send this text belongs to — and the only visible symptom would be a card
// rendered from the dynamic template whose plain-text twin came from the legacy
// hardcoded labels.
func buildDocsFallbackText(card *DocsCardFields, lang, spaceID string) string {
	if card != nil && card.Kind == DocsCardKindAccessRequested && docsApprovalCardsEnabled() {
		if text, ok := templateFallbackText(card, lang, spaceID); ok {
			return text
		}
	}
	labels := docsLabelsFor(lang)
	// sanitizeLine the actor before it flows into the attribution line, and each
	// other caller field, so an embedded newline can't inject a spoofed line.
	actorName := card.ActorName
	if card.Kind == DocsCardKindAccessGranted || card.Kind == DocsCardKindAccessDenied {
		actorName = ""
	}
	attribution, _ := docsAttributionAndVariant(card.Kind, sanitizeLine(actorName), labels)
	var b strings.Builder
	b.WriteString(attribution)
	if title := sanitizeLine(card.Title); title != "" {
		fmt.Fprintf(&b, "\n%s%s", labels.title+labels.kvSep, title)
	}
	if excerpt := sanitizeLine(docsSafeExcerpt(card)); excerpt != "" {
		fmt.Fprintf(&b, "\n%s", excerpt)
	}
	if ts := sanitizeLine(card.UpdatedAt); ts != "" {
		fmt.Fprintf(&b, "\n%s%s", labels.updatedAt+labels.kvSep, ts)
	}
	return b.String()
}

type docsLabels struct {
	sharedBanner              string // "%s 分享了文档"
	sharedBannerAnon          string
	commentedBanner           string
	commentedBannerAnon       string
	accessRequestedBanner     string
	accessRequestedBannerAnon string
	accessGrantedBanner       string
	accessDeniedBanner        string
	accessCancelledBanner     string
	accessUnavailableBanner   string
	approve                   string
	deny                      string
	title                     string
	actor                     string
	updatedAt                 string
	kvSep                     string
	sourceLabel               string // ResourceCard.Source.Label — "文档" / "Docs"
	// Enriched access-request / outcome card copy (docs-approval-card-enrich).
	approvalHeader      string // header label — "文档申请" / "Document access"
	pendingStatus       string // "待你处理" / "Pending"
	approvedStatus      string // "已允许" / "Approved"
	deniedStatus        string // "已拒绝" / "Denied"
	requestBannerSuffix string // subtle sentence after the bold actor
	requestBannerAnon   string // subject-less banner when actor is unknown
	roleRequester       string // "申请人" / "Requester"
	reasonLabel         string // "申请原因" / "Reason"
	denyReasonLabel     string // "拒绝原因" / "Reason for denial"
	approvedResult      string // result-box copy on approval
	deniedResult        string // result-box copy on denial
	decisionActor       string // safe generic when callback omits operator display name
}

func docsLabelsFor(lang string) docsLabels {
	if strings.EqualFold(lang, "zh-CN") || strings.HasPrefix(strings.ToLower(lang), "zh") {
		return docsLabels{
			sharedBanner:              "%s 分享了文档",
			sharedBannerAnon:          "有人分享了文档",
			commentedBanner:           "%s 评论了文档",
			commentedBannerAnon:       "有新评论",
			accessRequestedBanner:     "%s 请求访问文档",
			accessRequestedBannerAnon: "有人请求访问文档",
			accessGrantedBanner:       "文档访问已获批准",
			accessDeniedBanner:        "访问申请已拒绝",
			accessCancelledBanner:     "访问申请已取消",
			accessUnavailableBanner:   "当前无法处理此访问申请",
			approve:                   "允许",
			deny:                      "拒绝",
			title:                     "文档",
			actor:                     "操作人",
			updatedAt:                 "时间",
			kvSep:                     "：",
			sourceLabel:               "文档",
			approvalHeader:            "文档申请",
			pendingStatus:             "待你处理",
			approvedStatus:            "已允许",
			deniedStatus:              "已拒绝",
			requestBannerSuffix:       "申请成为此文档的查看者。",
			requestBannerAnon:         "有人申请成为此文档的查看者。",
			roleRequester:             "申请人",
			reasonLabel:               "申请原因",
			denyReasonLabel:           "拒绝原因",
			approvedResult:            "申请人已获得所申请的文档权限。",
			deniedResult:              "申请已被拒绝。",
			decisionActor:             "审批人",
		}
	}
	return docsLabels{
		sharedBanner:              "%s shared a document",
		sharedBannerAnon:          "A document was shared with you",
		commentedBanner:           "%s commented on a document",
		commentedBannerAnon:       "A new comment on a document",
		accessRequestedBanner:     "%s requested access to a document",
		accessRequestedBannerAnon: "Someone requested access to a document",
		accessGrantedBanner:       "Document access granted",
		accessDeniedBanner:        "Document access request denied",
		accessCancelledBanner:     "Document access request cancelled",
		accessUnavailableBanner:   "This access request is no longer actionable",
		approve:                   "Allow",
		deny:                      "Deny",
		title:                     "Document",
		actor:                     "By",
		updatedAt:                 "At",
		kvSep:                     ": ",
		sourceLabel:               "Docs",
		approvalHeader:            "Document access",
		pendingStatus:             "Pending",
		approvedStatus:            "Approved",
		deniedStatus:              "Denied",
		requestBannerSuffix:       "is requesting viewer access to this document.",
		requestBannerAnon:         "Someone is requesting viewer access to this document.",
		roleRequester:             "Requester",
		reasonLabel:               "Reason",
		denyReasonLabel:           "Reason for denial",
		approvedResult:            "The requester now has the requested document access.",
		deniedResult:              "The access request was denied.",
		decisionActor:             "Reviewer",
	}
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
			sourceLabel:       "智能总结",
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
		sourceLabel:       "Smart Summary",
	}
}
