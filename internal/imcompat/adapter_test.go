package imcompat

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

const testToken = "0123456789abcdef0123456789abcdef"
const testSecret = "abcdef0123456789abcdef0123456789"
const largeID = int64(2109916230306500608)

func newAdapter(t *testing.T, coreURL, webhookURL string, customize func(*Options)) *Adapter {
	t.Helper()
	opts := Options{CoreURL: coreURL, ServerWebhookURL: webhookURL, APIToken: testToken, WebhookSecret: testSecret}
	if customize != nil {
		customize(&opts)
	}
	a, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func call(t *testing.T, handler http.Handler, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("token", token)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestActualOctoLibSender(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{"v3 success", `{"client_msg_no":"send-1","message_id":2109916230306500608,"message_seq":8,"reason":1}`, false},
		{"legacy success", `{"status":200,"data":{"client_msg_no":"send-1","message_id":2109916230306500608,"message_seq":8}}`, false},
		{"permission rejection carried by 200", `{"client_msg_no":"send-1","message_id":0,"message_seq":0,"reason":3}`, true},
		{"incomplete success", `{"reason":1}`, true},
		{"invalid JSON", `not-json`, true},
		{"unsupported sequence width", `{"client_msg_no":"send-1","message_id":2109916230306500608,"message_seq":4294967296,"reason":1}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/message/send" || r.Header.Get("token") != "" {
					t.Errorf("unexpected upstream request")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer core.Close()
			a := newAdapter(t, core.URL, "http://127.0.0.1:1/v1/webhook", nil)
			proxy := httptest.NewServer(a.APIHandler())
			defer proxy.Close()
			cfg := config.New()
			cfg.WuKongIM.APIURL, cfg.WuKongIM.ManagerToken = proxy.URL, testToken
			ctx := config.NewContext(cfg)
			result, err := ctx.SendMessageWithResult(&config.MsgSendReq{FromUID: "alice", ChannelID: "group", ChannelType: 2, Payload: []byte(`{"type":1}`)})
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if !tc.wantError && (result.MessageID != largeID || result.MessageSeq != 8 || result.ClientMsgNo != "send-1") {
				t.Fatalf("lost acknowledgement fields: %+v", result)
			}
			if calls.Load() != 1 {
				t.Fatalf("send was retried: %d", calls.Load())
			}
		})
	}
}

func TestAPIAuthenticationHealthAndBounds(t *testing.T) {
	var calls atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/readyz" {
			_, _ = io.WriteString(w, `{"ready":true}`)
			return
		}
		w.WriteHeader(http.StatusTeapot)
	}))
	defer core.Close()
	a := newAdapter(t, core.URL, "http://127.0.0.1:1/v1/webhook", func(o *Options) { o.MaxBodyBytes = 1024 })
	for _, token := range []string{"", "wrong"} {
		w := call(t, a.APIHandler(), "POST", "/user/token", `{}`, token)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("unauthorized status=%d", w.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached Core")
	}
	w := call(t, a.APIHandler(), "GET", "/health", ``, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ready":true`) {
		t.Fatalf("health not mapped to readiness: %s", w.Body.String())
	}
	w = call(t, a.APIHandler(), "POST", "/channel", strings.Repeat("x", 1025), testToken)
	if w.Code != http.StatusRequestEntityTooLarge || calls.Load() != 1 {
		t.Fatal("oversized request reached Core")
	}
	w = call(t, a.APIHandler(), "GET", "/unknown", ``, testToken)
	if w.Code != http.StatusTeapot {
		t.Fatal("upstream failure was hidden")
	}
}

func TestSignedWebhookProjectionAndPrecision(t *testing.T) {
	for _, tc := range []struct{ name, event, body, wantChannel string }{
		{"person notify", "msg.notify", `[{"message_id":2109916230306500608,"from_uid":"alice","channel_id":"bob@alice","channel_type":1,"payload":"e30="}]`, "bob"},
		{"group with at sign", "msg.notify", `[{"message_id":2109916230306500608,"from_uid":"alice","channel_id":"topic@group","channel_type":2,"payload":"e30="}]`, "topic@group"},
		{"already projected", "msg.notify", `[{"message_id":2109916230306500608,"from_uid":"alice","channel_id":"bob","channel_type":1}]`, "bob"},
		{"person offline", "msg.offline", `{"message_id":2109916230306500608,"from_uid":"alice","channel_id":"alice@bob","channel_type":1,"to_uids":["bob"],"source_id":12,"payload":"e30="}`, "bob"},
		{"online status", "user.onlinestatus", `["alice-1-1-1-1-1"]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				mac := hmac.New(sha256.New, []byte(testSecret))
				_, _ = mac.Write(body)
				want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
				if !hmac.Equal([]byte(r.Header.Get("X-Signature-256")), []byte(want)) {
					t.Error("signature does not cover forwarded bytes")
				}
				if r.URL.Path != "/v1/webhook" || r.URL.Query().Get("event") != tc.event {
					t.Error("wrong callback target")
				}
				if tc.wantChannel != "" {
					if !bytes.Contains(body, []byte(`"channel_id":"`+tc.wantChannel+`"`)) || !bytes.Contains(body, []byte(`2109916230306500608`)) {
						t.Errorf("incorrect projection: %s", body)
					}
				}
				if tc.event == "msg.notify" {
					_, _ = io.WriteString(w, `["2109916230306500608"]`)
				} else {
					_, _ = io.WriteString(w, `{"status":200}`)
				}
			}))
			defer server.Close()
			a := newAdapter(t, "http://127.0.0.1:1", server.URL+"/v1/webhook", nil)
			w := call(t, a.WebhookHandler(), "POST", "/webhook?event="+tc.event, tc.body, "")
			if w.Code != 200 || calls.Load() != 1 {
				t.Fatalf("callback failed: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestWebhookRefusesNonlocalInvalidAndOversizedEvents(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(200) }))
	defer server.Close()
	a := newAdapter(t, "http://127.0.0.1:1", server.URL+"/v1/webhook", func(o *Options) { o.MaxBodyBytes = 1024 })
	for _, tc := range []struct {
		event, body string
		status      int
	}{
		{"unknown", `{}`, 400},
		{"msg.notify", `null`, 400},
		{"msg.notify", `[{"message_id":1,"channel_type":1,"from_uid":"alice","channel_id":"bob@alice@alice"}]`, 400},
		{"msg.notify", `[{"message_id":1,"channel_type":1,"from_uid":"eve","channel_id":"bob@alice"}]`, 400},
		{"msg.notify", strings.Repeat("x", 1025), 413},
		{"msg.notify", `[{"message_id":9223372036854775808,"channel_type":2,"from_uid":"alice","channel_id":"group"}]`, 400},
	} {
		w := call(t, a.WebhookHandler(), "POST", "/webhook?event="+tc.event, tc.body, "")
		if w.Code != tc.status {
			t.Fatalf("expected %d got %d", tc.status, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/webhook?event=user.onlinestatus", strings.NewReader(`[]`))
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	w := httptest.NewRecorder()
	a.WebhookHandler().ServeHTTP(w, r)
	if w.Code != 403 || calls.Load() != 0 {
		t.Fatal("untrusted callback was forwarded")
	}
	tooMany, _ := json.Marshal(make([]string, MaxEventItems+1))
	if _, _, err := normalizeEvent("user.onlinestatus", tooMany); err == nil {
		t.Fatal("unbounded event entries")
	}
}

func TestWebhookRequiresCompleteAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{500, `{"status":500}`}, {200, `{"status":200}`}, {200, `[]`},
		{200, `["wrong-id"]`}, {200, `null`}, {200, `["7","7"]`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
		}))
		a := newAdapter(t, "http://127.0.0.1:1", server.URL+"/v1/webhook", nil)
		w := call(t, a.WebhookHandler(), "POST", "/webhook?event=msg.notify", `[{"message_id":7,"from_uid":"a","channel_id":"g","channel_type":2}]`, "")
		server.Close()
		if w.Code != 502 {
			t.Fatalf("incorrectly acknowledged %d %s", tc.status, tc.body)
		}
	}
	if validWebhookAck("msg.offline", []byte(`{"status":500}`), nil) {
		t.Fatal("offline failure acknowledged")
	}
}

func TestBatchUsesPersonalSendsAndReportsPartialFailure(t *testing.T) {
	var calls atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			ChannelID   string `json:"channel_id"`
			ChannelType uint8  `json:"channel_type"`
			ClientMsgNo string `json:"client_msg_no"`
			FromUID     string `json:"from_uid"`
			Payload     []byte `json:"payload"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.ChannelType != 1 || body.FromUID != "alice" || body.ClientMsgNo == "" || string(body.Payload) != "{}" {
			t.Error("invalid fanout send")
		}
		reason := 1
		if body.ChannelID == "blocked" {
			reason = 3
		}
		_, _ = fmt.Fprintf(w, `{"reason":%d,"message_id":7,"message_seq":1,"client_msg_no":%q}`, reason, body.ClientMsgNo)
	}))
	defer core.Close()
	a := newAdapter(t, core.URL, "http://127.0.0.1:1/v1/webhook", nil)
	w := call(t, a.APIHandler(), "POST", "/message/sendbatch", `{"from_uid":"alice","subscribers":["bob","blocked"],"header":{"red_dot":1},"payload":"e30="}`, testToken)
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"fail_uids":["blocked"]`) || calls.Load() != 2 {
		t.Fatalf("partial failure hidden: %d %s", w.Code, w.Body.String())
	}
	w = call(t, a.APIHandler(), "POST", "/message/sendbatch", `{"from_uid":"alice","subscribers":["bob"],"payload":"e30="}`, testToken)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"fail_uids":[]`) {
		t.Fatalf("batch success failed: %s", w.Body.String())
	}
}

