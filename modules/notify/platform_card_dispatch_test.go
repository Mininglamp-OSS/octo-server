package notify

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	summarycompleted "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/summary_completed"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type platformDispatchIdentityResolver struct{}

func (platformDispatchIdentityResolver) Resolve(uid string) (*botidentity.Identity, error) {
	return &botidentity.Identity{UID: uid, Kind: botidentity.KindUserBot, CreatorUID: "owner-1"}, nil
}

type platformDispatchAuthorizer struct{}

func (platformDispatchAuthorizer) Authorize(
	context.Context, *botidentity.Identity, carddispatch.Target, carddispatch.AuthorizationPolicy,
) error {
	return nil
}

type platformDispatchTransport struct {
	calls int
	req   *config.MsgSendReq
}

func (t *platformDispatchTransport) SendMessageWithResult(req *config.MsgSendReq) (*config.MsgSendResp, error) {
	t.calls++
	copyReq := *req
	copyReq.Payload = append([]byte(nil), req.Payload...)
	t.req = &copyReq
	return &config.MsgSendResp{MessageID: 101, MessageSeq: 7, ClientMsgNo: "platform-card-101"}, nil
}

func newPlatformDispatchSender(
	t *testing.T, profile, actionOwner string,
) (carddispatch.Sender, *platformDispatchTransport) {
	t.Helper()
	transport := &platformDispatchTransport{}
	const producerID = carddispatch.ProducerID("platform-wire-test")
	registry := carddispatch.NewRegistry(carddispatch.Dependencies{
		IdentityResolver: platformDispatchIdentityResolver{},
		Authorizer:       platformDispatchAuthorizer{},
		Transport:        transport,
		FeatureEnabled:   func() bool { return true },
	}, []carddispatch.ProducerSpec{{
		ID:                  producerID,
		Enabled:             true,
		SenderUID:           NotifyBotUIDValue,
		AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
		AllowedProfiles:     []string{profile},
		SpacePolicy:         carddispatch.SpacePolicySystemNotification,
		GroupPolicy:         carddispatch.GroupPolicyMemberRequired,
		ActionEventOwner:    actionOwner,
		MaxInFlight:         1,
	}})
	sender, err := registry.Sender(producerID)
	require.NoError(t, err)
	return sender, transport
}

func requirePlatformTransportPayload(
	t *testing.T, transport *platformDispatchTransport, wantProfile string,
) map[string]interface{} {
	t.Helper()
	require.Equal(t, 1, transport.calls)
	require.NotNil(t, transport.req)
	assert.Equal(t, NotifyBotUIDValue, transport.req.FromUID)
	assert.Equal(t, "user-1", transport.req.ChannelID)
	assert.Equal(t, common.ChannelTypePerson.Uint8(), transport.req.ChannelType)

	var payload map[string]interface{}
	require.NoError(t, json.Unmarshal(transport.req.Payload, &payload))
	assert.Equal(t, float64(cardmsg.InteractiveCard.Int()), payload["type"])
	assert.Equal(t, wantProfile, payload["profile"])
	assert.Equal(t, cardmsg.RenderProfileOctoChatV1, payload["render_profile"])
	assert.NotContains(t, payload, "card_profile")
	require.NotNil(t, payload["card"])
	return payload
}

func platformDispatchTarget() carddispatch.Target {
	return carddispatch.Target{
		SpaceID: "space-1", ChannelID: "user-1", ChannelType: common.ChannelTypePerson.Uint8(),
	}
}

func TestSummaryCompletedRegistryCardReachesTransportWithRenderProfile(t *testing.T) {
	wk := newWuKongServer()
	defer wk.close()
	ctx := newTestContext(t, wk)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
	n := newTestNotify(ctx, nil, nil, nil, "token")

	document, renderProfile, err := n.buildSummaryCardViaRegistry(
		context.Background(), "space-1", validCard(), "zh-CN",
		summarycompleted.TemplateID, summarycompleted.StateShown,
	)
	require.NoError(t, err)
	require.Equal(t, cardmsg.RenderProfileOctoChatV1, renderProfile)

	sender, transport := newPlatformDispatchSender(t, cardmsg.ProfileV1, "")
	_, err = sender.Send(context.Background(), platformDispatchTarget(), carddispatch.Card{
		Profile: cardmsg.ProfileV1, RenderProfile: renderProfile, Document: document,
	})
	require.NoError(t, err)
	payload := requirePlatformTransportPayload(t, transport, cardmsg.ProfileV1)
	card := payload["card"].(map[string]interface{})
	metadata := card["metadata"].(map[string]interface{})
	octo := metadata["octo"].(map[string]interface{})
	assert.Equal(t, "summary.completed", octo["variant"])
}

