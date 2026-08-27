//go:build pilote2e

// Full-flow reproduction for the cross-Space DOCS ACCESS REQUEST (文档申请权限)
// story. Covers every hop that carries a Space decision, in both the
// cross-Space and the same-Space (control) shape.
//
// The flow has three notification hops, and octo-server owns the Space
// behaviour of all three:
//
//	hop 1  申请卡     docs → POST /v1/internal/notify  kind=access_requested → 审批人
//	hop 2  终态卡     审批人点击 → cardactiondispatch → DocsActionFinalizer
//	                  (改写审批人手里那张原卡；不给申请人发任何东西)
//	hop 3  结果卡     docs → POST /v1/internal/notify  kind=access_granted/denied → 申请人
//
// Hops 1 and 3 are mocked at the Docs backend's real integration seam: an HTTP
// POST to the REAL handler carrying the published cross-repo contract body
// (.octospec/tasks/card-message-internal-dispatch/docs-notify-contract.md,
// "一请求一收件人"). Hop 2 runs the REAL DocsActionFinalizer.
//
// Run:
//
//	go test -tags pilote2e ./pilote2e/ -run TestReproDocsApproval -v
package pilote2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/notify"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// docsApprovalWorld is the cast for the whole story.
//
//	spaceDoc  (B) — the document's Space (its creator's Space). creatorApprover
//	                and insider are members.
//	spaceReq  (A) — the outsiders' home Space. requester and outsideApprover are
//	                members, and NEITHER is a member of spaceDoc — that is what
//	                "跨空间" means here: they reach the document through a shared
//	                group or a per-document grant, not through space_member.
//
// The approver is NOT necessarily the creator: a document can carry several
// approvers, and one of them can be a cross-Space collaborator holding a
// per-document admin grant. outsideApprover is exactly that person, and the
// matrix below shows the delivery gap lands on whichever participant happens to
// be outside the document's Space — approver side and requester side alike.
type docsApprovalWorld struct {
	spaceDoc        string
	spaceReq        string
	creatorApprover string // approver who IS the creator, member of spaceDoc
	outsideApprover string // approver who is NOT the creator, member of spaceReq only
	requester       string
	insider         string // same-Space control requester: a member of spaceDoc
}

func newDocsApprovalWorld(t *testing.T, ctx *config.Context, tag string) docsApprovalWorld {
	t.Helper()
	stamp := time.Now().UnixNano()
	w := docsApprovalWorld{
		spaceDoc:        fmt.Sprintf("s_%s_doc_%d", tag, stamp),
		spaceReq:        fmt.Sprintf("s_%s_req_%d", tag, stamp),
		creatorApprover: fmt.Sprintf("u_%s_ca_%d", tag, stamp),
		outsideApprover: fmt.Sprintf("u_%s_oa_%d", tag, stamp),
		requester:       fmt.Sprintf("u_%s_rq_%d", tag, stamp),
		insider:         fmt.Sprintf("u_%s_in_%d", tag, stamp),
	}
	xsSeedSpace(t, ctx, w.spaceDoc, "2021-01-01 00:00:00")
	xsSeedSpace(t, ctx, w.spaceReq, "2020-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, w.spaceDoc, w.creatorApprover, "2021-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, w.spaceDoc, w.insider, "2021-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, w.spaceReq, w.requester, "2020-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, w.spaceReq, w.outsideApprover, "2020-01-01 00:00:00")
	return w
}

