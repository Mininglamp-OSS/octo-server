//go:build pilote2e

// Reproduction for symptom 2 of the cross-Space bot/notification report:
//
//	"文档申请卡片、跨空间成员申请卡片也没正确通过通知助手在对应的空间向我推送消息"
//
// The Docs backend lives in another repository, so it is mocked at its real
// integration seam: an HTTP POST to the REAL POST /v1/internal/notify handler
// carrying the published cross-repo contract body (docs/docs-notify-card.md).
// Everything downstream of that POST is production code running against the
// real stack (MySQL 3306 / Redis 6379 / WuKongIM 5001).
//
// Run:
//
//	go test -tags pilote2e ./pilote2e/ -run TestReproCrossSpace_Docs -v
//	go test -tags pilote2e ./pilote2e/ -run TestReproCrossSpace_ApprovalResultCard -v
//	go test -tags pilote2e ./pilote2e/ -run TestReproCrossSpace_NotificationBot -v
package pilote2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	liblog "github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/modules/notify"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReproCrossSpace_DocsAccessRequestCard_OneSideAlwaysLosesOut is the core
// reproduction. A document lives in Space B; the person asking for access lives
// in Space A. The Docs backend can put exactly ONE space_id on the notify
// request, and BOTH choices drop a required recipient:
//
//	space_id = B (the document's Space)  → approver gets the card,
//	                                       requester is filtered "not_space_member"
//	space_id = A (the requester's Space) → requester would be reachable,
//	                                       approver is filtered "not_space_member"
//
// The gate is modules/notify.deliverDocsCardNotification → memberCache.verify
// (modules/notify/card.go:467), which keeps only targets holding an active
// space_member row in req.SpaceID. Non-members are not merely mis-filed into
// the wrong Space — nothing is dispatched for them at all.
func TestReproCrossSpace_DocsAccessRequestCard_OneSideAlwaysLosesOut(t *testing.T) {
	stamp := time.Now().UnixNano()
	docsToken := fmt.Sprintf("xs-docs-token-%d", stamp)

	// Deliberately WITHOUT OCTO_DOCS_APPROVAL_CARD_ENABLED / OCTO_CARD_MESSAGE_ENABLED.
	// deliverDocsCardNotification applies the Space filter at card.go:467, which is
	// upstream of every rendering branch, so the cross-Space verdict under test is
	// identical with the approval-card (ProfileV2) rollout on or off; leaving the
	// gates off keeps the repro free of the card-template composition root.
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_DOCS_NOTIFY_TOKEN", docsToken)

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	var (
		spaceDoc       = fmt.Sprintf("spc_xsdoc_b_%d", stamp) // Space B — where the document lives
		spaceRequester = fmt.Sprintf("spc_xsdoc_a_%d", stamp) // Space A — where the requester lives
		approver       = fmt.Sprintf("uid_xs_approver_%d", stamp)
		requester      = fmt.Sprintf("uid_xs_requester_%d", stamp)
	)

	xsSeedSpace(t, ctx, spaceDoc, "2021-01-01 00:00:00")
	xsSeedSpace(t, ctx, spaceRequester, "2020-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceDoc, approver, "2021-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceRequester, requester, "2020-01-01 00:00:00")

	r := xsNotifyRouter(t, ctx)

	docsCard := func(docID string) *notify.DocsCardFields {
		return &notify.DocsCardFields{
			DocID:              docID,
			RequestID:          fmt.Sprintf("req_%s", docID),
			Kind:               notify.DocsCardKindAccessRequested,
			Title:              "产品设计方案",
			ActorName:          "申请人",
			ActorUID:           requester,
			RequesterSpaceName: spaceRequester,
			RequestedRole:      "reader",
			Excerpt:            "需要查看这份文档",
			UpdatedAt:          "2026-08-27 10:00",
		}
	}

	// ---- Choice 1: tag the notification with the DOCUMENT's Space ---------
	delivered, filtered := xsPostNotify(t, r, docsToken, notify.NotifyReq{
		SpaceID:  spaceDoc,
		Service:  "docs-service",
		Targets:  []string{approver, requester},
		DocsCard: docsCard(fmt.Sprintf("d_xs_docspace_%d", stamp)),
	})
	t.Logf("space_id=%s (document's Space) → delivered=%v filtered=%v", spaceDoc, delivered, filtered)

	assert.Contains(t, delivered, approver, "the approver is a member of the document's Space, so they are reachable")
	assert.NotContains(t, delivered, requester,
		"DEFECT: the requester receives NOTHING — the approval-request flow's other half is silently dropped")
	assert.Equal(t, "not_space_member", filtered[requester],
		"memberCache.verify drops the cross-Space requester before any card is built or dispatched")

	// ---- Choice 2: tag it with the REQUESTER's Space instead -------------
	delivered, filtered = xsPostNotify(t, r, docsToken, notify.NotifyReq{
		SpaceID:  spaceRequester,
		Service:  "docs-service",
		Targets:  []string{approver, requester},
		DocsCard: docsCard(fmt.Sprintf("d_xs_reqspace_%d", stamp)),
	})
	t.Logf("space_id=%s (requester's Space) → delivered=%v filtered=%v", spaceRequester, delivered, filtered)

	assert.Contains(t, delivered, requester, "flipping the tag makes the requester reachable…")
	assert.NotContains(t, delivered, approver,
		"DEFECT: …but now the APPROVER receives nothing, so the request can never be approved")
	assert.Equal(t, "not_space_member", filtered[approver],
		"the single-valued req.SpaceID cannot address recipients that live in different Spaces")
}

