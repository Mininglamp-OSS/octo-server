package carddispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/reqid"
	"go.uber.org/zap"
)

type producerSender struct {
	spec             ProducerSpec
	identityResolver IdentityResolver
	authorizer       Authorizer
	transport        Transport
	metrics          *Metrics
	logger           interface {
		Info(msg string, fields ...zap.Field)
		Error(msg string, fields ...zap.Field)
	}
	featureEnabled func() bool
	slots          chan struct{}
}

func (s *producerSender) Send(ctx context.Context, target Target, card Card) (result *Result, err error) {
	producer := string(s.spec.ID)
	targetLabel := normalizedTargetKind(target.ChannelType)
	started := s.metrics.begin(producer, targetLabel)
	terminal := CategoryDispatchFailed
	defer func() { s.metrics.finish(producer, targetLabel, started, terminal) }()

	if err := validateRequest(ctx, target, card); err != nil {
		terminal = CategoryInvalidRequest
		return nil, categorized(terminal, err)
	}
	if !s.featureEnabled() {
		terminal = CategoryFeatureDisabled
		return nil, categorized(terminal, errors.New("global card feature disabled"))
	}
	if !containsUint8(s.spec.AllowedChannelTypes, target.ChannelType) || !containsString(s.spec.AllowedProfiles, card.Profile) {
		terminal = CategoryProducerDisabled
		return nil, categorized(terminal, errors.New("producer policy does not allow target or profile"))
	}

	select {
	case s.slots <- struct{}{}:
		s.metrics.addInFlight(producer, 1)
		defer func() {
			<-s.slots
			s.metrics.addInFlight(producer, -1)
		}()
	default:
		terminal = CategoryBusy
		return nil, categorized(terminal, errors.New("producer concurrency saturated"))
	}

	// Snapshot the caller's bytes: once copied, a caller that retains and later
	// mutates its RawMessage cannot affect validation, finalization, or
	// transport serialization, which all read this private copy. (A caller that
	// mutates the same slice *concurrently* with this call is a caller-side data
	// race the copy cannot prevent; callers must not share a Card across
	// goroutines mid-Send.)
	document := append([]byte(nil), card.Document...)
	if err := ctx.Err(); err != nil {
		terminal = CategoryDispatchFailed
		return nil, categorized(terminal, err)
	}

	identity, resolveErr := s.identityResolver.Resolve(s.spec.SenderUID)
	if resolveErr != nil || identity == nil || identity.UID != s.spec.SenderUID || strings.HasPrefix(identity.UID, "iwh_") {
		terminal = CategoryIdentityUntrusted
		if resolveErr == nil {
			resolveErr = errors.New("bound sender is not an active authoritative bot")
		}
		return nil, categorized(terminal, resolveErr)
	}
	if identity.Kind != botidentity.KindUserBot && identity.Kind != botidentity.KindAppBot {
		terminal = CategoryIdentityUntrusted
		return nil, categorized(terminal, errors.New("unsupported bot identity kind"))
	}

	policy := AuthorizationPolicy{SpacePolicy: s.spec.SpacePolicy, GroupPolicy: s.spec.GroupPolicy}
	if authErr := s.authorizer.Authorize(ctx, identity, target, policy); authErr != nil {
		terminal = CategoryTargetDenied
		return nil, categorized(terminal, authErr)
	}

	var cardDocument map[string]interface{}
	if decodeErr := json.Unmarshal(document, &cardDocument); decodeErr != nil || len(cardDocument) == 0 {
		terminal = CategoryCardInvalid
		if decodeErr == nil {
			decodeErr = errors.New("card document must be a non-empty JSON object")
		}
		return nil, categorized(terminal, decodeErr)
	}
	payload := map[string]interface{}{
		"type":         cardmsg.InteractiveCard.Int(),
		"card_version": cardmsg.CardVersion,
		"profile":      card.Profile,
		"card":         cardDocument,
	}
	if validateErr := cardmsg.Validate(payload); validateErr != nil {
		terminal = cardErrorCategory(validateErr)
		return nil, categorized(terminal, validateErr)
	}

	// Authorization has already established this exact active Space. It is the
	// only source allowed to enrich the wire envelope.
	payload["space_id"] = target.SpaceID
	if finalizeErr := cardmsg.Finalize(payload); finalizeErr != nil {
		terminal = cardErrorCategory(finalizeErr)
		return nil, categorized(terminal, finalizeErr)
	}
	if sizeErr := cardmsg.RecheckPayloadSize(payload); sizeErr != nil {
		terminal = cardErrorCategory(sizeErr)
		return nil, categorized(terminal, sizeErr)
	}
	wire, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		terminal = CategoryCardInvalid
		return nil, categorized(terminal, marshalErr)
	}
	if err := ctx.Err(); err != nil {
		terminal = CategoryDispatchFailed
		return nil, categorized(terminal, err)
	}

	response, transportErr := s.transport.SendMessageWithResult(&config.MsgSendReq{
		Header:      config.MsgHeader{RedDot: 1},
		FromUID:     s.spec.SenderUID,
		ChannelID:   target.ChannelID,
		ChannelType: target.ChannelType,
		Payload:     wire,
	})
	if transportErr != nil || response == nil {
		terminal = CategoryDispatchFailed
		if transportErr == nil {
			transportErr = errors.New("transport returned an empty result")
		}
		if s.logger != nil {
			s.logger.Error("internal card dispatch failed",
				zap.String("request_id", reqid.FromContext(ctx)),
				zap.String("producer", producer),
				zap.String("sender_kind", string(identity.Kind)),
				zap.String("space_id", target.SpaceID),
				zap.String("target_kind", targetLabel),
				zap.Error(transportErr))
		}
		return nil, categorized(terminal, transportErr)
	}

	terminal = CategoryOK
	result = &Result{
		MessageID:   response.MessageID,
		MessageSeq:  response.MessageSeq,
		ClientMsgNo: response.ClientMsgNo,
	}
	if s.logger != nil {
		s.logger.Info("internal card dispatched",
			zap.String("request_id", reqid.FromContext(ctx)),
			zap.String("producer", producer),
			zap.String("sender_kind", string(identity.Kind)),
			zap.String("space_id", target.SpaceID),
			zap.String("target_kind", targetLabel),
			zap.Int64("message_id", result.MessageID),
			zap.Uint32("message_seq", result.MessageSeq),
			zap.String("client_msg_no", result.ClientMsgNo))
	}
	return result, nil
}