// TestReproDocsApproval_EveryHopSpaceMatrix walks both notify hops against both
// candidate Space values, plus the same-Space control. Read the table as: for
// each hop, which space_id the Docs backend could put on the request, and who
// actually receives anything.
//
// The verdict is produced by modules/notify.deliverDocsCardNotification →
// memberCache.verify (modules/notify/card.go:467), which keeps only targets
// holding an active space_member row in req.SpaceID. A filtered target is not
// delivered into the wrong Space — nothing is dispatched for it at all.
func TestReproDocsApproval_EveryHopSpaceMatrix(t *testing.T) {
	stamp := time.Now().UnixNano()
	docsToken := fmt.Sprintf("xsdocs-token-%d", stamp)

	// Deliberately WITHOUT OCTO_DOCS_APPROVAL_CARD_ENABLED / OCTO_CARD_MESSAGE_ENABLED.
	// memberCache.verify sits upstream of every rendering branch, so the Space
	// verdict under test is identical with the approval-card (ProfileV2) rollout
	// on or off — and leaving the gates off keeps the repro independent of the
	// card-template composition root.
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_DOCS_NOTIFY_TOKEN", docsToken)

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	w := newDocsApprovalWorld(t, ctx, "xsdocs")
	r := xsNotifyRouter(t, ctx)

	type hop struct {
		name      string
		kind      string
		target    string
		spaceID   string
		wantOK    bool
		narrative string
	}
	cases := []hop{
		// ---- hop 1: 申请卡 → 审批人 --------------------------------------
		{
			name: "hop1_申请卡_文档空间_审批人", kind: notify.DocsCardKindAccessRequested,
			target: w.creatorApprover, spaceID: w.spaceDoc, wantOK: true,
			narrative: "审批人是文档所在空间的成员 → 申请卡送达（这一跳是好的）",
		},
		{
			name: "hop1_申请卡_文档空间_跨空间审批人", kind: notify.DocsCardKindAccessRequested,
			target: w.outsideApprover, spaceID: w.spaceDoc, wantOK: false,
			narrative: "★ 审批人不一定是创建人：跨空间协作者被授予审批权时，docs 传的归属空间完全正确，" +
				"审批人却不是该空间的 space_member → 审批人收不到申请卡",
		},
		{
			name: "hop1_申请卡_审批人自己空间_跨空间审批人", kind: notify.DocsCardKindAccessRequested,
			target: w.outsideApprover, spaceID: w.spaceReq, wantOK: true,
			narrative: "同一个跨空间审批人，换成他自己的空间就送达 —— 又是路由问题，不是权限问题",
		},
		{
			name: "hop1_申请卡_申请人空间_创建人审批人", kind: notify.DocsCardKindAccessRequested,
			target: w.creatorApprover, spaceID: w.spaceReq, wantOK: false,
			narrative: "反过来：若 docs 用申请人所在空间发申请卡，创建人审批人同样被丢掉",
		},
		// ---- hop 3: 结果卡 → 申请人 ---------------------------------------
		{
			name: "hop3_结果卡_同意_文档空间_申请人", kind: notify.DocsCardKindAccessGranted,
			target: w.requester, spaceID: w.spaceDoc, wantOK: false,
			narrative: "★ 审批通过后：结果卡沿用文档空间，跨空间申请人被丢掉 → 申请人收不到审批结果",
		},
		{
			name: "hop3_结果卡_拒绝_文档空间_申请人", kind: notify.DocsCardKindAccessDenied,
			target: w.requester, spaceID: w.spaceDoc, wantOK: false,
			narrative: "拒绝结果同样丢失 —— 申请人永远停在“已提交”状态",
		},
		{
			name: "hop3_结果卡_同意_申请人空间_申请人", kind: notify.DocsCardKindAccessGranted,
			target: w.requester, spaceID: w.spaceReq, wantOK: true,
			narrative: "改用申请人自己的空间就能送达 —— 不缺权限，只缺“按收件人取空间”",
		},
		// ---- 同空间对照组 ------------------------------------------------
		{
			name: "control_结果卡_同意_文档空间_同空间申请人", kind: notify.DocsCardKindAccessGranted,
			target: w.insider, spaceID: w.spaceDoc, wantOK: true,
			narrative: "对照组：申请人本来就在文档空间内时一切正常 —— 故障只在跨空间出现",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The published contract is 一请求一收件人, so each hop is its own POST.
			delivered, filtered := xsPostNotify(t, r, docsToken, notify.NotifyReq{
				SpaceID: tc.spaceID,
				Service: "docs-service",
				Targets: []string{tc.target},
				DocsCard: &notify.DocsCardFields{
					DocID:              fmt.Sprintf("d_%s_%d", tc.name, stamp),
					RequestID:          fmt.Sprintf("req_%s_%d", tc.name, stamp),
					Kind:               tc.kind,
					Title:              "产品设计方案",
					ActorName:          "申请人",
					ActorUID:           w.requester,
					RequesterSpaceName: w.spaceReq,
					RequestedRole:      "reader",
					UpdatedAt:          "2026-08-27 10:00",
				},
			})
			t.Logf("%s\n  space_id=%s target=%s → delivered=%v filtered=%v",
				tc.narrative, tc.spaceID, tc.target, delivered, filtered)

			if tc.wantOK {
				assert.Equal(t, []string{tc.target}, delivered, "expected delivery: %s", tc.narrative)
				assert.Empty(t, filtered)
				return
			}
			assert.Empty(t, delivered, "expected ZERO delivery: %s", tc.narrative)
			assert.Equal(t, "not_space_member", filtered[tc.target],
				"the drop reason names the single-Space assumption: %s", tc.narrative)
		})
	}
}

