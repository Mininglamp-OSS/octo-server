package cardactiondispatch

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	rd "github.com/go-redis/redis"
)

const testCallbackSecret = "0123456789abcdef0123456789abcdef"

func validRouteSpec() RouteSpec {
	return RouteSpec{
		SenderUID:   "notification",
		Owner:       "docs",
		ActionType:  "access_request.decision",
		URL:         "https://docs.internal/v1/card-actions/decide",
		SecretEnv:   "OCTO_DOCS_CARD_ACTION_SECRET",
		Timeout:     3 * time.Second,
		MaxAttempts: 3,
		BaseBackoff: time.Second,
		MaxBackoff:  30 * time.Second,
		MaxInFlight: 4,
	}
}

func testGetenv(key string) string {
	if key == "OCTO_DOCS_CARD_ACTION_SECRET" {
		return testCallbackSecret
	}
	return ""
}

func TestRegistryResolveIsBoundToStoredSender(t *testing.T) {
	registry, err := NewRegistry(
		[]RouteSpec{validRouteSpec()},
		testGetenv,
	)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}

	tests := []struct {
		name       string
		senderUID  string
		owner      string
		actionType string
		want       ResolutionKind
	}{
		{
			name:       "registered sender and action use callback",
			senderUID:  "notification",
			owner:      "docs",
			actionType: "access_request.decision",
			want:       ResolutionCallback,
		},
		{
			name:       "external bot copying metadata stays on pull queue",
			senderUID:  "third-party-bot",
			owner:      "docs",
			actionType: "access_request.decision",
			want:       ResolutionBotPull,
		},
		{
			name:       "registered internal sender with unknown action fails closed",
			senderUID:  "notification",
			owner:      "docs",
			actionType: "unknown",
			want:       ResolutionReject,
		},
		{
			name:       "registered internal sender with malformed metadata fails closed",
			senderUID:  "notification",
			owner:      "",
			actionType: "",
			want:       ResolutionReject,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := registry.Resolve(tt.senderUID, tt.owner, tt.actionType)
			if got.Kind != tt.want {
				t.Fatalf("Resolve() kind = %q, want %q", got.Kind, tt.want)
			}
			if tt.want == ResolutionCallback && got.Route == nil {
				t.Fatal("callback resolution must include the immutable route")
			}
		})
	}
}

func TestNewRegistryRejectsUnsafeOrAmbiguousRoutes(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*RouteSpec)
		getenv     func(string) string
		additional []RouteSpec
	}{
		{
			name: "unsupported scheme",
			mutate: func(spec *RouteSpec) {
				spec.URL = "ftp://docs.internal/v1/card-actions/decide"
			},
		},
		{
			name: "credential bearing URL",
			mutate: func(spec *RouteSpec) {
				spec.URL = "https://user:pass@docs.internal/v1/card-actions/decide"
			},
		},
		{
			name: "fragment bearing URL",
			mutate: func(spec *RouteSpec) {
				spec.URL = "https://docs.internal/v1/card-actions/decide#fragment"
			},
		},
		{
			name: "query bearing URL",
			mutate: func(spec *RouteSpec) {
				spec.URL = "https://docs.internal/v1/card-actions/decide?trace=1"
			},
		},
		{
			name: "URL missing host",
			mutate: func(spec *RouteSpec) {
				spec.URL = "https:///v1/card-actions/decide"
			},
		},
		{
			name:   "missing secret",
			mutate: func(*RouteSpec) {},
			getenv: func(string) string { return "" },
		},
		{
			name:   "duplicate route key",
			mutate: func(*RouteSpec) {},
			additional: []RouteSpec{
				validRouteSpec(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := validRouteSpec()
			tt.mutate(&spec)
			getenv := tt.getenv
			if getenv == nil {
				getenv = testGetenv
			}
			_, err := NewRegistry(append([]RouteSpec{spec}, tt.additional...), getenv)
			if err == nil {
				t.Fatal("NewRegistry() error = nil, want configuration rejection")
			}
		})
	}
}

