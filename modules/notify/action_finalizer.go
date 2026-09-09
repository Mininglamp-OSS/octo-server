package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/user"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

type cardActionMutator interface {
	Mutate(context.Context, carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error)
}

type DocsActionFinalizer struct {
	ctx     *config.Context
	mutator cardActionMutator
	updater cardtmpl.CardUpdater
	// users/spaces resolve the AUTHORITATIVE decider display internally from the
	// ids Docs returns (result.DeciderUID / DeciderSpaceID). They are optional:
	// when nil (or when a callback carries no decider ids) the finalizer keeps the
	// bounded compatibility fallback to result.Display.
	users  deciderUserResolver
	spaces deciderSpaceResolver
}

type actionFinalizeError struct {
	category string
	err      error
}

func (e *actionFinalizeError) Error() string    { return "notify: " + e.category }
func (e *actionFinalizeError) Unwrap() error    { return e.err }
func (e *actionFinalizeError) Category() string { return e.category }

func NewDocsActionFinalizer(ctx *config.Context, mutator cardActionMutator) (*DocsActionFinalizer, error) {
	if ctx == nil || mutator == nil {
		return nil, errors.New("notify: docs action finalizer dependencies are required")
	}
	return &DocsActionFinalizer{ctx: ctx, mutator: mutator}, nil
}

func NewDocsActionFinalizerWithUpdater(ctx *config.Context, updater cardtmpl.CardUpdater, mutator cardActionMutator) (*DocsActionFinalizer, error) {
	if ctx == nil || updater == nil || mutator == nil {
		return nil, errors.New("notify: docs action finalizer dependencies are required")
	}
	return &DocsActionFinalizer{ctx: ctx, updater: updater, mutator: mutator}, nil
}

func NewDocsActionFinalizerFromContext(ctx *config.Context) (*DocsActionFinalizer, error) {
	mutator := carddispatch.NewCardMutator(ctx)
	updater, err := cardtmpl.NewCardUpdater(cardtmpl.DefaultCatalog(), mutator)
	if err != nil {
		return nil, err
	}
	finalizer, err := NewDocsActionFinalizerWithUpdater(ctx, updater, mutator)
	if err != nil {
		return nil, err
	}
	// Production wiring for authoritative decider display resolution: names come
	// from the user service, operator Space names from an active-membership check
	// against the server DB. Both stay best-effort inside Finalize.
	finalizer.users = user.NewService(ctx)
	finalizer.spaces = spaceMemberNameResolver{session: ctx.DB()}
	return finalizer, nil
}

