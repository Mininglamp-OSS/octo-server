package carddispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	renderProfile := card.RenderProfile
	if renderProfile == "" {
		renderProfile = cardmsg.RenderProfileOctoChatV1
	}
	payload := map[string]interface{}{
		"type":           cardmsg.InteractiveCard.Int(),
		"card_version":   cardmsg.CardVersion,
		"profile":        card.Profile,
		"render_profile": renderProfile,
		"card":           cardDocument,
	}
	if validateErr := cardmsg.Validate(payload); validateErr != nil {
		terminal = cardErrorCategory(validateErr)
		return nil, categorized(terminal, validateErr)
	}

	// PR-C D3：可信派发边界从「已通过 cardmsg.Validate 的 metadata.octo.template」
	// 与「已注册 ProducerSpec.ID + 已授权 target Space」为 Registry 产出的卡
	// 写入 server-authored 顶层 catalog 标记。业务调用方只掌握 Card.Document
	// （裸 card 节点），够不到信封顶层，因此无法自报 principal；无模板元数据的
	// legacy 文档不写标记（principal 只能靠猜，宁缺毋假）。
	if refID, refVersion, ok := cardDocumentTemplateContext(cardDocument); ok {
		provenance := cardmsg.CatalogProvenance{
			Version:       cardmsg.CatalogProvenanceVersion,
			PrincipalType: cardmsg.CatalogPrincipalWireInternalProducer,
			PrincipalID:   string(s.spec.ID),
			SpaceID:       target.SpaceID,
		}
		// Validate before stamping, because nothing downstream will. The
		// cardmsg.Validate call above runs before these keys exist, and
		// Finalize only builds `plain` and rechecks size — so this is the last
		// point at which an invalid marker can be refused.
		//
		// Refusing here rather than trimming target.SpaceID is deliberate: this
		// is the one boundary that authors an internal-producer marker, and it
		// should be unable to author one the readers reject. The readers are
		// strict (ParseCatalogProvenance requires a trimmed space_id) while the
		// gate on the way in only tests emptiness, and whether an untrimmed
		// Space survives the membership lookup is decided by a collation the
		// application does not choose: space_member declares none and inherits
		// the database default, and CI creates that database PAD SPACE
		// (utf8mb4_general_ci), where 'space-a ' matches 'space-a'. A stamped
		// frame that no reader accepts is unclickable and uneditable forever,
		// so this refuses the send instead of delivering a broken card.
		if err := provenance.Validate(); err != nil {
			terminal = cardErrorCategory(err)
			return nil, categorized(terminal, err)
		}
		payload[cardmsg.CatalogTemplateRefKey] = map[string]interface{}{
			"id": refID, "version": refVersion,
		}
		payload[cardmsg.CatalogProvenanceKey] = provenance.MarshalMap()
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
	// The persistence-column pre-check the bot template send already does
	// (modules/bot_api/send.go), mirrored here — review P2-3 (yujiawei).
	//
	// RecheckPayloadSize above enforces the 512 KiB wire limit. The narrower
	// limit is the TEXT column an *edit* of this card has to fit in: 65,535
	// bytes, two orders of magnitude smaller. A frame between the two is sent
	// happily and then fails every finalize with ErrCardMutationTooLarge, which
	// for docs.access-request means a card that renders, cannot be decided, and
	// cannot be repaired. That window is not hypothetical — main's own gate
	// comment measures docs.access-request@0.3.0 at 121% of the column under its
	// own display limits, and the markers stamped just above add ~150-190 bytes
	// to every frame that reaches here.
	//
	// Measured against the worst-case *first edit* envelope, not the send frame:
	// the edit path adds card_seq (always) and transient (progress frames), so
	// admitting a frame by its send size leaves a same-width window where the
	// send succeeds and the first edit is refused. Same reasoning, same two
	// worst-case values, as the bot side.
	probe := make(map[string]interface{}, len(payload)+2)
	for k, v := range payload {
		probe[k] = v
	}
	probe["card_seq"] = int64(math.MaxInt64)
	probe["transient"] = true
	probeFrame, probeErr := json.Marshal(probe)
	if probeErr != nil {
		// Unreachable in practice — payload just marshalled for the size check —
		// but a fail-open branch does not belong on a size gate.
		terminal = CategoryCardInvalid
		return nil, categorized(terminal, probeErr)
	}
	if _, columnErr := NormalizeFrameForPersistence(string(probeFrame)); columnErr != nil {
		terminal = cardErrorCategory(columnErr)
		if errors.Is(columnErr, ErrCardMutationTooLarge) {
			terminal = CategoryPayloadTooLarge
		}
		return nil, categorized(terminal, columnErr)
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
	if !cardmsg.IsAcceptedRenderProfile(card.RenderProfile) {
		return errors.New("unsupported render profile")
	}
	return nil
}

// cardDocumentTemplateContext 从裸 card 节点提取 Registry 模板身份，语义与
// cardmsg.CardTemplateContext（信封形态）一致：协议必须是 octo-card@1.0，
// id/version 非空且已 trim；任何偏差都按「非 Registry 文档」处理（不写标记）。
func cardDocumentTemplateContext(card map[string]interface{}) (string, string, bool) {
	metadata, _ := card["metadata"].(map[string]interface{})
	octo, _ := metadata["octo"].(map[string]interface{})
	template, _ := octo["template"].(map[string]interface{})
	protocol, _ := octo["protocol"].(string)
	id, _ := template["id"].(string)
	version, _ := template["version"].(string)
	if protocol != "octo-card@1.0" || id == "" || version == "" ||
		strings.TrimSpace(id) != id || strings.TrimSpace(version) != version {
		return "", "", false
	}
	return id, version, true
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
