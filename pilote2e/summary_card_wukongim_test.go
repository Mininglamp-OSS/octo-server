//go:build pilote2e

// Package pilote2e is a live end-to-end harness for the summary-notify card
// pilot. Unlike the mock-backed modules/notify tests, it drives the REAL
// internal/carddispatch producer (real botidentity resolution + real
// DBAuthorizer against MySQL + real WuKongIM transport) and then independently
// reads the message back out of WuKongIM to prove a type-17 card is actually
// persisted — i.e. "是否真的入库 WuKongIM".
//
// Requires the integration stack (MySQL 127.0.0.1:3306 root/demo/test,
// Redis 127.0.0.1:6379, WuKongIM 127.0.0.1:5001) and the build tag:
//
//	go test -tags pilote2e ./pilote2e/ -run TestSummaryCard -v
package pilote2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	liblog "github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	_ "github.com/Mininglamp-OSS/octo-server/internal" // blank-import every module so module.Setup migrates the full schema (space/space_member/robot/app_bot/group/thread ...)
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

const (
	e2eSpaceID     = "spc_pilote2e"
	e2eNotifyBotID = notify.NotifyBotUIDValue
	e2eProducer    = carddispatch.ProducerID("summary-notify")
)

// TestSummaryCard_DispatchesAndPersistsInWuKongIM simulates what octo-smart-summary
// will do once it switches to the card field: it builds the octo/v1 ResourceCard
// exactly as modules/notify.buildSummaryCard does, dispatches it through the
// producer-bound Sender, and then reads the recipient's DM channel back out of
// WuKongIM to confirm the type-17 card landed.
func TestSummaryCard_DispatchesAndPersistsInWuKongIM(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true") // deployment card gate ON (cardmsg.Enabled())
	// The full module set (blank-imported internal) wires the `common` module,
	// which refuses to boot without these 32-byte secrets. Mirror the CI env.
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")

	// WuKongIM persists its channel log in ~/.wukong ACROSS test runs (only MySQL
	// is truncated by CleanAllTables), so use a fresh recipient per run — the
	// notification↔recipient personal channel then holds exactly this run's card.
	recipient := fmt.Sprintf("uid_pilote2e_%d", time.Now().UnixNano())

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)

	seedSpace(t, ctx)
	seedSpaceMember(t, ctx, recipient)
	seedNotificationRobot(t, ctx)

	// Build the ONE process registry exactly like main.installCardDispatch, with
	// the summary-notify producer enabled (DM-only, octo/v1, system-notification).
	deps := carddispatch.Dependencies{
		IdentityResolver: botidentity.New(ctx),
		Authorizer:       carddispatch.NewDBAuthorizer(ctx.DB()),
		Transport:        ctx,
		Metrics:          carddispatch.NewMetrics(prometheus.NewRegistry()),
		Logger:           liblog.NewTLog("pilote2e"),
	}
	registry := carddispatch.NewRegistry(deps, []carddispatch.ProducerSpec{{
		ID:                  e2eProducer,
		Enabled:             true,
		SenderUID:           e2eNotifyBotID,
		AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
		AllowedProfiles:     []string{cardmsg.ProfileV1},
		SpacePolicy:         carddispatch.SpacePolicySystemNotification,
		GroupPolicy:         carddispatch.GroupPolicyMemberRequired,
		MaxInFlight:         20,
	}})
	sender, err := registry.Sender(e2eProducer)
	require.NoError(t, err, "summary-notify producer must be registered")

	// Build the card the same way notify.buildSummaryCard does.
	doc, err := cardtmpl.BuildSummaryResourceCard(
		context.Background(),
		"https://im.example.com/login", // External.WebLoginURL origin
		"TN_pilote2e_0001",
		e2eSpaceID,
		cardtmpl.ResourceCard{
			Title:       "产品周会纪要",
			Attribution: "总结已生成完成",
			Facts: []cardtmpl.Fact{
				{Title: "时间范围", Value: "2026-07-06 10:00 ~ 2026-07-13 10:00"},
				{Title: "参与成员", Value: "5 人"},
				{Title: "消息数量", Value: "128 条"},
			},
		},
	)
	require.NoError(t, err, "card build must succeed")

	// Dispatch through the real producer → real WuKongIM.
	result, err := sender.Send(context.Background(),
		carddispatch.Target{SpaceID: e2eSpaceID, ChannelID: recipient, ChannelType: common.ChannelTypePerson.Uint8()},
		carddispatch.Card{Profile: cardmsg.ProfileV1, Document: doc},
	)
	require.NoError(t, err, "dispatch must succeed")
	require.NotNil(t, result)
	t.Logf("dispatch result: message_id=%d message_seq=%d client_msg_no=%s", result.MessageID, result.MessageSeq, result.ClientMsgNo)
	// WuKongIM's /message/send returns a globally-unique message_id synchronously;
	// message_seq (the per-channel sequence) is assigned during persistence and is
	// 0 in this version's send response — the authoritative persistence proof is
	// the read-back below, which surfaces the assigned channel sequence.
	assert.NotZero(t, result.MessageID, "WuKongIM must assign a message_id")

	// Independent confirmation: read the recipient's DM channel back out of
	// WuKongIM and assert the persisted payload is the exact type-17 card we sent.
	msg := readBackType17(t, ctx.GetConfig().WuKongIM.APIURL, recipient, result.MessageID)
	require.NotNil(t, msg, "the dispatched type-17 card must be readable from WuKongIM channel log")

	assert.EqualValues(t, cardmsg.InteractiveCard.Int(), intOf(msg["type"]), "persisted payload must be type=17")
	assert.Equal(t, cardmsg.ProfileV1, msg["profile"], "profile must be octo/v1")
	assert.Equal(t, e2eSpaceID, msg["space_id"], "server-authored space_id must be on the wire")
	assert.NotZero(t, intOf(msg["__message_seq"]), "message must be persisted with a channel sequence")
	assert.EqualValues(t, result.MessageID, intOf(msg["__message_id"]), "read-back is the exact message we dispatched")
	assert.Equal(t, e2eNotifyBotID, msg["__from_uid"], "sender on the wire is the bound notification bot, not a caller-supplied uid")
	t.Logf("read-back persisted card: message_id=%d message_seq=%d from_uid=%v", intOf(msg["__message_id"]), intOf(msg["__message_seq"]), msg["__from_uid"])
	t.Logf("read-back persisted card payload: %s", compactJSON(msg))
}

