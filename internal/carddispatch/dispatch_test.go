package carddispatch

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeIdentityResolver struct {
	identity *botidentity.Identity
	err      error
	calls    atomic.Int32
	hook     func()
}

func (f *fakeIdentityResolver) Resolve(string) (*botidentity.Identity, error) {
	f.calls.Add(1)
	if f.hook != nil {
		f.hook()
	}
	return f.identity, f.err
}

type fakeAuthorizer struct {
	err    error
	calls  atomic.Int32
	target Target
	policy AuthorizationPolicy
	hook   func()
}

func (f *fakeAuthorizer) Authorize(_ context.Context, _ *botidentity.Identity, target Target, policy AuthorizationPolicy) error {
	f.calls.Add(1)
	f.target = target
	f.policy = policy
	if f.hook != nil {
		f.hook()
	}
	return f.err
}

type fakeTransport struct {
	mu      sync.Mutex
	calls   int
	req     *config.MsgSendReq
	resp    *config.MsgSendResp
	err     error
	started chan struct{}
	release chan struct{}
}

type fakeLogger struct {
	infoCalls  atomic.Int32
	errorCalls atomic.Int32
}

func (f *fakeLogger) Info(string, ...zap.Field)  { f.infoCalls.Add(1) }
func (f *fakeLogger) Debug(string, ...zap.Field) {}
func (f *fakeLogger) Error(string, ...zap.Field) { f.errorCalls.Add(1) }
func (f *fakeLogger) Warn(string, ...zap.Field)  {}

type fakeValueStore struct {
	mu     sync.Mutex
	values map[string]interface{}
}

func (f *fakeValueStore) SetValue(value interface{}, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.values == nil {
		f.values = make(map[string]interface{})
	}
	f.values[key] = value
}

func (f *fakeValueStore) Value(key string) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[key]
}

func (f *fakeTransport) SendMessageWithResult(req *config.MsgSendReq) (*config.MsgSendResp, error) {
	f.mu.Lock()
	f.calls++
	copyReq := *req
	copyReq.Payload = append([]byte(nil), req.Payload...)
	f.req = &copyReq
	started := f.started
	release := f.release
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	return f.resp, f.err
}

func (f *fakeTransport) snapshot() (int, *config.MsgSendReq) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.req
}

func validCardDocument() json.RawMessage {
	return json.RawMessage(`{"type":"AdaptiveCard","version":"1.5","body":[{"type":"TextBlock","text":"**hello**"}]}`)
}

func validSpec() ProducerSpec {
	return ProducerSpec{
		ID:                  "summary-notify",
		Enabled:             true,
		SenderUID:           "summary",
		AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
		AllowedProfiles:     []string{cardmsg.ProfileV1},
		SpacePolicy:         SpacePolicySystemNotification,
		GroupPolicy:         GroupPolicyMemberRequired,
		MaxInFlight:         1,
	}
}

func validTarget() Target {
	return Target{SpaceID: "space-a", ChannelID: "user-a", ChannelType: common.ChannelTypePerson.Uint8()}
}

func newHarness(t *testing.T, spec ProducerSpec) (*Registry, *fakeIdentityResolver, *fakeAuthorizer, *fakeTransport, *prometheus.Registry) {
	t.Helper()
	resolver := &fakeIdentityResolver{identity: &botidentity.Identity{
		UID:        spec.SenderUID,
		Kind:       botidentity.KindUserBot,
		CreatorUID: "owner-a",
	}}
	authorizer := &fakeAuthorizer{}
	transport := &fakeTransport{resp: &config.MsgSendResp{MessageID: 42, MessageSeq: 7, ClientMsgNo: "client-42"}}
	promRegistry := prometheus.NewRegistry()
	registry := NewRegistry(Dependencies{
		IdentityResolver: resolver,
		Authorizer:       authorizer,
		Transport:        transport,
		Metrics:          NewMetrics(promRegistry),
		FeatureEnabled:   func() bool { return true },
	}, []ProducerSpec{spec})
	return registry, resolver, authorizer, transport, promRegistry
}

func requireCategory(t *testing.T, want Category, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, want, CategoryOf(err), "err=%v", err)
}

