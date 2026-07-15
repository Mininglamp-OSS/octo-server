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
	"github.com/Mininglamp-OSS/octo-server/pkg/i18n"
)

type cardActionMutator interface {
	Mutate(context.Context, carddispatch.CardMutationRequest) (carddispatch.CardMutationResult, error)
}

type DocsActionFinalizer struct {
	ctx     *config.Context
	mutator cardActionMutator
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

func NewDocsActionFinalizerFromContext(ctx *config.Context) (*DocsActionFinalizer, error) {
	sender, err := carddispatch.SenderFromContext(ctx, docsNotifyProducerID)
	if err != nil {
		return nil, err
	}
	return NewDocsActionFinalizer(ctx, carddispatch.NewCardMutator(ctx), sender)
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
		title = docID
	}
	terminalDocument, err := f.buildTerminalDocument(ctx, lang, docID, event.SpaceID, title, result)
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
	if result.State != cardactiondispatch.StateApproved && result.State != cardactiondispatch.StateDenied {
		return nil
	}
	outcomeDocument, err := f.buildOutcomeDocument(ctx, lang, docID, event.SpaceID, title, result.State)
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

func (f *DocsActionFinalizer) buildTerminalDocument(ctx context.Context, lang, docID, spaceID, title string, result cardactiondispatch.DecisionResult) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	attribution := labels.accessDeniedBanner
	variant := "docs.access_denied"
	switch result.State {
	case cardactiondispatch.StateApproved:
		attribution, variant = labels.accessGrantedBanner, "docs.access_approved"
	case cardactiondispatch.StateDenied:
		attribution, variant = labels.accessDeniedBanner, "docs.access_denied"
	case cardactiondispatch.StateCancelled:
		attribution, variant = labels.accessCancelledBanner, "docs.access_cancelled"
	default:
		attribution, variant = labels.accessUnavailableBanner, "docs.access_unavailable"
	}
	return cardtmpl.BuildDocsResourceCard(ctx, f.ctx.GetConfig().External.WebLoginURL, docID, spaceID, cardtmpl.ResourceCard{
		Title: title, Attribution: attribution, Variant: variant, Source: cardtmpl.Source{Label: labels.sourceLabel},
	})
}

func (f *DocsActionFinalizer) buildOutcomeDocument(ctx context.Context, lang, docID, spaceID, title string, state cardactiondispatch.State) (json.RawMessage, error) {
	labels := docsLabelsFor(lang)
	attribution := labels.accessGrantedBanner
	variant := "docs.access_granted"
	if state == cardactiondispatch.StateDenied {
		attribution, variant = labels.accessDeniedBanner, "docs.access_denied"
	}
	return cardtmpl.BuildDocsResourceCard(ctx, f.ctx.GetConfig().External.WebLoginURL, docID, spaceID, cardtmpl.ResourceCard{
		Title: title, Attribution: attribution, Variant: variant, Source: cardtmpl.Source{Label: labels.sourceLabel},
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
