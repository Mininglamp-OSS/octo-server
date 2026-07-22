package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

type cardActionMutator interface {
	Mutate(context.Context, carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error)
}

type DocsActionFinalizer struct {
	ctx     *config.Context
	mutator cardActionMutator
	updater cardtmpl.CardUpdater
	sender  carddispatch.Sender
}

type actionFinalizeError struct {
	category string
	err      error
}

func (e *actionFinalizeError) Error() string    { return "notify: " + e.category }
func (e *actionFinalizeError) Unwrap() error    { return e.err }
func (e *actionFinalizeError) Category() string { return e.category }

func NewDocsActionFinalizer(ctx *config.Context, mutator cardActionMutator, sender carddispatch.Sender) (*DocsActionFinalizer, error) {
	if ctx == nil || mutator == nil || sender == nil {
		return nil, errors.New("notify: docs action finalizer dependencies are required")
	}
	return &DocsActionFinalizer{ctx: ctx, mutator: mutator, sender: sender}, nil
}

func NewDocsActionFinalizerWithUpdater(ctx *config.Context, updater cardtmpl.CardUpdater, mutator cardActionMutator, sender carddispatch.Sender) (*DocsActionFinalizer, error) {
	if ctx == nil || updater == nil || mutator == nil || sender == nil {
		return nil, errors.New("notify: docs action finalizer dependencies are required")
	}
	return &DocsActionFinalizer{ctx: ctx, updater: updater, mutator: mutator, sender: sender}, nil
}

func NewDocsActionFinalizerFromContext(ctx *config.Context) (*DocsActionFinalizer, error) {
	sender, err := carddispatch.SenderFromContext(ctx, docsNotifyProducerID)
	if err != nil {
		return nil, err
	}
	mutator := carddispatch.NewCardMutator(ctx)
	updater, err := cardtmpl.NewCardUpdater(cardtmpl.DefaultRegistry(), mutator)
	if err != nil {
		return nil, err
	}
	return NewDocsActionFinalizerWithUpdater(ctx, updater, mutator, sender)
}

func (f *DocsActionFinalizer) Finalize(ctx context.Context, event cardactiondispatch.Event, result cardactiondispatch.DecisionResult) error {
	if event.Owner != "docs" || event.ActionType != "access_request.decision" || event.EventID <= 0 {
		return errors.New("notify: unsupported card action result")
	}
	docID, _ := event.Data["doc_id"].(string)
	if strings.TrimSpace(docID) == "" || strings.TrimSpace(event.SpaceID) == "" {
		return errors.New("notify: docs action result is missing authoritative context")
	}
	if (result.State == cardactiondispatch.StateApproved || result.State == cardactiondispatch.StateDenied) &&
		strings.TrimSpace(result.RequesterUID) == "" {
		return errors.New("notify: terminal docs decision is missing requester_uid")
	}
	channelID := event.ChannelID
	if event.ChannelType == common.ChannelTypePerson.Uint8() {
		if strings.TrimSpace(event.OperatorUID) == "" {
			return errors.New("notify: docs DM action is missing operator_uid")
		}
		channelID = event.OperatorUID
	}
	lang := i18n.OutboundLanguage(ctx)
	title := strings.TrimSpace(result.Display["title"])
	if title == "" {
		title, _ = event.Data["doc_title"].(string)
	}
	if strings.TrimSpace(title) == "" {
		title = docID
	}
	// The reviewer deny reason (if any) rode in as a declared input; surface it on
	// the denied terminal card. Missing/blank is fine (approve, or no reason typed).
	denyReason := ""
	if v, ok := event.Inputs[cardtmpl.DocsDenyReasonInputID].(string); ok {
		denyReason = strings.TrimSpace(v)
	}
	terminal := result.State == cardactiondispatch.StateApproved || result.State == cardactiondispatch.StateDenied
	if terminal && f.updater != nil {
		if err := f.replaceWithRegistryResult(ctx, event, result, channelID, lang, title, denyReason); err != nil {
			return err
		}
	} else {
		terminalDocument, err := f.buildTerminalDocument(ctx, lang, docID, event.SpaceID, title, denyReason, result)
		if err != nil {
			return err
		}
		contentEdit, err := buildTerminalEnvelope(terminalDocument, event.SpaceID, event.EventID)
		if err != nil {
			return err
		}
		if _, err := f.mutator.Mutate(ctx, carddispatch.CardMutationRequest{
			SenderUID: event.SenderUID, MessageID: event.MessageID, ChannelID: channelID,
			ChannelType: event.ChannelType, ContentEdit: contentEdit,
		}); err != nil {
			return err
		}
	}
	if !terminal {
		return nil
	}
	outcomeDocument, err := f.buildOutcomeDocument(ctx, lang, docID, event.SpaceID, title, denyReason, result.State)
	if err != nil {
		return err
	}
	_, err = f.sender.Send(ctx, carddispatch.Target{
		SpaceID: event.SpaceID, ChannelID: result.RequesterUID, ChannelType: common.ChannelTypePerson.Uint8(),
	}, carddispatch.Card{Profile: cardmsg.ProfileV1, Document: outcomeDocument})
	if err != nil {
		return &actionFinalizeError{category: "applicant_notify_failed", err: err}
	}
	return nil
}

