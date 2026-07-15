//go:build pilote2e

// Docs-notify E2E: end-to-end proof that a POST to the real
// /v1/internal/notify handler with a structured DocsCard field results in a
// type-17 card persisted in WuKongIM under the /d/{doc_id}?sp={space_id}
// deep link, delivered by the shared notification bot. Mirrors
// summary_card_wukongim_test.go — same integration stack requirements
// (MySQL 127.0.0.1:3306 root/demo/test, Redis 127.0.0.1:6379,
// WuKongIM 127.0.0.1:5001) and the same build tag:
//
//	go test -tags pilote2e ./pilote2e/ -run TestDocsNotify -v
package pilote2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	liblog "github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
	"github.com/Mininglamp-OSS/octo-server/modules/botidentity"
	"github.com/Mininglamp-OSS/octo-server/modules/notify"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const e2eDocsProducer = carddispatch.ProducerID("docs-notify")

// postUntilDelivered posts JSON to the notify endpoint until BOTH conditions
// hold: (1) HTTP 200 (past the async notification-bot readiness window), and
// (2) `expectedUID` is in the `delivered` list. This tolerates a transient
// window where the second `notify` instance's memberCache hasn't yet caught up
// with a freshly-seeded space_member row — a real class of test setup race
// the smart-summary / docs-backend callers already handle by retrying filtered
// recipients, so the E2E test mirrors that behaviour.
func postUntilDelivered(
	t *testing.T,
	r wkhttpRouter,
	path, token string,
	body []byte,
	expectedUID string,
) (delivered []string, lastCode int, lastBody string) {
	t.Helper()
	for attempt := 0; attempt < 40; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, path, bytes.NewReader(body))
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
			if json.Unmarshal(w.Body.Bytes(), &resp) == nil {
				delivered = resp.Delivered
				for _, uid := range delivered {
					if uid == expectedUID {
						return
					}
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return
}

// wkhttpRouter is the minimal ServeHTTP surface both wkhttp.WKHttp and
// http.Handler satisfy; keeping it here so postUntilDelivered doesn't leak
// wkhttp into every test's import.
type wkhttpRouter interface {
	ServeHTTP(http.ResponseWriter, *http.Request)
}

// TestDocsNotify_HTTPEndpointDeliversCardToWuKongIM POSTs the cross-repo docs
// contract body to the real POST /v1/internal/notify handler, has
// modules/notify build the docs card server-side, and confirms the type-17
// card persisted in WuKongIM carries the docs-flavoured deep link
// (/d/{doc_id}?sp={space}) and the reserved metadata.octo.variant identifier.
func TestDocsNotify_HTTPEndpointDeliversCardToWuKongIM(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("NOTIFY_INTERNAL_TOKEN", "pilote2e-default-token")
	t.Setenv("OCTO_DOCS_NOTIFY_TOKEN", "pilote2e-docs-token")

	recipient := fmt.Sprintf("uid_pilote2e_docs_%d", time.Now().UnixNano())

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	seedSpace(t, ctx)
	seedSpaceMember(t, ctx, recipient)

	// Install BOTH production producers (mirrors main.cardDispatchProducerSpecs)
	// so notify.New picks up the docs-notify Sender via SenderFromContext.
	deps := carddispatch.Dependencies{
		IdentityResolver: botidentity.New(ctx),
		Authorizer:       carddispatch.NewDBAuthorizer(ctx.DB()),
		Transport:        ctx,
		Metrics:          carddispatch.NewMetrics(prometheus.NewRegistry()),
		Logger:           liblog.NewTLog("pilote2e-docs"),
	}
	baseSpec := carddispatch.ProducerSpec{
		Enabled:             true,
		SenderUID:           e2eNotifyBotID,
		AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
		AllowedProfiles:     []string{cardmsg.ProfileV1},
		SpacePolicy:         carddispatch.SpacePolicySystemNotification,
		GroupPolicy:         carddispatch.GroupPolicyMemberRequired,
		MaxInFlight:         20,
	}
	summarySpec := baseSpec
	summarySpec.ID = e2eProducer
	docsSpec := baseSpec
	docsSpec.ID = e2eDocsProducer
	registry := carddispatch.NewRegistry(deps, []carddispatch.ProducerSpec{summarySpec, docsSpec})
	require.NoError(t, carddispatch.Install(ctx, registry))

	n := notify.New(ctx)
	r := wkhttp.New()
	n.Route(r)
	r.SetErrorRenderer(octoi18n.NewErrorRenderer(octoi18n.NewLocalizer(octoi18n.DefaultLanguage)))

	reqBody := notify.NotifyReq{
		SpaceID: e2eSpaceID,
		Service: "docs-service",
		Targets: []string{recipient},
		DocsCard: &notify.DocsCardFields{
			DocID:     "d_pilote2e_http",
			Kind:      notify.DocsCardKindShared,
			Title:     "产品设计方案",
			ActorName: "Alice",
			Excerpt:   "Q3 上线计划已确认",
			UpdatedAt: "2026-07-13 15:04",
		},
	}
	body, _ := json.Marshal(reqBody)

	delivered, lastCode, lastBody := postUntilDelivered(t, r, "/v1/internal/notify", "pilote2e-docs-token", body, recipient)
	require.Equal(t, http.StatusOK, lastCode, "docs card POST must succeed (last body: %s)", truncate(lastBody))
	require.Equal(t, []string{recipient}, delivered, "the recipient must receive the docs card")

	msg := readBackType17(t, ctx.GetConfig().WuKongIM.APIURL, recipient, 0)
	require.NotNil(t, msg, "a type-17 docs card must be readable from WuKongIM channel log")
	assert.EqualValues(t, cardmsg.InteractiveCard.Int(), intOf(msg["type"]))
	assert.Equal(t, cardmsg.ProfileV1, msg["profile"])
	assert.Equal(t, e2eSpaceID, msg["space_id"], "server-authored space_id on the wire")
	assert.Equal(t, e2eNotifyBotID, msg["__from_uid"], "delivered by the bound notification bot (shared identity, capability isolated by producer)")

	payloadJSON := compactJSON(msg)
	// Deep-link points at octo-web /d/:docId (already live) — not /s/:taskId.
	assert.Contains(t, payloadJSON, "https://im.example.com/d/d_pilote2e_http?sp="+e2eSpaceID,
		"docs deep-link must be /d/{doc_id}?sp={space} — that's the whole reason for a separate producer")
	// Reserved namespace variant identifier so renderers can style docs cards
	// distinctly from summary cards later without re-parsing content.
	assert.Contains(t, payloadJSON, `"variant":"docs.shared"`,
		"metadata.octo.variant must carry the reserved docs.shared identifier")
	t.Logf("docs-notify HTTP-path persisted card: message_id=%d message_seq=%d from_uid=%v",
		intOf(msg["__message_id"]), intOf(msg["__message_seq"]), msg["__from_uid"])
}

// TestSummaryAndDocsNotify_NoRegression re-runs both producers back-to-back in
// one server lifecycle, proving the two shared-notification-bot paths don't
// interfere and that adding docs-notify hasn't changed summary behaviour.
func TestSummaryAndDocsNotify_NoRegression(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("NOTIFY_INTERNAL_TOKEN", "pilote2e-both-token")
	t.Setenv("OCTO_DOCS_NOTIFY_TOKEN", "pilote2e-both-docs-token")

	recipient := fmt.Sprintf("uid_pilote2e_both_%d", time.Now().UnixNano())

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	seedSpace(t, ctx)
	seedSpaceMember(t, ctx, recipient)

	deps := carddispatch.Dependencies{
		IdentityResolver: botidentity.New(ctx),
		Authorizer:       carddispatch.NewDBAuthorizer(ctx.DB()),
		Transport:        ctx,
		Metrics:          carddispatch.NewMetrics(prometheus.NewRegistry()),
		Logger:           liblog.NewTLog("pilote2e-both"),
	}
	baseSpec := carddispatch.ProducerSpec{
		Enabled:             true,
		SenderUID:           e2eNotifyBotID,
		AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
		AllowedProfiles:     []string{cardmsg.ProfileV1},
		SpacePolicy:         carddispatch.SpacePolicySystemNotification,
		GroupPolicy:         carddispatch.GroupPolicyMemberRequired,
		MaxInFlight:         20,
	}
	summarySpec := baseSpec
	summarySpec.ID = e2eProducer
	docsSpec := baseSpec
	docsSpec.ID = e2eDocsProducer
	require.NoError(t, carddispatch.Install(ctx, carddispatch.NewRegistry(deps, []carddispatch.ProducerSpec{summarySpec, docsSpec})))

	n := notify.New(ctx)
	r := wkhttp.New()
	n.Route(r)
	r.SetErrorRenderer(octoi18n.NewErrorRenderer(octoi18n.NewLocalizer(octoi18n.DefaultLanguage)))

	postJSON := func(t *testing.T, token string, body []byte, expected string) {
		t.Helper()
		delivered, lastCode, lastBody := postUntilDelivered(t, r, "/v1/internal/notify", token, body, expected)
		require.Equal(t, http.StatusOK, lastCode, "notify POST must succeed (last body: %s)", truncate(lastBody))
		require.Contains(t, delivered, expected, "recipient must be delivered")
	}

	summaryBody, _ := json.Marshal(notify.NotifyReq{
		SpaceID: e2eSpaceID, Service: "summary-service", Targets: []string{recipient},
		Card: &notify.SummaryCardFields{
			TaskNo: "TN_regression_1", Kind: notify.SummaryCardKindCompleted, Title: "周会纪要",
			TimeRange: "2026-07-06 10:00 ~ 2026-07-13 10:00", Members: 3, MsgCount: 42,
		},
	})
	docsBody, _ := json.Marshal(notify.NotifyReq{
		SpaceID: e2eSpaceID, Service: "docs-service", Targets: []string{recipient},
		DocsCard: &notify.DocsCardFields{
			DocID: "d_regression_1", Kind: notify.DocsCardKindShared, Title: "设计稿",
			ActorName: "Bob", Excerpt: "review please",
		},
	})
	postJSON(t, "pilote2e-both-token", summaryBody, recipient)
	postJSON(t, "pilote2e-both-docs-token", docsBody, recipient)

	// Both cards must be readable back from WuKongIM under the recipient's
	// notification-bot DM channel. Read all persisted type-17s and assert both
	// deep-link shapes appear.
	var sawSummary, sawDocs bool
	for attempt := 0; attempt < 20 && !(sawSummary && sawDocs); attempt++ {
		msgs := channelMessageSync(t, ctx.GetConfig().WuKongIM.APIURL, recipient, e2eNotifyBotID)
		for _, m := range msgs {
			payload := decodePayload(m)
			if payload == nil || intOf(payload["type"]) != int64(cardmsg.InteractiveCard.Int()) {
				continue
			}
			pj := compactJSON(payload)
			if bytes.Contains([]byte(pj), []byte("/s/TN_regression_1?sp="+e2eSpaceID)) {
				sawSummary = true
			}
			if bytes.Contains([]byte(pj), []byte("/d/d_regression_1?sp="+e2eSpaceID)) {
				sawDocs = true
			}
		}
		if sawSummary && sawDocs {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	assert.True(t, sawSummary, "summary card must persist under /s/ deep link (no regression)")
	assert.True(t, sawDocs, "docs card must persist under /d/ deep link")
}