func TestCommittedMaximumSequenceExactLookup(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		status         int
	}{
		{"committed tail", `{"items":[{"channel_id":"a@b","channel_type":1,"max_message_seq":19}]}`, 200},
		{"empty committed channel", `{"items":[{"channel_id":"a@b","channel_type":1,"max_message_seq":0}]}`, 200},
		{"missing observation", `{"items":[{"channel_id":"a@b","channel_type":1}]}`, 502},
		{"wrong channel", `{"items":[{"channel_id":"other","channel_type":1,"max_message_seq":19}]}`, 502},
		{"not found", `{"items":[]}`, 502},
		{"old client width exceeded", `{"items":[{"channel_id":"a@b","channel_type":1,"max_message_seq":4294967296}]}`, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/manager/login" {
					var creds map[string]string
					_ = json.NewDecoder(r.Body).Decode(&creds)
					if creds["username"] != "compat" || creds["password"] != "test-password" {
						t.Error("wrong Manager credentials")
					}
					_, _ = io.WriteString(w, `{"access_token":"test-jwt"}`)
					return
				}
				if r.URL.Path != "/manager/channel-runtime-meta" || r.URL.Query().Get("exact") != "true" || r.URL.Query().Get("channel_id") != "a@b" || r.Header.Get("Authorization") != "Bearer test-jwt" {
					t.Error("not an authenticated exact lookup")
				}
				_, _ = io.WriteString(w, tc.response)
			}))
			defer manager.Close()
			a := newAdapter(t, "http://127.0.0.1:1", "http://127.0.0.1:1/v1/webhook", func(o *Options) {
				o.ManagerURL = manager.URL
				o.ManagerUser = "compat"
				o.ManagerPassword = "test-password"
			})
			w := call(t, a.APIHandler(), "GET", "/channel/max_message_seq?channel_id=a%40b&channel_type=1", "", testToken)
			if w.Code != tc.status {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.name == "committed tail" && !strings.Contains(w.Body.String(), `"message_seq":19`) {
				t.Fatal("wrong legacy sequence response")
			}
		})
	}
}

func TestAdapterConfigurationBoundaries(t *testing.T) {
	for _, address := range []string{"0.0.0.0:5004", ":5004", "localhost:5004", "10.0.0.1:5004"} {
		if LoopbackAddress(address) {
			t.Fatalf("accepted nonnumeric/nonlocal listener %s", address)
		}
	}
	if !LoopbackAddress("127.0.0.1:5004") || !LoopbackAddress("[::1]:5004") {
		t.Fatal("loopback rejected")
	}
	for _, raw := range []string{"http://user:pass@example.com", "http://example.com?token=secret", "file:///tmp/im", "http://example.com/nested"} {
		if _, err := targetURL(raw, true); err == nil {
			t.Fatal("accepted invalid target")
		}
	}
	if _, err := New(Options{CoreURL: "http://127.0.0.1:1", ServerWebhookURL: "http://127.0.0.1:2/v1/webhook"}); err == nil {
		t.Fatal("empty service secrets accepted")
	}
}