// 0.3.0 result view schema 的各字段 rune 上限(见
// handoff/docs.access-request@0.3.0/contract/data.schema.json)。回调 Display 与
// event.Data 的值合法可达 500 runes(cardactiondispatch 契约),必须在渲染前截断
// 到各自 schema cap —— 否则 RenderCard 的 InputSchema 校验返回 ErrFieldsInvalid,
// ReplaceView 确定性失败,审批卡永远停在 pending 且申请人漏通知(PR#641 review P1)。
// 截断而非编造,与 denyReason 既有做法一致。
const (
	capResultTitle      = 200                     // document.title / document.sourceName
	capResultName       = 120                     // requester.name / decision.operatorName
	capResultShortLabel = 80                      // decidedAt/requestedAt/messageTime/permission.*
	capResultReason     = cardtmpl.MaxExcerptRunes // requestReason / rejectionReason (= 300)
)

// truncRunes 按 rune 截断到 max(max<=0 视为不限)。
func truncRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

func (f *DocsActionFinalizer) replaceWithRegistryResult(ctx context.Context, event cardactiondispatch.Event,
	result cardactiondispatch.DecisionResult, channelID, lang, title, denyReason string) error {
	state := docsaccessrequest.StateApproved
	wireState := "approved"
	if result.State == cardactiondispatch.StateDenied {
		state = docsaccessrequest.StateRejected
		wireState = "rejected"
	}
	if runes := []rune(denyReason); len(runes) > capResultReason {
		denyReason = string(runes[:capResultReason])
	}
	requestID, _ := event.Data["request_id"].(string)
	actor, _ := event.Data["actor"].(string)
	operatorName := strings.TrimSpace(result.Display["operator_name"])
	if operatorName == "" {
		operatorName = event.OperatorUID
	}
	document := map[string]any{
		"docId": strings.TrimSpace(stringEventData(event.Data, "doc_id")),
		"title": truncRunes(strings.TrimSpace(title), capResultTitle),
	}
	requester := map[string]any{"name": truncRunes(strings.TrimSpace(actor), capResultName)}
	if value := strings.TrimSpace(stringEventData(event.Data, "source_name")); value != "" {
		document["sourceName"] = truncRunes(value, capResultTitle)
	}
	if value := strings.TrimSpace(stringEventData(event.Data, "actor_avatar_url")); value != "" {
		// URL 不截断:schema 无 maxLength(https 由渲染层校验),截断会破坏链接。
		requester["avatarUrl"] = value
	}
	fields := map[string]any{
		"requestId": strings.TrimSpace(requestID),
		"state":     wireState,
		"document":  document,
		"requester": requester,
		"decision": map[string]any{
			"operatorName":     truncRunes(operatorName, capResultName),
			"decidedAtDisplay": truncRunes(strings.TrimSpace(result.Display["decided_at"]), capResultShortLabel),
		},
	}
	dataCaps := map[string]int{
		"requestReason":      capResultReason,
		"requestedAtDisplay": capResultShortLabel,
		"messageTimeDisplay": capResultShortLabel,
	}
	for destination, source := range map[string]string{
		"requestReason": "request_reason", "requestedAtDisplay": "requested_at_display",
		"messageTimeDisplay": "message_time_display",
	} {
		if value := strings.TrimSpace(stringEventData(event.Data, source)); value != "" {
			fields[destination] = truncRunes(value, dataCaps[destination])
		}
	}
	permission := make(map[string]any, 2)
	if value := strings.TrimSpace(stringEventData(event.Data, "permission_label")); value != "" {
		permission["label"] = truncRunes(value, capResultShortLabel)
	}
	if value := strings.TrimSpace(stringEventData(event.Data, "permission_role_label")); value != "" {
		permission["roleLabel"] = truncRunes(value, capResultShortLabel)
	}
	if len(permission) > 0 {
		fields["permission"] = permission
	}
	if wireState == "rejected" && denyReason != "" {
		fields["decision"].(map[string]any)["rejectionReason"] = denyReason
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("notify: marshal registry result fields: %w", err)
	}
	return f.updater.ReplaceView(ctx, cardtmpl.UpdateTarget{
		Target: carddispatch.Target{
			SpaceID: event.SpaceID, ChannelID: channelID, ChannelType: event.ChannelType,
		},
		// card_seq 单调性来源:event.EventID 是全局单调递增的 snowflake,直接用作
		// card_seq 满足 CardMutator CAS 的严格递增要求(见
		// .octospec/learnings/pending/cardtmpl-interaction-closure.md)。若未来 event
		// ID 生成器改为非单调,CAS 会静默丢弃合法更新——务必同步复核。
		SenderUID: event.SenderUID, MessageID: event.MessageID, CardSeq: event.EventID,
	}, docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3, state, raw, cardtmpl.BuildEnv{
		WebLoginURL: f.ctx.GetConfig().External.WebLoginURL, Lang: lang, SpaceID: event.SpaceID,
	})
}