func validateRequest(ctx context.Context, target Target, card Card) error {
	if ctx == nil {
		return errors.New("context is required")
	}
	if strings.TrimSpace(target.SpaceID) == "" || strings.TrimSpace(target.ChannelID) == "" {
		return errors.New("space and channel are required")
	}
	switch target.ChannelType {
	case common.ChannelTypePerson.Uint8(), common.ChannelTypeGroup.Uint8(), common.ChannelTypeCommunityTopic.Uint8():
	default:
		return fmt.Errorf("unsupported channel type %d", target.ChannelType)
	}
	if strings.TrimSpace(card.Profile) == "" || len(card.Document) == 0 {
		return errors.New("profile and card document are required")
	}
	return nil
}

func cardErrorCategory(err error) Category {
	if errors.Is(err, cardmsg.ErrCardPayloadTooLarge) {
		return CategoryPayloadTooLarge
	}
	return CategoryCardInvalid
}

func normalizedTargetKind(channelType uint8) string {
	switch channelType {
	case common.ChannelTypePerson.Uint8():
		return "person"
	case common.ChannelTypeGroup.Uint8():
		return "group"
	case common.ChannelTypeCommunityTopic.Uint8():
		return "thread"
	default:
		return "unknown"
	}
}

func containsUint8(values []uint8, want uint8) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
