package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSenderTestContext(apiURL string) *config.Context {
	cfg := config.New()
	cfg.Test = true
	cfg.WuKongIM.APIURL = apiURL
	cfg.WuKongIM.ManagerToken = "mgr-token-xyz"
	cfg.EventPoolSize = 1
	cfg.Push.PushPoolSize = 1
	cfg.Robot.EventPoolSize = 1
	return config.NewContext(cfg)
}

func TestSpaceWelcomeSender_SetsHeadersAndParsesResult(t *testing.T) {
	var gotContentType, gotToken, gotPath string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotToken = r.Header.Get("token")
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"data":{"message_id":123456,"client_msg_no":"cmn-9","message_seq":7}}`))
	}))
	defer srv.Close()

	sender := newSpaceWelcomeSender(newSenderTestContext(srv.URL))
	payload := map[string]interface{}{"type": 1, "content": "hi"}
	req := config.NewPersonalMsgSendReq("u_1", "notification", payload, "spc_1",
		config.PersonalMsgOptions{Header: config.MsgHeader{RedDot: 1}})

	res, err := sender.send(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, int64(123456), res.messageID)
	assert.Equal(t, "cmn-9", res.clientMsgNo)
	assert.Equal(t, uint32(7), res.messageSeq)

	// Content-Type must be set explicitly in addition to the token header.
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "mgr-token-xyz", gotToken)
	assert.Equal(t, "/message/send", gotPath)
	// Authoritative wire protocol on the outbound request.
	assert.Equal(t, "notification", gotBody["from_uid"])
	assert.EqualValues(t, 1, gotBody["channel_type"]) // ChannelTypePerson
}

func TestSpaceWelcomeSender_Non200IsBadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	sender := newSpaceWelcomeSender(newSenderTestContext(srv.URL))
	req := config.NewPersonalMsgSendReq("u_1", "notification", map[string]interface{}{"type": 1, "content": "x"}, "spc_1", config.PersonalMsgOptions{})
	_, err := sender.send(context.Background(), req)
	require.Error(t, err)
	var se *swSendError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, swErrIMBadResponse, se.class)
}

func TestSpaceWelcomeSender_Empty200IsBadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	sender := newSpaceWelcomeSender(newSenderTestContext(srv.URL))
	req := config.NewPersonalMsgSendReq("u_1", "notification", map[string]interface{}{"type": 1, "content": "x"}, "spc_1", config.PersonalMsgOptions{})
	_, err := sender.send(context.Background(), req)
	require.Error(t, err)
	var se *swSendError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, swErrIMBadResponse, se.class)
}

func TestSpaceWelcomeSender_ContextTimeoutIsTransportAmbiguous(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // hang until the test releases
	}))
	defer srv.Close()
	defer close(block)

	sender := newSpaceWelcomeSender(newSenderTestContext(srv.URL))
	req := config.NewPersonalMsgSendReq("u_1", "notification", map[string]interface{}{"type": 1, "content": "x"}, "spc_1", config.PersonalMsgOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := sender.send(ctx, req)
	require.Error(t, err)
	var se *swSendError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, swErrIMTimeout, se.class, "a timeout must classify as transport-ambiguous")
}

func TestMessageForLang(t *testing.T) {
	svc := &spaceWelcomeService{}
	cfg := common.SpaceWelcomeConfig{MessageZhCN: "中文", MessageEnUS: "english"}
	assert.Equal(t, "中文", svc.messageForLang(cfg, "zh-CN"))
	assert.Equal(t, "中文", svc.messageForLang(cfg, "zh-Hans"))
	assert.Equal(t, "english", svc.messageForLang(cfg, "en-US"))
	assert.Equal(t, "english", svc.messageForLang(cfg, ""))
	assert.Equal(t, "english", svc.messageForLang(cfg, "fr-FR"))
}

func TestClaimOwnerID(t *testing.T) {
	owner := claimOwnerID()
	assert.Contains(t, owner, ":", "owner must be <hostname>:<pid>")
	assert.NotEqual(t, ":", owner)
}

func TestResolveLanguage_FallbackWhenNoService(t *testing.T) {
	// langSvc == nil (no cache in this env) must fall back to the default
	// outbound language, never empty, never an error/retry.
	svc := &spaceWelcomeService{}
	lang := svc.resolveLanguage("u_1")
	assert.NotEmpty(t, lang, "must fall back to OCTO_DEFAULT_LANGUAGE, never empty")
}

func TestCallWithTimeout(t *testing.T) {
	// Completes within the deadline → (value, true).
	v, ok := callWithTimeout(time.Second, func() int { return 42 })
	assert.True(t, ok)
	assert.Equal(t, 42, v)

	// Exceeds the deadline → (zero, false); the caller is unblocked promptly.
	start := time.Now()
	_, ok = callWithTimeout(50*time.Millisecond, func() string {
		time.Sleep(500 * time.Millisecond)
		return "late"
	})
	assert.False(t, ok, "a hung call must time out")
	assert.Less(t, time.Since(start), 300*time.Millisecond, "caller must not block for the full call")
}
