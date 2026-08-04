package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/Mininglamp-OSS/octo-server/pkg/httperr"
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"go.uber.org/zap"
)

// docsCardMutateSeqKey is the single shared GenSeq flag for docs access-card
// card_seq mutations. The literal preserves the existing persisted sequence key
// byte-for-byte; despite its historical "messageExtra" prefix, it does NOT
// allocate message_extra.version and must not use the per-channel ReserveTx
// cursor allocator. card_seq only needs to be strictly positive and greater than
// the target card's stored card_seq; a sibling card was sent fresh (no
// message_extra row => NULL card_seq), so one globally-monotonic counter suffices
// and avoids an unbounded per-message key space.
const docsCardMutateSeqKey = "messageExtra:docs-card-mutate"

// docsAccessVariantPrefix gates which cards this docs-capability endpoint may
// mutate. The shared `notification` bot sends summary / docs / action-outcome
// cards, so "sender == notification" does NOT prove a card is docs-owned. The
// server-authored card variant (metadata.octo.variant, e.g.
// "docs.access_requested" / "docs.access_granted") is the authoritative
// docs-domain discriminator; only cards in this family may be touched.
const docsAccessVariantPrefix = "docs.access_"

var errDocsSiblingContextInvalid = errors.New("notify: invalid stored docs sibling context")

// docsAccessCardVariant reports the target card's variant and whether it belongs
// to the docs access family, from the effective card envelope. Pure so the
// capability gate is unit-testable without a live card store.
func docsAccessCardVariant(envelope []byte) (variant string, isDocsAccess bool) {
	variant, ok := cardmsg.CardVariant(envelope)
	if !ok {
		return "", false
	}
	return variant, strings.HasPrefix(variant, docsAccessVariantPrefix)
}

// docsAccessMutationDisposition protects retry idempotency after the first
// successful mutation has removed the pending Submit actions. A repeated
// decision for the same terminal state is already applied; the opposite state
// is a conflict and must never overwrite a committed decision.
func docsAccessMutationDisposition(variant, kind string) (alreadyApplied, conflict bool) {
	switch variant {
	case docsaccessrequest.VariantApproved:
		return kind == DocsCardKindAccessGranted, kind == DocsCardKindAccessDenied
	case docsaccessrequest.VariantRejected:
		return kind == DocsCardKindAccessDenied, kind == DocsCardKindAccessGranted
	default:
		return false, false
	}
}

// CardMutateReq is the access-decision card-sync request (task
// docs-access-decision-card-sync). docs-backend calls this once per sibling
// approver card after a decision commits, to drive that card to its terminal
// state so no approver is left holding an actionable card.
//
// The card is located by (ChannelID, MessageID); SpaceID is used ONLY to render
// the terminal card + its deep link, never to locate the card. MessageID is a
// decimal string because IM message ids are int64 and exceed JS's safe-integer
// range. SenderUID is NOT part of the request — it is resolved server-side to
// the docs producer bot (identical source to /notify), so the caller passes zero
// bot identity and a sender mismatch is structurally impossible.
type CardMutateReq struct {
	SpaceID     string `json:"space_id"`
	ChannelID   string `json:"channel_id"`
	ChannelType uint8  `json:"channel_type"`
	MessageID   string `json:"message_id"`
	Kind        string `json:"kind"` // access_granted | access_denied
	DocID       string `json:"doc_id"`
	Title       string `json:"title"`
	DenyReason  string `json:"deny_reason"` // surfaced on the denied terminal card; empty on approve
	// OperatorName / DecidedAtDisplay are optional additive display fields.
	// Older callers omit them and receive localized generic operator copy; raw
	// UIDs are never derived as display copy by this endpoint.
	OperatorName     string `json:"operator_name"`
	DecidedAtDisplay string `json:"decided_at_display"`
}

// CardMutateResp reports whether the card is now terminal.
type CardMutateResp struct {
	Mutated bool `json:"mutated"`
}

