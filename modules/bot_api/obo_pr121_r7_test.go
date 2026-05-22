// Package bot_api · PR#121 R7 (YUJ-1671) — extend the OBO send bypass
// from ChannelTypeGroup to ChannelTypeCommunityTopic.
//
// Jerry-Xin R7 finding: CommunityTopic fan-out (`obo_fanout.go`,
// mention-gated implicit-scope) successfully routes a topic message
// to a clone bot that is NOT a parent-group member, but the bot's OBO
// reply was rejected by `checkSendPermission` BEFORE `checkOBO` had a
// chance to authorize the grantor — the topic branch only verified
// "bot is a parent-group member", with no OBO bypass. The Group branch
// already had the right shape (`if !hasOBOContext` skip).
//
// This test pins that the topic branch now mirrors Group: when
// `on_behalf_of` is set on a CommunityTopic send, the bot's
// parent-group membership check is skipped, and the request reaches
// dispatch with FromUID = grantor.
//
// We do NOT need a real DB here: when `hasOBOContext=true` the
// membership SELECT is bypassed entirely. checkOBO is satisfied via
// the in-memory fakeOBOStore and oboChannelAccessOverride; dispatch
// is captured via dispatchOverride. If the bypass regressed, the
// nil `ba.db` would be dereferenced inside the SELECT and the test
// would panic — that panic IS the regression signal.
package bot_api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

// TestSendMessage_OBO_PR121R7_CommunityTopic_BypassesParentGroupMembership
// — YUJ-1671 headline regression. Topic send with `on_behalf_of` from
// a clone bot that is NOT a parent-group member, grantor IS a member;
// must succeed (200, FromUID swapped to grantor), not 403.
func TestSendMessage_OBO_PR121R7_CommunityTopic_BypassesParentGroupMembership(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const (
		botID    = "bot_clone_james"
		grantor  = "user_admin"
		topicCID = "group_42____topic_a1"
		creator  = "user_creator" // != grantor → no creator-bypass shortcut
	)

	// In-memory grant + scope so checkOBO authorizes the topic send.
	// We seed an explicit scope row covering this topic; that hits the
	// strict-scope branch and exercises the production checkOBO path
	// without depending on the implicit-scope SELECT.
	s := newFakeOBOStore()
	gid, _ := s.insertGrant(grantor, botID, "auto", "")
	enable := 1
	_ = s.updateGrant(gid, "", &enable, nil)
	_, _ = s.insertScope(gid, topicCID, common.ChannelTypeCommunityTopic.Uint8(), 1)

	dc := &dispatchCapture{}
	ba := &BotAPI{
		Log:              log.NewTLog("BotAPI-pr121-r7-topic"),
		spaceQuerier:     &fakeSpaceQuerier{defaultSpace: "space_A"},
		dispatchOverride: dc.hook,
		oboStoreOverride: s,
		// Live grantor channel-access re-check — admin IS in group_42.
		// Bot membership is intentionally NOT modeled: the bypass means
		// we never ask. If the bypass regressed, the production code
		// path would hit `ba.db.session` (nil here) and panic — which
		// itself is a hard regression signal.
		oboChannelAccessOverride: func(uid, channelID string, channelType uint8) (bool, error) {
			return uid == grantor && channelID == topicCID, nil
		},
	}

	body, _ := json.Marshal(BotSendMessageReq{
		ChannelID:   topicCID,
		ChannelType: common.ChannelTypeCommunityTopic.Uint8(),
		OnBehalfOf:  grantor,
		Payload:     map[string]interface{}{"content": "topic reply as admin", "type": 1},
	})

	httpReq := httptest.NewRequest(http.MethodPost, "/v1/bot/sendMessage", bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	gc, _ := gin.CreateTestContext(rec)
	gc.Request = httpReq
	c := &wkhttp.Context{Context: gc}
	c.Set(CtxKeyRobotID, botID)
	c.Set(CtxKeyBotKind, BotKindUser)
	// Creator != grantor and != topic so no creator-bypass shortcut
	// fires; the topic branch of checkSendPermission is forced to run.
	c.Set(CtxKeyRobot, &robotModel{RobotID: botID, CreatorUID: creator})

	ba.sendMessage(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("PR#121 R7 (YUJ-1671): topic OBO send must succeed when bot is NOT a parent-group member; got status=%d body=%s", rec.Code, rec.Body.String())
	}
	if dc.captured == nil {
		t.Fatal("PR#121 R7 (YUJ-1671): dispatch must fire after the bypass; got nil")
	}
	if dc.captured.FromUID != grantor {
		t.Errorf("PR#121 R7 (YUJ-1671): FromUID should be on_behalf_of grantor (%q), got %q", grantor, dc.captured.FromUID)
	}
	if dc.captured.ChannelType != common.ChannelTypeCommunityTopic.Uint8() {
		t.Errorf("PR#121 R7 (YUJ-1671): ChannelType should be CommunityTopic (%d), got %d", common.ChannelTypeCommunityTopic.Uint8(), dc.captured.ChannelType)
	}
	if dc.captured.ChannelID != topicCID {
		t.Errorf("PR#121 R7 (YUJ-1671): ChannelID should be %q, got %q", topicCID, dc.captured.ChannelID)
	}
	// Server-only OBO markers MUST be set on the dispatched payload —
	// the bypass must NOT skip the fan-out gate-3 marker, otherwise
	// the obo_fanout listener could re-process this message.
	var got map[string]interface{}
	if err := json.Unmarshal(dc.captured.Payload, &got); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if v, ok := got[oboProcessedMarkerKey].(bool); !ok || !v {
		t.Errorf("PR#121 R7 (YUJ-1671): payload must carry %s=true; got %v", oboProcessedMarkerKey, got[oboProcessedMarkerKey])
	}
	if v, _ := got["actual_sender_uid"].(string); v != botID {
		t.Errorf("PR#121 R7 (YUJ-1671): payload.actual_sender_uid should be bot %q, got %q", botID, v)
	}
}
