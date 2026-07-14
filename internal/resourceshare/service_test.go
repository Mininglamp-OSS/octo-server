package resourceshare

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	jose "github.com/go-jose/go-jose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type serviceAdapter struct {
	result *RevalidatedResource
	err    error
	calls  int
}

func (a *serviceAdapter) Revalidate(context.Context, VerifiedIntent) (*RevalidatedResource, error) {
	a.calls++
	return a.result, a.err
}

type allowAuthorizer struct {
	err   error
	calls int
}

func (a *allowAuthorizer) Authorize(context.Context, string, string, Target) error {
	a.calls++
	return a.err
}

type allowLimiter struct {
	decision LimitDecision
	err      error
	calls    int
}

func (l *allowLimiter) Allow(context.Context, LimitRequest) (LimitDecision, error) {
	l.calls++
	if l.decision == (LimitDecision{}) && l.err == nil {
		return LimitDecision{Allowed: true}, nil
	}
	return l.decision, l.err
}

type captureTransport struct {
	requests []*config.MsgSendReq
	response *config.MsgSendResp
	err      error
}

func (t *captureTransport) SendMessageWithResult(req *config.MsgSendReq) (*config.MsgSendResp, error) {
	cloned := *req
	cloned.Payload = append([]byte(nil), req.Payload...)
	t.requests = append(t.requests, &cloned)
	if t.err != nil {
		return nil, t.err
	}
	return t.response, nil
}

type serviceHarness struct {
	service    *ShareService
	adapter    *serviceAdapter
	authorizer *allowAuthorizer
	limiter    *allowLimiter
	transport  *captureTransport
	verifier   *ProofVerifier
	intentKey  *ecdsa.PrivateKey
	now        time.Time
}

func newServiceHarness(t *testing.T, intent Intent, disclosures []TargetDisclosure) serviceHarness {
	t.Helper()
	store, _, now := newStoreHarness(t)
	intentKey := newIntentTestKey(t)
	proofKey := newIntentTestKey(t)
	adapter := &serviceAdapter{result: &RevalidatedResource{
		Card:        ResourceCardInput{Title: "Quarterly summary", Description: "Three decisions"},
		Disclosures: disclosures,
	}}
	spec := validProviderSpec(t, intentKey)
	spec.Adapter = adapter
	registry, err := NewRegistry([]ProviderSpec{spec})
	require.NoError(t, err)
	signer, err := NewProofSigner(ProofSigningKey{KeyID: "proof-key-1", PrivateKey: proofKey})
	require.NoError(t, err)
	verifier, err := NewProofVerifier([]ProofVerificationKey{{KeyID: "proof-key-1", PublicKey: &proofKey.PublicKey}})
	require.NoError(t, err)
	authorizer := &allowAuthorizer{}
	limiter := &allowLimiter{}
	transport := &captureTransport{response: &config.MsgSendResp{MessageID: 99, MessageSeq: 7, ClientMsgNo: "server-msg"}}
	service, err := NewShareService(ShareServiceDependencies{
		Registry:       registry,
		Store:          store,
		Authorizer:     authorizer,
		Limiter:        limiter,
		ProofSigner:    signer,
		Transport:      transport,
		FeatureEnabled: func() bool { return true },
		Now:            func() time.Time { return now },
	})
	require.NoError(t, err)
	_ = intent
	return serviceHarness{service, adapter, authorizer, limiter, transport, verifier, intentKey, now}
}

func disclosureFor(target Target, allowed bool) TargetDisclosure {
	return TargetDisclosure{Target: target, Allowed: allowed}
}

func TestShareService_SendsAsLoginUserAndReturnsPerTargetResults(t *testing.T) {
	intent := validIntent(time.Unix(1_800_000_000, 0).UTC())
	disclosures := []TargetDisclosure{
		disclosureFor(intent.Targets[0], true),
		disclosureFor(intent.Targets[1], false),
	}
	h := newServiceHarness(t, intent, disclosures)
	compact := signIntent(t, h.intentKey, jose.ES256, "intent-key-1", intent)

	result, err := h.service.Share(context.Background(), "user-a", "space-a", compact, "request-1")
	require.NoError(t, err)
	require.Len(t, result.Results, 2)
	assert.Equal(t, ShareSent, result.Results[0].Outcome)
	assert.Equal(t, "99", result.Results[0].MessageID)
	assert.Equal(t, ShareDenied, result.Results[1].Outcome)
	assert.Equal(t, 1, h.adapter.calls)
	require.Len(t, h.transport.requests, 1)

	request := h.transport.requests[0]
	assert.Equal(t, "user-a", request.FromUID)
	assert.Equal(t, "user-b", request.ChannelID)
	assert.Equal(t, common.ChannelTypePerson.Uint8(), request.ChannelType)
	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(request.Payload, &payload))
	require.NoError(t, h.verifier.Verify(payload, ProofObservation{
		ActorUID:  "user-a",
		ViewerUID: "user-a",
		SpaceID:   "space-a",
		Target:    Target{Kind: TargetDM, PeerUID: "user-b"},
	}))
}