// TestReproDocsApproval_NoSingleSpaceIDServesBothHops states the conclusion the
// matrix implies, as its own assertion: whichever Space the Docs backend picks
// and reuses for the whole request, one participant is unreachable.
//
// It is not a permission problem — each participant is reachable in their own
// Space (proved by the last two sends). It is that req.SpaceID is a single
// value applied to a set of recipients who do not share a Space.
func TestReproDocsApproval_NoSingleSpaceIDServesBothHops(t *testing.T) {
	stamp := time.Now().UnixNano()
	docsToken := fmt.Sprintf("xsboth-token-%d", stamp)

	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_DOCS_NOTIFY_TOKEN", docsToken)

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	w := newDocsApprovalWorld(t, ctx, "xsboth")
	r := xsNotifyRouter(t, ctx)

	send := func(spaceID, target, kind string) bool {
		delivered, _ := xsPostNotify(t, r, docsToken, notify.NotifyReq{
			SpaceID: spaceID, Service: "docs-service", Targets: []string{target},
			DocsCard: &notify.DocsCardFields{
				DocID: fmt.Sprintf("d_both_%s_%d", target, stamp),
				Kind:  kind, Title: "产品设计方案", ActorUID: w.requester,
			},
		})
		return len(delivered) == 1
	}

	for _, spaceID := range []string{w.spaceDoc, w.spaceReq} {
		creatorOK := send(spaceID, w.creatorApprover, notify.DocsCardKindAccessRequested)
		outsideOK := send(spaceID, w.outsideApprover, notify.DocsCardKindAccessRequested)
		requesterOK := send(spaceID, w.requester, notify.DocsCardKindAccessGranted)
		t.Logf("space_id=%s → 创建人审批人可达=%v 跨空间审批人可达=%v 申请人可达=%v",
			spaceID, creatorOK, outsideOK, requesterOK)
		assert.False(t, creatorOK && outsideOK && requesterOK,
			"DEFECT: no single space_id reaches every participant (tried %s)", spaceID)
		// Sharper still: the APPROVER SET alone already splits, before the
		// requester is even considered. One document, two approvers, no Space
		// that addresses both.
		assert.False(t, creatorOK && outsideOK,
			"DEFECT: one document's approver fan-out alone cannot be addressed by a single space_id (tried %s)", spaceID)
	}

	assert.True(t, send(w.spaceDoc, w.creatorApprover, notify.DocsCardKindAccessRequested),
		"the approver IS reachable — in the document's Space")
	assert.True(t, send(w.spaceReq, w.requester, notify.DocsCardKindAccessGranted),
		"the requester IS reachable — in their own Space; nothing here is a permission denial")
}

// TestReproDocsApproval_FinalizerNeverNotifiesTheApplicant pins hop 2's
// contribution: the Docs finalizer terminalises the approver's own card in
// place and sends the applicant nothing.
//
// modules/notify.DocsActionFinalizer.Finalize ends with:
//
//	// The original request card is the sole terminal surface. Do not send a
//	// second "access granted/denied" card to the requester.
//
// (contrast StandardActionFinalizer, which does send to result.RequesterUID —
// see docs/card-action-callback-consumer.md: "the Docs specialized finalizer …
// sends no second applicant terminal IM card".)
//
// So the applicant's outcome depends entirely on the Docs backend making hop 3
// — which the matrix above shows fails cross-Space. That is why the symptom is
// "审批人审批完了，申请人什么也没收到" rather than a visible error anywhere.
func TestReproDocsApproval_FinalizerNeverNotifiesTheApplicant(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	w := newDocsApprovalWorld(t, ctx, "xsfin")
	seedNotificationRobot(t, ctx)
	apiURL := ctx.GetConfig().WuKongIM.APIURL

	// Sentinel: prove the read path for the requester's notification-bot DM
	// channel works, so the emptiness assertion below means "nothing was sent"
	// rather than "we looked in the wrong place".
	sentinel := fmt.Sprintf("sentinel-before-approval-%d", time.Now().UnixNano())
	require.NoError(t, ctx.SendMessage(config.NewPersonalMsgSendReq(
		w.requester, notify.NotifyBotUIDValue,
		map[string]interface{}{"type": 1, "content": sentinel},
		w.spaceReq, config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}})))
	found := xsWaitForPayloads(t, apiURL, w.requester, notify.NotifyBotUIDValue, sentinel)
	require.NotNil(t, found[sentinel], "sentinel must be readable — the channel read path is live")

	mutator := &xsCapturingMutator{}
	finalizer, err := notify.NewDocsActionFinalizer(ctx, mutator)
	require.NoError(t, err)

	// The approver approves. event.SpaceID is the card's authoritative ORIGIN
	// Space, stamped by the action ingress from the pending card — i.e. exactly
	// the space_id the Docs backend used on hop 1.
	err = finalizer.Finalize(context.Background(), cardactiondispatch.Event{
		EventID:         991,
		SenderUID:       notify.NotifyBotUIDValue,
		Owner:           "docs",
		ActionType:      "access_request.decision",
		MessageID:       "10001",
		ChannelID:       w.creatorApprover,
		ChannelType:     common.ChannelTypePerson.Uint8(),
		SpaceID:         w.spaceDoc,
		ActionID:        "approve",
		OperatorUID:     w.creatorApprover,
		OperatorSpaceID: w.spaceDoc,
		Data:            map[string]interface{}{"doc_id": "d_xsfin_1", "doc_title": "产品设计方案"},
	}, cardactiondispatch.DecisionResult{
		Disposition:  cardactiondispatch.DispositionApplied,
		State:        cardactiondispatch.StateApproved,
		RequesterUID: w.requester, // required by the contract — and then ignored
	})
	require.NoError(t, err, "the approval itself succeeds")

	require.Len(t, mutator.requests, 1, "exactly one card is touched")
	assert.Equal(t, w.creatorApprover, mutator.requests[0].ChannelID,
		"and it is the APPROVER's own pending card, rewritten in place")

	after := channelMessageSync(t, apiURL, w.requester, notify.NotifyBotUIDValue)
	var contents []string
	for _, m := range after {
		if p := decodePayload(m); p != nil {
			if c, ok := p["content"].(string); ok {
				contents = append(contents, c)
			}
		}
	}
	t.Logf("申请人在通知助手会话里收到的全部内容: %v", contents)
	assert.Equal(t, []string{sentinel}, contents,
		"DEFECT (by design, and the reason the gap is invisible): approving sends the applicant "+
			"NOTHING — their outcome depends entirely on the Docs backend's hop-3 notify, "+
			"which the Space matrix shows is dropped cross-Space")
}