func (f *DocsActionFinalizer) Finalize(ctx context.Context, event cardactiondispatch.Event, result cardactiondispatch.DecisionResult) error {
	if event.Owner != "docs" || event.ActionType != "access_request.decision" || event.EventID <= 0 {
		return errors.New("notify: unsupported card action result")
	}
	docID, _ := event.Data["doc_id"].(string)
	if strings.TrimSpace(docID) == "" || strings.TrimSpace(event.SpaceID) == "" {
		return errors.New("notify: docs action result is missing authoritative context")
	}
	// The mutator addresses a DM by the sender's peer. The ingress now stamps that
	// same peer into event.ChannelID, so for a freshly enqueued event this rewrite
	// lands on the value already there — but only while the clicker *is* the
	// sender's peer, which is every reachable click today (a card's sender is a
	// bot, and this endpoint needs a user login token). The projection also
	// answers the inverse shape, sender-clicks-own-card, and there ChannelID is
	// the other peer while OperatorUID is the sender: this rewrite would pick the
	// wrong one. Do not read it as an unconditional no-op and delete it on that
	// basis — it is still the fixup for events queued before the ingress was
	// corrected, which name the sending bot itself.
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
	// Authoritative decider resolution (decision-actor-server-resolution). When
	// Docs returns the decider's ids, octo-server — the identity authority —
	// resolves the display name and operator Space name internally and those win
	// over any caller-supplied display copy (anti-forgery: a callback could name
	// any operator/Space it liked). Only approved/denied cards carry a decision
	// block, so only they need it. Every lookup failure degrades the optional
	// display field to empty (the render then shows the generic placeholder); it
	// must never fail this already-committed decision. Legacy callbacks with no
	// decider ids keep the bounded compatibility fallback to result.Display.
	result = f.resolveAuthoritativeDeciderDisplay(ctx, result)
	// Every state renders through the Registry when an updater is present, not
	// just the decided ones. The pending card is a Registry render and so
	// carries the server-authored catalog markers; the fallback below rebuilds
	// the frame from a six-key allowlist that holds neither, so routing a
	// marked card there erased both on an ordinary user click and left the
	// identity guards in pkg/cardtmpl/updater.go inert for that message.
	//
	// Selecting the route by state was the bug's shape, so the state list is
	// gone rather than extended: `cancelled` was the reported case, `pending`
	// reaches the same branch, and a state added later would have inherited the
	// defect silently. What remains is the distinction the fallback was written
	// for — a deployment with no updater at all. `terminal` still means
	// "decided", and still gates the notification below.
	if f.updater != nil {
		if err := f.replaceWithRegistryResult(ctx, event, result, channelID, lang, title, denyReason); err != nil {
			return err
		}
	} else {
		terminalDocument, err := f.buildTerminalDocument(ctx, lang, event, title, denyReason, result)
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
	// The original request card is the sole terminal surface. Do not send a
	// second "access granted/denied" card to the requester.
	return nil
}

// resolveAuthoritativeDeciderDisplay overwrites the operator display fields with
// octo-server's internal resolution of the authoritative decider ids for
// approved/denied results. When result.DeciderUID is set, caller-supplied
// display.operator_name/operator_space_name are discarded (they must never win
// over the authoritative ids) and decided_at is taken from result.DecidedAt.
// When no decider id is present, the result is returned unchanged so legacy
// callbacks keep their bounded compatibility fallback. The returned copy owns a
// fresh Display map, so the shared result value is never mutated.
func (f *DocsActionFinalizer) resolveAuthoritativeDeciderDisplay(_ context.Context, result cardactiondispatch.DecisionResult) cardactiondispatch.DecisionResult {
	if strings.TrimSpace(result.DeciderUID) == "" {
		return result
	}
	if result.State != cardactiondispatch.StateApproved && result.State != cardactiondispatch.StateDenied {
		return result
	}
	resolved := resolveDeciderDisplay(f.users, f.spaces, result.DeciderUID, result.DeciderSpaceID, func(msg string, err error) {
		log.NewTLog("DocsActionFinalizer").Warn("docs action "+msg+" failed (display degraded)",
			zap.Error(err), zap.String("decider_uid", result.DeciderUID), zap.String("decider_space_id", result.DeciderSpaceID))
	})
	display := make(map[string]string, len(result.Display)+3)
	for k, v := range result.Display {
		display[k] = v
	}
	// Authoritative ids present → the resolved values win, even when a lookup
	// returned empty (degrade to placeholder rather than trust caller copy).
	// Clear ALL three legacy display fields first: if the authoritative time is
	// absent, retaining display.decided_at would pair a trusted decider with an
	// untrusted / potentially late-clicker timestamp.
	delete(display, "operator_name")
	delete(display, "operator_space_name")
	delete(display, "decided_at")
	display["operator_name"] = resolved.OperatorName
	display["operator_space_name"] = resolved.OperatorSpaceName
	if decidedAt := formatDecisionTime(result.DecidedAt); decidedAt != "" {
		display["decided_at"] = decidedAt
	}
	result.Display = display
	return result
}

// 0.3.0 result view schema 的各字段 rune 上限(见
// handoff/docs.access-request@0.3.0/contract/data.schema.json)。回调 Display
// 的值合法可达 500 runes，而 event.Data 只受卡片信封总大小约束；必须在渲染前截断
// 到各自 schema cap —— 否则 RenderCard 的 InputSchema 校验返回 ErrFieldsInvalid,
// ReplaceView 确定性失败,审批卡永远停在 pending，审批人看不到终态(PR#641 review P1)。
// 截断而非编造,与 denyReason 既有做法一致。
const (
	capResultTitle      = 200                      // document.title / document.sourceName
	capResultName       = 120                      // requester.name / decision.operatorName
	capResultShortLabel = 80                       // decidedAt/requestedAt/messageTime/permission.*
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

// docsResultTemplateVersion resolves the click-driven result edit's target
// version from the durable event's stored card context. The rules live in
// docsResultVersion so the sibling mutate endpoint cannot drift from them.
func docsResultTemplateVersion(event cardactiondispatch.Event) (string, error) {
	return docsResultVersion(event.Card.TemplateID, event.Card.TemplateVersion)
}

func (f *DocsActionFinalizer) replaceWithRegistryResult(ctx context.Context, event cardactiondispatch.Event,
	result cardactiondispatch.DecisionResult, channelID, lang, title, denyReason string) error {
	version, err := docsResultTemplateVersion(event)
	if err != nil {
		return err
	}
	fields, state, err := buildDocsAccessResultFields(lang, version, docsResultRenderInput{
		Data:                  event.Data,
		Title:                 title,
		OperatorName:          result.Display["operator_name"],
		OperatorSpaceName:     result.Display["operator_space_name"],
		DecidedAtDisplay:      result.Display["decided_at"],
		AuthoritativeDecision: strings.TrimSpace(result.DeciderUID) != "",
		DenyReason:            denyReason,
		Denied:                result.State == cardactiondispatch.StateDenied,
		Cancelled:             result.State == cardactiondispatch.StateCancelled,
		// Anything that is not a decision and not an explicit cancellation —
		// `pending` today, whatever a later contract adds — renders as
		// unavailable, which is what the pre-Registry fallback already showed
		// for those states.
		Unavailable: result.State != cardactiondispatch.StateApproved &&
			result.State != cardactiondispatch.StateDenied &&
			result.State != cardactiondispatch.StateCancelled,
	})
	if err != nil {
		return err
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
	}, docsaccessrequest.TemplateID, version, state, fields, cardtmpl.BuildEnv{
		WebLoginURL: f.ctx.GetConfig().External.WebLoginURL, Lang: lang, SpaceID: event.SpaceID,
	})
}

// docsResultRenderInput is the single bounded mapping input used by both the
// click-driven finalizer and the sibling-card mutation endpoint. Data must come
// from the stored server-authored Action.Submit payload, never caller display
// JSON, so request/document identity cannot be rewritten during a mutation.
type docsResultRenderInput struct {
	Data                  map[string]interface{}
	Title                 string
	OperatorName          string
	OperatorSpaceName     string
	DecidedAtDisplay      string
	AuthoritativeDecision bool
	DenyReason            string
	Denied                bool
	Cancelled             bool
	Unavailable           bool
}

func buildDocsAccessResultFields(lang, version string, input docsResultRenderInput) (json.RawMessage, cardtmpl.State, error) {
	requestID := strings.TrimSpace(stringEventData(input.Data, "request_id"))
	docID := strings.TrimSpace(stringEventData(input.Data, "doc_id"))
	if requestID == "" || docID == "" {
		return nil, "", errors.New("notify: docs result is missing stored request identity")
	}
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = strings.TrimSpace(stringEventData(input.Data, "doc_title"))
	}
	if title == "" {
		title = docID
	}
	operatorName := strings.TrimSpace(input.OperatorName)
	if operatorName == "" {
		// A UID is an internal identifier, not display copy. Use stable localized
		// generic text when the callback does not provide an operator name.
		operatorName = docsLabelsFor(lang).decisionActor
	}
	denyReason := strings.TrimSpace(input.DenyReason)
	if runes := []rune(denyReason); len(runes) > capResultReason {
		denyReason = string(runes[:capResultReason])
	}
	state := docsaccessrequest.StateApproved
	wireState := "approved"
	switch {
	case input.Unavailable:
		state, wireState = docsaccessrequest.StateUnavailable, "unavailable"
	case input.Cancelled:
		state, wireState = docsaccessrequest.StateCancelled, "cancelled"
	case input.Denied:
		state, wireState = docsaccessrequest.StateRejected, "rejected"
	}
	actor := stringEventData(input.Data, "actor")
	document := map[string]any{
		"docId": docID,
		"title": truncRunes(strings.TrimSpace(title), capResultTitle),
	}
	requester := map[string]any{"name": truncRunes(strings.TrimSpace(actor), capResultName)}
	if version == docsaccessrequest.TemplateVersionV3 {
		if value := strings.TrimSpace(stringEventData(input.Data, "requester_space_name")); value != "" {
			requester["sourceSpaceName"] = truncRunes(value, capResultTitle)
		}
	}
	if value := strings.TrimSpace(stringEventData(input.Data, "source_name")); value != "" {
		document["sourceName"] = truncRunes(value, capResultTitle)
	}
	if value := strings.TrimSpace(stringEventData(input.Data, "actor_avatar_url")); value != "" {
		// URL 不截断:schema 无 maxLength(https 由渲染层校验),截断会破坏链接。
		requester["avatarUrl"] = value
	}
	fields := map[string]any{
		"requestId": requestID,
		"state":     wireState,
		"document":  document,
		"requester": requester,
	}
	// A cancelled request was never decided, so it has no operator and no
	// decision time. The data contract requires `decision` only for
	// approved/rejected; emitting one here would put the generic operator
	// placeholder on a card nobody acted on.
	if !input.Cancelled && !input.Unavailable {
		decision := map[string]any{
			"operatorName":     truncRunes(operatorName, capResultName),
			"decidedAtDisplay": truncRunes(strings.TrimSpace(input.DecidedAtDisplay), capResultShortLabel),
		}
		if version == docsaccessrequest.TemplateVersionV3 {
			decision["operatorSpaceName"] = truncRunes(strings.TrimSpace(input.OperatorSpaceName), capResultTitle)
		}
		fields["decision"] = decision
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
		// V3 historically falls back from an empty decision time to the request
		// message time. Keep that for legacy callbacks, but never fabricate a
		// decision timestamp after Docs has supplied an authoritative decider.
		if destination == "messageTimeDisplay" && input.AuthoritativeDecision &&
			strings.TrimSpace(input.DecidedAtDisplay) == "" {
			continue
		}
		if value := strings.TrimSpace(stringEventData(input.Data, source)); value != "" {
			fields[destination] = truncRunes(value, dataCaps[destination])
		}
	}
	permission := make(map[string]any, 2)
	if value := strings.TrimSpace(stringEventData(input.Data, "permission_label")); value != "" {
		permission["label"] = truncRunes(value, capResultShortLabel)
	}
	if value := strings.TrimSpace(stringEventData(input.Data, "permission_role_label")); value != "" {
		permission["roleLabel"] = truncRunes(value, capResultShortLabel)
	}
	if len(permission) > 0 {
		fields["permission"] = permission
	}
	if version == docsaccessrequest.TemplateVersionV3 {
		if names := stringSliceEventData(input.Data, "requested_bot_names"); len(names) > 0 {
			bounded := make([]string, 0, len(names))
			for _, name := range names[:min(len(names), 50)] {
				if value := strings.TrimSpace(name); value != "" {
					bounded = append(bounded, truncRunes(value, capResultName))
				}
			}
			if len(bounded) > 0 {
				fields["requestedBotNames"] = bounded
			}
		}
	}
	if wireState == "rejected" && denyReason != "" {
		fields["decision"].(map[string]any)["rejectionReason"] = denyReason
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, "", fmt.Errorf("notify: marshal registry result fields: %w", err)
	}
	return raw, state, nil
}