// TestSummaryNotify_HTTPEndpointDeliversCardToWuKongIM is the most faithful
// simulation of the smart-summary call: it POSTs the cross-repo card contract
// body to the real POST /v1/internal/notify handler (structured Card field, no
// hand-built type-17 map), lets modules/notify build the card server-side and
// fan it out through the producer, and confirms the type-17 card persisted in
// WuKongIM. The notification bot is provisioned by notify.New itself (not seeded).
func TestSummaryNotify_HTTPEndpointDeliversCardToWuKongIM(t *testing.T) {
	t.Setenv(cardmsg.EnvEnabled, "true")
	t.Setenv("OCTO_MASTER_KEY", "0123456789abcdef0123456789abcdef")
	t.Setenv("OCTO_USER_API_KEY_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("NOTIFY_INTERNAL_TOKEN", "pilote2e-token")

	recipient := fmt.Sprintf("uid_pilote2e_http_%d", time.Now().UnixNano())

	_, ctx := testutil.NewTestServer()
	defer testutil.CleanAllTables(ctx)
	ctx.GetConfig().External.WebLoginURL = "https://im.example.com/login"

	seedSpace(t, ctx)
	seedSpaceMember(t, ctx, recipient)

	// Install the producer registry, then construct a notify instance that picks
	// up the bound card Sender (main.installCardDispatch happens before module
	// construction in production; here we replicate that ordering for one notify).
	deps := carddispatch.Dependencies{
		IdentityResolver: botidentity.New(ctx),
		Authorizer:       carddispatch.NewDBAuthorizer(ctx.DB()),
		Transport:        ctx,
		Metrics:          carddispatch.NewMetrics(prometheus.NewRegistry()),
		Logger:           liblog.NewTLog("pilote2e-http"),
	}
	registry := carddispatch.NewRegistry(deps, []carddispatch.ProducerSpec{{
		ID:                  e2eProducer,
		Enabled:             true,
		SenderUID:           e2eNotifyBotID,
		AllowedChannelTypes: []uint8{common.ChannelTypePerson.Uint8()},
		AllowedProfiles:     []string{cardmsg.ProfileV1},
		SpacePolicy:         carddispatch.SpacePolicySystemNotification,
		GroupPolicy:         carddispatch.GroupPolicyMemberRequired,
		MaxInFlight:         20,
	}})
	require.NoError(t, carddispatch.Install(ctx, registry))

	n := notify.New(ctx) // wires the card Sender + starts async notification-bot provisioning
	r := wkhttp.New()
	n.Route(r)
	r.SetErrorRenderer(octoi18n.NewErrorRenderer(octoi18n.NewLocalizer(octoi18n.DefaultLanguage)))

	// The exact cross-repo contract body smart-summary will POST.
	reqBody := notify.NotifyReq{
		SpaceID: e2eSpaceID,
		Service: "summary-service",
		Targets: []string{recipient},
		Card: &notify.SummaryCardFields{
			TaskNo:      "TN_pilote2e_http",
			Kind:        notify.SummaryCardKindCompleted,
			Title:       "产品周会纪要",
			TimeRange:   "2026-07-06 10:00 ~ 2026-07-13 10:00",
			Members:     5,
			MsgCount:    128,
			GeneratedAt: "2026-07-13 15:04",
		},
	}
	body, _ := json.Marshal(reqBody)

	// Poll until async notification-bot provisioning completes (the card path
	// returns "notification bot unavailable" → 500 until then).
	var delivered []string
	var lastCode int
	var lastBody string
	for attempt := 0; attempt < 40; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, "/v1/internal/notify", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(notify.InternalTokenHeader, "pilote2e-token")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		lastCode, lastBody = w.Code, w.Body.String()
		if w.Code == http.StatusOK {
			var resp struct {
				Delivered []string          `json:"delivered"`
				Filtered  map[string]string `json:"filtered"`
			}
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			delivered = resp.Delivered
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	require.Equal(t, http.StatusOK, lastCode, "notify card POST must succeed (last body: %s)", truncate(lastBody))
	require.Equal(t, []string{recipient}, delivered, "the recipient must receive the card")

	// Confirm the type-17 card persisted in the recipient's DM channel.
	msg := readBackType17(t, ctx.GetConfig().WuKongIM.APIURL, recipient, 0)
	require.NotNil(t, msg, "a type-17 card must be readable from WuKongIM channel log")
	assert.EqualValues(t, cardmsg.InteractiveCard.Int(), intOf(msg["type"]))
	assert.Equal(t, cardmsg.ProfileV1, msg["profile"])
	assert.Equal(t, e2eSpaceID, msg["space_id"], "server-authored space_id on the wire")
	assert.Equal(t, e2eNotifyBotID, msg["__from_uid"], "delivered by the bound notification bot")
	// The deep link the server built from External.WebLoginURL + task_no.
	assert.Contains(t, compactJSON(msg), "https://im.example.com/s/TN_pilote2e_http?sp="+e2eSpaceID)
	t.Logf("HTTP-path persisted card: message_id=%d message_seq=%d from_uid=%v",
		intOf(msg["__message_id"]), intOf(msg["__message_seq"]), msg["__from_uid"])
}

// The seed helpers insert the minimal authoritative rows the system-notification
// DM authorization reads. NOT NULL string columns are set explicitly so the
// inserts survive strict sql_mode.

func seedSpace(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertInto("space").
		Pair("space_id", e2eSpaceID).
		Pair("name", "pilote2e").
		Pair("description", "").
		Pair("logo", "").
		Pair("creator", "uid_pilote2e_creator").
		Pair("status", 1).
		Exec()
	require.NoError(t, err, "seed space")
}

func seedSpaceMember(t *testing.T, ctx *config.Context, uid string) {
	t.Helper()
	_, err := ctx.DB().InsertInto("space_member").
		Pair("space_id", e2eSpaceID).
		Pair("uid", uid).
		Pair("role", 0).
		Pair("status", 1).
		Exec()
	require.NoError(t, err, "seed space_member")
}

// seedNotificationRobot registers the notification bot as an active robot so botidentity
// resolves it as KindUserBot without going through notify's async provisioning.
func seedNotificationRobot(t *testing.T, ctx *config.Context) {
	t.Helper()
	_, err := ctx.DB().InsertInto("robot").
		Pair("robot_id", e2eNotifyBotID).
		Pair("token", "").
		Pair("status", 1).
		Pair("placeholder", "").
		Pair("username", e2eNotifyBotID).
		Pair("app_id", "").
		Pair("creator_uid", "").
		Pair("description", "").
		Pair("bot_token", "").
		Pair("im_token_cache", "").
		Pair("bot_commands", "").
		Exec()
	require.NoError(t, err, "seed robot (notification bot)")
}

// readBackType17 polls WuKongIM's /channel/messagesync for the summary↔recipient
// personal channel (from both login perspectives) and returns the persisted
// payload whose message_id equals wantID, decoded to a map with the WuKongIM
// envelope metadata (__message_id/__message_seq/__from_uid) attached. Returns
// nil if that message does not surface within the poll window.
func readBackType17(t *testing.T, apiURL, recipient string, wantID int64) map[string]interface{} {
	t.Helper()
	perspectives := []struct{ login, channel string }{
		{login: recipient, channel: e2eNotifyBotID},
		{login: e2eNotifyBotID, channel: recipient},
	}
	for attempt := 0; attempt < 15; attempt++ {
		for _, p := range perspectives {
			for _, m := range channelMessageSync(t, apiURL, p.login, p.channel) {
				if wantID > 0 && intOf(m["message_id"]) != wantID {
					continue
				}
				payload := decodePayload(m)
				if payload == nil || intOf(payload["type"]) != int64(cardmsg.InteractiveCard.Int()) {
					continue
				}
				payload["__message_id"] = intOf(m["message_id"])
				payload["__message_seq"] = intOf(m["message_seq"])
				payload["__from_uid"] = m["from_uid"]
				return payload
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

func channelMessageSync(t *testing.T, apiURL, loginUID, channelID string) []map[string]interface{} {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{
		"login_uid":         loginUID,
		"channel_id":        channelID,
		"channel_type":      common.ChannelTypePerson.Uint8(),
		"start_message_seq": 0,
		"end_message_seq":   0,
		"pull_mode":         1,
		"limit":             100,
	})
	resp, err := http.Post(apiURL+"/channel/messagesync", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Logf("messagesync post error (login=%s channel=%s): %v", loginUID, channelID, err)
		return nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Logf("messagesync status=%d (login=%s channel=%s) body=%s", resp.StatusCode, loginUID, channelID, truncate(string(raw)))
		return nil
	}
	// Response is either {"messages":[...]} or a bare array depending on version.
	// Decode with UseNumber: WuKongIM message_id is a ~2^61 snowflake that would
	// lose precision as a float64, breaking the exact message_id match.
	var obj struct {
		Messages []map[string]interface{} `json:"messages"`
	}
	if dec := newNumberDecoder(raw); dec.Decode(&obj) == nil && obj.Messages != nil {
		return obj.Messages
	}
	var arr []map[string]interface{}
	if newNumberDecoder(raw).Decode(&arr) == nil {
		return arr
	}
	t.Logf("messagesync unparsed body (login=%s channel=%s): %s", loginUID, channelID, truncate(string(raw)))
	return nil
}

// decodePayload extracts and JSON-decodes a WuKongIM message's payload, which is
// base64-encoded on the wire.
func decodePayload(m map[string]interface{}) map[string]interface{} {
	enc, ok := m["payload"].(string)
	if !ok || enc == "" {
		return nil
	}
	dec, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(dec, &payload); err != nil {
		return nil
	}
	return payload
}

func newNumberDecoder(raw []byte) *json.Decoder {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec
}

func intOf(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func compactJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func truncate(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