// TestReproDocsApproval_DispatchLayerRefusesTheApplicantToo shows the failure is
// not confined to the notify ingress: even a caller that skipped memberCache and
// dispatched straight through the producer is refused, because
// internal/carddispatch.DBAuthorizer.authorizeDM (authorizer_db.go:50) requires
// the RECIPIENT to hold an active space_member row in the target Space.
//
// That check precedes the SpacePolicy switch (authorizer_db.go:58 vs :83), so
// SpacePolicySystemNotification — the policy the notification bot's producer
// runs under — exempts the SENDER only. No policy currently lets a notification
// reach a non-member of the target Space.
func TestReproDocsApproval_DispatchLayerRefusesTheApplicantToo(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	w := newDocsApprovalWorld(t, ctx, "xsdisp")
	seedNotificationRobot(t, ctx)

	sender := xsInstallNotificationSender(t, ctx)
	document, err := cardtmpl.BuildApprovalResultCard(cardtmpl.ApprovalResultCard{
		Title: "审批请求", Status: "申请已允许", Variant: "approval.approved", Source: "审批",
	})
	require.NoError(t, err)

	_, err = sender.Send(context.Background(),
		carddispatch.Target{SpaceID: w.spaceDoc, ChannelID: w.creatorApprover, ChannelType: common.ChannelTypePerson.Uint8()},
		carddispatch.Card{Profile: cardmsg.ProfileV1, Document: document})
	require.NoError(t, err, "the approver is reachable in the document's Space — the stack is healthy")

	_, err = sender.Send(context.Background(),
		carddispatch.Target{SpaceID: w.spaceDoc, ChannelID: w.requester, ChannelType: common.ChannelTypePerson.Uint8()},
		carddispatch.Card{Profile: cardmsg.ProfileV1, Document: document})
	require.Error(t, err, "DEFECT: the applicant outcome card is refused in the document's Space")
	assert.Contains(t, err.Error(), "recipient is not an active member of target space",
		"the denial names the single-Space assumption; got: %v", err)

	_, err = sender.Send(context.Background(),
		carddispatch.Target{SpaceID: w.spaceReq, ChannelID: w.requester, ChannelType: common.ChannelTypePerson.Uint8()},
		carddispatch.Card{Profile: cardmsg.ProfileV1, Document: document})
	assert.NoError(t, err, "the same applicant is reachable in their OWN Space — only the target Space is wrong")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// xsCapturingMutator satisfies modules/notify's card mutator seam so the
// finalizer can run without a live card in WuKongIM. It records what the
// finalizer tried to rewrite.
type xsCapturingMutator struct {
	requests []carddispatch.CardMutationRequest
}

func (m *xsCapturingMutator) Mutate(
	_ context.Context, req carddispatch.CardMutationRequest,
) (carddispatch.CardMutationResult, error) {
	m.requests = append(m.requests, req)
	return carddispatch.CardMutationResult{}, nil
}