// TestReproCrossSpace_ApprovalResultCard_DeniedToCrossSpaceApplicant covers the
// second half of the same flow: the approver DOES receive and approve the card,
// and the applicant still never learns the outcome.
//
// modules/notify.StandardActionFinalizer.Finalize (standard_action_finalizer.go:91)
// sends the result card with Target{SpaceID: event.SpaceID, ChannelID:
// result.RequesterUID} — the CARD's Space, reused verbatim for a recipient who
// may live somewhere else. internal/carddispatch.DBAuthorizer.authorizeDM
// (authorizer_db.go:50) requires the RECIPIENT to hold an active space_member
// row in that Space and denies otherwise.
//
// The check sits BEFORE the SpacePolicy switch (authorizer_db.go:58 vs :83), so
// SpacePolicySystemNotification — which the notification bot's producer uses —
// exempts the SENDER only. There is no policy that lets a notification reach a
// non-member of the target Space.
func TestReproCrossSpace_ApprovalResultCard_DeniedToCrossSpaceApplicant(t *testing.T) {
	stamp := time.Now().UnixNano()

	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	var (
		spaceDoc       = fmt.Sprintf("spc_xsres_b_%d", stamp)
		spaceRequester = fmt.Sprintf("spc_xsres_a_%d", stamp)
		approver       = fmt.Sprintf("uid_xsres_approver_%d", stamp)
		requester      = fmt.Sprintf("uid_xsres_requester_%d", stamp)
	)
	xsSeedSpace(t, ctx, spaceDoc, "2021-01-01 00:00:00")
	xsSeedSpace(t, ctx, spaceRequester, "2020-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceDoc, approver, "2021-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceRequester, requester, "2020-01-01 00:00:00")
	seedNotificationRobot(t, ctx)

	sender := xsInstallNotificationSender(t, ctx)
	document, err := cardtmpl.BuildApprovalResultCard(cardtmpl.ApprovalResultCard{
		Title: "审批请求", Status: "申请已允许", Variant: "approval.approved", Source: "审批",
	})
	require.NoError(t, err)

	// The approver is in the card's Space — the finalizer's mutate side works.
	_, err = sender.Send(context.Background(),
		carddispatch.Target{SpaceID: spaceDoc, ChannelID: approver, ChannelType: common.ChannelTypePerson.Uint8()},
		carddispatch.Card{Profile: cardmsg.ProfileV1, Document: document})
	require.NoError(t, err, "an in-Space recipient is reachable, so the stack itself is healthy")

	// The applicant is not — this is the exact call StandardActionFinalizer makes.
	_, err = sender.Send(context.Background(),
		carddispatch.Target{SpaceID: spaceDoc, ChannelID: requester, ChannelType: common.ChannelTypePerson.Uint8()},
		carddispatch.Card{Profile: cardmsg.ProfileV1, Document: document})
	require.Error(t, err,
		"DEFECT: the approval RESULT card is refused for the cross-Space applicant "+
			"(finalizer surfaces this as applicant_notify_failed)")
	assert.Contains(t, err.Error(), "recipient is not an active member of target space",
		"the denial reason names the single-Space assumption; got: %v", err)

	// And it is genuinely the Space that is refused, not the applicant: the same
	// applicant is reachable in a Space they DO belong to.
	_, err = sender.Send(context.Background(),
		carddispatch.Target{SpaceID: spaceRequester, ChannelID: requester, ChannelType: common.ChannelTypePerson.Uint8()},
		carddispatch.Card{Profile: cardmsg.ProfileV1, Document: document})
	assert.NoError(t, err,
		"the applicant is perfectly reachable in their OWN Space — the finalizer just never targets it")
}