func TestRegistryFailsClosedForUnavailableProducer(t *testing.T) {
	base := validSpec()
	cases := []struct {
		name  string
		specs []ProducerSpec
		id    ProducerID
	}{
		{name: "unknown", specs: []ProducerSpec{base}, id: "missing"},
		{name: "disabled", specs: []ProducerSpec{func() ProducerSpec { s := base; s.Enabled = false; return s }()}, id: base.ID},
		{name: "missing sender config", specs: []ProducerSpec{func() ProducerSpec { s := base; s.SenderUID = ""; return s }()}, id: base.ID},
		{name: "duplicate", specs: []ProducerSpec{base, base}, id: base.ID},
		{name: "invalid concurrency", specs: []ProducerSpec{func() ProducerSpec { s := base; s.MaxInFlight = 0; return s }()}, id: base.ID},
		{name: "interactive without owner", specs: []ProducerSpec{func() ProducerSpec { s := base; s.AllowedProfiles = []string{cardmsg.ProfileV2}; return s }()}, id: base.ID},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry(Dependencies{}, tc.specs)
			sender, err := reg.Sender(tc.id)
			assert.Nil(t, sender)
			requireCategory(t, CategoryProducerDisabled, err)
		})
	}
}

func TestDuplicateProducerEmitsOneConfigurationError(t *testing.T) {
	spec := validSpec()
	promRegistry := prometheus.NewRegistry()
	logger := &fakeLogger{}
	registry := NewRegistry(Dependencies{
		Metrics: NewMetrics(promRegistry),
		Logger:  logger,
	}, []ProducerSpec{spec, spec})

	sender, err := registry.Sender(spec.ID)
	assert.Nil(t, sender)
	requireCategory(t, CategoryProducerDisabled, err)
	assert.Equal(t, int32(1), logger.errorCalls.Load())
	families, err := promRegistry.Gather()
	require.NoError(t, err)
	assert.Equal(t, float64(1), gatheredCounter(t, families,
		"dmwork_card_dispatch_config_error_total",
		map[string]string{"producer": "summary-notify", "reason": "duplicate"}))
}

func TestRegistryRejectsMalformedInteractiveActionOwner(t *testing.T) {
	spec := validSpec()
	spec.AllowedProfiles = []string{cardmsg.ProfileV2}
	spec.ActionEventOwner = "../../unbounded callback"
	registry, _, _, _, _ := newHarness(t, spec)

	sender, err := registry.Sender(spec.ID)
	assert.Nil(t, sender)
	requireCategory(t, CategoryProducerDisabled, err)
}

func TestRegistryRejectsEveryInvalidConfigurationClass(t *testing.T) {
	base := validSpec()
	cases := []struct {
		name   string
		mutate func(*ProducerSpec)
	}{
		{name: "invalid id", mutate: func(s *ProducerSpec) { s.ID = "Invalid" }},
		{name: "synthetic webhook sender", mutate: func(s *ProducerSpec) { s.SenderUID = "iwh_not-a-bot" }},
		{name: "excessive concurrency", mutate: func(s *ProducerSpec) { s.MaxInFlight = 1001 }},
		{name: "missing target types", mutate: func(s *ProducerSpec) { s.AllowedChannelTypes = nil }},
		{name: "unsupported target type", mutate: func(s *ProducerSpec) { s.AllowedChannelTypes = []uint8{255} }},
		{name: "duplicate target type", mutate: func(s *ProducerSpec) { s.AllowedChannelTypes = []uint8{1, 1} }},
		{name: "missing profiles", mutate: func(s *ProducerSpec) { s.AllowedProfiles = nil }},
		{name: "unsupported profile", mutate: func(s *ProducerSpec) { s.AllowedProfiles = []string{"octo/unknown"} }},
		{name: "duplicate profile", mutate: func(s *ProducerSpec) { s.AllowedProfiles = []string{cardmsg.ProfileV1, cardmsg.ProfileV1} }},
		{name: "invalid space policy", mutate: func(s *ProducerSpec) { s.SpacePolicy = "invalid" }},
		{name: "invalid group policy", mutate: func(s *ProducerSpec) { s.GroupPolicy = "invalid" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			tc.mutate(&spec)
			registry, _, _, _, _ := newHarness(t, spec)
			sender, err := registry.Sender(spec.ID)
			assert.Nil(t, sender)
			requireCategory(t, CategoryProducerDisabled, err)
		})
	}
}