func stringEventData(data map[string]interface{}, key string) string {
	value, _ := data[key].(string)
	return value
}

func (f *DocsActionFinalizer) buildTerminalDocument(ctx context.Context, lang, docID, spaceID, title, denyReason string, result cardactiondispatch.DecisionResult) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	webLoginURL := f.ctx.GetConfig().External.WebLoginURL
	// Approved / denied get the enriched outcome card (header + title + result
	// box); the denied box surfaces the reviewer reason. Rendered through the
	// shared builder so the click-driven terminal card and the sibling cards the
	// access-decision card-sync endpoint mutates are byte-identical.
	switch result.State {
	case cardactiondispatch.StateApproved:
		return buildDocsDecisionTerminalDocument(ctx, webLoginURL, lang, docID, spaceID, title, denyReason, false)
	case cardactiondispatch.StateDenied:
		return buildDocsDecisionTerminalDocument(ctx, webLoginURL, lang, docID, spaceID, title, denyReason, true)
	}
	// Cancelled / unavailable are transient states without an enriched design —
	// keep the prior plain resource-card rebuild.
	attribution, variant := labels.accessUnavailableBanner, "docs.access_unavailable"
	if result.State == cardactiondispatch.StateCancelled {
		attribution, variant = labels.accessCancelledBanner, "docs.access_cancelled"
	}
	return cardtmpl.BuildDocsResourceCard(ctx, webLoginURL, docID, spaceID, cardtmpl.ResourceCard{
		Title: title, Attribution: attribution, Variant: variant, Source: cardtmpl.Source{Label: labels.sourceLabel},
	})
}

// buildDocsDecisionTerminalDocument renders the enriched approved/denied outcome
// card. Shared by the click-driven finalizer (buildTerminalDocument) and the
// access-decision card-sync endpoint (mutateDocsCard) so the terminal card a
// clicker sees and the sibling cards other approvers see are byte-identical.
func buildDocsDecisionTerminalDocument(ctx context.Context, webLoginURL, lang, docID, spaceID, title, denyReason string, denied bool) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	if denied {
		return cardtmpl.BuildDocsApprovalOutcomeCard(ctx, webLoginURL, docID, spaceID, cardtmpl.DocsOutcomeContent{
			Title: title, Variant: "docs.access_denied", Source: cardtmpl.Source{Label: labels.sourceLabel},
			Denied: true, HeaderLabel: labels.approvalHeader, StatusLabel: labels.deniedStatus,
			ResultText: labels.deniedResult, ReasonLabel: labels.denyReasonLabel, Reason: denyReason,
		})
	}
	return cardtmpl.BuildDocsApprovalOutcomeCard(ctx, webLoginURL, docID, spaceID, cardtmpl.DocsOutcomeContent{
		Title: title, Variant: "docs.access_approved", Source: cardtmpl.Source{Label: labels.sourceLabel},
		Denied: false, HeaderLabel: labels.approvalHeader, StatusLabel: labels.approvedStatus,
		ResultText: labels.approvedResult,
	})
}

func (f *DocsActionFinalizer) buildOutcomeDocument(ctx context.Context, lang, docID, spaceID, title, denyReason string, state cardactiondispatch.State) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	attribution := labels.accessGrantedBanner
	variant := "docs.access_granted"
	var facts []cardtmpl.Fact
	if state == cardactiondispatch.StateDenied {
		attribution, variant = labels.accessDeniedBanner, "docs.access_denied"
		// Surface the reviewer's reason to the applicant — a denial is useless to
		// them without it. Rides as a labeled FactSet row on the same resource
		// card (no profile/card-type change); omitted when no reason was typed.
		if reason := strings.TrimSpace(denyReason); reason != "" {
			// cardtmpl caps FactSet values at maxFactRunes; an over-long reason
			// fails the whole card build and drops the denial notification
			// entirely (retries included). Truncate to MaxExcerptRunes — the
			// same bound the terminal card's reason box uses — so a long reason
			// is trimmed, never silently lost.
			if r := []rune(reason); len(r) > cardtmpl.MaxExcerptRunes {
				reason = string(r[:cardtmpl.MaxExcerptRunes])
			}
			facts = append(facts, cardtmpl.Fact{Title: labels.denyReasonLabel, Value: reason})
		}
	}
	return cardtmpl.BuildDocsResourceCard(ctx, f.ctx.GetConfig().External.WebLoginURL, docID, spaceID, cardtmpl.ResourceCard{
		Title: title, Attribution: attribution, Variant: variant, Facts: facts, Source: cardtmpl.Source{Label: labels.sourceLabel},
	})
}

func buildTerminalEnvelope(document json.RawMessage, spaceID string, cardSeq int64) (string, error) {
	var card map[string]interface{}
	if err := json.Unmarshal(document, &card); err != nil {
		return "", fmt.Errorf("notify: decode terminal card: %w", err)
	}
	envelope := map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": cardmsg.ProfileV2, "space_id": spaceID, "card_seq": cardSeq, "card": card,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("notify: marshal terminal card: %w", err)
	}
	return string(raw), nil
}