// TestReproCrossSpace_NotificationBotUntaggedDM_IsInvisibleEverywhere explains
// why symptom 2 presents as "nothing arrives" while symptom 1 presents as
// "arrives in the wrong Space".
//
// personSpaceAllows (modules/message/space_filter.go:582) gives an untagged DM
// a default-Space fallback (rule 2) — but ONLY for non-system bots. The
// notification assistant `notification` is in pkg/space.SystemBots, so an
// untagged DM from it hits rule 4 and is dropped in EVERY Space, the user's
// default one included. Any notify caller that cannot resolve a Space
// therefore produces a message no one can ever see.
func TestReproCrossSpace_NotificationBotUntaggedDM_IsInvisibleEverywhere(t *testing.T) {
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")

	s, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	cfg := ctx.GetConfig()
	require.NoError(t, ctx.Cache().Set(cfg.Cache.TokenCachePrefix+testutil.Token, testutil.UID+"@test"))

	stamp := time.Now().UnixNano()
	var (
		spaceA    = fmt.Sprintf("spc_xssys_a_%d", stamp) // user's default Space
		spaceB    = fmt.Sprintf("spc_xssys_b_%d", stamp)
		tagged    = fmt.Sprintf("tagged-notice-%d", stamp)
		untagged  = fmt.Sprintf("untagged-notice-%d", stamp)
		notifyBot = notify.NotifyBotUIDValue
	)
	xsSeedSpace(t, ctx, spaceA, "2020-01-01 00:00:00")
	xsSeedSpace(t, ctx, spaceB, "2021-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceA, testutil.UID, "2020-01-01 00:00:00")
	xsSeedSpaceMemberAt(t, ctx, spaceB, testutil.UID, "2021-01-01 00:00:00")
	seedNotificationRobot(t, ctx)
	xsResetLimits(t, ctx, testutil.UID, spaceA, spaceB)

	// Exactly what modules/notify does per recipient, with and without a
	// resolvable Space. NewPersonalMsgSendReq strips the tag on the empty case.
	require.NoError(t, ctx.SendMessage(config.NewPersonalMsgSendReq(
		testutil.UID, notifyBot,
		map[string]interface{}{"type": 1, "content": tagged},
		spaceB, config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}})))
	require.NoError(t, ctx.SendMessage(config.NewPersonalMsgSendReq(
		testutil.UID, notifyBot,
		map[string]interface{}{"type": 1, "content": untagged},
		"", config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}})))

	onWire := xsWaitForPayloads(t, cfg.WuKongIM.APIURL, testutil.UID, notifyBot, tagged, untagged)
	require.NotNil(t, onWire[tagged], "the tagged notice must be persisted")
	require.NotNil(t, onWire[untagged], "the untagged notice must be persisted")
	_, hasSpace := onWire[untagged]["space_id"]
	require.False(t, hasSpace, "the untagged notice really is on the wire without a space_id")

	inA := xsChannelSyncContents(t, s, notifyBot, spaceA)
	inB := xsChannelSyncContents(t, s, notifyBot, spaceB)
	t.Logf("Space A (default) sees: %v", inA)
	t.Logf("Space B           sees: %v", inB)

	assert.Contains(t, inB, tagged, "a correctly tagged notice reaches its Space")
	assert.NotContains(t, inA, untagged,
		"DEFECT: an untagged notification-assistant DM is invisible even in the user's DEFAULT Space — "+
			"SystemBots get no rule-2 fallback (personSpaceAllows rule 4)")
	assert.NotContains(t, inB, untagged, "…and invisible in every other Space too")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// xsInstallNotificationSender installs the notification bot's producer alone and