func TestInstallRegistryIsPerContextAndSingleAssignment(t *testing.T) {
	regA, _, _, _, _ := newHarness(t, validSpec())
	regB, _, _, _, _ := newHarness(t, validSpec())
	ctxA := &fakeValueStore{}
	ctxB := &fakeValueStore{}

	require.NoError(t, Install(ctxA, regA))
	require.ErrorIs(t, Install(ctxA, regB), ErrRegistryAlreadyInstalled)
	require.NoError(t, Install(ctxB, regB))

	senderA, err := SenderFromContext(ctxA, "summary-notify")
	require.NoError(t, err)
	senderB, err := SenderFromContext(ctxB, "summary-notify")
	require.NoError(t, err)
	assert.NotSame(t, senderA, senderB)
}

func TestRegistryCopiesProducerConfiguration(t *testing.T) {
	spec := validSpec()
	reg, _, _, _, _ := newHarness(t, spec)
	spec.SenderUID = "attacker"
	spec.AllowedProfiles[0] = cardmsg.ProfileV2
	spec.AllowedChannelTypes[0] = common.ChannelTypeGroup.Uint8()

	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)
	_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	require.NoError(t, err)
}

func TestSendBuildsAuthoritativeEnvelopeAndReturnsTransportResult(t *testing.T) {
	reg, resolver, authorizer, transport, _ := newHarness(t, validSpec())
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)

	result, err := sender.Send(context.Background(), validTarget(), Card{
		Profile:       cardmsg.ProfileV1,
		RenderProfile: cardmsg.RenderProfileOctoChatV1,
		Document:      validCardDocument(),
	})
	require.NoError(t, err)
	assert.Equal(t, &Result{MessageID: 42, MessageSeq: 7, ClientMsgNo: "client-42"}, result)
	assert.Equal(t, int32(1), resolver.calls.Load())
	assert.Equal(t, int32(1), authorizer.calls.Load())
	assert.Equal(t, SpacePolicySystemNotification, authorizer.policy.SpacePolicy)

	calls, req := transport.snapshot()
	require.Equal(t, 1, calls)
	require.NotNil(t, req)
	assert.Equal(t, "summary", req.FromUID)
	assert.Equal(t, "user-a", req.ChannelID)
	assert.Equal(t, common.ChannelTypePerson.Uint8(), req.ChannelType)
	assert.Empty(t, req.StreamNo)
	assert.Empty(t, req.Subscribers)

	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(req.Payload, &payload))
	assert.Equal(t, float64(cardmsg.InteractiveCard.Int()), payload["type"])
	assert.Equal(t, cardmsg.CardVersion, payload["card_version"])
	assert.Equal(t, cardmsg.ProfileV1, payload["profile"])
	assert.Equal(t, cardmsg.RenderProfileOctoChatV1, payload["render_profile"])
	assert.Equal(t, "space-a", payload["space_id"])
	assert.Equal(t, "hello", payload["plain"])
	assert.NotContains(t, payload, "from_uid")
	assert.NotContains(t, payload, "on_behalf_of")
	assert.NotContains(t, payload, "mention")
	require.NoError(t, cardmsg.Validate(payload))
	require.NoError(t, cardmsg.RecheckPayloadSize(payload))
	assert.LessOrEqual(t, len(req.Payload), cardmsg.MaxPayloadBytes)
}

func TestSendAuthorsDefaultRenderProfile(t *testing.T) {
	reg, _, _, transport, _ := newHarness(t, validSpec())
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)

	_, err = sender.Send(context.Background(), validTarget(), Card{
		Profile:  cardmsg.ProfileV1,
		Document: validCardDocument(),
	})
	require.NoError(t, err)

	calls, req := transport.snapshot()
	require.Equal(t, 1, calls)
	require.NotNil(t, req)

	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(req.Payload, &payload))
	assert.Equal(t, cardmsg.RenderProfileOctoChatV1, payload["render_profile"])
}