func stringEventData(data map[string]interface{}, key string) string {
	value, _ := data[key].(string)
	return value
}

func stringSliceEventData(data map[string]interface{}, key string) []string {
	switch values := data[key].(type) {
	case []string:
		return values
	case []interface{}:
		out := make([]string, 0, len(values))
		for _, raw := range values {
			if value, ok := raw.(string); ok {
				out = append(out, value)
			}
		}
		return out
	default:
		return nil
	}
}

func (f *DocsActionFinalizer) buildTerminalDocument(ctx context.Context, lang string, event cardactiondispatch.Event, title, denyReason string, result cardactiondispatch.DecisionResult) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	docID := stringEventData(event.Data, "doc_id")
	spaceID := event.SpaceID
	webLoginURL := f.ctx.GetConfig().External.WebLoginURL
	// The legacy/no-updater fallback still emits the enriched outcome card; the
	// normal finalizer and sibling mutation paths render V3 through the Registry.
	switch result.State {
	case cardactiondispatch.StateApproved:
		return buildDocsDecisionTerminalDocument(ctx, webLoginURL, lang, docID, spaceID, title, denyReason, false, event.Data, result.Display)
	case cardactiondispatch.StateDenied:
		return buildDocsDecisionTerminalDocument(ctx, webLoginURL, lang, docID, spaceID, title, denyReason, true, event.Data, result.Display)
	}
	// Cancelled / unavailable are transient states without an enriched design —
	// keep the prior plain resource-card rebuild.
	attribution, variant := labels.accessUnavailableBanner, "docs.access_unavailable"
	if result.State == cardactiondispatch.StateCancelled {
		attribution, variant = labels.accessCancelledBanner, "docs.access_cancelled"
	}
	return cardtmpl.BuildDocsResourceCard(ctx, webLoginURL, docID, cardtmpl.ResourceCard{
		Title: title, Attribution: attribution, Variant: variant, Source: cardtmpl.Source{Label: labels.sourceLabel},
	})
}

// buildDocsDecisionTerminalDocument renders the legacy/no-updater fallback for
// approved/denied outcomes. Production result updates use CardUpdater at the card's stored version.
func buildDocsDecisionTerminalDocument(ctx context.Context, webLoginURL, lang, docID, spaceID, title, denyReason string, denied bool, data map[string]interface{}, display map[string]string) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	content := cardtmpl.DocsOutcomeContent{
		Title: title, Source: cardtmpl.Source{Label: labels.sourceLabel},
		HeaderLabel:               labels.approvalHeader,
		RequestedAtDisplay:        stringEventData(data, "requested_at_display"),
		MessageTimeDisplay:        display["decided_at"],
		DecisionOperatorName:      display["operator_name"],
		DecisionOperatorSpaceName: display["operator_space_name"],
	}
	if denied {
		content.Variant = "docs.access_denied"
		content.Denied = true
		content.StatusLabel = labels.deniedStatus
		content.ReasonLabel = labels.denyReasonLabel
		content.Reason = denyReason
	} else {
		content.Variant = "docs.access_approved"
		content.StatusLabel = labels.approvedStatus
	}
	return cardtmpl.BuildDocsApprovalOutcomeCard(ctx, webLoginURL, docID, content)
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
