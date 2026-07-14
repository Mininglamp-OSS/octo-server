package resourceshare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/thread"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

type ShareOutcome string

const (
	ShareSent        ShareOutcome = "sent"
	ShareAlreadySent ShareOutcome = "already_sent"
	ShareDenied      ShareOutcome = "denied"
	ShareRateLimited ShareOutcome = "rate_limited"
	ShareFailed      ShareOutcome = "failed"
	ShareUnknown     ShareOutcome = "unknown"
)

type TargetResult struct {
	Target            Target       `json:"target"`
	Outcome           ShareOutcome `json:"outcome"`
	MessageID         string       `json:"message_id,omitempty"`
	MessageSeq        uint32       `json:"message_seq,omitempty"`
	RetryAfterSeconds int64        `json:"retry_after_seconds,omitempty"`
}

type ShareResult struct {
	Results []TargetResult `json:"results"`
}

type LimitRequest struct {
	ActorUID   string
	SpaceID    string
	ProviderID ProviderID
	Target     Target
}

type LimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

type shareStore interface {
	ClaimIntent(context.Context, VerifiedIntent) (*IntentClaimResult, error)
	ClaimDelivery(context.Context, int64, VerifiedIntent, Target, string) (*DeliveryClaimResult, error)
	BeginDispatch(context.Context, int64, string) error
	MarkSent(context.Context, int64, TransportResult, string) error
	MarkUnknown(context.Context, int64, string, string) error
	RecordPreTransportOutcome(context.Context, int64, DeliveryState, time.Time, string, string) error
	LoadDelivery(context.Context, int64) (*DeliveryRecord, error)
}

type shareAuthorizer interface {
	Authorize(context.Context, string, string, Target) error
}

type shareLimiter interface {
	Allow(context.Context, LimitRequest) (LimitDecision, error)
}

type shareProofSealer interface {
	Seal(map[string]interface{}, ProofContext) (map[string]interface{}, error)
}

type shareTransport interface {
	SendMessageWithResult(*config.MsgSendReq) (*config.MsgSendResp, error)
}

type ShareServiceDependencies struct {
	Registry       *Registry
	Store          shareStore
	Authorizer     shareAuthorizer
	Limiter        shareLimiter
	ProofSigner    shareProofSealer
	Transport      shareTransport
	FeatureEnabled func() bool
	Now            func() time.Time
}

type ShareService struct {
	registry       *Registry
	store          shareStore
	authorizer     shareAuthorizer
	limiter        shareLimiter
	proofSigner    shareProofSealer
	transport      shareTransport
	featureEnabled func() bool
	now            func() time.Time
}