func TestSendFailClosedStagesNeverReachTransport(t *testing.T) {
	tests := []struct {
		name          string
		mutate        func(*ProducerSpec, *fakeIdentityResolver, *fakeAuthorizer, *fakeTransport)
		target        Target
		card          Card
		want          Category
		wantResolver  int32
		wantAuthorize int32
	}{
		{name: "invalid target", target: Target{}, card: Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()}, want: CategoryInvalidRequest},
		{name: "profile denied", target: validTarget(), card: Card{Profile: cardmsg.ProfileV2, Document: validCardDocument()}, want: CategoryProducerDisabled},
		{name: "identity missing", mutate: func(_ *ProducerSpec, r *fakeIdentityResolver, _ *fakeAuthorizer, _ *fakeTransport) { r.identity = nil }, target: validTarget(), card: Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()}, want: CategoryIdentityUntrusted, wantResolver: 1},
		{name: "identity lookup error", mutate: func(_ *ProducerSpec, r *fakeIdentityResolver, _ *fakeAuthorizer, _ *fakeTransport) {
			r.err = errors.New("db down")
		}, target: validTarget(), card: Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()}, want: CategoryIdentityUntrusted, wantResolver: 1},
		{name: "acl denied", mutate: func(_ *ProducerSpec, _ *fakeIdentityResolver, a *fakeAuthorizer, _ *fakeTransport) {
			a.err = ErrTargetDenied
		}, target: validTarget(), card: Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()}, want: CategoryTargetDenied, wantResolver: 1, wantAuthorize: 1},
		{name: "invalid card after authorization", target: validTarget(), card: Card{Profile: cardmsg.ProfileV1, Document: json.RawMessage(`{"type":"AdaptiveCard","version":"1.5","body":[{"type":"Unknown"}]}`)}, want: CategoryCardInvalid, wantResolver: 1, wantAuthorize: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := validSpec()
			reg, resolver, authorizer, transport, _ := newHarness(t, spec)
			if tc.mutate != nil {
				tc.mutate(&spec, resolver, authorizer, transport)
			}
			sender, err := reg.Sender("summary-notify")
			require.NoError(t, err)
			result, err := sender.Send(context.Background(), tc.target, tc.card)
			assert.Nil(t, result)
			requireCategory(t, tc.want, err)
			assert.Equal(t, tc.wantResolver, resolver.calls.Load())
			assert.Equal(t, tc.wantAuthorize, authorizer.calls.Load())
			calls, _ := transport.snapshot()
			assert.Zero(t, calls)
		})
	}
}

func TestSendFeatureGatePrecedesIdentityLookup(t *testing.T) {
	spec := validSpec()
	resolver := &fakeIdentityResolver{identity: &botidentity.Identity{UID: spec.SenderUID, Kind: botidentity.KindUserBot}}
	authorizer := &fakeAuthorizer{}
	transport := &fakeTransport{}
	reg := NewRegistry(Dependencies{
		IdentityResolver: resolver,
		Authorizer:       authorizer,
		Transport:        transport,
		FeatureEnabled:   func() bool { return false },
	}, []ProducerSpec{spec})
	sender, err := reg.Sender(spec.ID)
	require.NoError(t, err)

	_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	requireCategory(t, CategoryFeatureDisabled, err)
	assert.Zero(t, resolver.calls.Load())
	assert.Zero(t, authorizer.calls.Load())
	calls, _ := transport.snapshot()
	assert.Zero(t, calls)
}