// hands back its bound Sender — the same object StandardActionFinalizer holds.
func xsInstallNotificationSender(t *testing.T, ctx *config.Context) carddispatch.Sender {
	t.Helper()
	deps := carddispatch.Dependencies{
		IdentityResolver: botidentity.New(ctx),
		Authorizer:       carddispatch.NewDBAuthorizer(ctx.DB()),
		Transport:        ctx,
		Metrics:          carddispatch.NewMetrics(prometheus.NewRegistry()),
		Logger:           liblog.NewTLog("pilote2e-xs-result"),
	}
	registry := carddispatch.NewRegistry(deps, []carddispatch.ProducerSpec{{
		ID:                  e2eDocsProducer,
		Enabled:             true,
		SenderUID:           notify.NotifyBotUIDValue,
		AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
		AllowedProfiles:     []string{cardmsg.ProfileV1, cardmsg.ProfileV2},
		ActionEventOwner:    "docs",
		SpacePolicy:         carddispatch.SpacePolicySystemNotification,
		GroupPolicy:         carddispatch.GroupPolicyMemberRequired,
		MaxInFlight:         20,
	}})
	sender, err := registry.Sender(e2eDocsProducer)
	require.NoError(t, err)
	return sender
}

// xsNotifyRouter builds the real /v1/internal/notify surface on its own router,
// the way the existing pilote2e tests do (the module set is memoized process-
// wide by register.GetModules, so a per-test notify instance is what picks up
// this test's env).
func xsNotifyRouter(t *testing.T, ctx *config.Context) *wkhttp.WKHttp {
	t.Helper()
	n := notify.New(ctx)
	r := wkhttp.New()
	n.Route(r)
	r.SetErrorRenderer(octoi18n.NewErrorRenderer(octoi18n.NewLocalizer(octoi18n.DefaultLanguage)))
	return r
}

// xsPostNotify performs the mocked Docs-backend call and returns the handler's
// delivered / filtered verdict. Retries while the notification bot is still
// being provisioned asynchronously by notify.New.
func xsPostNotify(t *testing.T, r *wkhttp.WKHttp, token string, body notify.NotifyReq) ([]string, map[string]string) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)

	var lastCode int
	var lastBody string
	for attempt := 0; attempt < 40; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/internal/notify", bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(notify.InternalTokenHeader, token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		lastCode, lastBody = w.Code, w.Body.String()
		if w.Code == http.StatusOK {
			var resp struct {
				Delivered []string          `json:"delivered"`
				Filtered  map[string]string `json:"filtered"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp), "notify response must decode: %s", truncate(lastBody))
			return resp.Delivered, resp.Filtered
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("notify POST never succeeded: status=%d body=%s", lastCode, truncate(lastBody))
	return nil, nil
}