func TestLoadRouteSpecsUsesBoundedProductionDefaults(t *testing.T) {
	raw := `[{
		"sender_uid":"notification",
		"owner":"docs",
		"action_type":"access_request.decision",
		"url":"https://docs.internal/v1/card-actions/decide",
		"secret_env":"OCTO_DOCS_CARD_ACTION_SECRET",
		"notify_token_env":"OCTO_DOCS_NOTIFY_TOKEN"
	}]`
	specs, err := LoadRouteSpecs(raw)
	if err != nil {
		t.Fatalf("LoadRouteSpecs() error = %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("LoadRouteSpecs() count = %d, want 1", len(specs))
	}
	got := specs[0]
	if got.Timeout <= 0 || got.Timeout > 10*time.Second {
		t.Errorf("default timeout = %s, want bounded positive timeout", got.Timeout)
	}
	if got.MaxAttempts < 1 || got.MaxAttempts > 10 {
		t.Errorf("default max attempts = %d, want [1,10]", got.MaxAttempts)
	}
	if got.MaxInFlight < 1 || got.MaxInFlight > 100 {
		t.Errorf("default max in flight = %d, want [1,100]", got.MaxInFlight)
	}
	if got.NotifyTokenEnv != "OCTO_DOCS_NOTIFY_TOKEN" {
		t.Errorf("notify token env = %q", got.NotifyTokenEnv)
	}
}

func TestRegistryBindsNotifyTokenToOnlyConfiguredOwnerActions(t *testing.T) {
	const notifyToken = "abcdef0123456789abcdef0123456789"
	primary := validRouteSpec()
	primary.Owner = "smart-summary"
	primary.ActionType = "summary.publish.decision"
	primary.NotifyTokenEnv = "OCTO_SMART_SUMMARY_NOTIFY_TOKEN"
	secondary := primary
	secondary.ActionType = "summary.delete.decision"
	secondary.NotifyTokenEnv = ""

	getenv := func(key string) string {
		switch key {
		case primary.SecretEnv:
			return testCallbackSecret
		case primary.NotifyTokenEnv:
			return notifyToken
		default:
			return ""
		}
	}
	registry, err := NewRegistry([]RouteSpec{primary, secondary}, getenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	capability, ok := registry.ResolveNotifyToken(notifyToken)
	if !ok {
		t.Fatal("ResolveNotifyToken() rejected configured token")
	}
	if capability.SenderUID != primary.SenderUID || capability.Owner != primary.Owner {
		t.Fatalf("notify capability = %+v", capability)
	}
	if !registry.CanNotify(capability, primary.ActionType) {
		t.Fatal("configured notify action was rejected")
	}
	if registry.CanNotify(capability, secondary.ActionType) {
		t.Fatal("callback-only action was accepted by notify capability")
	}
	if _, ok := registry.ResolveNotifyToken("wrong-token"); ok {
		t.Fatal("ResolveNotifyToken() accepted wrong token")
	}
	if got := registry.NotifyProducers(); len(got) != 1 || got[0] != capability {
		t.Fatalf("NotifyProducers() = %+v, want [%+v]", got, capability)
	}
}

func TestRegistryRejectsUnsafeNotifyTokenBindings(t *testing.T) {
	const notifyToken = "abcdef0123456789abcdef0123456789"
	tests := []struct {
		name   string
		specs  func() []RouteSpec
		getenv func(string) string
	}{
		{
			name: "short notify token",
			specs: func() []RouteSpec {
				spec := validRouteSpec()
				spec.NotifyTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"
				return []RouteSpec{spec}
			},
			getenv: func(key string) string {
				if key == "OCTO_DOCS_NOTIFY_TOKEN" {
					return "short"
				}
				return testGetenv(key)
			},
		},
		{
			name: "callback and notify secret reuse",
			specs: func() []RouteSpec {
				spec := validRouteSpec()
				spec.NotifyTokenEnv = "OCTO_DOCS_NOTIFY_TOKEN"
				return []RouteSpec{spec}
			},
			getenv: func(string) string { return testCallbackSecret },
		},
		{
			name: "one notify token bound to two owners",
			specs: func() []RouteSpec {
				docs := validRouteSpec()
				docs.NotifyTokenEnv = "OCTO_SHARED_NOTIFY_TOKEN"
				tasks := docs
				tasks.Owner = "tasks"
				tasks.ActionType = "task.decision"
				return []RouteSpec{docs, tasks}
			},
			getenv: func(key string) string {
				if key == "OCTO_SHARED_NOTIFY_TOKEN" {
					return notifyToken
				}
				return testGetenv(key)
			},
		},
		{
			name: "notify token reuses another route callback secret",
			specs: func() []RouteSpec {
				docs := validRouteSpec()
				tasks := docs
				tasks.Owner = "tasks"
				tasks.ActionType = "task.decision"
				tasks.SecretEnv = "OCTO_TASKS_CARD_ACTION_SECRET"
				tasks.NotifyTokenEnv = "OCTO_TASKS_NOTIFY_TOKEN"
				return []RouteSpec{docs, tasks}
			},
			getenv: func(key string) string {
				switch key {
				case "OCTO_DOCS_CARD_ACTION_SECRET", "OCTO_TASKS_NOTIFY_TOKEN":
					return testCallbackSecret
				case "OCTO_TASKS_CARD_ACTION_SECRET":
					return "fedcba9876543210fedcba9876543210"
				default:
					return ""
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRegistry(tt.specs(), tt.getenv); err == nil {
				t.Fatal("NewRegistry() error = nil")
			}
		})
	}
}

func TestRegistryRejectsNotifyTokenThatConflictsWithBroaderIngress(t *testing.T) {
	spec := validRouteSpec()
	spec.NotifyTokenEnv = "OCTO_TASKS_NOTIFY_TOKEN"
	getenv := func(key string) string {
		if key == spec.NotifyTokenEnv {
			return "abcdef0123456789abcdef0123456789"
		}
		return testGetenv(key)
	}
	registry, err := NewRegistry([]RouteSpec{spec}, getenv)
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	tests := []struct {
		name  string
		token string
	}{
		{name: "route notify token", token: "abcdef0123456789abcdef0123456789"},
		{name: "callback secret", token: testCallbackSecret},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := registry.ValidateNotifyTokenExclusions(tt.token); err == nil {
				t.Fatal("ValidateNotifyTokenExclusions() error = nil")
			}
		})
	}
}

func TestHMACSignatureUsesVersionedCanonicalRequest(t *testing.T) {
	body := []byte(`{"event_id":"42","decision":"approve"}`)
	timestamp := "1784073600"
	eventID := "42"
	wantCanonical := "v1\nPOST\n/v1/card-actions/decide\n1784073600\n42\n" + sha256Hex(body)
	if got := CanonicalRequest(http.MethodPost, "/v1/card-actions/decide", timestamp, eventID, body); got != wantCanonical {
		t.Fatalf("CanonicalRequest() = %q, want %q", got, wantCanonical)
	}

	mac := hmac.New(sha256.New, []byte(testCallbackSecret))
	_, _ = mac.Write([]byte(wantCanonical))
	wantSignature := "v1=" + hex.EncodeToString(mac.Sum(nil))
	gotSignature := Sign(testCallbackSecret, http.MethodPost, "/v1/card-actions/decide", timestamp, eventID, body)
	if gotSignature != wantSignature {
		t.Fatalf("Sign() = %q, want %q", gotSignature, wantSignature)
	}
	if !Verify(testCallbackSecret, gotSignature, http.MethodPost, "/v1/card-actions/decide", timestamp, eventID, body) {
		t.Fatal("Verify() rejected a valid signature")
	}
	if Verify(testCallbackSecret, gotSignature, http.MethodPost, "/v1/card-actions/decide", timestamp, eventID, append(body, ' ')) {
		t.Fatal("Verify() accepted a signature for mutated exact body bytes")
	}
}

func TestDecisionRequestMarshalsEventIDWithoutJavaScriptPrecisionLoss(t *testing.T) {
	const eventID int64 = 9_007_199_254_740_993
	body, err := json.Marshal(DecisionRequest{EventID: eventID})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got, want := string(fields["event_id"]), `"9007199254740993"`; got != want {
		t.Fatalf("event_id JSON = %s, want %s", got, want)
	}
}

func TestDecisionRequestNormalizesMissingInputsToEmptyObject(t *testing.T) {
	body, err := json.Marshal(DecisionRequestFromEvent(Event{EventID: 1}))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got, want := string(fields["inputs"]), `{}`; got != want {
		t.Fatalf("inputs JSON = %s, want %s", got, want)
	}
}

func TestDecodeDecisionResultIsTypedAndSizeBounded(t *testing.T) {
	valid := `{"disposition":"applied","state":"approved","requester_uid":"user-a","display":{"title":"Roadmap"}}`
	result, err := DecodeDecisionResult(strings.NewReader(valid))
	if err != nil {
		t.Fatalf("DecodeDecisionResult(valid) error = %v", err)
	}
	if result.Disposition != DispositionApplied || result.State != StateApproved || result.RequesterUID != "user-a" {
		t.Fatalf("DecodeDecisionResult(valid) = %+v", result)
	}

	invalid := []string{
		`{"disposition":"unknown","state":"approved"}`,
		`{"disposition":"applied","state":"unknown"}`,
		`{"disposition":"applied","state":"approved"}`,
		`{"disposition":"applied","state":"approved","requester_uid":"user-a","extra":true}`,
		`{"disposition":"applied","state":"approved","display":{"title":1}}`,
	}
	for _, body := range invalid {
		if _, err := DecodeDecisionResult(strings.NewReader(body)); err == nil {
			t.Errorf("DecodeDecisionResult(%s) error = nil, want rejection", body)
		}
	}

	oversized := bytes.NewReader(bytes.Repeat([]byte("x"), MaxDecisionResponseBytes+1))
	if _, err := DecodeDecisionResult(oversized); err == nil {
		t.Fatal("DecodeDecisionResult() accepted oversized response")
	}
}

func TestRedisQueueLeaseTokenRetryDLQAndReplay(t *testing.T) {
	client := rd.NewClient(&rd.Options{Addr: "127.0.0.1:6379"})
	if err := client.Ping().Err(); err != nil {
		t.Skipf("Redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	prefix := fmt.Sprintf("test:card_action_dispatch:%d", time.Now().UnixNano())
	queue, err := NewRedisQueue(client, QueueConfig{
		Prefix:       prefix,
		LiveTTL:      10 * time.Minute,
		DLQRetention: 30 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewRedisQueue() error = %v", err)
	}
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
	})

	now := time.Now().Truncate(time.Millisecond)
	event := Event{
		EventID:     42,
		SenderUID:   "notification",
		Owner:       "docs",
		ActionType:  "access_request.decision",
		MessageID:   "1001",
		ChannelID:   "user-b",
		ChannelType: 1,
		SpaceID:     "space-1",
		ActionID:    "approve",
		OperatorUID: "user-b",
		ClientToken: "tap-1",
		ActedAt:     now.Unix(),
		Inputs:      map[string]interface{}{},
		Data: map[string]interface{}{
			"owner":       "docs",
			"action_type": "access_request.decision",
			"decision":    "approve",
			"doc_id":      "doc-1",
			"request_id":  "request-1",
		},
	}
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	// Enqueue is idempotent on event_id and must not reset attempts/state.
	if err := queue.Enqueue(event, now.Add(time.Second)); err != nil {
		t.Fatalf("duplicate Enqueue() error = %v", err)
	}

	lease, err := queue.Claim(now, 5*time.Second)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if lease == nil || lease.Event.EventID != event.EventID || lease.Attempt != 1 || lease.Token == "" {
		t.Fatalf("Claim() = %+v, want event 42 attempt 1 with token", lease)
	}
	if acked, err := queue.Ack(event.EventID, "wrong-token"); err != nil || acked {
		t.Fatalf("Ack(wrong token) = (%v, %v), want (false, nil)", acked, err)
	}
	if reclaimed, err := queue.ReclaimExpired(now.Add(4*time.Second), 10); err != nil || reclaimed != 0 {
		t.Fatalf("ReclaimExpired(before lease) = (%d, %v), want (0, nil)", reclaimed, err)
	}
	if reclaimed, err := queue.ReclaimExpired(now.Add(6*time.Second), 10); err != nil || reclaimed != 1 {
		t.Fatalf("ReclaimExpired(after lease) = (%d, %v), want (1, nil)", reclaimed, err)
	}

	lease, err = queue.Claim(now.Add(6*time.Second), 5*time.Second)
	if err != nil || lease == nil || lease.Attempt != 2 {
		t.Fatalf("second Claim() = (%+v, %v), want attempt 2", lease, err)
	}
	if lease.Token == "" {
		t.Fatal("reclaimed lease has empty token")
	}
	outcome, err := queue.Nack(*lease, now.Add(6*time.Second), 2*time.Second, 3, "upstream_5xx")
	if err != nil || outcome != NackRequeued {
		t.Fatalf("Nack(retry) = (%q, %v), want (%q, nil)", outcome, err, NackRequeued)
	}
	if premature, err := queue.Claim(now.Add(7*time.Second), 5*time.Second); err != nil || premature != nil {
		t.Fatalf("Claim(before backoff) = (%+v, %v), want (nil, nil)", premature, err)
	}

	lease, err = queue.Claim(now.Add(9*time.Second), 5*time.Second)
	if err != nil || lease == nil || lease.Attempt != 3 {
		t.Fatalf("third Claim() = (%+v, %v), want attempt 3", lease, err)
	}
	outcome, err = queue.Nack(*lease, now.Add(9*time.Second), 0, 3, "timeout")
	if err != nil || outcome != NackDeadLettered {
		t.Fatalf("Nack(exhausted) = (%q, %v), want (%q, nil)", outcome, err, NackDeadLettered)
	}
	depths, err := queue.Depths()
	if err != nil {
		t.Fatalf("Depths() error = %v", err)
	}
	if depths.Ready != 0 || depths.Leased != 0 || depths.DLQ != 1 {
		t.Fatalf("Depths() = %+v, want ready=0 leased=0 dlq=1", depths)
	}

	replayed, err := queue.ReplayDLQ(event.EventID, now.Add(10*time.Second))
	if err != nil || !replayed {
		t.Fatalf("ReplayDLQ() = (%v, %v), want (true, nil)", replayed, err)
	}
	lease, err = queue.Claim(now.Add(10*time.Second), 5*time.Second)
	if err != nil || lease == nil || lease.Event.EventID != event.EventID || lease.Attempt != 1 {
		t.Fatalf("Claim(after DLQ replay) = (%+v, %v), want event 42 reset to attempt 1", lease, err)
	}
	if acked, err := queue.Ack(event.EventID, lease.Token); err != nil || !acked {
		t.Fatalf("Ack(valid token) = (%v, %v), want (true, nil)", acked, err)
	}
}

func TestRedisQueueDLQRetentionIsPerEvent(t *testing.T) {
	client := rd.NewClient(&rd.Options{Addr: "127.0.0.1:6379"})
	if err := client.Ping().Err(); err != nil {
		t.Skipf("Redis unavailable: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	prefix := fmt.Sprintf("test:card_action_dlq_retention:%d", time.Now().UnixNano())
	queue, err := NewRedisQueue(client, QueueConfig{Prefix: prefix, LiveTTL: time.Hour, DLQRetention: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("NewRedisQueue() error = %v", err)
	}
	t.Cleanup(func() {
		keys, _ := client.Keys(prefix + "*").Result()
		if len(keys) > 0 {
			_ = client.Del(keys...).Err()
		}
	})
	now := time.Now().Truncate(time.Millisecond)
	event := testDispatchEvent()
	if err := queue.Enqueue(event, now); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	lease, err := queue.Claim(now, time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("Claim() = (%+v, %v)", lease, err)
	}
	if outcome, err := queue.Nack(*lease, now, time.Hour, 1, "forced"); err != nil || outcome != NackDeadLettered {
		t.Fatalf("Nack() = (%q, %v)", outcome, err)
	}
	if replayed, err := queue.ReplayDLQ(event.EventID, now.Add(30*24*time.Hour+time.Millisecond)); err != nil || replayed {
		t.Fatalf("ReplayDLQ(expired) = (%v, %v), want (false, nil)", replayed, err)
	}
}

func sha256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