func TestSendRejectsInvalidContextIdentityAndTransportResult(t *testing.T) {
	t.Run("cancelled before identity lookup", func(t *testing.T) {
		reg, resolver, _, transport, _ := newHarness(t, validSpec())
		sender, err := reg.Sender("summary-notify")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err = sender.Send(ctx, validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
		requireCategory(t, CategoryDispatchFailed, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, resolver.calls.Load())
		calls, _ := transport.snapshot()
		assert.Zero(t, calls)
	})

	t.Run("identity uid mismatch", func(t *testing.T) {
		reg, resolver, _, transport, _ := newHarness(t, validSpec())
		resolver.identity.UID = "different-bot"
		sender, err := reg.Sender("summary-notify")
		require.NoError(t, err)
		_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
		requireCategory(t, CategoryIdentityUntrusted, err)
		calls, _ := transport.snapshot()
		assert.Zero(t, calls)
	})

	t.Run("unsupported identity kind", func(t *testing.T) {
		reg, resolver, _, transport, _ := newHarness(t, validSpec())
		resolver.identity.Kind = "unknown"
		sender, err := reg.Sender("summary-notify")
		require.NoError(t, err)
		_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
		requireCategory(t, CategoryIdentityUntrusted, err)
		calls, _ := transport.snapshot()
		assert.Zero(t, calls)
	})

	t.Run("empty transport result", func(t *testing.T) {
		reg, _, _, transport, _ := newHarness(t, validSpec())
		transport.resp = nil
		sender, err := reg.Sender("summary-notify")
		require.NoError(t, err)
		_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
		requireCategory(t, CategoryDispatchFailed, err)
		calls, _ := transport.snapshot()
		assert.Equal(t, 1, calls)
	})
}

func TestSendRejectsMalformedRequestsAndDocuments(t *testing.T) {
	reg, _, _, transport, _ := newHarness(t, validSpec())
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)
	cases := []struct {
		name   string
		ctx    context.Context
		target Target
		card   Card
		want   Category
	}{
		{name: "nil context", target: validTarget(), card: Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()}, want: CategoryInvalidRequest},
		{name: "unsupported target", ctx: context.Background(), target: Target{SpaceID: "space-a", ChannelID: "x", ChannelType: 255}, card: Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()}, want: CategoryInvalidRequest},
		{name: "missing document", ctx: context.Background(), target: validTarget(), card: Card{Profile: cardmsg.ProfileV1}, want: CategoryInvalidRequest},
		{name: "unsupported render profile", ctx: context.Background(), target: validTarget(), card: Card{Profile: cardmsg.ProfileV1, RenderProfile: "octo-chat/v2", Document: validCardDocument()}, want: CategoryInvalidRequest},
		{name: "malformed json", ctx: context.Background(), target: validTarget(), card: Card{Profile: cardmsg.ProfileV1, Document: json.RawMessage(`{"type":`)}, want: CategoryCardInvalid},
		{name: "empty object", ctx: context.Background(), target: validTarget(), card: Card{Profile: cardmsg.ProfileV1, Document: json.RawMessage(`{}`)}, want: CategoryCardInvalid},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sender.Send(tc.ctx, tc.target, tc.card)
			requireCategory(t, tc.want, err)
		})
	}
	calls, _ := transport.snapshot()
	assert.Zero(t, calls)
}

func TestSendEmitsOneSafeTerminalLog(t *testing.T) {
	build := func(transport *fakeTransport, logger *fakeLogger) Sender {
		spec := validSpec()
		registry := NewRegistry(Dependencies{
			IdentityResolver: &fakeIdentityResolver{identity: &botidentity.Identity{UID: spec.SenderUID, Kind: botidentity.KindUserBot}},
			Authorizer:       &fakeAuthorizer{},
			Transport:        transport,
			Metrics:          NewMetrics(prometheus.NewRegistry()),
			Logger:           logger,
			FeatureEnabled:   func() bool { return true },
		}, []ProducerSpec{spec})
		sender, err := registry.Sender(spec.ID)
		require.NoError(t, err)
		return sender
	}

	successLogger := &fakeLogger{}
	successTransport := &fakeTransport{resp: &config.MsgSendResp{MessageID: 1}}
	_, err := build(successTransport, successLogger).Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	require.NoError(t, err)
	assert.Equal(t, int32(1), successLogger.infoCalls.Load())
	assert.Zero(t, successLogger.errorCalls.Load())

	failureLogger := &fakeLogger{}
	failureTransport := &fakeTransport{err: errors.New("transport failed")}
	_, err = build(failureTransport, failureLogger).Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	requireCategory(t, CategoryDispatchFailed, err)
	assert.Zero(t, failureLogger.infoCalls.Load())
	assert.Equal(t, int32(1), failureLogger.errorCalls.Load())
}

func TestSendOwnsImmutableDocumentSnapshot(t *testing.T) {
	document := validCardDocument()
	original := append([]byte(nil), document...)
	reg, resolver, _, transport, _ := newHarness(t, validSpec())
	resolver.hook = func() {
		for i := range document {
			document[i] = ' '
		}
	}
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)

	_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: document})
	require.NoError(t, err)
	_, req := transport.snapshot()
	require.NotNil(t, req)
	assert.Contains(t, string(req.Payload), "hello")
	assert.JSONEq(t, string(original), string(validCardDocument()))
}

