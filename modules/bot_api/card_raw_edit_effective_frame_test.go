package bot_api

// PR-C review P2-1 (yujiawei), raised as a gate on this PR specifically.
//
// The raw-edit guard that refuses to overwrite a Registry-authored card used to
// read `msgPayload` — the immutable original row. That was complete only by
// induction over the three `template_ref` authors in the tree: each writes the
// original payload, or is an edit path already gated on the effective frame
// carrying the marker. So effective-has-marker implied original-has-marker.
//
// That is a property of the current author set, not one anybody stated, and
// this PR is the one that adds send and edit paths. If any of them can author
// a marker into `content_edit` on a frame whose payload stays markerless, the
// induction stops holding and the guard goes silently inert: the raw write goes
// through WriteCAS, which does not run CatalogMarkersPreserved, so the erasure
// is not caught downstream either. Both identity guards in
// pkg/cardtmpl/updater.go begin `if markers.Has…`, so they simply stop firing
// for that message.
//
// This test constructs that state directly rather than waiting for a future
// path to create it: a card sent markerless, then a marker present only in
// message_extra.content_edit. With the guard reading the original payload the
// raw edit is accepted; reading the effective frame refuses it.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/testutil"
	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
	"github.com/stretchr/testify/assert"
)

func TestRawEditRefusesAFrameMarkedOnlyInItsEffectiveContent(t *testing.T) {
	skipWithoutIMBot(t)
	t.Setenv(cardmsg.EnvEnabled, "true")
	s, ctx := testutil.NewTestServer()
	defer func() { _ = testutil.CleanAllTables(ctx) }()

	const botID, botToken = "bot_raw_eff", "bf_raw_eff_token"
	_, err := ctx.DB().InsertBySql(
		"insert into robot(robot_id,bot_token,status) values(?,?,1)", botID, botToken).Exec()
	assert.NoError(t, err)
	for _, pair := range [][2]string{{botID, testutil.UID}, {testutil.UID, botID}} {
		_, ferr := ctx.DB().InsertBySql(
			"insert into friend(uid,to_uid,is_deleted) values(?,?,0)", pair[0], pair[1]).Exec()
		assert.NoError(t, ferr)
	}

	do := func(path string, body map[string]interface{}) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", path, bytes.NewReader([]byte(util.ToJson(body))))
		req.Header.Set("Authorization", "Bearer "+botToken)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	// A perfectly ordinary raw card: no markers anywhere.
	w := do("/v1/bot/sendMessage", map[string]interface{}{
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"payload":      imCardEnvelope("approve_btn", 1),
	})
	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var sendResp struct {
		MessageID int64 `json:"message_id"`
	}
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &sendResp))
	assert.NotZero(t, sendResp.MessageID)
	msgID := fmt.Sprintf("%d", sendResp.MessageID)

	// The real seq, polled the way the sibling IM edit test does. Passing a
	// guessed one makes the handler answer "Invalid request." before it ever
	// reaches the guard under test — and because ResponseErrorL pins every
	// refusal to 400, a bare non-200 assertion would have called that a pass.
	var msgSeq uint32
	for i := 0; i < 20; i++ {
		sr, serr := ctx.IMSearchMessages(&config.MsgSearchReq{
			ChannelID:   testutil.UID,
			ChannelType: common.ChannelTypePerson.Uint8(),
			MessageIds:  []int64{sendResp.MessageID},
			LoginUID:    botID,
		})
		if serr == nil && sr != nil && len(sr.Messages) > 0 && sr.Messages[0].MessageSeq > 0 {
			msgSeq = sr.Messages[0].MessageSeq
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	assert.NotZero(t, msgSeq, "message did not finish IM persistence")

	// The state the induction assumed unreachable: the effective frame carries
	// a marker the original payload does not. Written directly because no
	// in-tree path produces it *today* — which is the whole point. A future
	// send/edit path in this PR's gated body would produce it for real.
	marked := imCardEnvelope("approve_btn", 2)
	marked["template_ref"] = map[string]interface{}{
		"id": "ai.reasoning-process", "version": "0.4.0",
	}
	_, err = ctx.DB().InsertBySql(
		"insert into message_extra(message_id,message_seq,channel_id,channel_type,content_edit,`revoke`,is_deleted) "+
			"values(?,?,?,?,?,0,0)",
		msgID, msgSeq, testutil.UID, common.ChannelTypePerson.Uint8(), util.ToJson(marked)).Exec()
	assert.NoError(t, err)

	// A raw edit of that card must now be refused. Reading the original payload
	// answers "markerless, go ahead" and the marker is erased with no downstream
	// check to catch it.
	w = do("/v1/bot/message/edit", map[string]interface{}{
		"message_id":   msgID,
		"message_seq":  msgSeq,
		"channel_id":   testutil.UID,
		"channel_type": common.ChannelTypePerson.Uint8(),
		"content_edit": util.ToJson(imCardEnvelope("erase_btn", 3)),
	})
	// Assert the *specific* refusal. Every ResponseErrorL answer is 400, so a
	// bare status check cannot tell "the guard fired" from "the request was
	// malformed" — which is how the first draft of this test passed while the
	// guard was disabled.
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "card",
		"expected the card-invalid refusal from the marker guard, got: "+w.Body.String())

	// And the stored frame still carries it.
	var stored string
	assert.NoError(t, ctx.DB().Select("content_edit").From("message_extra").
		Where("message_id=?", msgID).LoadOne(&stored))
	assert.Contains(t, stored, "template_ref", "the marker was erased from storage")
}