// mutateCard handles POST /v1/internal/cards/mutate. It drives one already-
// delivered docs access card to its terminal (approved/denied) state in place,
// reusing the same V3 Registry field mapping as the click-driven finalizer.
// Docs capability only.
func (n *Notify) mutateCard(c *wkhttp.Context) {
	capability, _ := c.Get(notifyCapabilityContextKey)
	typedCapability, _ := capability.(notifyCapability)
	if typedCapability.Kind != notifyCapabilityDocs {
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardNotAllowed, nil, nil)
		return
	}

	var req CardMutateReq
	if err := c.BindJSON(&req); err != nil {
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateInvalid, nil, nil)
		return
	}
	if req.SpaceID == "" || req.ChannelID == "" || req.MessageID == "" || req.DocID == "" ||
		(req.Kind != DocsCardKindAccessGranted && req.Kind != DocsCardKindAccessDenied) {
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateInvalid, nil, nil)
		return
	}
	ctx := c.Request.Context()
	channelType := req.ChannelType
	if channelType == 0 {
		channelType = common.ChannelTypePerson.Uint8()
	}

	// Capability containment (P0, security): this docs-capability endpoint may
	// ONLY drive docs access cards to terminal. Because the `notification` bot is
	// shared across producers, the downstream sender check ("card is from
	// notification") is NOT sufficient to prove the target is a docs card — a
	// docs token could otherwise overwrite any notification card (a summary card,
	// an action-outcome card, someone else's) into a docs terminal card. So read
	// the target's CURRENT card and require its own variant to be docs.access_*
	// BEFORE mutating. Snapshot also enforces sender==notification + lifecycle.
	mutator := carddispatch.NewCardMutator(n.ctx)
	snap, err := mutator.Snapshot(ctx, carddispatch.CardMutationTarget{
		SenderUID:   NotifyBotUIDValue,
		MessageID:   req.MessageID,
		ChannelID:   req.ChannelID,
		ChannelType: channelType,
	})
	if err != nil {
		// Malformed target (bad id / not a card) is the caller's bug (400); a
		// missing / sender-mismatched / backend-errored card is transient-or-benign
		// (409) — the caller retries or falls back to re-notify. Never fabricate
		// success: the decision is already committed and 409-guarded downstream.
		if errors.Is(err, carddispatch.ErrCardMutationInvalid) {
			n.Warn("card mutate snapshot invalid",
				zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("message_id", req.MessageID))
			httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateInvalid, nil, nil)
			return
		}
		n.Warn("card mutate snapshot failed",
			zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("message_id", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateFailed, nil, nil)
		return
	}
	variant, isDocsAccess := docsAccessCardVariant(snap.Envelope)
	if !isDocsAccess {
		n.Warn("card mutate rejected: target is not a docs access card",
			zap.String("space_id", req.SpaceID), zap.String("message_id", req.MessageID),
			zap.String("variant", variant))
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateForbidden, nil, nil)
		return
	}
	if alreadyApplied, conflict := docsAccessMutationDisposition(variant, req.Kind); alreadyApplied {
		c.Response(CardMutateResp{Mutated: true})
		return
	} else if conflict {
		n.Warn("card mutate rejected: target already has opposite terminal state",
			zap.String("space_id", req.SpaceID), zap.String("message_id", req.MessageID),
			zap.String("variant", variant), zap.String("kind", req.Kind))
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateFailed, nil, nil)
		return
	}

	cardSeq, err := n.ctx.GenSeq(docsCardMutateSeqKey)
	if err != nil {
		n.Error("gen card_seq for mutate failed", zap.Error(err), zap.String("message_id", req.MessageID))
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateFailed, nil, nil)
		return
	}
	updater, err := cardtmpl.NewCardUpdater(cardtmpl.DefaultCatalog(), mutator)
	if err != nil {
		n.Error("create card updater for mutate failed", zap.Error(err))
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateFailed, nil, nil)
		return
	}
	req.ChannelType = channelType
	if err = replaceSiblingWithRegistryResult(
		ctx, updater, req, snap, cardSeq, cardtmpl.BuildEnv{
			WebLoginURL: n.ctx.GetConfig().External.WebLoginURL,
			Lang:        i18n.OutboundLanguage(ctx),
			SpaceID:     req.SpaceID,
		}); err != nil {
		// A malformed edit is the caller's bug (400). Not-found / sender-mismatch
		// / stale / backend errors are transient-or-benign from the caller's view
		// (409): it retries or falls back to re-notify. The decision itself is
		// already committed and protected by the docs-backend pending guard, so a
		// mutate miss never corrupts state — it only leaves one stale card.
		if errors.Is(err, errDocsSiblingContextInvalid) || errors.Is(err, carddispatch.ErrCardMutationInvalid) ||
			errors.Is(err, cardtmpl.ErrFieldsInvalid) || errors.Is(err, cardtmpl.ErrUpdateInvalid) {
			n.Warn("card mutate invalid",
				zap.Error(err), zap.String("space_id", req.SpaceID), zap.String("message_id", req.MessageID))
			httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateInvalid, nil, nil)
			return
		}
		n.Warn("card mutate failed", zap.Error(err),
			zap.String("space_id", req.SpaceID), zap.String("message_id", req.MessageID),
			zap.String("kind", req.Kind))
		httperr.ResponseErrorL(c, errcode.ErrNotifyCardMutateFailed, nil, nil)
		return
	}
	c.Response(CardMutateResp{Mutated: true})
}