func TestShareService_IdenticalRetryDoesNotResendSuccessfulTarget(t *testing.T) {
	intent := validIntent(time.Unix(1_800_000_000, 0).UTC())
	intent.Targets = []Target{{Kind: TargetGroup, GroupNo: "group-a"}}
	h := newServiceHarness(t, intent, []TargetDisclosure{disclosureFor(intent.Targets[0], true)})
	compact := signIntent(t, h.intentKey, jose.ES256, "intent-key-1", intent)

	first, err := h.service.Share(context.Background(), "user-a", "space-a", compact, "request-1")
	require.NoError(t, err)
	assert.Equal(t, ShareSent, first.Results[0].Outcome)
	second, err := h.service.Share(context.Background(), "user-a", "space-a", compact, "request-2")
	require.NoError(t, err)
	assert.Equal(t, ShareAlreadySent, second.Results[0].Outcome)
	assert.Len(t, h.transport.requests, 1)
}

func TestShareService_TransportFailureBecomesUnknownAndIsNotRetried(t *testing.T) {
	intent := validIntent(time.Unix(1_800_000_000, 0).UTC())
	intent.Targets = []Target{{Kind: TargetGroup, GroupNo: "group-a"}}
	h := newServiceHarness(t, intent, []TargetDisclosure{disclosureFor(intent.Targets[0], true)})
	h.transport.err = errors.New("ambiguous timeout")
	compact := signIntent(t, h.intentKey, jose.ES256, "intent-key-1", intent)

	first, err := h.service.Share(context.Background(), "user-a", "space-a", compact, "request-1")
	require.NoError(t, err)
	assert.Equal(t, ShareUnknown, first.Results[0].Outcome)
	second, err := h.service.Share(context.Background(), "user-a", "space-a", compact, "request-2")
	require.NoError(t, err)
	assert.Equal(t, ShareUnknown, second.Results[0].Outcome)
	assert.Len(t, h.transport.requests, 1)
}

func TestShareService_BindsLoginActorAndSpaceBeforeProviderOrStore(t *testing.T) {
	intent := validIntent(time.Unix(1_800_000_000, 0).UTC())
	h := newServiceHarness(t, intent, []TargetDisclosure{
		disclosureFor(intent.Targets[0], true), disclosureFor(intent.Targets[1], true),
	})
	compact := signIntent(t, h.intentKey, jose.ES256, "intent-key-1", intent)

	_, err := h.service.Share(context.Background(), "attacker", "space-a", compact, "request-1")
	assert.ErrorIs(t, err, ErrShareForbidden)
	_, err = h.service.Share(context.Background(), "user-a", "space-b", compact, "request-2")
	assert.ErrorIs(t, err, ErrShareForbidden)
	assert.Zero(t, h.adapter.calls)
	assert.Empty(t, h.transport.requests)
}

func TestShareService_ProviderRevalidationMustCoverExactTargets(t *testing.T) {
	intent := validIntent(time.Unix(1_800_000_000, 0).UTC())
	tests := []struct {
		name        string
		disclosures []TargetDisclosure
		adapterErr  error
	}{
		{"stale revision", nil, errors.New("stale revision")},
		{"missing target decision", []TargetDisclosure{disclosureFor(intent.Targets[0], true)}, nil},
		{"mutated target decision", []TargetDisclosure{
			disclosureFor(intent.Targets[0], true),
			disclosureFor(Target{Kind: TargetGroup, GroupNo: "other"}, true),
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newServiceHarness(t, intent, tt.disclosures)
			h.adapter.err = tt.adapterErr
			compact := signIntent(t, h.intentKey, jose.ES256, "intent-key-1", intent)
			_, err := h.service.Share(context.Background(), "user-a", "space-a", compact, "request-1")
			assert.ErrorIs(t, err, ErrProviderRevalidation)
			assert.Empty(t, h.transport.requests)
		})
	}
}

func TestShareService_AuthorizationAndRateLimitArePerTargetAndFailClosed(t *testing.T) {
	intent := validIntent(time.Unix(1_800_000_000, 0).UTC())
	intent.Targets = []Target{{Kind: TargetGroup, GroupNo: "group-a"}}
	disclosures := []TargetDisclosure{disclosureFor(intent.Targets[0], true)}

	t.Run("authorization denied", func(t *testing.T) {
		h := newServiceHarness(t, intent, disclosures)
		h.authorizer.err = ErrTargetDenied
		result, err := h.service.Share(context.Background(), "user-a", "space-a", signIntent(t, h.intentKey, jose.ES256, "intent-key-1", intent), "request-1")
		require.NoError(t, err)
		assert.Equal(t, ShareDenied, result.Results[0].Outcome)
		assert.Empty(t, h.transport.requests)
	})

	t.Run("limiter unavailable", func(t *testing.T) {
		h := newServiceHarness(t, intent, disclosures)
		h.limiter.decision = LimitDecision{RetryAfter: 10 * time.Second}
		h.limiter.err = errors.New("redis unavailable")
		result, err := h.service.Share(context.Background(), "user-a", "space-a", signIntent(t, h.intentKey, jose.ES256, "intent-key-1", intent), "request-1")
		require.NoError(t, err)
		assert.Equal(t, ShareRateLimited, result.Results[0].Outcome)
		assert.Equal(t, int64(10), result.Results[0].RetryAfterSeconds)
		assert.Empty(t, h.transport.requests)
	})
}

func TestShareService_FeatureDisabledFailsBeforeIntentProcessing(t *testing.T) {
	service, err := NewShareService(ShareServiceDependencies{FeatureEnabled: func() bool { return false }})
	require.NoError(t, err)
	_, err = service.Share(context.Background(), "user-a", "space-a", "not-a-token", "request-1")
	assert.ErrorIs(t, err, ErrShareDisabled)
}