func TestDocsApplicantOutcomesReachTransportWithRenderProfile(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       cardactiondispatch.State
		inputs      map[string]interface{}
		wantVariant string
	}{
		{name: "approved", state: cardactiondispatch.StateApproved, wantVariant: "docs.access_granted"},
		{
			name: "denied", state: cardactiondispatch.StateDenied,
			inputs:      map[string]interface{}{cardtmpl.DocsDenyReasonInputID: "scope mismatch"},
			wantVariant: "docs.access_denied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wk := newWuKongServer()
			defer wk.close()
			ctx := newTestContext(t, wk)
			ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"
			sender, transport := newPlatformDispatchSender(t, cardmsg.ProfileV1, "")
			finalizer, err := NewDocsActionFinalizer(ctx, &captureCardMutator{}, sender)
			require.NoError(t, err)

			err = finalizer.Finalize(context.Background(), cardactiondispatch.Event{
				EventID: 42, SenderUID: NotifyBotUIDValue, Owner: "docs",
				ActionType: "access_request.decision", MessageID: "message-1",
				ChannelID: "reviewer-1", ChannelType: common.ChannelTypePerson.Uint8(),
				SpaceID: "space-1", OperatorUID: "reviewer-1", Inputs: tc.inputs,
				Data: map[string]interface{}{
					"doc_id": "doc-1", "request_id": "request-1", "doc_title": "Roadmap",
				},
			}, cardactiondispatch.DecisionResult{
				Disposition: cardactiondispatch.DispositionApplied, State: tc.state,
				RequesterUID: "user-1", Display: map[string]string{"title": "Roadmap"},
			})
			require.NoError(t, err)

			payload := requirePlatformTransportPayload(t, transport, cardmsg.ProfileV1)
			card := payload["card"].(map[string]interface{})
			metadata := card["metadata"].(map[string]interface{})
			octo := metadata["octo"].(map[string]interface{})
			assert.Equal(t, tc.wantVariant, octo["variant"])
		})
	}
}

func TestGenericApprovalCardsReachTransportWithRenderProfile(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		capability := cardactiondispatch.NotifyCapability{SenderUID: NotifyBotUIDValue, Owner: "smart-summary"}
		document, err := (&Notify{}).buildApprovalRequestCard(&ApprovalCardFields{
			ActionType: "summary.publish.decision", Title: "Publish summary",
			Description: "Review it first", Data: map[string]string{"task_no": "task-1"},
		}, capability, "zh-CN")
		require.NoError(t, err)

		sender, transport := newPlatformDispatchSender(t, cardmsg.ProfileV2, capability.Owner)
		_, err = sender.Send(context.Background(), platformDispatchTarget(), carddispatch.Card{
			Profile: cardmsg.ProfileV2, Document: document,
		})
		require.NoError(t, err)
		requirePlatformTransportPayload(t, transport, cardmsg.ProfileV2)
	})

	t.Run("applicant outcome", func(t *testing.T) {
		sender, transport := newPlatformDispatchSender(t, cardmsg.ProfileV1, "")
		finalizer, err := NewStandardActionFinalizer(&captureCardMutator{}, sender)
		require.NoError(t, err)
		err = finalizer.Finalize(context.Background(), cardactiondispatch.Event{
			EventID: 84, SenderUID: NotifyBotUIDValue, Owner: "smart-summary",
			ActionType: "summary.publish.decision", MessageID: "message-2",
			ChannelID: "reviewer-1", ChannelType: common.ChannelTypePerson.Uint8(),
			SpaceID: "space-1", OperatorUID: "reviewer-1",
		}, cardactiondispatch.DecisionResult{
			Disposition: cardactiondispatch.DispositionApplied,
			State:       cardactiondispatch.StateApproved, RequesterUID: "user-1",
			Display: map[string]string{"title": "Publish summary"},
		})
		require.NoError(t, err)

		payload := requirePlatformTransportPayload(t, transport, cardmsg.ProfileV1)
		card := payload["card"].(map[string]interface{})
		metadata := card["metadata"].(map[string]interface{})
		octo := metadata["octo"].(map[string]interface{})
		assert.Equal(t, "approval.approved", octo["variant"])
	})
}