// replaceSiblingWithRegistryResult recovers the original request display
// context from the stored server-authored pending card, then renders the same
// docs.access-request@0.3.0/result view used by the click-driven finalizer.
// Caller-supplied Space/document identity must match the stored frame.
func replaceSiblingWithRegistryResult(ctx context.Context, updater cardtmpl.CardUpdater, req CardMutateReq,
	snapshot carddispatch.CardMutationSnapshot, cardSeq int64, env cardtmpl.BuildEnv,
) error {
	if ctx == nil || updater == nil || cardSeq <= 0 || env.SpaceID != req.SpaceID {
		return errDocsSiblingContextInvalid
	}
	var envelope struct {
		SpaceID string `json:"space_id"`
	}
	if err := json.Unmarshal(snapshot.Envelope, &envelope); err != nil ||
		strings.TrimSpace(envelope.SpaceID) == "" || envelope.SpaceID != req.SpaceID {
		return fmt.Errorf("%w: space mismatch", errDocsSiblingContextInvalid)
	}
	data, found := cardmsg.SubmitAction(snapshot.Envelope, cardtmpl.DocsApproveActionID)
	if !found {
		data, found = cardmsg.SubmitAction(snapshot.Envelope, cardtmpl.DocsDenyActionID)
	}
	if !found || data == nil {
		return fmt.Errorf("%w: pending action data not found", errDocsSiblingContextInvalid)
	}
	storedDocID := strings.TrimSpace(stringEventData(data, "doc_id"))
	if storedDocID == "" || storedDocID != strings.TrimSpace(req.DocID) {
		return fmt.Errorf("%w: document mismatch", errDocsSiblingContextInvalid)
	}
	denied := req.Kind == DocsCardKindAccessDenied
	fields, state, err := buildDocsAccessResultFields(env.Lang, docsResultRenderInput{
		Data:             data,
		Title:            req.Title,
		OperatorName:     req.OperatorName,
		DecidedAtDisplay: req.DecidedAtDisplay,
		DenyReason:       req.DenyReason,
		Denied:           denied,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", errDocsSiblingContextInvalid, err)
	}
	return updater.ReplaceView(ctx, cardtmpl.UpdateTarget{
		Target: carddispatch.Target{
			SpaceID: req.SpaceID, ChannelID: req.ChannelID, ChannelType: req.ChannelType,
		},
		SenderUID: NotifyBotUIDValue, MessageID: req.MessageID, CardSeq: cardSeq,
	}, docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3, state, fields, env)
}