func TestSendRejectsPayloadThatOverflowsAfterAuthoritativeEnrichment(t *testing.T) {
	padding := strings.Repeat("x", cardmsg.MaxPayloadBytes)
	doc := json.RawMessage(`{"type":"AdaptiveCard","version":"1.5","padding":"` + padding + `","body":[]}`)
	for len(doc) > 0 {
		var card map[string]interface{}
		require.NoError(t, json.Unmarshal(doc, &card))
		base := map[string]interface{}{
			"type":         cardmsg.InteractiveCard.Int(),
			"card_version": cardmsg.CardVersion,
			"profile":      cardmsg.ProfileV1,
			"card":         card,
		}
		raw, err := json.Marshal(base)
		require.NoError(t, err)
		delta := len(raw) - (cardmsg.MaxPayloadBytes - 1)
		if delta == 0 {
			break
		}
		padding = padding[:len(padding)-delta]
		doc = json.RawMessage(`{"type":"AdaptiveCard","version":"1.5","padding":"` + padding + `","body":[]}`)
	}

	reg, _, _, transport, _ := newHarness(t, validSpec())
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)
	_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: doc})
	requireCategory(t, CategoryPayloadTooLarge, err)
	calls, _ := transport.snapshot()
	assert.Zero(t, calls)
}

func TestSendChecksCancellationImmediatelyBeforeTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reg, _, authorizer, transport, _ := newHarness(t, validSpec())
	authorizer.hook = cancel
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)

	_, err = sender.Send(ctx, validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	requireCategory(t, CategoryDispatchFailed, err)
	assert.ErrorIs(t, err, context.Canceled)
	calls, _ := transport.snapshot()
	assert.Zero(t, calls)
}

func TestSendMakesOneTransportAttemptWithoutRetry(t *testing.T) {
	reg, _, _, transport, _ := newHarness(t, validSpec())
	transport.err = errors.New("ambiguous transport failure")
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)

	result, err := sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	assert.Nil(t, result)
	requireCategory(t, CategoryDispatchFailed, err)
	calls, _ := transport.snapshot()
	assert.Equal(t, 1, calls)
}

func TestSendConcurrencyLimitFailsFastAndReleasesSlot(t *testing.T) {
	reg, _, _, transport, _ := newHarness(t, validSpec())
	transport.started = make(chan struct{}, 1)
	transport.release = make(chan struct{})
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)

	firstDone := make(chan error, 1)
	go func() {
		_, sendErr := sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
		firstDone <- sendErr
	}()
	<-transport.started

	_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	requireCategory(t, CategoryBusy, err)
	close(transport.release)
	require.NoError(t, <-firstDone)

	transport.release = nil
	_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	require.NoError(t, err)
	calls, _ := transport.snapshot()
	assert.Equal(t, 2, calls)
}

func TestMetricsUseBoundedLabelsAndOneTerminalResult(t *testing.T) {
	reg, _, _, _, promRegistry := newHarness(t, validSpec())
	sender, err := reg.Sender("summary-notify")
	require.NoError(t, err)
	_, err = sender.Send(context.Background(), validTarget(), Card{Profile: cardmsg.ProfileV1, Document: validCardDocument()})
	require.NoError(t, err)

	families, err := promRegistry.Gather()
	require.NoError(t, err)
	wantLabels := map[string]map[string]bool{
		"dmwork_card_dispatch_attempt_total":    {"producer": true, "target": true},
		"dmwork_card_dispatch_result_total":     {"producer": true, "target": true, "result": true},
		"dmwork_card_dispatch_duration_seconds": {"producer": true, "target": true, "result": true},
		"dmwork_card_dispatch_in_flight":        {"producer": true},
	}
	for _, family := range families {
		allowed, ok := wantLabels[family.GetName()]
		if !ok {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				assert.True(t, allowed[label.GetName()], "%s has forbidden label %s", family.GetName(), label.GetName())
				assert.NotContains(t, []string{"uid", "channel_id", "space_id", "message_id"}, label.GetName())
			}
		}
	}

	assert.Equal(t, float64(1), gatheredCounter(t, families, "dmwork_card_dispatch_attempt_total", map[string]string{"producer": "summary-notify", "target": "person"}))
	assert.Equal(t, float64(1), gatheredCounter(t, families, "dmwork_card_dispatch_result_total", map[string]string{"producer": "summary-notify", "target": "person", "result": "ok"}))
	assert.Equal(t, float64(0), gatheredGauge(t, families, "dmwork_card_dispatch_in_flight", map[string]string{"producer": "summary-notify"}))
}