func NewShareService(deps ShareServiceDependencies) (*ShareService, error) {
	featureEnabled := deps.FeatureEnabled
	if featureEnabled == nil {
		featureEnabled = func() bool { return false }
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	service := &ShareService{
		registry: deps.Registry, store: deps.Store, authorizer: deps.Authorizer,
		limiter: deps.Limiter, proofSigner: deps.ProofSigner, transport: deps.Transport,
		featureEnabled: featureEnabled, now: now,
	}
	if featureEnabled() && !service.ready() {
		return nil, fmt.Errorf("%w: enabled service has missing dependencies", ErrShareConfig)
	}
	return service, nil
}

func (s *ShareService) Share(
	ctx context.Context,
	loginUID, spaceID, compactIntent, requestID string,
) (*ShareResult, error) {
	if s == nil || s.featureEnabled == nil || !s.featureEnabled() {
		return nil, ErrShareDisabled
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context unavailable", ErrShareConfig)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.ready() {
		return nil, ErrShareConfig
	}
	if !validAuditValue(requestID, 0, 128) {
		return nil, fmt.Errorf("%w: request id invalid", ErrIntentInvalid)
	}
	verified, err := s.registry.VerifyIntent(ctx, compactIntent, s.now())
	if err != nil {
		return nil, err
	}
	if loginUID == "" || spaceID == "" || verified.Intent.ActorUID != loginUID || verified.Intent.SpaceID != spaceID {
		return nil, ErrShareForbidden
	}
	intentClaim, err := s.store.ClaimIntent(ctx, *verified)
	if err != nil {
		return nil, err
	}
	provider, err := s.registry.Provider(verified.ProviderID)
	if err != nil {
		return nil, err
	}
	prepared, err := provider.spec.Adapter.Revalidate(ctx, *verified)
	if err != nil || !exactDisclosureSet(verified.Intent.Targets, prepared) {
		return nil, fmt.Errorf("%w", ErrProviderRevalidation)
	}
	link, err := provider.spec.BuildDeepLink(verified.Intent.Resource)
	if err != nil || !safeResourceLink(link) {
		return nil, fmt.Errorf("%w: deep link", ErrProviderRevalidation)
	}
	card, err := provider.spec.RenderCard(prepared.Card, link)
	if err != nil || len(card) == 0 {
		return nil, fmt.Errorf("%w: card rendering", ErrProviderRevalidation)
	}

	result := &ShareResult{Results: make([]TargetResult, 0, len(verified.Intent.Targets))}
	for index, target := range verified.Intent.Targets {
		result.Results = append(result.Results, s.shareTarget(
			ctx, intentClaim.IntentID, *verified, target, prepared.Disclosures[index].Allowed, card, requestID,
		))
	}
	return result, nil
}

func (s *ShareService) shareTarget(
	ctx context.Context,
	intentID int64,
	verified VerifiedIntent,
	target Target,
	disclosureAllowed bool,
	card map[string]interface{},
	requestID string,
) TargetResult {
	claim, err := s.store.ClaimDelivery(ctx, intentID, verified, target, requestID)
	if err != nil {
		return TargetResult{Target: target, Outcome: ShareFailed}
	}
	if !claim.Created && claim.Record.State != DeliveryClaimed {
		return targetResultFromRecord(target, claim.Record, s.now())
	}
	rowID := claim.Record.ID
	if !disclosureAllowed {
		return s.finishPreTransport(ctx, rowID, target, DeliveryDenied, time.Time{}, "provider_denied", requestID)
	}
	if err := s.authorizer.Authorize(ctx, verified.Intent.ActorUID, verified.Intent.SpaceID, target); err != nil {
		if errors.Is(err, ErrTargetDenied) {
			return s.finishPreTransport(ctx, rowID, target, DeliveryDenied, time.Time{}, "target_denied", requestID)
		}
		return s.finishPreTransport(ctx, rowID, target, DeliveryFailed, s.now().Add(5*time.Second), "authorization_unavailable", requestID)
	}
	limit, limitErr := s.limiter.Allow(ctx, LimitRequest{
		ActorUID: verified.Intent.ActorUID, SpaceID: verified.Intent.SpaceID,
		ProviderID: verified.ProviderID, Target: target,
	})
	if limitErr != nil || !limit.Allowed {
		retry := boundedRetryAfter(limit.RetryAfter)
		return s.finishPreTransport(ctx, rowID, target, DeliveryRateLimited, s.now().Add(retry), "rate_limited", requestID)
	}

	envelope := map[string]interface{}{
		"type": cardmsg.InteractiveCard.Int(), "card_version": cardmsg.CardVersion,
		"profile": cardmsg.ProfileV1, "card": card,
	}
	sealed, err := s.proofSigner.Seal(envelope, ProofContext{
		ActorUID: verified.Intent.ActorUID, SpaceID: verified.Intent.SpaceID,
		ProviderID: verified.ProviderID, Resource: verified.Intent.Resource,
		Target: target, DeliveryID: claim.Record.DeliveryID,
	})
	if err != nil {
		return s.finishPreTransport(ctx, rowID, target, DeliveryFailed, s.now().Add(5*time.Second), "proof_unavailable", requestID)
	}
	if err := s.store.BeginDispatch(ctx, rowID, requestID); err != nil {
		if record, loadErr := s.store.LoadDelivery(ctx, rowID); loadErr == nil {
			return targetResultFromRecord(target, *record, s.now())
		}
		return TargetResult{Target: target, Outcome: ShareFailed}
	}
	channelID, channelType, err := transportTarget(target)
	if err != nil {
		_ = s.store.MarkUnknown(ctx, rowID, "target_encoding_failed", requestID)
		return TargetResult{Target: target, Outcome: ShareUnknown}
	}
	wire, err := json.Marshal(sealed)
	if err != nil {
		_ = s.store.MarkUnknown(ctx, rowID, "payload_encoding_failed", requestID)
		return TargetResult{Target: target, Outcome: ShareUnknown}
	}
	response, sendErr := s.transport.SendMessageWithResult(&config.MsgSendReq{
		Header: config.MsgHeader{RedDot: 1}, FromUID: verified.Intent.ActorUID,
		ChannelID: channelID, ChannelType: channelType, Payload: wire,
	})
	if sendErr != nil || response == nil {
		_ = s.store.MarkUnknown(ctx, rowID, "transport_ambiguous", requestID)
		return TargetResult{Target: target, Outcome: ShareUnknown}
	}
	transportResult := TransportResult{
		MessageID: strconv.FormatInt(response.MessageID, 10), MessageSeq: response.MessageSeq,
		ClientMsgNo: response.ClientMsgNo,
	}
	if err := s.store.MarkSent(ctx, rowID, transportResult, requestID); err != nil {
		_ = s.store.MarkUnknown(ctx, rowID, "store_confirm_failed", requestID)
		return TargetResult{Target: target, Outcome: ShareUnknown}
	}
	return TargetResult{
		Target: target, Outcome: ShareSent, MessageID: transportResult.MessageID,
		MessageSeq: transportResult.MessageSeq,
	}
}

func (s *ShareService) finishPreTransport(
	ctx context.Context,
	rowID int64,
	target Target,
	state DeliveryState,
	retryAt time.Time,
	code, requestID string,
) TargetResult {
	if err := s.store.RecordPreTransportOutcome(ctx, rowID, state, retryAt, code, requestID); err != nil {
		return TargetResult{Target: target, Outcome: ShareFailed}
	}
	record := DeliveryRecord{State: state, RetryAt: retryAt.Unix(), OutcomeCode: code}
	return targetResultFromRecord(target, record, s.now())
}

func targetResultFromRecord(target Target, record DeliveryRecord, now time.Time) TargetResult {
	result := TargetResult{Target: target, MessageID: record.MessageID, MessageSeq: record.MessageSeq}
	switch record.State {
	case DeliverySent:
		result.Outcome = ShareAlreadySent
	case DeliveryDenied:
		result.Outcome = ShareDenied
	case DeliveryRateLimited:
		result.Outcome = ShareRateLimited
		if record.RetryAt > 0 {
			result.RetryAfterSeconds = maxInt64(1, record.RetryAt-now.Unix())
		}
	case DeliveryFailed:
		result.Outcome = ShareFailed
	case DeliveryDispatching, DeliveryUnknown:
		result.Outcome = ShareUnknown
	default:
		result.Outcome = ShareFailed
	}
	return result
}

func exactDisclosureSet(targets []Target, prepared *RevalidatedResource) bool {
	if prepared == nil || len(prepared.Disclosures) != len(targets) {
		return false
	}
	for index := range targets {
		if prepared.Disclosures[index].Target != targets[index] {
			return false
		}
	}
	return true
}

func safeResourceLink(link *url.URL) bool {
	return link != nil && link.Scheme == "https" && link.Host != "" && link.User == nil
}

func transportTarget(target Target) (string, uint8, error) {
	switch target.Kind {
	case TargetDM:
		return target.PeerUID, common.ChannelTypePerson.Uint8(), nil
	case TargetGroup:
		return target.GroupNo, common.ChannelTypeGroup.Uint8(), nil
	case TargetThread:
		return thread.BuildChannelID(target.GroupNo, target.ShortID), common.ChannelTypeCommunityTopic.Uint8(), nil
	default:
		return "", 0, errors.New("unsupported target")
	}
}

func boundedRetryAfter(input time.Duration) time.Duration {
	if input < time.Second {
		return time.Second
	}
	if input > time.Minute {
		return time.Minute
	}
	return input
}

func retrySeconds(input time.Duration) int64 {
	return int64(math.Ceil(input.Seconds()))
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (s *ShareService) ready() bool {
	return s != nil && s.registry != nil && s.store != nil && s.authorizer != nil &&
		s.limiter != nil && s.proofSigner != nil && s.transport != nil && s.now != nil
}