func gatheredCounter(t *testing.T, families []*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if metricHasLabels(metric.Label, labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("counter %s labels=%v not found", name, labels)
	return 0
}

func gatheredGauge(t *testing.T, families []*dto.MetricFamily, name string, labels map[string]string) float64 {
	t.Helper()
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if metricHasLabels(metric.Label, labels) {
				return metric.GetGauge().GetValue()
			}
		}
	}
	t.Fatalf("gauge %s labels=%v not found", name, labels)
	return 0
}

func metricHasLabels(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, pair := range pairs {
		if want[pair.GetName()] != pair.GetValue() {
			return false
		}
	}
	return true
}

// ---- PR-C Slice 1 (D3): the trusted dispatch boundary authors catalog
// provenance from the registered ProducerSpec and the authorized target Space.
// Business callers cannot self-report a principal. ----

func registryCardDocument() json.RawMessage {
	return json.RawMessage(`{
		"type":"AdaptiveCard","version":"1.5",
		"body":[{"type":"TextBlock","text":"pending"}],
		"metadata":{"octo":{
			"protocol":"octo-card@1.0",
			"template":{"id":"docs.access-request","version":"0.3.0"}
		}}
	}`)
}

func TestSendAuthorsCatalogProvenanceForRegistryDocuments(t *testing.T) {
	registry, _, _, transport, _ := newHarness(t, validSpec())
	sender, err := registry.Sender("summary-notify")
	require.NoError(t, err)

	_, err = sender.Send(context.Background(), validTarget(), Card{
		Profile: cardmsg.ProfileV1, Document: registryCardDocument(),
	})
	require.NoError(t, err)
	_, req := transport.snapshot()
	require.NotNil(t, req)
	var wire map[string]interface{}
	require.NoError(t, json.Unmarshal(req.Payload, &wire))

	ref, _ := wire["template_ref"].(map[string]interface{})
	require.NotNil(t, ref, "server-authored template_ref missing: %s", req.Payload)
	assert.Equal(t, "docs.access-request", ref["id"])
	assert.Equal(t, "0.3.0", ref["version"])

	provenance, err := cardmsg.ParseCatalogProvenance(wire["catalog_provenance"])
	require.NoError(t, err, "authored provenance must be canonical: %#v", wire["catalog_provenance"])
	assert.Equal(t, cardmsg.CatalogPrincipalWireInternalProducer, provenance.PrincipalType)
	assert.Equal(t, "summary-notify", provenance.PrincipalID, "principal is the registered ProducerSpec.ID")
	assert.Equal(t, "space-a", provenance.SpaceID, "space is the authorized target Space")

	// The outbound frame must satisfy the same strict marker contract the
	// action ingress will later enforce.
	markers, err := cardmsg.CatalogFrameMarkers(req.Payload)
	require.NoError(t, err)
	assert.True(t, markers.HasRef)
	assert.True(t, markers.HasProvenance)
}

// PR-C review round 6 (yujiawei P1-1): the marker-authoring boundary must be
// unable to author a marker its readers reject.
//
// cardmsg.Validate runs before these keys exist and Finalize does not
// re-validate, so before this guard an untrimmed target Space produced a stored
// frame that every reader refuses — ParseCatalogProvenance requires a trimmed
// space_id. The card rendered and then nothing about it worked: every button
// click and every edit failed permanently, with no way to repair the message.
//
// Reaching it needed only an untrimmed Space to survive the membership lookup,
// and whether it does is decided by a collation the application does not
// choose. space_member declares none and inherits the database default; CI
// creates that database utf8mb4_general_ci, which is PAD SPACE, so 'space-a '
// matches 'space-a'. Measured both ways: PAD SPACE matches, and the MySQL 8.0
// default utf8mb4_0900_ai_ci (NO PAD) does not. Refusing the send here removes
// the dependency on that fact entirely.
func TestSendRefusesToAuthorAMarkerItsReadersWouldReject(t *testing.T) {
	registry, _, _, transport, _ := newHarness(t, validSpec())
	sender, err := registry.Sender("summary-notify")
	require.NoError(t, err)

	untrimmed := validTarget()
	untrimmed.SpaceID = "space-a "

	_, err = sender.Send(context.Background(), untrimmed, Card{
		Profile: cardmsg.ProfileV1, Document: registryCardDocument(),
	})
	require.Error(t, err, "an untrimmed Space produced a card whose markers no reader accepts")
	require.ErrorIs(t, err, cardmsg.ErrCatalogMarkerInvalid)

	// Nothing was delivered: the send fails instead of storing a broken card.
	_, req := transport.snapshot()
	assert.Nil(t, req, "a frame with an unreadable marker reached the transport")
}

func TestSendLeavesLegacyDocumentsUnmarked(t *testing.T) {
	registry, _, _, transport, _ := newHarness(t, validSpec())
	sender, err := registry.Sender("summary-notify")
	require.NoError(t, err)

	_, err = sender.Send(context.Background(), validTarget(), Card{
		Profile: cardmsg.ProfileV1, Document: validCardDocument(),
	})
	require.NoError(t, err)
	_, req := transport.snapshot()
	require.NotNil(t, req)
	var wire map[string]interface{}
	require.NoError(t, json.Unmarshal(req.Payload, &wire))
	_, hasRef := wire["template_ref"]
	_, hasProvenance := wire["catalog_provenance"]
	assert.False(t, hasRef, "legacy document grew template_ref")
	assert.False(t, hasProvenance, "legacy document grew catalog_provenance")
}

func TestProducerBindingExposesReadOnlySenderFacts(t *testing.T) {
	enabled := validSpec()
	disabled := validSpec()
	disabled.ID = "docs-notify"
	disabled.SenderUID = "notification"
	disabled.Enabled = false
	registry := NewRegistry(Dependencies{
		IdentityResolver: &fakeIdentityResolver{identity: &botidentity.Identity{UID: "summary", Kind: botidentity.KindUserBot}},
		Authorizer:       &fakeAuthorizer{},
		Transport:        &fakeTransport{},
		Metrics:          NewMetrics(prometheus.NewRegistry()),
	}, []ProducerSpec{enabled, disabled})

	sender, ok := registry.ProducerBinding("summary-notify")
	require.True(t, ok)
	assert.Equal(t, "summary", sender)

	// A disabled producer cannot send, but its declared identity binding is
	// still a fact the action ingress may verify historical frames against.
	sender, ok = registry.ProducerBinding("docs-notify")
	require.True(t, ok)
	assert.Equal(t, "notification", sender)

	_, ok = registry.ProducerBinding("never-registered")
	assert.False(t, ok)

	store := &fakeValueStore{}
	require.NoError(t, Install(store, registry))
	sender, ok = ProducerBindingFromContext(store, "summary-notify")
	require.True(t, ok)
	assert.Equal(t, "summary", sender)
	_, ok = ProducerBindingFromContext(store, "never-registered")
	assert.False(t, ok)
	_, ok = ProducerBindingFromContext(&fakeValueStore{}, "summary-notify")
	assert.False(t, ok, "missing registry must fail closed")
}

func TestProducerBindingRejectsAmbiguousOrInvalidSpecs(t *testing.T) {
	duplicateA := validSpec()
	duplicateB := validSpec()
	invalid := validSpec()
	invalid.ID = "docs-notify"
	invalid.SenderUID = "iwh_webhook"
	registry := NewRegistry(Dependencies{
		IdentityResolver: &fakeIdentityResolver{identity: &botidentity.Identity{UID: "summary", Kind: botidentity.KindUserBot}},
		Authorizer:       &fakeAuthorizer{},
		Transport:        &fakeTransport{},
		Metrics:          NewMetrics(prometheus.NewRegistry()),
	}, []ProducerSpec{duplicateA, duplicateB, invalid})
	if _, ok := registry.ProducerBinding("summary-notify"); ok {
		t.Fatal("duplicate producer ID must not expose a binding")
	}
	if _, ok := registry.ProducerBinding("docs-notify"); ok {
		t.Fatal("webhook-prefixed sender must not expose a binding")
	}
}
